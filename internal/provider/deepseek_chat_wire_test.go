package provider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"aurora/internal/deepseekweb"
	"aurora/internal/provider/session"
	"aurora/typings/official"
)

// ── wire 级回环测试:flow.run + session.Pool + wire fake 上游 ──
//
// 真实 DeepSeek + 真 session.Pool(非 fake pool)+ httptest 假上游:
// 断言第二轮请求体 parent_message_id 非空、prompt 为增量、session 复用
// 不再 create/delete。是「带 X-Session-Key 连续两轮」验收的直接证词。

// wireUpstream 是 httptest 假上游:实现 CreateSession/DeleteSession/Complete,
// 记录全部请求体。PoW 挑战可解(difficulty 小、target 已算好)。
// failCompletions 列出需返回 4xx 的 completion 序号(从 0 起),
// 用于注入续轮失败(ticket 03 降级验收);命中的请求仍记入 completions。
type wireUpstream struct {
	mu              sync.Mutex
	creates         int
	deletes         int
	createsAuth     []string         // 每次 create 收到的 Authorization 头
	completions     []wireCompletion // 按到达顺序
	failCompletions map[int]bool
}

type wireCompletion struct {
	ParentMessageID any // JSON 解码后的原值(nil = 字段缺省/null)
	Prompt          string
	SessionID       string
	Auth            string // Authorization 头(ticket 05 P0 回归:池路径不得空 token)
}

func (w *wireUpstream) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		defer w.mu.Unlock()
		switch r.URL.Path {
		case "/api/v0/chat_session/create":
			w.creates++
			w.createsAuth = append(w.createsAuth, r.Header.Get("Authorization"))
			writeJSONWire(rw, map[string]any{"code": 0, "data": map[string]any{"biz_code": 0, "biz_data": map[string]any{"id": "ws" + strconv.Itoa(w.creates)}}})
		case "/api/v0/chat_session/delete":
			w.deletes++
			writeJSONWire(rw, map[string]any{"code": 0, "data": map[string]any{"biz_code": 0, "biz_data": map[string]any{}}})
		case "/api/v0/chat/create_pow_challenge":
			salt := "wtest"
			target := hexEncode(deepseekweb.HashDeepSeekV1([]byte(salt + "_1700000000_1")))
			writeJSONWire(rw, map[string]any{"code": 0, "data": map[string]any{"biz_code": 0, "biz_data": map[string]any{
				"challenge": map[string]any{"algorithm": "DeepSeekHashV1", "challenge": target, "salt": salt, "signature": "sig", "difficulty": 10, "expire_at": 1700000000, "target_path": "/api/v0/chat/completion"},
			}}})
		case "/api/v0/chat/completion":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			idx := len(w.completions)
			w.completions = append(w.completions, wireCompletion{
				ParentMessageID: body["parent_message_id"],
				Prompt:          strOf(body["prompt"]),
				SessionID:       strOf(body["chat_session_id"]),
				Auth:            r.Header.Get("Authorization"),
			})
			msgID := "m" + strconv.Itoa(idx+1)
			text := "回复" + strconv.Itoa(idx+1)
			if w.failCompletions[idx] {
				// 上游 4xx(会话失效/风控):Complete 侧解析为错误
				// ("./chat/deepseek_chat_session.go 的发送失败分支)。
				writeJSONWire(rw, map[string]any{"code": 40003, "msg": "session invalid", "data": map[string]any{"biz_code": 0, "biz_msg": "", "biz_data": nil}})
				return
			}
			rw.Header().Set("Content-Type", "text/event-stream")
			rw.Write([]byte("data: {\"response_message_id\":\"" + msgID + "\"}\n\n" +
				"data: {\"v\":\"" + text + "\"}\n\n" +
				"data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n" +
				"event: close\n\n"))
		default:
			http.NotFound(rw, r)
		}
	})
}

func strOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// deepseekWireFacade 把 wire fake 接到 flow.run(等价生产接线,client 换成
// 指向 httptest 的 deepseekweb.Client)。
type deepseekWireFacade struct {
	pool   *session.Pool
	client *deepseekweb.Client
}

func startWire(t *testing.T, w *wireUpstream) (*deepseekweb.Client, func()) {
	t.Helper()
	return startWireWithTokenFile(t, w, "")
}

// startWireWithTokenFile 同上,但接一个 token 文件(热加载/轮换场景)。
func startWireWithTokenFile(t *testing.T, w *wireUpstream, tokenFile string) (*deepseekweb.Client, func()) {
	t.Helper()
	srv := httptest.NewServer(w.handler(t))
	c, err := deepseekweb.NewClient(srv.URL, tokenFile, "")
	if err != nil {
		srv.Close()
		t.Fatal(err)
	}
	return c, srv.Close
}

// responsesRequest 构造两轮 Responses input。
func responsesReq(turns ...string) *official.ResponsesAPIRequest {
	items := make([]map[string]any, 0, len(turns))
	for _, s := range turns {
		items = append(items, map[string]any{"type": "message", "role": "user", "content": s})
	}
	raw, _ := json.Marshal(items)
	return &official.ResponsesAPIRequest{Input: raw, Model: "deepseek"}
}

// runWireTurn 走 flow.run 一轮(与 chatResponses 非流式分支同型接线)。
func runWireTurn(t *testing.T, f *deepseekWireFacade, token string, req *official.ResponsesAPIRequest) deepseekTurnOutput {
	t.Helper()
	var turns []deepseekTurn
	for _, it := range responsesInputItems(req.Input) {
		if it.Text != "" {
			turns = append(turns, deepseekTurn{Text: it.Text})
		}
	}
	flow := &deepseekChatFlow{
		pool:      f.pool,
		sender:    &deepseekWebSender{client: f.client},
		clientKey: "sess-1",
		modelType: "default",
		token:     token,
		messages:  turns,
	}
	out, err := flow.run()
	if err != nil {
		t.Fatalf("flow.run: %v", err)
	}
	return out
}

// runWireTurnErr 与 runWireTurn 同型但透传 error(降级失败断言用)。
func runWireTurnErr(t *testing.T, f *deepseekWireFacade, token string, req *official.ResponsesAPIRequest) (deepseekTurnOutput, error) {
	t.Helper()
	var turns []deepseekTurn
	for _, it := range responsesInputItems(req.Input) {
		if it.Text != "" {
			turns = append(turns, deepseekTurn{Text: it.Text})
		}
	}
	flow := &deepseekChatFlow{
		pool:      f.pool,
		sender:    &deepseekWebSender{client: f.client},
		clientKey: "sess-1",
		modelType: "default",
		token:     token,
		messages:  turns,
	}
	return flow.run()
}

// writeJSONWire 输出 JSON 响应(wire 测试局部辅助)。
func writeJSONWire(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// hexEncode 局部别名(避免 import 漂移)。
func hexEncode(b []byte) string {
	const hexDigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hexDigits[v>>4]
		out[i*2+1] = hexDigits[v&0x0f]
	}
	return string(out)
}

