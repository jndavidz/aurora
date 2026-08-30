// Package doubaoweb 实现 www.doubao.com 网页接口逆向客户端。
//
// 协议要点(2026-08-14 CDP 抓包 + Node 重放验证):
//   - 端点:POST https://www.doubao.com/chat/completion?<URL参数>
//   - 认证:URL 参数(aid/device_id/fp/msToken/a_bogus/web_id 等,全部会话级
//     固定可复用)+ Cookie(登录凭证,bd_sso/sessionid 等)
//   - 请求 body:JSON(client_meta.conversation_id/bot_id + messages[] + option)
//   - 响应:SSE 事件流
//     SSE_HEARTBEAT → SSE_ACK(question_id/section_id)
//     → FULL_MSG_NOTIFY(回显)→ STREAM_CHUNK(增量文本)×N → 完成
//   - 增量文本在 STREAM_CHUNK.patch_op[].patch_value.content_block[].content.text_block.text
//   - 完成标记:patch_object=50 ext.is_finish="1";patch_type=2 是删除操作
package doubaoweb

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	defaultBase = "https://www.doubao.com"
	webUA       = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36"
	// defaultBotID 是默认豆包 bot。
	defaultBotID = "7338286299411103781"
	// minInterval 单账号限频(字节风控强,保守)。
	minInterval = 2 * time.Second
)

// Account 是一个豆包账号的完整凭据。
// 从浏览器登录 www.doubao.com 后提取一次(见 docs/DOUBAO.md):
//   - Cookie:完整 cookie 串(bd_sso/sessionid 等)
//   - 下述 URL 参数:aid/device_id/fp/msToken/a_bogus/web_id/tea_uuid/web_tab_id
//   - 全部会话级固定可复用(实测跨请求稳定)
type Account struct {
	Cookie   string `json:"cookie"`
	Aid      string `json:"aid"`
	DeviceID string `json:"device_id"`
	FP       string `json:"fp"`
	MsToken  string `json:"ms_token"`
	ABogus   string `json:"a_bogus"`
	WebID    string `json:"web_id"`
	TeaUUID  string `json:"tea_uuid"`
	WebTabID string `json:"web_tab_id"`
	BotID    string `json:"bot_id,omitempty"`
	Version  string `json:"version,omitempty"` // doubao_pc_version / pc_version
	// 会话续接:豆包必须用已有 conversation(不能创建新会话)。
	// 从浏览器抓一次(completion 请求的 client_meta),之后每次请求基于它续接。
	ConvID     string `json:"conv_id,omitempty"`
	SectionID  string `json:"section_id,omitempty"`
	LastMsgIdx int    `json:"last_msg_idx,omitempty"`
	// Query 与 Template(2026-08-21 起):a_bogus 绑定 URL 参数 + body 的
	// conversation 字段 —— 改 prompt 无碍,但参数集/会话字段变了报
	// "common invalid param"。因此 capture-doubao.mjs 整段复用捕获的
	// query(URL 参数含 a_bogus)与 template(完整 postData),aurora 只替换
	// 最后一条消息的文本,其余原样。
	Query    string `json:"query,omitempty"`
	Template string `json:"template,omitempty"`
}

// Client 是豆包客户端,持有账号池并限频。
type Client struct {
	accounts []*Account
	mu       sync.Mutex
	lastUsed []time.Time
	cursor   int
	client   *http.Client
}

// NewClient 构造客户端。accounts 至少一个。
func NewClient(accounts []*Account) *Client {
	return &Client{
		client:   &http.Client{Timeout: 120 * time.Second},
		lastUsed: make([]time.Time, len(accounts)),
		accounts: accounts,
	}
}

// LoadAccounts 从 JSON 文件加载账号池。
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

// nextAccount 轮询选账号并等待限频间隔。
func (c *Client) nextAccount() (*Account, error) {
	c.mu.Lock()
	if len(c.accounts) == 0 {
		c.mu.Unlock()
		return nil, fmt.Errorf("doubao: no account available")
	}
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
		time.Sleep(wait)
	}
	c.mu.Lock()
	c.lastUsed[idx] = time.Now()
	c.mu.Unlock()
	return acct, nil
}

// Delta 是一帧增量。
type Delta struct {
	Text string
	Done bool
}

