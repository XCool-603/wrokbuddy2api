package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/auth"
)

func TestDefaultInviteCodeAndURL(t *testing.T) {
	aCN := &auth.Auth{
		UID: "wb_user_12345",
	}
	codeCN := DefaultInviteCode(aCN)
	if codeCN != "user_12345" && codeCN != "wb_user_12345" {
		t.Errorf("unexpected code: %s", codeCN)
	}
	urlCN := DefaultInviteURL(aCN, codeCN)
	if urlCN != "https://workbuddy.cn/i/"+codeCN {
		t.Errorf("unexpected url: %s", urlCN)
	}

	aGlobal := &auth.Auth{
		UID:    "wb_user_99999",
		Domain: "workbuddy.ai",
	}
	codeGlobal := DefaultInviteCode(aGlobal)
	urlGlobal := DefaultInviteURL(aGlobal, codeGlobal)
	if urlGlobal != "https://www.workbuddy.ai/i/"+codeGlobal {
		t.Errorf("unexpected global url: %s", urlGlobal)
	}
}

func TestFetchInviteInfoFallback(t *testing.T) {
	c := New()
	a := &auth.Auth{
		UID: "test_uid_888",
	}
	info, err := c.FetchInviteInfo(a)
	if err != nil {
		t.Fatalf("FetchInviteInfo failed: %v", err)
	}
	if info.InviteCode == "" {
		t.Errorf("expected non-empty invite code")
	}
	if info.InviteURL == "" {
		t.Errorf("expected non-empty invite url")
	}
}

func TestFetchInviteInfoUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/activity/growth/user/invite" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{
					"invite_code":   "SPEC_INVITE_666",
					"invite_url":    "https://workbuddy.cn/i/SPEC_INVITE_666",
					"invited_count": 5,
					"reward_credit": 150,
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := New()
	c.ChatBaseCN = srv.URL
	a := &auth.Auth{
		UID:         "up_uid_777",
		AccessToken: "mock_at",
	}

	info, err := c.FetchInviteInfo(a)
	if err != nil {
		t.Fatalf("FetchInviteInfo failed: %v", err)
	}
	if info.InviteCode != "SPEC_INVITE_666" {
		t.Errorf("got %s, want SPEC_INVITE_666", info.InviteCode)
	}
	if info.InvitedCount != 5 {
		t.Errorf("got count %d, want 5", info.InvitedCount)
	}
	if info.RewardCredit != 150 {
		t.Errorf("got reward %d, want 150", info.RewardCredit)
	}
}
