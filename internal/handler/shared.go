package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"aurora/httpclient/bogdanfinn"
	"aurora/internal/accounts"
	"aurora/internal/chatgpt"
	"aurora/internal/config"
	chatgpt_types "aurora/typings/chatgpt"
	officialtypes "aurora/typings/official"
	"aurora/util"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/websocket"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

var ErrNoAvailable = errors.New("no available account of the requested type")

func respondError(c *gin.Context, status int, err error) {
	c.JSON(status, gin.H{"error": gin.H{
		"message": err.Error(),
		"type":    "invalid_request_error",
		"param":   nil,
		"code":    http.StatusText(status),
	}})
}

// resolveAccount 从请求 Authorization header 解析账号
// 替代旧的 secretFromAuthorization + accessTokenFromRefreshToken
// 返回 (account, http_status, error)
func resolveAccount(c *gin.Context, pool *accounts.Pool, cfg *config.Config, needsPaid bool) (*accounts.Account, int, error) {
	authHeader := c.GetHeader("Authorization")

	// 提取 Bearer token
	payload := strings.TrimSpace(authHeader)
	if len(payload) >= 7 && strings.EqualFold(payload[:7], "Bearer ") {
		payload = strings.TrimSpace(payload[7:])
	}
	parts := strings.SplitN(payload, ",", 2)
	token := strings.TrimSpace(parts[0])
	teamAccountID := ""
	if len(parts) > 1 {
		teamAccountID = strings.TrimSpace(parts[1])
	}

	// 补充检查专用 header: ChatGPT-Account-ID, Team-Account-ID 等
	for _, header := range []string{"ChatGPT-Account-ID", "Chatgpt-Account-Id", "Team-Account-ID", "X-ChatGPT-Account-ID"} {
		if value := strings.TrimSpace(c.GetHeader(header)); value != "" {
			teamAccountID = value
			break
		}
	}

	expected := cfg.Authorization

	// 无 token 或匹配全局密钥 → 先尝试 free,再 fallback 到 noauth
	if token == "" || (expected != "" && token == expected) {
		acct, err := pool.Acquire(accounts.TypeFree)
		if err != nil || acct == nil {
			// free 池空时(无 session/access/refresh token 账号),fallback 到 noauth(UUID 设备)
			acct, err = pool.Acquire(accounts.TypeNoAuth)
		}
		if err != nil || acct == nil {
			return nil, http.StatusUnauthorized, ErrNoAvailable
		}
		if needsPaid && acct.Type == accounts.TypeNoAuth {
			return nil, http.StatusForbidden, errors.New("this endpoint requires a logged-in ChatGPT account")
		}
		return acct, http.StatusOK, nil
	}

	// access_token (JWT) → 创建/复用临时账号 (受 ENABLE_EXTERNAL_TOKEN 控制)
	if strings.HasPrefix(token, "eyJ") {
		if !cfg.EnableExternalToken {
			return nil, http.StatusUnauthorized, errors.New("external access token disabled (set ENABLE_EXTERNAL_TOKEN=true)")
		}
		userAgent := c.GetHeader("User-Agent")
		proxyURL := cfg.ProxyURL
		if proxyURL == "" {
			proxyURL = cfg.HTTPProxy
		}
		acct := pool.GetOrCreateTempAccount(token, userAgent, proxyURL)
		acct.TeamUserID = teamAccountID
		return acct, http.StatusOK, nil
	}

	// UUID → noauth 账号
	if _, err := uuid.Parse(token); err == nil {
		if needsPaid {
			return nil, http.StatusForbidden, errors.New("this endpoint requires a paid ChatGPT account")
		}
		acct := accounts.NewAccount(token, accounts.TypeNoAuth, token)
		if err := acct.InitClient(); err != nil {
			return nil, http.StatusInternalServerError, err
		}
		acct.Status = accounts.StatusActive
		return acct, http.StatusOK, nil
	}

	// refresh_token → 换 access_token
	if teamAccountID != "" || len(token) > 64 {
		client := bogdanfinn.NewStdClient()
		result, status, err := chatgpt.GETTokenForRefreshToken(client, token, cfg.ProxyURL)
		if err != nil {
			return nil, status, err
		}
		if data, ok := result.(map[string]interface{}); ok {
			if accessToken, ok := data["access_token"].(string); ok && accessToken != "" {
				acct := accounts.NewAccount(accessToken, accounts.TypeFree, accessToken)
				acct.TeamUserID = teamAccountID
				acct.Proxy = cfg.ProxyURL
				acct.RefreshToken = token
				if err := acct.InitClient(); err != nil {
					return nil, http.StatusInternalServerError, err
				}
				acct.Status = accounts.StatusActive
				return acct, http.StatusOK, nil
			}
		}
		return nil, http.StatusBadRequest, errors.New("refresh token response did not include access_token")
	}

	// 兜底：从池里取
	acct, err := pool.Acquire(accounts.TypeFree)
	if err != nil {
		return nil, http.StatusUnauthorized, ErrNoAvailable
	}
	if needsPaid && acct.Type == accounts.TypeNoAuth {
		return nil, http.StatusForbidden, errors.New("this endpoint requires a logged-in ChatGPT account")
	}
	acct.LastUsed = time.Now()
	return acct, http.StatusOK, nil
}

