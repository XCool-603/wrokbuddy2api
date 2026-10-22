package server

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// dynamicModelsCache 动态模型缓存。
var dynamicModelsCache struct {
	sync.RWMutex
	ids      []upstream.ModelInfo
	fetched  time.Time // 最近一次成功拉取时间
	lastFail time.Time // 最近一次拉取失败时间（负缓存）
}

const (
	dynamicModelsTTL        = time.Hour
	modelsFetchFailCooldown = 5 * time.Minute
)

// models 返回模型列表：纯动态（缓存 1h），失败/无号返回空列表（无静态兜底）。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	ua := strings.ToLower(r.Header.Get("User-Agent"))
	// 无论下游是 New API、One API、Cherry Studio、NextChat、OpenAI SDK 或带 ?bare=true 参数，
	// 均自动透出裸模型名，以便客户端直接识别并使用上游模型。
	includeBare := r.URL.Query().Get("bare") == "true" ||
		r.URL.Query().Get("include_bare") == "true" ||
		r.Header.Get("X-Include-Bare-Models") == "true" ||
		strings.Contains(ua, "new-api") ||
		strings.Contains(ua, "one-api") ||
		strings.Contains(ua, "openai") ||
		strings.Contains(ua, "curl") ||
		strings.Contains(ua, "python") ||
		strings.Contains(ua, "langchain") ||
		r.URL.Query().Get("compat") == "bare"
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   h.modelList(includeBare),
	})
}

// fmtCreditsPrefix 从上游 credits 原文提取倍率并格式化为 "[x0.05 credit]"。
// 上游格式不统一："x0.05 credits" / "x0.29" / "x0.00 credits" 等，
// 统一提取 x数字 部分，去 "credits" 后缀。
func fmtCreditsPrefix(raw string) string {
	s := strings.TrimSpace(raw)
	// 去掉 "credits" 后缀
	s = strings.TrimSuffix(s, "credits")
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return "[" + s + " credit]"
}

// applyModelInfoFields 把上游模型对象全字段（ModelInfo）按「空值省略」写出规则
// 合入 /v1/models 条目：name/description/credits/tags/vendor/能力旗标/
// max_allowed_size/reasoning_effort/reasoning_summary。CN 动态分支与 global
// 探测命中分支共用（两域模型对象同构），保证输出字段集一致。
// 不覆盖 id/object/created/owned_by 及调用方先前写好的基础字段；上游未下发的
// 字段（零值）整体省略——不编造。
func applyModelInfoFields(entry map[string]any, mi upstream.ModelInfo) map[string]any {
	if mi.Name != "" {
		entry["name"] = mi.Name
	}
	if mi.Description != "" {
		// 积分倍率前缀：从 "x0.05 credits" / "x0.29" 等格式提取纯数字，
		// 统一为 "[x0.05 credit]" 前缀拼入 description，方便下游面板直接展示。
		if mi.Credits != "" {
			entry["description"] = fmtCreditsPrefix(mi.Credits) + " " + mi.Description
		} else {
			entry["description"] = mi.Description // descriptionZh 中文描述
		}
	}
	if mi.Credits != "" {
		entry["credits"] = mi.Credits // 积分倍率原文（如 "x0.05"），仅展示
	}
	if len(mi.Tags) > 0 {
		entry["tags"] = mi.Tags
	}
	if mi.Vendor != "" {
		entry["vendor"] = mi.Vendor
	}
	if mi.IsDefault {
		entry["is_default"] = true
	}
	if mi.SupportsImages {
		entry["supports_images"] = true // 多模态能力透出
	}
	if mi.SupportsReasoning {
		entry["supports_reasoning"] = true
	}
	if mi.SupportsToolCall {
		entry["supports_tool_call"] = true
	}
	if mi.OnlyReasoning {
		entry["only_reasoning"] = true
	}
	if mi.MaxAllowedSize > 0 {
		entry["max_allowed_size"] = mi.MaxAllowedSize
	}
	if mi.ReasoningEffort != "" {
		entry["reasoning_effort"] = mi.ReasoningEffort
	}
	if mi.ReasoningSummary != "" {
		entry["reasoning_summary"] = mi.ReasoningSummary
	}
	return entry
}

