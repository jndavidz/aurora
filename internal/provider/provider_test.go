package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"aurora/internal/config"
	"aurora/typings/official"
)

func newTestConfig() *config.Config {
	return &config.Config{
		DeepSeekWebTokens: "tokens/deepseek_tokens.txt",
		DeepSeekModels:    nil, // 用默认目录
	}
}

func TestParseDeepSeekModel(t *testing.T) {
	cases := []struct {
		id      string
		variant string
		mode    string
		caps    []Capability
	}{
		{"deepseek-v4-flash-chat", variantChat, modeQuick, []Capability{CapWebSearch, CapReasoning, CapVision}},
		{"deepseek-v4-pro-chat", variantChat, modeExpert, []Capability{CapReasoning}},
		{"deepseek-v4-flash-coding", variantCoding, "", []Capability{CapFunctionCall, CapReasoning}},
		{"deepseek-v4-pro-coding", variantCoding, "", []Capability{CapFunctionCall, CapReasoning}},
		{"gpt-5", "", "", nil}, // 非 deepseek 命名 → nil
		{"deepseek-v4-chat", variantChat, modeExpert, []Capability{CapReasoning}}, // 无 flash/pro → 默认 expert
	}
	for _, c := range cases {
		m := parseDeepSeekModel(c.id)
		if c.variant == "" && c.caps == nil {
			if m != nil {
				t.Errorf("%s: expected nil, got %+v", c.id, m)
			}
			continue
		}
		if m == nil {
			t.Errorf("%s: expected model, got nil", c.id)
			continue
		}
		if m.Variant != c.variant {
			t.Errorf("%s: variant = %s, want %s", c.id, m.Variant, c.variant)
		}
		if c.mode != "" && m.Mode != c.mode {
			t.Errorf("%s: mode = %s, want %s", c.id, m.Mode, c.mode)
		}
		if len(m.Caps) != len(c.caps) {
			t.Errorf("%s: caps = %v, want %v", c.id, m.Caps, c.caps)
		}
	}
}

func TestRegistryResolve(t *testing.T) {
	d := NewDeepSeek(newTestConfig())
	r := NewRegistry()
	r.Register(d)

	if p := r.Resolve("deepseek-v4-flash-chat"); p == nil {
		t.Fatal("Resolve(deepseek-v4-flash-chat) = nil, want DeepSeek")
	}
	if p := r.Resolve("auto"); p != nil {
		t.Fatalf("Resolve(auto) = %v, want nil (default ChatGPT)", p.Name())
	}
	if p := r.Resolve("gpt-5-6"); p != nil {
		t.Fatalf("Resolve(gpt-5-6) = %v, want nil", p.Name())
	}
	if len(r.Models()) != 4 {
		t.Errorf("Models() = %d entries, want 4", len(r.Models()))
	}
}

// chat 变体硬规则:即使客户端带 tools,拍平的 prompt 也绝不能含任何工具信息。
func TestFlattenChatInputNoToolInjection(t *testing.T) {
	req := &official.ResponsesAPIRequest{
		Model: "deepseek-v4-flash-chat",
		Input: json.RawMessage(`[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"列出文件"}]}
		]`),
		Instructions: json.RawMessage(`"你是助手"`),
		// 客户端带了 tools —— chat 变体必须剥离
		Tools: []official.Tool{{Type: "function", Function: official.ToolFunction{Name: "bash"}}},
	}
	req.ToolChoice = &official.ToolChoice{Type: "auto"}

	prompt := flattenChatInput(req, true)
	for _, banned := range []string{"bash", "tool_call", "TOOLS", "tool_calls", "arguments"} {
		if strings.Contains(prompt, banned) {
			t.Errorf("chat prompt must not contain tool info %q:\n%s", banned, prompt)
		}
	}
	if !strings.Contains(prompt, "你是助手") || !strings.Contains(prompt, "列出文件") {
		t.Errorf("chat prompt lost conversation content:\n%s", prompt)
	}
}