// conversationClientOrder 执行标准的 conversation 流程：
// sentinel → init → ws → prepare → POST
//
// 对齐 initialize/handlers.go:postConversationGptClientOrder
// pool 参数用于在 sentinel 401 时标记账号不可用
func conversationClientOrder(client **bogdanfinn.TlsClient, account *accounts.Account, translatedRequest chatgpt_types.ChatGPTRequest, proxyUrl string, stream bool, state *chatgpt.ChatClientState, pool *accounts.Pool) (*http.Response, *websocket.Conn, *chatgpt.TurnStile, int, error) {
	if state != nil {
		state.ApplyToRequest(&translatedRequest)
	}
	turnTraceID := uuid.NewString()

	(*client).SetCookies("https://chatgpt.com", chatgpt.BasicCookies)

	turnStile, status, err := chatgpt.InitSentinelWithState(*client, account, proxyUrl, 0, state)
	if err != nil {
		// sentinel 401 说明 token 可能过期，标记账号让 pool 后续绕过
		if status == http.StatusUnauthorized && pool != nil {
			pool.ReportFailure(account)
		}
		return nil, nil, nil, status, err
	}

	chatgpt.POSTConversationInit(*client, account, state)

	var wsConn *websocket.Conn
	if chatgpt.RequiresConversationWebsocket(stream, translatedRequest.ThinkingEffort) && account.Type.Satisfies(accounts.CapWebSocket) {
		wsConn, err = chatgpt.DialChatWebsocketWithStateAndProxy(*client, account, state, proxyUrl)
		if err != nil {
			return nil, nil, nil, http.StatusInternalServerError, err
		}
	}

	conduitToken, err := chatgpt.PrepareConversationConduitFullWithSentinel(*client, translatedRequest, account, proxyUrl, turnTraceID, state, turnStile)
	if err != nil {
		if wsConn != nil {
			wsConn.Close()
		}
		return nil, nil, nil, http.StatusInternalServerError, err
	}

	response, err := chatgpt.POSTconversationPreparedWithState(*client, translatedRequest, account, turnStile, proxyUrl, conduitToken, turnTraceID, state)
	if err != nil {
		if wsConn != nil {
			wsConn.Close()
		}
		return nil, nil, nil, http.StatusInternalServerError, err
	}
	return response, wsConn, turnStile, http.StatusOK, nil
}

// setupClientWithProxy 创建带代理的 std client
func setupClientWithProxy(proxyUrl string) *bogdanfinn.TlsClient {
	client := bogdanfinn.NewStdClient()
	if proxyUrl != "" {
		_ = client.SetProxy(proxyUrl)
	}
	return client
}

// websocketProxyFunc 为 WebSocket 连接配置代理（从原 request.go 复制）
func websocketProxyFunc(proxy string) (func(*fhttp.Request) (*url.URL, error), error) {
	if proxy == "" {
		return fhttp.ProxyFromEnvironment, nil
	}
	proxyURL, err := url.Parse(proxy)
	if err != nil {
		return nil, err
	}
	return fhttp.ProxyURL(proxyURL), nil
}

// original_requestHasFiles 检查请求消息中是否包含文件引用
func original_requestHasFiles(request officialtypes.APIRequest) bool {
	for _, message := range request.Messages {
		if len(message.Files()) > 0 {
			return true
		}
	}
	return false
}

// toolCallingEnabled 根据 Config + Tools 列表判定是否启用工具调用模拟。
func toolCallingEnabled(tools []officialtypes.Tool, cfg *config.Config) bool {
	if cfg != nil && !cfg.ToolCallingEnabled {
		return false
	}
	return len(tools) > 0
}

// countMessagesTokens 统计消息的 token 数
func countMessagesTokens(messages []officialtypes.APIMessage) int {
	total := 0
	for _, message := range messages {
		total += util.CountToken(message.Text())
	}
	return total
}

// writeChatCompletionStreamDone 写入流式结束标记
func writeChatCompletionStreamDone(c *gin.Context, stopSent bool, model string, conversationID string) {
	if !stopSent {
		finalLine := officialtypes.StopChunkWithConversation("stop", model, conversationID)
		c.Writer.WriteString("data: " + finalLine.String() + "\n\n")
		c.Writer.Flush()
	}
	c.Writer.WriteString("data: [DONE]\n\n")
	c.Writer.Flush()
}

