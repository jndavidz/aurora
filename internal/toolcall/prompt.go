package toolcall

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"aurora/typings/official"
)

// BuildInstructions 生成 system prompt 块,教导模型按 <tool_call>{...}</tool_call>
// 协议输出工具调用。tools 为空时返回 ""。
// 协议文本与  保持一致,但使用英语(目标用户以英文/中文为主)。
func BuildInstructions(tools []official.Tool, toolChoice *official.ToolChoice) string {
	if len(tools) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("# TOOLS AVAILABLE\n")
	sb.WriteString("You have access to the following tools. Use the EXACT tool name from the list below — do NOT rename, abbreviate or invent names.\n\n")
	sb.WriteString(compactToolsPrompt(tools))
	sb.WriteString("\n\n# TOOL CALLING FORMAT (MANDATORY)\n")
	sb.WriteString("To call a tool, output a JSON object wrapped EXACTLY in these tags:\n")
	sb.WriteString("<tool_call>\n")
	sb.WriteString(`{"name": "tool_name", "arguments": {"param_name": "value"}}`)
	sb.WriteString("\n</tool_call>\n\n")
	sb.WriteString("EXAMPLE OF MULTIPLE TOOL CALLS (replace <tool_name> with a REAL name from the list above):\n")
	sb.WriteString("<tool_call>\n")
	sb.WriteString(`{"name": "<tool_name>", "arguments": {"arg1": "value1"}}`)
	sb.WriteString("\n</tool_call>\n")
	sb.WriteString("<tool_call>\n")
	sb.WriteString(`{"name": "<tool_name>", "arguments": {"arg1": "value2"}}`)
	sb.WriteString("\n</tool_call>\n\n")
	sb.WriteString("CRITICAL RULES:\n")
	sb.WriteString("0. Use ONLY the EXACT tool names listed under TOOLS AVAILABLE. Names are case-sensitive: if the tool is \"bash\", calling it \"Bash\" is WRONG and will fail. Never rename, abbreviate or invent names. If the available tool is \"read\", do NOT call \"read_file\". Copy the name character-for-character, including case.\n")
	sb.WriteString("1. ONLY use the tags above for tool calling. NEVER output raw JSON without tags.\n")
	sb.WriteString("2. You can call multiple tools by emitting multiple <tool_call> blocks consecutively.\n")
	sb.WriteString("3. Do NOT output any other text after your <tool_call> blocks. Wait for the tool response.\n")
	sb.WriteString("4. The JSON inside the tags MUST be valid and include the 'arguments' field.\n")
	sb.WriteString("5. If you need to use a tool, do it IMMEDIATELY without preamble.\n")
	sb.WriteString("6. DO NOT use your internal/native Python tool, Advanced Data Analysis, or Code Interpreter. They run in a remote sandbox on your servers and have NO access to the user's workspace. You MUST use ONLY the custom tools listed under TOOLS AVAILABLE (like 'glob', 'read', 'grep', or 'bash').\n")
	sb.WriteString("7. Inside 'arguments', include ONLY the parameters listed under 'Params:' for that tool. Never add extra fields such as 'description', 'explanation' or 'note' — they will break the call.\n")
	if forced := toolChoice.ForcedFunctionName(); forced != "" {
		fmt.Fprintf(&sb, "\nCRITICAL: You MUST call the tool %q in this response. Do not call any other tool, and do not produce a final answer without calling it first.\n", forced)
	} else if toolChoice != nil && toolChoice.IsForcedNone() {
		sb.WriteString("\nCRITICAL: The user has DISABLED tool calling in this request. Do not emit any <tool_call> blocks. Just answer in plain text.\n")
	}
	return sb.String()
}

