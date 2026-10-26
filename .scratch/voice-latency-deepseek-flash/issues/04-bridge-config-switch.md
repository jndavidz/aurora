# 04 — bridge 生产配置切到 aurora + 流式（远程）

**What to build:** 修改 NAS 上挂载进 bridge 容器的生产配置，把 openai 段从 DeepSeek 官方 API 切到 aurora 网关并启用流式；bridge 代码零改动。

**前置授权：** 远程状态变更，需用户单独批准。执行前先做只读审计并记录：`tts_provider`、工具注册表状态、当前 `max_tokens`/`history_max_messages`/`session_header`。

**配置键（`openai` 段）：**
| 键 | 值 |
|---|---|
| `base_url` | aurora（NAS 本机 `http://127.0.0.1:65432/v1` 或容器可达地址） |
| `api_key` | aurora 鉴权值（从 aurora 侧环境读取，不回显、不入库） |
| `model` | `deepseek-v4-flash` |
| `stream` | `true`（代码键名 `openai.stream`，默认 false） |
| `session_header` | `X-Session-Key`（默认 `X-Hermes-Session-Key` aurora 不认） |
| `tool_calls_enabled` | `false`（命令表命中控制设备，不需要 LLM 工具；同时避免探测式流式） |
| `max_tokens` | `220`（若出现截断先降到 `rule_prompt` 150 字，不动 stream） |
| `history_max_messages` | `10` |
| `system_prompt` | 追加「先用一句不超过 15 字的话给出结论，再展开」 |

**步骤：** 备份生产配置文件（时间戳）→ 改上述键 → 重启 bridge 容器 → 回读启动日志无报错、容器 `Up`。

**验收：** 配置回读与上表一致；bridge 日志出现指向 aurora 的请求且无 4xx/5xx；`X-Session-Key` 头能在 aurora 侧观测到（debug 日志或临时抓包，不落凭证）。

**回退：** 恢复备份 + 重启容器（= 回官方 API 三项 base_url/model/stream，其余参数可保留）。

**Status:** pending

**Blocked by:** 03
