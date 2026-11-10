// model_route.go 虚拟模型路由组、免费模型池自动发现与模型级故障熔断器。
package server

import (
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ModelCircuitBreaker 模型级轻量级熔断器：跟踪每个具体模型的连续故障次数与冷却状态。
// 当某个模型在所有账号上连续报错（如 429 频控、11102 无此模型、5xx 服务故障）达到阈值时，
// 自动进入短暂熔断冷却（例如 30s~60s），路由池自动避开该模型并快速故障转移至下一候选模型。
type ModelCircuitBreaker struct {
	mu       sync.RWMutex
	failures map[string]int       // model -> 连续失败次数
	cooling  map[string]time.Time // model -> 熔断冷却到期时间
}

// NewModelCircuitBreaker 创建模型熔断器实例。
func NewModelCircuitBreaker() *ModelCircuitBreaker {
	return &ModelCircuitBreaker{
		failures: make(map[string]int),
		cooling:  make(map[string]time.Time),
	}
}

// IsCooling 报告指定模型当前是否正处于熔断冷却中。
func (b *ModelCircuitBreaker) IsCooling(model string) bool {
	if b == nil {
		return false
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	until, ok := b.cooling[model]
	if !ok {
		return false
	}
	return time.Now().Before(until)
}

// NoteFailure 记录一次模型级故障。连续失败达阈值（2次）后自动触发熔断。
func (b *ModelCircuitBreaker) NoteFailure(model string, cooldown time.Duration) {
	if b == nil || model == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures[model]++
	if cooldown <= 0 {
		cooldown = 60 * time.Second
	}
	if b.failures[model] >= 2 {
		b.cooling[model] = time.Now().Add(cooldown)
		log.Printf("WARN: [circuit-breaker] model %s reached %d failures, entering cooling for %v", model, b.failures[model], cooldown)
	}
}

// Trip 立即强制使模型进入熔断冷却（例如收到确切的 11102 该后端无此模型，或严重 429 时）。
func (b *ModelCircuitBreaker) Trip(model string, cooldown time.Duration) {
	if b == nil || model == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures[model] = 3
	if cooldown <= 0 {
		cooldown = 60 * time.Second
	}
	b.cooling[model] = time.Now().Add(cooldown)
	log.Printf("WARN: [circuit-breaker] model %s tripped into cooling for %v", model, cooldown)
}

// NoteSuccess 成功完成一次请求，清空该模型的失败计数与冷却状态。
func (b *ModelCircuitBreaker) NoteSuccess(model string) {
	if b == nil || model == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.failures, model)
	delete(b.cooling, model)
}

// IsFreeCredits 报告 credits 字段是否表示完全免费（0 算力 / 0 积分消耗）。
func IsFreeCredits(raw string) bool {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.TrimSuffix(s, "credits")
	s = strings.TrimSuffix(s, "credit")
	s = strings.TrimPrefix(s, "x")
	s = strings.TrimSpace(s)
	switch s {
	case "0", "0.0", "0.00", "0.000", "free", "免费":
		return true
	}
	if val, err := strconv.ParseFloat(s, 64); err == nil {
		return val <= 0.00001
	}
	return false
}

// IsFreeRouteAlias 报告给定的模型名是否属于内置的免费模型路由别名。
func IsFreeRouteAlias(model string) bool {
	lower := strings.ToLower(strings.TrimSpace(model))
	switch lower {
	case "auto-free", "free", "free-models", "free-pool", "auto_free", "free_model", "free-tier", "免费", "免费模型":
		return true
	}
	return false
}

// GetFreeModelCandidates 汇总所有可用的免费模型候选列表，并按健康度排序。
func (h *Handler) GetFreeModelCandidates() []string {
	var poolList []string

	// 1. 优先读取显式配置的免费模型列表
	if h != nil && len(h.cfg.FreeModels) > 0 {
		poolList = append(poolList, h.cfg.FreeModels...)
	}
	if h != nil && h.cfg.ModelRoutes != nil {
		if configured, ok := h.cfg.ModelRoutes["auto-free"]; ok && len(configured) > 0 {
			poolList = append(poolList, configured...)
		}
		if configured, ok := h.cfg.ModelRoutes["free"]; ok && len(configured) > 0 {
			poolList = append(poolList, configured...)
		}
	}

	// 2. 从动态拉取的上游模型中，自动提取所有 0 算力免费模型
	if h != nil {
		dynamicCN := h.fetchDynamicModels()
		for _, mi := range dynamicCN {
			if IsFreeCredits(mi.Credits) {
				poolList = append(poolList, "cn:"+mi.ID, mi.ID)
			}
		}
		if h.cfg.GlobalEnabled && h.cfg.Upstream != nil {
			_, globalAccount := h.fetchGlobalModels()
			if globalAccount != nil {
				globalInfos := h.cfg.Upstream.FetchGlobalModelInfos(globalAccount)
				for _, mi := range globalInfos {
					if IsFreeCredits(mi.Credits) {
						poolList = append(poolList, "global:"+mi.ID, mi.ID)
					}
				}
			}
		}
	}

	// 3. 兜底内置基础免费模型（官方长期 0 算力或轻量级高速模型）
	builtinDefaults := []string{
		"deepseek-v4.1-flash",
		"global:deepseek-v4.1-flash",
		"cn:deepseek-v4.1-flash",
		"deepseek-chat",
		"glm-4-flash",
	}
	poolList = append(poolList, builtinDefaults...)

	// 4. 去重并清洗
	seen := make(map[string]bool)
	var deduped []string
	for _, m := range poolList {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		deduped = append(deduped, m)
	}

	// 5. 按照「健康优先、可用优先、避开熔断」动态分级排序
	result := h.SortCandidatesByHealth(deduped)
	if len(result) == 0 {
		return builtinDefaults
	}
	return result
}

// SortCandidatesByHealth 对候选模型列表按照「当前有健康在线账号 > 备用无熔断 > 处于熔断冷却」动态分级排序。
func (h *Handler) SortCandidatesByHealth(candidates []string) []string {
	var healthyActive []string
	var backupNormal []string
	var coolingModels []string

	seen := make(map[string]bool)
	for _, m := range candidates {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true

		// 若该模型当前正在熔断冷却中，移至末尾
		if h != nil && h.modelBreaker != nil && h.modelBreaker.IsCooling(m) {
			coolingModels = append(coolingModels, m)
			continue
		}

		// 检查当前账号池是否有支持此模型的在线健康账号
		realm, bare := resolveModel(m)
		hasAccount := false
		if h != nil && h.cfg.Pool != nil {
			uids := h.cfg.Pool.AvailableUIDsForModelRealm(bare, realm)
			if len(uids) > 0 {
				hasAccount = true
			}
		}

		if hasAccount {
			healthyActive = append(healthyActive, m)
		} else {
			backupNormal = append(backupNormal, m)
		}
	}

	result := append(healthyActive, backupNormal...)
	result = append(result, coolingModels...)
	return result
}

// ResolveHunyuanFreeCandidates 解析混元等免费模型的跨域同名等价候选列表。
func (h *Handler) ResolveHunyuanFreeCandidates(model string) []string {
	clean := strings.ToLower(strings.TrimSpace(model))
	clean = strings.TrimPrefix(clean, "global:")
	clean = strings.TrimPrefix(clean, "cn:")
	switch clean {
	case "hy4free", "hy4-free", "hunyuan-free", "hunyuan4-free", "hy4free-preview":
		base := []string{
			"global:hy4-preview",
			"cn:hy4-preview",
			"global:hy4-preview-f",
			"cn:hy4-preview-f",
			"hy4-preview",
		}
		return h.SortCandidatesByHealth(base)
	case "hy3free", "hy3-free", "hunyuan3-free":
		base := []string{
			"global:hy3",
			"cn:hy3",
			"global:hy3-preview",
			"cn:hy3-preview",
			"hy3",
		}
		return h.SortCandidatesByHealth(base)
	}
	return nil
}

// ResolveModelCandidates 解析客户端请求的模型名，若命中路由组或别名列表，返回有序的候选模型切片。
func (h *Handler) ResolveModelCandidates(model string) []string {
	clean := strings.TrimSpace(model)
	if clean == "" {
		return []string{"deepseek-v4.1-flash"}
	}

	// 1. 命中内置免费模型别名 -> 自动返回动态免费模型候选池
	if IsFreeRouteAlias(clean) {
		return h.GetFreeModelCandidates()
	}

	// 1.5 命中混元专属免费模型别名 -> 返回带跨域健康排序的混元候选池
	if hyCands := h.ResolveHunyuanFreeCandidates(clean); len(hyCands) > 0 {
		return hyCands
	}

	// 2. 命中用户在 ModelRoutes 中自定义的路由组
	if h != nil && h.cfg.ModelRoutes != nil {
		if list, ok := h.cfg.ModelRoutes[clean]; ok && len(list) > 0 {
			return list
		}
		if list, ok := h.cfg.ModelRoutes[strings.ToLower(clean)]; ok && len(list) > 0 {
			return list
		}
	}

	// 3. 检查 ModelAliases 是否配置了逗号分隔的多模型故障转移列表（如 "my-free": "m1,m2,m3"）
	if h != nil && h.cfg.ModelAliases != nil {
		var targetStr string
		if t, ok := h.cfg.ModelAliases[clean]; ok && t != "" {
			targetStr = t
		} else if t, ok := h.cfg.ModelAliases[strings.ToLower(clean)]; ok && t != "" {
			targetStr = t
		}
		if targetStr != "" {
			if strings.Contains(targetStr, ",") {
				parts := strings.Split(targetStr, ",")
				var res []string
				for _, p := range parts {
					p = strings.TrimSpace(p)
					if p != "" {
						res = append(res, p)
					}
				}
				if len(res) > 0 {
					return res
				}
			} else if IsFreeRouteAlias(targetStr) {
				return h.GetFreeModelCandidates()
			} else if hyCands := h.ResolveHunyuanFreeCandidates(targetStr); len(hyCands) > 0 {
				return hyCands
			}
		}
	}

	// 4. 普通单模型别名解析与跨域自动补位
	realm, bare := h.resolveModelWithAliases(clean)
	if bare == "auto" || bare == "default-model" || bare == "" {
		bare = "deepseek-v4.1-flash"
	}
	full := bare
	if realm != "" {
		full = realm + ":" + bare
	}

	var cands []string
	cands = append(cands, full)

	// 若请求带明确域前缀且该域在池中有账号配置，当该域故障或全死时，自动生成对端等价模型作为后备候选进行补位
	if h != nil && h.cfg.Pool != nil {
		totalInRealm, _, _, _, _ := h.cfg.Pool.CountsDetailedForRealm(realm)
		if totalInRealm > 0 {
			if realm == "global" {
				cands = append(cands, "cn:"+bare, bare)
			} else if realm == "cn" && strings.HasPrefix(clean, "cn:") {
				cands = append(cands, "global:"+bare, bare)
			}
		}
	}

	return h.SortCandidatesByHealth(cands)
}
