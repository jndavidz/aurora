# WP2 ChatGPT 兜底 turnLoop 合并

Status: done (eb2728a)
Blocked by: 无

Spec: `../spec.md` WP2。三份 continue 轮询循环合为 chatgptTurnLoop（轮级回调输出）；sentinel ping 参数化（行为保全：仅 chat/completions 发）；新增 fake conversationClientOrder seam 测试；入口前奏合并。分「抽循环」「前奏合并」两步提交。

## 验收标准

- 三入口输出与合并前逐字节一致（时间戳等非确定字段除外）
- 兜底路径首个自动化测试通过
- ping 行为参数化且现状不变

## Comments

- 2026-10-26 完成:chatgptTurnLoop 单点(continue 链/累积/TTFT/sentinel/记账/ping 参数化 keepAlivePing);错误分类 turnSendFailed/turnHandledError 交三入口按表面转输出;onTurn 轮级回调接 emitter;共享前奏 prepareChatGPTTurn(~60行×2 合一)。sendTurn 注入 seam 测试 4 例=兜底首个自动化测试。continue 链触发需私有 v2 事件格式,重排逻辑由逐行 diff 审查保全(spec 偏离已记录)。行为微差(记录于 commit):Nightmare TTFT 基准提前至发起首轮/RecordCache 前移失败也记账/CapResponses 挪至前奏后。
