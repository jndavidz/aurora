// Package kimiweb 实现 Kimi(www.kimi.com)网页接口逆向客户端。
//
// 协议要点(2026-08-14 CDP 抓包 + curl 重放验证):
//   - 认证:localStorage 的 access_token(JWT,~15 分钟)作 Authorization: Bearer;
//     refresh_token(JWT,~90 天)经 POST auth.kimi.com/api/account.gateway.v1.AuthService/RefreshToken
//     换发新 accessToken + refreshToken(两者都轮换)
//   - x-msh-* 头(device-id/session-id/traffic-id)可从 refresh_token 的 JWT claims 解出
//   - completion:POST www.kimi.com/apiv2/kimi.gateway.chat.v1.ChatService/Chat
//     (Connect 协议,请求体帧 = flags(1) + 长度(4BE) + JSON;响应为服务端流,帧间夹心跳,
//     flags=2 的收尾帧结束)。快速模式 scenario = SCENARIO_K2D5
//   - 无签名、无 PoW、x-msh-shield-data 可选(实测不带也 200)
package kimiweb

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"aurora/httpclient/factory"
	"aurora/internal/jwtutil"
	"aurora/internal/poolfile"
)

const (
	defaultBase   = "https://www.kimi.com"
	authBase      = "https://auth.kimi.com"
	webUserAgent  = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36"
	clientVersion = "2.0.0"
	// defaultScenario 是"快速模式"(K2.6)的 scenario,见 docs/KIMI.md。
	defaultScenario = "SCENARIO_K2D5"
)

// Client 是 Kimi 网页客户端。
type Client struct {
	mu         sync.Mutex // 换发互斥(并发请求同时换发会互相作废轮换链)
	baseURL    string
	authURL    string
	httpClient factory.Client
	tokenFile  string // token 池文件路径(换发轮换时回写,防"重启后旧 token 作废")
	// 凭据
	accessToken  string // 短期 JWT(~15 分钟),Authorization: Bearer
	refreshToken string // 长期 JWT(~90 天),刷新时轮换
	// 账号身份(从 refresh_token 的 JWT claims 解出,见 parseClaims)
	deviceID  string // x-msh-device-id
	sessionID string // x-msh-session-id
	trafficID string // x-traffic-id(用户 id)
	// token 池(从文件加载,每行一个 refresh_token)
	tokens []string
	cursor int
}

// NewClient 构造客户端。tokenFile 是每行一个 refresh_token 的池文件。
func NewClient(baseURL, tokenFile string) *Client {
	if baseURL == "" {
		baseURL = defaultBase
	}
	c := &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		authURL:    authBase,
		httpClient: factory.NewWebClient(factory.Profile{Mode: factory.ModeTLSFaked, Upgradable: true}), // C1 灰度(2026-09-04):第 3/4 家
		tokenFile:  tokenFile,
	}
	if tokenFile != "" {
		if tokens, err := loadTokens(tokenFile); err == nil && len(tokens) > 0 {
			c.tokens = tokens
			if c.refreshToken == "" {
				c.SetRefreshToken(c.NextToken())
			}
		}
	}
	return c
}

// HasAccessToken 报告是否已有 access_token(避免重复换发)。
func (c *Client) HasAccessToken() bool { return c.accessToken != "" }

// AccessTokenNearExpiry 报告 access_token 缺失、exp 无法解析或将在 skew 内过期。
// Kimi 的 access_token 仅 ~15 分钟,skew 取 3 分钟即"过 12 分钟就该换"。
// exp 解析失败按"临近过期"处理:宁可多换发一次,不可拿过期票打上游。
func (c *Client) AccessTokenNearExpiry(skew time.Duration) bool {
	exp, ok := jwtutil.Exp(c.accessToken)
	if !ok {
		return true
	}
	return time.Until(exp) < skew
}

// ClearAccessToken 加锁丢弃当前 access_token(上游 401/403 或请求失败后调用,
// 确保下一次请求经 ensureToken 重换发)。走 c.mu 与 RefreshAccessToken 互斥。
func (c *Client) ClearAccessToken() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.accessToken = ""
}

