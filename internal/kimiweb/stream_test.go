package kimiweb

import (
	"bytes"
	"strings"
	"testing"
)

// frame 构造 Connect 帧:flags(1) + 长度(4BE) + payload。
func frame(flags byte, payload string) []byte {
	b := make([]byte, 5+len(payload))
	b[0] = flags
	b[1] = byte(len(payload) >> 24)
	b[2] = byte(len(payload) >> 16)
	b[3] = byte(len(payload) >> 8)
	b[4] = byte(len(payload))
	copy(b[5:], payload)
	return b
}

// TestConsumeStreamTextAndThink 验证正文/思考的 set+append 增量拼接与收尾。
func TestConsumeStreamTextAndThink(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(frame(0, `{"heartbeat":{}}`))
	buf.Write(frame(0, `{"op":"set","eventOffset":1,"chat":{"id":"c1"}}`))
	buf.Write(frame(0, `{"op":"set","mask":"message","eventOffset":2,"message":{"id":"m1","role":"assistant","status":"MESSAGE_STATUS_GENERATING"}}`))
	buf.Write(frame(0, `{"op":"set","mask":"block.think","eventOffset":3,"block":{"id":"1","think":{"content":"用户问"}}}`))
	buf.Write(frame(0, `{"op":"append","mask":"block.think.content","eventOffset":4,"block":{"id":"1","think":{"content":"了一个问题"}}}`))
	buf.Write(frame(0, `{"op":"set","mask":"block.text","eventOffset":5,"block":{"id":"2","text":{"content":"答案"}}}`))
	buf.Write(frame(0, `{"op":"append","mask":"block.text.content","eventOffset":6,"block":{"id":"2","text":{"content":"是 42"}}}`))
	buf.Write(frame(0, `{"op":"set","mask":"message.status","eventOffset":7,"message":{"id":"m1","status":"MESSAGE_STATUS_COMPLETED"}}`))
	buf.Write(frame(0, `{"eventOffset":8,"done":{}}`))
	buf.Write(frame(2, `{}`)) // END_OF_STREAM

	var deltas []Delta
	res := ConsumeStream(bytes.NewReader(buf.Bytes()), func(d Delta) { deltas = append(deltas, d) })
	if res.Text != "答案是 42" {
		t.Errorf("Text = %q, want 答案是 42", res.Text)
	}
	if res.Reasoning != "用户问了一个问题" {
		t.Errorf("Reasoning = %q, want 用户问了一个问题", res.Reasoning)
	}
	if !res.Finished {
		t.Error("Finished = false, want true")
	}
	if res.Err != "" {
		t.Errorf("Err = %q, want empty", res.Err)
	}
}

// TestConsumeStreamToolCall 验证原生工具调用生命周期:
// PENDING 起帧 → args append → RUNNING 定稿(整单上报)→ contents DONE。
func TestConsumeStreamToolCall(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(frame(0, `{"op":"set","eventOffset":1,"block":{"id":"4","parentId":"2","tool":{"toolCallId":"ipython_0","name":"ipython","status":"STATUS_PENDING"}}}`))
	buf.Write(frame(0, `{"op":"append","mask":"block.tool.args","eventOffset":2,"block":{"id":"4","tool":{"args":"{\"code\":"}}}`))
	buf.Write(frame(0, `{"op":"append","mask":"block.tool.args","eventOffset":3,"block":{"id":"4","tool":{"args":" \"17*23\"}"}}}`))
	buf.Write(frame(0, `{"op":"set","mask":"block.tool.args,block.tool.status,block.tool.toolCallId","eventOffset":4,"block":{"id":"4","tool":{"toolCallId":"ipython:1","args":"{\"code\":\"17*23\"}","status":"STATUS_RUNNING"}}}`))
	buf.Write(frame(0, `{"op":"set","mask":"block.tool.contents,block.tool.status","eventOffset":5,"block":{"id":"4","tool":{"contents":[{"text":"391"}],"status":"STATUS_DONE"}}}`))
	buf.Write(frame(0, `{"op":"set","mask":"block.text","eventOffset":6,"block":{"id":"6","text":{"content":"17×23=391"}}}`))
	buf.Write(frame(0, `{"eventOffset":7,"done":{}}`))
	buf.Write(frame(2, `{}`))

	var calls []*ToolCall
	res := ConsumeStream(bytes.NewReader(buf.Bytes()), func(d Delta) {
		if d.ToolCall != nil {
			calls = append(calls, d.ToolCall)
		}
	})
	if len(calls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(calls))
	}
	c := calls[0]
	if c.Name != "ipython" || c.ID != "ipython:1" {
		t.Errorf("tool call = %s/%s, want ipython/ipython:1", c.Name, c.ID)
	}
	if c.Arguments != `{"code":"17*23"}` {
		t.Errorf("args = %q, want {\"code\":\"17*23\"}", c.Arguments)
	}
	if res.Text != "17×23=391" {
		t.Errorf("Text = %q, want 17×23=391", res.Text)
	}
}

// TestConsumeStreamError 验证收尾帧带 error 时报错。
func TestConsumeStreamError(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(frame(0, `{"heartbeat":{}}`))
	buf.Write(frame(2, `{"error":{"code":"invalid_argument"}}`))
	res := ConsumeStream(bytes.NewReader(buf.Bytes()), func(d Delta) {})
	if !strings.Contains(res.Err, "invalid_argument") {
		t.Errorf("Err = %q, want contain invalid_argument", res.Err)
	}
}

// TestConsumeStreamEmpty 验证空流报错。
func TestConsumeStreamEmpty(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(frame(2, `{}`))
	res := ConsumeStream(bytes.NewReader(buf.Bytes()), func(d Delta) {})
	if res.Err == "" {
		t.Error("empty stream should report Err")
	}
}
