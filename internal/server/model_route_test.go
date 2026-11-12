package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

func TestIsFreeCredits(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"x0.00", true},
		{"x0.00 credits", true},
		{"0.00", true},
		{"0", true},
		{"free", true},
		{"x0.05", false},
		{"x1.00", false},
		{"x0.29 credits", false},
	}
	for _, tc := range tests {
		got := IsFreeCredits(tc.in)
		if got != tc.want {
			t.Errorf("IsFreeCredits(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestModelCircuitBreaker(t *testing.T) {
	cb := NewModelCircuitBreaker()
	model := "test-free-m1"

	if cb.IsCooling(model) {
		t.Errorf("expected model %s not cooling initially", model)
	}

	// 第一次失败，尚未达到 2 次阈值
	cb.NoteFailure(model, 2*time.Second)
	if cb.IsCooling(model) {
		t.Errorf("expected model %s not cooling after 1 failure", model)
	}

	// 第二次失败，达到阈值，触发熔断
	cb.NoteFailure(model, 2*time.Second)
	if !cb.IsCooling(model) {
		t.Errorf("expected model %s to be cooling after 2 failures", model)
	}

	// 成功调用，重置状态
	cb.NoteSuccess(model)
	if cb.IsCooling(model) {
		t.Errorf("expected model %s not cooling after success", model)
	}

	// Trip 强制熔断
	cb.Trip(model, 2*time.Second)
	if !cb.IsCooling(model) {
		t.Errorf("expected model %s to be cooling after Trip", model)
	}
}

func TestResolveModelCandidates(t *testing.T) {
	h := NewHandler(Config{
		FreeModels: []string{"custom-free-1", "custom-free-2"},
		ModelRoutes: map[string][]string{
			"code-group": {"model-a", "model-b"},
		},
		ModelAliases: map[string]string{
			"alias-csv": "m-csv-1,m-csv-2",
		},
	})

	// 测试内置 free 路由
	freeList := h.ResolveModelCandidates("auto-free")
	if len(freeList) == 0 {
		t.Fatalf("expected non-empty candidates for auto-free")
	}
	if freeList[0] != "custom-free-1" {
		t.Errorf("got %s, want custom-free-1", freeList[0])
	}

	// 测试自定义 ModelRoutes
	groupList := h.ResolveModelCandidates("code-group")
	if len(groupList) != 2 || groupList[0] != "model-a" || groupList[1] != "model-b" {
		t.Errorf("unexpected groupList: %+v", groupList)
	}

	// 测试逗号分隔的别名
	csvList := h.ResolveModelCandidates("alias-csv")
	if len(csvList) != 2 || csvList[0] != "m-csv-1" || csvList[1] != "m-csv-2" {
		t.Errorf("unexpected csvList: %+v", csvList)
	}

	// 测试普通模型
	singleList := h.ResolveModelCandidates("gpt-4o")
	if len(singleList) != 1 {
		t.Errorf("expected 1 element for single model, got %+v", singleList)
	}
}

func TestAutoFreeModelFailover(t *testing.T) {
	// 模拟上游服务：
	// 当请求 "bad-free-model" 时返回 429 限流
	// 当请求 "backup-free-model" 时返回 200 成功
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")

		if req.Model == "bad-free-model" {
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 429,
				"msg":  "Rate limit exceeded",
			})
			return
		}

		if req.Model == "backup-free-model" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			chunk := `data: {"id":"chatcmpl-test","choices":[{"delta":{"content":"Hello from backup free model!"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30,"credit":0}}` + "\n\n"
			w.Write([]byte(chunk))
			w.Write([]byte("data: [DONE]\n\n"))
			return
		}

		http.NotFound(w, r)
	}))
	defer srv.Close()

	up := upstream.New()
	up.ChatBaseCN = srv.URL
	up.BillingBaseCN = srv.URL

	p := pool.New("")
	a := &auth.Auth{
		UID:         "test_user_free",
		AccessToken: "mock_at",
	}
	p.Add(a)

	h := NewHandler(Config{
		Pool:     p,
		Upstream: up,
		FreeModels: []string{
			"bad-free-model",
			"backup-free-model",
		},
	})

	// 客户端请求 "auto-free" 虚拟模型
	reqBody := []byte(`{"model":"auto-free","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(reqBody))
	w := httptest.NewRecorder()

	h.chatCompletions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected failover to backup-free-model with 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if len(resp.Choices) == 0 || resp.Choices[0].Message.Content != "Hello from backup free model!" {
		t.Errorf("unexpected content: %+v", resp)
	}

	// 验证 bad-free-model 已被记入故障熔断
	if !h.modelBreaker.IsCooling("bad-free-model") {
		t.Errorf("expected bad-free-model to be cooling in model breaker")
	}
}

func Test6004InstantFailoverAcrossLargePoolAndResetBreaker(t *testing.T) {
	// 启用真实退避基数，验证 6004 确定性限流不会睡 rotateBackoff，且超过 MaxRotate=3 的健康第 4 号可秒接
	withRotateBackoff(t, 250*time.Millisecond)
	resetAt := time.Now().Add(40 * time.Minute)
	resetStr := resetAt.In(upstream.SoftRateResetLoc()).Format("2006-01-02 15:04:05")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authz := r.Header.Get("Authorization")
		if authz == "Bearer at-u4" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`data: {"id":"ok","choices":[{"delta":{"content":"fast-ok"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"code":6004,"msg":"usage exceeds frequency limit, but don't worry, your usage will reset at ` + resetStr + ` UTC+8, alternatively, you can switch to the other models to continue using it.","requestId":"req-6004"}`))
	}))
	defer srv.Close()

	up := upstream.New()
	up.ChatBaseCN = srv.URL
	up.BillingBaseCN = srv.URL
	up.ChatBaseGlobal = srv.URL
	up.BillingBaseGlobal = srv.URL
	up.GlobalEnabled = true

	p := pool.New("")
	for _, uid := range []string{"u1", "u2", "u3", "u4"} {
		a := &auth.Auth{UID: uid, AccessToken: "at-" + uid, Domain: "www.workbuddy.ai", ExpiresAt: 9999999999}
		a.SetRealm("global")
		p.Add(a)
	}

	h := NewHandler(Config{
		Pool:          p,
		Upstream:      up,
		GlobalEnabled: true,
		MaxRotate:     3,
	})

	start := time.Now()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"global:claude-4.5-sonnet","messages":[{"role":"user","content":"hi"}],"stream":false}`)))
	w := httptest.NewRecorder()
	h.chatCompletions(w, req)
	elapsed := time.Since(start)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK via 4th healthy account, got %d: %s", w.Code, w.Body.String())
	}
	if elapsed >= 200*time.Millisecond {
		t.Fatalf("expected 0-backoff instant failover on 6004, took %v", elapsed)
	}
}

