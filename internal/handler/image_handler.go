package handler

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"aurora/internal/accounts"
	"aurora/internal/apierrors"
	"aurora/internal/chatgpt"
	"aurora/internal/config"
	officialtypes "aurora/typings/official"

	"github.com/gin-gonic/gin"
)

type ImageHandler struct {
	accountPool *accounts.Pool
	cfg         *config.Config
}

func NewImageHandler(pool *accounts.Pool, cfg *config.Config) *ImageHandler {
	return &ImageHandler{accountPool: pool, cfg: cfg}
}

// ─── Image stream types ──────────────────────────────────────────

type imageStreamChunk struct {
	Object            string `json:"object"`
	Index             int    `json:"index"`
	Total             int    `json:"total"`
	Created           int64  `json:"created"`
	ProgressText      string `json:"progress_text,omitempty"`
	UpstreamEventType string `json:"upstream_event_type,omitempty"`
	Model             string `json:"model,omitempty"`
	AccountEmail      string `json:"_account_email,omitempty"`
	ConversationID    string `json:"_conversation_id,omitempty"`
}

type imageStreamResult struct {
	Object  string                              `json:"object"`
	Index   int                                 `json:"index"`
	Total   int                                 `json:"total"`
	Created int64                               `json:"created"`
	Model   string                              `json:"model,omitempty"`
	Data    []officialtypes.ImageGenerationData `json:"data"`
}

type imageStreamCompleted struct {
	Object  string                              `json:"object"`
	Created int64                               `json:"created"`
	Model   string                              `json:"model,omitempty"`
	Data    []officialtypes.ImageGenerationData `json:"data"`
}

func writeImageStreamHeader(c *gin.Context) {
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(200)
}

func writeImageStreamEvent(c *gin.Context, event string, payload interface{}) bool {
	data, err := json.Marshal(payload)
	if err != nil {
		return false
	}
	if event != "" {
		if _, err := c.Writer.WriteString("event: " + event + "\n"); err != nil {
			return false
		}
	}
	if _, err := c.Writer.WriteString("data: "); err != nil {
		return false
	}
	if _, err := c.Writer.Write(data); err != nil {
		return false
	}
	if _, err := c.Writer.WriteString("\n\n"); err != nil {
		return false
	}
	c.Writer.Flush()
	return true
}

func writeImageStreamDone(c *gin.Context) bool {
	if _, err := c.Writer.WriteString("data: [DONE]\n\n"); err != nil {
		return false
	}
	c.Writer.Flush()
	return true
}

// requestStreamFlag 解析 stream 参数,支持 JSON body 的 stream 字段或 ?stream=true 查询参数。

func requestStreamFlag(c *gin.Context, jsonStream bool) bool {
	if jsonStream {
		return true
	}
	if v := strings.ToLower(strings.TrimSpace(c.Query("stream"))); v == "true" || v == "1" || v == "yes" {
		return true
	}
	if v := strings.ToLower(strings.TrimSpace(c.PostForm("stream"))); v == "true" || v == "1" || v == "yes" {
		return true
	}
	return false
}

// isStreamTrue 把任意形式的 stream 字段值转换为 bool。

func isStreamTrue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}

// ─── /v1/images/generations ──────────────────────────────────────

func (h *ImageHandler) Generations(c *gin.Context) {
	var imageRequest officialtypes.ImageGenerationRequest
	err := c.BindJSON(&imageRequest)
	if err != nil {
		apierrors.JSONError(c, 400, "invalid_request_error", "Request must be proper JSON", nil, err.Error())
		return
	}
	if imageRequest.Prompt == "" {
		apierrors.JSONError(c, 400, "invalid_request_error", "Missing required parameter: prompt", apierrors.Param("prompt"), "missing_required_parameter")
		return
	}
	if imageRequest.N <= 0 {
		imageRequest.N = 1
	}
	if imageRequest.N > 10 {
		imageRequest.N = 10
	}
	if imageRequest.ResponseFormat == "" {
		imageRequest.ResponseFormat = "b64_json"
	}

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

	stream := requestStreamFlag(c, imageRequest.Stream)
	if stream {
		writeImageStreamHeader(c)
	}

	var data []officialtypes.ImageGenerationData
	for i := 0; i < imageRequest.N; i++ {
		if stream {
			writeImageStreamEvent(c, "image.generation.chunk", imageStreamChunk{
				Object:       "image.generation.chunk",
				Index:        i,
				Total:        imageRequest.N,
				Created:      0,
				Model:        imageRequest.Model,
				ProgressText: fmt.Sprintf("Generating image %d/%d ...", i+1, imageRequest.N),
			})
		}
		imageResults, upstreamText, err := chatgpt.GeneratePictureConversationImages(client, account, turnStile, imageRequest.Prompt, imageRequest.Model, proxyUrl)
		if err != nil {
			if stream {
				writeImageStreamEvent(c, "image.generation.error", gin.H{
					"object":  "image.generation.error",
					"index":   i,
					"total":   imageRequest.N,
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
				RevisedPrompt: imageRequest.Prompt,
			}
			if imageRequest.ResponseFormat == "b64_json" {
				if imageResult.B64JSON != "" {
					item.B64JSON = imageResult.B64JSON
				} else if imageResult.URL != "" {
					imageBytes, err := chatgpt.DownloadImageBytes(client, imageResult.URL, account)
					if err != nil {
						if stream {
							writeImageStreamEvent(c, "image.generation.error", gin.H{
								"object":  "image.generation.error",
								"index":   i,
								"total":   imageRequest.N,
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
					Total:   imageRequest.N,
					Created: 0,
					Model:   imageRequest.Model,
					Data:    []officialtypes.ImageGenerationData{item},
				})
			}
			if len(data) >= imageRequest.N {
				break
			}
		}
		if len(imageResults) == 0 && upstreamText != "" {
			if stream {
				writeImageStreamEvent(c, "image.generation.error", gin.H{
					"object":  "image.generation.error",
					"index":   i,
					"total":   imageRequest.N,
					"message": "No image result found in response: " + upstreamText,
				})
				writeImageStreamDone(c)
				return
			}
			apierrors.JSONError(c, 500, "image_generation_error", "No image result found in response: "+upstreamText, nil, "image_generation_error")
			return
		}
		if len(data) >= imageRequest.N {
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
			Model:   imageRequest.Model,
			Data:    data,
		})
		writeImageStreamDone(c)
		return
	}
	c.JSON(200, officialtypes.NewImageGenerationResponse(data))
}

// ─── Image Edit / Variation types ────────────────────────────────

// editImageInput 一张待编辑/变体使用的源图,支持 multipart 文件上传与 JSON 引用。

var imageEditImageReferenceFields = map[string]bool{
	"image":       true,
	"image[]":     true,
	"images":      true,
	"images[]":    true,
	"image_url":   true,
	"image_url[]": true,
}
