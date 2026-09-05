package handler

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	chatgptrequestconverter "aurora/conversion/requests/chatgpt"
	"aurora/httpclient/bogdanfinn"
	"aurora/internal/accounts"
	"aurora/internal/apierrors"
	"aurora/internal/chatgpt"
	"aurora/internal/config"
	"aurora/internal/httpstream"
	"aurora/internal/provider"
	chatgpt_types "aurora/typings/chatgpt"
	officialtypes "aurora/typings/official"
	"aurora/util"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type ChatHandler struct {
	accountPool *accounts.Pool
	sessions    *SessionManager
	cfg         *config.Config
	providers   *provider.Registry
	// coding 限频(chat 不限):gpt-coding 工具调用是 agent 连发流量
	codingLimiter *provider.CodingLimiter
}

func NewChatHandler(pool *accounts.Pool, cfg *config.Config, providers *provider.Registry) *ChatHandler {
	return &ChatHandler{
		accountPool: pool,
		sessions:    NewSessionManager(),
		cfg:         cfg,
		providers:   providers,
		// ChatGPT 免费账号周限额 + 行为分析,工具调用突发最伤,基础 2s + 抖动 0~2s
		codingLimiter: provider.NewCodingLimiter(2*time.Second, 2*time.Second),
	}
}

// recordProviderOutcome 在 provider 处理结束后按响应状态码记录熔断成败。
// 用 defer 挂在分派点:provider 内部无论多少错误出口都覆盖,无侵入。
//   - HTTP < 500(200/4xx):不计失败 —— 4xx 是请求侧问题,连续 500 才是上游坏了
//   - HTTP >= 500:计一次连续失败,达 3 次摘除 60s(Resolve 跳过 → 走 ChatGPT 兜底)

func (h *ChatHandler) recordProviderOutcome(name string, c *gin.Context) {
	if h.providers == nil {
		return
	}
	if c.Writer.Status() >= 500 {
		if tripped := h.providers.Breaker().RecordFailure(name, fmt.Sprintf("%d", c.Writer.Status())); tripped {
			log.Printf("[breaker] provider %s tripped: consecutive failures reached, cooling 60s", name)
		}
		return
	}
	h.providers.Breaker().RecordSuccess(name)
}