// chat 变体:function_call/function_call_output item 防御性跳过,不上游注入。
func TestFlattenChatInputSkipsToolItems(t *testing.T) {
	req := &official.ResponsesAPIRequest{
		Model: "deepseek-v4-flash-chat",
		Input: json.RawMessage(`[
			{"type":"message","role":"user","content":"读一下"},
			{"type":"function_call","call_id":"c1","name":"read","arguments":"{\"path\":\"a.txt\"}"},
			{"type":"function_call_output","call_id":"c1","output":"文件内容"},
			{"type":"message","role":"user","content":"总结"}
		]`),
	}
	prompt := flattenChatInput(req, true)
	if strings.Contains(prompt, "read") || strings.Contains(prompt, "文件内容") {
		t.Errorf("chat prompt must skip tool items:\n%s", prompt)
	}
	if !strings.Contains(prompt, "读一下") || !strings.Contains(prompt, "总结") {
		t.Errorf("chat prompt lost messages:\n%s", prompt)
	}
	// 不加角色前缀(实测 DeepSeek 网页 prompt 是纯文本)。
	if strings.Contains(prompt, "User:") || strings.Contains(prompt, "Assistant:") {
		t.Errorf("chat prompt must not contain role text prefixes:\n%s", prompt)
	}
}

// 多轮 input 重放:function_call/function_call_output 保留(供 coding 变体回放)。
func TestResponsesInputItemsMultiTurn(t *testing.T) {
	req := &official.ResponsesAPIRequest{
		Input: json.RawMessage(`[
			{"type":"message","role":"user","content":"读文件"},
			{"type":"function_call","call_id":"c1","name":"read","arguments":"{\"path\":\"a.txt\"}"},
			{"type":"function_call_output","call_id":"c1","output":"内容A"},
			{"type":"message","role":"user","content":"继续"}
		]`),
	}
	items := responsesInputItems(req.Input)
	if len(items) != 4 {
		t.Fatalf("items = %d, want 4", len(items))
	}
	if items[1].Type != "function_call" || !strings.Contains(items[1].Text, "\"name\":\"read\"") || !strings.Contains(items[1].Text, "a.txt") {
		t.Errorf("function_call item wrong: %+v", items[1])
	}
	if items[2].Type != "function_call_output" || items[2].Text != "内容A" {
		t.Errorf("function_call_output item wrong: %+v", items[2])
	}
}

// 双接口共享核心:APIRequest messages 与 Responses input 应产生相同的 prompt。
func TestSharedPromptCore(t *testing.T) {
	apiReq := &official.APIRequest{
		Messages: []official.APIMessage{
			{Role: "system", Content: official.MessageContent{TextValue: "你是助手"}},
			{Role: "user", Content: official.MessageContent{TextValue: "读一下"}},
			{Role: "assistant", ToolCalls: []official.ToolCallRef{{Index: 0, ID: "c1", Type: "function", Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{Name: "read", Arguments: `{"path":"a.txt"}`}}}},
			{Role: "tool", ToolCallID: "c1", Content: official.MessageContent{TextValue: "内容A"}},
			{Role: "user", Content: official.MessageContent{TextValue: "总结"}},
		},
	}
	// chat 变体:两种请求形态的 prompt 应一致(纯文本、跳过工具)
	chatAPI := flattenChatInputAPI(apiReq)
	respReq := &official.ResponsesAPIRequest{
		Input: json.RawMessage(`[
			{"type":"message","role":"user","content":"读一下"},
			{"type":"function_call","call_id":"c1","name":"read","arguments":"{\"path\":\"a.txt\"}"},
			{"type":"function_call_output","call_id":"c1","output":"内容A"},
			{"type":"message","role":"user","content":"总结"}
		]`),
		Instructions: json.RawMessage(`"你是助手"`),
	}
	chatResp := flattenChatInput(respReq, true)
	if chatAPI != chatResp {
		t.Errorf("chat prompt 不一致:\nAPI=%q\nRESP=%q", chatAPI, chatResp)
	}
	if strings.Contains(chatAPI, "read") || strings.Contains(chatAPI, "Tool result") {
		t.Errorf("chat prompt 不得含工具信息: %q", chatAPI)
	}

	// coding 变体:工具 item 保留
	codingAPI := buildDeepSeekCodingPromptAPI(apiReq)
	if !strings.Contains(codingAPI, "<|tool_call_begin|>") || !strings.Contains(codingAPI, "Tool result: 内容A") {
		t.Errorf("coding prompt 应含工具回放: %q", codingAPI)
	}
}

