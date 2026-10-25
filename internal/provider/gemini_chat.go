package provider

import (

	"aurora/internal/apierrors"
	"aurora/internal/geminweb"
	"aurora/typings/official"
	"aurora/util"

	"github.com/gin-gonic/gin"
)

// chatResponses 处理 Gemini chat 变体(/v1/responses)。
//
// Gemini 网页版模型自动使用原生搜索/地图等能力,chat 变体只需纯文本对话。
// 多轮:首轮返回 rcid,后续请求用 PrevRCID 续聊。
func (d *Gemini) chatResponses(c *gin.Context, m *geminiModel, req *official.ResponsesAPIRequest) {
	client := d.webClient()
	if client == nil || !client.HasAccount() {
		apierrors.JSONError(c, 502, "api_error", "gemini web client unavailable: missing GEMINI_ACCOUNTS?", nil, "upstream_error")
		return
	}
	prompt := flattenChatInput(req, true) // chat 变体剥离 tools
	streamReq := geminweb.CompletionRequest{Prompt: prompt}
	if req.Stream {
		d.chatResponsesStream(c, req, client, streamReq)
		return
	}
	d.chatResponsesNonStream(c, req, client, streamReq)
}

// chatResponsesStream 流式输出 Responses 事件。
func (d *Gemini) chatResponsesStream(c *gin.Context, req *official.ResponsesAPIRequest, client *geminweb.Client, streamReq geminweb.CompletionRequest) {
	e := NewResponsesEmitter(c, req.Model, false)

	var fullText string
	res := client.Complete(streamReq, func(delta geminweb.Delta) {
		if delta.Text == "" {
			return
		}
		fullText += delta.Text
		e.TextDelta(delta.Text)
	})
	if res.Err != "" && fullText == "" {
		e.Fail(res.Err)
		return
	}
	outResp := official.NewResponsesResponse(fullText, "", countInputChars(req), util.CountToken(fullText), 0, 0, 0, req.Model)
	e.Complete(outResp, fullText, "")
}

// chatResponsesNonStream 非流式。
func (d *Gemini) chatResponsesNonStream(c *gin.Context, req *official.ResponsesAPIRequest, client *geminweb.Client, streamReq geminweb.CompletionRequest) {
	var fullText string
	res := client.Complete(streamReq, func(delta geminweb.Delta) { fullText += delta.Text })
	if res.Err != "" && fullText == "" {
		apierrors.JSONError(c, 502, "api_error", res.Err, nil, "upstream_error")
		return
	}
	outResp := official.NewResponsesResponse(fullText, "", countInputChars(req), util.CountToken(fullText), 0, 0, 0, req.Model)
	c.JSON(200, outResp)
}

// chatCompletions 处理 Gemini chat 变体(/v1/chat/completions)。
func (d *Gemini) chatCompletions(c *gin.Context, m *geminiModel, req *official.APIRequest) {
	client := d.webClient()
	if client == nil || !client.HasAccount() {
		apierrors.JSONError(c, 502, "api_error", "gemini web client unavailable: missing GEMINI_ACCOUNTS?", nil, "upstream_error")
		return
	}
	prompt := flattenChatInputAPI(req)
	streamReq := geminweb.CompletionRequest{Prompt: prompt}
	if req.Stream {
		d.chatCompletionsStream(c, req, client, streamReq)
		return
	}
	d.chatCompletionsNonStream(c, req, client, streamReq)
}

// chatCompletionsStream 流式 chat.completion.chunk。
func (d *Gemini) chatCompletionsStream(c *gin.Context, req *official.APIRequest, client *geminweb.Client, streamReq geminweb.CompletionRequest) {
	model := req.Model
	if model == "" {
		model = "auto"
	}
	s := NewChunkStream(c, model)
	_ = client.Complete(streamReq, func(delta geminweb.Delta) {
		if delta.Text != "" {
			s.TextDelta(delta.Text)
		}
	})
	s.Stop("stop")
}

// chatCompletionsNonStream 非流式 ChatCompletion。
func (d *Gemini) chatCompletionsNonStream(c *gin.Context, req *official.APIRequest, client *geminweb.Client, streamReq geminweb.CompletionRequest) {
	var fullText string
	res := client.Complete(streamReq, func(delta geminweb.Delta) { fullText += delta.Text })
	if res.Err != "" && fullText == "" {
		apierrors.JSONError(c, 502, "api_error", res.Err, nil, "upstream_error")
		return
	}
	outResp := official.NewChatCompletionWithMetadataAndReasoning(fullText, "", countMessagesChars(req.Messages), util.CountToken(fullText), req.Model, "", nil)
	c.JSON(200, outResp)
}
