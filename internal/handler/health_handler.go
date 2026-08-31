package handler

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// CredentialHealth 处理 GET /v1/health/credentials(鉴权组内,内网运维用)。
//
// 汇总两类凭证健康:
//   - chatgptPool:ChatGPT 兜底账号池(三池 + 临时账号,access JWT exp)
//   - providers:各 provider 凭证池(GLM/Kimi 报告 refresh_token ~90 天到期线;
//     会话级通道报 unmanaged,有效期静态参考 docs/CREDENTIALS.md)
//
// 用途:配合外部探测(NUC credential-keeper)实现"到期前告警/重抓"。
func (h *ChatHandler) CredentialHealth(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"checkedAt":   time.Now().UTC().Format(time.RFC3339),
		"chatgptPool": h.accountPool.CredentialHealth(),
		"providers":   h.providers.CredentialHealthReport(),
	})
}
