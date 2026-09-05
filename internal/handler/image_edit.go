package handler

// image_edit.go —— 图片编辑/变体路径(G4 第二步拆分,2026-09-05)。
// 自 image_handler.go 原样迁移,零逻辑变更:
//   - Edits/Variations 入口 → runImageEditFlow(上传源图 → 带引用对话 → 取图)
//   - 图像源解析群(URL/dataURL/JSON/HTTP 下载 → editImageInput)

import (
	"encoding/base64"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"

	"aurora/httpclient"
	"aurora/httpclient/bogdanfinn"
	"aurora/internal/accounts"
	"aurora/internal/apierrors"
	"aurora/internal/chatgpt"
	officialtypes "aurora/typings/official"

	"github.com/gin-gonic/gin"
)

type editImageInput struct {
	Data        []byte
	Filename    string
	ContentType string
}

// imageEditImageReferenceFields 与 chatgpt2api/api/image_inputs.IMAGE_REFERENCE_FIELDS 对齐。

func normalizeImageEditImages(rawImages []interface{}) []editImageInput {
	out := make([]editImageInput, 0, len(rawImages))
	for _, raw := range rawImages {
		switch v := raw.(type) {
		case *multipart.FileHeader:
			if v == nil {
				continue
			}
			f, err := v.Open()
			if err != nil {
				continue
			}
			data, err := io.ReadAll(f)
			f.Close()
			if err != nil || len(data) == 0 {
				continue
			}
			ct := v.Header.Get("Content-Type")
			if ct == "" {
				ct = "image/png"
			}
			name := v.Filename
			if name == "" {
				name = "image.png"
			}
			out = append(out, editImageInput{Data: data, Filename: name, ContentType: ct})
		case editImageInput:
			if len(v.Data) > 0 {
				out = append(out, v)
			}
		default:
			// JSON 形态的 image 引用由 collectImageEditSourcesFromValue 处理,这里跳过
		}
	}
	return out
}

func imageEditReadJSONImage(data []byte, filename, contentType string) (editImageInput, error) {
	if len(data) == 0 {
		return editImageInput{}, fmt.Errorf("image data is empty")
	}
	if filename == "" {
		filename = "image.png"
	}
	if contentType == "" {
		contentType = "image/png"
	}
	return editImageInput{Data: data, Filename: filename, ContentType: contentType}, nil
}

func imageEditDecodeDataURL(url string) (editImageInput, error) {
	comma := strings.Index(url, ",")
	if comma < 0 {
		return editImageInput{}, fmt.Errorf("invalid data URL")
	}
	header := url[:comma]
	payload := url[comma+1:]
	contentType := "image/png"
	if semi := strings.Index(header, ";"); semi > 5 {
		contentType = header[5:semi]
		if !strings.HasPrefix(contentType, "image/") {
			return editImageInput{}, fmt.Errorf("data URL must be an image, got %q", contentType)
		}
	}
	dec := base64.StdEncoding
	if strings.Contains(header, ";base64") {
		dec = base64.StdEncoding
	} else {
		dec = base64.URLEncoding
	}
	raw, err := dec.DecodeString(payload)
	if err != nil {
		raw, err = base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return editImageInput{}, err
		}
		contentType = "image/png"
	}
	ext := "png"
	switch strings.ToLower(contentType) {
	case "image/jpeg":
		ext = "jpg"
	case "image/gif":
		ext = "gif"
	case "image/webp":
		ext = "webp"
	}
	return imageEditReadJSONImage(raw, "image_url."+ext, contentType)
}

func imageEditDownloadHTTPURL(client httpclient.AuroraHttpClient, url string) (editImageInput, error) {
	if client == nil {
		client = bogdanfinn.NewStdClient()
	}
	headers := httpclient.AuroraHeaders{
		"User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
		"Accept":     "image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8",
	}
	resp, err := client.Request(httpclient.GET, url, headers, nil, nil)
	if err != nil {
		return editImageInput{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return editImageInput{}, fmt.Errorf("download image failed: status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return editImageInput{}, err
	}
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = http.DetectContentType(data)
	}
	filename := "image_url"
	if idx := strings.LastIndex(url, "/"); idx >= 0 && idx < len(url)-1 {
		filename = url[idx+1:]
	}
	if dot := strings.Index(filename, "?"); dot >= 0 {
		filename = filename[:dot]
	}
	if filename == "" {
		filename = "image_url.png"
	}
	return imageEditReadJSONImage(data, filename, contentType)
}

func imageEditConvertURL(client httpclient.AuroraHttpClient, raw string) (editImageInput, bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return editImageInput{}, false, nil
	}
	if strings.HasPrefix(raw, "data:") {
		item, err := imageEditDecodeDataURL(raw)
		return item, true, err
	}
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		item, err := imageEditDownloadHTTPURL(client, raw)
		return item, true, err
	}
	// 非 URL、非 data URL:当 base64 处理
	item, err := imageEditReadJSONImage([]byte(raw), "image.png", "image/png")
	return item, true, err
}

