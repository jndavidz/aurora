package handler

import (
	"fmt"
	"net/http"
	"time"

	chatgptrequestconverter "aurora/conversion/requests/chatgpt"
	"aurora/internal/apierrors"
	officialtypes "aurora/typings/official"
	"github.com/google/uuid"

	"aurora/httpclient/bogdanfinn"
	"aurora/internal/accounts"
	"aurora/internal/chatgpt"
	chatgpt_types "aurora/typings/chatgpt"

	"github.com/bogdanfinn/websocket"
	"github.com/gin-gonic/gin"
)

// ─── ChatGPT 兜底 continue 轮询循环(spec .scratch/arch-deepening-phase2 WP2)───
//
// 此前「首轮请求 → 解析 → 累积 → 会话记账 → continue 重排再发」的状态机在
// chat/completions 与 Responses 两表面(流式+非流式)逐字手抄三份,且已漂移:
// sentinel 保活 ping 只剩 chat/completions 副本在发。本 module 收拢为一份,
// ping 参数化(keepAlivePing,行为保全:仅 chat/completions 传 true),
// 「发一轮上游」抽象为 sendTurn seam(生产 = conversationClientOrder)——
// 兜底路径自此可注入假上游做自动化测试(此前全项目唯一无测试的请求路径)。

// turnSendFn 发一轮上游请求(首轮与 continue 续轮同型),由调用方闭包提供:
// 捕获自身的 client 变量地址与 stream/proxy 等上下文。返回本轮 response、
// websocket 连接、turnstile 票、当前 client(通道内可能已被轮换)与错误。
type turnSendFn func() (*http.Response, *websocket.Conn, *chatgpt.TurnStile, *bogdanfinn.TlsClient, int, error)

// turnLoopErrorKind 标记循环中断的类别,三入口据此选择各自的输出形态。
type turnLoopErrorKind int

const (
	turnSendFailed   turnLoopErrorKind = iota + 1 // conversationClientOrder 失败(未发上游)
	turnHandledError                              // Handle_request_error 已写错误响应
)

type turnLoopError struct {
	kind   turnLoopErrorKind
	status int
	msg    string
}

func (e *turnLoopError) Error() string { return e.msg }

// chatgptTurnResult 一轮对话(可能多个 continue 子轮)的累积结果。
type chatgptTurnResult struct {
	text           string
	thinking       string
	conversationID string
	sentinel       []map[string]interface{}
	stopSent       bool
	ttftSet        bool
	ttftMs         int64
}

// chatgptTurnLoop 执行兜底的一轮对话。职责:首轮发送、按 continue 链续轮、
// 累积正文/思维链、TTFT 记录、sentinel 收集、会话记账(NoteTurnResult/Register)、
// 可选 sentinel ping。输出不在循环内做:流式 token 级直写由 HandlerDetailedWithOptions
// 内部承担(传 stream=true 时),轮级增量经 onTurn 回调交给调用方的 wire emitter。
func (h *ChatHandler) chatgptTurnLoop(
	c *gin.Context,
	account *accounts.Account,
	translatedRequest *chatgpt_types.ChatGPTRequest,
	clientState *chatgpt.ChatClientState,
	sendTurn turnSendFn,
	stream bool,
	uid, reqModel string,
	startTime time.Time,
	baseOpts chatgpt.HandlerDetailedOptions,
	keepAlivePing bool,
	onTurn func(chatgpt.HandlerResult),
) (chatgptTurnResult, *turnLoopError) {
	var res chatgptTurnResult

	response, wsConn, turnStile, client, status, err := sendTurn()
	if err != nil {
		return res, &turnLoopError{kind: turnSendFailed, status: status, msg: err.Error()}
	}
	defer response.Body.Close()
	if chatgpt.Handle_request_error(c, response) {
		if wsConn != nil {
			wsConn.Close()
			wsConn = nil
		}
		return res, &turnLoopError{kind: turnHandledError}
	}

	pingSent := false
	for i := h.cfg.MaxContinueCount; i > 0; i-- {
		opts := baseOpts
		opts.Websocket = wsConn
		opts.ClientState = clientState
		result := chatgpt.HandlerDetailedWithOptions(c, response, client, account, uid, *translatedRequest, stream, reqModel, opts)
		wsConn = nil
		res.text += result.Text
		res.thinking += result.ThinkingText
		// 首个输出 token 到达时记录 TTFT(text chunk 已在 HandlerDetailedWithOptions 内写出)
		if result.Text != "" && !res.ttftSet {
			res.ttftSet = true
			res.ttftMs = time.Since(startTime).Milliseconds()
		}
		if result.ConversationID != "" {
			res.conversationID = result.ConversationID
			h.sessions.Register(result.ConversationID, clientState)
			if keepAlivePing && !pingSent && turnStile != nil {
				pingSent = true
				lastMsgID := result.ParentMessageID
				pingClient := client
				pingAccount := account
				pingTurnStile := turnStile
				go func() {
					perr := chatgpt.POSTSentinelPing(pingClient, pingAccount, pingTurnStile, result.ConversationID, lastMsgID, clientState)
					if h.cfg.DebugSentinel {
						fmt.Printf("[sentinel-ping] conv=%s lastMsg=%s err=%v\n", result.ConversationID, lastMsgID, perr)
					}
				}()
			}
		}
		res.sentinel = append(res.sentinel, result.Sentinel...)
		res.stopSent = res.stopSent || result.StopSent

		parentMessageID := result.ParentMessageID
		if result.Continue != nil {
			parentMessageID = result.Continue.ParentID
		}
		clientState.NoteTurnResult(result.ConversationID, parentMessageID)

		if onTurn != nil {
			onTurn(result)
		}
		if result.Continue == nil {
			break
		}
		translatedRequest.Messages = nil
		translatedRequest.Action = "continue"
		translatedRequest.ConversationID = result.Continue.ConversationID
		translatedRequest.ParentMessageID = result.Continue.ParentID

		response, wsConn, _, client, status, err = sendTurn()
		if err != nil {
			return res, &turnLoopError{kind: turnSendFailed, status: status, msg: err.Error()}
		}
		defer response.Body.Close()
		if chatgpt.Handle_request_error(c, response) {
			if wsConn != nil {
				wsConn.Close()
				wsConn = nil
			}
			return res, &turnLoopError{kind: turnHandledError}
		}
	}
	return res, nil
}

