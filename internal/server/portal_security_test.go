package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/usermgr"
)

func TestPortalAndConsoleRouting(t *testing.T) {
	p := pool.New("")
	h := NewHandler(Config{
		Pool: p,
	})

	// 1. GET / 访问前台门户展示页
	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("GET / expected 200, got %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "WorkBuddy") || !strings.Contains(body, "双域融合") {
		t.Fatalf("GET / should serve portal page, got body: %s", body[:min(300, len(body))])
	}
	if !strings.Contains(body, "进入控制台") {
		t.Fatalf("GET / should contain '进入控制台' link")
	}

	// 2. GET /console 访问管理控制台
	reqConsole := httptest.NewRequest("GET", "/console", nil)
	rrConsole := httptest.NewRecorder()
	h.ServeHTTP(rrConsole, reqConsole)

	if rrConsole.Code != http.StatusOK {
		t.Fatalf("GET /console expected 200, got %d", rrConsole.Code)
	}
	consoleBody := rrConsole.Body.String()
	if !strings.Contains(consoleBody, "账号池矩阵") {
		t.Fatalf("GET /console should serve dashboard console, got: %s", consoleBody[:min(300, len(consoleBody))])
	}
	if !strings.Contains(consoleBody, "前台门户") {
		t.Fatalf("GET /console should contain '前台门户' link back to portal")
	}

	// 3. GET /dashboard 访问管理控制台（别名）
	reqDashboard := httptest.NewRequest("GET", "/dashboard", nil)
	rrDashboard := httptest.NewRecorder()
	h.ServeHTTP(rrDashboard, reqDashboard)

	if rrDashboard.Code != http.StatusOK {
		t.Fatalf("GET /dashboard expected 200, got %d", rrDashboard.Code)
	}
}

func TestPublicStatusEndpoint(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{
		UID:      "test-uid-123",
		Nickname: "TestAccount",
	})
	h := NewHandler(Config{
		Pool: p,
	})

	req := httptest.NewRequest("GET", "/ui/public/status", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("GET /ui/public/status expected 200, got %d", rr.Code)
	}

	var data map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &data); err != nil {
		t.Fatalf("failed to decode public status JSON: %v", err)
	}

	if data["service"] != ServiceName {
		t.Errorf("expected service %s, got %v", ServiceName, data["service"])
	}
	if data["status"] != "online" {
		t.Errorf("expected status online, got %v", data["status"])
	}

	// 保证完全安全脱敏：不泄露 uid、token 等敏感凭据
	raw := rr.Body.String()
	if strings.Contains(raw, "test-uid-123") || strings.Contains(raw, "TestAccount") {
		t.Fatalf("public status leaked private account data: %s", raw)
	}
}

func TestSecurityHeaders(t *testing.T) {
	p := pool.New("")
	h := NewHandler(Config{
		Pool: p,
	})

	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if ct := rr.Header().Get("X-Content-Type-Options"); ct != "nosniff" {
		t.Errorf("expected X-Content-Type-Options: nosniff, got %s", ct)
	}
	if fo := rr.Header().Get("X-Frame-Options"); fo != "SAMEORIGIN" {
		t.Errorf("expected X-Frame-Options: SAMEORIGIN, got %s", fo)
	}
	if rp := rr.Header().Get("Referrer-Policy"); rp != "strict-origin-when-cross-origin" {
		t.Errorf("expected Referrer-Policy: strict-origin-when-cross-origin, got %s", rp)
	}
}