func (h *ChatHandler) Nightmare(c *gin.Context) {
	var original_request officialtypes.APIRequest
	err := c.BindJSON(&original_request)
	if err != nil {
		apierrors.JSONError(c, 400, "invalid_request_error", "Request must be proper JSON", nil, err.Error())
		return
	}
	if len(original_request.Messages) == 0 {
		apierrors.JSONError(c, 400, "invalid_request_error", "Missing required parameter: messages", apierrors.Param("messages"), "missing_required_parameter")
		return
	}

	// coding 封存(2026-09-02):开关关闭时显式 400(不静默转 chat,避免误导;
	// 见 docs/CHATGPT_TOOL_BRIDGE.md —— agent 循环延迟/稳定性不达标,冻结不删除)
	if !h.cfg.CodingEnabled && isCodingModelName(original_request.Model) {
		apierrors.JSONError(c, 400, "invalid_request_error", "coding 模型已封存(2026-09-02,aurora 收敛为对话网关)。如需恢复,设置环境变量 CODING_ENABLED=true 并重启", nil, "coding_disabled")
		return
	}

	// Provider 分派:模型命中 DeepSeek 等新上游时,直接交给 Provider,
	// 不经过 ChatGPT 账号池 / resolveAccount。
	if h.providers != nil {
		if p, canonical := h.providers.ResolveCanonical(original_request.Model); p != nil {
			// 规范化命中(如面板把友好名 GLM-5.3 Flash 当 id):回填真实 id,
			// 否则 provider 内部 byID 精确表查不到
			if canonical != original_request.Model {
				original_request.Model = canonical
			}
			// E1/G2 熔断:按响应状态码记录 provider 成败(>=500 计失败,连续 3 次摘除 60s)
			defer h.recordProviderOutcome(p.Name(), c)
			p.ChatCompletions(c, &original_request)
			return
		}
	}

	// ChatGPT -coding 变体:改写为基础模型(透传上游用真实 slug),强制工具调用。
	// 响应仍回显客户端请求的 -coding id(reqModel 用 requestedModel)。
	requestedModel := original_request.Model
	if base, coding := normalizeCodingModel(original_request.Model); coding {
		if len(original_request.Tools) == 0 {
			apierrors.JSONError(c, 400, "invalid_request_error", "coding 模型(gpt-coding)需要携带 tools 参数", apierrors.Param("tools"), "missing_tools")
			return
		}
		original_request.Model = base
	}

	account, _, err := resolveAccount(c, h.accountPool, h.cfg, original_requestHasFiles(original_request))
	if err != nil {
		apierrors.JSONError(c, 400, "authorization_error", err.Error(), apierrors.Param("Authorization"), 400)
		return
	}
	if account == nil {
		apierrors.NotFoundAccount(c)
		return
	}

	proxyUrl := account.Proxy
	input_tokens := countMessagesTokens(original_request.Messages)

	uid := uuid.NewString()
	// 优先用 account.Client（bootstrap.InitClient 时已绑 fingerprint + proxy）
	// 只有在 account.Client 为 nil（理论上不应发生）才 fallback 到 setupClientWithProxy
	var client *bogdanfinn.TlsClient
	if c, ok := account.Client.(*bogdanfinn.TlsClient); ok && c != nil {
		client = c
	} else {
		client = setupClientWithProxy(proxyUrl)
	}

	// 工具调用模式判定(不再强制非流式:handleToolCalling 内部统一
	// 非流式调用上游攒全文解析,对外按 original_request.Stream 输出 SSE 或 JSON)
	toolsEnabled := toolCallingEnabled(original_request.Tools, h.cfg)

	// 发送上游前清洗历史里的"绕开工具"回复,防止模型被自己之前的
	// 拒绝/推诿文本锚定而每轮重复同样的行为
	sanitizeRefusalHistory(original_request.Messages)

	// Convert the chat request to a ChatGPT request
	translated_request := chatgptrequestconverter.ConvertAPIRequest(original_request, account, proxyUrl, client)

	// 按 conversationID 复用 ChatClientState
	var clientState *chatgpt.ChatClientState
	if translated_request.ConversationID != "" {
		clientState = h.sessions.Get(translated_request.ConversationID)
	}
	if clientState == nil {
		clientState = chatgpt.NewChatClientStateForAccount(account)
	}
	clientState.ConversationID = translated_request.ConversationID
	clientState.ParentMessageID = translated_request.ParentMessageID

	reqModel := requestedModel
	if reqModel == "" {
		reqModel = "auto"
	}

	// 工具调用提前分支
	if toolsEnabled {
		h.handleToolCalling(c, &original_request, account, &chatRequestState{
			client:      client,
			clientState: clientState,
			reqModel:    reqModel,
			uid:         uid,
			proxyUrl:    proxyUrl,
			inputTokens: input_tokens,
		})
		return
	}

	response, wsConn, turnStile, status, err := conversationClientOrder(&client, account, translated_request, proxyUrl, original_request.Stream, clientState, h.accountPool)
	if err != nil {
		apierrors.JSONError(c, status, "request_conversion_error", err.Error(), apierrors.Param("model"), "request_conversion_error")
		return
	}
	defer response.Body.Close()
	if chatgpt.Handle_request_error(c, response) {
		if wsConn != nil {
			wsConn.Close()
			wsConn = nil
		}
		return
	}
	var full_response string
	var full_thinking string
	var conversationID string
	var sentinel []map[string]interface{}
	var stopSent bool
	pingSent := false

	// 记录请求开始时间，用于 TTFT / total-time 计时
	startTime := time.Now()
	ttftSet := false
	var ttftMs int64

	// 提取 instructions / input 用于缓存模拟（与 Responses 路径一致）
	var instructions string
	var inputTextParts []string
	for _, msg := range original_request.Messages {
		if msg.Role == "system" {
			instructions += msg.Text()
		} else {
			inputTextParts = append(inputTextParts, msg.Text())
		}
	}
	inputText := strings.Join(inputTextParts, "\n")
	cacheWriteTokens, cachedTokens := RecordCache(translated_request.ConversationID, instructions, inputText)

	if !h.cfg.StreamMode {
		original_request.Stream = false
	}
	if original_request.Stream {
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Writer.Header().Set("Connection", "keep-alive")
		c.Writer.Header().Set("X-Accel-Buffering", "no")
	}
	for i := h.cfg.MaxContinueCount; i > 0; i-- {
		var continue_info *chatgpt.ContinueInfo
		result := chatgpt.HandlerDetailedWithOptions(c, response, client, account, uid, translated_request, original_request.Stream, reqModel, chatgpt.HandlerDetailedOptions{
			Websocket:        wsConn,
			ClientState:      clientState,
			ArtifactDelivery: original_request.ArtifactDelivery,
			ProxyURL:         proxyUrl,
		})
		wsConn = nil
		continue_info = result.Continue
		full_response += result.Text
		full_thinking += result.ThinkingText
		// 首个输出 token 到达时记录 TTFT（text chunk 已在 HandlerDetailedWithOptions 内写出）
		if result.Text != "" && !ttftSet {
			ttftSet = true
			ttftMs = time.Since(startTime).Milliseconds()
		}
		if result.ConversationID != "" {
			conversationID = result.ConversationID
			h.sessions.Register(conversationID, clientState)
			if !pingSent && turnStile != nil {
				pingSent = true
				lastMsgID := result.ParentMessageID
				pingClient := client
				pingAccount := account
				pingTurnStile := turnStile
				go func() {
					perr := chatgpt.POSTSentinelPing(pingClient, pingAccount, pingTurnStile, conversationID, lastMsgID, clientState)
					if h.cfg.DebugSentinel {
						fmt.Printf("[sentinel-ping] conv=%s lastMsg=%s err=%v\n", conversationID, lastMsgID, perr)
					}
				}()
			}
		}
		sentinel = append(sentinel, result.Sentinel...)
		if result.StopSent {
			stopSent = true
		}
		parentMessageID := result.ParentMessageID
		if continue_info != nil {
			parentMessageID = continue_info.ParentID
		}
		clientState.NoteTurnResult(result.ConversationID, parentMessageID)
		if continue_info == nil {
			break
		}
		translated_request.Messages = nil
		translated_request.Action = "continue"
		translated_request.ConversationID = continue_info.ConversationID
		translated_request.ParentMessageID = continue_info.ParentID

		response, wsConn, _, status, err = conversationClientOrder(&client, account, translated_request, proxyUrl, original_request.Stream, clientState, h.accountPool)
		if err != nil {
			apierrors.JSONError(c, status, "request_conversion_error", err.Error(), apierrors.Param("model"), "request_conversion_error")
			return
		}
		defer response.Body.Close()
		if chatgpt.Handle_request_error(c, response) {
			if wsConn != nil {
				wsConn.Close()
				wsConn = nil
			}
			return
		}
	}
	if c.Writer.Status() != 200 {
		return
	}
	if !original_request.Stream {
		output_tokens := util.CountToken(full_response)
		c.JSON(200, officialtypes.NewChatCompletionWithMetadataAndReasoning(full_response, full_thinking, input_tokens, output_tokens, reqModel, conversationID, sentinel))
	} else {
		if original_request.StreamOptions != nil && original_request.StreamOptions.IncludeUsage {
			output_tokens := util.CountToken(full_response)
			msSinceStart := time.Since(startTime).Milliseconds()
			httpstream.WriteUsageChunk(c, reqModel, input_tokens, output_tokens, cachedTokens, cacheWriteTokens, msSinceStart, ttftMs, ttftSet)
		}
		writeChatCompletionStreamDone(c, stopSent, reqModel, conversationID)
	}
}

