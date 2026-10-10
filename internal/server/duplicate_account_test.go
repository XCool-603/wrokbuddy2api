package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

func TestDuplicateAccountReproduction(t *testing.T) {
	authDir := t.TempDir()
	p := pool.New("")
	h := &Handler{
		cfg: Config{
			AuthDir: authDir,
			Pool:    p,
		},
	}

	// 1. 模拟 OAuth 授权 Google 账号 (email: same@example.com)
	googleJSON := `{
		"auth": {
			"accessToken": "google_at_1",
			"refreshToken": "google_rt_1",
			"domain": "www.workbuddy.ai",
			"realm": "global",
			"provider": "google"
		},
		"account": {
			"uid": "same@example.com",
			"email": "same@example.com",
			"nickname": "same@example.com",
			"provider": "google"
		}
	}`
	req1 := httptest.NewRequest(http.MethodPost, "/ui/oauth/import", bytes.NewReader([]byte(`{"json":`+quoteJSON(googleJSON)+`}`)))
	req1.Header.Set("Content-Type", "application/json")
	rec1 := httptest.NewRecorder()
	h.handleOAuthImport(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("import Google failed: %d %s", rec1.Code, rec1.Body.String())
	}

	// 2. 模拟 OAuth 授权 Twitter 账号 (相同 email: same@example.com)
	twitterJSON := `{
		"auth": {
			"accessToken": "twitter_at_1",
			"refreshToken": "twitter_rt_1",
			"domain": "www.workbuddy.ai",
			"realm": "global",
			"provider": "twitter"
		},
		"account": {
			"uid": "same@example.com",
			"email": "same@example.com",
			"nickname": "same@example.com",
			"provider": "twitter"
		}
	}`
	req2 := httptest.NewRequest(http.MethodPost, "/ui/oauth/import", bytes.NewReader([]byte(`{"json":`+quoteJSON(twitterJSON)+`}`)))
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	h.handleOAuthImport(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("import Twitter failed: %d %s", rec2.Code, rec2.Body.String())
	}

	// 3. 模拟目录扫描或同步 (比如目录监听触发)
	auths, err := auth.LoadDir(authDir)
	if err != nil {
		t.Fatalf("LoadDir failed: %v", err)
	}
	p.SyncToDir(auths)

	// 4. 调用 dashboard data
	reqDash := httptest.NewRequest(http.MethodGet, "/ui/data", nil)
	recDash := httptest.NewRecorder()
	h.handleDashboardData(recDash, reqDash)
	if recDash.Code != http.StatusOK {
		t.Fatalf("dashboard data status %d", recDash.Code)
	}

	var data struct {
		Accounts []struct {
			UID      string `json:"uid"`
			Realm    string `json:"realm"`
			Provider string `json:"provider"`
			Filename string `json:"filename"`
			Nickname string `json:"nickname"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(recDash.Body.Bytes(), &data); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	t.Logf("Files on disk: %d", len(auths))
	for i, a := range auths {
		t.Logf("  File %d: path=%s uid=%s provider=%s", i, a.FilePath, a.UID, a.Provider())
	}
	t.Logf("Accounts in dashboard: %d", len(data.Accounts))
	for i, acct := range data.Accounts {
		t.Logf("  Account %d: uid=%s realm=%s provider=%s file=%s nick=%s",
			i, acct.UID, acct.Realm, acct.Provider, acct.Filename, acct.Nickname)
	}

	if len(data.Accounts) != 2 {
		t.Errorf("EXPECTED 2 ACCOUNTS, GOT %d", len(data.Accounts))
	}

	// 5. 现在模拟用户再次通过网页 OAuth 重新授权了这两个账号（OAuth 颁发了全新的 access_token 和 refresh_token）
	reauthGoogleJSON := `{
		"auth": {
			"accessToken": "google_at_NEW_TOKEN",
			"refreshToken": "google_rt_NEW_TOKEN",
			"domain": "www.workbuddy.ai",
			"realm": "global",
			"provider": "google"
		},
		"account": {
			"uid": "same@example.com",
			"email": "same@example.com",
			"nickname": "same@example.com",
			"provider": "google"
		}
	}`
	reqRe1 := httptest.NewRequest(http.MethodPost, "/ui/oauth/import", bytes.NewReader([]byte(`{"json":`+quoteJSON(reauthGoogleJSON)+`}`)))
	reqRe1.Header.Set("Content-Type", "application/json")
	recRe1 := httptest.NewRecorder()
	h.handleOAuthImport(recRe1, reqRe1)

	reauthTwitterJSON := `{
		"auth": {
			"accessToken": "twitter_at_NEW_TOKEN",
			"refreshToken": "twitter_rt_NEW_TOKEN",
			"domain": "www.workbuddy.ai",
			"realm": "global",
			"provider": "twitter"
		},
		"account": {
			"uid": "same@example.com",
			"email": "same@example.com",
			"nickname": "same@example.com",
			"provider": "twitter"
		}
	}`
	reqRe2 := httptest.NewRequest(http.MethodPost, "/ui/oauth/import", bytes.NewReader([]byte(`{"json":`+quoteJSON(reauthTwitterJSON)+`}`)))
	reqRe2.Header.Set("Content-Type", "application/json")
	recRe2 := httptest.NewRecorder()
	h.handleOAuthImport(recRe2, reqRe2)

	// 再次触发目录同步
	authsAfter, _ := auth.LoadDir(authDir)
	p.SyncToDir(authsAfter)

	// 再次调用 dashboard data
	reqDash2 := httptest.NewRequest(http.MethodGet, "/ui/data", nil)
	recDash2 := httptest.NewRecorder()
	h.handleDashboardData(recDash2, reqDash2)
	var data2 struct {
		Accounts []struct {
			UID      string `json:"uid"`
			Realm    string `json:"realm"`
			Provider string `json:"provider"`
			Filename string `json:"filename"`
			Nickname string `json:"nickname"`
		} `json:"accounts"`
	}
	_ = json.Unmarshal(recDash2.Body.Bytes(), &data2)

	t.Logf("AFTER REAUTH: Files on disk: %d", len(authsAfter))
	for i, a := range authsAfter {
		t.Logf("  File %d: path=%s uid=%s provider=%s", i, a.FilePath, a.UID, a.Provider())
	}
	t.Logf("AFTER REAUTH: Accounts in dashboard: %d", len(data2.Accounts))
	for i, acct := range data2.Accounts {
		t.Logf("  Account %d: uid=%s realm=%s provider=%s file=%s nick=%s",
			i, acct.UID, acct.Realm, acct.Provider, acct.Filename, acct.Nickname)
	}

	if len(data2.Accounts) != 2 {
		t.Fatalf("BUG REPRODUCED: expected 2 accounts after reauth, but got %d!", len(data2.Accounts))
	}

	// 6. 极端测试：模拟磁盘上因历史旧版本已经存在了 -2.json 冗余文件，验证自动清理与控制台去重
	dupGooglePath := filepath.Join(authDir, "workbuddy-global-google-same@example.com-2.json")
	_ = os.WriteFile(dupGooglePath, []byte(reauthGoogleJSON), 0644)
	dupTwitterPath := filepath.Join(authDir, "workbuddy-global-twitter-same@example.com-2.json")
	_ = os.WriteFile(dupTwitterPath, []byte(reauthTwitterJSON), 0644)

	// 触发目录扫描与同步
	authsWithDups, _ := auth.LoadDir(authDir)
	if len(authsWithDups) != 4 {
		t.Fatalf("expected 4 files on disk before cleanup, got %d", len(authsWithDups))
	}
	p.SyncToDir(authsWithDups)

	// 再次调用 dashboard data
	reqDash3 := httptest.NewRequest(http.MethodGet, "/ui/data", nil)
	recDash3 := httptest.NewRecorder()
	h.handleDashboardData(recDash3, reqDash3)
	var data3 struct {
		Accounts []struct {
			UID      string `json:"uid"`
			Realm    string `json:"realm"`
			Provider string `json:"provider"`
			Filename string `json:"filename"`
			Nickname string `json:"nickname"`
		} `json:"accounts"`
	}
	_ = json.Unmarshal(recDash3.Body.Bytes(), &data3)

	t.Logf("WITH LEGACY DUPLICATES: Accounts in dashboard: %d", len(data3.Accounts))
	for i, acct := range data3.Accounts {
		t.Logf("  Account %d: uid=%s realm=%s provider=%s file=%s nick=%s",
			i, acct.UID, acct.Realm, acct.Provider, acct.Filename, acct.Nickname)
	}

	if len(data3.Accounts) != 2 {
		t.Fatalf("EXPECTED EXACTLY 2 ACCOUNTS, GOT %d", len(data3.Accounts))
	}

	// 并且历史遗留的 -2.json 冗余文件已被自动清理
	filesCleaned, _ := auth.LoadAuthFiles(authDir)
	if len(filesCleaned) != 2 {
		t.Fatalf("expected -2.json duplicate files to be automatically cleaned up, got %d files: %v", len(filesCleaned), filesCleaned)
	}
}


