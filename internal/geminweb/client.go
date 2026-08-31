// Package geminweb 实现 gemini.google.com 网页接口逆向客户端。
//
// 协议要点(2026-08-14 CDP 抓包 + Node 直连验证):
//   - 认证:纯 cookie(SID/SAPISID/HSID 等),无 Authorization header、无 API key。
//     请求 body:application/x-www-form-urlencoded,f.req=<嵌套JSON> + at=<令牌>。
//   - at 令牌 = window.WIZ_global_data.SNlM0e(格式 "base64url前缀:时间戳")。
//   - 生成对话:POST /_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate
//     (URL 带 f.sid / bl / _reqid 等);f.req 内层含 SNlM6e 大令牌(~2.6KB,会话级)。
//   - at + SNlM6e + f.sid 均**会话级固定可复用**(实测:同一会话多次请求不变)。
//   - 响应:Google RPC 帧,每行 [["wrb.fr",null,"<JSON>"], ...]。
//     文本在帧内层 [1][4][0][1][0](rc_id 数组),流式多帧取最新;结束帧 {"44":true}。
//
// 严格限频(防封号):单账号串行(并发 1)、请求间隔 >=2s。多账号轮询分担。
package geminweb

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
	"sync"
	"time"

	"aurora/httpclient"
	"aurora/httpclient/factory"
)

const (
	defaultBase = "https://gemini.google.com"
	webUA       = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36"
	// minInterval 是单账号两次请求的最小间隔(严格限频)。
	minInterval = 2 * time.Second
)

// Account 是一个 Gemini 账号的完整凭据。
// 从浏览器登录 gemini.google.com 后一次性提取(见 docs/GEMINI.md §六):
//   - Cookie:完整 cookie 串(SID/SAPISID/HSID 等)
//   - At:window.WIZ_global_data.SNlM0e 值
//   - SNlM6e:StreamGenerate f.req 内层 [3] 的令牌(从一次真实请求提取)
//   - Fsid:StreamGenerate URL 的 f.sid 参数
//   - Cid/RCID:会话 id 前缀(可选,首轮留空由服务端返回)
//   - PathPrefix:账户路径前缀(如 "/u/1";多账户登录时区分,单账户可为 "")
//   - SessionUuid:f.req 内层 [59] 的会话 UUID(与 x-goog-ext-525005358-jspb 头同值,
//     从一次真实请求提取;2026-08-14 Google 前端升级后硬编码值失效,必须会话级)
type Account struct {
	Cookie string `json:"cookie"`
	At     string `json:"at"`
	SNlM6e string `json:"snlM6e"`
	Fsid   string `json:"fsid"`
	// 可选:会话 id 模板(同一会话续聊用;留空则每次新会话)
	Cid  string `json:"cid,omitempty"`
	RCID string `json:"rcid,omitempty"`
	// 账户路径前缀,如 "/u/1"(URL 在 /_/BardChatUi 之前)
	PathPrefix string `json:"pathPrefix,omitempty"`
	// 会话级 UUID(从一次真实请求提取,见上方注释)
	SessionUuid string `json:"sessionUuid,omitempty"`
}

// Client 是 gemini 客户端,持有账号池并严格限频。
type Client struct {
	accounts []*Account
	mu       sync.Mutex
	lastUsed []time.Time // 每账号上次请求时间
	cursor   int
	tls      factory.Client
}

// NewClient 构造客户端。accounts 至少一个(空则请求时返回错误)。
// 必须用 Chrome TLS 指纹客户端(bogdanfinn):Google 2026-08-14 前端升级后
// 按 JA3/TLS 指纹校验,标准 Go http.Client / curl 返回 BardErrorInfo 1096。
func NewClient(accounts []*Account) *Client {
	c := &Client{
		tls:      factory.NewWebClient(factory.Profile{Mode: factory.ModeTLSFaked}),
		lastUsed: make([]time.Time, len(accounts)),
	}
	c.accounts = accounts
	return c
}