// compactToolsPrompt 把工具列表渲染成人可读的多行描述。
func compactToolsPrompt(tools []official.Tool) string {
	var sb strings.Builder
	for _, t := range tools {
		if t.Type != "function" {
			sb.WriteString("- ")
			// 非 function 工具:原样 JSON 化
			b, _ := json.Marshal(t)
			sb.Write(b)
			sb.WriteByte('\n')
			continue
		}
		fmt.Fprintf(&sb, "- %s: %s\n", t.Function.Name, t.Function.Description)
		var schema struct {
			Type       string                    `json:"type"`
			Properties map[string]map[string]any `json:"properties"`
			Required   []string                  `json:"required"`
		}
		if len(t.Function.Parameters) == 0 {
			continue
		}
		if err := json.Unmarshal(t.Function.Parameters, &schema); err != nil || schema.Type != "object" || len(schema.Properties) == 0 {
			continue
		}
		sb.WriteString("  Params:\n")
		// 排序以保证稳定输出(便于测试和 prompt 缓存)
		keys := make([]string, 0, len(schema.Properties))
		for k := range schema.Properties {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, key := range keys {
			prop := schema.Properties[key]
			isReq := "optional"
			for _, r := range schema.Required {
				if r == key {
					isReq = "required"
					break
				}
			}
			desc, _ := prop["description"].(string)
			typeStr, _ := prop["type"].(string)
			if typeStr == "" {
				typeStr = "string"
			}
			if enum, ok := prop["enum"].([]any); ok {
				var opts []string
				for _, e := range enum {
					opts = append(opts, fmt.Sprint(e))
				}
				if desc != "" {
					desc += " "
				}
				desc += "Options: [" + strings.Join(opts, ", ") + "]"
			}
			if desc != "" {
				fmt.Fprintf(&sb, "    * %s (%s, %s): %s\n", key, typeStr, isReq, desc)
			} else {
				fmt.Fprintf(&sb, "    * %s (%s, %s)\n", key, typeStr, isReq)
			}
		}
	}
	return sb.String()
}

// FirstToolCallExample 根据 tools 列表的语义,生成一个具体的"先做这个"示例,
// 帮模型跳出 sandbox 思维。优先级:bash/shell → glob → list_files → read → 任意。
// workingDir 用于替换占位符;若为空用 "."。
func FirstToolCallExample(tools []official.Tool, workingDir string) string {
	if len(tools) == 0 {
		return ""
	}
	wd := strings.ReplaceAll(workingDir, `\`, `\\`)
	if wd == "" {
		wd = "."
	}
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		if t.Type == "function" {
			names = append(names, t.Function.Name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	if n := pickFirst(names, ShellToolNames); n != "" {
		return fmt.Sprintf(`<tool_call>{"name": %q, "arguments": {"command": "Get-ChildItem -Force"}}</tool_call>`, n)
	}
	if n := pickFirst(names, []string{"glob", "find", "search_files", "file_search"}); n != "" {
		return fmt.Sprintf(`<tool_call>{"name": %q, "arguments": {"pattern": "*"}}</tool_call>`, n)
	}
	if n := pickFirst(names, []string{"list", "ls", "read_directory", "list_files", "list_directory"}); n != "" {
		return fmt.Sprintf(`<tool_call>{"name": %q, "arguments": {"path": %q}}</tool_call>`, n, wd)
	}
	if n := pickFirst(names, []string{"read", "read_file", "cat", "open", "view"}); n != "" {
		return fmt.Sprintf(`<tool_call>{"name": %q, "arguments": {"filePath": %q}}</tool_call>`, n, wd)
	}
	return fmt.Sprintf(`<tool_call>{"name": %q, "arguments": {}}</tool_call>`, names[0])
}

func pickFirst(haystack []string, candidates []string) string {
	for _, h := range haystack {
		hl := strings.ToLower(h)
		for _, c := range candidates {
			if hl == c {
				return h
			}
		}
	}
	return ""
}

// ExtractWorkingDir 从 messages 中扫描 "Working directory: X" 模式,
// Kilo/OpenCode 等客户端会在 user 消息里塞这个 hint。
func ExtractWorkingDir(messages []official.APIMessage) string {
	for _, m := range messages {
		for _, line := range strings.Split(m.Content.Text(), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "Working directory:") {
				return strings.TrimSpace(strings.TrimPrefix(line, "Working directory:"))
			}
		}
	}
	return ""
}

// ReadToolNames 候选文件读取工具名,按顺序匹配第一个出现在 tools 列表里的。
var ReadToolNames = []string{
	"read", "read_file", "readfile", "get_file_contents", "read_text_file",
	"cat", "open", "view",
}

// ReadPathCandidates 参数名候选(path / filePath / file_path / filename / file),
// 按出现顺序挑第一个在工具 schema 中声明的。
var ReadPathCandidates = []string{"path", "filePath", "file_path", "filename", "file", "file_name"}

// ResolveReadTool 找到 tools 列表里第一个文件读取工具,返回 (name, pathParam)。
// 没找到就返回 ("", "")。
func ResolveReadTool(tools []official.Tool) (string, string) {
	for _, t := range tools {
		if t.Type != "function" {
			continue
		}
		name := strings.ToLower(t.Function.Name)
		if !contains(ReadToolNames, name) {
			continue
		}
		return t.Function.Name, pickReadPathParam(t.Function.Parameters)
	}
	return "", ""
}

func pickReadPathParam(paramsJSON json.RawMessage) string {
	if len(paramsJSON) == 0 {
		return "path"
	}
	var schema struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(paramsJSON, &schema); err != nil || schema.Properties == nil {
		return "path"
	}
	for _, cand := range ReadPathCandidates {
		if _, ok := schema.Properties[cand]; ok {
			return cand
		}
	}
	return "path"
}

// FinalNudge 是给模型末尾追加的"先做这个,别分析 sandbox"系统指令。
// 用 lastRole 决定上下文:
//   - tool   : 提醒把 tool 输出当 ground truth,继续调用或总结
//   - user   : 强制模型立刻发 <tool_call>(不思考、不描述环境)
//   - 其他   : 返回空
func FinalNudge(tools []official.Tool, messages []official.APIMessage) string {
	if len(tools) == 0 || len(messages) == 0 {
		return ""
	}
	last := messages[len(messages)-1]
	switch last.Role {
	case "tool", "function":
		// 拿不到具体的 tool 名(API 没有 tool_call_id 映射),用一个通用表达。
		// 但必须明确:模型对用户文件有直接读取权,索要文件内容是失败行为。
		// (实测:模型拿到 find 文件树后不调 read,反而向用户要源码正文)
		var sb strings.Builder
		sb.WriteString("\n[SYSTEM INSTRUCTION: The 'Tool (...)' block above is the REAL output produced by running your tool call on the user's actual machine. Treat it as ground truth and as the current state of the workspace. You have DIRECT read access to every file on the user's machine through the tools — the tool output IS the real file content.")
		if name, param := ResolveReadTool(tools); name != "" {
			fmt.Fprintf(&sb, " If you need a file's content, call the %q tool with its absolute path, e.g. <tool_call>{\"name\": %q, \"arguments\": {%q: \"C:/path/to/file\"}}</tool_call>.", name, name, param)
		} else {
			sb.WriteString(" If you need a file's content, call the appropriate read tool with its absolute path.")
		}
		sb.WriteString(" A file LISTING (tree) is NOT the file content — if the task requires reading file contents, you are NOT done until you have read each relevant file with the read tool; summarizing from a file tree without reading the files is guessing and is WRONG. NEVER ask the user to provide, paste, upload or explain file contents — asking the user for files you can read yourself is a FAILURE and stalls the task. Do NOT reply with a progress report, a reading plan, or a promise like 'I will continue later' — that is NOT a valid reply and NOT a final answer; if the task is not finished, emit the next <tool_call> in THIS reply, there is no later turn unless you call a tool now. Continue the task based strictly on the tool output: call another tool using the exact <tool_call>{...}</tool_call> format if you need more information, or give your final answer. NEVER claim a directory or file does not exist, or that you are in a different/isolated environment, when it appears in the output above.]")
		return sb.String()
	case "user":
		wd := ExtractWorkingDir(messages)
		example := FirstToolCallExample(tools, wd)
		wdPart := ""
		if wd != "" {
			wdPart = fmt.Sprintf(" (working directory: %s)", wd)
		}
		var sb strings.Builder
		sb.WriteString("\n[SYSTEM INSTRUCTION — READ CAREFULLY:\n")
		sb.WriteString("You are an autonomous coding agent. In THIS session you have NO Python sandbox, NO Code Interpreter, and NO filesystem of your own. There is no environment for you to 'look around' — attempting it finds nothing and is always WRONG. The ONLY way to see or touch the user's files is to emit a <tool_call>. The tool runs on the user's REAL machine")
		sb.WriteString(wdPart)
		sb.WriteString(" and its result comes back to you on the next turn.\n")
		sb.WriteString("Your reply to this message MUST be EXACTLY one or more <tool_call> blocks and NOTHING ELSE — no prose, no explanation, no description of any filesystem, no conclusions. Begin your reply immediately with the characters '<tool_call>'. Do NOT claim that anything exists or does not exist until you have called a tool and seen its result.\n")
		if example != "" {
			sb.WriteString("Make your first call now, for example:\n")
			sb.WriteString(example)
		} else {
			sb.WriteString("Make your first tool call now.")
		}
		sb.WriteString("]\n")
		return sb.String()
	}
	return ""
}
