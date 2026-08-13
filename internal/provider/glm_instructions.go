package provider

import (
	"fmt"
	"strings"

	"aurora/internal/toolcall"
	"aurora/typings/official"
)
// glmBuildInstructions 生成智谱 coding 变体的工具指令。
//
// 与 ChatGPT/DeepSeek 的标签协议不同:GLM 网页版(chatglm.cn)模型是"全部工具智能体",
// 原生训练输出结构化 tool_calls(网页端由前端解析执行)。实测(2026-08-13):
//   - ChatGPT 的 <tool_call> 标签、DeepSeek 的 <|tool▁calls▁begin|> 标签 GLM 都不认,
//     会忽略标签直接散文回答,甚至编造 shell 输出。
//   - GLM 能识别的格式是 markdown 围栏 JSON(见 internal/toolcall/fence.go),
//     且示例必须是**智谱原生 tool_calls 结构**({"type":"tool_calls","tool_calls":{...}}),
//     普通 {"name":..,"arguments":..} 示例模型不当回事。
//   - 已知固有权衡:工具名若命中智谱内置沙箱联想(bash/shell/python 等),
//     模型会编造沙箱输出(如 /mnt/data 目录),无法通过提示词覆盖;
//     list_files 等陌生工具名则稳定输出围栏 JSON。
func glmBuildInstructions(tools []official.Tool, toolChoice *official.ToolChoice) string {
	if len(tools) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("# TOOLS AVAILABLE\n")
	sb.WriteString("You are a coding agent connected to the user's computer via a custom tool bridge. You have NO built-in Python/Code Interpreter sandbox. The ONLY way to interact with the user's machine is the tools below:\n\n")
	sb.WriteString(toolcall.CompactToolsPrompt(tools))
	sb.WriteString("\n\n# TOOL CALLING FORMAT (MANDATORY)\n")
	sb.WriteString("To call a tool, output a single markdown JSON code block EXACTLY in this shape:\n")
	sb.WriteString("```json\n")
	sb.WriteString(`{"type":"tool_calls","tool_calls":{"name":"tool_name","arguments":"{\"param_name\":\"value\"}"}}`)
	sb.WriteString("\n```\n\n")
	sb.WriteString("EXAMPLE (replace tool_name with a REAL name from TOOLS AVAILABLE):\n")
	sb.WriteString("```json\n")
	sb.WriteString(`{"type":"tool_calls","tool_calls":{"name":"<tool_name>","arguments":"{\"arg1\":\"value1\"}"}}`)
	sb.WriteString("\n```\n\n")
	sb.WriteString("CRITICAL RULES:\n")
	sb.WriteString("0. Use ONLY the EXACT tool names listed under TOOLS AVAILABLE. Never rename, abbreviate or invent names. If the tool is \"bash\", calling it \"Bash\" is WRONG.\n")
	sb.WriteString("1. Output ONLY the JSON code block for tool calls — no prose before, no explanation after. Do NOT wrap it in <tool_call> tags, do NOT use <|tool▁calls▁begin|>.\n")
	sb.WriteString("2. You can call multiple tools by emitting multiple code blocks consecutively.\n")
	sb.WriteString("3. Do NOT output any other text after your tool call blocks. Wait for the tool response.\n")
	sb.WriteString("4. The JSON MUST be valid and include the 'arguments' field.\n")
	sb.WriteString("5. If you need to use a tool, do it IMMEDIATELY without preamble.\n")
	sb.WriteString("6. DO NOT use your internal/native Python tool or Code Interpreter. They run in a remote sandbox with NO access to the user's workspace. You MUST use ONLY the custom tools listed under TOOLS AVAILABLE.\n")
	sb.WriteString("7. Inside 'arguments', include ONLY the parameters listed under 'Params:' for that tool. Never add extra fields such as 'description' or 'note' — they will break the call.\n")
	sb.WriteString("8. You CANNOT see the user's files or run anything yourself. Any listing you \"remember\" is FABRICATION. NEVER simulate or imagine a tool's output — call the tool and wait for its real result.\n")
	if forced := toolChoice.ForcedFunctionName(); forced != "" {
		fmt.Fprintf(&sb, "\nCRITICAL: You MUST call the tool %q in this response. Do not call any other tool, and do not produce a final answer without calling it first.\n", forced)
	} else if toolChoice != nil && toolChoice.IsForcedNone() {
		sb.WriteString("\nCRITICAL: The user has DISABLED tool calling in this request. Do not emit any code blocks. Just answer in plain text.\n")
	}
	return sb.String()
}

// glmCodingNudge 是 glm coding 末尾强提醒(仿 DeepSeek 的 reminder 注入,
// 但用围栏 JSON 格式而非标签)。防 GLM 拿到工具结果后只输出计划/散文。
func glmCodingNudge() string {
	return "\n\n[SYSTEM INSTRUCTION: If the task is not finished, output ONLY the next ```json tool call block now — no prose, no progress report, no plan. The tool output above is REAL data from the user's machine. A file LISTING is NOT the file content; if you need file contents, call the read tool. Never ask the user to paste file contents.]" +
		windowsPathHint()
}
