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
	"workbuddy2api/internal/upstream"
)

func TestCrossPlatformSameEmailImportAndCoexistence(t *testing.T) {
	authDir := t.TempDir()
	p := pool.New("")
	h := &Handler{
		cfg: Config{
			AuthDir: authDir,
			Pool:    p,
		},
	}

	// 1. 导入国内版账号（email: alice@example.com）
	cnJSON := `{
		"auth": {
			"accessToken": "cn_tok_1234567890",
			"refreshToken": "cn_refresh_123",
			"domain": "copilot.tencent.com",
			"realm": "cn"
		},
		"account": {
			"email": "alice@example.com",
			"nickname": "Alice CN"
		}
	}`
	req1 := httptest.NewRequest(http.MethodPost, "/ui/oauth/import", bytes.NewReader([]byte(`{"json":`+quoteJSON(cnJSON)+`}`)))
	req1.Header.Set("Content-Type", "application/json")
	rec1 := httptest.NewRecorder()
	h.handleOAuthImport(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("import CN failed: code=%d body=%s", rec1.Code, rec1.Body.String())
	}
	var res1 struct {
		Success  bool   `json:"success"`
		UID      string `json:"uid"`
		Filename string `json:"filename"`
	}
	if err := json.Unmarshal(rec1.Body.Bytes(), &res1); err != nil || !res1.Success {
		t.Fatalf("invalid CN response: %+v", res1)
	}

	// 2. 导入国际版账号（相同 email: alice@example.com）
	globalJSON := `{
		"auth": {
			"accessToken": "global_tok_1234567890",
			"refreshToken": "global_refresh_123",
			"domain": "www.workbuddy.ai",
			"realm": "global"
		},
		"account": {
			"email": "alice@example.com",
			"nickname": "Alice Global"
		}
	}`
	req2 := httptest.NewRequest(http.MethodPost, "/ui/oauth/import", bytes.NewReader([]byte(`{"json":`+quoteJSON(globalJSON)+`}`)))
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	h.handleOAuthImport(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("import Global failed: code=%d body=%s", rec2.Code, rec2.Body.String())
	}
	var res2 struct {
		Success  bool   `json:"success"`
		UID      string `json:"uid"`
		Filename string `json:"filename"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &res2); err != nil || !res2.Success {
		t.Fatalf("invalid Global response: %+v", res2)
	}

	// 3. 验证磁盘文件独立隔离：两个文件共存，互不覆盖
	files, err := auth.LoadAuthFiles(authDir)
	if err != nil {
		t.Fatalf("LoadAuthFiles failed: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files on disk, got %d: %v", len(files), files)
	}
	if res1.Filename == res2.Filename {
		t.Fatalf("filenames collided: %s == %s", res1.Filename, res2.Filename)
	}

	// 4. 验证池内共存与分池选号
	cnAcct := p.PickExcludingForRealm(nil, "", "cn")
	if cnAcct == nil {
		t.Fatalf("CN account missing from pool!")
	}
	if cnAcct.Realm() != "cn" {
		t.Errorf("expected realm cn, got %s", cnAcct.Realm())
	}
	if cnAcct.RawUID() != "alice@example.com" {
		t.Errorf("expected RawUID alice@example.com, got %s", cnAcct.RawUID())
	}

	globalAcct := p.PickExcludingForRealm(nil, "", "global")
	if globalAcct == nil {
		t.Fatalf("Global account missing from pool!")
	}
	if globalAcct.Realm() != "global" {
		t.Errorf("expected realm global, got %s", globalAcct.Realm())
	}
	if globalAcct.RawUID() != "alice@example.com" {
		t.Errorf("expected RawUID alice@example.com, got %s", globalAcct.RawUID())
	}

	// 5. 验证出站发送给上游的请求头：必须是纯净原始 UID，绝不泄漏内部隔离前缀
	upClient := upstream.New()
	reqUpCN, _ := http.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	upClient.ChatHeaders(reqUpCN, cnAcct, "", upstream.ChatMeta{})
	if uidHdr := reqUpCN.Header.Get("X-User-Id"); uidHdr != "alice@example.com" {
		t.Errorf("upstream CN X-User-Id leaked prefix: %q, want alice@example.com", uidHdr)
	}

	reqUpGL, _ := http.NewRequest(http.MethodPost, "https://www.workbuddy.ai/v2/chat/completions", nil)
	upClient.ChatHeaders(reqUpGL, globalAcct, "", upstream.ChatMeta{})
	if uidHdr := reqUpGL.Header.Get("X-User-Id"); uidHdr != "alice@example.com" {
		t.Errorf("upstream Global X-User-Id leaked prefix: %q, want alice@example.com", uidHdr)
	}

	// 6. 验证控制台 API 数据：两个账号均完整返回，各带正确 Realm 标签
	reqDash := httptest.NewRequest(http.MethodGet, "/ui/dashboard/data", nil)
	recDash := httptest.NewRecorder()
	h.handleDashboardData(recDash, reqDash)
	if recDash.Code != http.StatusOK {
		t.Fatalf("dashboard data returned %d: %s", recDash.Code, recDash.Body.String())
	}
	var dashData struct {
		Accounts []struct {
			UID      string `json:"uid"`
			Realm    string `json:"realm"`
			Nickname string `json:"nickname"`
			Filename string `json:"filename"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(recDash.Body.Bytes(), &dashData); err != nil {
		t.Fatalf("unmarshal dashboard data failed: %v", err)
	}
	if len(dashData.Accounts) != 2 {
		t.Fatalf("expected 2 accounts in dashboard data, got %d: %+v", len(dashData.Accounts), dashData.Accounts)
	}
	realmsFound := map[string]bool{}
	for _, a := range dashData.Accounts {
		realmsFound[a.Realm] = true
	}
	if !realmsFound["cn"] || !realmsFound["global"] {
		t.Errorf("expected both cn and global in dashboard data, got: %+v", realmsFound)
	}

	// 7. 验证删除隔离：删除 CN 账号凭证不影响国际版
	delReq := httptest.NewRequest(http.MethodPost, "/ui/action/delete", bytes.NewReader([]byte(`{"filename":"`+res1.Filename+`"}`)))
	delReq.Header.Set("Content-Type", "application/json")
	delRec := httptest.NewRecorder()
	h.handleActionDelete(delRec, delReq)
	if delRec.Code != http.StatusOK {
		t.Fatalf("delete CN returned %d: %s", delRec.Code, delRec.Body.String())
	}

	// CN 凭证已删除，但国际版账号文件与池内条目必须完好无损
	if _, err := os.Stat(filepath.Join(authDir, res1.Filename)); !os.IsNotExist(err) {
		t.Errorf("CN file %s should be deleted", res1.Filename)
	}
	if _, err := os.Stat(filepath.Join(authDir, res2.Filename)); err != nil {
		t.Errorf("Global file %s should still exist, got: %v", res2.Filename, err)
	}
	if g := p.PickExcludingForRealm(nil, "", "global"); g == nil {
		t.Errorf("Global account in pool should still be available after deleting CN account")
	}
}

func TestCrossPlatformSameEmailBatchImportAndSyncToDir(t *testing.T) {
	authDir := t.TempDir()
	p := pool.New("")
	h := &Handler{
		cfg: Config{
			AuthDir: authDir,
			Pool:    p,
		},
	}

	// 1. 批量导入：同一个 JSON 数组包含相同 email 的 CN 和 Global 凭证
	batchJSON := `[
		{
			"auth": {
				"accessToken": "tok_cn_batch",
				"refreshToken": "ref_cn_batch",
				"domain": "copilot.tencent.com",
				"realm": "cn"
			},
			"account": {
				"email": "bob@test.com",
				"nickname": "Bob CN"
			}
		},
		{
			"auth": {
				"accessToken": "tok_gl_batch",
				"refreshToken": "ref_gl_batch",
				"domain": "www.workbuddy.ai",
				"realm": "global"
			},
			"account": {
				"email": "bob@test.com",
				"nickname": "Bob Global"
			}
		}
	]`

	req := httptest.NewRequest(http.MethodPost, "/ui/oauth/import-batch", bytes.NewReader([]byte(`{"content":`+quoteJSON(batchJSON)+`}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.handleOAuthBatchImport(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("batch import failed: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var bRes struct {
		Success      bool     `json:"success"`
		SuccessCount int      `json:"success_count"`
		SuccessUIDs  []string `json:"success_uids"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &bRes); err != nil || !bRes.Success {
		t.Fatalf("batch response error: %+v", bRes)
	}
	if bRes.SuccessCount != 2 {
		t.Fatalf("expected 2 successful imports, got %d", bRes.SuccessCount)
	}

	// 2. 验证磁盘上有两个凭证文件
	files, err := auth.LoadAuthFiles(authDir)
	if err != nil || len(files) != 2 {
		t.Fatalf("expected 2 files on disk, got %d: %v", len(files), files)
	}

	// 3. 验证池内包含两个账号且能分别选号
	cn := p.PickExcludingForRealm(nil, "", "cn")
	if cn == nil || cn.Realm() != "cn" || cn.RawUID() != "bob@test.com" {
		t.Fatalf("failed to pick CN account: %+v", cn)
	}
	gl := p.PickExcludingForRealm(nil, "", "global")
	if gl == nil || gl.Realm() != "global" || gl.RawUID() != "bob@test.com" {
		t.Fatalf("failed to pick Global account: %+v", gl)
	}

	// 4. 仿真重启：新建 Pool，重新调用 LoadDir + SyncToDir
	pRestart := pool.New("")
	loadedAuths, err := auth.LoadDir(authDir)
	if err != nil {
		t.Fatalf("LoadDir failed: %v", err)
	}
	if len(loadedAuths) != 2 {
		t.Fatalf("expected 2 auths loaded, got %d", len(loadedAuths))
	}
	pRestart.SyncToDir(loadedAuths)

	// 重启后池内两个账号仍然健在，绝不相互覆盖
	cnRestart := pRestart.PickExcludingForRealm(nil, "", "cn")
	if cnRestart == nil || cnRestart.Realm() != "cn" || cnRestart.RawUID() != "bob@test.com" {
		t.Fatalf("restart pick CN failed: %+v", cnRestart)
	}
	glRestart := pRestart.PickExcludingForRealm(nil, "", "global")
	if glRestart == nil || glRestart.Realm() != "global" || glRestart.RawUID() != "bob@test.com" {
		t.Fatalf("restart pick Global failed: %+v", glRestart)
	}
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
