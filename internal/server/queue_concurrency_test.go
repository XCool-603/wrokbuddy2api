package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

func TestConcurrencyQueueingWhenInFlightFull(t *testing.T) {
	p := pool.New("")
	p.SetMaxInFlight(1)
	p.SetMaxInFlightGlobal(1)

	acct := &auth.Auth{
		UID:         "test-queue-user",
		AccessToken: "tok_test_123",
		Domain:      "",
	}
	acct.SetRealm("global")
	p.Add(acct)

	// mock server
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"))
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
		},
	}

	var wg sync.WaitGroup
	var code1, code2 int

	wg.Add(2)
	go func() {
		defer wg.Done()
		reqBody := []byte(`{"model":"global:deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi 1"}]}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(reqBody))
		rec := httptest.NewRecorder()
		h.chatCompletions(rec, req)
		code1 = rec.Code
	}()

	// 稍微延后 20ms 发出第 2 个请求，此时账号正被第 1 个请求占满 (in-flight = 1)
	time.Sleep(20 * time.Millisecond)
	go func() {
		defer wg.Done()
		reqBody := []byte(`{"model":"global:deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi 2"}]}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(reqBody))
		rec := httptest.NewRecorder()
		h.chatCompletions(rec, req)
		code2 = rec.Code
	}()

	wg.Wait()

	if code1 != http.StatusOK {
		t.Errorf("req1 failed with code %d", code1)
	}
	if code2 != http.StatusOK {
		t.Errorf("req2 should have queued and succeeded with 200, got %d", code2)
	}
}

func TestClientCancelDoesNotPunishAccount(t *testing.T) {
	p := pool.New("")
	p.SetMaxInFlight(2)

	acct := &auth.Auth{
		UID:         "test-cancel-user",
		AccessToken: "tok_test_cancel",
		Domain:      "",
	}
	acct.SetRealm("global")
	p.Add(acct)

	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
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
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	reqBody := []byte(`{"model":"global:deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi cancel"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(reqBody)).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.chatCompletions(rec, req)

	st, ok := p.Status(acct.UID)
	if !ok {
		t.Fatalf("account not found")
	}
	if st.ConsecutiveFails > 0 {
		t.Errorf("consecutive fails should be 0 on client cancel, got %d", st.ConsecutiveFails)
	}
	if st.Cooling {
		t.Errorf("account should not be cooling after client cancel")
	}
}

func TestConcurrencyQueueWithCoolingAccountsPresent(t *testing.T) {
	p := pool.New("")
	p.SetMaxInFlight(1)
	p.SetMaxInFlightGlobal(1)

	goodAcct := &auth.Auth{
		UID:         "good-queue-acct",
		AccessToken: "tok_good",
		Domain:      "",
	}
	goodAcct.SetRealm("global")
	p.Add(goodAcct)

	badAcct := &auth.Auth{
		UID:         "bad-cooling-acct",
		AccessToken: "tok_bad",
		Domain:      "",
	}
	badAcct.SetRealm("global")
	p.Add(badAcct)
	// 将 badAcct 置为 6004 冷却状态
	p.CooldownSoftForModel(badAcct.UID, time.Hour, time.Now().Add(time.Hour), "deepseek-v4.1-flash", "6004 model rate limit")

	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHdr := r.Header.Get("Authorization")
		if authHdr == "Bearer tok_bad" {
			// 若错误兜底捞了 badAcct，模拟 APISIX 401 报错
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("<html><head><title>401 Authorization Required</title></head></html>"))
			return
		}
		time.Sleep(150 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"))
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
		},
	}

	var wg sync.WaitGroup
	var code1, code2 int

	wg.Add(2)
	go func() {
		defer wg.Done()
		reqBody := []byte(`{"model":"global:deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi 1"}]}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(reqBody))
		rec := httptest.NewRecorder()
		h.chatCompletions(rec, req)
		code1 = rec.Code
	}()

	time.Sleep(20 * time.Millisecond)
	go func() {
		defer wg.Done()
		reqBody := []byte(`{"model":"global:deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi 2"}]}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(reqBody))
		rec := httptest.NewRecorder()
		h.chatCompletions(rec, req)
		code2 = rec.Code
	}()

	wg.Wait()

	if code1 != http.StatusOK {
		t.Errorf("req1 failed with code %d", code1)
	}
	if code2 != http.StatusOK {
		t.Errorf("req2 should have waited for goodAcct and returned 200, but got %d (likely falsely fell back to cooling acct)", code2)
	}
}

func TestUpstream401DisablesDeadAccount(t *testing.T) {
	p := pool.New("")
	p.SetMaxInFlight(2)

	deadAcct := &auth.Auth{
		UID:         "dead-token-acct",
		AccessToken: "tok_dead_no_refresh",
		Domain:      "",
	}
	deadAcct.SetRealm("global")
	p.Add(deadAcct)

	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("<html><head><title>401 Authorization Required</title></head></html>"))
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
		},
	}

	reqBody := []byte(`{"model":"global:deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(reqBody))
	rec := httptest.NewRecorder()
	h.chatCompletions(rec, req)

	st, ok := p.Status(deadAcct.UID)
	if !ok {
		t.Fatalf("account not found")
	}
	if !st.Disabled {
		t.Errorf("dead account without refreshToken getting 401 should be disabled, got disabled=%v", st.Disabled)
	}
}
