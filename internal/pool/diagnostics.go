// Package pool diagnostics 提供账号池选号失败诊断与状态分析
package pool

import (
	"fmt"
	"strings"
	"time"
)

// Diagnostics 选号失败或池状态诊断信息
type Diagnostics struct {
	TotalAccounts      int       `json:"total_accounts"`       // 池内全部账号总数
	RealmAccounts      int       `json:"realm_accounts"`       // 当前域（cn/global）下的账号总数
	OwnerMatched       int       `json:"owner_matched"`        // 符合用户归属隔离的账号数
	DisabledCount      int       `json:"disabled_count"`       // 禁用/停用账号数
	InFlightFullCount  int       `json:"in_flight_full_count"` // 在途请求占满的账号数
	HardCoolingCount   int       `json:"hard_cooling_count"`   // 余额耗尽/硬冷却账号数
	SoftCoolingAccount int       `json:"soft_cooling_account"` // 账号级限流软冷却账号数
	SoftCoolingModel   int       `json:"soft_cooling_model"`   // 模型级限流软冷却账号数（针对 reqModel）
	BreakerCooling     int       `json:"breaker_cooling"`      // 熔断中账号数
	DegradeCooling     int       `json:"degrade_cooling"`      // 连败降权中账号数
	TriedCount         int       `json:"tried_count"`          // 本轮请求已尝试排除的账号数
	EarliestReset      time.Time `json:"earliest_reset"`       // 最早恢复时间（若全部在冷却中）
	Summary            string    `json:"summary"`              // 诊断可读摘要
	IsRateLimited      bool      `json:"is_rate_limited"`      // 是否主要是因为限流/冷却导致不可用
}

// DiagnosticsForModelRealmAndOwner 对指定模型、域和用户归属进行选号不可用原因诊断分析
func (p *Pool) DiagnosticsForModelRealmAndOwner(reqModel, realm, owner string, tried map[string]bool) Diagnostics {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()

	d := Diagnostics{
		TotalAccounts: len(p.byUID),
	}

	var earliestReset time.Time
	updateEarliest := func(t time.Time) {
		if !t.IsZero() && now.Before(t) {
			if earliestReset.IsZero() || t.Before(earliestReset) {
				earliestReset = t
			}
		}
	}

	for uid, e := range p.byUID {
		// 域过滤
		inRealm := (realm == "" || e.a.Realm() == realm)
		if inRealm {
			d.RealmAccounts++
		} else {
			continue
		}

		// 用户隔离
		ownerOK := (owner == "" || owner == "admin" || e.a.OwnerValue() == owner || e.a.OwnerValue() == "public")
		if ownerOK {
			d.OwnerMatched++
		} else {
			continue
		}

		if tried != nil && tried[uid] {
			d.TriedCount++
			continue
		}

		if e.disabled || e.manualDisabled {
			d.DisabledCount++
			continue
		}

		if p.inFlightFull(e) {
			d.InFlightFullCount++
		}

		// 检查冷却
		if e.coolKind == CoolHard && !e.until.IsZero() && now.Before(e.until) {
			d.HardCoolingCount++
			updateEarliest(e.until)
		} else if !e.breakerUntil.IsZero() && now.Before(e.breakerUntil) {
			d.BreakerCooling++
			updateEarliest(e.breakerUntil)
		} else if !e.degradeUntil.IsZero() && now.Before(e.degradeUntil) {
			d.DegradeCooling++
			updateEarliest(e.degradeUntil)
		} else if !e.until.IsZero() && now.Before(e.until) {
			d.SoftCoolingAccount++
			updateEarliest(e.until)
		} else if reqModel != "" && e.modelCooled(now, reqModel) {
			d.SoftCoolingModel++
			if mc, ok := e.modelCooldowns[reqModel]; ok && !mc.Until.IsZero() {
				updateEarliest(mc.Until)
			}
		}
	}

	d.EarliestReset = earliestReset

	// 综合推导诊断摘要
	if d.TotalAccounts == 0 {
		d.Summary = "账号池为空，未配置任何账号"
		return d
	}
	if d.RealmAccounts == 0 {
		d.Summary = fmt.Sprintf("realm=%q 下无可用账号 (池内总账号数: %d)", realm, d.TotalAccounts)
		return d
	}
	if d.OwnerMatched == 0 {
		d.Summary = fmt.Sprintf("realm=%q 下有 %d 个账号，但均不属于当前用户(owner=%s)且无 public 共享账号", realm, d.RealmAccounts, owner)
		return d
	}

	if d.DisabledCount >= d.OwnerMatched {
		d.Summary = fmt.Sprintf("realm=%q 下全部 %d 个账号已被禁用或手动停用", realm, d.OwnerMatched)
		return d
	}

	coolingTotal := d.SoftCoolingAccount + d.SoftCoolingModel + d.BreakerCooling + d.DegradeCooling + d.HardCoolingCount
	if coolingTotal >= (d.OwnerMatched - d.DisabledCount) && coolingTotal > 0 {
		d.IsRateLimited = true
		resetStr := "未知"
		if !earliestReset.IsZero() {
			resetStr = earliestReset.Format("15:04:05")
		}
		if d.SoftCoolingModel > 0 && d.SoftCoolingModel >= (d.OwnerMatched-d.DisabledCount) {
			d.Summary = fmt.Sprintf("realm=%q 下可用账号(%d个)均处于模型 %q 限流冷却中 (最早恢复: %s)", realm, d.SoftCoolingModel, reqModel, resetStr)
		} else if d.HardCoolingCount >= (d.OwnerMatched - d.DisabledCount) {
			d.Summary = fmt.Sprintf("realm=%q 下可用账号(%d个)余额耗尽/硬冷却中 (最早恢复: %s)", realm, d.HardCoolingCount, resetStr)
		} else {
			d.Summary = fmt.Sprintf("realm=%q 下全部可用账号(%d个)均处于限流冷却中 (最早恢复: %s)", realm, coolingTotal, resetStr)
		}
		return d
	}

	if d.InFlightFullCount >= (d.OwnerMatched - d.DisabledCount) && d.InFlightFullCount > 0 {
		d.Summary = fmt.Sprintf("realm=%q 下全部可用账号(%d个)并发在途请求均已占满", realm, d.InFlightFullCount)
		return d
	}

	var parts []string
	if d.TriedCount > 0 {
		parts = append(parts, fmt.Sprintf("已尝试排除%d个", d.TriedCount))
	}
	if d.DisabledCount > 0 {
		parts = append(parts, fmt.Sprintf("%d个停用", d.DisabledCount))
	}
	if coolingTotal > 0 {
		parts = append(parts, fmt.Sprintf("%d个冷却中", coolingTotal))
	}
	if d.InFlightFullCount > 0 {
		parts = append(parts, fmt.Sprintf("%d个满载", d.InFlightFullCount))
	}
	if len(parts) > 0 {
		d.Summary = fmt.Sprintf("realm=%q 账号受限: %s", realm, strings.Join(parts, ", "))
	} else {
		d.Summary = fmt.Sprintf("realm=%q 暂无可用健康账号", realm)
	}
	return d
}