func TestAuthRateLimiter(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "users.json")
	um, err := usermgr.New(dbPath, "admin123456", "sk-admin-key")
	if err != nil {
		t.Fatalf("usermgr init failed: %v", err)
	}
	h := NewHandler(Config{
		Pool:    pool.New(""),
		UserMgr: um,
	})

	testIP := "192.0.2.88"
	// 模拟 5 次密码错误登录
	for i := 0; i < 5; i++ {
		body, _ := json.Marshal(map[string]string{
			"username": "admin",
			"password": "wrong-password",
		})
		req := httptest.NewRequest("POST", "/ui/auth/login", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", testIP)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d expected 401, got %d", i+1, rr.Code)
		}
	}

	// 第 6 次请求应被速率限制器锁定，返回 429 Too Many Requests
	body, _ := json.Marshal(map[string]string{
		"username": "admin",
		"password": "wrong-password",
	})
	reqLocked := httptest.NewRequest("POST", "/ui/auth/login", bytes.NewReader(body))
	reqLocked.Header.Set("Content-Type", "application/json")
	reqLocked.Header.Set("X-Forwarded-For", testIP)
	rrLocked := httptest.NewRecorder()
	h.ServeHTTP(rrLocked, reqLocked)

	if rrLocked.Code != http.StatusTooManyRequests {
		t.Fatalf("6th attempt expected 429 Too Many Requests, got %d (body: %s)", rrLocked.Code, rrLocked.Body.String())
	}

	// 验证其他未受影响的 IP 不被锁定
	otherIP := "192.0.2.99"
	reqOther := httptest.NewRequest("POST", "/ui/auth/login", bytes.NewReader(body))
	reqOther.Header.Set("Content-Type", "application/json")
	reqOther.Header.Set("X-Forwarded-For", otherIP)
	rrOther := httptest.NewRecorder()
	h.ServeHTTP(rrOther, reqOther)

	if rrOther.Code != http.StatusUnauthorized {
		t.Fatalf("other IP should get 401 not 429, got %d", rrOther.Code)
	}
}

func TestAdminEndpointSecurity(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "users.json")
	um, err := usermgr.New(dbPath, "admin123456", "sk-admin-key")
	if err != nil {
		t.Fatalf("usermgr init failed: %v", err)
	}
	// 创建普通租户
	normalUser, err := um.Register("tenant1", "password123")
	if err != nil {
		t.Fatalf("register tenant failed: %v", err)
	}

	h := NewHandler(Config{
		Pool:    pool.New(""),
		UserMgr: um,
	})

	// 为 tenant1 生成普通会话
	sessToken := "sess_test_tenant"
	webSessions.Store(sessToken, webSessionInfo{
		User:      normalUser,
		ExpiresAt: time.Now().Add(1 * time.Hour),
	})

	// 普通租户尝试修改全局 API Key 应该被 403 Forbidden 拦截
	body, _ := json.Marshal(map[string]string{"api_key": "sk-evil-key"})
	req := httptest.NewRequest("POST", "/ui/config/apikey", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{
		Name:  webSessionCookieName,
		Value: sessToken,
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for non-admin on /ui/config/apikey, got %d", rr.Code)
	}
}

