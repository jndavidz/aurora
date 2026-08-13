package provider

import (
	"fmt"
	"strings"

	"aurora/internal/toolcall"
	"aurora/typings/official"
)

// glmBuildInstructions 生成智谱 coding 变体的指令。
//
// 定位:智谱 coding 变体是"云端代码执行助手"(见 docs/GLM.md §四)。
// 智谱网页版模型(chatglm.cn "全部工具智能体")无 function calling 训练,
// 20+ 组实测确认它无法可靠输出外部工具调用;它的真实能力是自带的
// execute_sandbox_code —— 在智谱云端真实执行代码并回传结果。
//
// 因此指令策略:
//   - 承认模型有云端代码执行能力(不禁止,这是它真实可用的能力);
//   - 客户端自定义工具(TOOLS AVAILABLE)作为"尽力而为"通道:模型若按
//     智谱原生 tool_calls 结构或围栏 JSON 输出,aurora 的 FenceParser /
//     原生 tool_calls 解析会捕获并转发;不强制,模型不输出也不影响。
func glmBuildInstructions(tools []official.Tool, toolChoice *official.ToolChoice) string {
	var sb strings.Builder
	sb.WriteString("You are a coding assistant.\n")
	if len(tools) > 0 {
		sb.WriteString("\n# TOOLS AVAILABLE(尽力而为通道)\n")
		sb.WriteString("The user may expose custom tools on their machine. If you decide to use one, output a single markdown JSON code block EXACTLY in this shape:\n")
		sb.WriteString("```json\n")
		sb.WriteString(`{"type":"tool_calls","tool_calls":{"name":"tool_name","arguments":"{\"param\":\"value\"}"}}`)
		sb.WriteString("\n```\n\n")
		sb.WriteString("Tools:\n")
		sb.WriteString(toolcall.CompactToolsPrompt(tools))
		sb.WriteString("\n\nRules for the code block (only when you actually call a custom tool):\n")
		sb.WriteString("- Use ONLY tool names listed above; output ONLY the JSON block, no prose.\n")
		sb.WriteString("- If you do NOT need a custom tool, just answer normally.\n")
	} else {
		sb.WriteString("\nYou can write and run code to help answer questions.\n")
	}
	if forced := toolChoice.ForcedFunctionName(); forced != "" {
		fmt.Fprintf(&sb, "\nCRITICAL: The user requires you to call the tool %q. Output ONLY its JSON code block.\n", forced)
	} else if toolChoice != nil && toolChoice.IsForcedNone() {
		sb.WriteString("\nThe user has DISABLED tool calling. Do not emit any code blocks.\n")
	}
	return sb.String()
}

// glmCodingNudge 是 glm coding 末尾的轻提醒(保持简洁,不施压)。
// 工具结果后模型自然继续回答或继续执行沙箱,无需强 nudge。
func glmCodingNudge() string {
	return "\n\n[SYSTEM INSTRUCTION: Continue with the task. If you need more information, use your available capabilities or call a custom tool from the list.]"
}
