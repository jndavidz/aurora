package provider

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"aurora/internal/config"
	"aurora/internal/toolcall"
	"aurora/typings/official"
	"aurora/util"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// GeminiCDP 通过家庭/办公室 PC 上的 CDP 桥(scripts/cdp/bridge.mjs)执行 gemini.google.com 对话。
//
// 桥用真实浏览器的页内 fetch() 发请求 —— cookie/TLS/指纹/JS 运行时全部由浏览器自带
// (零指纹模拟),aurora 侧只做 OpenAI 兼容的 HTTP 转发。直连通道(geminweb)因数据中心
// IP + 模拟指纹被 Google 风控(commit ff4af80)已停用,本 Provider 是 Gemini 的正式通道。
//
// 配置:
//   - GEMINI_CDP_URL=http://<PC1>:8799[,http://<PC2>:8799...] —— 桥池,逗号分隔;
//     轮询 + 故障转移(某桥网络不通/5xx 时自动换下一个),每个桥登录不同的小号
//     (也摊薄 Google 限频:桥数 = 账号数 = 吞吐翻倍)。仅配置时注册。
//   - GEMINI_CDP_KEY 可选,与桥的 BRIDGE_AUTH 一致。
//
// 变体:
//   - -chat  :纯对话(桥直通,请求原样转发)
//   - -coding:工具调用 —— aurora 侧注入工具指令 prompt + FenceParser 解析
//     (复用直连时代 gemini_coding.go 的同一套 prompt/解析,桥只当纯 chat 用)
type GeminiCDP struct {
	cfg    *config.Config
	models []Model
	byID   map[string]string // model id → variant("chat" | "coding")
	urls   []string          // 桥池
	rr     int64             // 轮询游标
	http   *http.Client

	// 桥熔断:某桥网络不通/5xx 后冷却 60s(如办公室桥关机时快速跳过,不拖慢请求)
	mu        sync.Mutex
	deadUntil map[string]time.Time
}

// defaultGeminiCDPModels 默认目录(与桥 /v1/models 的 chat + aurora 侧 coding)。
var defaultGeminiCDPModels = []string{"gemini-3-flash-chat", "gemini-3-flash-coding"}

// NewGeminiCDP 构造 CDP 桥 provider。
func NewGeminiCDP(cfg *config.Config) *GeminiCDP {
	d := &GeminiCDP{
		cfg:       cfg,
		byID:      make(map[string]string),
		deadUntil: make(map[string]time.Time),
	}
	// 短拨号超时:某桥离线(如办公室 PC 关机)时 3s 内快速失败并换下一桥,
	// 而不是等系统 TCP 超时几十秒。总超时仍 10 分钟(流式长响应)。
	transport := &http.Transport{
		DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
	}
	d.http = &http.Client{Timeout: 10 * time.Minute, Transport: transport}
	for _, u := range strings.Split(cfg.GeminiCDPURL, ",") {
		if u = strings.TrimSpace(u); u != "" {
			d.urls = append(d.urls, u)
		}
	}
	ids := cfg.GeminiModels
	if len(ids) == 0 {
		ids = defaultGeminiCDPModels
	}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if !strings.HasPrefix(id, "gemini-") {
			continue
		}
		switch {
		case strings.HasSuffix(id, "-chat"):
			d.byID[id] = "chat"
			d.models = append(d.models, Model{ID: id, OwnedBy: "google", Caps: []Capability{CapWebSearch, CapReasoning, CapVision}})
		case strings.HasSuffix(id, "-coding"):
			d.byID[id] = "coding"
			d.models = append(d.models, Model{ID: id, OwnedBy: "google", Caps: []Capability{CapFunctionCall, CapReasoning}})
		}
	}
	return d
}

func (d *GeminiCDP) Name() string { return "google" }

func (d *GeminiCDP) Models() []Model { return d.models }

func (d *GeminiCDP) Handles(model string) bool {
	_, ok := d.byID[model]
	return ok
}

// ─── 桥池调用(轮询 + 故障转移)────────────────────────────────────

func (d *GeminiCDP) bridgeURLFor(u string) string {
	return strings.TrimRight(u, "/") + "/v1/chat/completions"
}