func TestPasswordSetAndClear(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "users.json")
	um, err := usermgr.New(dbPath, "", "sk-admin-key")
	if err != nil {
		t.Fatalf("usermgr init failed: %v", err)
	}

	h := NewHandler(Config{
		Pool:        pool.New(""),
		UserMgr:     um,
		WebPassword: "", // 初始单机免密模式
	})

	// 1. 单机免密状态下访问 /ui/data 直接放行 200
	reqData := httptest.NewRequest("GET", "/ui/data", nil)
	rrData := httptest.NewRecorder()
	h.ServeHTTP(rrData, reqData)
	if rrData.Code != http.StatusOK {
		t.Fatalf("passwordless mode should allow /ui/data, got %d", rrData.Code)
	}

	// 2. 尝试设置小于 6 位的新密码，应被拒绝
	shortBody, _ := json.Marshal(map[string]string{
		"new_password": "123",
	})
	reqShort := httptest.NewRequest("POST", "/ui/config/password", bytes.NewReader(shortBody))
	reqShort.Header.Set("Content-Type", "application/json")
	rrShort := httptest.NewRecorder()
	h.ServeHTTP(rrShort, reqShort)
	if rrShort.Code != http.StatusBadRequest {
		t.Fatalf("short password (<6) should be rejected with 400, got %d", rrShort.Code)
	}

	// 3. 设置合法密码 (>=6位)
	setBody, _ := json.Marshal(map[string]string{
		"new_password": "secret_password",
	})
	reqSet := httptest.NewRequest("POST", "/ui/config/password", bytes.NewReader(setBody))
	reqSet.Header.Set("Content-Type", "application/json")
	rrSet := httptest.NewRecorder()
	h.ServeHTTP(rrSet, reqSet)
	if rrSet.Code != http.StatusOK {
		t.Fatalf("setting password should succeed, got %d (body: %s)", rrSet.Code, rrSet.Body.String())
	}
	var setResp map[string]any
	_ = json.Unmarshal(rrSet.Body.Bytes(), &setResp)
	if setResp["has_password"] != true {
		t.Fatalf("expected has_password true, got %v", setResp["has_password"])
	}

	// 4. 设密后未登录访问 /ui/data，必须被 401 拦截
	reqLocked := httptest.NewRequest("GET", "/ui/data", nil)
	rrLocked := httptest.NewRecorder()
	h.ServeHTTP(rrLocked, reqLocked)
	if rrLocked.Code != http.StatusUnauthorized {
		t.Fatalf("after password set, unauthenticated /ui/data should return 401, got %d", rrLocked.Code)
	}

	// 登录以获取管理员会话
	loginBody, _ := json.Marshal(map[string]string{
		"username": "admin",
		"password": "secret_password",
	})
	reqLogin := httptest.NewRequest("POST", "/ui/auth/login", bytes.NewReader(loginBody))
	reqLogin.Header.Set("Content-Type", "application/json")
	rrLogin := httptest.NewRecorder()
	h.ServeHTTP(rrLogin, reqLogin)
	if rrLogin.Code != http.StatusOK {
		t.Fatalf("admin login should succeed, got %d", rrLogin.Code)
	}
	authCookie := rrLogin.Result().Cookies()[0]

	// 5. 原密码不正确时，尝试清空密码应被拒绝 403
	badClearBody, _ := json.Marshal(map[string]string{
		"old_password": "wrong_password",
		"new_password": "",
	})
	reqBadClear := httptest.NewRequest("POST", "/ui/config/password", bytes.NewReader(badClearBody))
	reqBadClear.Header.Set("Content-Type", "application/json")
	reqBadClear.AddCookie(authCookie)
	rrBadClear := httptest.NewRecorder()
	h.ServeHTTP(rrBadClear, reqBadClear)
	if rrBadClear.Code != http.StatusForbidden {
		t.Fatalf("clear password with wrong old password should return 403, got %d", rrBadClear.Code)
	}

	// 6. 原密码正确，留空 new_password 清空密码并恢复单机免密
	clearBody, _ := json.Marshal(map[string]string{
		"old_password": "secret_password",
		"new_password": "",
	})
	reqClear := httptest.NewRequest("POST", "/ui/config/password", bytes.NewReader(clearBody))
	reqClear.Header.Set("Content-Type", "application/json")
	reqClear.AddCookie(authCookie)
	rrClear := httptest.NewRecorder()
	h.ServeHTTP(rrClear, reqClear)
	if rrClear.Code != http.StatusOK {
		t.Fatalf("clearing password should succeed with 200, got %d (body: %s)", rrClear.Code, rrClear.Body.String())
	}
	var clearResp map[string]any
	_ = json.Unmarshal(rrClear.Body.Bytes(), &clearResp)
	if clearResp["has_password"] != false {
		t.Fatalf("expected has_password false after clearing, got %v", clearResp["has_password"])
	}

	// 7. 清空后再次访问 /ui/data，恢复单机免密直接放行 200
	reqRestored := httptest.NewRequest("GET", "/ui/data", nil)
	rrRestored := httptest.NewRecorder()
	h.ServeHTTP(rrRestored, reqRestored)
	if rrRestored.Code != http.StatusOK {
		t.Fatalf("after password cleared, /ui/data should be restored to passwordless 200, got %d", rrRestored.Code)
	}
}
