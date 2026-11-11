package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/pool"
)

func TestParseWhitelistEntries(t *testing.T) {
	prefixes, norm, err := ParseWhitelistEntries([]string{
		"203.0.113.5, 192.168.1.0/24 # office lan",
		"  ::ffff:203.0.113.5\n2001:db8::/32 ",
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(prefixes) != 3 || len(norm) != 3 {
		t.Fatalf("got prefixes=%d norm=%v, want 3 deduplicated entries", len(prefixes), norm)
	}
	if norm[0] != "203.0.113.5" || norm[1] != "192.168.1.0/24" || norm[2] != "2001:db8::/32" {
		t.Errorf("unexpected normalized entries: %v", norm)
	}

	if _, _, err := ParseWhitelistEntries([]string{"not-an-ip"}); err == nil {
		t.Errorf("expected invalid IP error, got nil")
	}
	if _, _, err := ParseWhitelistEntries([]string{"10.0.0.1/99"}); err == nil {
		t.Errorf("expected invalid CIDR error, got nil")
	}
}

func TestIPWhitelistEnforcedAndSpoofResistant(t *testing.T) {
	p := testPoolWith()
	h := NewHandler(Config{
		Pool:               p,
		APIKey:             "secret-key",
		IPWhitelistEnabled: true,
		AllowedIPs:         []string{"203.0.113.10", "10.20.0.0/16"},
	})

	doModels := func(remoteAddr, xff, apiKey string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.RemoteAddr = remoteAddr
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// 1. Direct public attacker with valid key AND spoofed X-Forwarded-For: 127.0.0.1 MUST be blocked (403).
	if rec := doModels("198.51.100.77:5555", "127.0.0.1", "secret-key"); rec.Code != http.StatusForbidden {
		t.Fatalf("public attacker with spoofed XFF got %d (%s), want 403", rec.Code, rec.Body.String())
	}

	// 2. Trusted local proxy forwarding an untrusted client IP MUST check the forwarded client IP -> 403.
	if rec := doModels("127.0.0.1:43210", "198.51.100.77", "secret-key"); rec.Code != http.StatusForbidden {
		t.Fatalf("proxied untrusted IP got %d (%s), want 403", rec.Code, rec.Body.String())
	}

	// 3. Allowed explicit IP with valid key -> 200.
	if rec := doModels("203.0.113.10:1234", "", "secret-key"); rec.Code != http.StatusOK {
		t.Fatalf("allowed IP got %d (%s), want 200", rec.Code, rec.Body.String())
	}

	// 4. Allowed CIDR via trusted proxy -> 200.
	if rec := doModels("127.0.0.1:1234", "10.20.5.6", "secret-key"); rec.Code != http.StatusOK {
		t.Fatalf("allowed CIDR via proxy got %d (%s), want 200", rec.Code, rec.Body.String())
	}

	// 5. Loopback allowed by default (AllowLocalhost=true) -> 200, then blocked when AllowLocalhost=false.
	if rec := doModels("127.0.0.1:1234", "", "secret-key"); rec.Code != http.StatusOK {
		t.Fatalf("loopback default got %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if _, err := h.SetIPWhitelist(true, []string{"203.0.113.10"}, false); err != nil {
		t.Fatal(err)
	}
	if rec := doModels("127.0.0.1:1234", "", "secret-key"); rec.Code != http.StatusForbidden {
		t.Fatalf("loopback when AllowLocalhost=false got %d (%s), want 403", rec.Code, rec.Body.String())
	}
}

func TestAPIKeyBruteForceLock(t *testing.T) {
	scannerIP := "198.51.100.222"
	globalAuthLimiter.resetAPIKeyFail(scannerIP)
	defer globalAuthLimiter.resetAPIKeyFail(scannerIP)

	h := NewHandler(Config{
		Pool:   pool.New(""),
		APIKey: "real-secret",
	})

	for i := 0; i < 25; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.RemoteAddr = scannerIP + ":9000"
		req.Header.Set("Authorization", "Bearer wrong-guess")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d got %d, want 401", i+1, rec.Code)
		}
	}

	// 26th request even with correct key is temporarily rate-locked (429).
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.RemoteAddr = scannerIP + ":9000"
	req.Header.Set("Authorization", "Bearer real-secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("locked scanner got %d (%s), want 429", rec.Code, rec.Body.String())
	}
}

func TestHandleConfigIPWhitelistAndRuntimeInfo(t *testing.T) {
	tmpDir := t.TempDir()
	stateFile := filepath.Join(tmpDir, "data", "state.json")
	h := NewHandler(Config{
		Pool:       pool.New(""),
		AuthDir:    tmpDir,
		StateFile:  stateFile,
		ConfigPath: filepath.Join(tmpDir, "config.json"),
		ListenAddr: ":7863",
	})

	// 1. Invalid CIDR rejected with 400
	badBody := []byte(`{"enabled":true,"allowed_ips_text":"bad-cidr/xyz"}`)
	reqBad := httptest.NewRequest(http.MethodPost, "/ui/config/ip_whitelist", bytes.NewReader(badBody))
	recBad := httptest.NewRecorder()
	h.handleConfigIPWhitelist(recBad, reqBad)
	if recBad.Code != http.StatusBadRequest {
		t.Fatalf("invalid CIDR got %d (%s), want 400", recBad.Code, recBad.Body.String())
	}

	// 2. Valid update saved & enforced & persisted to settings.json
	okBody := []byte(`{"enabled":true,"allowed_ips_text":"203.0.113.9, 10.8.0.0/16","allow_localhost":true}`)
	reqOK := httptest.NewRequest(http.MethodPost, "/ui/config/ip_whitelist", bytes.NewReader(okBody))
	reqOK.RemoteAddr = "203.0.113.9:3333"
	recOK := httptest.NewRecorder()
	h.handleConfigIPWhitelist(recOK, reqOK)
	if recOK.Code != http.StatusOK {
		t.Fatalf("valid update got %d (%s), want 200", recOK.Code, recOK.Body.String())
	}

	settingsRaw, err := os.ReadFile(filepath.Join(tmpDir, "data", "settings.json"))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	if !strings.Contains(string(settingsRaw), `"203.0.113.9"`) || !strings.Contains(string(settingsRaw), `"ip_whitelist_enabled": true`) {
		t.Errorf("settings.json missing persisted whitelist: %s", string(settingsRaw))
	}

	// 3. /ui/data exposes ipWhitelist & networkInfo (listen_addr, running_ips, client_ip)
	reqDash := httptest.NewRequest(http.MethodGet, "/ui/data", nil)
	reqDash.RemoteAddr = "203.0.113.9:4444"
	recDash := httptest.NewRecorder()
	h.handleDashboardData(recDash, reqDash)
	if recDash.Code != http.StatusOK {
		t.Fatalf("/ui/data got %d", recDash.Code)
	}
	var dash struct {
		IPWhitelist IPWhitelistConfig  `json:"ipWhitelist"`
		NetworkInfo RuntimeNetworkInfo `json:"networkInfo"`
	}
	if err := json.Unmarshal(recDash.Body.Bytes(), &dash); err != nil {
		t.Fatalf("unmarshal /ui/data: %v", err)
	}
	if !dash.IPWhitelist.Enabled || len(dash.IPWhitelist.AllowedIPs) != 2 {
		t.Errorf("unexpected ipWhitelist: %+v", dash.IPWhitelist)
	}
	if dash.NetworkInfo.ListenAddr != ":7863" || dash.NetworkInfo.ClientIP != "203.0.113.9" || len(dash.NetworkInfo.RunningIPs) == 0 {
		t.Errorf("unexpected networkInfo: %+v", dash.NetworkInfo)
	}
}