// LoadAccounts 从 JSON 文件加载账号池(数组)。
func LoadAccounts(path string) ([]*Account, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var accounts []*Account
	if err := json.NewDecoder(f).Decode(&accounts); err != nil {
		return nil, err
	}
	return accounts, nil
}

// HasAccount 报告是否有可用账号。
func (c *Client) HasAccount() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.accounts) > 0
}

// nextAccount 轮询选账号,并等待该账号的限频间隔。
// 返回 (账号, 释放函数)。调用方在请求完成后调用释放。
func (c *Client) nextAccount() (*Account, func(), error) {
	c.mu.Lock()
	if len(c.accounts) == 0 {
		c.mu.Unlock()
		return nil, nil, fmt.Errorf("gemini: no account available")
	}
	// 找最早可用的账号(最小 lastUsed)
	best := 0
	for i := 1; i < len(c.accounts); i++ {
		if c.lastUsed[i].Before(c.lastUsed[best]) {
			best = i
		}
	}
	idx := best
	acct := c.accounts[idx]
	wait := minInterval - time.Since(c.lastUsed[idx])
	c.mu.Unlock()
	if wait > 0 {
		time.Sleep(wait) // 严格限频
	}
	c.mu.Lock()
	c.lastUsed[idx] = time.Now()
	c.mu.Unlock()
	release := func() {}
	return acct, release, nil
}

// Delta 是一帧增量。
type Delta struct {
	Text string // 正文增量(相对上一帧)
	Done bool   // 流结束
}

// CompletionRequest 是一次对话请求。
type CompletionRequest struct {
	Prompt string // 用户消息(多轮用拍平文本)
	// 多轮续聊:上一轮响应返回的 rcid。首轮留空。
	PrevRCID string
}

// StreamResult 是整条流的汇总。
type StreamResult struct {
	Text string
	RCID string // 本轮回复的 rc_id(多轮续聊的 PrevRCID)
	Err  string
	Done bool
}

// Complete 发起一次对话,流式返回增量。
func (c *Client) Complete(req CompletionRequest, onDelta func(Delta)) StreamResult {
	var res StreamResult
	acct, _, err := c.nextAccount()
	if err != nil {
		res.Err = err.Error()
		return res
	}
	body, err := c.buildReqBody(acct, req)
	if err != nil {
		res.Err = err.Error()
		return res
	}
	resp, err := c.tls.Request(httpclient.POST, c.streamURL(acct), c.buildHeaders(acct), parseCookies(acct.Cookie), bytes.NewReader(body))
	if err != nil {
		res.Err = fmt.Sprintf("gemini stream: %v", err)
		return res
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		res.Err = fmt.Sprintf("gemini http %d: %s", resp.StatusCode, truncate(string(data), 200))
		return res
	}

	// 解析 Google RPC 帧流
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var lastText string
	var lastRCID string
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line == ")]}'" {
			continue
		}
		// 每帧 [["wrb.fr",null,"<json>"], ...] 或裸长度数字行
		if strings.HasPrefix(line, "[") {
			text, rcid, done := parseRPCFrame(line)
			if rcid != "" {
				lastRCID = rcid
			}
			// done 帧可能与文本帧在同一行(结束标记在前,文本在后)。
			// 不 break,先处理完本行全部帧,流结束后统一收尾。
			if done {
				res.Done = true
			}
			if text != "" && text != lastText {
				cleanText := sanitizeText(text)
				// 防御:清洗可能移除中途出现的卡片链接,导致 lastText 不是 cleanText 前缀。
				if len(cleanText) < len(lastText) || cleanText[:len(lastText)] != lastText {
					lastText = cleanText
					if onDelta != nil {
						onDelta(Delta{Text: cleanText})
					}
					res.Text = cleanText
					continue
				}
				delta := cleanText[len(lastText):]
				lastText = cleanText
				res.Text = cleanText
				if onDelta != nil {
					onDelta(Delta{Text: delta})
				}
			}
		}
	}
	// 流结束:若 done 标记且已拿到文本,正常返回。
	if err := sc.Err(); err != nil && res.Text == "" {
		res.Err = fmt.Sprintf("gemini read: %v", err)
	}
	res.RCID = lastRCID
	if res.Text == "" && res.Err == "" {
		res.Err = "gemini: empty response"
	}
	return res
}

