package glmweb

import (
	"strings"
	"testing"
)

// TestConsumeStream 验证智谱 SSE 帧解析:parts 全量重发,差值输出增量,
// status=finish 收尾,conversation_id/model 汇总。
func TestConsumeStream(t *testing.T) {
	sse := `data: {"id":"1","conversation_id":"conv_abc","status":"start","model":"GLM-5.2","parts":[{"role":"assistant","content":[{"type":"think","think":"让我想"},{"type":"text","text":"你好"}]}]}

data: {"id":"2","conversation_id":"conv_abc","parts":[{"role":"assistant","content":[{"type":"think","think":"让我想想"},{"type":"text","text":"你好,世界"}]}]}

data: {"id":"3","conversation_id":"conv_abc","parts":[{"role":"assistant","content":[{"type":"think","think":"让我想想"},{"type":"text","text":"你好,世界!"}]}]}

data: {"id":"4","conversation_id":"conv_abc","status":"finish","parts":[{"role":"assistant","content":[{"type":"think","think":"让我想想"},{"type":"text","text":"你好,世界!"}]}]}

data: [DONE]
`

	var texts, thinks []string
	res := ConsumeStream(strings.NewReader(sse), func(d Delta) {
		if d.Text != "" {
			texts = append(texts, d.Text)
		}
		if d.Reasoning != "" {
			thinks = append(thinks, d.Reasoning)
		}
	})

	if res.ConversationID != "conv_abc" {
		t.Fatalf("conversation_id = %q, want conv_abc", res.ConversationID)
	}
	if res.Model != "GLM-5.2" {
		t.Fatalf("model = %q, want GLM-5.2", res.Model)
	}
	if !res.Finished {
		t.Fatal("Finished = false, want true")
	}
	// 文本增量: 你好 → 你好,世界 → 你好,世界!
	wantText := []string{"你好", ",世界", "!"}
	if strings.Join(texts, "") != "你好,世界!" {
		t.Fatalf("text deltas = %v, want cumulative 你好,世界!", texts)
	}
	if len(texts) != len(wantText) {
		t.Fatalf("text delta count = %d, want %d (%v)", len(texts), len(wantText), texts)
	}
	// 思考增量: 让我想 → 让我想想(只发新增部分)
	if strings.Join(thinks, "") != "让我想想" {
		t.Fatalf("reasoning deltas = %v, want cumulative 让我想想", thinks)
	}
	if len(thinks) != 2 {
		t.Fatalf("reasoning delta count = %d, want 2", len(thinks))
	}
}

// TestConsumeStreamNativeToolCall 验证原生 tool_calls content 帧解析 + 去重 + finish 哨兵。
func TestConsumeStreamNativeToolCall(t *testing.T) {
	sse := `data: {"id":"1","conversation_id":"c1","status":"start","parts":[{"role":"assistant","content":[{"type":"tool_calls","tool_calls":{"id":"tool-111","name":"search","arguments":"{\"q\":\"x\"}"}}]}]}

data: {"id":"2","conversation_id":"c1","parts":[{"role":"assistant","content":[{"type":"tool_calls","tool_calls":{"id":"tool-111","name":"search","arguments":"{\"q\":\"x\"}"}}]}]}

data: {"id":"3","conversation_id":"c1","parts":[{"role":"assistant","content":[{"type":"tool_calls","tool_calls":{"name":"finish","arguments":"{}"}},{"type":"text","text":"好了"}]}]}

data: {"id":"4","conversation_id":"c1","status":"finish","parts":[{"role":"assistant","content":[{"type":"text","text":"好了"}]}]}
`
	var calls []*ToolCall
	var text string
	res := ConsumeStream(strings.NewReader(sse), func(d Delta) {
		if d.ToolCall != nil {
			calls = append(calls, d.ToolCall)
		}
		text += d.Text
	})
	if len(calls) != 1 {
		t.Fatalf("tool calls = %d, want 1 (deduped)", len(calls))
	}
	if calls[0].Name != "search" || calls[0].ID != "tool-111" {
		t.Errorf("tool call = %+v, want search/tool-111", calls[0])
	}
	// finish 哨兵不产生调用
	if text != "好了" {
		t.Errorf("text = %q, want 好了", text)
	}
	if !res.Finished {
		t.Error("Finished = false, want true")
	}
}

// TestConsumeStreamErrFrame 验证 last_error 帧上报错误。
func TestConsumeStreamErrFrame(t *testing.T) {
	sse := `data: {"id":"1","conversation_id":"conv_x","status":"error","last_error":{"code":40012,"msg":"签名错误"},"parts":[]}

data: {"id":"2","conversation_id":"conv_x","status":"finish","parts":[]}
`
	res := ConsumeStream(strings.NewReader(sse), func(Delta) {})
	if res.Err == "" {
		t.Fatal("Err = empty, want error msg from last_error")
	}
	if res.Finished != true {
		t.Fatal("Finished = false, want true (finish frame present)")
	}
}

// TestConsumeStreamEmpty 验证空流(无任何帧)报 empty stream。
func TestConsumeStreamEmpty(t *testing.T) {
	res := ConsumeStream(strings.NewReader(""), func(Delta) {})
	if res.Err == "" {
		t.Fatal("Err = empty, want empty stream error")
	}
}

// TestConsumeStreamLiteralNewlineDedup 两阶段去重:GLM 上游流式分片里换行是
// 字面 \n 两字符,结束帧重发完整文本用真换行 —— 两种表示不一致曾让 HasPrefix
// 失效,结束帧全文被当增量重发(线上:末尾整段重复 + 字面 \n 直通客户端)。
// 帧级归一后两阶段前缀关系成立,重复消失。
func TestConsumeStreamLiteralNewlineDedup(t *testing.T) {
	// 帧内 \\n(JSON 转义后)= 字面 \n 两字符(上游分片形态);
	// 帧 4 的 \n(JSON 转义后)= 真换行(结束帧完整文本形态)。
	sse := `data: {"id":"1","parts":[{"role":"assistant","content":[{"type":"text","text":"您好!"}]}]}

data: {"id":"2","parts":[{"role":"assistant","content":[{"type":"text","text":"您好!\\n我是"}]}]}

data: {"id":"3","parts":[{"role":"assistant","content":[{"type":"text","text":"您好!\\n我是智谱助手。"}]}]}

data: {"id":"4","parts":[{"role":"assistant","content":[{"type":"text","text":"您好!\n我是智谱助手。"}]}]}

data: [DONE]
`
	var texts []string
	res := ConsumeStream(strings.NewReader(sse), func(d Delta) {
		if d.Text != "" {
			texts = append(texts, d.Text)
		}
	})
	if !res.Finished {
		t.Fatal("Finished = false, want true")
	}
	joined := strings.Join(texts, "")
	want := "您好!\n我是智谱助手。"
	if joined != want {
		t.Fatalf("拼接输出 = %q, want %q", joined, want)
	}
	// 字面 \n 不得直通客户端
	if strings.Contains(joined, "\\n") {
		t.Fatalf("输出含字面 \\n:%q", joined)
	}
	// 结束帧(帧 4)与归一后的累计全等 → 差值为空,不产生重复 delta
	if len(texts) != 3 {
		t.Fatalf("delta count = %d, want 3(帧4 应幂等无增量): %q", len(texts), texts)
	}
}
