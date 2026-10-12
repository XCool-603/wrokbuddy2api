package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

func TestModelAliasesResolution(t *testing.T) {
	h := &Handler{
		cfg: Config{
			ModelAliases: map[string]string{
				"deepseek-chat":     "global:deepseek-v4.1-flash",
				"deepseek-reasoner": "global:deepseek-v4.1-flash",
				"gpt-4o":            "cn:glm-5.3",
			},
		},
	}

	// 1. Alias mapped to global model
	realm, bare := h.resolveModelWithAliases("deepseek-chat")
	if realm != "global" || bare != "deepseek-v4.1-flash" {
		t.Fatalf("resolveModelWithAliases(deepseek-chat) = (%s, %s), want (global, deepseek-v4.1-flash)", realm, bare)
	}

	// 2. Case-insensitive alias
	realm, bare = h.resolveModelWithAliases("  DEEPSEEK-CHAT ")
	if realm != "global" || bare != "deepseek-v4.1-flash" {
		t.Fatalf("resolveModelWithAliases(DEEPSEEK-CHAT) = (%s, %s), want (global, deepseek-v4.1-flash)", realm, bare)
	}

	// 3. Alias mapped to cn model
	realm, bare = h.resolveModelWithAliases("gpt-4o")
	if realm != "cn" || bare != "glm-5.3" {
		t.Fatalf("resolveModelWithAliases(gpt-4o) = (%s, %s), want (cn, glm-5.3)", realm, bare)
	}

	// 4. Unmapped native model
	realm, bare = h.resolveModelWithAliases("cn:glm-5.2")
	if realm != "cn" || bare != "glm-5.2" {
		t.Fatalf("resolveModelWithAliases(cn:glm-5.2) = (%s, %s), want (cn, glm-5.2)", realm, bare)
	}
}

func TestModelAliasesInModelList(t *testing.T) {
	p := pool.New("")
	h := &Handler{
		cfg: Config{
			Pool: p,
			ModelAliases: map[string]string{
				"deepseek-chat": "global:deepseek-v4.1-flash",
				"gpt-4o":        "cn:glm-5.3",
			},
		},
	}

	models := h.modelList()
	foundDeepseek := false
	foundGPT4o := false
	for _, m := range models {
		if id, ok := m["id"].(string); ok {
			if id == "deepseek-chat" {
				foundDeepseek = true
			}
			if id == "gpt-4o" {
				foundGPT4o = true
			}
		}
	}
	if !foundDeepseek {
		t.Errorf("model deepseek-chat not found in modelList output")
	}
	if !foundGPT4o {
		t.Errorf("model gpt-4o not found in modelList output")
	}
}

func TestConcurrencyQueueingSSEPing(t *testing.T) {
	p := pool.New("")
	p.SetMaxInFlight(1)
	p.SetMaxInFlightGlobal(1)

	acct := &auth.Auth{
		UID:         "test-queue-ping-user",
		AccessToken: "tok_test_ping_123",
	}
	acct.SetRealm("global")
	p.Add(acct)

	// mock server holding request for 3.5 seconds to trigger SSE ping (>3s)
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3200 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello from upstream\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstreamServer.Close()

	upClient := upstream.New()
	upClient.GlobalEnabled = true
	upClient.ChatBaseGlobal = upstreamServer.URL

	h := &Handler{
		cfg: Config{
			Pool:          p,
			Upstream:      upClient,
			GlobalEnabled: true,
			MaxRotate:     1,
			QueueTimeout:  10 * time.Second,
		},
	}

	// 先发第 1 个请求占满 in-flight (1/1)
	go func() {
		reqBody := []byte(`{"model":"global:deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"req 1"}]}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(reqBody))
		rec := httptest.NewRecorder()
		h.chatCompletions(rec, req)
	}()

	// 延后 50ms 发第 2 个流式请求，此时账号处于在途满载，第 2 个请求排队 >3s 并收到 SSE ping 注释
	time.Sleep(50 * time.Millisecond)
	reqBody2 := []byte(`{"model":"global:deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"req 2"}]}`)
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(reqBody2))
	rec2 := httptest.NewRecorder()
	h.chatCompletions(rec2, req2)

	respBody := rec2.Body.String()
	if !strings.Contains(respBody, ": ping queue wait slot...") {
		t.Errorf("expected SSE ping comment in queued response, got:\n%s", respBody)
	}
	if !strings.Contains(respBody, "hello from upstream") {
		t.Errorf("expected final upstream content in queued response, got:\n%s", respBody)
	}
}

func TestConcurrencyQueueingTimeoutSSEError(t *testing.T) {
	p := pool.New("")
	p.SetMaxInFlight(1)
	p.SetMaxInFlightGlobal(1)

	acct := &auth.Auth{
		UID:         "test-queue-timeout-user",
		AccessToken: "tok_test_timeout_123",
	}
	acct.SetRealm("global")
	p.Add(acct)

	// mock server holding connection indefinitely
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
	}))
	defer upstreamServer.Close()

	upClient := upstream.New()
	upClient.GlobalEnabled = true
	upClient.ChatBaseGlobal = upstreamServer.URL

	h := &Handler{
		cfg: Config{
			Pool:          p,
			Upstream:      upClient,
			GlobalEnabled: true,
			MaxRotate:     1,
			QueueTimeout:  3500 * time.Millisecond, // 3.5s timeout: will fire ping at 3s, then timeout at 3.5s
		},
	}

	// 占满 in-flight
	p.Acquire(acct.UID)
	defer p.Release(acct.UID)

	reqBody := []byte(`{"model":"global:deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"req wait"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(reqBody))
	rec := httptest.NewRecorder()
	h.chatCompletions(rec, req)

	respBody := rec.Body.String()
	if !strings.Contains(respBody, ": ping queue wait slot...") {
		t.Errorf("expected ping before timeout, got: %s", respBody)
	}
	if !strings.Contains(respBody, "temporarily unavailable") && !strings.Contains(respBody, "cooling down") {
		t.Errorf("expected SSE error frame upon queue timeout, got: %s", respBody)
	}
}