// innerSkeleton 是 StreamGenerate f.req 内层的 97 字段骨架(2026-08-14 抓包)。
// 动态位:<PROMPT>(inner[0])、<IDS>(inner[2])、<SNL6E>(inner[3])、<UUID>(inner[4])。
// 其余字段(长尾开关/功能位)保持抓包原样。
var innerSkeleton = []any{
	"<PROMPT>", []any{"zh-CN"}, "<IDS>", "<SNL6E>", "<UUID>", nil, []any{0}, 1, nil, nil, 1, 0,
	nil, nil, nil, nil, nil, []any{[]any{4}}, 0, nil, nil, nil, nil, nil, nil, nil, nil, 1, nil, nil,
	[]any{4}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, []any{1}, nil, nil, nil, nil, nil,
	nil, nil, nil, nil, nil, nil, 0, nil, nil, nil, nil, nil, "D255675A-C4BA-41A4-A443-B39356B0DE81",
	nil, []any{}, nil, nil, nil, nil, nil, nil, 1, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	1, 1, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, 0, nil, nil, nil, nil, 0,
}

// buildReqBody 构造 f.req + at 的 urlencoded body。
// 从骨架模板出发,只替换 prompt / ids(含 rcid) / SNlM6e / uuid / sessionUuid。
func (c *Client) buildReqBody(acct *Account, req CompletionRequest) ([]byte, error) {
	inner := make([]any, len(innerSkeleton))
	copy(inner, innerSkeleton)
	inner[0] = []any{req.Prompt, 0, nil, nil, nil, nil, 0}
	// ids:[cid, rid, rcid, null×6, Aw...]
	rid := ""
	rcid := req.PrevRCID
	if rcid == "" {
		rcid = acct.RCID
	}
	inner[2] = []any{acct.Cid, rid, rcid, nil, nil, nil, nil, nil, nil, "AwAAAAAAAAAQANM7mBjXKZTJBWnA_xk"}
	inner[3] = acct.SNlM6e
	inner[4] = "43815fc519b6c3ff5bfcc5e4ee164142"
	// 会话级 UUID(f.req [59]);2026-08-14 前端升级后必须与会话一致,
	// 否则报 BardErrorInfo 1096。硬编码 D255675A... 已失效。
	if acct.SessionUuid != "" {
		inner[59] = acct.SessionUuid
	}
	innerJSON, _ := json.Marshal(inner)
	fReq, _ := json.Marshal([]any{nil, string(innerJSON)})
	form := url.Values{}
	form.Set("f.req", string(fReq))
	form.Set("at", acct.At)
	return []byte(form.Encode()), nil
}

// streamURL 构造 StreamGenerate URL(含账户路径前缀 + f.sid)。
func (c *Client) streamURL(acct *Account) string {
	prefix := acct.PathPrefix
	if prefix == "" {
		prefix = ""
	}
	fsidVal := acct.Fsid
	if strings.HasPrefix(acct.Fsid, "f.sid=") {
		fsidVal = strings.TrimPrefix(acct.Fsid, "f.sid=")
	}
	return fmt.Sprintf("%s%s/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate?bl=boq_assistant-bard-web-server_20260812.16_p0&f.sid=%s&hl=zh-CN&pageId=none",
		defaultBase, prefix, fsidVal)
}

