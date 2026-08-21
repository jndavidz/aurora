// Package mimoweb 实现 aistudio.xiaomimimo.com 网页接口逆向客户端(直连)。
//
// 协议要点(2026-08-14 CDP 抓包 + 页内 fetch/直连实测):
//   - 认证:登录态 cookie `xiaomichatbot_ph`,以 URL 查询参数形式附加在
//     每个 /open-apis/ 请求上(值需 urlencode)。
//   - Chat:POST /open-apis/bot/chat,body 含 modelConfig.model 指定模型,
//     响应为 SSE:event=dialogId(对话 id)、event=message(data:{"type":"text",
//     "content":...}) 为正文增量、event=usage(用量)、event=finish([DONE])。
//     mimo-v2.5-pro 会先流 <think>...</think> 思考段(闭合标记明确,即使
//     enableThinking:false 也会输出),解析时剔除只回正式答复。
//   - ASR:四步 ——
//     1) POST /open-apis/resource/genUploadInfo {"fileName":...}
//     → {resourceId, resourceUrl}(FDS 签名 PUT URL)
//     2) PUT resourceUrl(音频字节)
//     3) POST /open-apis/asr/recognize {conversationId,msgId,audioUrl,
//     language:"auto",modelConfig:{modelCode:"mimo-v2.5-asr"}}
//     → {taskId}
//     4) 轮询 GET /open-apis/asr/recognizeStatus?taskId=...
//     status=generating→success 时 data.text 为识别文本。
//   - 多轮:conversationId 服务端记忆;aurora 策略与其余直连 provider 一致:
//     拍平全量 prompt + 每轮新 conversationId。
package mimoweb

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"aurora/httpclient"
	"aurora/httpclient/bogdanfinn"

	"github.com/google/uuid"
)

const (
	DefaultBase = "https://aistudio.xiaomimimo.com"
	webUA       = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"
)

// Client 是 mimo 客户端,持有 ph token 池。
type Client struct {
	tokens []string
	cursor int
	base   string
	tls    *bogdanfinn.TlsClient
}

// NewClient 构造客户端。tokens 每行一个完整 Cookie 串
// (形如 'xiaomichatbot_ph="..."; xiaomichatbot_serviceToken="..."; userId=...')。
func NewClient(tokens []string) *Client {
	return &Client{tokens: tokens, base: DefaultBase, tls: bogdanfinn.NewStdClient()}
}

// HasToken 报告是否有可用 token。
func (c *Client) HasToken() bool { return len(c.tokens) > 0 }

// NextToken 轮询取 token(cookie 串)。
func (c *Client) NextToken() string {
	if len(c.tokens) == 0 {
		return ""
	}
	t := c.tokens[c.cursor%len(c.tokens)]
	c.cursor++
	return t
}

