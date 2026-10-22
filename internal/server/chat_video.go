package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/upstream"
)

// isVideoModel 判断指定模型名称是否属于视频生成大模型。
func isVideoModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if idx := strings.Index(m, ":"); idx != -1 {
		m = m[idx+1:]
	}
	return strings.HasPrefix(m, "kling") ||
		strings.Contains(m, "minimax-video") ||
		strings.Contains(m, "hailuo-video") ||
		strings.HasSuffix(m, "-video") ||
		strings.Contains(m, "video-v") ||
		strings.Contains(m, "video-h")
}

// parseChatVideoRequest 从 chat/completions 请求体中解析视频生成所需的 prompt、参考图与规格参数。
func parseChatVideoRequest(body []byte) (req ClientVideoGenRequest, err error) {
	_ = json.Unmarshal(body, &req)

	var chatPayload struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
		Prompt         string `json:"prompt"`
		ImageURL       string `json:"image_url"`
		LastImageURL   string `json:"last_image_url"`
		Seconds        int    `json:"seconds"`
		Duration       int    `json:"duration"`
		Resolution     string `json:"resolution"`
		AspectRatio    string `json:"aspect_ratio"`
		EnableAudio    *bool  `json:"enable_audio"`
		NegativePrompt string `json:"negative_prompt"`
	}

	if err := json.Unmarshal(body, &chatPayload); err != nil {
		return req, err
	}

	if req.Prompt == "" && chatPayload.Prompt != "" {
		req.Prompt = chatPayload.Prompt
	}
	if req.ImageURL == "" && chatPayload.ImageURL != "" {
		req.ImageURL = chatPayload.ImageURL
	}
	if req.LastImageURL == "" && chatPayload.LastImageURL != "" {
		req.LastImageURL = chatPayload.LastImageURL
	}

	// 从 messages 提取用户最后一条文本 prompt，以及包含的图片参考
	var userTexts []string
	var images []string

	for _, msg := range chatPayload.Messages {
		if msg.Role == "user" || msg.Role == "" {
			switch c := msg.Content.(type) {
			case string:
				txt := strings.TrimSpace(c)
				if txt != "" {
					userTexts = append(userTexts, txt)
				}
			case []any:
				for _, part := range c {
					if pm, ok := part.(map[string]any); ok {
						if pType, ok := pm["type"].(string); ok {
							if pType == "text" {
								if t, ok := pm["text"].(string); ok && strings.TrimSpace(t) != "" {
									userTexts = append(userTexts, strings.TrimSpace(t))
								}
							} else if pType == "image_url" {
								if im, ok := pm["image_url"].(map[string]any); ok {
									if u, ok := im["url"].(string); ok && strings.TrimSpace(u) != "" {
										images = append(images, strings.TrimSpace(u))
									}
								}
							} else if pType == "image" {
								if u, ok := pm["image"].(string); ok && strings.TrimSpace(u) != "" {
									images = append(images, strings.TrimSpace(u))
								}
							}
						}
					}
				}
			}
		}
	}

	if req.Prompt == "" && len(userTexts) > 0 {
		req.Prompt = userTexts[len(userTexts)-1]
	}

	if req.ImageURL == "" && len(images) > 0 {
		req.ImageURL = images[0]
	}
	if req.LastImageURL == "" && len(images) > 1 {
		req.LastImageURL = images[1]
	}

	return req, nil
}