// RefreshTokenExps 返回池内各 refresh_token 的 exp(供凭证健康端点报告
// "距重抓还剩多久");池空但直传了单 token 时解析直传值。
// Kimi 的 refresh_token 每次换发都会轮换,这里反映的是当前池内值的到期时间。
func (c *Client) RefreshTokenExps() []time.Time {
	exps := make([]time.Time, 0, len(c.tokens)+1)
	for _, tok := range c.tokens {
		if exp, ok := jwtutil.Exp(tok); ok {
			exps = append(exps, exp)
		}
	}
	if len(exps) == 0 && c.refreshToken != "" {
		if exp, ok := jwtutil.Exp(c.refreshToken); ok {
			exps = append(exps, exp)
		}
	}
	return exps
}

// HasToken 报告是否有可用的 refresh_token(池或直传)。
func (c *Client) HasToken() bool { return c.refreshToken != "" }

// NextToken 轮询取下一个 refresh_token;池空返回当前 token。
func (c *Client) NextToken() string {
	if len(c.tokens) == 0 {
		return c.refreshToken
	}
	t := c.tokens[c.cursor%len(c.tokens)]
	c.cursor++
	return t
}

// SetRefreshToken 切换当前生效的 refresh_token(轮换时用),并从其 JWT claims
// 解出账号身份头(device_id/session_id/sub)。
func (c *Client) SetRefreshToken(t string) {
	c.refreshToken = t
	if claims, err := parseClaims(t); err == nil {
		if s, ok := claims["device_id"].(string); ok && s != "" {
			c.deviceID = s
		}
		if s, ok := claims["ssid"].(string); ok && s != "" {
			c.sessionID = s
		}
		if s, ok := claims["sub"].(string); ok && s != "" {
			c.trafficID = s
		}
	}
}

// PoolSize 返回 token 池大小;直传单 token 时视为 1。
func (c *Client) PoolSize() int {
	if len(c.tokens) == 0 {
		return 1
	}
	return len(c.tokens)
}

// loadTokens 读 token 池文件(每行一个,忽略空行/注释)。
func loadTokens(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out, sc.Err()
}

// parseClaims 解出 JWT 的 payload(HS512 签名不校验,只需要 claims 里的账号身份)。
func parseClaims(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("kimi: not a jwt")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, err
	}
	return claims, nil
}

// RefreshAccessToken 用 refresh_token 换发新的 access_token + refresh_token。
// 返回结构:{"accessToken":"...","refreshToken":"..."}(两者都轮换)。
func (c *Client) RefreshAccessToken() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.refreshToken == "" {
		return fmt.Errorf("kimi: no refresh token")
	}
	body, _ := json.Marshal(map[string]string{"refresh_token": c.refreshToken})
	req, err := http.NewRequest(http.MethodPost, c.authURL+"/api/account.gateway.v1.AuthService/RefreshToken", bytes.NewReader(body))
	if err != nil {
		return err
	}
	c.setHeaders(req, "")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("refresh http %d: %s", resp.StatusCode, truncate(string(data), 200))
	}
	var ar struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
	}
	if err := json.Unmarshal(data, &ar); err != nil {
		return fmt.Errorf("refresh: decode %w (raw=%s)", err, truncate(string(data), 200))
	}
	if ar.AccessToken == "" {
		return fmt.Errorf("refresh: no accessToken (raw=%s)", truncate(string(data), 200))
	}
	c.accessToken = ar.AccessToken
	if ar.RefreshToken != "" {
		oldToken := c.refreshToken
		c.SetRefreshToken(ar.RefreshToken)
		c.persistRefreshToken(oldToken, ar.RefreshToken)
	}
	return nil
}

