// Package minimaxweb 实现 agent.minimaxi.com 网页接口逆向客户端(直连,非 CDP 桥)。
//
// 协议要点(2026-08-14 CDP 抓包 + Node/Go 直连实测):
//   - 认证:token(JWT,localStorage._token),同时放在 header 与 URL 查询参数里。
//   - 签名:x-signature = MD5(x-timestamp秒 + "I*7Cf%WZ#S&%1RlZJ&C2" + 请求体)。
//   - 会话:POST /minimax-cloud/api/v1/agent/{agentId}/session(带 yy 头)创建,
//     响应含 session_id。
//   - 发消息:POST agent-stream.minimaxi.com/.../session/{sid}/message,
//     SSE 流(data: JSON 行):type=10 心跳、type=2 消息(用户回显/最终完整消息)、
//     type=6 agent_message_chunk 正文增量(finish:true 为结束标记)。
//   - URL 公共参数(缺一不可,401):device_platform/biz_id/app_id/version_code/unix/
//     timezone_offset/sys_language/lang/uuid/device_id/os_name/browser_name/
//     device_memory/cpu_core_num/browser_language/browser_platform/user_id/
//     screen_width/screen_height/token/client/region
//   - 模型:普通模式 MiniMax-M3(variant:"",team_mode_off);agent 模式 variant:"thinking"。
package minimaxweb

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"aurora/httpclient"
	"aurora/httpclient/bogdanfinn"

	"github.com/google/uuid"
)

const (
	DefaultBase    = "https://agent.minimaxi.com"
	streamBase     = "https://agent-stream.minimaxi.com"
	webUA          = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"
	signSalt       = "I*7Cf%WZ#S&%1RlZJ&C2"
	DefaultAgentID = "430731272630966" // 普通模式 agent(账号级;换账号需更新)
)

// Client 是 minimax 客户端,持有 token 池与设备参数。
type Client struct {
	tokens   []string
	cursor   int
	base     string
	agentID  string
	deviceID string
	userID   string
	uuids    map[string]string // token -> 页面实例级 uuid(跨请求复用,服务端或校验一致性)
	uuidMu   sync.Mutex        // 保护 uuids:并发请求对同一 map 读写会触发
	// fatal error: concurrent map writes(不可 recover)
	tls *bogdanfinn.TlsClient
}

// NewClient 构造客户端。
func NewClient(tokens []string, agentID, deviceID, userID string) *Client {
	return &Client{
		tokens:   tokens,
		base:     DefaultBase,
		agentID:  agentID,
		deviceID: deviceID,
		userID:   userID,
		uuids:    make(map[string]string),
		tls:      bogdanfinn.NewStdClient(),
	}
}

// HasToken 报告是否有可用 token。
func (c *Client) HasToken() bool { return len(c.tokens) > 0 }

// NextToken 轮询取 token。
func (c *Client) NextToken() string {
	if len(c.tokens) == 0 {
		return ""
	}
	t := c.tokens[c.cursor%len(c.tokens)]
	c.cursor++
	return t
}

// instanceUUID 返回该 token 的页面实例级 uuid(跨请求复用)。
// 多个并发请求会同时经过 commonQuery 到达此处,必须加锁保护 map。
func (c *Client) instanceUUID(token string) string {
	c.uuidMu.Lock()
	defer c.uuidMu.Unlock()
	if u, ok := c.uuids[token]; ok {
		return u
	}
	u := uuid.NewString()
	c.uuids[token] = u
	return u
}

// commonQuery 构造公共 URL 查询参数(缺一不可,否则 401)。
func (c *Client) commonQuery(token string) string {
	q := url.Values{}
	q.Set("device_platform", "web")
	q.Set("biz_id", "3")
	q.Set("app_id", "3001")
	q.Set("version_code", "22201")
	q.Set("unix", fmt.Sprintf("%d", time.Now().UnixMilli()))
	q.Set("timezone_offset", "28800")
	q.Set("sys_language", "zh")
	q.Set("lang", "zh")
	q.Set("uuid", c.instanceUUID(token))
	q.Set("device_id", c.deviceID)
	q.Set("os_name", "Windows")
	q.Set("browser_name", "Chrome")
	q.Set("device_memory", "16")
	q.Set("cpu_core_num", "8")
	q.Set("browser_language", "zh-CN")
	q.Set("browser_platform", "Win32")
	q.Set("user_id", c.userID)
	q.Set("screen_width", "1920")
	q.Set("screen_height", "1080")
	q.Set("token", token)
	q.Set("client", "web")
	q.Set("region", "cn")
	return q.Encode()
}

// sign 计算 x-signature。
func sign(ts int64, body string) string {
	sum := md5.Sum([]byte(fmt.Sprintf("%d%s%s", ts, signSalt, body)))
	return hex.EncodeToString(sum[:])
}