// Acceptance 1:带 X-Session-Key 连续两轮,第二轮请求体 parent_message_id
// 非空、prompt 为增量,且不再 create/delete session。
// 同时回归 ticket 05 的 P0:池路径建会话与 completion 都必须带上本轮 token
// (修复前 newLease 硬编码空 token,带 X-Session-Key 的请求 100% 401)。
func TestWireTwoTurnsResume(t *testing.T) {
	w := &wireUpstream{}
	client, closer := startWire(t, w)
	defer closer()

	pool := session.NewPool(deepseekUpstream{client}, session.PoolConfig{CleanupInterval: -1})
	f := &deepseekWireFacade{pool: pool, client: client}

	runWireTurn(t, f, "tok", responsesReq("你好"))
	runWireTurn(t, f, "tok", responsesReq("你好", "换个话题聊聊"))

	if w.creates != 1 {
		t.Fatalf("两轮应只建 1 个上游 session, creates=%d", w.creates)
	}
	if w.deletes != 0 {
		t.Fatalf("复用期间不应删除 session, deletes=%d", w.deletes)
	}
	if len(w.completions) != 2 {
		t.Fatalf("completions=%d, want 2", len(w.completions))
	}
	first, second := w.completions[0], w.completions[1]
	if first.ParentMessageID != nil {
		t.Fatalf("首轮 parent_message_id 应为 null, got %v", first.ParentMessageID)
	}
	if second.ParentMessageID == nil {
		t.Fatal("第二轮 parent_message_id 应非空(续轮锚点)")
	}
	if s, ok := second.ParentMessageID.(string); !ok || !strings.HasPrefix(s, "m") {
		t.Fatalf("第二轮 parent_message_id 应为上轮 response_message_id, got %v", second.ParentMessageID)
	}
	if second.Prompt != "换个话题聊聊" {
		t.Fatalf("第二轮 prompt 应只含本轮增量, got %q", second.Prompt)
	}
	if first.Prompt != "你好" {
		t.Fatalf("首轮 prompt = %q, want 你好", first.Prompt)
	}
	if second.SessionID != first.SessionID {
		t.Fatalf("两轮应同 session: %q vs %q", second.SessionID, first.SessionID)
	}
	// token 透传回归(修复前此处为空,假上游不校验 token 所以测试一直绿)。
	if len(w.createsAuth) != 1 || w.createsAuth[0] != "Bearer tok" {
		t.Fatalf("池路径建会话 Authorization = %v, want [Bearer tok]", w.createsAuth)
	}
	for i, comp := range w.completions {
		if comp.Auth != "Bearer tok" {
			t.Fatalf("第 %d 轮 completion Authorization = %q, want Bearer tok", i, comp.Auth)
		}
	}
}

// Acceptance 3:TTL 过期后下一轮 → 新会话引导(不重放历史),旧 session 删除。
func TestWireTTLExpiryNewBootstrap(t *testing.T) {
	w := &wireUpstream{}
	client, closer := startWire(t, w)
	defer closer()

	pool := session.NewPool(deepseekUpstream{client}, session.PoolConfig{CleanupInterval: -1, TTL: 10 * time.Millisecond})
	f := &deepseekWireFacade{pool: pool, client: client}

	runWireTurn(t, f, "tok", responsesReq("你好"))
	time.Sleep(30 * time.Millisecond)
	// TTL 过期即接受记忆断档:客户端(bridge)本轮只发新内容,
	// aurora 不做记忆 —— 不存在「重放历史」路径。
	runWireTurn(t, f, "tok", responsesReq("还在吗"))

	if w.creates != 2 {
		t.Fatalf("TTL 过期后应新开 session, creates=%d", w.creates)
	}
	if w.deletes != 1 {
		t.Fatalf("过期条目应顺手删上游, deletes=%d", w.deletes)
	}
	second := w.completions[1]
	if second.ParentMessageID != nil {
		t.Fatalf("新会话引导 parent 应为 null, got %v", second.ParentMessageID)
	}
	// spec 拍板:不重放历史 —— 但客户端本轮发来的内容即其最新一条,
	// 拍平结果 = 本轮内容(input 只带增量),prompt 不得包含首轮文本。
	if strings.Contains(second.Prompt, "你好\n\n还在吗") {
		t.Fatalf("TTL 过期后不应重放历史, prompt=%q", second.Prompt)
	}
	if second.Prompt != "还在吗" {
		t.Fatalf("新会话引导 prompt = %q, want 还在吗", second.Prompt)
	}
}