func Test6004ModelBreakerCooldownMatchesResetAndSkipsCoolingCandidate(t *testing.T) {
	withRotateBackoff(t, 250*time.Millisecond)
	resetAt := time.Now().Add(35 * time.Minute)
	resetStr := resetAt.In(upstream.SoftRateResetLoc()).Format("2006-01-02 15:04:05")

	var hitsA, hitsB int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Model == "model-a" {
			hitsA++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"code":6004,"msg":"usage exceeds frequency limit, your usage will reset at ` + resetStr + ` UTC+8"}`))
			return
		}
		hitsB++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`data: {"id":"b","choices":[{"delta":{"content":"model-b-ok"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	up := upstream.New()
	up.ChatBaseCN = srv.URL
	up.BillingBaseCN = srv.URL

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999})

	h := NewHandler(Config{
		Pool:     p,
		Upstream: up,
		ModelRoutes: map[string][]string{
			"smart-route": {"model-a", "model-b"},
		},
	})

	start := time.Now()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"smart-route","messages":[{"role":"user","content":"hi"}],"stream":false}`)))
	w := httptest.NewRecorder()
	h.chatCompletions(w, req)
	elapsed := time.Since(start)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from model-b, got %d: %s", w.Code, w.Body.String())
	}
	if elapsed >= 200*time.Millisecond {
		t.Fatalf("expected instant model failover without backoff sleep, took %v", elapsed)
	}
	if hitsA != 1 || hitsB != 1 {
		t.Fatalf("expected hitsA=1 hitsB=1, got hitsA=%d hitsB=%d", hitsA, hitsB)
	}
	if !h.modelBreaker.IsCooling("model-a") {
		t.Fatalf("expected model-a to be cooling in modelBreaker")
	}
	h.modelBreaker.mu.RLock()
	until := h.modelBreaker.cooling["model-a"]
	h.modelBreaker.mu.RUnlock()
	if time.Until(until) < 20*time.Minute {
		t.Fatalf("expected model-a breaker cooldown to align with 35m resetAt, got %v", time.Until(until))
	}

	// 第二次请求：model-a 熔断中，ResolveModelCandidates 应直接把 model-b 排在首位且零命中 model-a
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"smart-route","messages":[{"role":"user","content":"hi"}],"stream":false}`)))
	w2 := httptest.NewRecorder()
	h.chatCompletions(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on 2nd request, got %d: %s", w2.Code, w2.Body.String())
	}
	if hitsA != 1 || hitsB != 2 {
		t.Fatalf("expected cached breaker to skip model-a completely (hitsA=1, hitsB=2), got hitsA=%d hitsB=%d", hitsA, hitsB)
	}
}