func (h *ChatHandler) Responses(c *gin.Context) {
	var responsesRequest officialtypes.ResponsesAPIRequest
	err := c.BindJSON(&responsesRequest)
	if err != nil {
		apierrors.JSONError(c, 400, "invalid_request_error", "Request must be proper JSON", nil, err.Error())
		return
	}

	// coding 封存(同 Nightmare 入口)
	if !h.cfg.CodingEnabled && isCodingModelName(responsesRequest.Model) {
		apierrors.JSONError(c, 400, "invalid_request_error", "coding 模型已封存(2026-09-02,aurora 收敛为对话网关)。如需恢复,设置环境变量 CODING_ENABLED=true 并重启", nil, "coding_disabled")
		return
	}

	// Provider 分派:模型命中 DeepSeek 等新上游时,直接交给 Provider 处理,
	// 不经过 ChatGPT 账号池 / resolveAccount。
	if h.providers != nil {
		if p, canonical := h.providers.ResolveCanonical(responsesRequest.Model); p != nil {
			if canonical != responsesRequest.Model {
				responsesRequest.Model = canonical
			}
			defer h.recordProviderOutcome(p.Name(), c)
			p.Responses(c, &responsesRequest)
			return
		}
	}

	original_request, err := responsesRequest.ToAPIRequest()
	if err != nil {
		apierrors.JSONError(c, 400, "invalid_request_error", err.Error(), apierrors.Param("input"), "invalid_request_error")
		return
	}

	// ChatGPT -coding 变体:改写为基础模型(透传上游用真实 slug),强制工具调用。
	// 响应仍回显客户端请求的 -coding id(reqModel 用 requestedModel)。
	requestedModel := original_request.Model
	if base, coding := normalizeCodingModel(original_request.Model); coding {
		if len(original_request.Tools) == 0 {
			apierrors.JSONError(c, 400, "invalid_request_error", "coding 模型(gpt-coding)需要携带 tools 参数", apierrors.Param("tools"), "missing_tools")
			return
		}
		original_request.Model = base
	}

	account, _, err := resolveAccount(c, h.accountPool, h.cfg, original_requestHasFiles(original_request))
	if err != nil {
		apierrors.JSONError(c, 400, "authorization_error", err.Error(), apierrors.Param("Authorization"), 400)
		return
	}
	if account == nil {
		apierrors.NotFoundAccount(c)
		return
	}
	if !account.Type.Satisfies(accounts.CapResponses) {
		c.JSON(403, gin.H{"error": "Responses API requires a logged-in ChatGPT account."})
		return
	}

	proxyUrl := account.Proxy
	input_tokens := 0
	for _, message := range original_request.Messages {
		input_tokens += util.CountToken(message.Text())
	}

	uid := uuid.NewString()
	// 优先用 account.Client（bootstrap.InitClient 时已绑 fingerprint + proxy）
	var client *bogdanfinn.TlsClient
	if c, ok := account.Client.(*bogdanfinn.TlsClient); ok && c != nil {
		client = c
	} else {
		client = setupClientWithProxy(proxyUrl)
	}

	// 发送上游前清洗历史里的"绕开工具"回复(同 Nightmare)
	sanitizeRefusalHistory(original_request.Messages)

	translated_request := chatgptrequestconverter.ConvertAPIRequest(original_request, account, proxyUrl, client)

	// 按 conversationID 复用 ChatClientState，保持 DeviceID/SessionID 一致
	var clientState *chatgpt.ChatClientState
	if translated_request.ConversationID != "" {
		clientState = h.sessions.Get(translated_request.ConversationID)
	}
	if clientState == nil {
		clientState = chatgpt.NewChatClientStateForAccount(account)
	}
	clientState.ConversationID = translated_request.ConversationID
	clientState.ParentMessageID = translated_request.ParentMessageID
	reqModel := requestedModel
	if reqModel == "" {
		reqModel = "auto"
	}

	// ChatGPT 工具调用(coding):带 tools 的请求走文本协议工具调用,输出 Responses 事件。
	if toolCallingEnabled(original_request.Tools, h.cfg) {
		h.responsesToolCalling(c, &original_request, account, &chatRequestState{
			client:      client,
			clientState: clientState,
			reqModel:    reqModel,
			uid:         uid,
			proxyUrl:    proxyUrl,
			inputTokens: input_tokens,
		})
		return
	}

	// 提取 instructions / input 用于缓存模拟
	var instructions string
	var inputTextParts []string
	for _, msg := range original_request.Messages {
		if msg.Role == "system" {
			instructions += msg.Text()
		} else {
			inputTextParts = append(inputTextParts, msg.Text())
		}
	}
	inputText := strings.Join(inputTextParts, "\n")
	cacheWriteTokens, cachedTokens := RecordCache(translated_request.ConversationID, instructions, inputText)

	streamRequested := responsesRequest.Stream && h.cfg.StreamMode

	// 非流式路径：保持原有行为，使用新的 NewResponsesResponse 签名（含 reasoning + cache）
	if !streamRequested {
		response, wsConn, _, status, err := conversationClientOrder(&client, account, translated_request, proxyUrl, false, clientState, h.accountPool)
		if err != nil {
			apierrors.JSONError(c, status, "request_conversion_error", err.Error(), apierrors.Param("model"), "request_conversion_error")
			return
		}
		defer response.Body.Close()
		if chatgpt.Handle_request_error(c, response) {
			if wsConn != nil {
				wsConn.Close()
				wsConn = nil
			}
			return
		}

		var full_response string
		var full_thinking string
		var conversationID string
		for i := h.cfg.MaxContinueCount; i > 0; i-- {
			var continue_info *chatgpt.ContinueInfo
			result := chatgpt.HandlerDetailedWithOptions(c, response, client, account, uid, translated_request, false, reqModel, chatgpt.HandlerDetailedOptions{
				Websocket:   wsConn,
				ClientState: clientState,
			})
			wsConn = nil
			full_response += result.Text
			full_thinking += result.ThinkingText
			parentMessageID := result.ParentMessageID
			continue_info = result.Continue
			if continue_info != nil {
				parentMessageID = continue_info.ParentID
			}
			clientState.NoteTurnResult(result.ConversationID, parentMessageID)
			if result.ConversationID != "" {
				conversationID = result.ConversationID
				h.sessions.Register(conversationID, clientState)
			}
			if continue_info == nil {
				break
			}
			translated_request.Messages = nil
			translated_request.Action = "continue"
			translated_request.ConversationID = continue_info.ConversationID
			translated_request.ParentMessageID = continue_info.ParentID

			response, wsConn, _, status, err = conversationClientOrder(&client, account, translated_request, proxyUrl, false, clientState, h.accountPool)
			if err != nil {
				apierrors.JSONError(c, status, "request_conversion_error", err.Error(), apierrors.Param("model"), "request_conversion_error")
				return
			}
			defer response.Body.Close()
			if chatgpt.Handle_request_error(c, response) {
				if wsConn != nil {
					wsConn.Close()
					wsConn = nil
				}
				return
			}
		}
		if c.Writer.Status() != 200 {
			return
		}

		output_tokens := util.CountToken(full_response)
		reasoning_tokens := util.CountToken(full_thinking)
		responsesResponse := officialtypes.NewResponsesResponse(full_response, full_thinking, input_tokens, output_tokens, reasoning_tokens, cachedTokens, cacheWriteTokens, reqModel)
		c.JSON(200, responsesResponse)
		return
	}

	// ── 流式路径 ──
	startTime := time.Now()
	respID := "resp_" + uuid.NewString()
	reasoningItemID := "rs_" + uuid.NewString()
	messageItemID := "msg_" + uuid.NewString()

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")

	flusher, _ := c.Writer.(http.Flusher)

	// response.created
	c.Writer.WriteString("event: response.created\ndata: " + responsesCreatedEvent(respID, reqModel) + "\n\n")
	// output_item.added (reasoning, output_index 0)
	c.Writer.WriteString("event: response.output_item.added\ndata: " + responsesOutputItemAddedEvent(0, reasoningItemID, "reasoning") + "\n\n")
	// output_item.added (message, output_index 1)
	c.Writer.WriteString("event: response.output_item.added\ndata: " + responsesOutputItemAddedEvent(1, messageItemID, "message") + "\n\n")
	if flusher != nil {
		c.Writer.WriteHeader(200)
		flusher.Flush()
	}

	response, wsConn, _, _, err := conversationClientOrder(&client, account, translated_request, proxyUrl, true, clientState, h.accountPool)
	if err != nil {
		c.Writer.WriteString("event: response.failed\ndata: " + responsesFailedEvent(err.Error()) + "\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	defer response.Body.Close()
	if chatgpt.Handle_request_error(c, response) {
		if wsConn != nil {
			wsConn.Close()
			wsConn = nil
		}
		c.Writer.WriteString("event: response.failed\ndata: " + responsesFailedEvent("upstream error") + "\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		return
	}

	var full_response string
	var full_thinking string
	var conversationID string
	ttftSet := false
	var ttftMs int64

	for i := h.cfg.MaxContinueCount; i > 0; i-- {
		var continue_info *chatgpt.ContinueInfo
		result := chatgpt.HandlerDetailedWithOptions(c, response, client, account, uid, translated_request, true, reqModel, chatgpt.HandlerDetailedOptions{
			Websocket:   wsConn,
			ClientState: clientState,
		})
		wsConn = nil
		full_response += result.Text
		full_thinking += result.ThinkingText

		// 思维链增量
		if result.ThinkingText != "" {
			reasoningEvt := officialtypes.ResponsesReasoningDeltaEvent{
				Type:         "response.reasoning_text.delta",
				ItemID:       reasoningItemID,
				OutputIndex:  0,
				ContentIndex: 0,
				Delta:        result.ThinkingText,
			}
			c.Writer.WriteString("event: response.reasoning_text.delta\ndata: " + reasoningEvt.String() + "\n\n")
		}

		// 正文增量
		if result.Text != "" {
			if !ttftSet {
				ttftSet = true
				ttftMs = time.Since(startTime).Milliseconds()
			}
			textEvt := officialtypes.ResponsesTextDeltaEvent{
				Type:         "response.output_text.delta",
				ItemID:       messageItemID,
				OutputIndex:  1,
				ContentIndex: 0,
				Delta:        result.Text,
			}
			c.Writer.WriteString("event: response.output_text.delta\ndata: " + textEvt.String() + "\n\n")
		}

		if flusher != nil {
			flusher.Flush()
		}

		parentMessageID := result.ParentMessageID
		continue_info = result.Continue
		if continue_info != nil {
			parentMessageID = continue_info.ParentID
		}
		clientState.NoteTurnResult(result.ConversationID, parentMessageID)
		if result.ConversationID != "" {
			conversationID = result.ConversationID
			h.sessions.Register(conversationID, clientState)
		}
		if continue_info == nil {
			break
		}
		translated_request.Messages = nil
		translated_request.Action = "continue"
		translated_request.ConversationID = continue_info.ConversationID
		translated_request.ParentMessageID = continue_info.ParentID

		response, wsConn, _, _, err = conversationClientOrder(&client, account, translated_request, proxyUrl, true, clientState, h.accountPool)
		if err != nil {
			c.Writer.WriteString("event: response.failed\ndata: " + responsesFailedEvent(err.Error()) + "\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
		defer response.Body.Close()
		if chatgpt.Handle_request_error(c, response) {
			if wsConn != nil {
				wsConn.Close()
				wsConn = nil
			}
			c.Writer.WriteString("event: response.failed\ndata: " + responsesFailedEvent("upstream error") + "\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
	}

	// output_item.done (reasoning)
	c.Writer.WriteString("event: response.output_item.done\ndata: " + responsesOutputItemDoneEvent(0, reasoningItemID, "reasoning", full_thinking) + "\n\n")
	// output_item.done (message)
	c.Writer.WriteString("event: response.output_item.done\ndata: " + responsesOutputItemDoneEvent(1, messageItemID, "message", full_response) + "\n\n")

	output_tokens := util.CountToken(full_response)
	reasoning_tokens := util.CountToken(full_thinking)
	responsesResponse := officialtypes.NewResponsesResponse(full_response, full_thinking, input_tokens, output_tokens, reasoning_tokens, cachedTokens, cacheWriteTokens, reqModel)
	// 在 response.completed 事件里附带 timing（HTTP headers 在首次 Flush 后不可写）
	responsesResponse.MsSinceStart = time.Since(startTime).Milliseconds()
	if ttftSet {
		responsesResponse.MsTTFT = ttftMs
	}
	// response.completed
	c.Writer.WriteString("event: response.completed\ndata: " + responsesCompletedEvent(responsesResponse) + "\n\n")
	c.Writer.WriteString("data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// writeTimingHeader 在非流式响应中设置 timing 头部（仅非流式路径使用）。

// responsesToolCalling 处理 ChatGPT Responses 的工具调用(coding)路径。
//
// 复用 toolCallingRetry 的 <tool_call> 文本协议收集(与 handleToolCalling 同源):
// 上游非流式收集全文 + REFUSAL_RETRIES 重试 + 拒绝分类器 + RecoverFromText 兜底,
// 解析出工具调用后按 Responses 事件(流式)或 JSON(非流式)输出。
// 修复:原流式路径无重试,模型偶发绕开工具直接纯文本回答即"偶发不触发"。

func (h *ChatHandler) Files(c *gin.Context) {
	account, _, err := resolveAccount(c, h.accountPool, h.cfg, true)
	if err != nil {
		apierrors.JSONError(c, 400, "invalid_request_error", "Files API requires a logged-in ChatGPT access token.", nil, "missing_access_token")
		return
	}
	if account == nil || account.Token == "" || !account.Type.Satisfies(accounts.CapFileUpload) {
		apierrors.JSONError(c, 400, "invalid_request_error", "Files API requires a logged-in ChatGPT access token.", nil, "missing_access_token")
		return
	}

	formFile, err := c.FormFile("file")
	if err != nil {
		respondError(c, 400, err)
		return
	}
	file, err := formFile.Open()
	if err != nil {
		respondError(c, 400, err)
		return
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		respondError(c, 400, err)
		return
	}
	if len(data) == 0 {
		apierrors.JSONError(c, 400, "invalid_request_error", "Uploaded file is empty", apierrors.Param("file"), "empty_file")
		return
	}

	contentType := formFile.Header.Get("Content-Type")

	// 使用 account 绑定的 Client（有指纹 + 代理）；不存在则新建
	var fileClient *bogdanfinn.TlsClient
	if c, ok := account.Client.(*bogdanfinn.TlsClient); ok && c != nil {
		fileClient = c
	} else {
		fileClient = bogdanfinn.NewStdClient()
		fileClient.SetCookies("https://chatgpt.com", chatgpt.BasicCookies)
	}

	uploaded, status, err := chatgpt.UploadFile(fileClient, account, account.Proxy, formFile.Filename, contentType, data)
	if err != nil {
		apierrors.JSONError(c, status, "file_upload_error", err.Error(), apierrors.Param("file"), "file_upload_error")
		return
	}
	uploaded.CreatedAt = time.Now().Unix()
	chatgpt.RegisterUploadedFile(uploaded)
	c.JSON(200, uploaded)
}

// chatRequestState 聚合一次 ChatGPT 请求在 handler 内流转的请求级上下文。
// F2 收窄版(2026-09-05):替代 toolCalling 系列函数的 6 指针输出参数
// (client/clientState/reqModel/uid/proxyUrl/inputTokens)——原先全部按指针
// 传递,但其中仅 clientState 在 toolCallingRetry 内会被重新赋值(按会话复用
// 或重建),其余为纯输入。struct 化后"哪些是 in-out"由字段使用显式化,
// 调用方统一从 st 读最终值。

func (h *ChatHandler) ChatGPTConversation(c *gin.Context) {
	var original_request chatgpt_types.ChatGPTRequest
	if err := c.BindJSON(&original_request); err != nil {
		apierrors.JSONError(c, 400, "invalid_request_error", "Request must be proper JSON", nil, err.Error())
		return
	}
	if len(original_request.Messages) > 0 && original_request.Messages[0].Author.Role == "" {
		original_request.Messages[0].Author.Role = "user"
	}

	account, _, err := resolveAccount(c, h.accountPool, h.cfg, false)
	if err != nil {
		apierrors.JSONError(c, 400, "authorization_error", err.Error(), apierrors.Param("Authorization"), 400)
		return
	}
	if account == nil || account.Token == "" || !account.Type.Satisfies(accounts.CapChat) {
		apierrors.NotFoundAccount(c)
		return
	}

	// 使用 account 绑定的 Client（有指纹 + 代理）；不存在则新建
	var convClient *bogdanfinn.TlsClient
	if c, ok := account.Client.(*bogdanfinn.TlsClient); ok && c != nil {
		convClient = c
	} else {
		convClient = bogdanfinn.NewStdClient()
		if account.Proxy != "" {
			convClient.SetProxy(account.Proxy)
		}
	}
	turnStile, status, err := chatgpt.InitSentinel(convClient, account, account.Proxy, 0)
	if err != nil {
		if status == http.StatusUnauthorized {
			h.accountPool.ReportFailure(account)
		}
		c.JSON(status, gin.H{
			"message": err.Error(),
			"type":    "InitTurnStile_request_error",
			"param":   err,
			"code":    status,
		})
		return
	}

	response, err := chatgpt.POSTconversation(convClient, original_request, account, turnStile, account.Proxy)
	if err != nil {
		c.JSON(500, gin.H{"error": "error sending request"})
		return
	}
	defer response.Body.Close()

	if chatgpt.Handle_request_error(c, response) {
		return
	}

	c.Header("Content-Type", response.Header.Get("Content-Type"))
	if cacheControl := response.Header.Get("Cache-Control"); cacheControl != "" {
		c.Header("Cache-Control", cacheControl)
	}

	if _, err := io.Copy(c.Writer, response.Body); err != nil {
		c.JSON(500, gin.H{"error": "Error sending response"})
	}
}