// persistRefreshToken 把轮换后的新 refresh_token 回写池文件(替换旧值)。
// 否则文件里永远是最旧的已作废 token —— 重启后 401 "invalid claims"(实测)。
func (c *Client) persistRefreshToken(oldToken, newToken string) {
	if c.tokenFile == "" || newToken == "" || oldToken == "" {
		return
	}
	// A2:回写逻辑收口到 poolfile(唯一 tmp + 锁 + 原子 rename)。
	// 旧实现所有错误静默吞掉 —— 只读挂载下"内存轮换成功、文件仍旧值",
	// 重启后拿已作废旧票(实测 401 "invalid claims")。写失败必须告警。
	replaced, err := poolfile.ReplaceToken(c.tokenFile, oldToken, newToken, "kimi")
	switch {
	case err != nil:
		log.Printf("[kimi][ERROR] refresh_token 回写失败(%s): %v —— 重启后将用旧票,需人工重抓", c.tokenFile, err)
	case replaced:
		log.Printf("[kimi] refresh_token rotated & persisted (%s)", c.tokenFile)
	default:
		log.Printf("[kimi] refresh_token 旧值不在池文件中,已追加 (%s)", c.tokenFile)
	}
}

// setHeaders 设置通用头。token 非空时作 Authorization: Bearer。
func (c *Client) setHeaders(req *http.Request, token string) {
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("x-msh-device-id", c.deviceID)
	req.Header.Set("x-msh-session-id", c.sessionID)
	req.Header.Set("x-traffic-id", c.trafficID)
	req.Header.Set("x-msh-platform", "web")
	req.Header.Set("x-msh-version", clientVersion)
	req.Header.Set("x-language", "zh-CN")
	req.Header.Set("r-timezone", "Asia/Shanghai")
	req.Header.Set("connect-protocol-version", "1")
	req.Header.Set("User-Agent", webUserAgent)
	req.Header.Set("Referer", c.baseURL+"/")
}

// CompletionRequest 是 ChatService/Chat 的请求体。
type CompletionRequest struct {
	Scenario     string // 默认 SCENARIO_K2D5(快速模式 K2.6)
	Tools        []Tool // 原生工具(TOOL_TYPE_SEARCH 等;客户端自定义工具不支持)
	Text         string // 拍平后的单条用户消息文本(见 provider 侧 kimiFlatten*)
	Thinking     bool   // options.thinking
	EnablePlugin bool   // options.enable_plugin
	ProjectID    string // 默认 ""
}

// Tool 是 Chat 请求的原生工具。
type Tool struct {
	Type   string         `json:"type"`
	Search map[string]any `json:"search,omitempty"`
}

// Complete 发起一次 completion,返回 SSE 响应(调用方负责 Close + 解析)。
func (c *Client) Complete(req CompletionRequest) (*http.Response, error) {
	if req.Scenario == "" {
		req.Scenario = defaultScenario
	}
	body := map[string]any{
		"scenario": req.Scenario,
		"tools":    req.Tools,
		"message": map[string]any{
			"role":     "user",
			"blocks":   []map[string]any{{"message_id": "", "text": map[string]any{"content": req.Text}}},
			"scenario": req.Scenario,
			"is_goal":  false,
		},
		"options": map[string]any{
			"thinking":         req.Thinking,
			"enable_plugin":    req.EnablePlugin,
			"reasoning_effort": "REASONING_EFFORT_LOW",
		},
		"project_id": req.ProjectID,
	}
	payload, _ := json.Marshal(body)
	// Connect unary 帧:flags(0) + 4 字节大端长度 + JSON
	buf := make([]byte, 5+len(payload))
	buf[0] = 0
	putUint32BE(buf[1:5], uint32(len(payload)))
	copy(buf[5:], payload)

	httpReq, err := http.NewRequest(http.MethodPost, c.baseURL+"/apiv2/kimi.gateway.chat.v1.ChatService/Chat", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	c.setHeaders(httpReq, c.accessToken)
	httpReq.Header.Set("Content-Type", "application/connect+json")
	httpReq.Header.Set("Accept", "application/connect+json")
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("completion http %d: %s", resp.StatusCode, truncate(string(data), 300))
	}
	return resp, nil
}

func putUint32BE(b []byte, v uint32) {
	b[0] = byte(v >> 24)
	b[1] = byte(v >> 16)
	b[2] = byte(v >> 8)
	b[3] = byte(v)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
