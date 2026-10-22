// Package upstream 视频生成接口封装（快手可灵 Kling 3.0 文生视频 / 图生视频）。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/session"
)

// VideoGenRequest 视频生成请求参数。
type VideoGenRequest struct {
	Model          string            `json:"model,omitempty"`           // kling-v3-t2v / kling-v3-i2v
	Prompt         string            `json:"prompt"`                    // 文本提示词
	NegativePrompt string            `json:"negative_prompt,omitempty"` // 负向提示词
	Seconds        int               `json:"seconds,omitempty"`         // 视频时长（3-15秒，默认5）
	Watermark      *bool             `json:"watermark,omitempty"`       // 水印，默认 true
	ImageURL       string            `json:"image_url,omitempty"`       // 图生视频首帧（Data URL 或 HTTP URL）
	LastImageURL   string            `json:"last_image_url,omitempty"`  // 图生视频尾帧
	ExtraParams    *VideoExtraParams `json:"extra_parameters,omitempty"`
}

// VideoExtraParams 视频生成拓展参数。
type VideoExtraParams struct {
	Resolution  string `json:"resolution,omitempty"`   // 720P / 1080P，默认 1080P
	AspectRatio string `json:"aspect_ratio,omitempty"` // 16:9 / 9:16 / 1:1，默认 16:9
	EnableAudio *bool  `json:"enable_audio,omitempty"`  // 是否生成音频，默认 true
}

// VideoGenResponse 提交视频任务上游响应。
type VideoGenResponse struct {
	Code      int    `json:"code"`
	Msg       string `json:"msg"`
	RequestID string `json:"requestId"`
	Data      struct {
		ID string `json:"id"`
	} `json:"data"`
}

// VideoTaskQueryResponse 轮询视频任务进度响应。
type VideoTaskQueryResponse struct {
	Code      int    `json:"code"`
	Msg       string `json:"msg"`
	RequestID string `json:"requestId"`
	Data      struct {
		ID     string `json:"id"`
		Status string `json:"status"` // "processing" / "completed" / "failed"
		Data   []struct {
			URL string `json:"url"`
		} `json:"data"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"data"`
	Usage struct {
		Credit float64 `json:"credit"`
	} `json:"usage"`
}

// CreateVideoTask 向上游提交视频生成任务。
func (c *Client) CreateVideoTask(ctx context.Context, a *auth.Auth, req *VideoGenRequest, clientIP string) (*VideoGenResponse, error) {
	if a == nil {
		return nil, fmt.Errorf("no account available for video generation")
	}

	bodyBytes, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal video gen request failed: %w", err)
	}

	url := c.chatBase(a) + "/v2/videos/generations"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create video request failed: %w", err)
	}

	meta := ChatMeta{
		ConversationID:        session.NewUUID(),
		ConversationRequestID: session.NewUUID(),
	}
	c.ChatHeaders(httpReq, a, clientIP, meta)
	httpReq.Header.Set("Content-Type", "application/json")

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 120 * time.Second}
	}

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("do video request failed: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read video response failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream video API status %d: %s", resp.StatusCode, string(respBytes))
	}

	var res VideoGenResponse
	if err := json.Unmarshal(respBytes, &res); err != nil {
		return nil, fmt.Errorf("unmarshal video response failed: %w (raw: %s)", err, string(respBytes))
	}

	if res.Code != 0 {
		return nil, fmt.Errorf("upstream video error (code %d): %s (requestId: %s)", res.Code, res.Msg, res.RequestID)
	}
	if res.Data.ID == "" {
		return nil, fmt.Errorf("upstream video task id is empty (requestId: %s)", res.RequestID)
	}

	return &res, nil
}

// QueryVideoTask 向上游轮询查询视频任务状态。
func (c *Client) QueryVideoTask(ctx context.Context, a *auth.Auth, taskID string, clientIP string) (*VideoTaskQueryResponse, error) {
	if a == nil {
		return nil, fmt.Errorf("no account available for video task query")
	}

	queryPayload := map[string]string{
		"task_id": taskID,
	}
	bodyBytes, err := json.Marshal(queryPayload)
	if err != nil {
		return nil, fmt.Errorf("marshal video query request failed: %w", err)
	}

	url := c.chatBase(a) + "/v2/videos/tasks"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create video query request failed: %w", err)
	}

	meta := ChatMeta{
		ConversationID:        session.NewUUID(),
		ConversationRequestID: session.NewUUID(),
	}
	c.ChatHeaders(httpReq, a, clientIP, meta)
	httpReq.Header.Set("Content-Type", "application/json")

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("do video query failed: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read video query response failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream video query status %d: %s", resp.StatusCode, string(respBytes))
	}

	var res VideoTaskQueryResponse
	if err := json.Unmarshal(respBytes, &res); err != nil {
		return nil, fmt.Errorf("unmarshal video query response failed: %w (raw: %s)", err, string(respBytes))
	}

	if res.Code != 0 {
		return nil, fmt.Errorf("upstream video query error (code %d): %s (requestId: %s)", res.Code, res.Msg, res.RequestID)
	}

	return &res, nil
}