// post 向桥池发请求:从轮询游标起逐个尝试,网络错误/5xx 熔断该桥并换下一个,
// 全失败返回错误。
func (d *GeminiCDP) post(body []byte) (*http.Response, error) {
	if len(d.urls) == 0 {
		return nil, fmt.Errorf("GEMINI_CDP_URL not configured")
	}
	now := time.Now()
	start := int(atomic.AddInt64(&d.rr, 1)-1) % len(d.urls)
	var lastErr error
	// 熔断中的桥跳过(冷却 60s);若全部熔断则放行重试一次(同时清冷却,便于恢复)
	for i := 0; i < len(d.urls); i++ {
		u := d.urls[(start+i)%len(d.urls)]
		d.mu.Lock()
		dead := d.deadUntil[u]
		d.mu.Unlock()
		if !dead.IsZero() && dead.After(now) {
			lastErr = fmt.Errorf("bridge %s cooling down until %s", u, dead.Format("15:04:05"))
			continue
		}
		resp, err := d.postTo(u, body)
		if err != nil {
			d.markDead(u)
			lastErr = fmt.Errorf("bridge %s unreachable: %w", u, err)
			continue
		}
		if resp.StatusCode >= 500 {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			d.markDead(u)
			lastErr = fmt.Errorf("bridge %s http %d", u, resp.StatusCode)
			continue
		}
		d.clearDead(u)
		return resp, nil
	}
	// 全熔断:清冷却,强制重试一轮(可能刚恢复)
	d.mu.Lock()
	clear(d.deadUntil)
	d.mu.Unlock()
	return nil, lastErr
}

func (d *GeminiCDP) markDead(u string) {
	d.mu.Lock()
	d.deadUntil[u] = time.Now().Add(60 * time.Second)
	d.mu.Unlock()
}

func (d *GeminiCDP) clearDead(u string) {
	d.mu.Lock()
	delete(d.deadUntil, u)
	d.mu.Unlock()
}

func (d *GeminiCDP) postTo(u string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, d.bridgeURLFor(u), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if d.cfg.GeminiCDPKey != "" {
		req.Header.Set("Authorization", "Bearer "+d.cfg.GeminiCDPKey)
	}
	return d.http.Do(req)
}

// ─── 桥的流式/非流式调用助手 ──────────────────────────────────────

// streamPrompt 把 prompt 作为单条 user 消息发给桥(stream),逐块回调 delta 文本。
func (d *GeminiCDP) streamPrompt(model, prompt string, onDelta func(string)) error {
	body, _ := json.Marshal(map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": prompt}},
		"stream":   true,
	})
	resp, err := d.post(body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("gemini cdp bridge http %d: %s", resp.StatusCode, truncateStr(string(data), 300))
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	got := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}
		var chunk bridgeChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if chunk.Error != nil {
			return fmt.Errorf("%s", chunk.Error.Message)
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.Content != "" {
				got = true
				if onDelta != nil {
					onDelta(ch.Delta.Content)
				}
			}
		}
	}
	if !got {
		return fmt.Errorf("empty reply from gemini cdp bridge")
	}
	return nil
}

// completePrompt 非流式取全文。
func (d *GeminiCDP) completePrompt(model, prompt string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": prompt}},
		"stream":   false,
	})
	resp, err := d.post(body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gemini cdp bridge http %d: %s", resp.StatusCode, truncateStr(string(data), 300))
	}
	var comp bridgeCompletion
	if err := json.Unmarshal(data, &comp); err != nil {
		return "", fmt.Errorf("bad bridge response: %w", err)
	}
	if comp.Error != nil {
		return "", fmt.Errorf("%s", comp.Error.Message)
	}
	if len(comp.Choices) == 0 || comp.Choices[0].Message.Content == "" {
		return "", fmt.Errorf("empty reply from gemini cdp bridge")
	}
	return comp.Choices[0].Message.Content, nil
}

// messagesFromItems 把统一 item 列表转成桥的 messages(纯文本,跳过工具项)。
func messagesFromItems(items []responsesInputItem, instructions string) []map[string]string {
	var out []map[string]string
	if instructions != "" {
		out = append(out, map[string]string{"role": "system", "content": instructions})
	}
	for _, it := range items {
		if it.Type != "message" || it.Text == "" {
			continue
		}
		role := it.Role
		if role == "" || role == "tool" || role == "function" {
			role = "user"
		}
		out = append(out, map[string]string{"role": role, "content": it.Text})
	}
	return out
}

// ─── ChatCompletions ──────────────────────────────────────────────

