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
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"aurora/httpclient/factory"
)

const (
	defaultBase   = "https://chat.deepseek.com"
	webUserAgent  = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36"
	clientVersion = "2.3.0"
)

// Client 持有一个网页 token 池并复用 HTTP 连接。
type Client struct {
	baseURL string
	client  factory.Client
	tokens  []string
	cursor  int
}

// NewClient 构造客户端。tokenFile 为空时 tokens 为空(由调用方逐 token 传入)。
func NewClient(baseURL, tokenFile, proxyURL string) (*Client, error) {
	if baseURL == "" {
		baseURL = defaultBase
	}
	c := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		client: factory.NewWebClient(factory.Profile{
			Mode:       factory.ModeTLSFaked, // C1 灰度(2026-09-04):Go JA3 + Chrome UA 是最刺眼的非真人特征
			Upgradable: true,                // 保留:AURORA_LEGACY_IDENTITY=1 可回退 Go 原生
			ProxyURL:   proxyURL,            // 代理语义由工厂内化(clone DefaultTransport + 注入)
		}),
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

// setHeaders 设置与真实网页客户端一致的请求头。
// 实测(P0):WAF 需浏览器级 User-Agent + Origin + Referer;x-client-platform 是 web;
// 认证 = Authorization: Bearer <userToken>(userToken 存在网页 localStorage["userToken"].value)。
func (c *Client) setHeaders(req *http.Request, token string) {
	req.Header.Set("Host", strings.TrimPrefix(c.baseURL, "https://"))
	req.Header.Set("User-Agent", webUserAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Origin", c.baseURL)
	req.Header.Set("Referer", c.baseURL+"/")
	req.Header.Set("x-client-platform", "web")
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
	ParentMessageID string // 首轮空;续轮 = 上轮 response_message_id
	Prompt          string // 拍平后的完整多轮字符串
	ModelType       string // "default"(快速) | "expert"(专家) | "vision"(识图)
	ThinkingEnabled bool
	SearchEnabled   bool
	RefFileIDs      []string // 识图:已上传文件 id
}

// Complete 发起一次 completion,返回原始 SSE 响应(调用方负责 Close + 解析)。
// 返回的 *http.Response 的 Body 是 p/o/v JSON-Patch 流。
//
// 实测(P0):completion 前必须先 create_pow_challenge 并带 x-ds-pow-response,
// 否则 40300 MISSING_HEADER;请求体不含 stream 字段(SSE 是默认),含 preempt。
func (c *Client) Complete(token string, req CompletionRequest) (*http.Response, error) {
	body := map[string]any{
		"chat_session_id":   req.SessionID,
		"parent_message_id": req.ParentMessageID,
		"model_type":        req.ModelType,
		"prompt":            req.Prompt,
		"ref_file_ids":      []string{},
		"thinking_enabled":  req.ThinkingEnabled,
		"search_enabled":    req.SearchEnabled,
		"action":            nil,
		"preempt":           false,
	}
	if req.ModelType == "" {
		body["model_type"] = nil
	}
	// parent_message_id 实测(P0):服务端要 u32 整数(或 null),不能是字符串。
	if req.ParentMessageID == "" {
		body["parent_message_id"] = nil
	} else if n, err := strconv.Atoi(req.ParentMessageID); err == nil {
		body["parent_message_id"] = n
	} else {
		body["parent_message_id"] = req.ParentMessageID
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

	// PoW:先取 challenge 再求解(DeepSeekHashV1,23 轮 Keccak)。
	powHeader, err := c.fetchAndSolvePow(token, "/api/v0/chat/completion")
	if err != nil {
		return nil, fmt.Errorf("pow: %w", err)
	}
	httpReq.Header.Set("x-ds-pow-response", powHeader)

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

// fetchAndSolvePow 请求 create_pow_challenge 并求解,返回 x-ds-pow-response 头值。
func (c *Client) fetchAndSolvePow(token, targetPath string) (string, error) {
	raw, err := c.doJSON(token, "/api/v0/chat/create_pow_challenge", map[string]string{"target_path": targetPath})
	if err != nil {
		return "", err
	}
	return SolvePowForPath(raw)
}

// UploadFile 上传一个文件(识图),返回 file_id。
// 实测(P0):POST /api/v0/file/upload_file(multipart,需先解该 target_path 的 PoW),
// 响应 biz_data.id 即 completion 的 ref_file_ids 元素。
func (c *Client) UploadFile(token, filename, contentType string, data []byte) (string, error) {
	powHeader, err := c.fetchAndSolvePow(token, "/api/v0/file/upload_file")
	if err != nil {
		return "", fmt.Errorf("upload pow: %w", err)
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(data); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/api/v0/file/upload_file", &buf)
	if err != nil {
		return "", err
	}
	c.setHeaders(req, token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("x-ds-pow-response", powHeader)

	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var ar apiResponse
	if err := json.Unmarshal(body, &ar); err != nil {
		return "", fmt.Errorf("decode upload envelope: %w", err)
	}
	if err := ar.err(); err != nil {
		return "", err
	}
	var v struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(ar.Data.BizData, &v); err != nil || v.ID == "" {
		return "", fmt.Errorf("upload: missing file id (raw=%s)", truncate(string(ar.Data.BizData), 200))
	}
	return v.ID, nil
}

// FetchFiles 确认上传的文件已就绪(浏览器上传后调 fetch_files 等 READY)。
// 返回状态;status==READY 即可用。
func (c *Client) FetchFiles(token, fileID string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+"/api/v0/file/fetch_files?file_ids="+url.QueryEscape(fileID), nil)
	if err != nil {
		return "", err
	}
	c.setHeaders(req, token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var ar apiResponse
	if err := json.Unmarshal(body, &ar); err != nil {
		return "", fmt.Errorf("decode fetch_files: %w", err)
	}
	if err := ar.err(); err != nil {
		return "", err
	}
	var v struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(ar.Data.BizData, &v)
	if v.Status == "" {
		v.Status = "READY" // 结构不符时乐观放行
	}
	return v.Status, nil
}

// ForkFileToVision 把已上传文件 fork 成 vision 版(实测 P0:识图 completion
// 的 ref_file_ids 必须是 fork 后的新 file_id;缺这步服务端报"发送至识图模式")。
func (c *Client) ForkFileToVision(token, fileID string) (string, error) {
	raw, err := c.doJSON(token, "/api/v0/file/fork_file_task", map[string]string{
		"file_id":       fileID,
		"to_model_type": "vision",
	})
	if err != nil {
		return "", err
	}
	var v struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &v); err != nil || v.ID == "" {
		return "", fmt.Errorf("fork_file_task: missing file id (raw=%s)", truncate(string(raw), 200))
	}
	return v.ID, nil
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
