package factory

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aurora/httpclient"
)

// 回归:B3 —— 灰度回滚开关只影响 Upgradable 通道:
//   - Upgradable + env=1 → 强制 Go 原生
//   - 非 Upgradable(一直走 TLS 的通道)不受开关影响
func TestLegacyIdentitySwitch(t *testing.T) {
	t.Setenv("AURORA_LEGACY_IDENTITY", "1")

	up := NewWebClient(Profile{Mode: ModeTLSFaked, Upgradable: true})
	if _, ok := up.(*nativeClient); !ok {
		t.Fatalf("Upgradable 通道在 legacy 开关下应为 native, got %T", up)
	}

	fixed := NewWebClient(Profile{Mode: ModeTLSFaked, Upgradable: false})
	if _, ok := fixed.(*tlsClient); !ok {
		t.Fatalf("一直走 TLS 的通道不应受 legacy 开关影响, got %T", fixed)
	}

	native := NewWebClient(Profile{Mode: ModeGoNative, Upgradable: true})
	if _, ok := native.(*nativeClient); !ok {
		t.Fatalf("native 模式不受开关影响, got %T", native)
	}
}

// 回归:B2 —— native 模式下调用方的 Do 代码零改动(经工厂转发)。
func TestNativeDoPassthrough(t *testing.T) {
	var gotPath, gotUA, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotUA = r.Header.Get("User-Agent")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := NewWebClient(Profile{Mode: ModeGoNative})
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/test", strings.NewReader(`{"q":1}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", "UA-Test")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || gotPath != "/v1/test" || gotUA != "UA-Test" || gotBody != `{"q":1}` {
		t.Fatalf("status=%d path=%q ua=%q body=%q", resp.StatusCode, gotPath, gotUA, gotBody)
	}
}

// 回归:B2 —— native 模式下 AuroraHttpClient 形状的 Request 也零改动可用
// (为调用方将来无缝切 TLS 模式兜底)。
func TestNativeRequestPassthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test") != "v1" {
			w.WriteHeader(400)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := NewWebClient(Profile{Mode: ModeGoNative})
	hdrs := httpclient.AuroraHeaders{"X-Test": "v1"}
	resp, err := c.Request(http.MethodGet, srv.URL+"/x", hdrs, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

// 回归:B3 —— legacy 开关下 native 的 ProxyURL 仍应生效(构造期代理不回滚)。
func TestNativeProxyStillWired(t *testing.T) {
	c := NewWebClient(Profile{Mode: ModeGoNative, Upgradable: true, ProxyURL: "127.0.0.1:1"})
	n, ok := c.(*nativeClient)
	if !ok {
		t.Fatalf("want native, got %T", c)
	}
	tr, ok := n.inner.Transport.(*http.Transport)
	if !ok || tr.Proxy == nil {
		t.Fatal("ProxyURL 应已注入 transport")
	}
}