func (d *GeminiCDP) ChatCompletions(c *gin.Context, req *official.APIRequest) {
	variant, ok := d.byID[req.Model]
	if !ok {
		c.JSON(404, gin.H{"error": "model not found"})
		return
	}
	if variant == "coding" {
		d.codingCompletions(c, req)
		return
	}
	body, err := json.Marshal(map[string]any{
		"model":    req.Model,
		"messages": messagesFromItems(apiMessagesToItems(req.Messages), ""),
		"stream":   req.Stream,
	})
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	resp, err := d.post(body)
	if err != nil {
		c.JSON(502, gin.H{"error": "gemini cdp bridge unreachable: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	if req.Stream {
		d.relayStream(c, resp)
		return
	}
	d.relayJSON(c, resp)
}

// relayStream 逐块透传桥的 SSE(桥已输出标准 OpenAI SSE)。
func (d *GeminiCDP) relayStream(c *gin.Context, resp *http.Response) {
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "text/event-stream"
	}
	c.Writer.Header().Set("Content-Type", ct)
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.WriteHeader(resp.StatusCode)
	flusher, _ := c.Writer.(http.Flusher)
	buf := make([]byte, 8192)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := c.Writer.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// relayJSON 透传桥的非流式 JSON(含错误体)。
func (d *GeminiCDP) relayJSON(c *gin.Context, resp *http.Response) {
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		c.JSON(502, gin.H{"error": err.Error()})
		return
	}
	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(resp.StatusCode)
	_, _ = c.Writer.Write(data)
}

// ─── Responses ────────────────────────────────────────────────────

// bridgeChunk 是桥 SSE 的单个 chunk(chat.completion.chunk 最小解析)。
type bridgeChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// bridgeCompletion 是桥非流式响应(chat.completion 最小解析)。
type bridgeCompletion struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (d *GeminiCDP) Responses(c *gin.Context, req *official.ResponsesAPIRequest) {
	variant, ok := d.byID[req.Model]
	if !ok {
		c.JSON(404, gin.H{"error": "model not found"})
		return
	}
	if variant == "coding" {
		d.codingResponses(c, req)
		return
	}
	msgs := messagesFromItems(responsesInputItems(req.Input), rawResponsesText(req.Instructions))
	if len(msgs) == 0 {
		c.JSON(400, gin.H{"error": "no input"})
		return
	}
	if req.Stream {
		d.responsesStream(c, req, msgs)
		return
	}
	d.responsesNonStream(c, req, msgs)
}

// responsesStream 把桥的 SSE delta 转成 Responses 事件流。
func (d *GeminiCDP) responsesStream(c *gin.Context, req *official.ResponsesAPIRequest, msgs []map[string]string) {
	w := newSSEWriter(c)
	respID := "resp_" + uuid.NewString()
	messageItemID := "msg_" + uuid.NewString()
	w.event("response.created", createdEvent(respID, req.Model))
	w.event("response.output_item.added", outputItemAddedEvent(0, map[string]any{
		"id": messageItemID, "type": "message", "status": "in_progress", "role": "assistant",
	}))

	body, _ := json.Marshal(map[string]any{"model": req.Model, "messages": msgs, "stream": true})
	resp, err := d.post(body)
	if err != nil {
		w.event("response.failed", failedEvent("gemini cdp bridge unreachable: "+err.Error()))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		w.event("response.failed", failedEvent(fmt.Sprintf("gemini cdp bridge http %d: %s", resp.StatusCode, truncateStr(string(data), 300))))
		return
	}

	var fullText string
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}
		var chunk bridgeChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if chunk.Error != nil && fullText == "" {
			w.event("response.failed", failedEvent(chunk.Error.Message))
			return
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.Content != "" {
				fullText += ch.Delta.Content
				w.event("response.output_text.delta", map[string]any{
					"type": "response.output_text.delta", "item_id": messageItemID,
					"output_index": 0, "content_index": 0, "delta": ch.Delta.Content,
				})
			}
		}
	}
	if fullText == "" {
		w.event("response.failed", failedEvent("empty reply from gemini cdp bridge"))
		return
	}
	w.event("response.output_item.done", outputItemDoneEvent(0, messageItem(messageItemID, fullText, "completed")))
	outResp := official.NewResponsesResponse(fullText, "", countInputChars(req), util.CountToken(fullText), 0, 0, 0, req.Model)
	w.event("response.completed", completedEvent(outResp))
}

