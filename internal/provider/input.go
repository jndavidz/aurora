package provider

import (
	"encoding/json"
	"strings"

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
		return responsesInputItem{Type: "function_call", Role: "assistant", Text: it.Arguments}
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

// contentPartsText 从 content 里抽取文本与图片 URL。
func contentPartsText(raw json.RawMessage) (string, []string) {
	// 纯字符串
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

// uploadImages [P0] 把 input 里的图片上传,返回 ref_file_ids。
// 网页文件上传端点未在参考仓库统一;当前返回空(识图待 P0 验证后实现)。
func uploadImages(client interface {
	NextToken() string
}, token string, req *official.ResponsesAPIRequest) ([]string, error) {
	for _, it := range responsesInputItems(req.Input) {
		if len(it.ImageURLs) > 0 {
			// TODO(P0): 走 deepseekweb 文件上传端点取 ref_file_ids。
			return nil, nil
		}
	}
	return nil, nil
}
