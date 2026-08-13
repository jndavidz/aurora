// Package deepseekweb 实现 chat.deepseek.com 网页接口逆向客户端。
//
// 依据 docs/deepseek网页协议整理.md 整理;标注 [P0] 的字段/流程尚未经官网
// 实测验证(见该文档 §9 验证清单),接入前需逐项抓包确认。
package deepseekweb

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

const (
	defaultBase   = "https://chat.deepseek.com"
	userAgent     = "DeepSeek/2.0.0 Android/12"
	clientVersion = "2.0.0"
)

// Client 持有一个网页 token 池并复用 HTTP 连接。
type Client struct {
	baseURL string
	client  *http.Client
	tokens  []string
	cursor  int
}

// NewClient 构造客户端。tokenFile 为空时 tokens 为空(由调用方逐 token 传入)。
func NewClient(baseURL, tokenFile, proxyURL string) (*Client, error) {
	if baseURL == "" {
		baseURL = defaultBase
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if proxyURL != "" {
		if err := setTransportProxy(transport, proxyURL); err != nil {
			return nil, err
		}
	}
	c := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Transport: transport, Timeout: 0},
	}
	if tokenFile != "" {
		tokens, err := loadTokens(tokenFile)
		if err != nil {
			return nil, err
		}
		c.tokens = tokens
	}
	return c, nil
}

// NextToken 轮询取下一个 token;池空时返回空串。
func (c *Client) NextToken() string {
	if len(c.tokens) == 0 {
		return ""
	}
	t := c.tokens[c.cursor%len(c.tokens)]
	c.cursor++
	return t
}

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

// apiResponse 是网页接口统一信封:{code, msg, data:{biz_code, biz_msg, biz_data}}。
type apiResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		BizCode int             `json:"biz_code"`
		BizMsg  string          `json:"biz_msg"`
		BizData json.RawMessage `json:"biz_data"`
	} `json:"data"`
}

func (r *apiResponse) err() error {
	if r.Code != 0 {
		return fmt.Errorf("deepseek api error code=%d msg=%s", r.Code, r.Msg)
	}
	if r.Data.BizCode != 0 {
		return fmt.Errorf("deepseek api biz error code=%d msg=%s", r.Data.BizCode, r.Data.BizMsg)
	}
	return nil
}

// doJSON POST 一个 JSON 到网页接口并解析信封,返回 biz_data 原始字节。
func (c *Client) doJSON(token, path string, body any) (json.RawMessage, error) {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequest(http.MethodPost, c.baseURL+path, &buf)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req, token)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var ar apiResponse
	if err := json.Unmarshal(data, &ar); err != nil {
		return nil, fmt.Errorf("decode envelope: %w (raw=%s)", err, truncate(string(data), 200))
	}
	if err := ar.err(); err != nil {
		return nil, err
	}
	return ar.Data.BizData, nil
}

func (c *Client) setHeaders(req *http.Request, token string) {
	req.Header.Set("Host", strings.TrimPrefix(c.baseURL, "https://"))
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("x-client-platform", "android")
	req.Header.Set("x-client-version", clientVersion)
	req.Header.Set("x-client-locale", "zh_CN")
	req.Header.Set("accept-charset", "UTF-8")
}

// CreateSession 创建会话,返回 chat_session_id。
func (c *Client) CreateSession(token string) (string, error) {
	raw, err := c.doJSON(token, "/api/v0/chat_session/create", struct{}{})
	if err != nil {
		return "", err
	}
	// 新格式:data.biz_data.chat_session.id;老格式:data.biz_data.id
	var v struct {
		ChatSession struct {
			ID json.RawMessage `json:"id"`
		} `json:"chat_session"`
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", fmt.Errorf("decode session: %w", err)
	}
	id := rawJSONString(v.ChatSession.ID)
	if id == "" {
		id = rawJSONString(v.ID)
	}
	if id == "" {
		return "", fmt.Errorf("empty chat_session id")
	}
	return id, nil
}

// CompletionRequest 是 /api/v0/chat/completion 的请求体(拍平 prompt,无服务端历史)。
type CompletionRequest struct {
	SessionID       string
	ParentMessageID string   // 首轮空;续轮 = 上轮 response_message_id
	Prompt          string   // 拍平后的完整多轮字符串
	ModelType       string   // "default"(快速) | "expert"(专家) | "vision"(识图)
	ThinkingEnabled bool
	SearchEnabled   bool
	RefFileIDs      []string // 识图:已上传文件 id
}

// Complete 发起一次 completion,返回原始 SSE 响应(调用方负责 Close + 解析)。
// 返回的 *http.Response 的 Body 是 p/o/v JSON-Patch 流。
func (c *Client) Complete(token string, req CompletionRequest) (*http.Response, error) {
	body := map[string]any{
		"chat_session_id":  req.SessionID,
		"parent_message_id": req.ParentMessageID,
		"prompt":           req.Prompt,
		"thinking_enabled": req.ThinkingEnabled,
		"search_enabled":   req.SearchEnabled,
		"stream":           true,
	}
	if req.ModelType != "" {
		body["model_type"] = req.ModelType
	}
	if len(req.RefFileIDs) > 0 {
		body["ref_file_ids"] = req.RefFileIDs
	}

	httpReq, err := http.NewRequest(http.MethodPost, c.baseURL+"/api/v0/chat/completion", jsonBody(body))
	if err != nil {
		return nil, err
	}
	c.setHeaders(httpReq, token)
	httpReq.Header.Set("Accept", "text/event-stream")
	// [P0] PoW:多数实现需先 create_pow_challenge 解 challenge 并带 x-ds-pow-response。
	// challenge 为空(免 PoW 账号/区域)时直接放行。
	httpReq.Header.Set("x-ds-pow-response", c.solvePow(token))
	resp, err := c.client.Do(httpReq)
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

// solvePow [P0] 返回 PoW 响应头值。当前为占位:免 PoW 场景返回空;
// 需 PoW 的账号/区域在 P0 验证后移植 masterzerno pow.go 实现 DeepSeekHashV1。
func (c *Client) solvePow(token string) string {
	return ""
}

// StopStream [P0] 中断生成。协议待验证(chat/stop_stream 或 chat_session/delete)。
func (c *Client) StopStream(token, sessionID string) {}

// DeleteSession 删除会话(兜底清理,防账号后台堆积)。
func (c *Client) DeleteSession(token, sessionID string) {
	_, _ = c.doJSON(token, "/api/v0/chat_session/delete", map[string]string{"chat_session_id": sessionID})
}

func jsonBody(v any) io.Reader {
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(v)
	return &buf
}

func rawJSONString(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return strings.Trim(string(raw), `"`)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// setTransportProxy 设置 transport 的代理(HTTP 与 HTTPS 都生效)。
func setTransportProxy(t *http.Transport, proxyURL string) error {
	u := proxyURL
	if !strings.Contains(u, "://") {
		u = "http://" + u
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return err
	}
	t.Proxy = http.ProxyURL(parsed)
	return nil
}
