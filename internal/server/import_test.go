package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

func TestExtractAuthItems(t *testing.T) {
	// 1. 单个 JSON 对象
	single := []byte(`{"accessToken":"token1","uid":"u1"}`)
	items := extractAuthItems(single, "single.json")
	if len(items) != 1 || items[0].name != "single.json" {
		t.Fatalf("single item extraction failed: %+v", items)
	}

	// 2. JSON 数组
	arr := []byte(`[
		{"accessToken":"token1","uid":"u1"},
		{"accessToken":"token2","uid":"u2"}
	]`)
	items = extractAuthItems(arr, "batch.json")
	if len(items) != 2 || items[0].name != "batch.json#1" || items[1].name != "batch.json#2" {
		t.Fatalf("array extraction failed: %+v", items)
	}

	// 3. 包装对象: accounts / data / items / auths
	wrap := []byte(`{
		"status": 200,
		"accounts": [
			{"accessToken":"token_wrap1","uid":"w1"},
			{"accessToken":"token_wrap2","uid":"w2"}
		]
	}`)
	items = extractAuthItems(wrap, "wrapped.json")
	if len(items) != 2 || items[0].name != "wrapped.json#1" {
		t.Fatalf("wrapped extraction failed: %+v", items)
	}

	// 4. NDJSON / 换行流式 JSON
	ndjson := []byte("{\"accessToken\":\"token_nd1\",\"uid\":\"nd1\"}\n{\"accessToken\":\"token_nd2\",\"uid\":\"nd2\"}")
	items = extractAuthItems(ndjson, "stream.json")
	if len(items) != 2 || items[0].name != "stream.json#1" || items[1].name != "stream.json#2" {
		t.Fatalf("ndjson extraction failed: %+v", items)
	}
}

