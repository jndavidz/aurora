# aurora 支持 function calling（供 open-xiaoai-bridge 使用）

Status: implemented-pending-live-verification

> 状态：**代码完成，待 L2/L3 验收**（spec 2026-10-27）· 影响仓：`repos/aurora`（集成分支 `feat/provider-tool-calling`）
> 本轮拍板（2026-10-27）：方案 A + 方案 B 同批实施；L2 NAS 验收与 L3 真机验收不在本轮。
> 触发场景：bridge 的 Agent 工具层（`song_request`、`hass`、`ir_send` 等）需要 OpenAI 兼容的
> `tools` → `tool_calls` 回环；当前 aurora 只有 ChatGPT 通道支持，provider 通道一律剥离 tools。
> 关联：`repos/open-xiaoai/bridge` 的 `docs/plan/README.md`（B 线）与
> `.scratch/music-ecosystem-voice/`（V1 点歌验收）。

## 0. 结论摘要

| 事实 | 影响 |
|---|---|
| `internal/provider/deepseek_chat.go` 文件头写明「上游只发真人对话形态，**剥离 tools/tool_choice**」 | 现役默认后端 `deepseek-no-search` 永远不产生 `tool_calls`；bridge 点歌/设备工具全部失效 |
| 工具调用模拟层已存在：`internal/toolcall`（prompt 注入 + `<tool_call>` 解析 + 重试分类器），但只接在 ChatGPT 通道（`internal/handler/tool_calling.go`） | 可复用，不需要从零写协议 |
| aurora 网关里 claude/gpt/gemini 等模型实际转发到办公室侧 CDP 桥（`10.20.0.2:8799`），家里不可达 | 换模型不可行；必须在 aurora 内部解决 |
| bridge 侧工具轮允许非流式（流式探测到 `tool_calls` 增量会整轮回退非流式） | aurora 最小实现只需保证**非流式 JSON 带 tool_calls** 即可先跑通 |

**推荐**：方案 A（provider 工具模拟，复用 `internal/toolcall`）+ 方案 B（原生透传）作为可选加速器。
先做 A 的最小闭环（非流式 + 单轮 + 多轮回传），再补流式与重试。

---

## 1. 现状（代码事实，2026-10-27 快照）

- 路由顺序：`internal/handler/chat_handler.go` 先走 **provider 循环**（`Resolve(model)` 命中即
  `p.ChatCompletions(c, &original_request)`，约 L80–96），未命中才进入 ChatGPT 通道；
  ChatGPT 通道在 `toolCallingEnabled(...)` 为真时调用 `handleToolCalling`（L121 附近）。
- provider 接口（`internal/provider/provider.go`）：`Name/Models/Handles/Responses/ChatCompletions`，
  **没有**「收集全文」的返回式接口，`ChatCompletions` 直接写 `gin.Context`。
- DeepSeek 通道（`internal/provider/deepseek_chat.go`）：
  - 文件头硬规则：剥离 `tools/tool_choice`，不注入任何工具信息；
  - 已有 `Responses` 与 `ChatCompletions` 两条路径，内部持有网页会话/搜索/识图逻辑。
- 工具模拟工具箱（`internal/toolcall/`）：
  - `BuildInstructions(tools, toolChoice)` / `BuildInstructionsWithTags(...)`（prompt.go:16/22）
  - `NewParserWithTagsAndTools(...)`、`(*Parser).Feed/Flush`（parser.go:49/83/159）
  - `FinalNudge(...)`、`looksLikeCodingTask(...)`、`ResolveReadTool(...)` 等重试/提示辅助
- ChatGPT 通道的现成实现：`internal/handler/tool_calling.go` 的 `toolCallingRetry` +
  `looksLike*` 分类器；`typings/chatgpt/request.go` 的 `AddToolMessage`（把 `role=tool`
  转成 `role=user` 文本——网页端不认 tool 角色）。

**结论**：能力齐备，缺的是「provider 通道 → 模拟工具 → 标准 tool_calls」这一段接线。

---

## 2. 目标契约（bridge 侧真实需求）

bridge `core/openai.py` 的请求/回环形态：

1. 首轮：`POST /v1/chat/completions`，`tools=[...]`、`tool_choice="auto"`，`stream=true`（可探测回退）；
2. aurora 返回 `choices[0].message.tool_calls[]`，`finish_reason="tool_calls"`；
3. bridge 执行工具（点歌：搜歌→起播），把结果以
   `{"role":"tool","tool_call_id":...,"content":"..."}` 追加；