// ─── 兜底两表面的共享前奏(spec WP2 第二步)─────────────────────────

// chatgptPreamble 是 prepareChatGPTTurn 的产物:兜底前奏的全部共享状态。
type chatgptPreamble struct {
	account     *accounts.Account
	proxyUrl    string
	client      *bogdanfinn.TlsClient
	translated  chatgpt_types.ChatGPTRequest
	clientState *chatgpt.ChatClientState
	reqModel    string
	uid         string
}

// prepareChatGPTTurn 合并兜底两表面此前各自复制的入口前奏:账号解析(失败已写
// 响应)、client 获取(优先 account.Client)、拒绝历史清洗(仅工具路径,WP1 语义)、
// 请求转换、会话状态复用、模型名默认化。返回 false 表示响应已写出,调用方直接
// 返回。表面特有判定(Responses 的 CapResponses 检查、input_tokens 口径)留在各入口。
func (h *ChatHandler) prepareChatGPTTurn(c *gin.Context, originalRequest officialtypes.APIRequest, requestedModel string) (*chatgptPreamble, bool) {
	account, _, err := resolveAccount(c, h.accountPool, h.cfg, original_requestHasFiles(originalRequest))
	if err != nil {
		apierrors.JSONError(c, 400, "authorization_error", err.Error(), apierrors.Param("Authorization"), 400)
		return nil, false
	}
	if account == nil {
		apierrors.NotFoundAccount(c)
		return nil, false
	}

	pre := &chatgptPreamble{
		account:  account,
		proxyUrl: account.Proxy,
		uid:      uuid.NewString(),
	}
	// 优先用 account.Client(bootstrap.InitClient 时已绑 fingerprint + proxy);
	// 只有 account.Client 为 nil(理论上不应发生)才 fallback 到 setupClientWithProxy。
	if cl, ok := account.Client.(*bogdanfinn.TlsClient); ok && cl != nil {
		pre.client = cl
	} else {
		pre.client = setupClientWithProxy(pre.proxyUrl)
	}

	// 发送上游前清洗历史里的"绕开工具"回复,防止模型被自己之前的拒绝/推诿
	// 文本锚定。仅工具路径:防锚定是 coding 域策略,纯 chat 请求历史必须原样
	// 发上游(工具入口内部另有同型清洗)。
	if toolCallingEnabled(originalRequest.Tools, h.cfg) {
		sanitizeRefusalHistory(originalRequest.Messages)
	}

	pre.translated = chatgptrequestconverter.ConvertAPIRequest(originalRequest, account, pre.proxyUrl, pre.client)

	// 按 conversationID 复用 ChatClientState(保持 DeviceID/SessionID 一致)
	if pre.translated.ConversationID != "" {
		pre.clientState = h.sessions.Get(pre.translated.ConversationID)
	}
	if pre.clientState == nil {
		pre.clientState = chatgpt.NewChatClientStateForAccount(account)
	}
	pre.clientState.ConversationID = pre.translated.ConversationID
	pre.clientState.ParentMessageID = pre.translated.ParentMessageID

	pre.reqModel = requestedModel
	if pre.reqModel == "" {
		pre.reqModel = "auto"
	}
	return pre, true
}