// Acceptance 4:vision 请求(model_type=vision)不进池,每轮独立 session。
func TestWireVisionBypassesPool(t *testing.T) {
	w := &wireUpstream{}
	client, closer := startWire(t, w)
	defer closer()

	pool := session.NewPool(deepseekUpstream{client}, session.PoolConfig{CleanupInterval: -1})

	for i := 0; i < 2; i++ {
		flow := &deepseekChatFlow{
			pool:      pool,
			sender:    &deepseekWebSender{client: client},
			clientKey: "sess-1",
			vision:    true,
			modelType: "vision",
			token:     "tok",
			messages:  []deepseekTurn{{Text: "看图"}},
		}
		if _, err := flow.run(); err != nil {
			t.Fatalf("vision run: %v", err)
		}
	}
	if w.creates != 2 {
		t.Fatalf("vision 请求不进池,应每轮新开 session, creates=%d", w.creates)
	}
	if w.deletes != 2 {
		t.Fatalf("vision 独立会话路径应逐轮删除, deletes=%d", w.deletes)
	}
	for _, comp := range w.completions {
		if comp.SessionID != "vision" && comp.SessionID == "" {
			t.Fatalf("vision 请求应有自建 session id")
		}
	}
}

// Acceptance 2(ticket 03):续轮 completion 失败(上游 4xx)→ 池内 entry 被丢弃
// (旧上游 session 顺手删)→ 自动新开 session 重试一次成功。
func TestWireFailureDegradesToNewSession(t *testing.T) {
	w := &wireUpstream{failCompletions: map[int]bool{1: true}}
	client, closer := startWire(t, w)
	defer closer()

	pool := session.NewPool(deepseekUpstream{client}, session.PoolConfig{CleanupInterval: -1})
	f := &deepseekWireFacade{pool: pool, client: client}

	// 第一轮:建会话,completion #0 成功。
	runWireTurn(t, f, "tok", responsesReq("你好"))

	// 第二轮续轮:completion #1 注入 4xx(会话失效/风控)→ 降级新开重试。
	out, err := runWireTurnErr(t, f, "tok", responsesReq("你好", "还在吗"))
	if err != nil {
		t.Fatalf("降级重试应成功,却返回 error: %v", err)
	}
	if out.text == "" {
		t.Fatal("降级重试成功但无正文")
	}

	if w.creates != 2 {
		t.Fatalf("失败后应新开一个 session(共 2), creates=%d", w.creates)
	}
	if w.deletes != 1 {
		t.Fatalf("失败租约应丢弃并删旧上游 session, deletes=%d", w.deletes)
	}
	if len(w.completions) != 3 {
		t.Fatalf("应恰好发 3 次(首轮+失败续轮+重试新开), got %d", len(w.completions))
	}
	failed, retry := w.completions[1], w.completions[2]
	if failed.ParentMessageID == nil {
		t.Fatal("失败的那次应是续轮形态(parent 非空)")
	}
	if failed.Prompt != "还在吗" {
		t.Fatalf("失败续轮 prompt = %q, want 还在吗", failed.Prompt)
	}
	if retry.ParentMessageID != nil {
		t.Fatalf("重试应新会话引导(parent 为 null), got %v", retry.ParentMessageID)
	}
	if retry.Prompt != "你好\n\n还在吗" {
		t.Fatalf("重试应拍平本轮内容, prompt=%q", retry.Prompt)
	}
	if retry.SessionID == failed.SessionID {
		t.Fatalf("重试应换用新 session: %q", retry.SessionID)
	}
	// 降级成功后新 entry 入池:下一轮继续复用重试建立的 session。
	out3, err := runWireTurnErr(t, f, "tok", responsesReq("你好", "还在吗", "再问"))
	if err != nil {
		t.Fatalf("降级后下一轮: %v", err)
	}
	if w.creates != 2 {
		t.Fatalf("降级后下一轮应复用重试 session, creates=%d", w.creates)
	}
	if w.completions[3].SessionID != retry.SessionID || w.completions[3].ParentMessageID == nil {
		t.Fatalf("降级后下一轮应续轮同一 session: %+v", w.completions[3])
	}
	_ = out3
}

