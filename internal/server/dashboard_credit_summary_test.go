package server

import (
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

func TestHandleDashboardDataCreditSummary(t *testing.T) {
	tmpDir := t.TempDir()

	cnFile := filepath.Join(tmpDir, "workbuddy-cn-sum.json")
	cnAuthJSON := `{
		"uid": "cn_sum_1",
		"access_token": "mock_at_cn",
		"nickname": "CN-Account",
		"realm": "cn"
	}`
	if err := os.WriteFile(cnFile, []byte(cnAuthJSON), 0600); err != nil {
		t.Fatal(err)
	}

	glFile := filepath.Join(tmpDir, "workbuddy-gl-sum.json")
	glAuthJSON := `{
		"uid": "gl_sum_1",
		"access_token": "mock_at_gl",
		"nickname": "GL-Account",
		"realm": "global"
	}`
	if err := os.WriteFile(glFile, []byte(glAuthJSON), 0600); err != nil {
		t.Fatal(err)
	}

	p := pool.New("")
	cnAuth, _ := auth.Parse([]byte(cnAuthJSON))
	cnAuth.FilePath = cnFile
	p.Add(cnAuth)

	glAuth, _ := auth.Parse([]byte(glAuthJSON))
	glAuth.FilePath = glFile
	p.Add(glAuth)

	p.CooldownFreeQuotaModel("gl_sum_1", time.Now().Add(time.Hour), "hy4free", "free quota exhausted")
	p.NoteModelCost("cn_sum_1", "claude-4.0-sonnet", 12.5, 5000)

	cnServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{
				"Response": map[string]any{
					"Data": map[string]any{
						"TotalDosage": 350,
						"Accounts": []map[string]any{
							{
								"PackageCode":                "pkg_cn_base",
								"PackageName":                "CodeBuddy个人体验版",
								"SubProductCode":             "sp_tcaca_codebuddy_ide",
								"Status":                     0,
								"CycleCapacitySizePrecise":   "200",
								"CycleCapacityRemainPrecise": "100.5",
								"CycleCapacityUsedPrecise":   "99.5",
							},
							{
								"PackageCode":           "pkg_cn_bonus",
								"PackageName":           "CodeBuddy个人版拉新权益包",
								"SubProductCode":        "sp_tcaca_codebuddyide_bonus_pack",
								"Status":                0,
								"CapacitySizePrecise":   "300",
								"CapacityRemainPrecise": "250.25",
								"CapacityUsedPrecise":   "49.75",
							},
						},
					},
				},
			},
		})
	}))
	defer cnServer.Close()

	glServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{
				"Response": map[string]any{
					"Data": map[string]any{
						"TotalDosage": 350,
						"Accounts": []map[string]any{
							{
								"PackageCode":           "pkg_gl_base",
								"PackageName":           "Free Plan Subscription",
								"SubProductCode":        "sp_tcaca_codebuddy_ide",
								"Status":                0,
								"CapacitySizePrecise":   "400",
								"CapacityRemainPrecise": "300",
								"CapacityUsedPrecise":   "100",
							},
							{
								"PackageCode":           "pkg_gl_purchased",
								"PackageName":           "Add-on Pack",
								"SubProductCode":        "sp_tcaca_addon",
								"Status":                0,
								"CapacitySizePrecise":   "100",
								"CapacityRemainPrecise": "50.5",
								"CapacityUsedPrecise":   "49.5",
							},
						},
					},
				},
			},
		})
	}))
	defer glServer.Close()

	up := upstream.New()
	up.ChatBaseCN = cnServer.URL
	up.BillingBaseCN = cnServer.URL
	up.ChatBaseGlobal = glServer.URL
	up.BillingBaseGlobal = glServer.URL
	up.GlobalEnabled = true

	h := NewHandler(Config{
		Pool:     p,
		Upstream: up,
		AuthDir:  tmpDir,
	})

	req := httptest.NewRequest(http.MethodGet, "/ui/data?fresh=1", nil)
	w := httptest.NewRecorder()
	h.handleDashboardData(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		CurrentVersion string               `json:"currentVersion"`
		CreditSummary  CreditSummaryPayload `json:"creditSummary"`
		Accounts       []struct {
			UID          string                    `json:"uid"`
			Realm        string                    `json:"realm"`
			CreditDetail *upstream.CreditBreakdown `json:"credit_detail"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response failed: %v", err)
	}
	if resp.CurrentVersion != CurrentVersion {
		t.Errorf("currentVersion = %q, want %q", resp.CurrentVersion, CurrentVersion)
	}
	if len(resp.Accounts) != 2 {
		t.Fatalf("accounts len = %d, want 2", len(resp.Accounts))
	}
	for _, a := range resp.Accounts {
		if a.CreditDetail == nil || len(a.CreditDetail.Packs) != 2 {
			t.Fatalf("account %s missing credit_detail packs: %+v", a.UID, a.CreditDetail)
		}
	}

	all := resp.CreditSummary.All
	if all.Remain != 701.25 || all.Total != 1000 || all.Used != 298.75 {
		t.Errorf("all totals = (%.2f / %.2f / %.2f), want (701.25 / 1000 / 298.75)", all.Remain, all.Total, all.Used)
	}
	if all.BaseRemain != 400.5 || all.BonusRemain != 250.25 || all.PurchasedRemain != 50.5 {
		t.Errorf("all buckets = (base=%.2f bonus=%.2f purch=%.2f), want (400.5 / 250.25 / 50.5)", all.BaseRemain, all.BonusRemain, all.PurchasedRemain)
	}
	if all.RateLimitedModelsCount != 1 || len(all.RateLimitedModels) != 1 || all.RateLimitedModels[0] != "hy4free" {
		t.Errorf("all rate_limited_models mismatch: count=%d list=%v", all.RateLimitedModelsCount, all.RateLimitedModels)
	}

	cn := resp.CreditSummary.CN
	if cn.Remain != 350.75 || cn.Total != 500 || cn.BonusRemain != 250.25 || cn.BaseRemain != 100.5 {
		t.Errorf("cn mismatch: %+v", cn)
	}
	if len(cn.ModelQuotas) == 0 || cn.ModelQuotas[0].Model != "claude-4.0-sonnet" || cn.ModelQuotas[0].CostPer1k != 2.5 || cn.ModelQuotas[0].Samples != 1 {
		t.Errorf("cn model quotas mismatch: %+v", cn.ModelQuotas)
	}

	gl := resp.CreditSummary.Global
	if gl.Remain != 350.5 || gl.Total != 500 || gl.BaseRemain != 300 || gl.PurchasedRemain != 50.5 {
		t.Errorf("global mismatch: %+v", gl)
	}
	if gl.RateLimitedModelsCount != 1 {
		t.Errorf("global rate_limited_models_count = %d, want 1", gl.RateLimitedModelsCount)
	}
}
