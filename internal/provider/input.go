package provider

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"aurora/typings/official"
)

// responsesInputItem 是 Responses input 数组里的一项(轻量解析,供 provider 侧拍平)。
type responsesInputItem struct {
	Type      string // "message" | "function_call" | "function_call_output"
	Role      string
	Text      string
	ImageURLs []string
}

// rawInputItem 是 input 数组元素的原始结构。
type rawInputItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Output    json.RawMessage `json:"output"`
}

// responsesInputItems 把 ResponsesAPIRequest.Input 解析成统一的 item 列表。
// 支持字符串 / 内容对象 / item 数组(message、function_call、function_call_output)。
func responsesInputItems(raw json.RawMessage) []responsesInputItem {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	// 纯字符串
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return []responsesInputItem{{Type: "message", Role: "user", Text: text}}
	}
	// 单个对象
	var single rawInputItem
	if err := json.Unmarshal(raw, &single); err == nil && (single.Content != nil || single.Type != "") {
		return []responsesInputItem{parseInputItem(single)}
	}
	// item 数组
	var items []rawInputItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	out := make([]responsesInputItem, 0, len(items))
	for _, it := range items {
		out = append(out, parseInputItem(it))
	}
	return out
}

func parseInputItem(it rawInputItem) responsesInputItem {
	switch it.Type {
	case "function_call":
		// 历史呈现必须复刻模型自己输出的完整 glm 围栏形状(含 name)—— 只给裸
		// arguments 会让模型在下一轮"失忆"(不知道自己调过什么工具/什么协议,
		// 实测 2026-09-02 pi 多轮:轮2 声称"未提供 run_cmd"而胡乱拒答)。
		inner, _ := json.Marshal(map[string]any{
			"type": "tool_calls",
			"tool_calls": map[string]any{"name": it.Name, "arguments": it.Arguments},
		})
		return responsesInputItem{Type: "function_call", Role: "assistant", Text: string(inner)}
	case "function_call_output":
		return responsesInputItem{Type: "function_call_output", Role: "tool", Text: rawResponsesText(it.Output)}
	default:
		item := responsesInputItem{Type: "message", Role: it.Role}
		if item.Role == "" {
			item.Role = "user"
		}
		item.Text, item.ImageURLs = contentPartsText(it.Content)
		return item
	}
}

// apiMessagesToItems 把 chat.completions 的 messages 转成统一的 responsesInputItem 列表,
// 供两种接口共享 prompt 构建。
func apiMessagesToItems(messages []official.APIMessage) []responsesInputItem {
	var out []responsesInputItem
	for _, msg := range messages {
		switch msg.Role {
		case "assistant":
			if len(msg.ToolCalls) > 0 {
				for _, tc := range msg.ToolCalls {
					// 历史呈现复刻完整 glm 围栏形状(含 name),否则模型下轮失忆
					inner, _ := json.Marshal(map[string]any{
						"type":       "tool_calls",
						"tool_calls": map[string]any{"name": tc.Function.Name, "arguments": tc.Function.Arguments},
					})
					out = append(out, responsesInputItem{Type: "function_call", Role: "assistant", Text: string(inner)})
				}
				continue
			}
		case "tool", "function":
			out = append(out, responsesInputItem{Type: "function_call_output", Role: "tool", Text: msg.Text()})
			continue
		}
		text, urls := contentPartsFromMessage(msg.Content)
		item := responsesInputItem{Type: "message", Role: msg.Role}
		if item.Role == "" {
			item.Role = "user"
		}
		item.Text = text
		item.ImageURLs = urls
		if item.Text == "" && len(item.ImageURLs) == 0 {
			continue
		}
		out = append(out, item)
	}
	return out
}

// contentPartsFromMessage 从已解析的 MessageContent 提取文本与图片 URL。
func contentPartsFromMessage(c official.MessageContent) (string, []string) {
	if len(c.Parts) == 0 {
		return c.TextValue, nil
	}
	var texts []string
	var urls []string
	for _, p := range c.Parts {
		if p.Type == "image_url" && p.ImageURL != nil && p.ImageURL.URL != "" {
			urls = append(urls, p.ImageURL.URL)
			continue
		}
		if p.Text != "" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, ""), urls
}

// contentPartsText 从 content 里抽取文本与图片 URL。
func contentPartsText(raw json.RawMessage) (string, []string) { // 纯字符串
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	// parts 数组
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var texts []string
		var urls []string
		for _, p := range parts {
			if p.Type == "image_url" && p.ImageURL.URL != "" {
				urls = append(urls, p.ImageURL.URL)
				continue
			}
			if p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
		return strings.Join(texts, ""), urls
	}
	return "", nil
}

