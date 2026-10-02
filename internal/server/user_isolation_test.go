package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/usermgr"
)

func TestUserAuthAndIsolation(t *testing.T) {
	tmpDir := t.TempDir()
	usersDB := filepath.Join(tmpDir, "users.json")

	mgr, err := usermgr.New(usersDB, "admin123456", "sk-admin-key")
	if err != nil {
		t.Fatalf("usermgr.New failed: %v", err)
	}

	// 注册普通用户 bob
	bob, err := mgr.Register("bob", "bob123456")
	if err != nil {
		t.Fatalf("Register bob failed: %v", err)
	}

	p := pool.New("")
	// 添加管理员私有账号（旧账号无 owner 或 owner="admin"）
	p.Add(&auth.Auth{UID: "admin_legacy", AccessToken: "tok_admin_leg", Domain: "copilot.tencent.com", Owner: ""})
	p.Add(&auth.Auth{UID: "admin_explicit", AccessToken: "tok_admin_exp", Domain: "copilot.tencent.com", Owner: "admin"})
	// 添加显式公共账号
	p.Add(&auth.Auth{UID: "pub_shared", AccessToken: "tok_pub", Domain: "copilot.tencent.com", Owner: "public"})
	// 添加属于 bob 的专属账号
	p.Add(&auth.Auth{UID: "bob1", AccessToken: "tok_bob", Domain: "copilot.tencent.com", Owner: bob.ID})
	// 添加属于 alice 的专属账号
	p.Add(&auth.Auth{UID: "alice1", AccessToken: "tok_alice", Domain: "copilot.tencent.com", Owner: "u_alice"})

	up := upstream.New()
	h := NewHandler(Config{
		Pool:         p,
		Upstream:     up,
		APIKey:       "sk-admin-key",
		UserMgr:      mgr,
		SoftCooldown: 600 * time.Second,
	})

	// 1. 测试使用 bob 的 API Key 请求鉴权
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+bob.APIKey)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Bob API Key should authenticate, got code %d: %s", w.Code, w.Body.String())
	}

	// 2. 测试无效 API Key
	reqBad := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	reqBad.Header.Set("Authorization", "Bearer sk-invalid-key")
	wBad := httptest.NewRecorder()
	h.ServeHTTP(wBad, reqBad)
	if wBad.Code != http.StatusUnauthorized {
		t.Fatalf("Invalid API Key should return 401, got code %d", wBad.Code)
	}

	// 3. 测试选号严格隔离逻辑：
	// bob 选号，只能选中 bob1 或 pub_shared，绝不能选到 admin_legacy、admin_explicit 或 alice1
	for i := 0; i < 20; i++ {
		picked := p.PickExcludingForRealmAndOwner(nil, "", "", bob.ID)
		if picked == nil {
			t.Fatalf("Pick for bob should return an account")
		}
		if picked.UID != "bob1" && picked.UID != "pub_shared" {
			t.Fatalf("Bob should NEVER pick unowned/admin/Alice's accounts, got: %s", picked.UID)
		}
	}

	// 4. 新用户 charlie 没有自己的账号，池内只有 pub_shared，绝不能选到管理员账号
	for i := 0; i < 20; i++ {
		picked := p.PickExcludingForRealmAndOwner(nil, "", "", "u_charlie")
		if picked == nil {
			t.Fatalf("Pick for charlie should return pub_shared")
		}
		if picked.UID != "pub_shared" {
			t.Fatalf("Charlie with no accounts should only pick pub_shared, got: %s", picked.UID)
		}
	}

	// 5. 管理员选号（owner="" 或 "admin"），全池账号均可调度
	pickedAdmin := p.PickExcludingForRealmAndOwner(nil, "", "", "admin")
	if pickedAdmin == nil {
		t.Fatalf("Admin pick should succeed")
	}

	// 6. 测试 Web 控制台 handleDashboardData：登录管理员后，即使磁盘 auth 目录无文件，池中账号也必须并集展示
	loginReq := httptest.NewRequest(http.MethodPost, "/ui/auth/login", strings.NewReader(`{"username":"admin","password":"admin123456"}`))
	loginW := httptest.NewRecorder()
	h.ServeHTTP(loginW, loginReq)
	if loginW.Code != http.StatusOK {
		t.Fatalf("Admin login failed with %d: %s", loginW.Code, loginW.Body.String())
	}
	cookie := loginW.Result().Cookies()[0]

	wData := httptest.NewRecorder()
	reqData := httptest.NewRequest(http.MethodGet, "/ui/data", nil)
	reqData.AddCookie(cookie)
	h.ServeHTTP(wData, reqData)
	if wData.Code != http.StatusOK {
		t.Fatalf("Dashboard data endpoint returned %d: %s", wData.Code, wData.Body.String())
	}
	body := wData.Body.String()
	if !strings.Contains(body, "admin_legacy") || !strings.Contains(body, "bob1") {
		t.Fatalf("Dashboard accounts must include pool accounts! body: %s", body)
	}
}

