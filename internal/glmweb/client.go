package glmweb

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
)

const (
	defaultBase   = "https://chatglm.cn"
	webUserAgent  = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36"
	clientVersion = "0.0.1"
	defaultAssistantID = "65940acff94777010aa6b796" // 默认助手(全部工具智能体)
)

// Client 是智谱网页客户端。
type Client struct {
	baseURL    string
	httpClient *http.Client
	tokenFile  string // token 池文件路径(换发轮换时回写,防"重启后旧 token 作废")
	// 凭据
	refreshToken string // 当前生效的 chatglm_refresh_token(长期)
	accessToken  string // 换发来的 JWT(短期,~2h)
	deviceID     string // X-Device-Id(持久化 uuid)
	// token 池(从文件加载,每行一个 refresh_token)
	tokens []string
	cursor int
}

// NewClient 构造客户端。tokenFile 是每行一个 refresh_token 的池文件
// (与 DeepSeek 的 token 池一致);也可直接传单个 refreshToken。
// 可选传入 accessToken(已有有效 JWT 时跳过首次换发)。
func NewClient(baseURL, tokenFile, refreshToken, accessToken, deviceID string) *Client {
	if baseURL == "" {
		baseURL = defaultBase
	}
	if deviceID == "" {
		deviceID = uuidNoDash()
	}
	c := &Client{
		baseURL:      strings.TrimRight(baseURL, "/"),
		httpClient:   &http.Client{},
		refreshToken: refreshToken,
		accessToken:  accessToken,
		deviceID:     deviceID,
		tokenFile:    tokenFile,
	}
	if tokenFile != "" {
		if tokens, err := loadTokens(tokenFile); err == nil && len(tokens) > 0 {
			c.tokens = tokens
			if c.refreshToken == "" {
				c.refreshToken = c.NextToken()
			}
		}
	}
	return c
}

// HasAccessToken 报告是否已有 access_token(避免重复换发)。
func (c *Client) HasAccessToken() bool { return c.accessToken != "" }

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

// SetRefreshToken 切换当前生效的 refresh_token(轮换时用)。
func (c *Client) SetRefreshToken(t string) { c.refreshToken = t }

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

// RefreshAccessToken 用 refresh_token 换发新的 access_token(JWT)。
func (c *Client) RefreshAccessToken() error {
	if c.refreshToken == "" {
		return fmt.Errorf("glm: no refresh token")
	}
	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/chatglm/user-api/user/refresh", bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	c.setSignedHeaders(req, c.refreshToken)
	req.Header.Set("Content-Type", "application/json;charset=utf-8")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var ar struct {
		Status int    `json:"status"`
		Result struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &ar); err != nil {
		return fmt.Errorf("refresh: decode %w (raw=%s)", err, truncate(string(body), 200))
	}
	if ar.Result.AccessToken == "" {
		return fmt.Errorf("refresh: no access_token (raw=%s)", truncate(string(body), 200))
	}
	c.accessToken = ar.Result.AccessToken
	if ar.Result.RefreshToken != "" {
		oldToken := c.refreshToken
		c.refreshToken = ar.Result.RefreshToken
		c.persistRefreshToken(oldToken, ar.Result.RefreshToken)
	}
	return nil
}

// persistRefreshToken 把换发轮换后的新 refresh_token 回写池文件(替换旧值)。
// 否则文件里永远是最旧的已作废 token —— 重启后换发失败(与 kimi 相同问题)。
func (c *Client) persistRefreshToken(oldToken, newToken string) {
	if c.tokenFile == "" || newToken == "" || oldToken == "" {
		return
	}
	data, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	replaced := false
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line == oldToken || line == newToken {
			lines[i] = newToken
			replaced = true
		}
	}
	if !replaced {
		lines = append(lines, newToken)
	}
	out := strings.Join(lines, "\n")
	tmp := c.tokenFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(out), 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, c.tokenFile)
	log.Printf("[glm] refresh_token rotated & persisted (%s)", c.tokenFile)
}

// setSignedHeaders 设置签名头 + 固定头。token 作 Authorization。
func (c *Client) setSignedHeaders(req *http.Request, token string) {
	sg := NewSign()
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("App-Name", "chatglm")
	req.Header.Set("X-App-Platform", "pc")
	req.Header.Set("X-App-Version", clientVersion)
	req.Header.Set("X-App-fr", "default")
	req.Header.Set("X-Lang", "zh")
	req.Header.Set("X-Device-Id", c.deviceID)
	req.Header.Set("X-Request-Id", uuidNoDash())
	req.Header.Set("X-Timestamp", sg.Timestamp)
	req.Header.Set("X-Nonce", sg.Nonce)
	req.Header.Set("X-Sign", sg.Sign)
	req.Header.Set("User-Agent", webUserAgent)
	req.Header.Set("Origin", c.baseURL)
	req.Header.Set("Referer", c.baseURL+"/")
}

// CompletionRequest 是 assistant/stream 的请求体。
type CompletionRequest struct {
	AssistantID     string
	ConversationID  string // 首轮空;续轮用上一轮返回的 conversation_id
	Messages        []Message
	ChatMode        string // "thinking"(深度思考) | "" 等
	IsNetworking    bool   // 联网搜索
	Platform        string // "pc"
}

// Message 是一条消息。
type Message struct {
	Role    string    `json:"role"`
	Content []Content `json:"content"`
}

// Content 是消息内容(文本 / 图片)。
type Content struct {
	Type     string `json:"type"`           // "text" | "image_url"
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url,omitempty"`
}

// Complete 发起一次 completion,返回 SSE 响应(调用方负责 Close + 解析)。
func (c *Client) Complete(req CompletionRequest) (*http.Response, error) {
	if req.AssistantID == "" {
		req.AssistantID = defaultAssistantID
	}
	if req.Platform == "" {
		req.Platform = "pc"
	}
	body := map[string]any{
		"assistant_id":    req.AssistantID,
		"conversation_id": req.ConversationID,
		"project_id":      "",
		"chat_type":       "user_chat",
		"meta_data": map[string]any{
			"cogview":             map[string]any{"rm_label_watermark": false},
			"is_test":             false,
			"input_question_type": "xxxx",
			"channel":             "",
			"draft_id":            "",
			"chat_mode":           req.ChatMode,
			"is_networking":       req.IsNetworking,
			"quote_log_id":        "",
			"platform":            req.Platform,
		},
		"messages": req.Messages,
	}
	buf, _ := json.Marshal(body)
	httpReq, err := http.NewRequest(http.MethodPost, c.baseURL+"/chatglm/backend-api/assistant/stream", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	c.setSignedHeaders(httpReq, c.accessToken)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
