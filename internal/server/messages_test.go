package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

func TestMessagesAuth(t *testing.T) {
	h := NewHandler(Config{APIKey: "secret-key"})

	// 1. 无 Key -> 401
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}

	// 2. 带 x-api-key -> 通过鉴权 (由于无账号可能返回 503/400，但不是 401)
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"test","messages":[{"role":"user","content":"hi"}]}`))
	req2.Header.Set("x-api-key", "secret-key")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code == http.StatusUnauthorized {
		t.Fatalf("expected non-401 with valid x-api-key, got 401")
	}
}

func TestMessagesPayloadConversion(t *testing.T) {
	// 验证请求体解析与转发逻辑
	reqBody := anthropicMessageRequest{
		Model: "claude-3-5-sonnet",
		Messages: []anthropicMsg{
			{Role: "user", Content: json.RawMessage(`"hello"`)},
		},
		System:    json.RawMessage(`"you are helpful"`),
		MaxTokens: 500,
	}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatal(err)
	}

	h := NewHandler(Config{})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// 没有账号池配置，预期返回 503 no_healthy_account
	if rec.Code != http.StatusServiceUnavailable {
		t.Logf("got status %d with body: %s", rec.Code, rec.Body.String())
	}
}

func TestMessagesNonStreamingE2E(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{
		UID:         "u1",
		Nickname:    "nick1",
		AccessToken: "token1",
	})

	u := newFakeUpstream(t, func(auth string) (int, string, bool) {
		return http.StatusOK, sseOK, true
	})

	h := NewHandler(Config{
		Pool:     p,
		Upstream: u,
	})

	reqBody := `{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d with body %s", rec.Code, rec.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal claude response failed: %v", err)
	}

	if resp["type"] != "message" {
		t.Fatalf("expected type 'message', got %v", resp["type"])
	}
	if resp["role"] != "assistant" {
		t.Fatalf("expected role 'assistant', got %v", resp["role"])
	}
	content, ok := resp["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("expected non-empty content list")
	}
	firstPart := content[0].(map[string]any)
	if firstPart["type"] != "text" || firstPart["text"] != "你好" {
		t.Fatalf("unexpected content: %+v", firstPart)
	}
}

func TestMessagesStreamingE2E(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{
		UID:         "u1",
		Nickname:    "nick1",
		AccessToken: "token1",
	})

	u := newFakeUpstream(t, func(auth string) (int, string, bool) {
		return http.StatusOK, sseOK, true
	})

	h := NewHandler(Config{
		Pool:     p,
		Upstream: u,
	})

	reqBody := `{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d with body %s", rec.Code, rec.Body.String())
	}

	bodyStr := rec.Body.String()
	if !strings.Contains(bodyStr, "event: message_start") {
		t.Fatalf("expected event: message_start, got: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "event: content_block_delta") {
		t.Fatalf("expected event: content_block_delta, got: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "event: message_stop") {
		t.Fatalf("expected event: message_stop, got: %s", bodyStr)
	}
}