func resolveEditImageSources(c *gin.Context, body map[string]interface{}, client httpclient.AuroraHttpClient) ([]editImageInput, error) {
	out := make([]editImageInput, 0, 2)
	appendValue := func(v interface{}) error {
		switch t := v.(type) {
		case string:
			item, ok, err := imageEditConvertURL(client, t)
			if err != nil {
				return err
			}
			if ok {
				out = append(out, item)
			}
		case map[string]interface{}:
			if urlVal, ok := t["image_url"]; ok {
				if s, ok := urlVal.(string); ok {
					item, _, err := imageEditConvertURL(client, s)
					if err != nil {
						return err
					}
					out = append(out, item)
				}
			} else if u, ok := t["url"]; ok {
				if s, ok := u.(string); ok {
					item, _, err := imageEditConvertURL(client, s)
					if err != nil {
						return err
					}
					out = append(out, item)
				}
			} else if b64, ok := t["b64_json"].(string); ok && b64 != "" {
				item, err := imageEditReadJSONImage([]byte(b64), "image.png", "image/png")
				if err != nil {
					return err
				}
				out = append(out, item)
			} else if b64, ok := t["base64"].(string); ok && b64 != "" {
				item, err := imageEditReadJSONImage([]byte(b64), "image.png", "image/png")
				if err != nil {
					return err
				}
				out = append(out, item)
			}
		}
		return nil
	}

	for _, key := range []string{"images", "image", "image_url"} {
		val, ok := body[key]
		if !ok || val == nil {
			continue
		}
		switch arr := val.(type) {
		case []interface{}:
			for _, item := range arr {
				if err := appendValue(item); err != nil {
					return nil, err
				}
			}
		case string:
			if err := appendValue(arr); err != nil {
				return nil, err
			}
		case map[string]interface{}:
			if err := appendValue(arr); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// collectResponsesAPIParts 从 Responses API 风格的 input / content / messages 提取文本和图片。

func collectResponsesAPIParts(raw interface{}) (string, []string) {
	if raw == nil {
		return "", nil
	}
	var textParts []string
	var imageURLs []string

	appendFromContent := func(content interface{}) {
		switch c := content.(type) {
		case string:
			if strings.TrimSpace(c) != "" {
				textParts = append(textParts, c)
			}
		case []interface{}:
			for _, item := range c {
				part, ok := item.(map[string]interface{})
				if !ok {
					continue
				}
				partType := strings.ToLower(strings.TrimSpace(stringFromAny(part["type"])))
				switch partType {
				case "input_text", "text", "output_text":
					if s := stringFromAny(part["text"]); s != "" {
						textParts = append(textParts, s)
					}
				case "input_image", "image", "image_url":
					switch u := part["image_url"].(type) {
					case string:
						imageURLs = append(imageURLs, u)
					case map[string]interface{}:
						if s := stringFromAny(u["url"]); s != "" {
							imageURLs = append(imageURLs, s)
						}
					}
					if s := stringFromAny(part["url"]); s != "" {
						imageURLs = append(imageURLs, s)
					}
				}
			}
		}
	}

	switch v := raw.(type) {
	case string:
		textParts = append(textParts, v)
	case map[string]interface{}:
		appendFromContent(v["content"])
	case []interface{}:
		for _, item := range v {
			switch m := item.(type) {
			case string:
				textParts = append(textParts, m)
			case map[string]interface{}:
				partType := strings.ToLower(strings.TrimSpace(stringFromAny(m["type"])))
				if partType == "input_image" || partType == "image" || partType == "image_url" {
					switch u := m["image_url"].(type) {
					case string:
						imageURLs = append(imageURLs, u)
					case map[string]interface{}:
						if s := stringFromAny(u["url"]); s != "" {
							imageURLs = append(imageURLs, s)
						}
					}
					if s := stringFromAny(m["url"]); s != "" {
						imageURLs = append(imageURLs, s)
					}
					continue
				}
				appendFromContent(m["content"])
			}
		}
	}
	return strings.Join(textParts, "\n"), imageURLs
}

func stringFromAny(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// ─── /v1/images/edits 与 /v1/images/variations ───────────────────

func (h *ImageHandler) Edits(c *gin.Context) {
	h.runImageEditFlow(c, false)
}

func (h *ImageHandler) Variations(c *gin.Context) {
	h.runImageEditFlow(c, true)
}

func (h *ImageHandler) runImageEditFlow(c *gin.Context, asVariation bool) {
	contentType := strings.Split(c.GetHeader("Content-Type"), ";")[0]
	contentType = strings.ToLower(strings.TrimSpace(contentType))

	var imageSources []editImageInput
	var prompt, model, responseFormat string
	var n int
	var stream bool

	parseFormFields := func(promptVal, modelVal, nVal, responseFormatVal, streamVal string) {
		prompt = strings.TrimSpace(promptVal)
		model = strings.TrimSpace(modelVal)
		responseFormat = strings.TrimSpace(responseFormatVal)
		if nVal != "" {
			if v, err := strconv.Atoi(nVal); err == nil {
				n = v
			}
		}
		stream = isStreamTrue(streamVal)
	}

	if contentType == "application/json" {
		var body struct {
			Prompt         string                          `json:"prompt"`
			Model          string                          `json:"model"`
			N              int                             `json:"n"`
			Size           string                          `json:"size"`
			ResponseFormat string                          `json:"response_format"`
			Stream         bool                            `json:"stream"`
			Images         []officialtypes.ImageEditSource `json:"images,omitempty"`
			Image          *officialtypes.ImageEditSource  `json:"image,omitempty"`
			ImageURL       interface{}                     `json:"image_url,omitempty"`
			Input          interface{}                     `json:"input,omitempty"`
			Content        interface{}                     `json:"content,omitempty"`
			Messages       interface{}                     `json:"messages,omitempty"`
		}
		if err := c.BindJSON(&body); err != nil {
			apierrors.JSONError(c, 400, "invalid_request_error", "Request must be proper JSON", nil, err.Error())
			return
		}
		prompt = strings.TrimSpace(body.Prompt)
		model = body.Model
		n = body.N
		responseFormat = body.ResponseFormat
		stream = body.Stream

		client := bogdanfinn.NewStdClient()

		for _, src := range body.Images {
			item, _, err := imageEditConvertURL(client, src.ImageURL)
			if err != nil {
				apierrors.JSONError(c, 400, "invalid_request_error", "invalid image reference: "+err.Error(), apierrors.Param("images"), "invalid_image")
				return
			}
			if len(item.Data) > 0 {
				imageSources = append(imageSources, item)
			}
		}
		if body.Image != nil {
			item, _, err := imageEditConvertURL(client, body.Image.ImageURL)
			if err != nil {
				apierrors.JSONError(c, 400, "invalid_request_error", "invalid image reference: "+err.Error(), apierrors.Param("image"), "invalid_image")
				return
			}
			if len(item.Data) > 0 {
				imageSources = append(imageSources, item)
			}
		}
		if body.ImageURL != nil {
			switch t := body.ImageURL.(type) {
			case string:
				item, _, err := imageEditConvertURL(client, t)
				if err == nil && len(item.Data) > 0 {
					imageSources = append(imageSources, item)
				}
			case map[string]interface{}:
				if u, ok := t["url"].(string); ok {
					item, _, err := imageEditConvertURL(client, u)
					if err == nil && len(item.Data) > 0 {
						imageSources = append(imageSources, item)
					}
				}
			}
		}

		promptFromParts, imageParts := collectResponsesAPIParts(body.Input)
		if len(imageParts) == 0 {
			if p, imgs := collectResponsesAPIParts(body.Content); len(imgs) > 0 {
				promptFromParts = p
				imageParts = imgs
			}
		}
		if len(imageParts) == 0 {
			if p, imgs := collectResponsesAPIParts(body.Messages); len(imgs) > 0 {
				promptFromParts = p
				imageParts = imgs
			}
		}
		for _, p := range imageParts {
			item, _, err := imageEditConvertURL(client, p)
			if err != nil {
				apierrors.JSONError(c, 400, "invalid_request_error", "invalid image reference: "+err.Error(), apierrors.Param("input"), "invalid_image")
				return
			}
			if len(item.Data) > 0 {
				imageSources = append(imageSources, item)
			}
		}
		if prompt == "" {
			prompt = strings.TrimSpace(promptFromParts)
		}
	} else {
		form, err := c.MultipartForm()
		if err != nil {
			apierrors.JSONError(c, 400, "invalid_request_error", "Request must be multipart/form-data or application/json: "+err.Error(), nil, "invalid_multipart")
			return
		}
		parseFormFields(
			strings.TrimSpace(c.PostForm("prompt")),
			strings.TrimSpace(c.PostForm("model")),
			c.PostForm("n"),
			strings.TrimSpace(c.PostForm("response_format")),
			c.PostForm("stream"),
		)
		rawSources := make([]interface{}, 0, 4)
		for _, key := range []string{"image", "image[]", "images", "images[]"} {
			if vs, ok := form.File[key]; ok {
				for _, fh := range vs {
					rawSources = append(rawSources, fh)
				}
			}
		}
		if vs, ok := form.Value["image_url"]; ok {
			client := bogdanfinn.NewStdClient()
			for _, s := range vs {
				item, _, err := imageEditConvertURL(client, s)
				if err == nil && len(item.Data) > 0 {
					imageSources = append(imageSources, item)
				}
			}
		}
		imageSources = append(imageSources, normalizeImageEditImages(rawSources)...)
	}

	if asVariation {
		if prompt == "" {
			prompt = "Generate a variation of the provided image(s). Return only the generated image, not a text description."
		}
	}

	if len(imageSources) == 0 {
		apierrors.JSONError(c, 400, "invalid_request_error", "Missing required image input. Provide multipart 'image'/'images' field, or JSON 'image'/'images' field with image_url.", apierrors.Param("image"), "missing_required_parameter")
		return
	}
	if !asVariation && prompt == "" {
		apierrors.JSONError(c, 400, "invalid_request_error", "Missing required parameter: prompt", apierrors.Param("prompt"), "missing_required_parameter")
		return
	}
	if n <= 0 {
		n = 1
	}
	if n > 10 {
		n = 10
	}
	if responseFormat == "" {
		responseFormat = "b64_json"
	}
	stream = requestStreamFlag(c, stream)

	account, _, err := resolveAccount(c, h.accountPool, h.cfg, true)
	if err != nil {
		apierrors.JSONError(c, 400, "authorization_error", err.Error(), apierrors.Param("Authorization"), 400)
		return
	}
	if account == nil || account.Token == "" {
		apierrors.NotFoundAccount(c)
		return
	}
	if !account.Type.Satisfies(accounts.CapImageGenerate) {
		c.JSON(403, gin.H{"error": "Images API requires a logged-in ChatGPT account."})
		return
	}
	if asVariation {
		if !account.Type.Satisfies(accounts.CapImageVariation) {
			c.JSON(403, gin.H{"error": "Image variation requires a logged-in ChatGPT account."})
			return
		}
	} else {
		if !account.Type.Satisfies(accounts.CapImageEdit) {
			c.JSON(403, gin.H{"error": "Image edit requires a logged-in ChatGPT account."})
			return
		}
	}

	proxyUrl := account.Proxy
	client := setupClientWithProxy(proxyUrl)
	client.SetCookies("https://chatgpt.com", chatgpt.BasicCookies)
	turnStile, status, err := chatgpt.InitSentinel(client, account, proxyUrl, 0)
	if err != nil {
		if status == http.StatusUnauthorized {
			h.accountPool.ReportFailure(account)
		}
		c.JSON(status, gin.H{
			"message": err.Error(),
			"type":    "InitTurnStile_request_error",
			"param":   err,
			"code":    status,
		})
		return
	}

	if stream {
		writeImageStreamHeader(c)
	}

	// 1) 上传所有源图
	references := make([]chatgpt.ImageEditReference, 0, len(imageSources))
	for idx, src := range imageSources {
		uploaded, upStatus, upErr := chatgpt.UploadFile(client, account, proxyUrl, src.Filename, src.ContentType, src.Data)
		if upErr != nil {
			if stream {
				writeImageStreamEvent(c, "image.generation.error", gin.H{
					"object":  "image.generation.error",
					"index":   idx,
					"message": "upload image failed: " + upErr.Error(),
				})
				writeImageStreamDone(c)
				return
			}
			apierrors.JSONError(c, upStatus, "image_upload_error", "upload image failed: "+upErr.Error(), apierrors.Param(fmt.Sprintf("image[%d]", idx)), "image_upload_error")
			return
		}
		references = append(references, chatgpt.ImageEditReference{
			FileID:   uploaded.FileID,
			Width:    uploaded.Width,
			Height:   uploaded.Height,
			Size:     int(uploaded.Bytes),
			MimeType: uploaded.MimeType,
			Filename: uploaded.Filename,
		})
	}

	// 2) 调起带 reference 的 image conversation,循环 n 次以满足 N
	var data []officialtypes.ImageGenerationData
	for i := 0; i < n; i++ {
		if stream {
			writeImageStreamEvent(c, "image.generation.chunk", imageStreamChunk{
				Object:       "image.generation.chunk",
				Index:        i,
				Total:        n,
				Created:      0,
				Model:        model,
				ProgressText: fmt.Sprintf("Generating image %d/%d ...", i+1, n),
			})
		}
		imageResults, upstreamText, err := chatgpt.GeneratePictureConversationImagesWithReferences(client, account, turnStile, prompt, model, proxyUrl, references)
		if err != nil {
			if stream {
				writeImageStreamEvent(c, "image.generation.error", gin.H{
					"object":  "image.generation.error",
					"index":   i,
					"total":   n,
					"message": err.Error(),
				})
				writeImageStreamDone(c)
				return
			}
			apierrors.JSONError(c, 500, "image_generation_error", err.Error(), nil, "image_generation_error")
			return
		}
		for _, imageResult := range imageResults {
			item := officialtypes.ImageGenerationData{
				RevisedPrompt: prompt,
			}
			if responseFormat == "b64_json" {
				if imageResult.B64JSON != "" {
					item.B64JSON = imageResult.B64JSON
				} else if imageResult.URL != "" {
					imageBytes, err := chatgpt.DownloadImageBytes(client, imageResult.URL, account)
					if err != nil {
						if stream {
							writeImageStreamEvent(c, "image.generation.error", gin.H{
								"object":  "image.generation.error",
								"index":   i,
								"total":   n,
								"message": err.Error(),
							})
							writeImageStreamDone(c)
							return
						}
						apierrors.JSONError(c, 500, "image_download_error", err.Error(), nil, "image_download_error")
						return
					}
					item.B64JSON = base64.StdEncoding.EncodeToString(imageBytes)
				}
			} else {
				item.URL = imageResult.URL
				if item.URL == "" && imageResult.B64JSON != "" {
					item.B64JSON = imageResult.B64JSON
				}
			}
			data = append(data, item)
			if stream {
				writeImageStreamEvent(c, "image.generation.result", imageStreamResult{
					Object:  "image.generation.result",
					Index:   len(data) - 1,
					Total:   n,
					Created: 0,
					Model:   model,
					Data:    []officialtypes.ImageGenerationData{item},
				})
			}
			if len(data) >= n {
				break
			}
		}
		if len(imageResults) == 0 && upstreamText != "" {
			if stream {
				writeImageStreamEvent(c, "image.generation.error", gin.H{
					"object":  "image.generation.error",
					"index":   i,
					"total":   n,
					"message": "No image result found in response: " + upstreamText,
				})
				writeImageStreamDone(c)
				return
			}
			apierrors.JSONError(c, 500, "image_generation_error", "No image result found in response: "+upstreamText, nil, "image_generation_error")
			return
		}
		if len(data) >= n {
			break
		}
	}
	if len(data) == 0 {
		if stream {
			writeImageStreamEvent(c, "image.generation.error", gin.H{
				"object":  "image.generation.error",
				"message": "No image result found in response",
			})
			writeImageStreamDone(c)
			return
		}
		apierrors.JSONError(c, 500, "image_generation_error", "No image result found in response", nil, "image_generation_error")
		return
	}
	if stream {
		writeImageStreamEvent(c, "image.generation.completed", imageStreamCompleted{
			Object:  "image.generation.completed",
			Created: 0,
			Model:   model,
			Data:    data,
		})
		writeImageStreamDone(c)
		return
	}
	if asVariation {
		c.JSON(200, officialtypes.NewImageVariationResponse(data))
	} else {
		c.JSON(200, officialtypes.NewImageEditResponse(data))
	}
}
