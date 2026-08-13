package toolcall

import (
	"encoding/json"

	"aurora/typings/official"
)

// InferToolFromParams 当 JSON 无 name 字段时,按参数键匹配工具 schema 推断工具名。
// 实测(DeepSeek web):模型偶尔输出"参数直给"格式,如 {"file_path":"a.js"} 或
// {"command":"ls"} —— 没有 {"name":..., "arguments":...} 包装。
// 规则:obj 的所有键都是某个工具 schema 的参数子集,且仅一个工具匹配时,推断为该工具。
// 多个工具匹配(键冲突)时返回 nil(不猜测)。
func InferToolFromParams(obj map[string]any, tools []official.Tool) *official.ToolCall {
	if len(obj) == 0 || len(tools) == 0 {
		return nil
	}
	var match *official.ToolCall
	for _, t := range tools {
		if t.Type != "function" {
			continue
		}
		props := paramKeys(t.Function.Parameters)
		if len(props) == 0 {
			continue
		}
		allMatch := true
		for k := range obj {
			if !props[k] {
				allMatch = false
				break
			}
		}
		if !allMatch {
			continue
		}
		if match != nil {
			// 多个工具都匹配 → 无法唯一推断
			return nil
		}
		match = &official.ToolCall{
			ID:   generateCallID(),
			Type: "function",
			Function: official.ToolCallFunc{
				Name:      t.Function.Name,
				Arguments: marshalArguments(obj),
			},
		}
	}
	return match
}

// paramKeys 解析工具 parameters JSON schema,返回声明的属性名集合。
func paramKeys(paramsJSON json.RawMessage) map[string]bool {
	if len(paramsJSON) == 0 {
		return nil
	}
	var schema struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(paramsJSON, &schema); err != nil || schema.Properties == nil {
		return nil
	}
	out := make(map[string]bool, len(schema.Properties))
	for k := range schema.Properties {
		out[k] = true
	}
	return out
}
