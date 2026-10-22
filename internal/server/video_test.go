package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

func setupVideoTestServer(t *testing.T) (*httptest.Server, *Handler, *auth.Auth) {
	var queryCount int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/videos/generations":
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{
				"code":      0,
				"msg":       "",
				"requestId": "req_gen_1",
				"data": map[string]any{
					"id": "task_video_999",
				},
			})
		case "/v2/videos/tasks":
			cnt := atomic.AddInt32(&queryCount, 1)
			w.WriteHeader(http.StatusOK)
			if cnt < 2 {
				json.NewEncoder(w).Encode(map[string]any{
					"code":      0,
					"msg":       "",
					"requestId": "req_task_poll",
					"data": map[string]any{
						"id":     "task_video_999",
						"status": "processing",
					},
				})
			} else {
				json.NewEncoder(w).Encode(map[string]any{
					"code":      0,
					"msg":       "",
					"requestId": "req_task_done",
					"data": map[string]any{
						"id":     "task_video_999",
						"status": "completed",
						"data": []map[string]any{
							{"url": "https://cdn.example.com/video999.mp4"},
						},
					},
					"usage": map[string]any{
						"credit": 10.0,
					},
				})
			}
		default:
			http.NotFound(w, r)
		}
	}))

	uClient := &upstream.Client{
		HTTP:       srv.Client(),
		ChatBaseCN: srv.URL,
	}

	testPool := pool.New("")
	testAcct := &auth.Auth{
		UID:         "test_user",
		AccessToken: "test_at",
	}
	testPool.Add(testAcct)

	cfg := Config{
		Pool:     testPool,
		Upstream: uClient,
	}
	h := NewHandler(cfg)

	return srv, h, testAcct
}

func TestVideoGenerationsAsync(t *testing.T) {
	srv, h, _ := setupVideoTestServer(t)
	defer srv.Close()

	body := map[string]any{
		"prompt": "sunset on the beach, slow motion",
		"model":  "kling-v3-t2v",
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest("POST", "/v1/videos/generations", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var res map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if res["id"] != "task_video_999" || res["status"] != "processing" {
		t.Errorf("unexpected response: %+v", res)
	}
}

func TestVideoGenerationsSyncWait(t *testing.T) {
	srv, h, _ := setupVideoTestServer(t)
	defer srv.Close()

	body := map[string]any{
		"prompt": "sunset on the beach, slow motion",
		"wait":   true,
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest("POST", "/v1/videos/generations", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var res map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if res["status"] != "completed" {
		t.Errorf("status=%v, want completed", res["status"])
	}

	dataList, ok := res["data"].([]any)
	if !ok || len(dataList) == 0 {
		t.Fatalf("expected non-empty data array in result: %+v", res)
	}
	firstItem := dataList[0].(map[string]any)
	if firstItem["url"] != "https://cdn.example.com/video999.mp4" {
		t.Errorf("unexpected url: %v", firstItem["url"])
	}
}

func TestVideoTaskGetAndPost(t *testing.T) {
	srv, h, _ := setupVideoTestServer(t)
	defer srv.Close()

	// 1. GET /v1/videos/tasks/{id}
	reqGet := httptest.NewRequest("GET", "/v1/videos/tasks/task_video_999", nil)
	wGet := httptest.NewRecorder()
	h.ServeHTTP(wGet, reqGet)

	if wGet.Code != http.StatusOK {
		t.Fatalf("GET /v1/videos/tasks/task_video_999 failed: %d (%s)", wGet.Code, wGet.Body.String())
	}

	// 2. POST /v1/videos/tasks
	postBody, _ := json.Marshal(map[string]string{"task_id": "task_video_999"})
	reqPost := httptest.NewRequest("POST", "/v1/videos/tasks", bytes.NewReader(postBody))
	reqPost.Header.Set("Content-Type", "application/json")
	wPost := httptest.NewRecorder()
	h.ServeHTTP(wPost, reqPost)

	if wPost.Code != http.StatusOK {
		t.Fatalf("POST /v1/videos/tasks failed: %d (%s)", wPost.Code, wPost.Body.String())
	}
}

func TestVideoValidation(t *testing.T) {
	srv, h, _ := setupVideoTestServer(t)
	defer srv.Close()

	// Missing prompt without image
	bodyBytes, _ := json.Marshal(map[string]any{})
	req := httptest.NewRequest("POST", "/v1/videos/generations", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request, got %d: %s", w.Code, w.Body.String())
	}
}