// CompletionRequest 是一次对话请求。
type CompletionRequest struct {
	Prompt    string    // 用户消息(多轮用拍平文本或消息数组)
	ConvID    string    // 多轮:上一轮 conversation_id(首轮空)
	SectionID string    // 多轮:上一轮 section_id(首轮空)
	MsgIndex  int       // 多轮:上一轮 last_message_index(首轮 0)
	DeepThink bool      // 深度思考
	WebSearch bool      // 联网搜索
	Messages  []Message // 可选:直接给消息数组(多轮精确回放)
}

// Message 是一条历史消息。
type Message struct {
	Role    string // "user" / "assistant"
	Content string
}

// StreamResult 是整条流的汇总。
type StreamResult struct {
	Text      string
	ConvID    string
	SectionID string
	MsgIndex  int
	Err       string
	Done      bool
}

// Complete 发起一次对话,流式返回增量。
// 会话自动续接:基于账号的 ConvID/SectionID/LastMsgIdx(豆包不能创建新会话),
// 成功后更新账号会话状态(线程安全)。
func (c *Client) Complete(req CompletionRequest, onDelta func(Delta)) StreamResult {
	var res StreamResult
	acct, err := c.nextAccount()
	if err != nil {
		res.Err = err.Error()
		return res
	}
	// 若调用方显式指定会话,优先;否则用账号会话
	if req.ConvID == "" {
		c.mu.Lock()
		req.ConvID = acct.ConvID
		req.SectionID = acct.SectionID
		req.MsgIndex = acct.LastMsgIdx
		c.mu.Unlock()
	}
	body, err := c.buildReqBody(acct, req)
	if err != nil {
		res.Err = err.Error()
		return res
	}
	httpReq, err := http.NewRequest(http.MethodPost, c.completionURL(acct), bytes.NewReader(body))
	if err != nil {
		res.Err = err.Error()
		return res
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Cookie", acct.Cookie)
	httpReq.Header.Set("User-Agent", webUA)
	httpReq.Header.Set("Origin", defaultBase)
	httpReq.Header.Set("Referer", defaultBase+"/chat/")
	resp, err := c.client.Do(httpReq)
	if err != nil {
		res.Err = fmt.Sprintf("doubao stream: %v", err)
		return res
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		res.Err = fmt.Sprintf("doubao http %d: %s", resp.StatusCode, truncate(string(data), 200))
		return res
	}

	// 解析 SSE 流
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var lastText string
	var eventName string
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event: ") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		switch eventName {
		case "SSE_ACK":
			var ack struct {
				QueryList []struct {
					MessageIndex int `json:"message_index"`
				} `json:"query_list"`
				AckClientMeta struct {
					ConversationID string `json:"conversation_id"`
					SectionID      string `json:"section_id"`
				} `json:"ack_client_meta"`
			}
			if json.Unmarshal([]byte(data), &ack) == nil {
				res.ConvID = ack.AckClientMeta.ConversationID
				res.SectionID = ack.AckClientMeta.SectionID
				// 服务端返回的新消息索引(下一次 last_message_index 用它,不是简单+1)
				if len(ack.QueryList) > 0 {
					res.MsgIndex = ack.QueryList[0].MessageIndex
				}
			}
		case "STREAM_CHUNK":
			var chunk struct {
				PatchOp []struct {
					PatchObject int `json:"patch_object"`
					PatchType   int `json:"patch_type"`
					PatchValue  struct {
						ContentBlock []struct {
							Content struct {
								TextBlock struct {
									Text string `json:"text"`
								} `json:"text_block"`
							} `json:"content"`
							IsFinish bool `json:"is_finish"`
						} `json:"content_block"`
						TTSContent string `json:"tts_content"`
						Ext        struct {
							IsFinish string `json:"is_finish"`
						} `json:"ext"`
					} `json:"patch_value"`
				} `json:"patch_op"`
			}
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				continue
			}
			for _, op := range chunk.PatchOp {
				if op.PatchObject == 50 && op.PatchValue.Ext.IsFinish == "1" {
					res.Done = true
				}
				if op.PatchType == 2 {
					continue // 删除操作
				}
				// 正文流:patch_object=111 的 tts_content 是完整增量正文
				// (patch_object=1 的 text_block 只含首个字且与 tts 重复,忽略)。
				if t := op.PatchValue.TTSContent; t != "" {
					lastText += t
					res.Text = lastText
					if onDelta != nil {
						onDelta(Delta{Text: t})
					}
				}
				for _, cb := range op.PatchValue.ContentBlock {
					if cb.IsFinish {
						res.Done = true
					}
				}
			}
		case "FULL_MSG_NOTIFY":
			// 回显用户消息,可忽略
		}
	}
	if err := sc.Err(); err != nil && res.Text == "" {
		res.Err = fmt.Sprintf("doubao read: %v", err)
	}
	// 更新账号会话状态(豆包按 conversation 续接,成功后推进索引)
	if res.Err == "" {
		c.mu.Lock()
		if res.ConvID != "" {
			acct.ConvID = res.ConvID
		}
		if res.SectionID != "" {
			acct.SectionID = res.SectionID
		}
		if res.MsgIndex > 0 {
			acct.LastMsgIdx = res.MsgIndex
		}
		c.mu.Unlock()
	}
	if res.Text == "" && res.Err == "" {
		res.Err = "doubao: empty response"
	}
	return res
}