func TestBatchImportJSON(t *testing.T) {
	authDir := t.TempDir()
	p := pool.New("")
	h := NewHandler(Config{
		Pool:    p,
		AuthDir: authDir,
	})

	// 1. 测试粘贴 JSON 数组批量导入
	payload := `[
		{
			"auth": {"accessToken": "at_batch_1", "refreshToken": "rt_batch_1", "realm": "cn"},
			"account": {"uid": "batch_user_1", "nickname": "用户1"}
		},
		{
			"auth": {"accessToken": "at_batch_2", "refreshToken": "rt_batch_2", "realm": "global"},
			"account": {"uid": "batch_user_2", "nickname": "用户2"}
		}
	]`
	reqBody, _ := json.Marshal(map[string]string{"content": payload})
	req := httptest.NewRequest(http.MethodPost, "/ui/oauth/batch_import", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("batch import HTTP status %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Success      bool     `json:"success"`
		Total        int      `json:"total"`
		SuccessCount int      `json:"success_count"`
		FailedCount  int      `json:"failed_count"`
		SuccessUIDs  []string `json:"success_uids"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal resp: %v", err)
	}
	if !resp.Success || resp.Total != 2 || resp.SuccessCount != 2 || resp.FailedCount != 0 {
		t.Fatalf("unexpected resp: %+v", resp)
	}

	// 验证磁盘落盘
	files, err := auth.LoadAuthFiles(authDir)
	if err != nil || len(files) != 2 {
		t.Fatalf("expected 2 files in authDir, got %v (err=%v)", files, err)
	}

	// 验证池同步
	list := p.List()
	if len(list) != 2 {
		t.Fatalf("expected 2 accounts in pool, got %d", len(list))
	}
}

func TestBatchImportZipAndFiltering(t *testing.T) {
	authDir := t.TempDir()
	p := pool.New("")
	h := NewHandler(Config{
		Pool:    p,
		AuthDir: authDir,
	})

	// 创建一个测试 ZIP 内存包，包含：
	// 1. workbuddy-u101.json (正常单账号)
	// 2. nested/accounts.json (JSON 数组包含 2 个账号)
	// 3. __MACOSX/._workbuddy-u101.json (macOS 二进制元数据，应被自动过滤)
	// 4. .DS_Store (隐藏文件，应被忽略)
	buf := new(bytes.Buffer)
	zw := zip.NewWriter(buf)

	// 1. workbuddy-u101.json
	f1, _ := zw.Create("workbuddy-u101.json")
	_, _ = f1.Write([]byte(`{"accessToken":"at_101","refreshToken":"rt_101","uid":"u101"}`))

	// 2. nested/accounts.json (包含数组)
	f2, _ := zw.Create("nested/accounts.json")
	_, _ = f2.Write([]byte(`[
		{"accessToken":"at_arr1","uid":"u_arr1"},
		{"accessToken":"at_arr2","uid":"u_arr2"}
	]`))

	// 3. __MACOSX 噪音文件
	fNoise, _ := zw.Create("__MACOSX/._workbuddy-u101.json")
	_, _ = fNoise.Write([]byte("\x00\x05\x16\x07AppleDoubleBinaryData"))

	// 4. .DS_Store
	fDs, _ := zw.Create(".DS_Store")
	_, _ = fDs.Write([]byte("dummy ds_store"))

	_ = zw.Close()

	// 构造 multipart 请求
	body := new(bytes.Buffer)
	mw := multipart.NewWriter(body)
	fw, _ := mw.CreateFormFile("file", "test_backup.zip")
	_, _ = io.Copy(fw, buf)
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/ui/oauth/batch_import", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("batch import zip failed %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Success      bool     `json:"success"`
		Total        int      `json:"total"`
		SuccessCount int      `json:"success_count"`
		FailedCount  int      `json:"failed_count"`
		FailReasons  []string `json:"fail_reasons"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal resp: %v", err)
	}

	if !resp.Success || resp.SuccessCount != 3 || resp.FailedCount != 0 {
		t.Fatalf("expected 3 successful imports without failures, got %+v", resp)
	}

	// 验证账号在池中存在
	if p.AuthByUID("u101") == nil || p.AuthByUID("u_arr1") == nil || p.AuthByUID("u_arr2") == nil {
		t.Errorf("imported accounts missing from pool")
	}
}

func TestBatchImportAllFailed(t *testing.T) {
	authDir := t.TempDir()
	h := NewHandler(Config{
		Pool:    pool.New(""),
		AuthDir: authDir,
	})

	// 传入非法凭证（缺少 accessToken）
	reqBody, _ := json.Marshal(map[string]string{"content": `[{"uid":"bad1"},{"uid":"bad2"}]`})
	req := httptest.NewRequest(http.MethodPost, "/ui/oauth/batch_import", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var resp struct {
		Success      bool     `json:"success"`
		SuccessCount int      `json:"success_count"`
		FailedCount  int      `json:"failed_count"`
		Error        string   `json:"error"`
		FailReasons  []string `json:"fail_reasons"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)

	if resp.Success {
		t.Fatalf("expected success: false when all fail, got %+v", resp)
	}
	if resp.FailedCount != 2 || len(resp.FailReasons) != 2 {
		t.Fatalf("expected 2 fail reasons, got %+v", resp)
	}
}

func TestSingleOAuthImportAssignsUID(t *testing.T) {
	authDir := t.TempDir()
	p := pool.New("")
	h := NewHandler(Config{
		Pool:    p,
		AuthDir: authDir,
	})

	// 没有提供 uid 的凭证
	doc := `{"auth":{"accessToken":"token_single_no_uid","refreshToken":"rt_single"}}`
	reqBody, _ := json.Marshal(map[string]string{"json": doc})
	req := httptest.NewRequest(http.MethodPost, "/ui/oauth/import", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("single import failed: %d %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Success  bool   `json:"success"`
		UID      string `json:"uid"`
		Filename string `json:"filename"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.Success || resp.UID == "" {
		t.Fatalf("expected non-empty assigned UID, got %+v", resp)
	}

	// 确认池内对象的 UID 非空
	a := p.AuthByUID(resp.UID)
	if a == nil {
		t.Fatalf("expected account %s in pool", resp.UID)
	}
	if a.UID != resp.UID {
		t.Errorf("expected a.UID == %s, got %s", resp.UID, a.UID)
	}

	// 确认磁盘保存内容也有该 UID
	target := filepath.Join(authDir, resp.Filename)
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	parsed, err := auth.Parse(raw)
	if err != nil {
		t.Fatalf("parse saved file: %v", err)
	}
	if parsed.UID != resp.UID {
		t.Errorf("saved file uid=%s, expected %s", parsed.UID, resp.UID)
	}
}

func TestBatchImportTxtAndRawToken(t *testing.T) {
	authDir := t.TempDir()
	p := pool.New("")
	h := NewHandler(Config{
		Pool:    p,
		AuthDir: authDir,
	})

	// 1. 粘贴带分隔符的原始 Token 导入: eyJ...----refresh_tok
	pastePayload := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.e30.test_signature----my_refresh_123"
	reqBody, _ := json.Marshal(map[string]string{"content": pastePayload})
	req := httptest.NewRequest(http.MethodPost, "/ui/oauth/batch_import", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("batch import raw token status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Success      bool     `json:"success"`
		SuccessCount int      `json:"success_count"`
		SuccessUIDs  []string `json:"success_uids"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || !resp.Success || resp.SuccessCount != 1 {
		t.Fatalf("unexpected resp for raw token: %+v (err: %v)", resp, err)
	}

	// 2. 上传 .txt 文件包含多行 token 凭证
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "tokens.txt")
	txtContent := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.e30.sig1----ref1\neyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.e30.sig2----ref2\n"
	_, _ = io.WriteString(fw, txtContent)
	_ = mw.Close()

	uploadReq := httptest.NewRequest(http.MethodPost, "/ui/oauth/batch_import", &buf)
	uploadReq.Header.Set("Content-Type", mw.FormDataContentType())
	uploadRec := httptest.NewRecorder()
	h.ServeHTTP(uploadRec, uploadReq)

	if uploadRec.Code != http.StatusOK {
		t.Fatalf("upload txt status %d: %s", uploadRec.Code, uploadRec.Body.String())
	}
	var uploadResp struct {
		Success      bool `json:"success"`
		SuccessCount int  `json:"success_count"`
	}
	if err := json.Unmarshal(uploadRec.Body.Bytes(), &uploadResp); err != nil || !uploadResp.Success || uploadResp.SuccessCount != 2 {
		t.Fatalf("unexpected resp for txt upload: %+v", uploadResp)
	}
}

func TestBatchImportWithRealmSpecification(t *testing.T) {
	authDir := t.TempDir()
	p := pool.New("")
	h := NewHandler(Config{
		Pool:    p,
		AuthDir: authDir,
	})

	// 1. 指定 realm 为 global
	rawToken := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.e30.glob_sig----ref_global_1"
	reqBody, _ := json.Marshal(map[string]string{
		"content": rawToken,
		"realm":   "global",
	})
	req := httptest.NewRequest(http.MethodPost, "/ui/oauth/batch_import", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("batch import status %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Success      bool     `json:"success"`
		SuccessCount int      `json:"success_count"`
		SuccessUIDs  []string `json:"success_uids"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.SuccessCount != 1 {
		t.Fatalf("failed resp: %+v", resp)
	}

	importedUID := resp.SuccessUIDs[0]
	a := p.AuthByUID(importedUID)
	if a == nil {
		t.Fatalf("account %s not found in pool", importedUID)
	}
	if a.Realm() != "global" {
		t.Errorf("expected realm 'global', got %q", a.Realm())
	}
	if a.Domain != "www.workbuddy.ai" {
		t.Errorf("expected domain 'www.workbuddy.ai', got %q", a.Domain)
	}
}


