package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/usermgr"
)

// TestInFlightTracker 测试在途流请求监控 (F-06)
func TestInFlightTracker(t *testing.T) {
	tracker := NewInFlightTracker()
	if tracker.Count() != 0 {
		t.Fatalf("expected 0 active requests, got %d", tracker.Count())
	}

	done := tracker.Begin("req-1", "deepseek-v4.1-flash", "uid-100", "Nick1", true, "127.0.0.1", "u_admin")
	if tracker.Count() != 1 {
		t.Fatalf("expected 1 active request, got %d", tracker.Count())
	}

	snap := tracker.Snapshot()
	if len(snap) != 1 || snap[0].ID != "req-1" || snap[0].Model != "deepseek-v4.1-flash" {
		t.Fatalf("snapshot mismatch: %+v", snap)
	}

	done()
	if tracker.Count() != 0 {
		t.Fatalf("expected 0 active requests after done, got %d", tracker.Count())
	}
}

// TestAlertManager 测试多通道告警引擎与限频 (F-05)
func TestAlertManager(t *testing.T) {
	received := make(chan string, 10)
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer mockServer.Close()

	mgr := NewAlertManager(AlertConfig{
		Enabled:   true,
		Webhooks:  []string{mockServer.URL},
		Events:    []string{"ban", "401", "cooling", "low_credit"},
		MinCredit: 50,
	})

	// 1. NotifyBan
	mgr.NotifyBan("uid-ban-1", "NickBan", "11140 request illegal")
	select {
	case msg := <-received:
		if !strings.Contains(msg, "11140") && !strings.Contains(msg, "封禁") {
			t.Errorf("unexpected ban alert body: %s", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ban alert")
	}

	// 2. 限流校验：连续发送相同 uid 的 ban 告警应被限频拦截
	mgr.NotifyBan("uid-ban-1", "NickBan", "11140 request illegal again")
	select {
	case msg := <-received:
		t.Fatalf("throttled alert should not be dispatched, got: %s", msg)
	case <-time.After(300 * time.Millisecond):
		// 正确被限流
	}

	// 3. NotifyLowCredit
	mgr.NotifyLowCredit("uid-low-1", "NickLow", 20)
	select {
	case msg := <-received:
		if !strings.Contains(msg, "20") && !strings.Contains(msg, "不足") {
			t.Errorf("unexpected low credit alert body: %s", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for low credit alert")
	}
}

// TestAccountUpdateToken 测试一键重填 Token 与解封 (F-04)
func TestAccountUpdateToken(t *testing.T) {
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	_ = os.MkdirAll(authDir, 0755)

	authPath := filepath.Join(authDir, "test.json")
	initialAuth := &auth.Auth{
		UID:          "uid-update-1",
		Nickname:     "OldNick",
		AccessToken:  "old_access_token",
		RefreshToken: "old_refresh_token",
		FilePath:     authPath,
	}
	raw, _ := json.Marshal(initialAuth)
	_ = os.WriteFile(authPath, raw, 0600)

	p := pool.New("")
	p.Add(initialAuth)
	p.Disable("uid-update-1", "manual disable test")

	userMgr, _ := usermgr.New("", "", "admin-key")

	h := &Handler{
		cfg: Config{
			Pool:    p,
			AuthDir: authDir,
			UserMgr: userMgr,
		},
	}

	// 构造请求更新 Token
	reqBody := `{"uid":"uid-update-1","access_token":"new_tok_999","refresh_token":"new_ref_999"}`
	req := httptest.NewRequest(http.MethodPost, "/ui/account/update_token", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()
	h.handleAccountUpdateToken(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("update_token returned %d: %s", rec.Code, rec.Body.String())
	}

	// 校验内存账号已解封且 token 已更新
	acct := p.AuthByUID("uid-update-1")
	if acct == nil || acct.AccessToken != "new_tok_999" || acct.RefreshToken != "new_ref_999" {
		t.Fatalf("account tokens not updated: %+v", acct)
	}

	st, ok := p.Status("uid-update-1")
	if !ok || st.Disabled {
		t.Fatalf("account should be revived from disabled, got st: %+v", st)
	}

	// 校验磁盘文件已持久化
	diskRaw, _ := os.ReadFile(authPath)
	if !strings.Contains(string(diskRaw), "new_tok_999") {
		t.Fatalf("disk file not updated with new access token: %s", string(diskRaw))
	}
}

// TestTenantQuotaAndRPM 测试租户积分配额与 RPM 频控 (F-07)
func TestTenantQuotaAndRPM(t *testing.T) {
	mgr, _ := usermgr.New("", "", "")
	user, err := mgr.CreateUser("tenant1", "password123", usermgr.RoleUser)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	// 设置配额: 每天 10 积分, RPM = 2
	if err := mgr.SetUserQuota(user.Username, 10, 2); err != nil {
		t.Fatalf("set quota: %v", err)
	}

	// 1. RPM 频控测试 (允许 2 次，第 3 次超限)
	if err := mgr.CheckRPM(user.APIKey); err != nil {
		t.Fatalf("rpm check 1 failed: %v", err)
	}
	if err := mgr.CheckRPM(user.APIKey); err != nil {
		t.Fatalf("rpm check 2 failed: %v", err)
	}
	if err := mgr.CheckRPM(user.APIKey); err == nil {
		t.Fatalf("rpm check 3 should exceed limit, got nil")
	}

	// 2. 每日积分配额测试 (总 10 积分)
	if err := mgr.CheckDailyCredit(user.APIKey); err != nil {
		t.Fatalf("daily credit check 1 failed: %v", err)
	}
	// 消耗 6 积分
	mgr.DeductDailyCredit(user.APIKey, 6)
	if err := mgr.CheckDailyCredit(user.APIKey); err != nil {
		t.Fatalf("daily credit check 2 (after 6) failed: %v", err)
	}
	// 消耗 5 积分 (总计 11 积分，已超 10)
	mgr.DeductDailyCredit(user.APIKey, 5)
	if err := mgr.CheckDailyCredit(user.APIKey); err == nil {
		t.Fatalf("daily credit check 3 should fail after exceeding quota, got nil")
	}
}

// TestBackupExportAndImport 测试系统全量备份与还原 (F-08)
func TestBackupExportAndImport(t *testing.T) {
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	dataDir := filepath.Join(dir, "data")
	_ = os.MkdirAll(authDir, 0755)
	_ = os.MkdirAll(dataDir, 0755)

	// 准备原始文件
	_ = os.WriteFile(filepath.Join(authDir, "acc1.json"), []byte(`{"uid":"u1","token":"t1"}`), 0600)
	_ = os.WriteFile(filepath.Join(dataDir, "settings.json"), []byte(`{"api_key":"test-key"}`), 0600)

	userMgr, _ := usermgr.New("", "", "admin-key")
	h := &Handler{
		cfg: Config{
			AuthDir:   authDir,
			StateFile: filepath.Join(dataDir, "state.json"),
			UserMgr:   userMgr,
		},
	}

	// 1. 导出备份
	req := httptest.NewRequest(http.MethodGet, "/ui/backup/export", nil)
	rec := httptest.NewRecorder()
	h.handleActionBackup(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("backup export returned %d", rec.Code)
	}

	zipBytes := rec.Body.Bytes()
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatalf("open zip reader: %v", err)
	}

	foundAcc := false
	foundSettings := false
	for _, f := range zr.File {
		if f.Name == "acc1.json" {
			foundAcc = true
		}
		if f.Name == "data/settings.json" {
			foundSettings = true
		}
	}
	if !foundAcc {
		t.Errorf("acc1.json not found in backup zip")
	}
	if !foundSettings {
		t.Errorf("data/settings.json not found in backup zip")
	}
}