// buildReqBody 构造请求 JSON。
func (c *Client) buildReqBody(acct *Account, req CompletionRequest) ([]byte, error) {
	// 模板模式:解析捕获的 postData,只替换最后一条消息的文本(a_bogus 绑定
	// conversation 字段,必须保留捕获的 conv/section/message_index/local_message_id)。
	if acct.Template != "" {
		var tmpl map[string]any
		if err := json.Unmarshal([]byte(acct.Template), &tmpl); err != nil {
			return nil, fmt.Errorf("doubao template parse: %w", err)
		}
		msgs, _ := tmpl["messages"].([]any)
		if len(msgs) > 0 {
			last, _ := msgs[len(msgs)-1].(map[string]any)
			if cbs, _ := last["content_block"].([]any); len(cbs) > 0 {
				if cb, _ := cbs[0].(map[string]any); cb != nil {
					if content, _ := cb["content"].(map[string]any); content != nil {
						if tb, _ := content["text_block"].(map[string]any); tb != nil {
							tb["text"] = req.Prompt
						}
					}
				}
			}
		}
		// 若调用方给了完整历史,替换 messages(多轮回放;无历史则用模板单条)
		if len(req.Messages) > 0 {
			var hist []any
			for i, m := range req.Messages {
				hist = append(hist, map[string]any{
					"local_message_id": "m" + fmt.Sprint(i),
					"content_block": []any{map[string]any{
						"block_type": 10000,
						"content":    map[string]any{"text_block": map[string]any{"text": m.Content, "icon_url": "", "icon_url_dark": "", "summary": ""}, "pc_event_block": ""},
						"block_id":   "b" + fmt.Sprint(i),
						"parent_id":  "", "meta_info": []any{}, "append_fields": []any{},
					}},
					"message_status": 0,
				})
			}
			tmpl["messages"] = hist
		}
		return json.Marshal(tmpl)
	}
	type textBlock struct {
		Text        string `json:"text"`
		IconURL     string `json:"icon_url"`
		IconURLDark string `json:"icon_url_dark"`
		Summary     string `json:"summary"`
	}
	type blockContent struct {
		TextBlock    textBlock `json:"text_block"`
		PCEventBlock string    `json:"pc_event_block"`
	}
	type contentBlock struct {
		BlockType    int          `json:"block_type"`
		Content      blockContent `json:"content"`
		BlockID      string       `json:"block_id"`
		ParentID     string       `json:"parent_id"`
		MetaInfo     []any        `json:"meta_info"`
		AppendFields []any        `json:"append_fields"`
	}
	type msg struct {
		LocalMessageID string         `json:"local_message_id"`
		ContentBlock   []contentBlock `json:"content_block"`
		MessageStatus  int            `json:"message_status"`
	}
	// 多轮:全量回放历史(用户/助手交替),最后追加新消息。
	// 验证:豆包支持 messages 数组回放(不依赖 conversation 服务端记忆)。
	var msgs []msg
	history := req.Messages
	for _, m := range history {
		msgs = append(msgs, msg{
			LocalMessageID: uuid.NewString(),
			ContentBlock: []contentBlock{{
				BlockType: 10000,
				Content:   blockContent{TextBlock: textBlock{Text: m.Content}},
				BlockID:   uuid.NewString(),
			}},
			MessageStatus: 0,
		})
	}
	// 新消息(若无历史则只有这一条)
	msgs = append(msgs, msg{
		LocalMessageID: uuid.NewString(),
		ContentBlock: []contentBlock{{
			BlockType: 10000,
			Content:   blockContent{TextBlock: textBlock{Text: req.Prompt}},
			BlockID:   uuid.NewString(),
		}},
		MessageStatus: 0,
	})
	option := map[string]any{
		"send_message_scene": "", "create_time_ms": time.Now().UnixMilli(), "collect_id": "",
		"is_audio": false, "answer_with_suggest": false, "agent_mode": 2, "tts_switch": false,
		"need_deep_think": boolToInt(req.DeepThink), "click_clear_context": false,
		"from_suggest": false, "is_regen": false, "is_replace": false, "is_from_click_option": false,
		"is_from_click_softlink": false, "disable_sse_cache": false, "select_text_action": "",
		"is_select_text": false, "resend_for_regen": false, "scene_type": 0,
		"unique_key": uuid.NewString(), "start_seq": 0, "need_create_conversation": false,
		"regen_query_id": []any{}, "edit_query_id": []any{}, "regen_instruction": "",
		"no_replace_for_regen": false, "message_from": 0, "shared_app_name": "", "shared_app_id": "",
		"sse_recv_event_options": map[string]any{"support_chunk_delta": true},
		"is_ai_playground":       false, "is_old_user": true,
		"recovery_option":      map[string]any{"is_recovery": false, "req_create_time_sec": time.Now().Unix(), "append_sse_event_scene": 0},
		"message_storage_type": 0, "related_deleted_message_ids": map[string]any{},
		"connector_info_list": []any{},
		"model_config":        map[string]any{"model_item_key": "0", "model_extra_params": map[string]any{}},
		"aggregate_params":    map[string]any{"model_item_key": "0", "provider_id": ""},
	}
	botID := acct.BotID
	if botID == "" {
		botID = defaultBotID
	}
	body := map[string]any{
		"client_meta": map[string]any{
			"conversation_id":    req.ConvID,
			"bot_id":             botID,
			"last_section_id":    req.SectionID,
			"last_message_index": req.MsgIndex,
		},
		"messages": msgs,
		"option":   option,
	}
	return json.Marshal(body)
}

