package provider

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"aurora/internal/config"
	"aurora/typings/official"
	"aurora/util"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// GeminiCDP 通过家庭 PC 上的 CDP 桥(scripts/cdp/bridge.mjs)执行 gemini.google.com 对话。
//
// 桥用真实浏览器的页内 fetch() 发请求 —— cookie/TLS/指纹/JS 运行时全部由浏览器自带
// (零指纹模拟),NAS 侧只做 OpenAI 兼容的 HTTP 转发。直连通道(geminweb)因数据中心 IP
// + 模拟指纹被 Google 风控(commit ff4af80)已停用,本 Provider 是 Gemini 的正式通道。
//
// 配置:GEMINI_CDP_URL=http://<PC>:8799(仅配置时注册);
// GEMINI_CDP_KEY 可选,与桥的 BRIDGE_AUTH 一致。
type GeminiCDP struct {
	cfg    *config.Config
	models []Model
	byID   map[string]struct{}
	http   *http.Client
}

// defaultGeminiCDPModels 默认目录(与桥 /v1/models 暴露一致)。
var defaultGeminiCDPModels = []string{"gemini-3-flash-chat"}

// NewGeminiCDP 构造 CDP 桥 provider。只接受 -chat 变体(桥暂未实现工具调用)。
func NewGeminiCDP(cfg *config.Config) *GeminiCDP {
	d := &GeminiCDP{
		cfg:  cfg,
		byID: make(map[string]struct{}),
		http: &http.Client{Timeout: 10 * time.Minute},
	}
	ids := cfg.GeminiModels
	if len(ids) == 0 {
		ids = defaultGeminiCDPModels
	}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if !strings.HasPrefix(id, "gemini-") || !strings.HasSuffix(id, "-chat") {
			continue
		}
		d.byID[id] = struct{}{}
		d.models = append(d.models, Model{ID: id, OwnedBy: "google", Caps: []Capability{CapWebSearch, CapReasoning, CapVision}})
	}
	return d
}

func (d *GeminiCDP) Name() string { return "google" }

func (d *GeminiCDP) Models() []Model { return d.models }

func (d *GeminiCDP) Handles(model string) bool {
	_, ok := d.byID[model]
	return ok
}

// ─── 桥调用 ───────────────────────────────────────────────────────

func (d *GeminiCDP) bridgeURL() string {
	return strings.TrimRight(d.cfg.GeminiCDPURL, "/") + "/v1/chat/completions"
}

func (d *GeminiCDP) post(body []byte) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, d.bridgeURL(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if d.cfg.GeminiCDPKey != "" {
		req.Header.Set("Authorization", "Bearer "+d.cfg.GeminiCDPKey)
	}
	return d.http.Do(req)
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
	if _, ok := d.byID[req.Model]; !ok {
		c.JSON(404, gin.H{"error": "model not found"})
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
	if _, ok := d.byID[req.Model]; !ok {
		c.JSON(404, gin.H{"error": "model not found"})
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

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