// responsesNonStream 非流式 Responses。
func (d *GeminiCDP) responsesNonStream(c *gin.Context, req *official.ResponsesAPIRequest, msgs []map[string]string) {
	body, _ := json.Marshal(map[string]any{"model": req.Model, "messages": msgs, "stream": false})
	resp, err := d.post(body)
	if err != nil {
		c.JSON(502, gin.H{"error": "gemini cdp bridge unreachable: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		c.JSON(502, gin.H{"error": fmt.Sprintf("gemini cdp bridge http %d: %s", resp.StatusCode, truncateStr(string(data), 300))})
		return
	}
	var comp bridgeCompletion
	if err := json.Unmarshal(data, &comp); err != nil {
		c.JSON(502, gin.H{"error": "bad bridge response: " + err.Error()})
		return
	}
	if comp.Error != nil {
		c.JSON(502, gin.H{"error": comp.Error.Message})
		return
	}
	fullText := ""
	if len(comp.Choices) > 0 {
		fullText = comp.Choices[0].Message.Content
	}
	if fullText == "" {
		c.JSON(502, gin.H{"error": "empty reply from gemini cdp bridge"})
		return
	}
	outResp := official.NewResponsesResponse(fullText, "", countInputChars(req), util.CountToken(fullText), 0, 0, 0, req.Model)
	c.JSON(200, outResp)
}

// ─── coding 变体(工具调用:aurora 侧注入 + 解析,桥只当纯 chat)────────

// codingResponses 处理 coding 变体(/v1/responses)。
// prompt 构建与解析复用直连时代 gemini_coding.go 的同一套(围栏 JSON + FenceParser)。
func (d *GeminiCDP) codingResponses(c *gin.Context, req *official.ResponsesAPIRequest) {
	prompt := geminiCodingPromptFromResponses(req)
	if req.Stream {
		d.codingResponsesStream(c, req, prompt)
		return
	}
	d.codingResponsesNonStream(c, req, prompt)
}

func (d *GeminiCDP) codingResponsesStream(c *gin.Context, req *official.ResponsesAPIRequest, prompt string) {
	w := newSSEWriter(c)
	respID := "resp_" + uuid.NewString()
	messageItemID := "msg_" + uuid.NewString()
	w.event("response.created", createdEvent(respID, req.Model))
	w.event("response.output_item.added", outputItemAddedEvent(0, map[string]any{"id": messageItemID, "type": "message", "status": "in_progress", "role": "assistant"}))

	parser := toolcall.NewFenceParser(req.Tools)
	var textBuf strings.Builder
	var calls []official.ToolCall
	err := d.streamPrompt(req.Model, prompt, func(delta string) {
		textDelta, parsed := parser.Feed(delta)
		if textDelta != "" {
			textBuf.WriteString(textDelta)
			w.event("response.output_text.delta", map[string]any{
				"type": "response.output_text.delta", "item_id": messageItemID,
				"output_index": 0, "content_index": 0, "delta": textDelta,
			})
		}
		calls = append(calls, parsed...)
	})
	if err == nil {
		textDelta, parsed := parser.Flush()
		if textDelta != "" {
			textBuf.WriteString(textDelta)
			w.event("response.output_text.delta", map[string]any{
				"type": "response.output_text.delta", "item_id": messageItemID,
				"output_index": 0, "content_index": 0, "delta": textDelta,
			})
		}
		calls = append(calls, parsed...)
	}
	calls = mergeRecoveredCalls(calls, textBuf.String(), req.Tools)

	finalText := textBuf.String()
	if err != nil && finalText == "" && len(calls) == 0 {
		w.event("response.failed", failedEvent(err.Error()))
		return
	}
	w.event("response.output_item.done", outputItemDoneEvent(0, messageItem(messageItemID, finalText, "completed")))
	outResp := official.NewResponsesResponse(finalText, "", countInputChars(req), len(finalText), 0, 0, 0, req.Model)
	for i, tc := range calls {
		idx := i + 1
		fcID := "fc_" + uuid.NewString()
		callID := tc.ID
		if callID == "" {
			callID = "call_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:24]
		}
		w.event("response.output_item.added", outputItemAddedEvent(idx, functionCallItem(fcID, callID, tc.Function.Name, "", "in_progress")))
		w.event("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": fcID,
			"output_index": idx, "delta": tc.Function.Arguments,
		})
		w.event("response.function_call_arguments.done", map[string]any{
			"type": "response.function_call_arguments.done", "item_id": fcID,
			"output_index": idx, "arguments": tc.Function.Arguments,
		})
		w.event("response.output_item.done", outputItemDoneEvent(idx, functionCallItem(fcID, callID, tc.Function.Name, tc.Function.Arguments, "completed")))
		outResp.Output = append(outResp.Output, official.ResponsesOutputItem{
			ID: fcID, Type: "function_call", Status: "completed",
			CallID: callID, Name: tc.Function.Name, Arguments: tc.Function.Arguments,
		})
	}
	w.event("response.completed", completedEvent(outResp))
}

func (d *GeminiCDP) codingResponsesNonStream(c *gin.Context, req *official.ResponsesAPIRequest, prompt string) {
	fullText, err := d.completePrompt(req.Model, prompt)
	if err != nil {
		c.JSON(502, gin.H{"error": err.Error()})
		return
	}
	parser := toolcall.NewFenceParser(req.Tools)
	_, calls := parser.Feed(fullText)
	calls = append(calls, parser.FlushCalls()...)
	calls = mergeRecoveredCalls(calls, fullText, req.Tools)
	cleanText := toolcall.StripFencedBlocks(fullText)
	outResp := official.NewResponsesResponse(cleanText, "", countInputChars(req), util.CountToken(cleanText), 0, 0, 0, req.Model)
	for _, tc := range calls {
		outResp.Output = append(outResp.Output, official.ResponsesOutputItem{
			ID: "fc_" + uuid.NewString(), Type: "function_call", Status: "completed",
			CallID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments,
		})
	}
	c.JSON(200, outResp)
}

// codingCompletions 处理 coding 变体(/v1/chat/completions)。
func (d *GeminiCDP) codingCompletions(c *gin.Context, req *official.APIRequest) {
	prompt := geminiCodingPromptFromAPI(req)
	if req.Stream {
		d.codingCompletionsStream(c, req, prompt)
		return
	}
	d.codingCompletionsNonStream(c, req, prompt)
}

func (d *GeminiCDP) codingCompletionsStream(c *gin.Context, req *official.APIRequest, prompt string) {
	model := req.Model
	if model == "" {
		model = "auto"
	}
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := c.Writer.(http.Flusher)
	writeChunk := func(chunk official.ChatCompletionChunk) {
		c.Writer.WriteString("data: " + chunk.String() + "\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
	roleChunk := official.NewChatCompletionChunk("", model)
	roleChunk.Choices[0].Delta.Role = "assistant"
	writeChunk(roleChunk)

	parser := toolcall.NewFenceParser(req.Tools)
	var textBuf strings.Builder
	var emittedCall bool
	err := d.streamPrompt(model, prompt, func(delta string) {
		textDelta, calls := parser.Feed(delta)
		if textDelta != "" {
			textBuf.WriteString(textDelta)
			writeChunk(official.NewChatCompletionChunk(textDelta, model))
		}
		for _, tc := range calls {
			emittedCall = true
			for _, dd := range toolcall.StreamToToolCallDeltas([]official.ToolCall{tc}) {
				writeChunk(official.NewToolCallChunk(model, dd...))
			}
		}
	})
	if err == nil {
		textDelta, calls := parser.Flush()
		if textDelta != "" {
			textBuf.WriteString(textDelta)
			writeChunk(official.NewChatCompletionChunk(textDelta, model))
		}
		for _, tc := range calls {
			emittedCall = true
			for _, dd := range toolcall.StreamToToolCallDeltas([]official.ToolCall{tc}) {
				writeChunk(official.NewToolCallChunk(model, dd...))
			}
		}
	}
	for _, tc := range mergeRecoveredCalls(nil, textBuf.String(), req.Tools) {
		emittedCall = true
		for _, dd := range toolcall.StreamToToolCallDeltas([]official.ToolCall{tc}) {
			writeChunk(official.NewToolCallChunk(model, dd...))
		}
	}
	if err != nil && textBuf.Len() == 0 && !emittedCall {
		// 桥失败且无任何输出:发一个 error chunk 再收尾
		c.Writer.WriteString("data: {\"error\":{\"message\":\"" + strings.ReplaceAll(err.Error(), "\"", "'") + "\"}}\n\n")
	}
	if emittedCall {
		writeChunk(official.NewToolCallStopChunk(model, ""))
	} else {
		writeChunk(official.StopChunk("stop", model))
	}
	c.Writer.WriteString("data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func (d *GeminiCDP) codingCompletionsNonStream(c *gin.Context, req *official.APIRequest, prompt string) {
	fullText, err := d.completePrompt(req.Model, prompt)
	if err != nil {
		c.JSON(502, gin.H{"error": err.Error()})
		return
	}
	parser := toolcall.NewFenceParser(req.Tools)
	_, calls := parser.Feed(fullText)
	calls = append(calls, parser.FlushCalls()...)
	calls = mergeRecoveredCalls(calls, fullText, req.Tools)
	cleanText := toolcall.StripFencedBlocks(fullText)
	outResp := official.NewChatCompletionWithToolCalls(cleanText, "", calls, countMessagesChars(req.Messages), util.CountToken(cleanText), req.Model, "", nil)
	c.JSON(200, outResp)
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