// post 发签名 POST。withYY 仅在会话创建时带(实测 message 请求不带)。
func (c *Client) post(token, fullURL, body string, withYY bool) (*http.Response, error) {
	ts := time.Now().Unix()
	h := httpclient.AuroraHeaders{
		"Content-Type": "application/json",
		"token":        token,
		"x-timestamp":  fmt.Sprintf("%d", ts),
		"x-signature":  sign(ts, body),
		"Origin":       c.base,
		"Referer":      c.base + "/",
		"User-Agent":   webUA,
	}
	if withYY {
		// yy 是会话创建时的防伪值:32hex,与签名无关(抓包复用同一值多次成功)
		h["yy"] = "c36d99d9ca994cf9e85b4a588739daa4"
	}
	return c.tls.Request(httpclient.POST, fullURL, h, nil, bytes.NewReader([]byte(body)))
}

// CreateSession 创建会话,返回 session_id。
func (c *Client) CreateSession(token string) (string, error) {
	body := `{"team_mode_off":true,"model":{"provider_id":"minimax","model_id":"MiniMax-M3"}}`
	u := c.base + "/minimax-cloud/api/v1/agent/" + c.agentID + "/session?" + c.commonQuery(token)
	resp, err := c.post(token, u, body, true)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("minimax create session http %d: %s", resp.StatusCode, truncate(string(data), 200))
	}
	var out struct {
		SessionID string `json:"session_id"`
		BaseResp  struct {
			StatusMsg  string `json:"status_msg"`
			StatusCode int    `json:"status_code"`
		} `json:"base_resp"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("minimax session parse: %v (%s)", err, truncate(string(data), 200))
	}
	if out.SessionID == "" {
		return "", fmt.Errorf("minimax session: no session_id, %s", truncate(string(data), 200))
	}
	return out.SessionID, nil
}

// Delta 是一帧增量。
type Delta struct {
	Text string
	Done bool
}

// CompletionRequest 是一次对话请求。
type CompletionRequest struct {
	Prompt string
}

// StreamResult 是整条流的汇总。
type StreamResult struct {
	Text string
	Err  string
	Done bool
}

// Complete 发消息并流式返回增量。
// 每轮新会话(多轮上下文靠全量拍平 prompt,与 Gemini/CDP 同策略)。
func (c *Client) Complete(token string, req CompletionRequest, onDelta func(Delta)) StreamResult {
	var res StreamResult
	sessionID, err := c.CreateSession(token)
	if err != nil {
		res.Err = err.Error()
		return res
	}
	bodyObj := map[string]any{
		"content":      req.Prompt,
		"model":        map[string]string{"provider_id": "minimax", "model_id": "MiniMax-M3", "variant": ""},
		"turn_id":      uuid.NewString(),
		"worktreeMode": false,
	}
	bodyBytes, _ := json.Marshal(bodyObj)
	u := streamBase + "/minimax-cloud/api/v1/session/" + sessionID + "/message?" + c.commonQuery(token)
	resp, err := c.post(token, u, string(bodyBytes), false)
	if err != nil {
		res.Err = fmt.Sprintf("minimax message: %v", err)
		return res
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		res.Err = fmt.Sprintf("minimax message http %d: %s", resp.StatusCode, truncate(string(data), 200))
		return res
	}

	// SSE 解析:data: {json} 行
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			if payload == "[DONE]" {
				res.Done = true
			}
			continue
		}
		var ev struct {
			Type  int `json:"type"`
			Chunk struct {
				MsgContent string `json:"msg_content"`
				Finish     bool   `json:"finish"`
			} `json:"agent_message_chunk"`
			AgentMsg struct {
				MsgContent   string `json:"msg_content"`
				FinishReason string `json:"finish_reason"`
			} `json:"agent_message"`
		}
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case 2: // agent_message:用户回显或错误帧(如 Token Plan 配额耗尽 2056)
			if ev.AgentMsg.FinishReason == "error" && ev.AgentMsg.MsgContent != "" && res.Err == "" {
				res.Err = "minimax: " + ev.AgentMsg.MsgContent
			}
		case 6: // agent_message_chunk:正文增量 / finish 结束
			if ev.Chunk.MsgContent != "" {
				res.Text += ev.Chunk.MsgContent
				if onDelta != nil {
					onDelta(Delta{Text: ev.Chunk.MsgContent})
				}
			}
			if ev.Chunk.Finish {
				res.Done = true
			}
		}
	}
	if err := sc.Err(); err != nil && res.Text == "" {
		res.Err = fmt.Sprintf("minimax read: %v", err)
	}
	if res.Text == "" && res.Err == "" {
		res.Err = "minimax: empty response"
	}
	return res
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
