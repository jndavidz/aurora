package doubaoweb

import (
	"os"
	"strings"
	"testing"
	"time"
)

// 账号池加载 + 轮询。
func TestAccountPool(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/accts.json"
	content := `[{"cookie":"c1","aid":"497858","device_id":"d1","fp":"f1","ms_token":"m1","a_bogus":"a1","web_id":"w1"},{"cookie":"c2","aid":"497858","device_id":"d2","fp":"f2","ms_token":"m2","a_bogus":"a2","web_id":"w2"}]`
	if err := writeFile(path, content); err != nil {
		t.Fatal(err)
	}
	accts, err := LoadAccounts(path)
	if err != nil || len(accts) != 2 {
		t.Fatalf("load: %v %d", err, len(accts))
	}
	c := NewClient(accts)
	if !c.HasAccount() {
		t.Fatal("HasAccount false")
	}
	// 多账号:两次调用选不同账号(轮询)
	a1, err := c.nextAccount()
	if err != nil {
		t.Fatal(err)
	}
	a2, err := c.nextAccount()
	if err != nil {
		t.Fatal(err)
	}
	if a1 == a2 {
		t.Error("rotation should pick different account")
	}
}

// 限频:单账号连续两次调用,第二次应等待 minInterval。
func TestRateLimit(t *testing.T) {
	acct := &Account{Cookie: "c", Aid: "a", DeviceID: "d", FP: "f"}
	c := NewClient([]*Account{acct})
	start := time.Now()
	if _, err := c.nextAccount(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := c.nextAccount(); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < minInterval {
		t.Errorf("rate limit not enforced: %v", time.Since(start))
	}
}

// buildReqBody:含 prompt + conversation 续接。
func TestBuildReqBody(t *testing.T) {
	acct := &Account{Aid: "497858", DeviceID: "d", FP: "f", MsToken: "m", ABogus: "a", WebID: "w", TeaUUID: "t", WebTabID: "tab"}
	c := NewClient([]*Account{acct})
	body, err := c.buildReqBody(acct, CompletionRequest{Prompt: "你好", ConvID: "conv1", SectionID: "sec1", MsgIndex: 3})
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	for _, want := range []string{"你好", "conv1", "sec1", "7338286299411103781", "need_create_conversation"} {
		if !strings.Contains(s, want) {
			t.Errorf("body missing %q: %s", want, s)
		}
	}
	// 豆包不创建新会话(need_create_conversation 恒 false)
	if !strings.Contains(s, `"need_create_conversation":false`) {
		t.Errorf("need_create_conversation should be false: %s", s)
	}
}

// completionURL:含全部参数。
func TestCompletionURL(t *testing.T) {
	acct := &Account{Aid: "497858", DeviceID: "d", FP: "f", MsToken: "m", ABogus: "a", WebID: "w", TeaUUID: "t", WebTabID: "tab"}
	c := NewClient([]*Account{acct})
	u := c.completionURL(acct)
	for _, want := range []string{"aid=497858", "device_id=d", "fp=f", "msToken=m", "a_bogus=a", "web_id=w"} {
		if !strings.Contains(u, want) {
			t.Errorf("url missing %q: %s", want, u)
		}
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
