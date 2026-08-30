// Package grokweb 实现 grok.com 网页接口逆向客户端。
//
// 协议要点(2026-08-13 CDP 抓包 + Node WS 直连验证):
//   - 认证:httpOnly cookie(sso / sso-rw / cf_clearance / grok_device_id 等)
//     随 WebSocket 握手发送;URL = wss://grok.com/ws/mgw/?uid=<x-userid>
//   - 协议:OpenAI Realtime 风格事件(envelope 带 session_id + event):
//     1. session.create        → 建会话(event 里必须带 session 对象,缺则 invalid_envelope)
//     2. conversation.item.create → 发用户消息(x_grok.input_chunks[{text:{text}}])
//     3. response.create       → 请求回复(castle_request_token 可选,实测缺省正常)
//     4. 响应:response.created → output_text.delta → response.done
//   - 多轮:item.create 带 parent_response_id(上一轮 response.id)即可续上下文
package grokweb

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	defaultWSHost = "grok.com"
	webUserAgent  = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36"
)

// Client 是 grok.com 网页客户端,持有一个账号池。
type Client struct {
	accounts []account
	cursor   int
	mu       sync.Mutex
	dialer   *websocket.Dialer
}

type account struct {
	UID    string
	Cookie string
}

// NewClient 构造客户端。cookieFile 每行一个账号:uid|cookie 串。
func NewClient(cookieFile string) (*Client, error) {
	c := &Client{
		dialer: &websocket.Dialer{
			HandshakeTimeout: 15 * time.Second,
			Proxy:            nil, // 直连,不走系统代理
		},
	}
	if cookieFile != "" {
		accounts, err := loadAccounts(cookieFile)
		if err != nil {
			return nil, err
		}
		c.accounts = accounts
	}
	return c, nil
}

// AddAccount 注入一个账号(测试/临时用)。
func (c *Client) AddAccount(uid, cookie string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.accounts = append(c.accounts, account{UID: uid, Cookie: cookie})
}

// HasAccount 报告是否有可用账号。
func (c *Client) HasAccount() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.accounts) > 0
}

// NextAccount 轮询取下一个账号;池空返回零值。
func (c *Client) NextAccount() (uid, cookie string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.accounts) == 0 {
		return "", ""
	}
	a := c.accounts[c.cursor%len(c.accounts)]
	c.cursor++
	return a.UID, a.Cookie
}

// loadAccounts 读账号文件(每行 uid|cookie)。
func loadAccounts(path string) ([]account, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []account
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, "|")
		if idx <= 0 {
			continue
		}
		out = append(out, account{UID: strings.TrimSpace(line[:idx]), Cookie: strings.TrimSpace(line[idx+1:])})
	}
	return out, sc.Err()
}

// CompletionRequest 是一次对话请求(字段对齐网页实测)。
type CompletionRequest struct {
	Prompt           string // 拍平后的完整多轮 prompt
	ParentResponseID string // 首轮空;续轮 = 上一轮 response.id
	Model            string // "grok-3" 等
}

// stream 是一次完成的对话流(单读循环)。
type stream struct {
	ws         *websocket.Conn
	sessionID  string
	responseID string
	onDelta    func(Delta)
}

// Delta 是一帧增量。
type Delta struct {
	Text string
	Done bool
}

// StreamResult 是整条流的汇总。
type StreamResult struct {
	Text       string
	ResponseID string
	Finished   bool
	Err        string
}

// Complete 发起一次完整对话:建连接 → 发消息 → 收增量 → 关连接。
func (c *Client) Complete(req CompletionRequest, onDelta func(Delta)) StreamResult {
	var res StreamResult
	uid, cookie := c.NextAccount()
	if uid == "" || cookie == "" {
		res.Err = "grok: no account available (cookie file empty?)"
		return res
	}
	s, err := c.dial(uid, cookie)
	if err != nil {
		res.Err = err.Error()
		return res
	}
	defer s.ws.Close()

	if err := s.send(req.Prompt, req.ParentResponseID); err != nil {
		res.Err = err.Error()
		return res
	}

	// 单读循环:消费所有帧直到 response.done / error。
	for {
		_, data, err := s.ws.ReadMessage()
		if err != nil {
			if res.Text == "" {
				res.Err = fmt.Sprintf("grok ws read: %v", err)
			}
			return res
		}
		var env envelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		var ev eventEnvelope
		if err := json.Unmarshal(env.Event, &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "response.output_text.delta":
			if ev.Delta != "" {
				res.Text += ev.Delta
				if onDelta != nil {
					onDelta(Delta{Text: ev.Delta})
				}
			}
		case "response.grok.output":
			// 流错误(如 usage_limit_reached 用量限制)在这里上报,
			// 否则会静默走到 response.done 返回 200 空正文(客户端"无反应")。
			if ev.Output.StreamError.Kind != "" {
				msg := ev.Output.StreamError.Message
				if msg == "" {
					msg = ev.Output.StreamError.Kind
				}
				res.Err = fmt.Sprintf("grok %s: %s", ev.Output.StreamError.Kind, msg)
				return res
			}
		case "response.done":
			if ev.Response.ID != "" {
				res.ResponseID = ev.Response.ID
			}
			res.Finished = true
			return res
		case "error":
			msg := "grok error"
			if ev.Error != nil {
				msg = fmt.Sprintf("grok %s (%s): %s", ev.Error.Type, ev.Error.Code, ev.Error.Message)
			}
			res.Err = msg
			return res
		}
	}
}

