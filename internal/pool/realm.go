// 分池选号域：按 realm（cn/global）过滤选号与可用集合。realm=="" 退化为现状。
package pool

import (
	"time"

	"workbuddy2api/internal/auth"
)

// PickExcludingForRealm 按 realm 过滤的轮换选号：候选仅限 Realm()==realm 的账号。
// realm=="" 退化为 PickExcluding（现状语义，老调用零改动）。
// 可选做请求级轮换（tried）与模型感知（reqModel，6004 模型豁免照常生效）；
// reqModel 非空时健康口径换成 healthyForModel。realm 不匹配的全冷却兜底同样排除。
func (p *Pool) PickExcludingForRealm(tried map[string]bool, reqModel, realm string) *auth.Auth {
	return p.pick(tried, reqModel, realm, "")
}

// PickExcludingForRealmAndOwner 按 realm 与 user owner 过滤的轮换选号：
// 支持用户多租户隔离，普通用户仅能调用自己绑定的账号或系统共享账号。
func (p *Pool) PickExcludingForRealmAndOwner(tried map[string]bool, reqModel, realm, owner string) *auth.Auth {
	return p.pick(tried, reqModel, realm, owner)
}

// PickHealthyOnlyExcludingForRealmAndOwner 仅在候选账号处于健康正常状态时选号；
// 若当前域所有账号全在冷却或不可用，返回 nil（不触发全冷却死号兜底），供跨域容灾或多模型补位使用。
func (p *Pool) PickHealthyOnlyExcludingForRealmAndOwner(tried map[string]bool, reqModel, realm, owner string) *auth.Auth {
	return p.pickInternal(tried, reqModel, realm, owner, false)
}

// AvailableUIDsForRealm 同 AvailableUIDs，但仅返回 Realm()==realm 的账号。
// DeptestOnly: 仅 realm_test.go 引用；生产经 wiring.go 走
// AvailableUIDsForModelRealm。保留作 ForModelRealm 的模型维度退化
// （model=""）语义锚点测试。
// realm=="" 退化为 AvailableUIDs（现状语义）。
func (p *Pool) AvailableUIDsForRealm(realm string) []string {
	return p.availableUIDsLocked(realm, func(e *entry, now time.Time) bool { return e.healthy(now) })
}

// AvailableUIDsForModelRealm 同 AvailableUIDsForModel，但仅返回 Realm()==realm 的账号
// （6004 模型豁免照常生效）。realm=="" 退化为 AvailableUIDsForModel。
func (p *Pool) AvailableUIDsForModelRealm(model, realm string) []string {
	return p.availableUIDsLocked(realm,
		func(e *entry, now time.Time) bool { return e.healthyForModel(now, model) })
}

// HasHealthyForModelRealmAndOwner 报告指定 realm/owner 下是否存在当前健康可用的账号
// （含 6004 模型级冷却与 11102 负缓存过滤，忽略 inFlight 仅判断账号+模型可服务性）。
func (p *Pool) HasHealthyForModelRealmAndOwner(reqModel, realm, owner string) bool {
	if p == nil {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	for _, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if owner != "" && owner != "admin" && owner != "u_admin" {
			acctOwner := e.a.OwnerValue()
			if acctOwner != "public" && !IsOwnerMatch(acctOwner, owner) {
				continue
			}
		}
		if reqModel != "" {
			if e.healthyForModel(now, reqModel) {
				return true
			}
		} else if e.healthy(now) {
			return true
		}
	}
	return false
}
