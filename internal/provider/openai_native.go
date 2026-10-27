package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"aurora/internal/apierrors"
	"aurora/internal/config"
	"aurora/typings/official"

	"github.com/gin-gonic/gin"
)

// nativeToolsName 是透传 provider 的注册名(方案 B)。
const nativeToolsName = "openai-native"

// NativeTools 是「原生工具透传」provider:把 /v1/chat/completions 请求原样
// 转发到 OpenAI 兼容上游(NATIVE_TOOLS_BASE),由上游真正执行 function calling,
// 响应(含 SSE delta.tool_calls)原样回吐。它不做任何工具模拟,也不解析上游协议,
// 与网页逆向通道完全解耦——只是 aurora 内挂一个直连上游,用于方案 A 尚未覆盖的
// 工具轮(见 .scratch/aurora-provider-tool-calling/spec.md §4)。
type NativeTools struct {
	baseURL string
	apiKey  string
	// modelMap 是客户端 model id → 上游 model id(左值精确匹配 Handles)。
	modelMap map[string]string
	models   []Model
	client   *http.Client
}

// NewNativeTools 按配置构造透传 provider。映射非法(缺 '='、任一侧为空、
// 左值重复)或为空时返回 nil(fail-closed,整个 provider 不注册),
// 并打一条明确警告供部署时排查;不透传任何凭证内容。
func NewNativeTools(cfg *config.Config) *NativeTools {
	if cfg == nil {
		log.Printf("[%s] 配置为空,provider 不注册", nativeToolsName)
		return nil
	}
	base := strings.TrimSpace(cfg.NativeToolsBase)
	key := strings.TrimSpace(cfg.NativeToolsKey)
	if base == "" || key == "" {
		log.Printf("[%s] NATIVE_TOOLS_BASE / NATIVE_TOOLS_KEY 未配置,provider 不注册", nativeToolsName)
		return nil
	}
	modelMap, models, err := parseNativeToolsModelMap(cfg.NativeToolsModelMap)
	if err != nil {
		log.Printf("[%s] NATIVE_TOOLS_MODEL_MAP 解析失败(%v),provider 不注册", nativeToolsName, err)
		return nil
	}
	if len(modelMap) == 0 {
		log.Printf("[%s] NATIVE_TOOLS_MODEL_MAP 为空,provider 不注册", nativeToolsName)
		return nil
	}
	return &NativeTools{
		baseURL:  strings.TrimRight(base, "/"),
		apiKey:   key,
		modelMap: modelMap,
		models:   models,
		// 透传层不做隐式重试,60s 覆盖常见工具轮;流式不受该超时限制的语义
		// 由上游决定(与 OpenAI SDK 默认一致:超时是整轮上限)。
		client: &http.Client{Timeout: 60 * time.Second},
	}
}

// parseNativeToolsModelMap 把 "客户端 id=上游 id" 列表解析成映射与模型目录。
// 任一项非法即整体失败(fail-closed,不做部分注册)。
func parseNativeToolsModelMap(items []string) (map[string]string, []Model, error) {
	modelMap := make(map[string]string, len(items))
	models := make([]Model, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		left, right, ok := strings.Cut(item, "=")
		if !ok {
			return nil, nil, fmt.Errorf("缺少 '=': %q", item)
		}
		left, right = strings.TrimSpace(left), strings.TrimSpace(right)
		if left == "" || right == "" {
			return nil, nil, fmt.Errorf("映射两侧不能为空: %q", item)
		}
		if _, dup := modelMap[left]; dup {
			return nil, nil, fmt.Errorf("左值重复: %q", left)
		}
		modelMap[left] = right
		models = append(models, Model{ID: left, OwnedBy: nativeToolsName, Caps: []Capability{CapFunctionCall}})
	}
	return modelMap, models, nil
}

func (n *NativeTools) Name() string { return nativeToolsName }

func (n *NativeTools) Models() []Model { return n.models }

func (n *NativeTools) Handles(model string) bool {
	_, ok := n.modelMap[model]
	return ok
}

// Responses 本轮不支持:不透传 /v1/responses,也不伪造 Responses 事件。
func (n *NativeTools) Responses(c *gin.Context, req *official.ResponsesAPIRequest) {
	apierrors.InvalidRequest(c, "openai-native 仅支持 /v1/chat/completions（本轮），/v1/responses 暂不支持", "unsupported_endpoint")
}

