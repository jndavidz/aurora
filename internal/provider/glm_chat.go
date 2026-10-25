package provider

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"aurora/internal/apierrors"
	"aurora/internal/glmweb"
	"aurora/typings/official"
	"aurora/util"

	"github.com/gin-gonic/gin"
)

// glmRefreshSkew 是 access_token 的提前换发余量(GLM access ~2h)。
// 距过期不足 10 分钟即视为需要换发,避免拿到临界票打到一半过期。
const glmRefreshSkew = 10 * time.Minute

// chatResponses 处理智谱 chat 变体(/v1/responses)。
//
// 与 DeepSeek 差异:智谱用 messages 数组 + conversation_id 服务端会话
// (非拍平 prompt)。chat 变体剥离 tools,纯对话 + 深度思考/联网开关。
func (d *Glm) chatResponses(c *gin.Context, m *glmModel, req *official.ResponsesAPIRequest) {
	client := d.webClient()
	if err := d.ensureToken(client); err != nil {
		apierrors.JSONError(c, 502, "api_error", err.Error(), nil, "upstream_error")
		return
	}

	messages := glmMessagesFromResponses(req, true) // chat 变体剥离 tools
	streamReq := glmweb.CompletionRequest{
		Messages:     messages,
		ChatMode:     m.Mode,             // "speed"(快速,默认)|"thinking"(思考),由模型 id 挡位决定
		IsNetworking: d.cfg.GlmWebSearch, // N4(2026-09-04):默认关闭(提速);GLM_WEB_SEARCH=1 开启
	}
	if req.Stream {
		d.chatResponsesStream(c, req, client, streamReq)
		return
	}
	d.chatResponsesNonStream(c, req, client, streamReq)
}

// ensureToken 确保有有效 access_token(必要时换发;换发失败轮询下一个池 token)。
// tokMu 护整个换发序列(同 kimi 侧):并发请求同时换发会互相作废轮换链。
func (d *Glm) ensureToken(client *glmweb.Client) error {
	d.tokMu.Lock()
	defer d.tokMu.Unlock()
	if client == nil || !client.HasToken() {
		return fmt.Errorf("glm web client unavailable: missing GLM_WEB_TOKENS?")
	}
	if client.HasAccessToken() && !client.AccessTokenNearExpiry(glmRefreshSkew) {
		return nil
	}
	for attempt := 0; attempt <= client.PoolSize(); attempt++ {
		token := client.NextToken()
		if token == "" || (attempt > 0 && token == d.lastToken) {
			break
		}
		client.SetRefreshToken(token)
		d.lastToken = token
		if err := client.RefreshAccessToken(); err == nil {
			return nil
		}
	}
	return fmt.Errorf("glm: all refresh tokens failed")
}

// completeWithAuth 调 client.Complete;请求失败即清票(401/403 或传输错误),
// 下一次请求经 ensureToken 重换发 —— 修复"access_token 过期后拿废票打到进程重启"的短路。
// 对传输类瞬时错误多清一次票的代价只是一次额外换发(纯 HTTP,毫秒级),可接受。
func (d *Glm) completeWithAuth(client *glmweb.Client, req glmweb.CompletionRequest) (*http.Response, error) {
	resp, err := client.Complete(req)
	if err != nil {
		client.ClearAccessToken()
	}
	return resp, err
}

// glmMessagesFromResponses 把 Responses input 转成智谱 messages(chat 变体剥离 tools)。
func glmMessagesFromResponses(req *official.ResponsesAPIRequest, stripTools bool) []glmweb.Message {
	items := responsesInputItems(req.Input)
	var msgs []glmweb.Message
	// instructions → 第一条 system 消息
	if instr := rawResponsesText(req.Instructions); instr != "" {
		msgs = append(msgs, glmweb.Message{Role: "user", Content: []glmweb.Content{{Type: "text", Text: instr}}})
	}
	for _, it := range items {
		if stripTools && (it.Type == "function_call" || it.Type == "function_call_output") {
			continue
		}
		role := it.Role
		if role == "" || role == "system" {
			role = "user" // 智谱无 system role,归入 user
		}
		if it.Text == "" {
			continue
		}
		msgs = append(msgs, glmweb.Message{Role: role, Content: []glmweb.Content{{Type: "text", Text: it.Text}}})
	}
	if len(msgs) == 0 {
		msgs = append(msgs, glmweb.Message{Role: "user", Content: []glmweb.Content{{Type: "text", Text: ""}}})
	}
	return msgs
}

// chatResponsesStream 流式输出 Responses 事件。
func (d *Glm) chatResponsesStream(c *gin.Context, req *official.ResponsesAPIRequest, client *glmweb.Client, streamReq glmweb.CompletionRequest) {
	resp, err := d.completeWithAuth(client, streamReq)
	if err != nil {
		apierrors.JSONError(c, 502, "api_error", err.Error(), nil, "upstream_error")
		return
	}
	defer resp.Body.Close()

	e := NewResponsesEmitter(c, req.Model, true)

	var fullText, fullReasoning string
	res := glmweb.ConsumeStream(resp.Body, func(delta glmweb.Delta) {
		fullReasoning += delta.Reasoning
		fullText += delta.Text
		e.ReasoningDelta(delta.Reasoning)
		e.TextDelta(delta.Text)
	})
	if res.Err != "" && fullText == "" && fullReasoning == "" {
		e.Fail(res.Err)
		return
	}
	outResp := official.NewResponsesResponse(fullText, fullReasoning, countInputChars(req), util.CountToken(fullText), util.CountToken(fullReasoning), 0, 0, req.Model)
	e.Complete(outResp, fullText, fullReasoning)
}