// chatVideoBridge 在 POST /v1/chat/completions 中为视频大模型提供对话窗口直出视频能力。
func (h *Handler) chatVideoBridge(w http.ResponseWriter, r *http.Request, body []byte, reqModel string, stream bool) {
	req, err := parseChatVideoRequest(body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "failed to parse chat video request: "+err.Error())
		return
	}

	imgURL := req.ImageURL
	if imgURL == "" {
		imgURL = req.Image
	}
	if imgURL == "" {
		imgURL = req.InputImage
	}
	lastImgURL := req.LastImageURL
	if lastImgURL == "" {
		lastImgURL = req.LastImage
	}

	model := strings.TrimSpace(reqModel)
	if model == "" {
		model = req.Model
	}
	isMiniMax := strings.Contains(strings.ToLower(model), "minimax") ||
		strings.Contains(strings.ToLower(model), "hailuo") ||
		strings.Contains(strings.ToLower(model), "h3")

	if isMiniMax {
		model = "minimax-video-h3"
	} else if model == "" || model == "kling" || model == "kling-v3" || model == "kling-video" || model == "kling-video-v3" {
		if imgURL != "" || lastImgURL != "" {
			model = "kling-v3-i2v"
		} else {
			model = "kling-v3-t2v"
		}
	}

	prompt := strings.TrimSpace(req.Prompt)
	if imgURL == "" && lastImgURL == "" && prompt == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "prompt is required for video generation")
		return
	}

	seconds := req.Seconds
	if seconds <= 0 {
		seconds = req.Duration
	}
	if seconds <= 0 {
		seconds = 5
	}
	if isMiniMax {
		if seconds < 4 {
			seconds = 4
		}
		if seconds > 15 {
			seconds = 15
		}
	} else {
		if seconds < 3 {
			seconds = 3
		}
		if seconds > 15 {
			seconds = 15
		}
	}

	res := strings.ToUpper(strings.TrimSpace(req.Resolution))
	if isMiniMax {
		if res != "768P" && res != "2K" {
			res = "768P"
		}
	} else {
		if res != "720P" && res != "1080P" && res != "4K" {
			res = "1080P"
		}
	}

	ar := strings.ToLower(strings.TrimSpace(req.AspectRatio))
	if isMiniMax {
		validAR := map[string]bool{"adaptive": true, "21:9": true, "16:9": true, "4:3": true, "1:1": true, "3:4": true, "9:16": true}
		if !validAR[ar] {
			ar = "adaptive"
		}
	} else {
		if ar != "16:9" && ar != "9:16" && ar != "1:1" {
			ar = "16:9"
		}
	}

	enableAudio := true
	if req.EnableAudio != nil {
		enableAudio = *req.EnableAudio
	}
	watermark := true
	if req.Watermark != nil {
		watermark = *req.Watermark
	}

	upReq := &upstream.VideoGenRequest{
		Model:          model,
		Prompt:         prompt,
		NegativePrompt: req.NegativePrompt,
		Seconds:        seconds,
		ImageURL:       imgURL,
		LastImageURL:   lastImgURL,
		Watermark:      &watermark,
		ExtraParams: &upstream.VideoExtraParams{
			Resolution:  res,
			AspectRatio: ar,
			EnableAudio:  &enableAudio,
		},
	}

	acct := h.cfg.Pool.Pick(model)
	if acct == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", "no available account in pool for video generation")
		return
	}

	clientIP := upstream.ExtractClientIP(r)
	resp, err := h.cfg.Upstream.CreateVideoTask(r.Context(), acct, upReq, clientIP)
	if err != nil {
		log.Printf("[VideoBridge] CreateVideoTask failed (account=%s, model=%s): %v", acct.UID, model, err)
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "failed to create video task: "+err.Error())
		return
	}

	taskID := resp.Data.ID
	ctx := r.Context()
	chunkID := fmt.Sprintf("chatcmpl-vid-%s", taskID)

	// 流式直出（SSE）
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
			return
		}

		sendChunk := func(content string, role string, finish *string) {
			delta := map[string]any{}
			if role != "" {
				delta["role"] = role
			}
			if content != "" {
				delta["content"] = content
			}
			chunk := map[string]any{
				"id":      chunkID,
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   model,
				"choices": []map[string]any{
					{
						"index":         0,
						"delta":         delta,
						"finish_reason": finish,
					},
				},
			}
			bs, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", bs)
			flusher.Flush()
		}

		initMsg := fmt.Sprintf("🎬 **专属视频模型直出中** (`%s`)\n\n> **提示词**: %s\n> **规格**: 时长 %ds | 分辨率 %s | 画幅 %s\n\n⏳ 任务已提交至渲染集群 (Task ID: `%s`)，正在生成中，请稍候", model, prompt, seconds, res, ar, taskID)
		sendChunk(initMsg, "assistant", nil)

		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		timeout := time.After(300 * time.Second)

		for {
			select {
			case <-ctx.Done():
				return
			case <-timeout:
				sendChunk("\n\n⚠️ **视频生成超时**（耗时超过 5 分钟，后台任务可能仍在运行，请稍后通过任务 ID 查询）。\n", "", nil)
				stop := "stop"
				sendChunk("", "", &stop)
				fmt.Fprint(w, "data: [DONE]\n\n")
				flusher.Flush()
				return
			case <-ticker.C:
				qResp, err := h.cfg.Upstream.QueryVideoTask(ctx, acct, taskID, clientIP)
				if err != nil {
					log.Printf("[VideoBridge] QueryVideoTask tick error (taskID=%s): %v", taskID, err)
					sendChunk(" ·", "", nil)
					continue
				}

				if qResp.Data.Status == "completed" {
					var sb strings.Builder
					sb.WriteString("\n\n✅ **视频渲染完成！**\n\n")
					for _, item := range qResp.Data.Data {
						if item.URL != "" {
							sb.WriteString(fmt.Sprintf("🎥 **视频直链**: [点击播放 / 下载 MP4](%s)\n\n", item.URL))
							sb.WriteString(fmt.Sprintf("<video src=\"%s\" controls autoplay loop style=\"max-width:100%%;border-radius:8px;\"></video>\n\n", item.URL))
						}
					}
					sendChunk(sb.String(), "", nil)
					stop := "stop"
					sendChunk("", "", &stop)
					fmt.Fprint(w, "data: [DONE]\n\n")
					flusher.Flush()
					return
				}

				if qResp.Data.Status == "failed" {
					errMsg := qResp.Data.Error.Message
					if errMsg == "" {
						errMsg = "上游渲染失败，请调整提示词后重试"
					}
					sendChunk(fmt.Sprintf("\n\n❌ **视频生成失败**: %s\n", errMsg), "", nil)
					stop := "stop"
					sendChunk("", "", &stop)
					fmt.Fprint(w, "data: [DONE]\n\n")
					flusher.Flush()
					return
				}

				// 心跳打点推进，防止客户端长等待超时断连
				sendChunk(" ·", "", nil)
			}
		}
	}

	// 非流式（同步等待直接返回完整响应）
	ticker := time.NewTicker(4 * time.Second)
	defer ticker.Stop()
	timeout := time.After(300 * time.Second)

	for {
		select {
		case <-ctx.Done():
			writeOpenAIError(w, http.StatusRequestTimeout, "client_closed", "client disconnected while waiting for video")
			return
		case <-timeout:
			writeOpenAIError(w, http.StatusGatewayTimeout, "timeout", "video generation timed out")
			return
		case <-ticker.C:
			qResp, err := h.cfg.Upstream.QueryVideoTask(ctx, acct, taskID, clientIP)
			if err != nil {
				continue
			}

			if qResp.Data.Status == "completed" {
				var sb strings.Builder
				sb.WriteString(fmt.Sprintf("🎬 **视频生成成功！** (`%s`)\n\n", model))
				for _, item := range qResp.Data.Data {
					if item.URL != "" {
						sb.WriteString(fmt.Sprintf("🎥 **视频直链**: [点击播放 / 下载 MP4](%s)\n\n", item.URL))
						sb.WriteString(fmt.Sprintf("<video src=\"%s\" controls autoplay loop style=\"max-width:100%%;border-radius:8px;\"></video>\n\n", item.URL))
					}
				}
				writeJSON(w, http.StatusOK, map[string]any{
					"id":      chunkID,
					"object":  "chat.completion",
					"created": time.Now().Unix(),
					"model":   model,
					"choices": []map[string]any{
						{
							"index": 0,
							"message": map[string]any{
								"role":    "assistant",
								"content": sb.String(),
							},
							"finish_reason": "stop",
						},
					},
					"usage": map[string]any{
						"prompt_tokens":     10,
						"completion_tokens": 100,
						"total_tokens":      110,
					},
				})
				return
			}

			if qResp.Data.Status == "failed" {
				errMsg := qResp.Data.Error.Message
				if errMsg == "" {
					errMsg = "video generation failed upstream"
				}
				writeOpenAIError(w, http.StatusInternalServerError, "video_generation_failed", errMsg)
				return
			}
		}
	}
}
