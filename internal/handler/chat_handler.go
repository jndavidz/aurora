package handler

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

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

	"github.com/bogdanfinn/websocket"
	"github.com/gin-gonic/gin"
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

	// 槽位模型只接受带 tools 的 chat 请求:无 tools 时明确 400,
	// 而不是落到 ChatGPT 兜底(消耗账号、返回莫名 4xx)。两表面同口径。
	if h.rejectNativeModelWithoutTools(c, original_request.Model, len(original_request.Tools) > 0) {
		return
	}

	// 方案 B:原生工具透传优先。带 tools 且模型命中槽位时先给 native,
	// 必须插在 ResolveCanonical 分派之前(槽位 provider 不进 providers 列表,
	// 否则会劫持同名模型的普通对话与 /v1/responses)。
	// 熔断期间跳过 native(上游持续 5xx 时别再打空,让方案 A / provider 接手)。
	if len(original_request.Tools) > 0 && h.providers != nil {
		if native := h.providers.NativeTools(); native != nil && native.Handles(original_request.Model) &&
			!h.providers.Breaker().Tripped(native.Name()) {
			defer h.recordProviderOutcome(native.Name(), c)
			c.Set(provider.ProviderNameCtxKey, native.Name())
			native.ChatCompletions(c, &original_request)
			return
		}
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
			// 流式失败上报熔断器用(WP6):emitter Fail 经 context 取 provider 名
			c.Set(provider.ProviderNameCtxKey, p.Name())
			// 方案 A:Provider 工具模拟(仅 chat 表面)。门禁在 provider 命中之内:
			// 配置白名单按 canonical 回填后的模型判断;请求侧还要求 tool_choice
			// 未显式禁止工具、且不是 -coding 变体(见 providerToolEmulationRequestEligible)。
			// provider 未实现 TextCollector 时维持现状,行为与未接线时逐字一致。
			if len(original_request.Tools) > 0 && providerToolEmulationEnabled(h.cfg, original_request.Model) &&
				providerToolEmulationRequestEligible(&original_request) {
				if tc, ok := p.(provider.TextCollector); ok {
					h.handleProviderToolCalling(c, &original_request, tc)
					return
				}
			}
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

	pre, ok := h.prepareChatGPTTurn(c, original_request, requestedModel)
	if !ok {
		return
	}
	account, proxyUrl, client := pre.account, pre.proxyUrl, pre.client
	translated_request := pre.translated
	clientState := pre.clientState
	reqModel, uid := pre.reqModel, pre.uid
	input_tokens := countMessagesTokens(original_request.Messages)
	toolsEnabled := toolCallingEnabled(original_request.Tools, h.cfg)

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

	// 记录请求开始时间(TTFT 基准)。turnLoop 接管首轮发送后,基准点从「首轮响应
	// 到达」提前到「发起首轮」——与 Responses 面原语义一致,TTFT 含首轮上游往返。
	startTime := time.Now()

	// 提取 instructions / input 用于缓存模拟(与 Responses 路径一致)。
	// 注:RecordCache 随之前移,首轮发送失败也会记账(原顺序在 send 之后)。
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

	// continue 轮询循环单点(chatgptTurnLoop,spec arch-deepening-phase2 WP2);
	// sentinel 保活 ping 仅本表面发送(keepAlivePing=true,现状行为)。
	sendTurn := func() (*http.Response, *websocket.Conn, *chatgpt.TurnStile, *bogdanfinn.TlsClient, int, error) {
		resp, ws, ts, st, serr := conversationClientOrder(&client, account, translated_request, proxyUrl, original_request.Stream, clientState, h.accountPool)
		return resp, ws, ts, client, st, serr
	}
	turnRes, tlErr := h.chatgptTurnLoop(c, account, &translated_request, clientState, sendTurn, original_request.Stream, uid, reqModel, startTime,
		chatgpt.HandlerDetailedOptions{ArtifactDelivery: original_request.ArtifactDelivery, ProxyURL: proxyUrl}, true, nil)
	if tlErr != nil {
		if tlErr.kind == turnSendFailed {
			apierrors.JSONError(c, tlErr.status, "request_conversion_error", tlErr.msg, apierrors.Param("model"), "request_conversion_error")
		}
		return
	}
	full_response := turnRes.text
	full_thinking := turnRes.thinking
	conversationID := turnRes.conversationID
	sentinel := turnRes.sentinel
	stopSent := turnRes.stopSent
	ttftSet := turnRes.ttftSet
	ttftMs := turnRes.ttftMs

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

	// 槽位模型只接受带 tools 的 chat 请求(同 Nightmare 入口)。
	if h.rejectNativeModelWithoutTools(c, responsesRequest.Model, len(responsesRequest.Tools) > 0) {
		return
	}

	// 方案 B:原生工具透传优先(Responses 表面)。带 tools 且模型命中槽位时
	// 先给 native,同样插在 ResolveCanonical 分派之前并挂熔断/ctx 上报;
	// 熔断期间跳过槽位,与 chat 表面口径一致。
	if len(responsesRequest.Tools) > 0 && h.providers != nil {
		if native := h.providers.NativeTools(); native != nil && native.Handles(responsesRequest.Model) &&
			!h.providers.Breaker().Tripped(native.Name()) {
			defer h.recordProviderOutcome(native.Name(), c)
			c.Set(provider.ProviderNameCtxKey, native.Name())
			native.Responses(c, &responsesRequest)
			return
		}
	}

	// Provider 分派:模型命中 DeepSeek 等新上游时,直接交给 Provider 处理,
	// 不经过 ChatGPT 账号池 / resolveAccount。
	// 方案 A(provider 工具模拟)仅接 /v1/chat/completions;Responses 表面的
	// 工具模拟为后续工作,带 tools 的 Responses 请求维持现状。
	if h.providers != nil {
		if p, canonical := h.providers.ResolveCanonical(responsesRequest.Model); p != nil {
			if canonical != responsesRequest.Model {
				responsesRequest.Model = canonical
			}
			defer h.recordProviderOutcome(p.Name(), c)
			c.Set(provider.ProviderNameCtxKey, p.Name())
			p.Responses(c, &responsesRequest)
			return
		}
	}

	// previous_response_id:复用上一次响应的 ChatGPT 会话状态(conversation/parent),
	// 并用历史 call_id→name 映射补齐本次 input 里未标名的工具结果消息。
	var previousClientState *chatgpt.ChatClientState
	if responsesRequest.PreviousResponseID != "" {
		var callNames map[string]string
		previousClientState, callNames = h.sessions.GetResponse(responsesRequest.PreviousResponseID)
		if previousClientState == nil {
			apierrors.JSONError(c, 400, "invalid_request_error",
				"Unknown previous_response_id: "+responsesRequest.PreviousResponseID,
				apierrors.Param("previous_response_id"), "invalid_previous_response_id")
			return
		}
		responsesRequest.ApplyFunctionCallNames(callNames)
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

	pre, ok := h.prepareChatGPTTurn(c, original_request, requestedModel)
	if !ok {
		return
	}
	if !pre.account.Type.Satisfies(accounts.CapResponses) {
		c.JSON(403, gin.H{"error": "Responses API requires a logged-in ChatGPT account."})
		return
	}
	account, proxyUrl, client := pre.account, pre.proxyUrl, pre.client
	translated_request := pre.translated
	clientState := pre.clientState
	if previousClientState != nil {
		clientState = previousClientState
		translated_request.ConversationID = clientState.ConversationID
		translated_request.ParentMessageID = clientState.ParentMessageID
	}
	reqModel, uid := pre.reqModel, pre.uid
	input_tokens := 0
	for _, message := range original_request.Messages {
		input_tokens += util.CountToken(message.Text())
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
		// continue 轮询循环单点(chatgptTurnLoop);Responses 不发 sentinel ping
		// (keepAlivePing=false,现状行为;是否全开见 spec 观察项)。
		sendTurn := func() (*http.Response, *websocket.Conn, *chatgpt.TurnStile, *bogdanfinn.TlsClient, int, error) {
			resp, ws, ts, st, serr := conversationClientOrder(&client, account, translated_request, proxyUrl, false, clientState, h.accountPool)
			return resp, ws, ts, client, st, serr
		}
		turnRes, tlErr := h.chatgptTurnLoop(c, account, &translated_request, clientState, sendTurn, false, uid, reqModel, time.Now(), chatgpt.HandlerDetailedOptions{}, false, nil)
		if tlErr != nil {
			if tlErr.kind == turnSendFailed {
				apierrors.JSONError(c, tlErr.status, "request_conversion_error", tlErr.msg, apierrors.Param("model"), "request_conversion_error")
			}
			return
		}
		if c.Writer.Status() != 200 {
			return
		}

		output_tokens := util.CountToken(turnRes.text)
		reasoning_tokens := util.CountToken(turnRes.thinking)
		responsesResponse := officialtypes.NewResponsesResponse(turnRes.text, turnRes.thinking, input_tokens, output_tokens, reasoning_tokens, cachedTokens, cacheWriteTokens, reqModel)
		h.sessions.RegisterResponse(responsesResponse.ID, clientState, nil)
		c.JSON(200, responsesResponse)
		return
	}

	// ── 流式路径 ──
	startTime := time.Now()
	e := provider.NewResponsesEmitter(c, reqModel, true)

	// continue 轮询循环单点(chatgptTurnLoop);轮级增量经 onTurn 发 emitter。
	// Responses 不发 sentinel ping(keepAlivePing=false,现状行为)。
	sendTurn := func() (*http.Response, *websocket.Conn, *chatgpt.TurnStile, *bogdanfinn.TlsClient, int, error) {
		resp, ws, ts, st, serr := conversationClientOrder(&client, account, translated_request, proxyUrl, true, clientState, h.accountPool)
		return resp, ws, ts, client, st, serr
	}
	turnRes, tlErr := h.chatgptTurnLoop(c, account, &translated_request, clientState, sendTurn, true, uid, reqModel, startTime, chatgpt.HandlerDetailedOptions{}, false, func(result chatgpt.HandlerResult) {
		e.ReasoningDelta(result.ThinkingText)
		e.TextDelta(result.Text)
	})
	if tlErr != nil {
		if tlErr.kind == turnSendFailed {
			e.Fail(tlErr.msg)
		} else {
			e.Fail("upstream error")
		}
		return
	}

	output_tokens := util.CountToken(turnRes.text)
	reasoning_tokens := util.CountToken(turnRes.thinking)
	responsesResponse := officialtypes.NewResponsesResponse(turnRes.text, turnRes.thinking, input_tokens, output_tokens, reasoning_tokens, cachedTokens, cacheWriteTokens, reqModel)
	// 在 response.completed 事件里附带 timing（HTTP headers 在首次 Flush 后不可写）
	responsesResponse.MsSinceStart = time.Since(startTime).Milliseconds()
	if turnRes.ttftSet {
		responsesResponse.MsTTFT = turnRes.ttftMs
	}
	h.sessions.RegisterResponse(responsesResponse.ID, clientState, nil)
	e.Complete(responsesResponse, turnRes.text, turnRes.thinking)
	e.Done()
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