// Acceptance 2b(ticket 03):连续失败 → 恰好重试一次后返回错误,不无限重试。
func TestWireDoubleFailureStopsAfterOneRetry(t *testing.T) {
	w := &wireUpstream{failCompletions: map[int]bool{0: true, 1: true, 2: true}}
	client, closer := startWire(t, w)
	defer closer()

	pool := session.NewPool(deepseekUpstream{client}, session.PoolConfig{CleanupInterval: -1})
	f := &deepseekWireFacade{pool: pool, client: client}

	if _, err := runWireTurnErr(t, f, "tok", responsesReq("你好")); err == nil {
		t.Fatal("连续失败应返回 error")
	}
	if len(w.completions) != 2 {
		t.Fatalf("应恰好尝试 2 次(首次+重试一次), got %d", len(w.completions))
	}
	if w.creates != 2 {
		t.Fatalf("两次尝试各新开 session, creates=%d", w.creates)
	}
	if w.deletes != 2 {
		t.Fatalf("两次失败租约都应丢弃删除, deletes=%d", w.deletes)
	}
}

// Acceptance(ticket 03):token 热加载/轮换后,池内旧 entry 视为失效——
// 同一 clientKey 的下一轮降级新开(parent 为 null),旧上游 session 顺手删。
func TestWireTokenRotationInvalidatesPoolEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.txt")
	if err := os.WriteFile(path, []byte("tok-A\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().Add(-2 * time.Second)
	if err := os.Chtimes(path, t0, t0); err != nil {
		t.Fatal(err)
	}

	w := &wireUpstream{}
	client, closer := startWireWithTokenFile(t, w, path)
	defer closer()
	pool := session.NewPool(deepseekUpstream{client}, session.PoolConfig{CleanupInterval: -1})
	f := &deepseekWireFacade{pool: pool, client: client}

	// 第一轮:token A,建 session 入池。
	runWireTurn(t, f, client.NextToken(), responsesReq("你好"))
	first := w.completions[0]

	// 凭证热加载:文件轮换到 token B。
	if err := os.WriteFile(path, []byte("tok-B\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t1 := time.Now()
	if err := os.Chtimes(path, t1, t1); err != nil {
		t.Fatal(err)
	}
	newTok := client.NextToken()
	if newTok != "tok-B" {
		t.Fatalf("热加载后 token = %q, want tok-B", newTok)
	}

	// 第二轮:同 clientKey 但新 token → 旧 entry 失效,新会话引导。
	if _, err := runWireTurnErr(t, f, newTok, responsesReq("你好", "还在吗")); err != nil {
		t.Fatalf("轮换后应降级新开成功: %v", err)
	}
	if w.creates != 2 {
		t.Fatalf("token 轮换应新开 session, creates=%d", w.creates)
	}
	if w.deletes != 1 {
		t.Fatalf("轮换旧 entry 应删上游 session, deletes=%d", w.deletes)
	}
	second := w.completions[1]
	if second.ParentMessageID != nil {
		t.Fatalf("轮换后新会话引导 parent 应为 null, got %v", second.ParentMessageID)
	}
	if second.SessionID == first.SessionID {
		t.Fatalf("轮换后不应复用旧 session: %q", second.SessionID)
	}
}

// usage 回填:非流式输出带上游 usage(错误字段落入响应统计)。
func TestWireUsageCollected(t *testing.T) {
	w := &wireUpstream{}
	client, closer := startWire(t, w)
	defer closer()
	pool := session.NewPool(deepseekUpstream{client}, session.PoolConfig{CleanupInterval: -1})
	out := runWireTurn(t, &deepseekWireFacade{pool: pool, client: client}, "tok", responsesReq("你好"))
	if out.promptTokens != 10 || out.completionTokens != 5 {
		t.Fatalf("usage 未回填: %d/%d, want 10/5", out.promptTokens, out.completionTokens)
	}
}
