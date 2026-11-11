package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func TestCooldownFreeQuotaModelPreservesResetAndIsolatesModel(t *testing.T) {
	p := New("")
	p.SetSoftRateMax(10 * time.Minute) // 验证免费额度冷却不会被 10m soft_rate_max 截断
	p.Add(&auth.Auth{UID: "gl1", AccessToken: "at1", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "gl2", AccessToken: "at2", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999})

	reset := time.Now().Add(18 * time.Hour)
	p.CooldownFreeQuotaModel("gl1", reset, "hy4free", "free quota exhausted (credit=0.20, quota=100%)")

	// gl1 对 hy4free 不再可用（无论是 PickByUIDForModel 粘性还是 PickExcluding 兜底）
	if a := p.PickByUIDForModel("gl1", "hy4free"); a != nil {
		t.Errorf("gl1 should be rate-limited on hy4free, got %s", a.UID)
	}
	if got := p.AvailableUIDsForModelRealm("hy4free", "global"); len(got) != 1 || got[0] != "gl2" {
		t.Errorf("available for hy4free=%v want [gl2]", got)
	}

	// gl1 对其他模型（如 gpt-5.4）完全不受影响
	if a := p.PickByUIDForModel("gl1", "gpt-5.4"); a == nil || a.UID != "gl1" {
		t.Errorf("gl1 should remain healthy for gpt-5.4, got %v", a)
	}

	// 即使只冷却 hy4free，ResetAt 应保留完整 18h（不被 10m soft_rate_max 提前放行）
	st, ok := p.Status("gl1")
	if !ok || len(st.RateLimitedModels) != 1 {
		t.Fatalf("expected 1 rate-limited model, got %+v", st.RateLimitedModels)
	}
	if st.RateLimitedModels[0].Until.Before(time.Now().Add(17 * time.Hour)) {
		t.Errorf("until=%v should preserve full reset horizon (~18h), not capped to 10m", st.RateLimitedModels[0].Until)
	}
}