func TestSkipsCoolingFallbackWhenHealthyBackupCandidateExists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Model == "model-a" {
			t.Fatalf("must not probe cooling account on model-a when healthy model-b exists")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`data: {"id":"b","choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	up := upstream.New()
	up.ChatBaseCN = srv.URL
	up.BillingBaseCN = srv.URL

	p := pool.New("")
	a1 := &auth.Auth{UID: "cn1", AccessToken: "at-cn1", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999}
	a2 := &auth.Auth{UID: "gl1", AccessToken: "at-gl1", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999}
	a2.SetRealm("global")
	p.Add(a1)
	p.Add(a2)
	// 让 cn1 对 model-a 处于 6004 冷却，gl1 整体软冷却（但只影响 model-a）——其实让 cn1 也对 model-b 健康，
	// 或用 cn:model-a + cn:model-b 模拟 model-a 在 cn1 被 6004 冷却而 model-b 健康。
	p.CooldownSoftForModel("cn1", time.Minute, time.Now().Add(10*time.Minute), "model-a", "6004 model rate limit")
	p.Cooldown("gl1", pool.CoolSoft, 10*time.Minute, "429")

	h := NewHandler(Config{
		Pool:          p,
		Upstream:      up,
		GlobalEnabled: true,
		ModelRoutes: map[string][]string{
			"route-ab": {"cn:model-a", "cn:model-b"},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"route-ab","messages":[{"role":"user","content":"hi"}],"stream":false}`)))
	w := httptest.NewRecorder()
	h.chatCompletions(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from cn:model-b without poking gl1 cooling fallback on model-a, got %d: %s", w.Code, w.Body.String())
	}
}

func TestNoCoolingProbeWhenPoolHasHealthyAccountsTried(t *testing.T) {
	// 8 账号池：2 个健康 + 6 个冷却中（非全冷却）。
	// 2 个健康号失败后，绝不允许去试探那 6 个冷却号！
	var called []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = append(called, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"code":6004,"msg":"usage exceeds frequency limit, your usage will reset at 2099-01-01 00:00:00 UTC+8"}`))
	}))
	defer srv.Close()

	up := upstream.New()
	up.ChatBaseCN = srv.URL
	up.BillingBaseCN = srv.URL

	p := pool.New("")
	for i := 1; i <= 8; i++ {
		uid := "u" + string(rune('0'+i))
		p.Add(&auth.Auth{UID: uid, AccessToken: "at-" + uid, ExpiresAt: 9999999999})
		if i >= 3 {
			p.Cooldown(uid, pool.CoolSoft, 10*time.Minute, "429")
		}
	}

	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 3})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}],"stream":false}`)))
	w := httptest.NewRecorder()
	h.chatCompletions(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d: %s", w.Code, w.Body.String())
	}
	if len(called) != 2 {
		t.Fatalf("expected only the 2 healthy accounts tried (zero cooling probes), got %d calls: %v", len(called), called)
	}
}

func TestAllCoolingProbesOnlySingleEarliestAccount(t *testing.T) {
	// 8 账号全冷却：只允许试探最早到期那 1 个冷却账号；试探失败立刻结束，绝不逐个试探其余 7 个。
	var called []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = append(called, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"code":429,"msg":"rate limited"}`))
	}))
	defer srv.Close()

	up := upstream.New()
	up.ChatBaseCN = srv.URL
	up.BillingBaseCN = srv.URL

	p := pool.New("")
	for i := 1; i <= 8; i++ {
		uid := "u" + string(rune('0'+i))
		p.Add(&auth.Auth{UID: uid, AccessToken: "at-" + uid, ExpiresAt: 9999999999})
		p.Cooldown(uid, pool.CoolSoft, time.Duration(i)*time.Minute, "429")
	}

	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 3})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}],"stream":false}`)))
	w := httptest.NewRecorder()
	h.chatCompletions(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d: %s", w.Code, w.Body.String())
	}
	if len(called) != 1 || called[0] != "Bearer at-u1" {
		t.Fatalf("expected single probe on earliest-expiring u1 only, got %v", called)
	}
}