// 角色感知 nudge:最后是工具结果时,提示词必须含"继续调工具"强提醒。
func TestToolResultNudge(t *testing.T) {
	// 最后一条是工具结果 → 强 nudge
	items := []responsesInputItem{
		{Type: "message", Role: "user", Text: "读文件"},
		{Type: "function_call", Role: "assistant", Text: `{"path":"a.txt"}`},
		{Type: "function_call_output", Role: "tool", Text: "内容A"},
	}
	if !lastItemIsToolResult(items) {
		t.Fatal("应识别为工具结果结尾")
	}
	tools := []official.Tool{{Type: "function", Function: official.ToolFunction{Name: "read"}}}
	prompt := buildCodingPromptItems(items, "", tools, nil)
	if !strings.Contains(prompt, "A file LISTING is NOT the file content") {
		t.Errorf("工具结果后应含强 nudge:\n%s", prompt)
	}

	// 最后是用户消息 → 通用 nudge(不含强提醒)
	items2 := []responsesInputItem{{Type: "message", Role: "user", Text: "读文件"}}
	if lastItemIsToolResult(items2) {
		t.Fatal("用户消息结尾不应算工具结果")
	}
	prompt2 := buildCodingPromptItems(items2, "", tools, nil)
	if strings.Contains(prompt2, "A file LISTING is NOT the file content") {
		t.Errorf("用户消息结尾不应含工具结果强 nudge:\n%s", prompt2)
	}
}

// 兜底解析:模型输出 markdown 围栏 JSON(不带 <|tool▁calls▁begin|> 标签)时,
// mergeRecoveredCalls 应能扫出工具调用(ZCode 实测失败场景)。
func TestMergeRecoveredCallsFencedJSON(t *testing.T) {
	tools := []official.Tool{{Type: "function", Function: official.ToolFunction{Name: "bash"}}}
	text := "让我先查看项目结构。``` {\"name\": \"bash\", \"arguments\": {\"command\": \"find . -type f | head -100\"}}```"
	calls := mergeRecoveredCalls(nil, text, tools)
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if calls[0].Function.Name != "bash" || !strings.Contains(calls[0].Function.Arguments, "find") {
		t.Errorf("解析错误: %+v", calls[0])
	}
	// 去重:已解析的与兜底的不重复
	dup := mergeRecoveredCalls(calls, text, tools)
	if len(dup) != 1 {
		t.Errorf("去重失败: %d", len(dup))
	}
}

// Responses 风格工具(顶层 name/description)必须能解析出 Function 字段。
func TestResponsesStyleToolUnmarshal(t *testing.T) {
	raw := `[{"type":"function","name":"bash","description":"Execute a bash command","parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}]`
	var tools []official.Tool
	if err := json.Unmarshal([]byte(raw), &tools); err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 {
		t.Fatalf("tools = %d", len(tools))
	}
	if tools[0].Function.Name != "bash" {
		t.Fatalf("Function.Name = %q, want bash(responses 顶层格式)", tools[0].Function.Name)
	}
	if tools[0].Function.Description == "" {
		t.Error("Description 应为空字符串以外")
	}
	if len(tools[0].Function.Parameters) == 0 {
		t.Error("Parameters 不应为空")
	}
	// chat.completions 嵌套格式回归
	raw2 := `[{"type":"function","function":{"name":"read","description":"d","parameters":{"type":"object"}}}]`
	var tools2 []official.Tool
	if err := json.Unmarshal([]byte(raw2), &tools2); err != nil {
		t.Fatal(err)
	}
	if tools2[0].Function.Name != "read" {
		t.Fatalf("嵌套格式 Function.Name = %q", tools2[0].Function.Name)
	}
}
