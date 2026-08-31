package qianwenweb

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"aurora/httpclient"
	"aurora/httpclient/factory"

	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
)

const (
	defaultBase  = "https://chat2.qianwen.com"
	webOrigin    = "https://www.qianwen.com"
	webUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36"
	webAccept    = "application/json, text/event-stream, text/plain, */*"
	chatPath     = "/api/v2/chat"
	webVersion   = "4.2.1"
)

// Client 是千问网页客户端(www.qianwen.com 逆向)。
//
// 认证与风控都靠 cookie:核心是 tongyi_sso_ticket(httpOnly,约 1 年,见 docs/QIANWEN.md);
// 当 WAF 升级后还需要 x5sec 通关 cookie(约 20 分钟,浏览器过滑块后签发)。
// 安全头(clt-acs-sign/bx-ua/eo-clt-actkn 等)服务端不校验,均不需要。
//
// 注意:必须用 Chrome TLS 指纹客户端(bogdanfinn tls-client)。千问 WAF 按 TLS 指纹
// (JA3)风控:Go 标准库 http.Client 请求量一上来就被拦截(返回 RGV587 captcha),
// 浏览器指纹(Chrome_146)实测可长期通过。
type Client struct {
	baseURL   string
	tlsClient factory.Client
	cookies   string // 当前生效的 cookie header(ticket + x5sec 等)
	userID    string // ut / x-device-id(非空即可,服务端不校验绑定)
	// token 池(从文件加载,每行一个完整 cookie header)
	tokens []string
	cursor int
}

// NewClient 构造客户端。tokenFile 是每行一个 cookie header 的池文件
// (与 DeepSeek/GLM 的 token 池一致);也可直接传单个 cookie header。
// userID 为空时生成随机 uuid(实测服务端不校验 ut 绑定)。
func NewClient(baseURL, tokenFile, cookies, userID string) *Client {
	if baseURL == "" {
		baseURL = defaultBase
	}
	if userID == "" {
		userID = uuid.NewString()
	}
	c := &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		tlsClient: factory.NewWebClient(factory.Profile{Mode: factory.ModeTLSFaked}),
		cookies:   cookies,
		userID:    userID,
	}
	if tokenFile != "" {
		if tokens, err := loadTokens(tokenFile); err == nil && len(tokens) > 0 {
			c.tokens = tokens
			if c.cookies == "" {
				c.cookies = c.NextToken()
			}
		}
	}
	return c
}

// HasToken 报告是否有可用的 cookie header(池或直传)。
func (c *Client) HasToken() bool { return c.cookies != "" }

// NextToken 轮询取下一个 cookie header;池空返回当前值。
func (c *Client) NextToken() string {
	if len(c.tokens) == 0 {
		return c.cookies
	}
	t := c.tokens[c.cursor%len(c.tokens)]
	c.cursor++
	return t
}

// SetCookieHeader 切换当前生效的 cookie header(轮换时用)。
func (c *Client) SetCookieHeader(h string) { c.cookies = h }

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

// ChatRequest 是 /api/v2/chat 的请求参数。
type ChatRequest struct {
	Model    string
	Messages []Message
}

// Message 是一条消息(text/plain)。
//
// 千问协议消息无 role 字段:user 消息带 meta_data.ori_query,assistant 消息 meta_data 为空。
type Message struct {
	MimeType string       `json:"mime_type"`
	Content  string       `json:"content"`
	MetaData *MessageMeta `json:"meta_data"`
	Status   string       `json:"status"` // 固定 "complete"
}

// MessageMeta 是消息元数据。
type MessageMeta struct {
	OriQuery string `json:"ori_query,omitempty"`
}

// UserMsg 构造用户消息(带 ori_query)。
func UserMsg(text string) Message {
	return Message{MimeType: "text/plain", Content: text, MetaData: &MessageMeta{OriQuery: text}, Status: "complete"}
}

// AssistantMsg 构造助手消息(无 ori_query)。
func AssistantMsg(text string) Message {
	return Message{MimeType: "text/plain", Content: text, MetaData: &MessageMeta{}, Status: "complete"}
}

// Complete 发起一次 chat,返回 SSE 响应(调用方负责 Close + 解析)。
//
// 每轮用 first_turn + 随机 session/topic id + 全量 messages 历史(无状态,服务端自动建档)。
func (c *Client) Complete(req ChatRequest) (*http.Response, error) {
	if c.cookies == "" {
		return nil, fmt.Errorf("qianwen: no cookie header")
	}
	reqID := uuidNoDash()
	sessionID := uuidNoDash()
	topicID := uuidNoDash()
	body := map[string]any{
		"req_id":           reqID,
		"parent_req_id":    "0",
		"messages":         req.Messages,
		"scene":            "chat",
		"sub_scene":        "",
		"scene_param":      "first_turn",
		"session_id":       sessionID,
		"biz_id":           "ai_qwen",
		"topic_id":         topicID,
		"model":            req.Model,
		"from":             "default",
		"protocol_version": "v2",
		"messages_merge":   false,
		"chat_client":      "h5",
		"deep_search":      nil,
		"temporary":        false,
		"chat_mode":        "quick",
		"bucket":           map[string]any{},
	}
	buf, _ := json.Marshal(body)

	q := url.Values{}
	q.Set("biz_id", "ai_qwen")
	q.Set("fe_version", "1.0.0")
	q.Set("chat_client", "h5")
	q.Set("device", "pc")
	q.Set("fr", "pc")
	q.Set("pr", "qwen")
	q.Set("ut", c.userID)
	q.Set("la", "zh-CN")
	q.Set("tz", "Asia/Shanghai")
	q.Set("wv", webVersion)
	q.Set("ve", webVersion)
	q.Set("nonce", uuidNoDash()[:10])
	q.Set("timestamp", fmt.Sprintf("%d", time.Now().UnixMilli()))

	// 头要求见 docs/QIANWEN.md §二:Accept 必须显式含 text/event-stream(否则空流),
	// Origin/Referer 必须匹配(否则 WAF 人机验证 captcha);安全签名头均不需要。
	// Accept-Encoding 只声明 gzip(tls-client 不解压,手动处理),服务器会尊重声明。
	headers := httpclient.AuroraHeaders{
		"Content-Type":    "application/json",
		"User-Agent":      webUserAgent,
		"Accept":          webAccept,
		"Accept-Encoding": "gzip",
		"Origin":          webOrigin,
		"Referer":         webOrigin + "/chat/" + sessionID,
		"x-device-id":     c.userID,
		"x-platform":      "pc_tongyi",
		"prod_id":         "tongyi",
	}
	cookies := parseCookies(c.cookies)
	resp, err := c.tlsClient.Request(httpclient.POST, c.baseURL+chatPath+"?"+q.Encode(), headers, cookies, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("qianwen chat http %d: %s", resp.StatusCode, truncate(string(data), 300))
	}
	// 解压(tls-client 不做透明解压;服务器按声明一般只回 gzip,防御性兼容 zstd)。
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

func uuidNoDash() string {
	return strings.ReplaceAll(uuid.NewString(), "-", "")
}

// parseCookies 把 cookie header 字符串("k1=v1; k2=v2")解析成 []*http.Cookie。
// 千问 token 池每行就是一个完整 cookie header(含 tongyi_sso_ticket 与 x5sec 等)。
func parseCookies(header string) []*http.Cookie {
	var out []*http.Cookie
	for _, part := range strings.Split(header, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 || kv[0] == "" {
			continue
		}
		out = append(out, &http.Cookie{Name: strings.TrimSpace(kv[0]), Value: strings.TrimSpace(kv[1])})
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