// phFromCookie 从 cookie 串提取 xiaomichatbot_ph 值(去掉可能的外层引号)。
func phFromCookie(cookie string) string {
	i := strings.Index(cookie, "xiaomichatbot_ph=")
	if i < 0 {
		return ""
	}
	rest := cookie[i+len("xiaomichatbot_ph="):]
	rest = strings.TrimPrefix(rest, `"`)
	if j := strings.IndexAny(rest, `";`); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// apiURL 构造带认证参数的 API URL。
func (c *Client) apiURL(cookie, path string) string {
	return c.base + path + "?xiaomichatbot_ph=" + url.QueryEscape(phFromCookie(cookie))
}

// authHeaders 返回带 Cookie 的基础请求头。
func authHeaders(cookie string) httpclient.AuroraHeaders {
	return httpclient.AuroraHeaders{
		"Content-Type": "application/json",
		"User-Agent":   webUA,
		"Origin":       DefaultBase,
		"Referer":      DefaultBase + "/",
		"Cookie":       cookie,
	}
}

// ─── Chat ─────────────────────────────────────────────────────────

// Delta 是一帧增量。
type Delta struct {
	Text string
	Done bool
}

// CompletionRequest 是一次对话请求。
type CompletionRequest struct {
	Prompt string
	Model  string // mimo-v2.5-pro 等
}

// StreamResult 是整条流的汇总。
type StreamResult struct {
	Text string
	Err  string
	Done bool
}

// Complete 发消息并流式返回增量(SSE 解析,过滤 <think> 思考段)。
func (c *Client) Complete(token string, req CompletionRequest, onDelta func(Delta)) StreamResult {
	var res StreamResult
	bodyObj := map[string]any{
		"msgId":          strings.ReplaceAll(uuid.NewString(), "-", ""),
		"conversationId": strings.ReplaceAll(uuid.NewString(), "-", ""),
		"query":          req.Prompt,
		"isEditedQuery":  false,
		"modelConfig": map[string]any{
			"enableThinking":  false,
			"webSearchStatus": "disabled",
			"model":           req.Model,
		},
		"multiMedias": []any{},
	}
	bodyBytes, _ := json.Marshal(bodyObj)
	u := c.apiURL(token, "/open-apis/bot/chat")
	h := authHeaders(token)
	resp, err := c.tls.Request(httpclient.POST, u, h, nil, bytes.NewReader(bodyBytes))
	if err != nil {
		res.Err = fmt.Sprintf("mimo chat: %v", err)
		return res
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		res.Err = fmt.Sprintf("mimo chat http %d: %s", resp.StatusCode, truncate(string(data), 200))
		return res
	}

	// SSE 解析:event: message + data:{"type":"text","content":...}
	// <think>...</think> 思考段剔除;event:finish(data:[DONE])为流结束。
	// 清洗:独立 "webSearch" 帧跳过;citation 标记(跨帧分片,如 "(citation:5" + ")")
	// 用 cleaner 缓冲丢弃(2026-08-22 实测分片)。
	inThink := false
	cc := newCitationCleaner()
	emit := func(text string) {
		text = cc.push(strings.TrimPrefix(text, "\u0000"))
		if text == "" {
			return
		}
		res.Text += text
		if onDelta != nil {
			onDelta(Delta{Text: text})
		}
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == `{"content":"[DONE]"}` {
			res.Done = true
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		if ev.Type != "text" || ev.Content == "" {
			continue
		}
		// 独立 webSearch 标记帧(联网搜索状态,非正文)
		if ev.Content == "webSearch" {
			continue
		}
		text := ev.Content
		for len(text) > 0 {
			if inThink {
				if idx := strings.Index(text, "</think>"); idx >= 0 {
					inThink = false
					text = text[idx+len("</think>"):]
					continue
				}
				text = ""
				break
			}
			if idx := strings.Index(text, "<think>"); idx >= 0 {
				if idx > 0 {
					emit(text[:idx])
				}
				text = text[idx+len("<think>"):]
				inThink = true
				continue
			}
			emit(text)
			break
		}
	}
	if err := sc.Err(); err != nil && res.Text == "" {
		res.Err = fmt.Sprintf("mimo read: %v", err)
	}
	// 最终兜底:整体剥离残留的 citation 标记(流式 cleaner 之外的遗漏,如未闭合片段)
	res.Text += cc.flush()
	res.Text = stripAllCitations(res.Text)
	if res.Text == "" && res.Err == "" {
		res.Err = "mimo: empty response"
	}
	if res.Err == "" {
		res.Done = true
	}
	return res
}

// ─── ASR ──────────────────────────────────────────────────────────

// ASR 上传音频并识别,返回文本。
func (c *Client) ASR(token, fileName string, audio []byte) (string, error) {
	// 页面行为:上传文件统一以 .mp3 命名(genUploadInfo 的 fileName 决定 FDS 路径)
	if !strings.HasSuffix(strings.ToLower(fileName), ".mp3") {
		fileName = fileName + ".mp3"
	}
	// 1. 申请上传
	upBody, _ := json.Marshal(map[string]string{"fileName": fileName})
	resp, err := c.tls.Request(httpclient.POST, c.apiURL(token, "/open-apis/resource/genUploadInfo"),
		authHeaders(token),
		nil, bytes.NewReader(upBody))
	if err != nil {
		return "", fmt.Errorf("mimo genUploadInfo: %v", err)
	}
	upData, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	resp.Body.Close()
	var up struct {
		Code int `json:"code"`
		Data struct {
			ResourceID  string `json:"resourceId"`
			ResourceURL string `json:"resourceUrl"` // 下载签名 URL(用于 recognize 的 audioUrl)
			UploadURL   string `json:"uploadUrl"`   // 上传签名 URL(用于 PUT)
		} `json:"data"`
	}
	if err := json.Unmarshal(upData, &up); err != nil || up.Code != 0 || up.Data.UploadURL == "" {
		return "", fmt.Errorf("mimo genUploadInfo failed: %s", truncate(string(upData), 200))
	}

	// 2. PUT 上传文件字节(必须用 uploadUrl 的签名;FDS 校验签名与 Content-Type)
	putResp, err := c.tls.Request(httpclient.PUT, up.Data.UploadURL,
		httpclient.AuroraHeaders{
			"Content-Type": "application/octet-stream",
			"User-Agent":   webUA,
			"Referer":      c.base + "/",
		},
		nil, bytes.NewReader(audio))
	if err != nil {
		return "", fmt.Errorf("mimo put: %v", err)
	}
	putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK && putResp.StatusCode != http.StatusNoContent && putResp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("mimo put http %d", putResp.StatusCode)
	}

	// 3. 建会话(前端先本地生成 conversationId 再 save;recognize 校验其存在)
	convID := strings.ReplaceAll(uuid.NewString(), "-", "")
	saveBody, _ := json.Marshal(map[string]string{
		"conversationId": convID,
		"title":          "语音识别",
		"type":           "chat",
	})
	saveResp, err := c.tls.Request(httpclient.POST, c.apiURL(token, "/open-apis/chat/conversation/save"),
		authHeaders(token), nil, bytes.NewReader(saveBody))
	if err != nil {
		return "", fmt.Errorf("mimo conversation save: %v", err)
	}
	io.Copy(io.Discard, io.LimitReader(saveResp.Body, 4096))
	saveResp.Body.Close()

	// 4. 识别
	recBody, _ := json.Marshal(map[string]any{
		"conversationId": convID,
		"msgId":          strings.ReplaceAll(uuid.NewString(), "-", ""),
		"audioUrl":       up.Data.ResourceURL,
		"language":       "auto",
		"modelConfig":    map[string]string{"modelCode": "mimo-v2.5-asr"},
	})
	recResp, err := c.tls.Request(httpclient.POST, c.apiURL(token, "/open-apis/asr/recognize"),
		authHeaders(token),
		nil, bytes.NewReader(recBody))
	if err != nil {
		return "", fmt.Errorf("mimo recognize: %v", err)
	}
	recData, _ := io.ReadAll(io.LimitReader(recResp.Body, 64*1024))
	recResp.Body.Close()
	var rec struct {
		Code int `json:"code"`
		Data struct {
			TaskID json.Number `json:"taskId"` // 数字
		} `json:"data"`
	}
	if err := json.Unmarshal(recData, &rec); err != nil || rec.Code != 0 || rec.Data.TaskID.String() == "" {
		return "", fmt.Errorf("mimo recognize failed: %s", truncate(string(recData), 200))
	}

	// 4. 轮询状态直到出文本
	taskID := rec.Data.TaskID.String()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		stResp, err := c.tls.Request(httpclient.GET, c.base+"/open-apis/asr/recognizeStatus?taskId="+taskID,
			authHeaders(token), nil, nil)
		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		stData, _ := io.ReadAll(io.LimitReader(stResp.Body, 128*1024))
		stResp.Body.Close()
		var st struct {
			Code int `json:"code"`
			Data struct {
				Status string `json:"status"`
				Text   string `json:"text"`
			} `json:"data"`
		}
		if err := json.Unmarshal(stData, &st); err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if st.Data.Text != "" {
			return st.Data.Text, nil
		}
		if st.Data.Status == "failed" {
			return "", fmt.Errorf("mimo asr failed: %s", truncate(string(stData), 200))
		}
		time.Sleep(500 * time.Millisecond)
	}
	return "", fmt.Errorf("mimo asr timeout")
}

