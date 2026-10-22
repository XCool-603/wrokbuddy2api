package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/usermgr"
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

// normalizeVideoModel 统一解析并归一化视频模型名称（支持 kling、keling、minimax、hailuo 及其中文别名）。
func normalizeVideoModel(reqModel string, hasImage bool) string {
	m := strings.ToLower(strings.TrimSpace(reqModel))
	if idx := strings.Index(m, ":"); idx != -1 {
		m = m[idx+1:]
	}

	if strings.Contains(m, "minimax") || strings.Contains(m, "hailuo") || strings.Contains(m, "海螺") || strings.Contains(m, "h3") {
		return "minimax-video-h3"
	}

	if strings.Contains(m, "i2v") || strings.Contains(m, "image") {
		return "kling-v3-i2v"
	}
	if strings.Contains(m, "t2v") || strings.Contains(m, "text") {
		return "kling-v3-t2v"
	}

	if hasImage {
		return "kling-v3-i2v"
	}
	return "kling-v3-t2v"
}

// createVideoTaskWithRetry 在账号池中带重试与多账号轮转提交视频任务，自动处理 401 token 过期与刷新。
func (h *Handler) createVideoTaskWithRetry(ctx context.Context, model string, upReq *upstream.VideoGenRequest, clientIP string, reqOwner string) (*upstream.VideoGenResponse, *auth.Auth, error) {
	maxAttempts := h.cfg.MaxRotate
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	tried := map[string]bool{}
	var lastErr error

	for i := 0; i < maxAttempts; i++ {
		// 优先在 CN 域挑选（可灵/海螺在 CN 域稳定供给）
		acct := h.cfg.Pool.PickExcludingForRealmAndOwner(tried, model, "cn", reqOwner)
		if acct == nil {
			acct = h.cfg.Pool.PickExcludingForRealmAndOwner(tried, model, "", reqOwner)
		}
		if acct == nil {
			break
		}
		tried[acct.UID] = true

		resp, err := h.cfg.Upstream.CreateVideoTask(ctx, acct, upReq, clientIP)
		if err == nil && resp != nil && resp.Data.ID != "" {
			h.cfg.Pool.NoteSuccess(acct.UID)
			return resp, acct, nil
		}

		lastErr = err
		h.cfg.Pool.NoteError(acct.UID)
		log.Printf("[Video] CreateVideoTask attempt %d failed (acct=%s, model=%s): %v", i+1, acct.UID, model, err)

		errStr := ""
		if err != nil {
			errStr = err.Error()
		}
		// 若为 401 凭证过期，自动尝试 RefreshToken
		if strings.Contains(errStr, "401") || strings.Contains(errStr, "token") {
			if refErr := h.cfg.Upstream.RefreshToken(acct); refErr == nil {
				// 刷新成功后重试该账号
				respRetry, errRetry := h.cfg.Upstream.CreateVideoTask(ctx, acct, upReq, clientIP)
				if errRetry == nil && respRetry != nil && respRetry.Data.ID != "" {
					h.cfg.Pool.NoteSuccess(acct.UID)
					return respRetry, acct, nil
				}
				lastErr = errRetry
			}
		}
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("no healthy account available in pool for video generation")
	}
	return nil, nil, lastErr
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

	// 模型自适应与归一化（支持快手可灵 Kling/Keling 与 MiniMax-H3 海螺双视频方案）
	model := normalizeVideoModel(req.Model, imgURL != "" || lastImgURL != "")
	isMiniMax := strings.Contains(model, "minimax")

	// 校验提示词
	prompt := strings.TrimSpace(req.Prompt)
	if imgURL == "" && lastImgURL == "" && prompt == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "prompt is required for text-to-video")
		return
	}

	// 时长控制（默认 5 秒）
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

	// 分辨率
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

	// 画幅比
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

	reqOwner := ""
	if u, ok := r.Context().Value(userContextKey).(*usermgr.User); ok && u != nil {
		if u.Role != usermgr.RoleAdmin {
			reqOwner = u.ID
		}
	}

	clientIP := upstream.ExtractClientIP(r)
	resp, acct, err := h.createVideoTaskWithRetry(r.Context(), model, upReq, clientIP, reqOwner)
	if err != nil {
		log.Printf("[Video] createVideoTaskWithRetry failed (model=%s): %v", model, err)
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
				} else {
					errMsg = formatVideoError(errMsg)
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