// chatResponsesNonStream 非流式。
func (d *Glm) chatResponsesNonStream(c *gin.Context, req *official.ResponsesAPIRequest, client *glmweb.Client, streamReq glmweb.CompletionRequest) {
	resp, err := d.completeWithAuth(client, streamReq)
	if err != nil {
		apierrors.JSONError(c, 502, "api_error", err.Error(), nil, "upstream_error")
		return
	}
	defer resp.Body.Close()
	var fullText, fullReasoning string
	res := glmweb.ConsumeStream(resp.Body, func(delta glmweb.Delta) {
		fullText += delta.Text
		fullReasoning += delta.Reasoning
	})
	if res.Err != "" && fullText == "" && fullReasoning == "" {
		apierrors.JSONError(c, 502, "api_error", res.Err, nil, "upstream_error")
		return
	}
	outResp := official.NewResponsesResponse(fullText, fullReasoning, countInputChars(req), util.CountToken(fullText), util.CountToken(fullReasoning), 0, 0, req.Model)
	c.JSON(200, outResp)
}

// chatCompletions 处理智谱 chat 变体(/v1/chat/completions)。
func (d *Glm) chatCompletions(c *gin.Context, m *glmModel, req *official.APIRequest) {
	client := d.webClient()
	if err := d.ensureToken(client); err != nil {
		apierrors.JSONError(c, 502, "api_error", err.Error(), nil, "upstream_error")
		return
	}
	messages := glmMessagesFromAPI(req)
	streamReq := glmweb.CompletionRequest{
		Messages:     messages,
		ChatMode:     m.Mode,             // "speed"(快速,默认)|"thinking"(思考),由模型 id 挡位决定
		IsNetworking: d.cfg.GlmWebSearch, // N4(2026-09-04):默认关闭(提速);GLM_WEB_SEARCH=1 开启
	}
	if req.Stream {
		d.chatCompletionsStream(c, req, client, streamReq)
		return
	}
	d.chatCompletionsNonStream(c, req, client, streamReq)
}

// glmMessagesFromAPI 把 chat.completions messages 转成智谱 messages。
func glmMessagesFromAPI(req *official.APIRequest) []glmweb.Message {
	var msgs []glmweb.Message
	for _, msg := range req.Messages {
		role := msg.Role
		if role == "system" {
			role = "user" // 智谱无 system role
		}
		if t := msg.Text(); t != "" {
			msgs = append(msgs, glmweb.Message{Role: role, Content: []glmweb.Content{{Type: "text", Text: t}}})
		}
	}
	if len(msgs) == 0 {
		msgs = append(msgs, glmweb.Message{Role: "user", Content: []glmweb.Content{{Type: "text", Text: ""}}})
	}
	return msgs
}

// chatCompletionsStream 流式 chat.completion.chunk。
func (d *Glm) chatCompletionsStream(c *gin.Context, req *official.APIRequest, client *glmweb.Client, streamReq glmweb.CompletionRequest) {
	resp, err := d.completeWithAuth(client, streamReq)
	if err != nil {
		apierrors.JSONError(c, 502, "api_error", err.Error(), nil, "upstream_error")
		return
	}
	defer resp.Body.Close()

	model := req.Model
	if model == "" {
		model = "auto"
	}
	s := NewChunkStream(c, model)
	_ = glmweb.ConsumeStream(resp.Body, func(delta glmweb.Delta) {
		s.ReasoningDelta(delta.Reasoning)
		s.TextDelta(delta.Text)
	})
	s.Stop("stop")
}

// chatCompletionsNonStream 非流式 ChatCompletion。
func (d *Glm) chatCompletionsNonStream(c *gin.Context, req *official.APIRequest, client *glmweb.Client, streamReq glmweb.CompletionRequest) {
	resp, err := d.completeWithAuth(client, streamReq)
	if err != nil {
		apierrors.JSONError(c, 502, "api_error", err.Error(), nil, "upstream_error")
		return
	}
	defer resp.Body.Close()
	var fullText, fullReasoning string
	res := glmweb.ConsumeStream(resp.Body, func(delta glmweb.Delta) {
		fullText += delta.Text
		fullReasoning += delta.Reasoning
	})
	if res.Err != "" && fullText == "" && fullReasoning == "" {
		apierrors.JSONError(c, 502, "api_error", res.Err, nil, "upstream_error")
		return
	}
	outResp := official.NewChatCompletionWithMetadataAndReasoning(fullText, fullReasoning, countMessagesChars(req.Messages), util.CountToken(fullText), req.Model, "", nil)
	c.JSON(200, outResp)
}

var _ = strings.TrimSpace