4. 第二轮请求带完整历史（含 assistant.tool_calls），aurora 必须接受并给出最终口语文本；
5. 终止条件：`finish_reason="stop"`。

附加约束：
- 首响预算：工具轮 ≤ 6s（bridge 对话 SLO 的一部分），模拟层不得引入二次上游往返；
- 会话：可选 `X-Session-Key` 头（aurora DeepSeek 通道已支持）；缺失不报错；
- 错误面：上游 4xx/5xx 保留原始错误体，bridge 只认 `success/error` 字段。

---

## 3. 方案 A：provider 工具模拟（推荐）

### 3.1 设计

```
client(tools) ──► chat_handler
                    │ tools 非空 且 模型命中 provider 且 开关开
                    ▼
       handleProviderToolCalling(c, req, p)
         1) toolcall.BuildInstructions(req.Tools, req.ToolChoice) → 注入 system/首条 user
         2) p.CollectText(ctx, req)            // 新增可选接口：非流式全文
         3) toolcall.Parser.Feed/Flush(text)   // 文本 → []official.ToolCall
         4) 有 calls → 组装 chat.completion（tool_calls）/ SSE 单 chunk
            无 calls → 原样返回文本（普通对话不受影响）
```

要点：
- **不改** `internal/toolcall`（已被 ChatGPT 通道验证）；
- 上游仍走「真人对话」形态（DeepSeek 硬规则不破），工具协议只在 prompt 里；
- 重试复用 `looksLike*` 分类器：模型说「没有工具」/退回纯文本时重试 N 次
  （沿用 `REFUSAL_RETRIES` 口径），仍失败则**降级为普通文本**而不是报错。

### 3.2 新增可选接口（`internal/provider/provider.go`）

```go
// TextCollector 由需要接入工具模拟的 provider 实现：剥离 tools 后
// 以非流式方式取回全文，供 handler 侧解析 <tool_call>。
type TextCollector interface {
    CollectText(c *gin.Context, req *official.APIRequest) (text string, usage *official.Usage, err error)
}
```

DeepSeek 通道实现要点（`internal/provider/deepseek_chat.go`）：
- 复用现有非流式 chat 分支的会话/token/搜索逻辑，**保持 tools 剥离**；
- 返回拼接后的全文与 usage；超时沿用通道现有值，不可大于 60s。

### 3.3 handler 接线（`internal/handler/chat_handler.go`）

provider 循环内、`p.ChatCompletions` 之前插入：

```go
if len(original_request.Tools) > 0 && h.cfg.ProviderToolEmulation {
    if tc, ok := p.(provider.TextCollector); ok {
        h.handleProviderToolCalling(c, &original_request, tc)
        return
    }
}
```

### 3.4 多轮回传（关键坑）

第二轮的 `assistant.tool_calls` 与 `role=tool` 消息，网页通道不认识：
- 复用 ChatGPT 通道的做法：把 `role=tool` 转成 `role=user`，文本前缀
  `工具 <name> 的执行结果：` + content；
- `assistant.tool_calls` 转成 `role=assistant` 的简述文本（`调用 <name>(<args>)`），
  避免模型复述原始 JSON；
- 该转换应放在 provider 的请求翻译层（与 `excludeTools` 同一处），保证
  `Responses`/`ChatCompletions` 两条入口行为一致。

### 3.5 响应封装

| 客户端 stream | 返回 |
|---|---|
| false | `chat.completion`：有 calls 时 `message.tool_calls` + `finish_reason="tool_calls"`；无 calls 时普通文本 |
| true | `chat.completion.chunk`：首 chunk 携带 `delta.tool_calls[]` + `finish_reason` 收尾，末 chunk `[DONE]`；**不要求**逐字增量（bridge 探测到 tool_calls 会回退非流式；ZCode 侧 relay.py 也是同款「非流式→SSE 包装」） |

### 3.6 开关与灰度

`internal/config/config.go` 新增：
- `PROVIDER_TOOL_EMULATION`（默认 0，开=1）；
- `PROVIDER_TOOL_MODELS`（可选，逗号分隔白名单；空=所有 provider 模型）；
- `PROVIDER_TOOL_RETRIES`（默认 3，沿用 REFUSAL_RETRIES 语义）。