// completionURL 构造带全部 URL 参数的端点。
func (c *Client) completionURL(acct *Account) string {
	// 模板模式:整段复用捕获的 query(a_bogus 绑定参数集,不可重拼)
	if acct.Query != "" {
		return defaultBase + "/chat/completion?" + acct.Query
	}
	ver := acct.Version
	if ver == "" {
		ver = "3.32.3"
	}
	q := map[string]string{
		"aid":                    acct.Aid,
		"device_id":              acct.DeviceID,
		"device_platform":        "web",
		"doubao_device_platform": "web",
		"doubao_pc_version":      ver,
		"fp":                     acct.FP,
		"language":               "zh",
		"pc_version":             ver,
		"pkg_type":               "release_version",
		"real_aid":               acct.Aid,
		"region":                 "CN",
		"samantha_web":           "1",
		"sys_region":             "CN",
		"tea_uuid":               acct.TeaUUID,
		"tz_name":                "Asia/Shanghai",
		"use-olympus-account":    "1",
		"version_code":           "20800",
		"web_id":                 acct.WebID,
		"web_platform":           "browser",
		"web_tab_id":             acct.WebTabID,
		"msToken":                acct.MsToken,
		"a_bogus":                acct.ABogus,
	}
	var sb strings.Builder
	sb.WriteString(defaultBase + "/chat/completion?")
	first := true
	for k, v := range q {
		if !first {
			sb.WriteString("&")
		}
		first = false
		sb.WriteString(k)
		sb.WriteString("=")
		sb.WriteString(urlQueryEscape(v))
	}
	return sb.String()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func urlQueryEscape(s string) string {
	// 简化转义:空格→%20,+→%2B(与浏览器一致)
	r := strings.NewReplacer(
		" ", "%20", "+", "%2B", "/", "%2F", "=", "%3D",
		"&", "%26", ":", "%3A", ",", "%2C",
	)
	return r.Replace(s)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

var _ = strconv.Itoa
