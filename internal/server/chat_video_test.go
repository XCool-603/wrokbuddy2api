package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

func TestIsVideoModel(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"kling-video-v3", true},
		{"kling-v3-t2v", true},
		{"kling-v3-i2v", true},
		{"kling", true},
		{"kling-v3", true},
		{"keling", true},
		{"keling-v3", true},
		{"keling-video-v3", true},
		{"可灵", true},
		{"可灵视频", true},
		{"minimax-video-h3", true},
		{"hailuo-video", true},
		{"海螺", true},
		{"cn:kling-video-v3", true},
		{"cn:keling-video-v3", true},
		{"global:minimax-video-h3", true},
		{"deepseek-v3", false},
		{"deepseek-v4-flash", false},
		{"gpt-4o", false},
		{"claude-3-5-sonnet", false},
		{"", false},
	}

	for _, c := range cases {
		got := isVideoModel(c.model)
		if got != c.want {
			t.Errorf("isVideoModel(%q) = %v, want %v", c.model, got, c.want)
		}
	}
}

func TestParseChatVideoRequest(t *testing.T) {
	body := []byte(`{
		"model": "kling-video-v3",
		"messages": [
			{"role": "system", "content": "You are a helpful assistant"},
			{"role": "user", "content": [
				{"type": "text", "text": "一段美丽的日落视频"},
				{"type": "image_url", "image_url": {"url": "https://example.com/sunset.jpg"}}
			]}
		],
		"seconds": 5
	}`)

	req, err := parseChatVideoRequest(body)
	if err != nil {
		t.Fatalf("parseChatVideoRequest failed: %v", err)
	}

	if req.Prompt != "一段美丽的日落视频" {
		t.Errorf("prompt = %q, want 一段美丽的日落视频", req.Prompt)
	}
	if req.ImageURL != "https://example.com/sunset.jpg" {
		t.Errorf("image_url = %q, want https://example.com/sunset.jpg", req.ImageURL)
	}
	if req.Seconds != 5 {
		t.Errorf("seconds = %d, want 5", req.Seconds)
	}
}

func TestChatVideoBridgeNonStream(t *testing.T) {
	// Mock upstream server
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/v2/videos/generations") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"code": 0,
				"msg": "OK",
				"requestId": "req-test-1",
				"data": {"id": "task-test-123"}
			}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/v2/videos/tasks") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"code": 0,
				"msg": "OK",
				"requestId": "req-test-2",
				"data": {
					"id": "task-test-123",
					"status": "completed",
					"data": [{"url": "https://example.com/result.mp4"}]
				}
			}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer upSrv.Close()

	testPool := pool.New("")
	testAcct := &auth.Auth{
		UID:         "test_user",
		AccessToken: "test_at",
	}
	testPool.Add(testAcct)

	uClient := &upstream.Client{
		HTTP:       upSrv.Client(),
		ChatBaseCN: upSrv.URL,
	}

	cfg := Config{
		Pool:     testPool,
		Upstream: uClient,
	}
	h := NewHandler(cfg)

	// Test Non-Stream Chat Completion with video model
	chatReq := `{
		"model": "kling-video-v3",
		"messages": [{"role": "user", "content": "生成一段日落视频"}],
		"stream": false
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatReq))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal resp failed: %v, raw: %s", err, rec.Body.String())
	}

	if len(resp.Choices) == 0 {
		t.Fatalf("no choices returned")
	}

	content := resp.Choices[0].Message.Content
	if !strings.Contains(content, "https://example.com/result.mp4") {
		t.Errorf("content does not contain video URL: %s", content)
	}
	if !strings.Contains(content, "<video") {
		t.Errorf("content does not contain video tag: %s", content)
	}
}

func TestChatVideoBridgeStream(t *testing.T) {
	// Mock upstream server
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/v2/videos/generations") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"code": 0,
				"msg": "OK",
				"requestId": "req-stream-1",
				"data": {"id": "task-stream-456"}
			}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/v2/videos/tasks") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"code": 0,
				"msg": "OK",
				"requestId": "req-stream-2",
				"data": {
					"id": "task-stream-456",
					"status": "completed",
					"data": [{"url": "https://example.com/stream_video.mp4"}]
				}
			}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer upSrv.Close()

	testPool := pool.New("")
	testAcct := &auth.Auth{
		UID:         "test_user",
		AccessToken: "test_at",
	}
	testPool.Add(testAcct)

	uClient := &upstream.Client{
		HTTP:       upSrv.Client(),
		ChatBaseCN: upSrv.URL,
	}

	cfg := Config{
		Pool:     testPool,
		Upstream: uClient,
	}
	h := NewHandler(cfg)

	// Test Stream Chat Completion with video model
	chatReq := `{
		"model": "minimax-video-h3",
		"messages": [{"role": "user", "content": "浩瀚星空宇宙"}],
		"stream": true
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatReq))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if !strings.Contains(body, "data: ") {
		t.Fatalf("expected SSE data format, got: %s", body)
	}
	if !strings.Contains(body, "https://example.com/stream_video.mp4") {
		t.Errorf("stream output does not contain video URL: %s", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Errorf("stream output does not contain [DONE]: %s", body)
	}
}

func TestChatVideoBridgeWithKelingAliasAndRetry(t *testing.T) {
	// Account 1 will return 401, Account 2 will succeed
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		token := r.Header.Get("Authorization")
		if strings.Contains(token, "bad-token") {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code": 401, "msg": "Authorization Required"}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/v2/videos/generations") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"code": 0,
				"msg": "OK",
				"requestId": "req-retry-1",
				"data": {"id": "task-retry-789"}
			}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/v2/videos/tasks") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"code": 0,
				"msg": "OK",
				"requestId": "req-retry-2",
				"data": {
					"id": "task-retry-789",
					"status": "completed",
					"data": [{"url": "https://example.com/retry_video.mp4"}]
				}
			}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer upSrv.Close()

	testPool := pool.New("")
	// Bad account
	badAcct := &auth.Auth{
		UID:         "bad_user",
		AccessToken: "bad-token",
	}
	testPool.Add(badAcct)

	// Good account
	goodAcct := &auth.Auth{
		UID:         "good_user",
		AccessToken: "good-token",
	}
	testPool.Add(goodAcct)

	uClient := &upstream.Client{
		HTTP:       upSrv.Client(),
		ChatBaseCN: upSrv.URL,
	}

	cfg := Config{
		Pool:     testPool,
		Upstream: uClient,
	}
	h := NewHandler(cfg)

	// Test calling with "keling" alias
	chatReq := `{
		"model": "keling",
		"messages": [{"role": "user", "content": "测试可灵别名视频直出"}],
		"stream": false
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatReq))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal resp failed: %v", err)
	}

	content := resp.Choices[0].Message.Content
	if !strings.Contains(content, "https://example.com/retry_video.mp4") {
		t.Errorf("content does not contain video URL: %s", content)
	}
}
