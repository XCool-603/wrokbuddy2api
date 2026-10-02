package pool

import (
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func TestDiagnosticsForModelRealmAndOwner(t *testing.T) {
	// 1. 空池诊断
	p := New("")
	dEmpty := p.DiagnosticsForModelRealmAndOwner("deepseek-v4.1-flash", "global", "u1", nil)
	if dEmpty.TotalAccounts != 0 || !strings.Contains(dEmpty.Summary, "账号池为空") {
		t.Fatalf("empty pool diagnostics failed: %+v", dEmpty)
	}

	// 2. 跨 Realm 诊断（仅有 CN，请求 global）
	p.Add(&auth.Auth{UID: "cn1", Domain: "copilot.tencent.com", Owner: "u1"})
	dRealm := p.DiagnosticsForModelRealmAndOwner("deepseek-v4.1-flash", "global", "u1", nil)
	if dRealm.RealmAccounts != 0 || !strings.Contains(dRealm.Summary, "realm=\"global\" 下无可用账号") {
		t.Fatalf("realm mismatch diagnostics failed: %+v", dRealm)
	}

	// 3. 用户隔离诊断（有 global 账号，但属于 u2，当前用户为 u1）
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", Owner: "u2"})
	dOwner := p.DiagnosticsForModelRealmAndOwner("deepseek-v4.1-flash", "global", "u1", nil)
	if dOwner.RealmAccounts != 1 || dOwner.OwnerMatched != 0 || !strings.Contains(dOwner.Summary, "均不属于当前用户") {
		t.Fatalf("owner mismatch diagnostics failed: %+v", dOwner)
	}

	// 4. 全部停用诊断
	p.Add(&auth.Auth{UID: "g2", Domain: "www.workbuddy.ai", Owner: "u1"})
	p.SetManualDisabled("g2", true, "admin disabled")
	dDisabled := p.DiagnosticsForModelRealmAndOwner("deepseek-v4.1-flash", "global", "u1", nil)
	if !strings.Contains(dDisabled.Summary, "已被禁用或手动停用") {
		t.Fatalf("disabled diagnostics failed: %+v", dDisabled)
	}

	// 5. 模型级 6004 限流冷却诊断
	p.SetManualDisabled("g2", false, "")
	resetTime := time.Now().Add(10 * time.Minute)
	p.CooldownSoftForModel("g2", time.Minute, resetTime, "deepseek-v4.1-flash", "6004 model rate limit")
	dRate := p.DiagnosticsForModelRealmAndOwner("deepseek-v4.1-flash", "global", "u1", nil)
	if !dRate.IsRateLimited {
		t.Fatalf("expected is_rate_limited=true, got: %+v", dRate)
	}
	if !strings.Contains(dRate.Summary, "deepseek-v4.1-flash") || !strings.Contains(dRate.Summary, "限流冷却中") {
		t.Fatalf("model rate limit diagnostics failed: %+v", dRate)
	}

	// 6. 管理员透视全部账号
	dAdmin := p.DiagnosticsForModelRealmAndOwner("deepseek-v4.1-flash", "global", "admin", nil)
	if dAdmin.OwnerMatched != 2 { // g1 (u2) + g2 (u1)
		t.Fatalf("admin owner matched expected 2, got %d", dAdmin.OwnerMatched)
	}
}