// rawResponsesText 把 json.RawMessage 还原为字符串(字符串或 JSON 字符串)。
func rawResponsesText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}

// countInputChars 粗略统计 input 的字符数(代替 token 统计,够用)。
func countInputChars(req *official.ResponsesAPIRequest) int {
	n := 0
	for _, it := range responsesInputItems(req.Input) {
		n += len(it.Text)
	}
	if instr := rawResponsesText(req.Instructions); instr != "" {
		n += len(instr)
	}
	return n
}

// hasImages 报告 input 是否带图片。
func hasImages(refFileIDs []string) bool { return len(refFileIDs) > 0 }

// uploadImages 把 Responses input 里的图片上传并 fork 成 vision 版(实测 P0),
// 返回 ref_file_ids。流程:upload_file → fetch_files(READY)→ fork_file_task(to_model_type=vision)。
func uploadImages(client interface {
	NextToken() string
}, token string, req *official.ResponsesAPIRequest) ([]string, error) {
	var urls []string
	for _, it := range responsesInputItems(req.Input) {
		urls = append(urls, it.ImageURLs...)
	}
	return uploadImageURLs(client, token, urls)
}

// uploadImagesFromMessages 从 chat.completions 的 messages 里收集图片并上传。
func uploadImagesFromMessages(client interface {
	NextToken() string
}, token string, messages []official.APIMessage) ([]string, error) {
	var urls []string
	for _, msg := range messages {
		for _, f := range msg.Files() {
			u := f.Source
			if u == "" {
				u = f.ID
			}
			if u == "" {
				u = f.FileID
			}
			if u != "" && (strings.HasPrefix(u, "data:") || strings.HasPrefix(u, "http")) {
				urls = append(urls, u)
			}
		}
	}
	return uploadImageURLs(client, token, urls)
}

// uploadImageURLs 上传并 fork 一批图片 URL,返回 ref_file_ids。
func uploadImageURLs(client interface {
	NextToken() string
}, token string, urls []string) ([]string, error) {
	if len(urls) == 0 {
		return nil, nil
	}
	web, ok := client.(interface {
		UploadFile(token, filename, contentType string, data []byte) (string, error)
		FetchFiles(token, fileID string) (string, error)
		ForkFileToVision(token, fileID string) (string, error)
	})
	if !ok {
		return nil, nil
	}
	var refIDs []string
	for _, imgURL := range urls {
		data, ct, name, err := fetchImage(imgURL)
		if err != nil {
			return nil, err
		}
		id, err := web.UploadFile(token, name, ct, data)
		if err != nil {
			return nil, err
		}
		// fetch_files 确认就绪(最多 6 次 × 500ms)。
		for i := 0; i < 6; i++ {
			status, err := web.FetchFiles(token, id)
			if err == nil && status == "READY" {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		// fork 成 vision 版(识图 completion 必需)。
		visionID, err := web.ForkFileToVision(token, id)
		if err != nil {
			return nil, err
		}
		refIDs = append(refIDs, visionID)
	}
	return refIDs, nil
}

// fetchImage 取回图片字节:data: URI 直接解码,http(s) URL 下载。
func fetchImage(u string) ([]byte, string, string, error) {
	if strings.HasPrefix(u, "data:") {
		// data:[<mime>][;base64],<data>
		comma := strings.Index(u, ",")
		if comma < 0 {
			return nil, "", "", fmt.Errorf("invalid data uri")
		}
		meta := u[5:comma]
		dataPart := u[comma+1:]
		mime := "image/png"
		enc := ""
		if semi := strings.Index(meta, ";"); semi >= 0 {
			mime = meta[:semi]
			enc = meta[semi+1:]
		} else if meta != "" {
			mime = meta
		}
		var b []byte
		var err error
		if enc == "base64" {
			b, err = base64.StdEncoding.DecodeString(dataPart)
		} else {
			b = []byte(dataPart)
		}
		if err != nil {
			return nil, "", "", err
		}
		return b, mime, "image." + guessExt(mime), nil
	}
	// http(s) URL
	resp, err := http.Get(u)
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil {
		return nil, "", "", err
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "image/png"
	}
	return b, ct, "image." + guessExt(ct), nil
}

func guessExt(mime string) string {
	switch strings.ToLower(strings.TrimSpace(strings.Split(mime, ";")[0])) {
	case "image/jpeg", "image/jpg":
		return "jpg"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	default:
		return "png"
	}
}
