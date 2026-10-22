package upstream

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/auth"
)

func TestCreateVideoTaskSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/videos/generations" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test_token" {
			t.Errorf("unexpected auth header: %s", r.Header.Get("Authorization"))
		}
		if r.Header.Get("X-Conversation-Id") == "" {
			t.Errorf("expected non-empty X-Conversation-Id")
		}

		var req VideoGenRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Model != "kling-v3-t2v" {
			t.Errorf("model=%s, want kling-v3-t2v", req.Model)
		}
		if req.Prompt != "test prompt" {
			t.Errorf("prompt=%s, want test prompt", req.Prompt)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"code":      0,
			"msg":       "",
			"requestId": "req_123",
			"data": map[string]any{
				"id": "task_abc_456",
			},
		})
	}))
	defer srv.Close()

	c := &Client{
		HTTP:       srv.Client(),
		ChatBaseCN: srv.URL,
	}

	a := &auth.Auth{
		AccessToken: "test_token",
	}

	enableAudio := true
	req := &VideoGenRequest{
		Model:   "kling-v3-t2v",
		Prompt:  "test prompt",
		Seconds: 5,
		ExtraParams: &VideoExtraParams{
			Resolution:  "1080P",
			AspectRatio: "16:9",
			EnableAudio: &enableAudio,
		},
	}

	resp, err := c.CreateVideoTask(context.Background(), a, req, "127.0.0.1")
	if err != nil {
		t.Fatalf("CreateVideoTask failed: %v", err)
	}
	if resp.Data.ID != "task_abc_456" {
		t.Errorf("got taskId %s, want task_abc_456", resp.Data.ID)
	}
}

func TestQueryVideoTaskSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/videos/tasks" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		if payload["task_id"] != "task_abc_456" {
			t.Errorf("task_id=%s, want task_abc_456", payload["task_id"])
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"code":      0,
			"msg":       "",
			"requestId": "req_789",
			"data": map[string]any{
				"id":     "task_abc_456",
				"status": "completed",
				"data": []map[string]any{
					{"url": "https://video.example.com/result.mp4"},
				},
			},
			"usage": map[string]any{
				"credit": 10.5,
			},
		})
	}))
	defer srv.Close()

	c := &Client{
		HTTP:       srv.Client(),
		ChatBaseCN: srv.URL,
	}

	a := &auth.Auth{
		AccessToken: "test_token",
	}

	resp, err := c.QueryVideoTask(context.Background(), a, "task_abc_456", "127.0.0.1")
	if err != nil {
		t.Fatalf("QueryVideoTask failed: %v", err)
	}
	if resp.Data.Status != "completed" {
		t.Errorf("got status %s, want completed", resp.Data.Status)
	}
	if len(resp.Data.Data) != 1 || resp.Data.Data[0].URL != "https://video.example.com/result.mp4" {
		t.Errorf("unexpected video url: %+v", resp.Data.Data)
	}
}
