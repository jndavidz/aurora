package yuanbaoweb

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"aurora/httpclient"
	"aurora/httpclient/bogdanfinn"

	"github.com/klauspost/compress/zstd"
)

const (
	defaultBase    = "https://yuanbao.tencent.com"
	defaultAgentID = "naQivTmsDa"               // 元宝主 agent(hy3/deepseek 共用,页面 /chat/naQivTmsDa)
	webModelField  = "gpt_175B_0404"            // agent 主模型标识,固定;真正的模型由 chatModelId 决定
	webUserAgent   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36"
	webAccept      = "text/event-stream, application/json, text/plain, */*"
	webVersion     = "2.80.10"
	webInstanceID  = "5"
)

// 上游 chatModelId(网页 ModelInfoCacheV1 实测,2026-08-13)。
const (
	ModelHy3     = "hunyuan_gpt_175B_0404"
	ModelDeepSeek = "deep_seek_v3"
)

// tokenPair 是一条登录凭据:网页请求头 X-Uskey + 完整 cookie header。
// 两者都必需(缺任一 401,实测见 docs/YUANBAO.md §二)。
type tokenPair struct {
	Uskey  string
	Cookie string
}

// Client 是腾讯元宝(yuanbao.tencent.com)网页逆向客户端。
//
// 认证与风控(实测结论):
//   - 凭据是 X-Uskey 请求头 + Cookie(hy_user/hy_token 等 .tencent.com cookie),随登录稳定,
//     池文件每行一条 "<uskey>\t<cookie header>"(与千问"完整 cookie header"池同思路)。
//   - 必须用 Chrome TLS 指纹客户端(bogdanfinn tls-client,Chrome_146)。元宝网关(stgw)按
//     TLS 指纹(JA3)风控:curl 可过,node/Go 标准库被拦(401/400),Chrome 指纹实测可长期通过。
type Client struct {
	baseURL   string
	agentID   string
	tlsClient *bogdanfinn.TlsClient
	// token 池
	tokens []tokenPair
	cursor int
	// 当前生效凭据
	uskey  string
	cookie string
}

// NewClient 构造客户端。tokenFile 是每行一条 "<uskey>\t<cookie>" 的池文件
// (X-Uskey 与 cookie 都从已登录浏览器抓取,见 docs/CDP_BROWSER_DEBUG.md)。
// agentID 为空时用默认元宝主 agent。
func NewClient(baseURL, tokenFile, agentID string) *Client {
	if baseURL == "" {
		baseURL = defaultBase
	}
	if agentID == "" {
		agentID = defaultAgentID
	}
	c := &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		agentID:   agentID,
		tlsClient: bogdanfinn.NewStdClient(),
	}
	if tokenFile != "" {
		if tokens, err := loadTokens(tokenFile); err == nil && len(tokens) > 0 {
			c.tokens = tokens
			if c.uskey == "" && c.cookie == "" {
				c.uskey, c.cookie = c.NextToken()
			}
		}
	}
	return c
}

// HasToken 报告是否有可用凭据(池或直传)。
func (c *Client) HasToken() bool { return c.uskey != "" && c.cookie != "" }

// NextToken 轮询取下一条凭据;池空返回当前值。
func (c *Client) NextToken() (string, string) {
	if len(c.tokens) == 0 {
		return c.uskey, c.cookie
	}
	t := c.tokens[c.cursor%len(c.tokens)]
	c.cursor++
	return t.Uskey, t.Cookie
}

// SetCredential 切换当前生效凭据(轮换时用)。
func (c *Client) SetCredential(uskey, cookie string) {
	c.uskey = uskey
	c.cookie = cookie
}

// PoolSize 返回 token 池大小;直传单凭据时视为 1。
func (c *Client) PoolSize() int {
	if len(c.tokens) == 0 {
		return 1
	}
	return len(c.tokens)
}

// loadTokens 读 token 池文件(每行一条,<uskey>\t<cookie>,忽略空行/注释)。
func loadTokens(path string) ([]tokenPair, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []tokenPair
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p := splitCredential(line)
		if p.Uskey != "" && p.Cookie != "" {
			out = append(out, p)
		}
	}
	return out, sc.Err()
}

// splitCredential 解析一行凭据 "<uskey>\t<cookie header>"。
// 兼容整行是单值(历史格式)时拆不出 cookie,返回空。
func splitCredential(line string) tokenPair {
	parts := strings.SplitN(line, "\t", 2)
	if len(parts) != 2 {
		return tokenPair{}
	}
	return tokenPair{Uskey: strings.TrimSpace(parts[0]), Cookie: strings.TrimSpace(parts[1])}
}

// CreateConversation 新建一条会话,返回 chat_id。
// POST /api/user/agent/conversation/create,body {"agentId":...} → {"id": cid}。
func (c *Client) CreateConversation() (string, error) {
	body, _ := json.Marshal(map[string]string{"agentId": c.agentID})
	resp, err := c.tlsClient.Request(httpclient.POST, c.baseURL+"/api/user/agent/conversation/create",
		c.authHeaders("application/json"), nil, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("yuanbao create conversation http %d: %s", resp.StatusCode, truncate(string(data), 300))
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &created); err != nil || created.ID == "" {
		return "", fmt.Errorf("yuanbao create conversation bad response: %s", truncate(string(data), 200))
	}
	return created.ID, nil
}

// ChatRequest 是一次 /api/chat 的参数。
type ChatRequest struct {
	ChatModelID string // hunyuan_gpt_175B_0404(hy3) | deep_seek_v3
	Prompt      string // 拍平后的全量 prompt
	WebSearch   bool   // 网页"自动联网搜索"开关(autoInternetSearch)
}

