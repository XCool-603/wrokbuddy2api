package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

func TestHandleActionWelfareAndInvite(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "wb2a-welfare-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	authFile := filepath.Join(tmpDir, "workbuddy-user1.json")
	authContent := `{
		"uid": "wb_user_welfare_1",
		"access_token": "mock_at",
		"nickname": "TestUser"
	}`
	if err := os.WriteFile(authFile, []byte(authContent), 0600); err != nil {
		t.Fatal(err)
	}

	p := pool.New(tmpDir)
	a, _ := auth.Parse([]byte(authContent))
	a.FilePath = authFile
	p.Add(a)

	upServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/billing/meter/claim-gift":
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"credit": 66}})
		case "/billing/meter/claim-compensation":
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"credit": 20}})
		case "/billing/meter/checkin":
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{}})
		case "/billing/meter/resource":
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"credit": 186}})
		case "/activity/growth/buddy/agreement", "/activity/growth/buddy/first":
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{}})
		case "/activity/growth/invite/accept":
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"reward_credit": 50}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{}})
		}
	}))
	defer upServer.Close()

	up := upstream.New()
	up.ChatBaseCN = upServer.URL
	up.BillingBaseCN = upServer.URL

	h := NewHandler(Config{
		Pool:     p,
		Upstream: up,
		AuthDir:  tmpDir,
	})

	// 测试一键吃满福利 handleActionWelfare
	reqBody := []byte(`{}`)
	req := httptest.NewRequest(http.MethodPost, "/ui/action/welfare", bytes.NewReader(reqBody))
	w := httptest.NewRecorder()
	h.handleActionWelfare(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", w.Code)
	}

	var welfareResp struct {
		Success         bool                     `json:"success"`
		TotalNewCredits int64                    `json:"total_new_credits"`
		Results         []upstream.WelfareResult `json:"results"`
		Output          string                   `json:"output"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &welfareResp); err != nil {
		t.Fatalf("unmarshal response failed: %v", err)
	}
	if !welfareResp.Success {
		t.Errorf("expected success true")
	}
	if welfareResp.TotalNewCredits != 386 { // 66 (gift) + 20 (comp) + 300 (buddy adopt) = 386
		t.Errorf("expected 86 total credits, got %d", welfareResp.TotalNewCredits)
	}

	// 测试绑定邀请码 handleActionBindInvite
	bindReqBody := []byte(`{"uid":"wb_user_welfare_1","invite_code":"OLD_FRIEND_INVITE"}`)
	bindReq := httptest.NewRequest(http.MethodPost, "/ui/action/bind_invite", bytes.NewReader(bindReqBody))
	bindW := httptest.NewRecorder()
	h.handleActionBindInvite(bindW, bindReq)

	if bindW.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", bindW.Code)
	}
	var bindResp struct {
		Success bool  `json:"success"`
		Reward  int64 `json:"reward"`
	}
	if err := json.Unmarshal(bindW.Body.Bytes(), &bindResp); err != nil {
		t.Fatalf("unmarshal bind response failed: %v", err)
	}
	if !bindResp.Success {
		t.Errorf("expected success true")
	}
	if bindResp.Reward != 50 {
		t.Errorf("expected 50 reward credits, got %d", bindResp.Reward)
	}

	// 测试派猫猫旅行 handleActionTravel
	travelReq := httptest.NewRequest(http.MethodPost, "/ui/action/travel", bytes.NewReader([]byte(`{}`)))
	travelW := httptest.NewRecorder()
	h.handleActionTravel(travelW, travelReq)
	if travelW.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for travel, got %d", travelW.Code)
	}
	var travelResp struct {
		Success bool   `json:"success"`
		Output  string `json:"output"`
	}
	if err := json.Unmarshal(travelW.Body.Bytes(), &travelResp); err != nil {
		t.Fatalf("unmarshal travel response failed: %v", err)
	}
	if !travelResp.Success {
		t.Errorf("expected travel success true")
	}

	// 测试一键活动任务 handleActionTasks
	tasksDone := make(chan struct{}, 1)
	h.cfg.RunTasksFunc = func() {
		select {
		case tasksDone <- struct{}{}:
		default:
		}
	}
	tasksReq := httptest.NewRequest(http.MethodPost, "/ui/action/tasks", bytes.NewReader([]byte(`{}`)))
	tasksW := httptest.NewRecorder()
	h.handleActionTasks(tasksW, tasksReq)
	if tasksW.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for tasks, got %d", tasksW.Code)
	}
	select {
	case <-tasksDone:
	case <-time.After(500 * time.Millisecond):
		t.Errorf("expected RunTasksFunc to be called")
	}
}