// looksLikeSandboxRefusal 检测模型是否声称自己处于隔离环境/无法访问工具。
func looksLikeSandboxRefusal(text string) bool {
	if text == "" {
		return false
	}
	t := strings.ToLower(text)
	markers := []string{
		"/mnt/data", "/workspace", "/home/oai", "filesystem isolado", "ambiente isolado",
		"root linux", "linux/container", "container atual", "não tem acesso ao diret",
		"nao tem acesso ao diret", "não está montado", "nao esta montado",
		"não foi montado", "nao foi montado", "não existe neste ambiente",
		"nao existe neste ambiente", "não pode continuar neste ambiente",
		"não é possível ler", "nao e possivel ler",
		"não foi possível abrir", "nao foi possivel abrir",
		"não foi possível executar", "nao foi possivel executar",
		"falha na interface de execução", "falha no parsing",
		"inferência baseada na estrutura", "inferencia baseada na estrutura",
		"baseada apenas na estrutura",
		// 中文拒绝模式(实测常见:模型声称"没有工具接口/无法访问")
		"没有可用的", "我当前会话", "我目前没有", "当前会话中", "无法访问",
		"我无法读取", "无法读取", "不能读取", "没有提供", "无法实际",
		"未提供这些", "工具列表里未提供", "工具接口",
		// 英文拒绝模式
		"i cannot access", "i can't access", "i cannot read", "i can't read",
		"don't have access", "do not have access", "no access to", "cannot access",
		"not available in this", "no such tool", "tool is not available",
	}
	for _, m := range markers {
		if strings.Contains(t, m) {
			return true
		}
	}
	return false
}

// looksLikeRequestingUserContent 检测模型"向用户索要文件/内容/路径/上传/粘贴"
// 式的停顿文本。这类回复不是任务完成,而是模型放弃工具调用——它明明可以
// 用 read/bash 工具自己读,却让用户把文件内容"提供"过来(实测原文:
// "请把源码内容继续提供(或让我能读取工作区文件),我就继续。")。
// 命中时应继续重试,而不是被"工具结果后散文=完成"的启发式放行。
func looksLikeRequestingUserContent(text string) bool {
	if text == "" {
		return false
	}
	t := strings.ToLower(text)
	markers := []string{
		// 中文:索要文件/内容/路径/上传/粘贴
		"请提供", "请你提供", "请把", "请上传", "上传给", "粘贴", "发给我",
		"让我能读取", "让我读取", "无法读取工作区", "需要你提供", "需要您提供",
		"把源码", "把代码", "把文件内容", "把内容", "告诉我路径", "告诉我文件",
		"直接上传", "请补充", "请给出", "你方便的话",
		// "请继续提供源码读取结果后…" 这类变体(实测原文)
		"请继续提供", "继续提供", "提供源码", "提供文件", "提供内容",
		"读取结果后", "拿到源码", "等你提供", "等待你",
		// 英文:请求用户提供内容
		"please provide", "please share", "please paste", "please upload",
		"please send", "can you provide", "can you share", "could you provide",
		"could you share", "paste the file", "upload the file", "share the file", "send me the file",
		"give me access to", "attach the file", "provide me with",
	}
	for _, m := range markers {
		if strings.Contains(t, m) {
			return true
		}
	}
	return false
}

// looksLikePrematureStop 检测模型"中途停顿"文本:没有 tool_call,却输出
// 进度报告/后续计划/继续承诺(实测原文:读完两个文件后输出
// "我已经读完 manifest.json、background.js 前半部分…当前已确认核心架构"
// 就停下,等用户发"继续";或反复输出"我会按以下顺序通读…读完后整理")。
// 这类回复不是完成,应重试逼它立刻继续调工具,而不是放行。
func looksLikePrematureStop(text string) bool {
	if text == "" {
		return false
	}
	t := strings.ToLower(text)
	// 明确的"部分完成/还没完成"信号
	partial := []string{
		"前半部分", "还没读完", "还没有读完", "尚未读完", "还没通读", "没有通读",
		"还没拿到", "还没有拿到", "尚未拿到", "还没读取", "还没有读取", "还没看",
		"没有拿到正文", "只看到", "只读取", "只拿到", "只有文件列表", "只有目录",
		"先到这里", "暂时先",
	}
	// 明确的"将来才做"计划/继续承诺信号
	future := []string{
		"我继续", "我会继续", "我将继续", "继续通读", "继续读取", "继续阅读",
		"继续分析", "继续完成", "会继续", "再继续", "按以下顺序", "按这个顺序",
		"我会按", "读完后", "等读完", "之后我", "之后再", "让我继续", "待我",
		"等我", "我先读", "先读取", "我再读", "继续补充", "继续深入",
	}
	enFuture := []string{
		"i will continue", "i'll continue", "continue reading", "continue to read",
		"let me continue", "to be continued", "next i will", "will now continue",
		"will continue reading", "i will read", "i'll read", "will continue",
	}
	for _, m := range partial {
		if strings.Contains(t, m) {
			return true
		}
	}
	for _, m := range future {
		if strings.Contains(t, m) {
			return true
		}
	}
	for _, m := range enFuture {
		if strings.Contains(t, m) {
			return true
		}
	}
	return false
}

