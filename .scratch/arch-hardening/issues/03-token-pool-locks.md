# WP3 token 池补锁

Status: done (7df3b7e)
Blocked by: 无

Spec: `../spec.md`（本 ticket 对应其中 WP3 token 池补锁 的 Implementation Decisions）

## 验收标准

- 三家 NextToken + GLM 换发加锁，-race 通过
- `go build` + `go test ./...` 通过（live 用例缺环境自动跳过）

## Comments

- 2026-10-26 完成:三家 NextToken/SetRefreshToken(SetCookieHeader)/RefreshTokenExps/PoolSize/HasToken 加锁,qianwen Complete 改 cookie 快照,GLM ensureToken 加 tokMu。WSL go1.27 -race 五包全绿(Windows 侧无 CGO 工具链,-race 走 WSL)。
