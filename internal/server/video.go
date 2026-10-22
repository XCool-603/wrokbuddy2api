package server

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/upstream"
)

// ClientVideoGenRequest 客户端发来的通用视频生成请求。
type ClientVideoGenRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	NegativePrompt string `json:"negative_prompt"`
	Image          string `json:"image"`
	ImageURL       string `json:"image_url"`
	InputImage     string `json:"input_image"`
	LastImage      string `json:"last_image"`
	LastImageURL   string `json:"last_image_url"`
	Seconds        int    `json:"seconds"`
	Duration       int    `json:"duration"`
	Resolution     string `json:"resolution"`
	AspectRatio    string `json:"aspect_ratio"`
	EnableAudio    *bool  `json:"enable_audio"`
	Watermark      *bool  `json:"watermark"`
	Wait           bool   `json:"wait"`
	Sync           bool   `json:"sync"`
}

// videoGenerations 处理 POST /v1/videos/generations 与 POST /v2/videos/generations。
func (h *Handler) videoGenerations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return
	}

	var req ClientVideoGenRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON request body: "+err.Error())
		return
	}

	// 统一处理图片字段（支持 image / image_url / input_image）
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

	// 模型自适应推断：若有图片默认 kling-v3-i2v（图生视频），否则 kling-v3-t2v（文生视频）
	model := strings.TrimSpace(req.Model)
	if model == "" || model == "kling" || model == "kling-v3" {
		if imgURL != "" || lastImgURL != "" {
			model = "kling-v3-i2v"
		} else {
			model = "kling-v3-t2v"
		}
	}

	// 校验提示词
	prompt := strings.TrimSpace(req.Prompt)
	if imgURL == "" && lastImgURL == "" && prompt == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "prompt is required for text-to-video")
		return
	}

	// 时长控制（3-15秒，默认 5 秒）
	seconds := req.Seconds
	if seconds <= 0 {
		seconds = req.Duration
	}
	if seconds <= 0 {
		seconds = 5
	}
	if seconds < 3 {
		seconds = 3
	}
	if seconds > 15 {
		seconds = 15
	}

	// 分辨率（720P / 1080P，默认 1080P）
	res := strings.ToUpper(strings.TrimSpace(req.Resolution))
	if res != "720P" && res != "1080P" {
		res = "1080P"
	}

	// 画幅比（16:9 / 9:16 / 1:1，默认 16:9）
	ar := strings.TrimSpace(req.AspectRatio)
	if ar != "16:9" && ar != "9:16" && ar != "1:1" {
		ar = "16:9"
	}

	// 音频开关（默认开启）
	enableAudio := true
	if req.EnableAudio != nil {
		enableAudio = *req.EnableAudio
	}

	// 水印开关（默认开启）
	watermark := true
	if req.Watermark != nil {
		watermark = *req.Watermark
	}

	// 组装上游请求
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

	// 从账号池中挑选可用账号
	acct := h.cfg.Pool.Pick(model)
	if acct == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", "no available account in pool for video generation")
		return
	}

	clientIP := upstream.ExtractClientIP(r)
	resp, err := h.cfg.Upstream.CreateVideoTask(r.Context(), acct, upReq, clientIP)
	if err != nil {
		log.Printf("[Video] CreateVideoTask failed (account=%s, model=%s): %v", acct.UID, model, err)
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "failed to create video task: "+err.Error())
		return
	}

	taskID := resp.Data.ID

	// 同步等待模式（支持 query ?wait=true / ?sync=true 或 body wait=true）
	syncWait := req.Wait || req.Sync || r.URL.Query().Get("wait") == "true" || r.URL.Query().Get("sync") == "true"
	if !syncWait {
		// 异步模式：立即返回任务 ID
		writeJSON(w, http.StatusOK, map[string]any{
			"id":      taskID,
			"object":  "video.task",
			"created": time.Now().Unix(),
			"model":   model,
			"status":  "processing",
			"task_id": taskID,
		})
		return
	}

	// 同步轮询模式：等待任务直到完成或超时（最多等待 300 秒）
	ctx := r.Context()
	ticker := time.NewTicker(4 * time.Second)
	defer ticker.Stop()

	timeout := time.After(300 * time.Second)

	for {
		select {
		case <-ctx.Done():
			writeOpenAIError(w, http.StatusRequestTimeout, "client_closed", "client disconnected while waiting for video generation")
			return
		case <-timeout:
			writeOpenAIError(w, http.StatusGatewayTimeout, "timeout", "video generation timed out waiting for completion")
			return
		case <-ticker.C:
			qResp, err := h.cfg.Upstream.QueryVideoTask(ctx, acct, taskID, clientIP)
			if err != nil {
				log.Printf("[Video] QueryVideoTask tick error (taskID=%s): %v", taskID, err)
				continue
			}

			if qResp.Data.Status == "completed" {
				writeJSON(w, http.StatusOK, map[string]any{
					"id":      taskID,
					"object":  "video.generation",
					"created": time.Now().Unix(),
					"model":   model,
					"status":  "completed",
					"data":    qResp.Data.Data,
					"usage":   qResp.Usage,
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

// VideoTaskQueryPayload POST /v1/videos/tasks 查询请求体。
type VideoTaskQueryPayload struct {
	TaskID string `json:"task_id"`
	ID     string `json:"id"`
}

// videoTasksPost 处理 POST /v1/videos/tasks 与 POST /v2/videos/tasks。
func (h *Handler) videoTasksPost(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "failed to read body")
		return
	}

	var payload VideoTaskQueryPayload
	_ = json.Unmarshal(bodyBytes, &payload)

	taskID := payload.TaskID
	if taskID == "" {
		taskID = payload.ID
	}
	if taskID == "" {
		taskID = r.URL.Query().Get("task_id")
	}
	if taskID == "" {
		taskID = r.URL.Query().Get("id")
	}

	h.handleQueryVideoTask(w, r, taskID)
}

// videoTaskGet 处理 GET /v1/videos/tasks/{id} 与 GET /v1/videos/tasks。
func (h *Handler) videoTaskGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	taskID := r.PathValue("id")
	if taskID == "" {
		taskID = r.URL.Query().Get("task_id")
	}
	if taskID == "" {
		taskID = r.URL.Query().Get("id")
	}

	h.handleQueryVideoTask(w, r, taskID)
}

func (h *Handler) handleQueryVideoTask(w http.ResponseWriter, r *http.Request, taskID string) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "task_id is required")
		return
	}

	acct := h.cfg.Pool.Pick("")
	if acct == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", "no available account in pool to query video task")
		return
	}

	clientIP := upstream.ExtractClientIP(r)
	qResp, err := h.cfg.Upstream.QueryVideoTask(r.Context(), acct, taskID, clientIP)
	if err != nil {
		log.Printf("[Video] QueryVideoTask failed (taskID=%s): %v", taskID, err)
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "failed to query video task: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":      taskID,
		"object":  "video.task",
		"status":  qResp.Data.Status,
		"data":    qResp.Data.Data,
		"error":   qResp.Data.Error,
		"usage":   qResp.Usage,
		"task_id": taskID,
	})
}
