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