// modelList 动态获取模型列表并包装成 OpenAI 格式（含 context_length）。
// CN 模型输出统一加 "cn:" 前缀（gateway 路由协议，与 resolveModel 对称）。
// 纯动态：动态拉取失败/无号 → 该域空列表，无静态兜底；
// global.enabled=false（显式逃生门）时只列 CN（global 名单不出现）。
func (h *Handler) modelList(includeBare ...bool) []map[string]any {
	var clientHTTP *http.Client
	if h.cfg.Upstream != nil {
		clientHTTP = h.cfg.Upstream.HTTP
	}

	out := make([]map[string]any, 0)
	for _, mi := range h.fetchDynamicModels() {
		entry := map[string]any{
			"id":       "cn:" + mi.ID,
			"object":   "model",
			"created":  1753600000,
			"owned_by": "workbuddy",
		}
		// context_length / max_output_tokens 四级查找（upstream.context_catalog +
		// model_catalog）：上游动态值（maxInputTokens/maxOutputTokens）权威 → 静态
		// 种子表 → model.json 本地缓存 → models.dev 按需拉取（异步不阻塞本次响应，
		// 拉到后写 model.json 供下次命中）→ 1M 兜底 / max_output_tokens 省略。
		// 上游零值不再透出假 131072（误导 Codex/ZCode 等按 context_length 提前
		// 截断、白白丢上下文）。
		entry["context_length"] = upstream.ContextWindowListingV4(mi.ID, mi.ContextWindow, clientHTTP)
		if mo, ok := upstream.MaxOutputTokensListingV4(mi.ID, mi.MaxTokens, clientHTTP); ok {
			entry["max_output_tokens"] = mo
		}
		// 上游模型对象全字段透出（name/描述/标签/倍率/能力旗标等，空值省略）。
		entry = applyModelInfoFields(entry, mi)
		// P0：effort 能力透出——远端 supportedEfforts 权威，缺失落到 CN 静态兜底表
		// （issue #84 客户端可发现档位，不再盲传）。无档位→省略字段（非空数组）。
		if efforts, def := upstream.EffortListing("cn", mi.ID, mi.Efforts, mi.DefaultEffort); efforts != nil {
			entry["reasoning_supported_efforts"] = efforts
			if def != "" {
				entry["reasoning_default_effort"] = def
			}
		}
		out = append(out, entry)
	}
	// global 模型名单：仅 GlobalEnabled=true 时列出（逃生门）。
	// 名单 = 探测结果（fetchGlobalModels 纯动态，失败/无号 → 空）；无 global 账号时
	// 空名单且零上游调用。
	if h.cfg.GlobalEnabled && h.cfg.Upstream != nil {
		// global 域 effort 能力三级查找：探测下发桶（权威）→ 静态兜底表 → 省略。
		// 先 fetchGlobalModels（内部探测并落 effort 桶），再按 id 取快照。
		globalIDs, globalAccount := h.fetchGlobalModels()
		// 探测对象形态的全字段条目（与 fetchGlobalModels 共享同一次探测缓存）：
		// 命中 id 才透出富字段；窄表/失败 → nil，按裸 ID 条目输出（不编造字段）。
		// globalAccount 为 nil（无 global 号）时返回 nil，跳过富字段映射。
		globalInfos := map[string]upstream.ModelInfo{}
		for _, mi := range h.cfg.Upstream.FetchGlobalModelInfos(globalAccount) {
			globalInfos[mi.ID] = mi
		}
		globalEfforts, globalDefaults := h.cfg.Upstream.GlobalEffortSnapshot()
		for _, id := range globalIDs {
			entry := map[string]any{
				"id":       "global:" + id,
				"object":   "model",
				"created":  1753600000,
				"owned_by": "workbuddy",
			}
			// context_length / max_output_tokens 四级查找（upstream.model_catalog，
			// 与 CN 动态分支同口径）：探测富条目真实值权威 → 静态种子表 →
			// model.json 缓存 → models.dev 按需拉取（异步）→ 1M 兜底/省略。
			// 裸 ID 条目（窄表探测）也经种子表补齐，不再裸 131072。
			var remoteCtx, remoteOut int64
			if mi, ok := globalInfos[id]; ok {
				entry = applyModelInfoFields(entry, mi)
				remoteCtx, remoteOut = mi.ContextWindow, mi.MaxTokens
			}
			entry["context_length"] = upstream.ContextWindowListingV4(id, remoteCtx, clientHTTP)
			if mo, ok := upstream.MaxOutputTokensListingV4(id, remoteOut, clientHTTP); ok {
				entry["max_output_tokens"] = mo
			}
			if efforts, def := upstream.EffortListing("global", id, globalEfforts[id], globalDefaults[id]); efforts != nil {
				entry["reasoning_supported_efforts"] = efforts
				if def != "" {
					entry["reasoning_default_effort"] = def
				}
			}
			out = append(out, entry)
		}
	}

	// 裸模型名透出（兼容 New API / One API 等下游上游拉取模型列表）：
	// 若显式开启了 includeBare（如 /v1/models?bare=true 或指定 header），
	// 除了带有 "cn:" / "global:" 命名空间前缀的模型外，去重并追加上游原始裸模型名（如 "deepseek-v4.1-flash"），
	// 使得下游无需复杂映射即可直接获取并调用原生模型名。
	if len(includeBare) > 0 && includeBare[0] {
		seenBare := make(map[string]bool)
		for _, m := range out {
			fullID, _ := m["id"].(string)
			bare := fullID
			if idx := strings.IndexByte(fullID, ':'); idx >= 0 {
				bare = fullID[idx+1:]
			}
			if bare == "" || seenBare[bare] {
				continue
			}
			seenBare[bare] = true
			bareEntry := make(map[string]any, len(m))
			for k, v := range m {
				bareEntry[k] = v
			}
			bareEntry["id"] = bare
			out = append(out, bareEntry)
		}
	}

	// 若 DeepSeek Harness (dsh) 正在运行或环境已安装就绪，直接向模型列表透出 dsh 智能体模型
	if h.cfg.DshMgr != nil {
		st := h.cfg.DshMgr.GetStatus()
		if st.Installed || st.Running {
			dshEntry := map[string]any{
				"id":                 "dsh",
				"name":               "DeepSeek Harness (dsh Agent)",
				"description":        "DeepSeek 官方自主编程与智能体框架，支持本地环境操控、网页自动化与工程级代码调度",
				"object":             "model",
				"created":            1753600000,
				"owned_by":           "deepseek-ai",
				"context_length":     131072,
				"max_output_tokens":  8192,
				"supports_tool_call": true,
				"supports_reasoning": true,
				"vendor":             "DeepSeek",
			}
			out = append(out, dshEntry)
		}
	}

	// 虚拟模型别名注入（如 deepseek-chat -> global:deepseek-v4.1-flash）
	if len(h.cfg.ModelAliases) > 0 {
		seen := make(map[string]bool)
		for _, m := range out {
			if id, ok := m["id"].(string); ok {
				seen[id] = true
			}
		}
		for alias, target := range h.cfg.ModelAliases {
			if !seen[alias] {
				out = append(out, map[string]any{
					"id":             alias,
					"name":           alias + " (-> " + target + ")",
					"description":    "虚拟别名模型，请求自动重定向至 " + target,
					"object":         "model",
					"created":        1753600000,
					"owned_by":       "alias",
					"context_length": 131072,
				})
				seen[alias] = true
			}
		}
	}

	// 视频生成模型注入（快手可灵 Kling 3.0 与 MiniMax-H3 海螺双视频方案）
	if len(includeBare) > 0 && includeBare[0] {
		videoModels := []map[string]any{
			{
				"id":              "kling-video-v3",
				"name":            "【视频】快手可灵 3.0 (Kling-Video-V3)",
				"description":     "【视频专属大模型】快手可灵 3.0 旗舰视频大模型，支持 720P/1080P/4K 高清生成、首尾帧驱动与原生音效",
				"object":          "model",
				"created":         1753600000,
				"owned_by":        "kuaishou",
				"category":        "video",
				"modality":        "video",
				"supports_video":  true,
				"supports_images": true,
				"tags":            []string{"video", "text-to-video", "image-to-video"},
				"vendor":          "Kling",
				"context_length":  2500,
			},
			{
				"id":              "keling-video-v3",
				"name":            "【视频】快手可灵 3.0 (Keling-Video-V3 拼音别名)",
				"description":     "【视频专属大模型】快手可灵 3.0 旗舰视频大模型拼音别名，支持 720P/1080P/4K 高清生成与视频直出",
				"object":          "model",
				"created":         1753600000,
				"owned_by":        "kuaishou",
				"category":        "video",
				"modality":        "video",
				"supports_video":  true,
				"supports_images": true,
				"tags":            []string{"video", "text-to-video", "image-to-video"},
				"vendor":          "Kling",
				"context_length":  2500,
			},
			{
				"id":              "minimax-video-h3",
				"name":            "【视频】MiniMax-H3 海螺视频",
				"description":     "【视频专属大模型】MiniMax 原生电影级视频生成大模型，支持 768P/2K、电影级运镜、9参考图与原生音频",
				"object":          "model",
				"created":         1753600000,
				"owned_by":        "minimax",
				"category":        "video",
				"modality":        "video",
				"supports_video":  true,
				"supports_images": true,
				"tags":            []string{"video", "text-to-video", "image-to-video"},
				"vendor":          "MiniMax",
				"context_length":  2000,
			},
			{
				"id":             "kling-v3-t2v",
				"name":           "【视频】快手可灵 3.0 文生视频",
				"description":    "【视频专属大模型】快手可灵 3.0 旗舰级文生视频大模型，支持 720P/1080P 高清生成与原生音效",
				"object":         "model",
				"created":        1753600000,
				"owned_by":       "kuaishou",
				"category":       "video",
				"modality":       "video",
				"supports_video": true,
				"tags":           []string{"video", "text-to-video"},
				"vendor":         "Kling",
				"context_length": 2500,
			},
			{
				"id":              "kling-v3-i2v",
				"name":            "【视频】快手可灵 3.0 图生视频",
				"description":     "【视频专属大模型】快手可灵 3.0 旗舰级图生视频大模型，支持首帧/首尾帧驱动与 1080P 高清生成",
				"object":          "model",
				"created":         1753600000,
				"owned_by":        "kuaishou",
				"category":        "video",
				"modality":        "video",
				"supports_video":  true,
				"supports_images": true,
				"tags":            []string{"video", "image-to-video"},
				"vendor":          "Kling",
				"context_length":  2500,
			},
		}
		out = append(out, videoModels...)
	}

	return out
}

