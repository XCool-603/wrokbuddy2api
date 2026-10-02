package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/pool"
)

func TestOAuthFlow(t *testing.T) {
	// 1. 模拟上游 CodeBuddy / WorkBuddy 授权服务器
	var tokenCalls int
	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{
					"state":   "test_state_12345",
					"authUrl": "https://fake.upstream/auth?state=test_state_12345",
				},
			})
		case "/v2/plugin/auth/token":
			tokenCalls++
			if tokenCalls == 1 {
				// 第一次轮询：模拟正在授权中
				json.NewEncoder(w).Encode(map[string]any{
					"code": 10001,
					"msg":  "login ing",
					"data": nil,
				})
				return
			}
			if tokenCalls == 2 {
				// 第二次轮询：模拟网络颠簸或 502 网关重试
				w.WriteHeader(http.StatusBadGateway)
				w.Write([]byte("502 Bad Gateway"))
				return
			}
			// 第三次轮询：授权成功返回 token (带 JWT payload)
			// JWT payload: {"sub":"user_oauth_999","name":"OAuthUser"}
			// Base64URL( {"sub":"user_oauth_999","name":"OAuthUser"} ) = eyJzdWIiOiJ1c2VyX29hdXRoXzk5OSIsIm5hbWUiOiJPYXV0aFVzZXIifQ
			jwtToken := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1c2VyX29hdXRoXzk5OSIsIm5hbWUiOiJPYXV0aFVzZXIifQ.sig"
			json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"msg":  "success",
				"data": map[string]any{
					"accessToken":  jwtToken,
					"refreshToken": "rt_oauth_999",
					"expiresIn":    7200,
					"domain":       "",
				},
			})
		case "/v2/plugin/login/account":
			json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{
					"uid":      "user_oauth_999",
					"nickname": "OAuthUser",
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer fakeUpstream.Close()

	authDir := t.TempDir()
	p := pool.New("")
	h := NewHandler(Config{
		Pool:    p,
		AuthDir: authDir,
	})

	// 注入 mock upstream 地址
	oauthSessionMu.Lock()
	oauthSessions["test_state_12345"] = oauthSession{Realm: "cn", CreatedAt: time.Now()}
	oauthLatestState["cn"] = "test_state_12345"
	oauthSessionMu.Unlock()

	// 2. 测试 handleOAuthPoll - 第一次轮询（waiting: true）
	pollReq1 := httptest.NewRequest(http.MethodPost, "/ui/oauth/poll", strings.NewReader(`{"realm":"cn","state":"test_state_12345"}`))
	pollReq1.Header.Set("Content-Type", "application/json")
	pollRec1 := httptest.NewRecorder()

	// 替换临时 mock client
	origTransport := http.DefaultTransport
	defer func() { http.DefaultTransport = origTransport }()

	// 使用 custom roundtripper 劫持 copilot.tencent.com 请求到 fakeUpstream
	http.DefaultTransport = &mockRewriteTransport{TargetURL: fakeUpstream.URL}

	h.ServeHTTP(pollRec1, pollReq1)
	if pollRec1.Code != http.StatusOK {
		t.Fatalf("poll 1 returned %d: %s", pollRec1.Code, pollRec1.Body.String())
	}
	var res1 map[string]any
	json.Unmarshal(pollRec1.Body.Bytes(), &res1)
	if res1["waiting"] != true {
		t.Fatalf("expected waiting: true, got %+v", res1)
	}

	// 3. 测试 handleOAuthPoll - 第二次轮询（网关 502 颠簸，不报错 500，优雅继续 waiting）
	pollReq2 := httptest.NewRequest(http.MethodPost, "/ui/oauth/poll", strings.NewReader(`{"realm":"cn","state":"test_state_12345"}`))
	pollReq2.Header.Set("Content-Type", "application/json")
	pollRec2 := httptest.NewRecorder()
	h.ServeHTTP(pollRec2, pollReq2)
	if pollRec2.Code != http.StatusOK {
		t.Fatalf("poll 2 returned %d (expected 200 waiting): %s", pollRec2.Code, pollRec2.Body.String())
	}
	var res2 map[string]any
	json.Unmarshal(pollRec2.Body.Bytes(), &res2)
	if res2["waiting"] != true {
		t.Fatalf("expected waiting: true on upstream 502, got %+v", res2)
	}

	// 4. 测试 handleOAuthPoll - 第三次轮询（授权成功入池）
	pollReq3 := httptest.NewRequest(http.MethodPost, "/ui/oauth/poll", strings.NewReader(`{"realm":"cn","state":"test_state_12345"}`))
	pollReq3.Header.Set("Content-Type", "application/json")
	pollRec3 := httptest.NewRecorder()
	h.ServeHTTP(pollRec3, pollReq3)
	if pollRec3.Code != http.StatusOK {
		t.Fatalf("poll 3 returned %d: %s", pollRec3.Code, pollRec3.Body.String())
	}
	var res3 struct {
		Success  bool   `json:"success"`
		UID      string `json:"uid"`
		Nickname string `json:"nickname"`
		Filename string `json:"filename"`
	}
	if err := json.Unmarshal(pollRec3.Body.Bytes(), &res3); err != nil || !res3.Success {
		t.Fatalf("unexpected poll 3 response: %+v", res3)
	}
	if res3.UID != "user_oauth_999" {
		t.Errorf("expected UID user_oauth_999, got %q", res3.UID)
	}

	// 验证池内账号
	a := p.AuthByUID("user_oauth_999")
	if a == nil {
		t.Fatalf("account user_oauth_999 not synced into pool")
	}
	if a.Domain != "copilot.tencent.com" {
		t.Errorf("expected domain fallback to copilot.tencent.com, got %q", a.Domain)
	}
}

type mockRewriteTransport struct {
	TargetURL string
}

func (m *mockRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	u, _ := req.URL.Parse(m.TargetURL + req.URL.Path + "?" + req.URL.RawQuery)
	cloned.URL = u
	cloned.Host = u.Host
	return http.DefaultClient.Transport.RoundTrip(cloned)
}

func init() {
	if http.DefaultClient.Transport == nil {
		http.DefaultClient.Transport = http.DefaultTransport
	}
}