// citationCleaner 处理跨帧分片的 citation 标记(如 "(citation:5" 与 ")" 分属两帧)。
// 缓冲未闭合的 "citation:" 片段,闭合后整体丢弃;其余文本正常输出。
type citationCleaner struct {
	buf string
}

func newCitationCleaner() *citationCleaner { return &citationCleaner{} }

func (c *citationCleaner) push(text string) string {
	c.buf += text
	var out strings.Builder
	for {
		idx := strings.Index(c.buf, "citation") // 含无冒号残片("citation" 帧)
		if idx < 0 {
			out.WriteString(c.buf)
			c.buf = ""
			break
		}
		// 开括号([ 或 ()紧邻 citation 前,一并吞掉
		start := idx
		for start > 0 && (c.buf[start-1] == '[' || c.buf[start-1] == '(') {
			start--
		}
		// 缓冲过长仍无闭合:判定为正文(如模型回复里出现 citation 单词),整体放行
		if len(c.buf)-start > 100 {
			out.WriteString(c.buf)
			c.buf = ""
			break
		}
		out.WriteString(c.buf[:start])
		rest := c.buf[idx+len("citation"):]
		if closeIdx := strings.IndexAny(rest, ")]"); closeIdx >= 0 {
			c.buf = rest[closeIdx+1:] // 完整标记,丢弃
			continue
		}
		c.buf = c.buf[start:] // 未闭合,留待下一帧
		break
	}
	res := out.String()
	// 帧尾孤立开括号(可能是 citation 开括号跨帧,如 "25℃(" + 下一帧 "citation:1)"),
	// 回退到缓冲等待确认;非 citation 时下一帧会原样输出。
	if res != "" && (strings.HasSuffix(res, "(") || strings.HasSuffix(res, "[")) {
		c.buf = res[len(res)-1:] + c.buf
		res = res[:len(res)-1]
	}
	return res
}

// flush 流结束时处理残留缓冲:未闭合 citation 标记丢弃;孤立开括号等输出。
func (c *citationCleaner) flush() string {
	if c.buf == "" {
		return ""
	}
	res := c.buf
	c.buf = ""
	if strings.Contains(res, "citation") {
		return "" // 未闭合引用标记,丢弃
	}
	return res
}

// stripAllCitations 整体剥离 citation 标记及流式分片残留(含未闭合片段)。
// mimo 分片极细:可能出现 "citation"、"citation:18"、"(citation"、"防晒措施(citation"
// 等残片 —— 统一匹配 [\[(]? citation [：:]? 数字? [:url]? [)\]]?(2026-08-22 实测)。
var citationFullRe = regexp.MustCompile(`[\[(]?\s*citation\s*[:：]?\s*\d*\s*(?:[:：][^)\]]*)?[)\]]?`)

func stripAllCitations(s string) string { return citationFullRe.ReplaceAllString(s, "") }

// CleanCitations 导出:整体剥离 citation 残留(供 provider 层对聚合文本兜底清洗)。
func CleanCitations(s string) string { return stripAllCitations(s) }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