// fetchGlobalModels 返回 global 模型名单（纯动态探测结果）及被探测账号。
// 与 fetchDynamicModels（CN 侧）同语义不同归位：缓存/失败回落封在 upstream.FetchGlobalModels
// （内部 1h + 5min 负缓存）。本方法只负责"何时探测"：
//   - 池中无 global 账号 → 空名单 + nil 账号（不发起上游调用）；
//   - 有 global 账号 → 单账号 Pick（global 域谓词），交 upstream 探测。
//
// 返回的 acct 供调用方在同一账号上取富 ModelInfo（FetchGlobalModelInfos 与
// FetchGlobalModels 共享缓存，不会触发第二次上游探测）。
// GlobalEnabled=false 时 modelList 已不进入本分支（逃生门在调用方 gate）。
func (h *Handler) fetchGlobalModels() ([]string, *auth.Auth) {
	if h.cfg.Upstream == nil {
		return nil, nil
	}
	acct := h.cfg.Pool.PickExcludingForRealm(nil, "", "global")
	if acct == nil {
		return nil, nil
	}
	return h.cfg.Upstream.FetchGlobalModels(acct), acct
}

// fetchDynamicModels 从池中任一健康 CN 账号拉模型列表（含 contextWindow/maxTokens），缓存 1h。
// 拉取失败记录时间戳进入 5min 负缓存，冷却期内直接返回 nil（纯动态，无静态表兜底），
// 避免反复打上游。
// 只从 CN realm 账号拉取（PickExcludingForRealm(nil,"","cn")）：全局账号的模型列表
// 未必与 CN 一致，动态模型表只服务 CN 前缀（global 走独立探测）。
func (h *Handler) fetchDynamicModels() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	if len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL {
		out := dynamicModelsCache.ids
		dynamicModelsCache.RUnlock()
		return out
	}
	// 失败负缓存：冷却期内不再请求上游，但若此前有成功拉取过的缓存，则优先返回陈旧缓存保底
	if !dynamicModelsCache.lastFail.IsZero() && time.Since(dynamicModelsCache.lastFail) < modelsFetchFailCooldown {
		if len(dynamicModelsCache.ids) > 0 {
			out := dynamicModelsCache.ids
			dynamicModelsCache.RUnlock()
			return out
		}
		dynamicModelsCache.RUnlock()
		return nil
	}
	dynamicModelsCache.RUnlock()

	if h.cfg.Upstream == nil {
		return nil
	}
	acct := h.cfg.Pool.PickExcludingForRealm(nil, "", "cn")
	if acct == nil {
		// 容错兜底：即使 CN 账号处于冷却期或被手动停用（不参与选号聊天），
		// 查询模型元数据不消耗积分，依然可借用其凭证拉取模型列表，防止前台模型全部蒸发
		for _, st := range h.cfg.Pool.List() {
			if st.Realm == "cn" && !st.Disabled {
				if a := h.cfg.Pool.AuthByUID(st.UID); a != nil && a.AccessTokenValue() != "" {
					acct = a
					break
				}
			}
		}
	}
	if acct == nil {
		dynamicModelsCache.RLock()
		stale := dynamicModelsCache.ids
		dynamicModelsCache.RUnlock()
		return stale
	}
	infos, err := h.cfg.Upstream.FetchModels(acct)
	if err != nil || len(infos) == 0 {
		// 拉取失败只进负缓存（5min lastFail），不 NoteError（P1-6/发现 6）：
		// NoteError 喂的是 chat 熔断器，models 端点偶发 5xx 跨界惩罚 chat 通道
		// 健康的账号；models 拉取失败 ≠ 账号 chat 不可用。
		dynamicModelsCache.Lock()
		dynamicModelsCache.lastFail = time.Now()
		stale := dynamicModelsCache.ids
		dynamicModelsCache.Unlock()
		if len(stale) > 0 {
			return stale
		}
		return nil
	}
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{} // 成功则清空负缓存
	dynamicModelsCache.Unlock()
	return infos
}

// cachedModelsSnapshot 只读模型目录缓存（TTL 内快照）；缓存冷/空 → nil。
// 不发起任何上游调用（与 fetchDynamicModels 的差异点，见 hintContext 注释）。
func cachedModelsSnapshot() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	if len(dynamicModelsCache.ids) == 0 || time.Since(dynamicModelsCache.fetched) >= dynamicModelsTTL {
		return nil
	}
	return dynamicModelsCache.ids
}