// looksLikeEnvironmentExcuse 检测模型"推诿环境"文本:拿到了工具输出(如 ls 的
// 相对文件列表),却不自己 pwd/按相对路径读取,反而声称环境/项目目录不可用,
// 让用户"重新连接/打开项目环境/挂载"(实测原文:"当前可用的源码读取环境中
// 没有找到对应的 ai-roundtable 项目目录…请重新连接/打开该项目环境后,我会
// 直接读取…")。这类回复不是完成,应重试逼它用工具定位并读取。
func looksLikeEnvironmentExcuse(text string) bool {
	if text == "" {
		return false
	}
	t := strings.ToLower(text)
	markers := []string{
		// 中文:让用户重新连接/打开/挂载/加载环境或项目
		"请重新连接", "请重新打开", "重新连接/打开", "重新连接或打开",
		"打开该项目环境", "打开项目环境", "打开这个环境", "加载该项目",
		"重新加载", "需要挂载", "请挂载", "挂载项目", "挂载目录",
		"找不到该项目", "没有找到该项目", "未找到该项目", "无法找到项目",
		"没找到项目", "项目目录不可用", "无法定位项目", "无法访问项目",
		"环境不可用", "环境不存在", "环境未挂载", "读取环境中没有",
		"环境中没有找到", "没有找到对应的", "环境后才能", "环境后,我会",
		// 英文
		"please reconnect", "please reopen", "reconnect the", "reopen the",
		"mount the", "could not locate", "cannot locate", "project not found",
		"environment is not", "environment not available", "environment unavailable",
	}
	for _, m := range markers {
		if strings.Contains(t, m) {
			return true
		}
	}
	return false
}

// appendToolDebugLog 把每次工具解析的输入文本和解析结果写入日志文件。
// 带时间戳与耗时,便于关联 aurora_run.log 与定位慢请求。
func appendToolDebugLog(path string, attempt int, elapsed time.Duration, text string, calls []officialtypes.ToolCall) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	callsJSON, _ := json.Marshal(calls)
	ts := time.Now().Format("2006-01-02 15:04:05.000")
	fmt.Fprintf(f, "\n=== %s attempt %d (%.0fms) ===\ntext: %s\ncalls: %s\n", ts, attempt, float64(elapsed.Milliseconds()), text, string(callsJSON))
}

// ── Responses 流式事件构造器 ──

func responsesCreatedEvent(respID, model string) string {
	evt := map[string]interface{}{
		"type": "response.created",
		"response": map[string]interface{}{
			"id": respID, "object": "response", "created_at": time.Now().Unix(),
			"model": model, "status": "in_progress",
		},
	}
	b, _ := json.Marshal(evt)
	return string(b)
}

func responsesOutputItemAddedEvent(outputIndex int, itemID, itemType string) string {
	evt := map[string]interface{}{
		"type":         "response.output_item.added",
		"output_index": outputIndex,
		"item": map[string]interface{}{
			"id": itemID, "type": itemType, "status": "in_progress",
		},
	}
	b, _ := json.Marshal(evt)
	return string(b)
}

func responsesOutputItemDoneEvent(outputIndex int, itemID, itemType, text string) string {
	item := map[string]interface{}{
		"id": itemID, "type": itemType, "status": "completed",
	}
	if itemType == "message" {
		item["role"] = "assistant"
		item["content"] = []map[string]interface{}{
			{"type": "output_text", "text": text},
		}
	} else if itemType == "reasoning" {
		item["content"] = []map[string]interface{}{
			{"type": "reasoning_text", "text": text},
		}
	}
	evt := map[string]interface{}{
		"type":         "response.output_item.done",
		"output_index": outputIndex,
		"item":         item,
	}
	b, _ := json.Marshal(evt)
	return string(b)
}

func responsesFailedEvent(msg string) string {
	evt := map[string]interface{}{
		"type": "response.failed",
		"response": map[string]interface{}{
			"error": map[string]interface{}{
				"message": msg, "type": "server_error",
			},
		},
	}
	b, _ := json.Marshal(evt)
	return string(b)
}

func responsesCompletedEvent(resp officialtypes.ResponsesResponse) string {
	evt := map[string]interface{}{
		"type":     "response.completed",
		"response": resp,
	}
	b, _ := json.Marshal(evt)
	return string(b)
}
