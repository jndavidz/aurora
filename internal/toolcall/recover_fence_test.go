package toolcall

import (
	"testing"

	"aurora/typings/official"
)

func TestRecoverFencedJSON(t *testing.T) {
	text := "让我先查看项目结构，然后阅读核心源代码。``` {\"name\": \"bash\", \"arguments\": {\"command\": \"find . -type f | head -100\"}}```"
	tools := []official.Tool{{Type: "function", Function: official.ToolFunction{Name: "bash"}}}
	calls := RecoverFromText(text, tools)
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1 (text=%q)", len(calls), text)
	}
	if calls[0].Function.Name != "bash" {
		t.Errorf("name = %q", calls[0].Function.Name)
	}
}
