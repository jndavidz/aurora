package handler

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aurora/httpclient/bogdanfinn"
	"aurora/internal/accounts"
	"aurora/internal/chatgpt"
	"aurora/internal/config"
	chatgpt_types "aurora/typings/chatgpt"

	"github.com/bogdanfinn/websocket"
	"github.com/gin-gonic/gin"
)

// ── chatgptTurnLoop seam 测试(spec arch-deepening-phase2 WP2)───
//
// 「发一轮上游」经 sendTurn 注入假实现(标准 OpenAI chunk 形态的假 SSE,
// 同 chatgpt 包 request_test 先例)——这是 ChatGPT 兜底路径的第一个自动化
// 测试(此前循环埋在三份入口里,无法离线驱动)。
// continue 链的触发需模拟 ChatGPT 私有 v2 事件格式,成本高于收益;重排逻辑
// (4 行赋值+循环再入)由合并时的逐行 diff 审查保全,见 ticket 02 记录。

// fakeTurnSender 按脚本逐轮返回预置响应或 send 失败。
type fakeTurnSender struct {
	rounds []fakeTurnRound
	n      int
}

type fakeTurnRound struct {
	resp *http.Response
	err  error
	// sendErrStatus 是 send 失败时透传的 HTTP 状态码。
	sendErrStatus int
}

func (f *fakeTurnSender) send() (*http.Response, *websocket.Conn, *chatgpt.TurnStile, *bogdanfinn.TlsClient, int, error) {
	i := f.n
	if i >= len(f.rounds) {
		i = len(f.rounds) - 1
	}
	f.n++
	r := f.rounds[i]
	if r.err != nil {
		return nil, nil, nil, nil, r.sendErrStatus, r.err
	}
	return r.resp, nil, nil, nil, 0, nil
}

// sseRoundBody 构造 HandlerDetailedWithOptions 可解析的最小单轮 OpenAI chunk SSE。
func sseRoundBody(text, convID string) *http.Response {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant"}}]}`,
		`data: {"choices":[{"delta":{"content":"` + text + `"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"conversation_id":"` + convID + `"}`,
		`data: [DONE]`,
		``,
	}, "\n")
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

var turnLoopTestCfg = config.Config{MaxContinueCount: 3}

func newTurnLoopHarness(t *testing.T) (*ChatHandler, *fakeTurnSender, *gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(nil))
	h := &ChatHandler{
		sessions: NewSessionManager(),
		cfg:      &turnLoopTestCfg,
	}
	return h, &fakeTurnSender{}, c, rec
}

// TestTurnLoopSingleTurn 单轮成功:累积正文/思维链、conversationID、TTFT、onTurn 回调。
func TestTurnLoopSingleTurn(t *testing.T) {
	h, sender, c, _ := newTurnLoopHarness(t)
	sender.rounds = []fakeTurnRound{{resp: sseRoundBody("你好", "conv-1")}}
	req := &chatgpt_types.ChatGPTRequest{ConversationID: "conv-1"}
	state := chatgpt.NewChatClientStateForAccount(&accounts.Account{})

	var turns int
	var seenText string
	res, tlErr := h.chatgptTurnLoop(c, &accounts.Account{}, req, state, sender.send, false, "uid", "auto", time.Now(), chatgpt.HandlerDetailedOptions{}, false, func(r chatgpt.HandlerResult) {
		turns++
		seenText += r.Text
	})
	if tlErr != nil {
		t.Fatalf("turnLoop err: %v", tlErr)
	}
	if res.text != "你好" || res.conversationID != "conv-1" {
		t.Fatalf("res=%+v", res)
	}
	if !res.ttftSet || res.ttftMs < 0 {
		t.Fatalf("ttft=%v/%d", res.ttftSet, res.ttftMs)
	}
	if turns != 1 || seenText != "你好" {
		t.Fatalf("onTurn calls=%d text=%q", turns, seenText)
	}
	if sender.n != 1 {
		t.Fatalf("sendTurn=%d, want 1", sender.n)
	}
}

// TestTurnLoopSendFailureClassified 首轮发送失败 → turnSendFailed 携带状态码;
// 会话未注册、无 onTurn 回调。
func TestTurnLoopSendFailureClassified(t *testing.T) {
	h, sender, c, _ := newTurnLoopHarness(t)
	sender.rounds = []fakeTurnRound{{err: errOf("no account"), sendErrStatus: 401}}
	req := &chatgpt_types.ChatGPTRequest{}

	var turns int
	_, tlErr := h.chatgptTurnLoop(c, &accounts.Account{}, req, chatgpt.NewChatClientStateForAccount(&accounts.Account{}), sender.send, false, "uid", "auto", time.Now(), chatgpt.HandlerDetailedOptions{}, false, func(r chatgpt.HandlerResult) { turns++ })
	if tlErr == nil || tlErr.kind != turnSendFailed || tlErr.status != 401 || tlErr.msg != "no account" {
		t.Fatalf("tlErr=%+v, want kind=turnSendFailed status=401 msg=no account", tlErr)
	}
	if turns != 0 {
		t.Fatalf("onTurn must not fire, got %d", turns)
	}
}

// TestTurnLoopHandledErrorResponse 上游错误响应(Handle_request_error 已写) →
// turnHandledError;调用方据此直接返回(不再输出)。
func TestTurnLoopHandledErrorResponse(t *testing.T) {
	h, sender, c, _ := newTurnLoopHarness(t)
	sender.rounds = []fakeTurnRound{{resp: &http.Response{
		StatusCode: 429,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"detail":{"message":"limit"}}`)),
	}}}
	_, tlErr := h.chatgptTurnLoop(c, &accounts.Account{}, &chatgpt_types.ChatGPTRequest{}, chatgpt.NewChatClientStateForAccount(&accounts.Account{}), sender.send, false, "uid", "auto", time.Now(), chatgpt.HandlerDetailedOptions{}, false, nil)
	if tlErr == nil || tlErr.kind != turnHandledError {
		t.Fatalf("tlErr=%+v, want kind=turnHandledError", tlErr)
	}
}

// TestTurnLoopPingParameterized keepAlivePing=false 时成功对话只产生一次
// 上游调用(ping 分支不产生额外通道;参数化后 Responses 面零行为变化)。
func TestTurnLoopPingParameterized(t *testing.T) {
	h, sender, c, _ := newTurnLoopHarness(t)
	sender.rounds = []fakeTurnRound{{resp: sseRoundBody("答", "conv-9")}}

	res, tlErr := h.chatgptTurnLoop(c, &accounts.Account{}, &chatgpt_types.ChatGPTRequest{}, chatgpt.NewChatClientStateForAccount(&accounts.Account{}), sender.send, false, "uid", "auto", time.Now(), chatgpt.HandlerDetailedOptions{}, false, nil)
	if tlErr != nil || res.text != "答" {
		t.Fatalf("res=%+v err=%v", res, tlErr)
	}
	if sender.n != 1 {
		t.Fatalf("sendTurn=%d, want 1(无 ping/续轮)", sender.n)
	}
}

type errOf string

func (e errOf) Error() string { return string(e) }