func TestAdminUserManagementEndpoints(t *testing.T) {
	tmpDir := t.TempDir()
	usersDB := filepath.Join(tmpDir, "users.json")
	mgr, _ := usermgr.New(usersDB, "admin123456", "sk-admin-key")
	mgr.Register("testuser", "oldpass123")

	h := NewHandler(Config{
		Pool:    pool.New(""),
		APIKey:  "sk-admin-key",
		UserMgr: mgr,
	})

	// 1. 管理员登录
	adminLoginReq := httptest.NewRequest(http.MethodPost, "/ui/auth/login", strings.NewReader(`{"username":"admin","password":"admin123456"}`))
	adminLoginW := httptest.NewRecorder()
	h.ServeHTTP(adminLoginW, adminLoginReq)
	if adminLoginW.Code != http.StatusOK {
		t.Fatalf("admin login failed: %d", adminLoginW.Code)
	}
	adminCookie := adminLoginW.Result().Cookies()[0]

	// 2. 普通用户登录
	userLoginReq := httptest.NewRequest(http.MethodPost, "/ui/auth/login", strings.NewReader(`{"username":"testuser","password":"oldpass123"}`))
	userLoginW := httptest.NewRecorder()
	h.ServeHTTP(userLoginW, userLoginReq)
	if userLoginW.Code != http.StatusOK {
		t.Fatalf("user login failed: %d", userLoginW.Code)
	}
	userCookie := userLoginW.Result().Cookies()[0]

	// 3. 普通用户尝试访问 /ui/admin/users -> 403
	forbidReq := httptest.NewRequest(http.MethodGet, "/ui/admin/users", nil)
	forbidReq.AddCookie(userCookie)
	forbidW := httptest.NewRecorder()
	h.ServeHTTP(forbidW, forbidReq)
	if forbidW.Code != http.StatusForbidden {
		t.Fatalf("normal user accessing admin endpoint should return 403, got %d", forbidW.Code)
	}

	// 4. 管理员访问 /ui/admin/users -> 200
	listReq := httptest.NewRequest(http.MethodGet, "/ui/admin/users", nil)
	listReq.AddCookie(adminCookie)
	listW := httptest.NewRecorder()
	h.ServeHTTP(listW, listReq)
	if listW.Code != http.StatusOK {
		t.Fatalf("admin listing users failed: %d %s", listW.Code, listW.Body.String())
	}
	if !strings.Contains(listW.Body.String(), "testuser") || !strings.Contains(listW.Body.String(), "admin") {
		t.Fatalf("users list missing users: %s", listW.Body.String())
	}

	// 5. 管理员重置 testuser 密码
	resetPwReq := httptest.NewRequest(http.MethodPost, "/ui/admin/user/reset_password", strings.NewReader(`{"username":"testuser","new_password":"newpass666"}`))
	resetPwReq.AddCookie(adminCookie)
	resetPwW := httptest.NewRecorder()
	h.ServeHTTP(resetPwW, resetPwReq)
	if resetPwW.Code != http.StatusOK {
		t.Fatalf("reset password failed: %d %s", resetPwW.Code, resetPwW.Body.String())
	}

	// 用新密码登录应成功，旧密码登录应失败
	authNew, errNew := mgr.Authenticate("testuser", "newpass666")
	if errNew != nil || authNew == nil {
		t.Fatalf("authenticate with new password failed: %v", errNew)
	}
	_, errOld := mgr.Authenticate("testuser", "oldpass123")
	if errOld == nil {
		t.Fatalf("old password should no longer work")
	}

	// 6. 管理员重置 testuser API Key
	oldKey := authNew.APIKey
	resetKeyReq := httptest.NewRequest(http.MethodPost, "/ui/admin/user/reset_key", strings.NewReader(`{"username":"testuser"}`))
	resetKeyReq.AddCookie(adminCookie)
	resetKeyW := httptest.NewRecorder()
	h.ServeHTTP(resetKeyW, resetKeyReq)
	if resetKeyW.Code != http.StatusOK {
		t.Fatalf("reset key failed: %d %s", resetKeyW.Code, resetKeyW.Body.String())
	}
	uFresh, _ := mgr.FindByUsername("testuser")
	if uFresh.APIKey == oldKey {
		t.Fatalf("API Key was not updated")
	}
}

