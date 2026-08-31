// Package factory 是网页逆向通道 HTTP 客户端的统一构造入口(路线图 B1)。
//
// 目标:TLS 档位、代理、超时一个入口收口 —— 阶段 C 的反封改造
// (5 家 Go 原生客户端升级 TLS 伪装)从"改 10 处分散实现"变成"改 1 处"。
//
// 关键约束:调用方只换构造处,请求代码零改动(B2)。为此两种模式都实现
// 同一 Client 接口 —— TLS 模式把 net/http.Request 拆解后委托
// bogdanfinn 包装器的 Request(输出已是 net/http.Response),native 模式
// 直接委托 http.Client.Do。
//
// 灰度回滚(路线图 B3):环境变量 AURORA_LEGACY_IDENTITY=1 时,标记为
// Upgradable 的通道强制回退 Go 原生。一直走 TLS 伪装的通道
// (gemini/qianwen/minimax/mimo/yuanbao)不受开关影响 —— 它们没有
// "改造前"的 Go 原生状态,回退反而会坏。
package factory

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"aurora/httpclient"
	"aurora/httpclient/bogdanfinn"
)

// Mode 出站传输模式。
type Mode int

const (
	ModeTLSFaked Mode = iota // bogdanfinn TLS 指纹伪装(JA3 对齐浏览器)
	ModeGoNative             // Go 原生 net/http(legacy/回滚态)
)

// Profile 描述一个通道的出站身份。
type Profile struct {
	Mode Mode
	// Upgradable 标记"C1 将从 GoNative 升级为 TLSFaked"的通道;
	// AURORA_LEGACY_IDENTITY=1 仅对这类通道强制回退。
	Upgradable bool
	ProxyURL   string        // 构造期代理(native 模式克隆 DefaultTransport 后注入)
	Timeout    time.Duration // 仅 native 模式生效;0 = 无超时(流式必需)
}

// LegacyIdentity 报告灰度回滚开关(AURORA_LEGACY_IDENTITY=1)。
func LegacyIdentity() bool { return os.Getenv("AURORA_LEGACY_IDENTITY") == "1" }

// Client 统一两种模式的出站接口。
// Request 是 TLS 通道既有用法(AuroraHttpClient 形状);Do 是 native 通道
// 既有用法 —— 调用方的请求代码在切换构造处后无需任何改动。
type Client interface {
	Request(method httpclient.HttpMethod, url string, headers httpclient.AuroraHeaders, cookies []*http.Cookie, body io.Reader) (*http.Response, error)
	Do(req *http.Request) (*http.Response, error)
	SetProxy(rawURL string) error
	CloseIdleConnections()
}

// NewWebClient 按 profile 构造通道客户端。
func NewWebClient(p Profile) Client {
	if p.Mode == ModeTLSFaked && p.Upgradable && LegacyIdentity() {
		p.Mode = ModeGoNative
	}
	if p.Mode == ModeGoNative {
		return newNative(p)
	}
	return &tlsClient{inner: bogdanfinn.NewStdClient()}
}

// ── TLS 模式(包装既有 bogdanfinn 包装器)─────────────────────────

type tlsClient struct{ inner *bogdanfinn.TlsClient }

func (t *tlsClient) Request(method httpclient.HttpMethod, rawURL string, headers httpclient.AuroraHeaders, cookies []*http.Cookie, body io.Reader) (*http.Response, error) {
	return t.inner.Request(method, rawURL, headers, cookies, body)
}

func (t *tlsClient) SetProxy(rawURL string) error { return t.inner.SetProxy(rawURL) }

// CloseIdleConnections 委托底层 TLS 客户端(若其支持;不支持则忽略)。
func (t *tlsClient) CloseIdleConnections() {
	if c, ok := t.inner.Client.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// Do 把调用方的 net/http.Request 拆解为 Request 的部件参数。
// 头部多值合并为逗号列表(HTTP 列表型头的规范形态);Cookie 头单独走
// cookies 参数(避免与 Cookie jar 双写)。
func (t *tlsClient) Do(req *http.Request) (*http.Response, error) {
	hdrs := httpclient.AuroraHeaders{}
	for k, vs := range req.Header {
		if strings.EqualFold(k, "Cookie") || len(vs) == 0 {
			continue
		}
		hdrs.Set(k, strings.Join(vs, ", "))
	}
	if req.Host != "" {
		hdrs.Set("Host", req.Host)
	}
	return t.inner.Request(httpclient.HttpMethod(req.Method), req.URL.String(), hdrs, req.Cookies(), req.Body)
}

// ── native 模式(包装 net/http.Client)───────────────────────────

type nativeClient struct{ inner *http.Client }

func newNative(p Profile) *nativeClient {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if p.ProxyURL != "" {
		if parsed, err := url.Parse(normalizeProxy(p.ProxyURL)); err == nil {
			tr.Proxy = http.ProxyURL(parsed)
		}
	}
	return &nativeClient{inner: &http.Client{Transport: tr, Timeout: p.Timeout}}
}

func (n *nativeClient) Do(req *http.Request) (*http.Response, error) { return n.inner.Do(req) }

// Request 按 AuroraHttpClient 形状拼装请求后走 Do(供调用方将来无缝切 TLS 模式)。
func (n *nativeClient) Request(method httpclient.HttpMethod, rawURL string, headers httpclient.AuroraHeaders, cookies []*http.Cookie, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(string(method), rawURL, body)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	return n.inner.Do(req)
}

func (n *nativeClient) SetProxy(rawURL string) error {
	if tr, ok := n.inner.Transport.(*http.Transport); ok {
		if parsed, err := url.Parse(normalizeProxy(rawURL)); err == nil {
			tr.Proxy = http.ProxyURL(parsed)
		}
	}
	return nil
}

func (n *nativeClient) CloseIdleConnections() { n.inner.CloseIdleConnections() }

// normalizeProxy 无 scheme 时补 http://(对齐 deepseekweb.setTransportProxy 语义)。
func normalizeProxy(rawURL string) string {
	if !strings.Contains(rawURL, "://") {
		return "http://" + rawURL
	}
	return rawURL
}