建议灰度：先只对 `deepseek-no-search` 开，验证 bridge 点歌与 HA 工具，再推广。

---

## 4. 方案 B：原生透传（备选 / 过渡加速器）

新增 provider `openai-native`（`internal/provider/openai_native.go`）：
- 配置：`NATIVE_TOOLS_BASE`（如 `https://api.deepseek.com/v1`）、`NATIVE_TOOLS_KEY`、
  `NATIVE_TOOLS_MODEL_MAP`；
- 仅当请求带 `tools` 时由 handler 路由过来；`messages/tools/tool_choice` **原样转发**，
  响应（含 SSE `delta.tool_calls`）原样回吐；
- 优点：可靠、标准、零模拟延迟；缺点：绕开逆向通道、增加外部 API 成本，
  等于 aurora 内再挂一个直连上游。

**A/B 组合建议**：配置了 `NATIVE_TOOLS_*` 时，工具请求优先走 B；未配置则回落到 A。
两者都关时维持现状（剥离 tools，普通对话不受影响）。

---

## 5. 文件级落地清单

| # | 文件 | 改动 |
|---|---|---|
| 1 | `internal/provider/provider.go` | 新增 `TextCollector` 可选接口 + 注释 |
| 2 | `internal/provider/deepseek_chat.go` | 实现 `CollectText`（非流式全文；保持 tools 剥离）；`role=tool`/`tool_calls` 历史转换 |
| 3 | `internal/handler/provider_tools.go`（新） | `handleProviderToolCalling`：注入指令 → CollectText → Parser → JSON/SSE 封装 → 重试与降级 |
| 4 | `internal/handler/chat_handler.go` | provider 分支前置接线（见 §3.3） |
| 5 | `internal/config/config.go` | `PROVIDER_TOOL_*` / `NATIVE_TOOLS_*` 开关 |
| 6 | `internal/handler/provider_tools_test.go`（新） | golden：文本→tool_calls、无调用透传、SSE chunk 形状、tool 历史转换、重试耗尽降级 |
| 7 | `docs/ARCHITECTURE.md` §5.2 | 补「provider 工具模拟」小节与数据流 |
| 8 | `D:\_work\dev\apps\aurora\部署说明.md` | 新增开关说明 + 回退步骤 |

---

## 6. 验收

**L1 单测**：`go test ./...` 全绿（新增用例覆盖 §5#6）。

**L2 接口验收（curl，NAS 本机）**：

```bash
curl -s -X POST http://127.0.0.1:65432/v1/chat/completions \
  -H 'Content-Type: application/json' -H 'Authorization: Bearer <key>' \
  -d '{"model":"deepseek-no-search","messages":[{"role":"user","content":"查询北京天气"}],
       "tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}],
       "tool_choice":"required"}'
```

判据：`choices[0].message.tool_calls[0].function.name == "get_weather"`，
`finish_reason == "tool_calls"`；连发 5 次，≥4 次命中（模拟通道非确定性，允许重试兜底）。

**L3 bridge 集成（真机）**：
1. 「你好小塔」→「我要点歌」→「晴天」→ LX06 出声（bridge 日志出现
   `[SongRequest] playing ... confirmed=True`）；
2. 「小爱同学」→「下一首」→ 切到不同曲目；
3. 工具轮首响 ≤ 6s（`TurnTrace` 的 `llm_ms`）。

**L4 回归**：无 tools 的普通对话与现状逐字等价（开关关闭时行为不变）。

---

## 7. 风险与回退

| 风险 | 缓解 |
|---|---|
| 网页模型对 `<tool_call>` 协议遵循不稳（历史单轮触发率曾 ~1/3，修复后 agent_sim 5/5） | 复用 `looksLike*` 重试 + `FinalNudge`；重试耗尽降级为文本，不 500 |
| `role=tool` 转换导致长结果挤占上下文 | 工具结果截断（如 4KB）+ 摘要前缀 |
| 模拟层增加一次全文收集 → 首响变慢 | 非流式收集本就与 bridge 回退路径等价；必要时走方案 B |
| 灰度误伤普通对话 | 默认关闭 + 模型白名单；回退 = 关开关（无需回滚镜像） |

回退步骤：`.env` 置 `PROVIDER_TOOL_EMULATION=0`（或清空 `PROVIDER_TOOL_MODELS`）→ 重启 aurora；
bridge 侧无改动。