// Chat 发起一次 chat,返回 SSE 响应(调用方负责 Close + ConsumeStream 解析)。
//
// 每次调用独立 CreateConversation 拿 cid,请求带全量 prompt(无服务端历史依赖,
// 与 DeepSeek 网页通道一致,客户端无状态)。会话用完即弃,不主动清理。
func (c *Client) Chat(req ChatRequest) (*http.Response, error) {
	if !c.HasToken() {
		return nil, fmt.Errorf("yuanbao: no credential (missing YUANBAO_WEB_TOKENS?)")
	}
	cid, err := c.CreateConversation()
	if err != nil {
		return nil, err
	}
	body := c.chatBody(cid, req)
	buf, _ := json.Marshal(body)
	resp, err := c.tlsClient.Request(httpclient.POST, c.baseURL+"/api/chat/"+cid,
		c.chatHeaders(), nil, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("yuanbao chat http %d: %s", resp.StatusCode, truncate(string(data), 300))
	}
	// 解压(tls-client 无透明解压;声明 gzip,服务器按声明回 gzip,防御性兼容 zstd)。
	switch strings.ToLower(resp.Header.Get("Content-Encoding")) {
	case "gzip":
		if zr, err := gzip.NewReader(resp.Body); err == nil {
			resp.Body = zr
		}
	case "zstd":
		if zr, err := zstd.NewReader(resp.Body); err == nil {
			resp.Body = zr.IOReadCloser()
		}
	}
	return resp, nil
}

// chatBody 组装 /api/chat 请求体(结构与网页抓包一致,见 docs/YUANBAO.md §三)。
// model 固定 gpt_175B_0404(agent 主模型标识),实际模型由 chatModelId 决定。
func (c *Client) chatBody(cid string, req ChatRequest) map[string]any {
	extInfo := fmt.Sprintf(`{"modelId":%q,"subModelId":"","supportFunctions":{}}`, req.ChatModelID)
	supportF := []string{}
	if req.WebSearch {
		// 网页自动联网搜索:extInfo 带 internetSearch 字段 + supportFunctions 两项。
		extInfo = fmt.Sprintf(`{"modelId":%q,"subModelId":"","supportFunctions":{"internetSearch":""},"internetSearch":"autoInternetSearch"}`, req.ChatModelID)
		supportF = []string{"openAutoSearchSwitch", "autoInternetSearch"}
	}
	return map[string]any{
		"model":             webModelField,
		"prompt":            req.Prompt,
		"plugin":            "Adaptive",
		"displayPrompt":     req.Prompt,
		"displayPromptType": 1,
		"agentId":           c.agentID,
		"isTemporary":       false,
		"projectId":         "",
		"chatModelId":       req.ChatModelID,
		"supportFunctions":  supportF,
		"docOpenid":         "",
		"options": map[string]any{
			"imageIntention": map[string]any{"needIntentionModel": true, "backendUpdateFlag": 2, "intentionStatus": true},
		},
		"multimedia":        []any{},
		"supportHint":       1,
		"chatModelExtInfo":  extInfo,
		"applicationIdList": []any{},
		"chatSource":        "prompt",
		"version":           "v2",
		"extReportParams":   nil,
		"isAtomInput":       false,
		"conversationId":    cid,
		"offsetOfHour":      8,
		"offsetOfMinute":    0,
	}
}

// authHeaders 是 create 等 JSON 接口的公共请求头。
func (c *Client) authHeaders(contentType string) httpclient.AuroraHeaders {
	h := c.baseHeaders()
	h["Content-Type"] = contentType
	h["Accept"] = "application/json, text/plain, */*"
	return h
}

// chatHeaders 是 /api/chat 的请求头(Content-Type 固定 text/plain;charset=UTF-8)。
func (c *Client) chatHeaders() httpclient.AuroraHeaders {
	h := c.baseHeaders()
	h["Content-Type"] = "text/plain;charset=UTF-8"
	h["Accept"] = webAccept
	h["X-Event-Input-Type"] = "11"
	h["X-Trid-Channel"] = "undefined"
	return h
}

// baseHeaders 是元宝网页请求的公共头。
// X-ID/T-UserID 从 cookie 的 hy_user 派生,X-device-id/X-HY93 从 _qimei_uuid42 派生
// (实测与浏览器一致即可,服务端不校验强绑定)。
func (c *Client) baseHeaders() httpclient.AuroraHeaders {
	xid := cookieValue(c.cookie, "hy_user")
	deviceID := cookieValue(c.cookie, "_qimei_uuid42")
	return httpclient.AuroraHeaders{
		"User-Agent":         webUserAgent,
		"Origin":             c.baseURL,
		"Referer":            c.baseURL + "/chat/" + c.agentID,
		"X-Uskey":            c.uskey,
		"Cookie":             c.cookie,
		"X-ID":               xid,
		"T-UserID":           xid,
		"X-device-id":        deviceID,
		"X-HY93":             deviceID,
		"X-Instance-ID":      webInstanceID,
		"X-WebVersion":       webVersion,
		"X-Web-Third-Source": "main",
		"X-AgentID":          c.agentID,
		"X-Language":         "zh-CN",
		"X-Requested-With":   "XMLHttpRequest",
		"X-Platform":         "win",
		"X-Source":           "web",
		"x-commit-tag":       "19c7ad7c",
		"Accept-Encoding":    "gzip",
	}
}

// cookieValue 从 cookie header 字符串里取指定 cookie 的值。
func cookieValue(header, name string) string {
	for _, part := range strings.Split(header, ";") {
		part = strings.TrimSpace(part)
		kv := strings.SplitN(part, "=", 2)
		if len(kv) == 2 && kv[0] == name {
			return kv[1]
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
