package util

import (
	"fmt"
	"math/rand"
	"regexp"
	"strings"
	"sync"
	"time"
)

const FixedUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36"

// userAgentSpec 描述一个主流桌面浏览器的 User-Agent 模板
// 模板中可使用 %d 作为版本占位符
type userAgentSpec struct {
	Template   string
	MinVersion int
	MaxVersion int // 闭区间上界
	Family     string
}

// 保留 RandomUserAgent 用于其它场景(测试、临时抓包等)。生产路径走 FixedUserAgent。
var userAgentSpecs = []userAgentSpec{
	{
		Template:   "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.0.0 Safari/537.36",
		MinVersion: 148,
		MaxVersion: 148,
		Family:     "Chrome-Win",
	},
}

var (
	uaRand     *rand.Rand
	uaRandOnce sync.Once
)

func initUARand() {
	uaRandOnce.Do(func() {
		uaRand = rand.New(rand.NewSource(time.Now().UnixNano()))
	})
}

// RandomUserAgent 返回一个随机的主流桌面浏览器 User-Agent
func RandomUserAgent() string {
	initUARand()
	spec := userAgentSpecs[uaRand.Intn(len(userAgentSpecs))]

	version := spec.MinVersion
	if spec.MaxVersion > spec.MinVersion {
		version += uaRand.Intn(spec.MaxVersion - spec.MinVersion + 1)
	}

	// 数一下模板里 %d 的个数,按个数填充 version
	placeholders := strings.Count(spec.Template, "%d")
	switch placeholders {
	case 1:
		return fmt.Sprintf(spec.Template, version)
	default:
		// 0 个或 2+ 个(兼容老 Edge 模板): 都用 version 填充
		return fmt.Sprintf(strings.Replace(spec.Template, "%d", "%v", -1), version)
	}
}

// chromeMajorRe 从 User-Agent 提取 Chrome 主版本号(如 "Chrome/148.0.0.0" → "148")。
var chromeMajorRe = regexp.MustCompile(`Chrome/(\d+)`)

// ChromeMajorFromUA 提取 UA 中的 Chrome 主版本号;解析失败返回空串。
func ChromeMajorFromUA(ua string) string {
	m := chromeMajorRe.FindStringSubmatch(ua)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// SecChUaForUA 根据 User-Agent 生成一致的 Sec-Ch-Ua 头。
// C2(2026-09-05):sec-ch-ua 版本必须与 UA 的 Chrome 版本一致(不一致是经典
// bot 检测信号),此前 builder.go 硬编码 v="148",UA 升级时两者会脱节。
// UA 解析失败时回退 FixedUserAgent 的版本,再失败回退当前默认 148。
func SecChUaForUA(ua string) string {
	v := ChromeMajorFromUA(ua)
	if v == "" {
		v = ChromeMajorFromUA(FixedUserAgent)
	}
	if v == "" {
		v = "148"
	}
	return `"Chromium";v="` + v + `", "Google Chrome";v="` + v + `", "Not/A)Brand";v="99"`
}