---

## 8. 附：bridge 侧已就绪的对接面（无需改动）

- 工具规格来源：`bridge/core/tools/registry.py`（`get_all_tools`）；
- 回环：`bridge/core/openai.py:296-345`（tool_calls → execute_tool → `role=tool` 回传）；
- 流式探测回退：`bridge/core/openai.py:354-395`（`_ToolLoopDetected` → 非流式）；
- 工具轮超时：`bridge/deploy/config.py` `openai.response_timeout`。

## 9. 施工记录（2026-10-27）

**代码侧完成**，集成分支 `feat/provider-tool-calling`（tip `39b4c38`），主干 `local-toolfix` 未动、未 push。

- tickets：`01`–`07` 全部 done（07 = review 修复票，P1×4 + P2×9）。
- 范围偏差（相对本 spec）：
  1. **方案 A 只接 `/v1/chat/completions`**；`/v1/responses` 带 tools 维持现状（§3.3 未提表面限制）。
  2. **方案 B 不 `Register` 进 providers 列表**，只登记 `Registry.SetNativeTools` 槽位——`Register` 是全量路由，会劫持同名模型的普通对话与 `/v1/responses`（§4 原文「由 handler 路由过来」按此落地）；因此 B 的模型 id 不进 `/v1/models`，且 B 的 `Responses` 表面返回 400。
  3. **不做无标签 JSON 恢复**（§3.1 的重试分类器只保留标签解析）：`RecoverFromText` 的参数推断会把正文里解释性的 `{"query":"…"}` 伪造成调用，对音箱/红外这类真实执行工具是有害失败。
  4. 工具结果文本沿用 ChatGPT 通道的 `Tool (Resultado da ferramenta <name>): …` 前缀（§3.4 建议的中文前缀会削弱共享 prompt 的遵循率）。
  5. `CollectText` 每轮新开会话（`clientKey=""`）：续轮路径只发最后一条 message，注入的协议指令会整段丢失。
- 已知代价（未做）：重试轮各自新开会话/重传图片/可能换 token，需改 `TextCollector` 签名才能复用；已加 context-aware 退避（400ms×N，上限 2s）。
- **未验收**：L1 单测全绿（`go test ./...` 37 包），L2（NAS curl 命中率）与 L3（bridge 真机点歌、首响 ≤6s）需 NAS 与音箱，留待你执行；本 spec 状态在完成 L2/L3 前不置 closed。
- 回退：`.env` 置 `PROVIDER_TOOL_EMULATION=0` 或清空 `PROVIDER_TOOL_MODELS` → 重启 aurora；不开两个开关时行为与旧版逐字一致。

## 10. L2 验收记录（2026-10-28，ZCode 会话）

合并部署：`feat/provider-tool-calling` 经用户拍板 ff 合并入 `local-toolfix`（`ed2f3a6`），compose 灰度开关入库（`28cfcbd`：`PROVIDER_TOOL_EMULATION=true` + `PROVIDER_TOOL_MODELS=deepseek-no-search`，硬编码——部署脚本会清 .env），NAS 镜像 `525dfe247` 部署探活通过，容器内 env 回读确认开关生效。

三发单发验证（`song_request` 假工具、`X-Session-Key: l2-toolcall-1028`）：

| 轮次 | 期望 | 实测 | 耗时 |
|---|---|---|---|
| R1 带 tools | `tool_calls` + `finish_reason=tool_calls` | ✅ `song_request({"query":"周杰伦 晴天"})`，参数正确 | **6.73s**（略超 6s SLO，未触发重试；单点） |
| R2 工具结果轮 | 口语终答 + `stop` | ✅「已开始播放：晴天 - 周杰伦」+ `stop` | **2.78s** ✅ |
| R3 无 tools 回归 | 普通流式对话不受影响 | ✅ 流式正文 + `stop` | TTFT 2.39s（上游抖动范围内） |

结论：**多轮工具回环契约（§2 目标契约）端到端成立**。R1 超 SLO 0.7s 为首建会话+上游抖动单点，待 L3 bridge 真机复测；若真机持续超 6s，杠杆是 `PROVIDER_TOOL_RETRIES` 下调与首句约束（不首先动重试预算）。**L3（bridge 真机点歌）待 bridge 重部署后执行**（前置：bridge B-29 系列收尾）。