// buildHeaders 构造请求头(cookie 用 cookies 参数传,见 parseCookies)。
func (c *Client) buildHeaders(acct *Account) httpclient.AuroraHeaders {
	h := httpclient.AuroraHeaders{
		"Content-Type":  "application/x-www-form-urlencoded;charset=UTF-8",
		"User-Agent":    webUA,
		"Origin":        defaultBase,
		"Referer":       defaultBase + "/u/1/app",
		"X-Same-Domain": "1",
	}
	// Google 前端升级(2026-08-14,bl 20260812)后校验 jspb 头。
	// x-goog-ext-525005358 与会话 UUID 同值;缺头/错值报 BardErrorInfo 1096。
	if acct.SessionUuid != "" {
		h["x-goog-ext-525005358-jspb"] = fmt.Sprintf("[\"%s\",1]", acct.SessionUuid)
	}
	return h
}

// parseCookies 把 cookie header 字符串("k1=v1; k2=v2")解析成 []*http.Cookie。
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

// parseRPCFrame 解析一帧 [["wrb.fr",null,"<json>"]]。
// 返回 (最新正文, rcid, 是否结束)。非 wrb.fr 帧返回空。
func parseRPCFrame(line string) (text, rcid string, done bool) {
	// 帧: [["wrb.fr",null,"<json>"], ...]
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "[[") {
		return "", "", false
	}
	var frames [][]json.RawMessage
	if err := json.Unmarshal([]byte(line), &frames); err != nil {
		return "", "", false
	}
	for _, fr := range frames {
		if len(fr) < 3 {
			continue
		}
		var name string
		if err := json.Unmarshal(fr[0], &name); err != nil || name != "wrb.fr" {
			continue
		}
		var payload string
		if err := json.Unmarshal(fr[2], &payload); err != nil {
			continue
		}
		t, r, d := parsePayload(payload)
		if r != "" {
			rcid = r
		}
		if d {
			done = true
		}
		if t != "" {
			text = t
		}
	}
	return text, rcid, done
}

// parsePayload 解析 wrb.fr 的 payload JSON:
// 文本帧:[null,[cid,rid],meta,null,[[rc_id,[text],...],...]](5 元素)
// 结束帧:[null,[cid,rid],{"44":true,...}](3 元素,无 data[4])
func parsePayload(payload string) (text, rcid string, done bool) {
	var data []any
	if err := json.Unmarshal([]byte(payload), &data); err != nil || len(data) < 3 {
		return "", "", false
	}
	// 结束标记:data[2] 是 map 且含 "44":true
	if meta, ok := data[2].(map[string]any); ok {
		if v, ok := meta["44"].(bool); ok && v {
			done = true
		}
	}
	// rcid:data[1] 是 [cid,rid]
	if ids, ok := data[1].([]any); ok && len(ids) >= 2 {
		if s, ok := ids[1].(string); ok {
			rcid = s
		}
	}
	// 文本:data[4] 是 [[rc_id,[text],...],...](文本帧才有)
	if len(data) < 5 {
		return "", rcid, done
	}
	parts, ok := data[4].([]any)
	if !ok || len(parts) == 0 {
		return "", rcid, done
	}
	for _, p := range parts {
		arr, ok := p.([]any)
		if !ok || len(arr) < 2 {
			continue
		}
		content, ok := arr[1].([]any)
		if !ok || len(content) == 0 {
			continue
		}
		if s, ok := content[0].(string); ok {
			text = s
			if rid, ok := arr[0].(string); ok {
				rcid = rid
			}
		}
	}
	return text, rcid, done
}

// sanitizeText 清洗 Gemini 回复里的网页端占位符:
//   - http://googleusercontent.com/card_content/N —— 搜索卡片引用占位符,
//     网页端渲染成卡片,API 转发时是裸链接,剥离(可能多行/多处)。
func sanitizeText(s string) string {
	lines := strings.Split(s, "\n")
	var out []string
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if strings.HasPrefix(trimmed, "http://googleusercontent.com/card_content/") ||
			strings.HasPrefix(trimmed, "https://googleusercontent.com/card_content/") {
			continue
		}
		out = append(out, l)
	}
	s = strings.Join(out, "\n")
	// 兜底:行内也可能出现(罕见),直接替换
	s = strings.ReplaceAll(s, "http://googleusercontent.com/card_content/", "")
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