// ChatCompletions 把请求转发到 <base>/chat/completions 并原样回吐响应。
func (n *NativeTools) ChatCompletions(c *gin.Context, req *official.APIRequest) {
	if req == nil {
		apierrors.InvalidRequest(c, "openai-native 请求体为空", "invalid_request_error")
		return
	}
	upstreamModel, ok := n.modelMap[req.Model]
	if !ok {
		// 不应发生:Handles 已拦截。
		apierrors.JSONError(c, http.StatusNotFound, "invalid_request_error", "model not found", nil, "model_not_found")
		return
	}
	body, err := nativeToolsPayload(req, upstreamModel)
	if err != nil {
		apierrors.InternalError(c, "server_error", "openai-native 构造上游请求失败: "+err.Error(), "upstream_request_error")
		return
	}

	httpReq, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, n.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		apierrors.InternalError(c, "server_error", "openai-native 构造上游请求失败: "+err.Error(), "upstream_request_error")
		return
	}
	httpReq.Header.Set("Authorization", "Bearer "+n.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	if req.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}

	resp, err := n.client.Do(httpReq)
	if err != nil {
		// 网络错误(含超时):502 + 错误文本;不重试。
		apierrors.JSONError(c, http.StatusBadGateway, "upstream_error", "openai-native 上游请求失败: "+err.Error(), nil, "upstream_error")
		return
	}
	defer resp.Body.Close()

	// 状态码与 Content-Type 原样透传:4xx/5xx 的 error body 交给 bridge 判读。
	// 流式补齐 SSE 头四件套(与同包 sseHeaders 同源,防中间层缓冲)。
	if req.Stream {
		sseHeaders(c.Writer)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		c.Writer.Header().Set("Content-Type", ct)
	} else if req.Stream {
		c.Writer.Header().Set("Content-Type", "text/event-stream")
	} else {
		c.Writer.Header().Set("Content-Type", "application/json")
	}
	c.Writer.WriteHeader(resp.StatusCode)

	if req.Stream {
		pipeNativeStream(c.Writer, resp.Body)
		return
	}
	_, _ = io.Copy(c.Writer, resp.Body)
}

// pipeNativeStream 逐块透传上游 SSE 并在每块后 flush,不解析帧内容。
func pipeNativeStream(w gin.ResponseWriter, body io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return // 客户端断开
			}
			w.Flush()
		}
		if err != nil {
			return
		}
	}
}

// nativeToolsPayload 构造转发到 OpenAI 兼容上游的请求体。
//
// handler 已把原始 body 绑定进 official.APIRequest(原始字节不可复用,且 T05
// 之前不改 chat_handler),所以这里按结构体重新序列化:结构体覆盖了 chat 表面
// 的完整字段集,不会丢 messages(含 role=tool / assistant.tool_calls)、tools、
// tool_choice、stream 等。在此基础上做三处必要修正:
//  1. model 换成映射后的上游 id,并删除 aurora 自有字段(artifact_delivery);
//  2. stop / tool_choice 在 official 里只实现了 UnmarshalJSON,默认 Marshal
//     会变形(如 "auto" → {"type":"auto"}),这里按 OpenAI 线上形态还原;
//     stop 为空数组时删掉该字段(否则变成 {"Values":null});
//  3. 消息按白名单重序列化(只留 role/content/name/tool_calls/tool_call_id),
//     attachments 等 aurora 扩展不外泄;assistant.tool_calls 的回放不需要
//     index(客户端请求里没有该字段),去掉以免严格上游拒收。
func nativeToolsPayload(req *official.APIRequest, upstreamModel string) ([]byte, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}

	delete(fields, "artifact_delivery")

	if fields["model"], err = json.Marshal(upstreamModel); err != nil {
		return nil, err
	}

	if req.Stop != nil {
		if len(req.Stop.Values) == 0 {
			// 空数组("stop": [])在 official.StopParam 下会 marshal 成
			// {"Values":null} —— 上游不认,直接删掉该字段。
			delete(fields, "stop")
		} else {
			var stop any = req.Stop.Values
			if len(req.Stop.Values) == 1 {
				stop = req.Stop.Values[0]
			}
			if fields["stop"], err = json.Marshal(stop); err != nil {
				return nil, err
			}
		}
	}

	if req.ToolChoice != nil {
		var toolChoice any = req.ToolChoice.Type
		if req.ToolChoice.Type == "function" && req.ToolChoice.Function != nil {
			toolChoice = map[string]any{
				"type":     "function",
				"function": map[string]string{"name": req.ToolChoice.Function.Name},
			}
		}
		if fields["tool_choice"], err = json.Marshal(toolChoice); err != nil {
			return nil, err
		}
	}

	if fields["messages"], err = nativeToolsMessages(req.Messages); err != nil {
		return nil, err
	}

	return json.Marshal(fields)
}

// nativeToolsMessageFields 是允许转发上游的消息字段白名单。
// attachments(以及未来任何 aurora 自有扩展)不得外泄给严格上游 —— 未声明的
// 字段一律剥掉;assistant.tool_calls[].index 另按 OpenAI 线上形态处理。
var nativeToolsMessageFields = map[string]bool{
	"role":         true,
	"content":      true,
	"name":         true,
	"tool_calls":   true,
	"tool_call_id": true,
}

// nativeToolsMessages 序列化消息列表:按白名单只保留标准字段(剥离
// attachments 等 aurora 扩展),并去掉 assistant.tool_calls 里客户端请求
// 不认识的 index 字段。
func nativeToolsMessages(msgs []official.APIMessage) (json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(msgs))
	for _, m := range msgs {
		raw, err := json.Marshal(m)
		if err != nil {
			return nil, err
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, err
		}
		for key := range obj {
			if !nativeToolsMessageFields[key] {
				delete(obj, key)
			}
		}
		if len(m.ToolCalls) > 0 {
			var calls []map[string]json.RawMessage
			if err := json.Unmarshal(obj["tool_calls"], &calls); err != nil {
				return nil, err
			}
			for _, call := range calls {
				delete(call, "index")
			}
			if obj["tool_calls"], err = json.Marshal(calls); err != nil {
				return nil, err
			}
		}
		if raw, err = json.Marshal(obj); err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return json.Marshal(out)
}
