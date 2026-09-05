package chatgpt

import (
	"crypto/sha256"
	"math"
	"time"

	"github.com/google/uuid"

	"aurora/internal/accounts"
	chatgpt_types "aurora/typings/chatgpt"
	"aurora/util"
)

type ChatClientState struct {
	DeviceID        string
	SessionID       string
	StartTime       time.Time
	ConversationID  string
	ParentMessageID string
	UserAgent       string
}

func NewChatClientState() *ChatClientState {
	return &ChatClientState{
		DeviceID:        uuid.NewString(),
		SessionID:       uuid.NewString(),
		StartTime:       time.Now(),
		ParentMessageID: "client-created-root",
		UserAgent:       util.FixedUserAgent,
	}
}

// NewChatClientStateForAccount 创建账号级固化的 client state。
// C3(2026-09-05):DeviceID 此前每请求新生成 —— 真实浏览器的 oai-did 存在
// localStorage 长期稳定,每请求轮换是强 bot 信号。改为:
//  1. 优先用账号指纹的 OaiDeviceID(账号级,loader/pool 已固化);
//  2. 指纹为空(临时账号等)时用 token 派生确定性 UUID(同账号恒定,重装不漂移);
//  3. 无账号上下文才回退随机(兼容旧路径)。
// SessionID 仍每请求新生成(对齐浏览器每标签页/每次访问语义)。
func NewChatClientStateForAccount(account *accounts.Account) *ChatClientState {
	state := NewChatClientState()
	if account != nil {
		if account.Fingerprint.OaiDeviceID != "" {
			state.DeviceID = account.Fingerprint.OaiDeviceID
		} else if account.Token != "" {
			state.DeviceID = deterministicDeviceID(account.Token)
		}
	}
	return state
}

// deterministicDeviceID 从 token 派生格式上与 v4 UUID 无异的确定性设备 id
// (sha256 取前 16 字节,重写 version/variant 位)。
func deterministicDeviceID(token string) string {
	sum := sha256.Sum256([]byte("aurora:device:" + token))
	var id uuid.UUID
	copy(id[:], sum[:16])
	id[6] = (id[6] & 0x0f) | 0x40 // version 4
	id[8] = (id[8] & 0x3f) | 0x80 // RFC 4122 variant
	return id.String()
}

func (s *ChatClientState) TimeSinceLoadedSeconds() int {
	if s == nil || s.StartTime.IsZero() {
		return 0
	}
	seconds := math.Round(float64(time.Since(s.StartTime).Milliseconds()) / 1000.0)
	if seconds < 0 {
		return 0
	}
	return int(seconds)
}

func (s *ChatClientState) ApplyToRequest(request *chatgpt_types.ChatGPTRequest) {
	if s == nil || request == nil {
		return
	}
	if request.ParentMessageID == "" || request.ParentMessageID == "client-created-root" {
		request.ParentMessageID = s.ParentMessageID
	}
	if request.ConversationID == "" && s.ConversationID != "" {
		request.ConversationID = s.ConversationID
	}
	// 对齐浏览器: /f/conversation 主请求必须携带 client_prepare_state=success,
	// 告知服务端客户端已完成三态 prepare 流程。缺此字段会被路由到 mini 池。
	// ClientContextualInfo 仍用于 prepare 请求；普通主请求在发送前单独清理。
	request.ClientPrepareState = "success"
	ensureClientContextualInfo(request)
	request.ClientContextualInfo["time_since_loaded"] = s.TimeSinceLoadedSeconds()
}

func (s *ChatClientState) ClientContextualInfo() map[string]interface{} {
	request := chatgpt_types.ChatGPTRequest{}
	ensureClientContextualInfo(&request)
	if s != nil {
		request.ClientContextualInfo["time_since_loaded"] = s.TimeSinceLoadedSeconds()
	} else {
		request.ClientContextualInfo["time_since_loaded"] = 0
	}
	return request.ClientContextualInfo
}

func (s *ChatClientState) NoteTurnResult(conversationID, parentMessageID string) {
	if s == nil {
		return
	}
	if conversationID != "" {
		s.ConversationID = conversationID
	}
	if parentMessageID != "" {
		s.ParentMessageID = parentMessageID
	}
}

func ensureClientContextualInfo(request *chatgpt_types.ChatGPTRequest) {
	if request.ClientContextualInfo == nil {
		request.ClientContextualInfo = map[string]interface{}{}
	}
	if _, ok := request.ClientContextualInfo["is_dark_mode"]; !ok {
		request.ClientContextualInfo["is_dark_mode"] = false
	}
	if _, ok := request.ClientContextualInfo["page_height"]; !ok {
		request.ClientContextualInfo["page_height"] = 1014
	}
	if _, ok := request.ClientContextualInfo["page_width"]; !ok {
		request.ClientContextualInfo["page_width"] = 1055
	}
	if _, ok := request.ClientContextualInfo["pixel_ratio"]; !ok {
		request.ClientContextualInfo["pixel_ratio"] = 1
	}
	if _, ok := request.ClientContextualInfo["screen_height"]; !ok {
		request.ClientContextualInfo["screen_height"] = 1080
	}
	if _, ok := request.ClientContextualInfo["screen_width"]; !ok {
		request.ClientContextualInfo["screen_width"] = 1920
	}
	request.ClientContextualInfo["app_name"] = "chatgpt.com"
}

func requestWithClientState(request chatgpt_types.ChatGPTRequest, state *ChatClientState) chatgpt_types.ChatGPTRequest {
	if state != nil {
		state.ApplyToRequest(&request)
	}
	return request
}