// envelope 是 WS 帧的统一信封。
type envelope struct {
	SessionID string          `json:"session_id"`
	Event     json.RawMessage `json:"event"`
}

// eventEnvelope 是 event 字段的解析结构。
// 注意:response.output_text.delta 的 delta 字段是**纯字符串**(非 {"text":..} 对象)。
type eventEnvelope struct {
	Type     string `json:"type"`
	Delta    string `json:"delta"`
	Response struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"response"`
	// response.grok.output 的流错误(如 usage_limit_reached)。
	Output struct {
		StreamError struct {
			Kind     string `json:"kind"`
			Message  string `json:"message"`
			Severity string `json:"severity"`
		} `json:"stream_error"`
	} `json:"output"`
	Error *struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// dial 建立 WS 连接并创建会话。
func (c *Client) dial(uid, cookie string) (*stream, error) {
	u := url.URL{Scheme: "wss", Host: defaultWSHost, Path: "/ws/mgw/", RawQuery: "uid=" + uid}
	header := http.Header{
		"Origin":          []string{"https://grok.com"},
		"Cookie":          []string{cookie},
		"User-Agent":      []string{webUserAgent},
		"Accept-Language": []string{"zh-CN,zh;q=0.9"},
		"Pragma":          []string{"no-cache"},
		"Cache-Control":   []string{"no-cache"},
	}
	ws, resp, err := c.dialer.Dial(u.String(), header)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("grok ws dial: %v (status %d)", err, resp.StatusCode)
		}
		return nil, fmt.Errorf("grok ws dial: %w", err)
	}
	s := &stream{ws: ws, sessionID: "sess_" + randHex(16)}
	// session.create:必须带 event.session 对象
	sc := fmt.Sprintf(`{"session_id":%q,"event":{"type":"session.create","event_id":"evt_sess_%d","session":{"model":%q}}}`,
		s.sessionID, time.Now().UnixMilli(), "grok-3")
	if err := ws.WriteMessage(websocket.TextMessage, []byte(sc)); err != nil {
		ws.Close()
		return nil, fmt.Errorf("grok session.create: %w", err)
	}
	// 等 session.created(确认会话可用)
	if err := s.waitFor("session.created"); err != nil {
		ws.Close()
		return nil, fmt.Errorf("grok session: %w", err)
	}
	return s, nil
}

// send 发送用户消息 + response.create。
func (s *stream) send(prompt, parentResponseID string) error {
	item := map[string]any{
		"session_id": s.sessionID,
		"event": map[string]any{
			"type":     "conversation.item.create",
			"event_id": "evt_msg_" + fmt.Sprint(time.Now().UnixMilli()),
			"item": map[string]any{
				"type": "message",
				"role": "user",
				"x_grok": map[string]any{
					"client_message_id": "cm_" + randHex(16),
					"input_chunks":      []map[string]any{{"text": map[string]any{"text": prompt}}},
				},
			},
			"parent_response_id": nullIfEmpty(parentResponseID),
		},
	}
	rc := map[string]any{
		"session_id": s.sessionID,
		"event": map[string]any{
			"type":     "response.create",
			"event_id": "evt_resp_" + fmt.Sprint(time.Now().UnixMilli()),
		},
	}
	ib, _ := json.Marshal(item)
	if err := s.ws.WriteMessage(websocket.TextMessage, ib); err != nil {
		return fmt.Errorf("grok item.create: %w", err)
	}
	rb, _ := json.Marshal(rc)
	if err := s.ws.WriteMessage(websocket.TextMessage, rb); err != nil {
		return fmt.Errorf("grok response.create: %w", err)
	}
	return nil
}

// waitFor 阻塞读取直到出现指定事件类型。
func (s *stream) waitFor(target string) error {
	s.ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer s.ws.SetReadDeadline(time.Time{})
	for {
		_, data, err := s.ws.ReadMessage()
		if err != nil {
			return err
		}
		var env envelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		var ev struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(env.Event, &ev); err != nil {
			continue
		}
		if ev.Type == target {
			return nil
		}
	}
}

// randHex 生成 n 字节随机 hex(轻量 LCG,仅用于 event_id/session 前缀)。
func randHex(n int) string {
	const hex = "0123456789abcdef"
	b := make([]byte, n*2)
	seed := time.Now().UnixNano()
	for i := range b {
		seed = seed*6364136223846793005 + 1442695040888963407
		b[i] = hex[(seed>>33)&0xf]
	}
	return string(b)
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
