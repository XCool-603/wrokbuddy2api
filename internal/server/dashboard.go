package server

import (
	_ "embed"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/usermgr"
)

var (
	creditFetchMu  sync.Mutex
	creditFetching = make(map[string]time.Time)

	creditDetailMu    sync.RWMutex
	creditDetailCache = make(map[string]upstream.CreditBreakdown)
)

func getCachedCreditBreakdown(uid string) *upstream.CreditBreakdown {
	creditDetailMu.RLock()
	defer creditDetailMu.RUnlock()
	bd, ok := creditDetailCache[uid]
	if !ok {
		return nil
	}
	cp := bd
	if len(bd.Packs) > 0 {
		cp.Packs = append([]upstream.CreditPackDetail(nil), bd.Packs...)
	}
	return &cp
}

// ModelQuotaSummary 聚合单模型在域内的限流/耗尽状态与实测积分单价。
type ModelQuotaSummary struct {
	Model        string  `json:"model"`
	LimitedCount int     `json:"limited_count"`
	Reason       string  `json:"reason,omitempty"`
	CostPer1k    float64 `json:"cost_per_1k,omitempty"`
	Samples      int     `json:"samples,omitempty"`
}

// RealmCreditSummary 全池或单域（cn / global）的积分总额度与模型额度明细。
type RealmCreditSummary struct {
	Remain                 float64             `json:"remain"`
	Total                  float64             `json:"total"`
	Used                   float64             `json:"used"`
	ActiveTotal            float64             `json:"active_total"`
	BaseRemain             float64             `json:"base_remain"`
	BonusRemain            float64             `json:"bonus_remain"`
	PurchasedRemain        float64             `json:"purchased_remain"`
	ExpiringRemain         float64             `json:"expiring_remain"`
	PackCount              int                 `json:"pack_count"`
	ActivePackCount        int                 `json:"active_pack_count"`
	AccountCount           int                 `json:"account_count"`
	HealthyCount           int                 `json:"healthy_count"`
	CoolingCount           int                 `json:"cooling_count"`
	DisabledCount          int                 `json:"disabled_count"`
	DetailAccountCount     int                 `json:"detail_account_count"`
	RateLimitedModelsCount int                 `json:"rate_limited_models_count"`
	RateLimitedModels      []string            `json:"rate_limited_models,omitempty"`
	ModelQuotas            []ModelQuotaSummary `json:"model_quotas,omitempty"`
}

// CreditSummaryPayload 控制台展示的全池、国内版、国际版三栏总额度汇总。
type CreditSummaryPayload struct {
	All    RealmCreditSummary `json:"all"`
	CN     RealmCreditSummary `json:"cn"`
	Global RealmCreditSummary `json:"global"`
}

type realmQuotaAcc struct {
	summary    RealmCreditSummary
	limitedMap map[string]int
	reasonMap  map[string]string
	costSumMap map[string]float64
	costWgtMap map[string]int
	samplesMap map[string]int
}

func newRealmQuotaAcc() *realmQuotaAcc {
	return &realmQuotaAcc{
		limitedMap: make(map[string]int),
		reasonMap:  make(map[string]string),
		costSumMap: make(map[string]float64),
		costWgtMap: make(map[string]int),
		samplesMap: make(map[string]int),
	}
}

func roundSummary2(v float64) float64 {
	return math.Round(v*100) / 100
}

func (a *realmQuotaAcc) add(disabled, manualDisabled, cooling bool, credits, expiring int64, bd *upstream.CreditBreakdown, rl []pool.RateLimitedModel, costs []pool.ModelCostStatus) {
	a.summary.AccountCount++
	if disabled || manualDisabled {
		a.summary.DisabledCount++
	} else if cooling {
		a.summary.CoolingCount++
	} else {
		a.summary.HealthyCount++
	}

	if bd != nil {
		a.summary.DetailAccountCount++
		a.summary.Remain += bd.Remain
		a.summary.Total += bd.Total
		a.summary.Used += bd.Used
		a.summary.ActiveTotal += bd.ActiveTotal
		a.summary.BaseRemain += bd.BaseRemain
		a.summary.BonusRemain += bd.BonusRemain
		a.summary.PurchasedRemain += bd.PurchasedRemain
		a.summary.ExpiringRemain += bd.ExpiringRemain
		a.summary.PackCount += bd.PackCount
		a.summary.ActivePackCount += bd.ActivePackCount
	} else if credits >= 0 {
		a.summary.Remain += float64(credits)
		if expiring > 0 {
			a.summary.ExpiringRemain += float64(expiring)
		}
	}

	for _, rm := range rl {
		m := strings.TrimSpace(rm.Model)
		if m == "" {
			continue
		}
		a.summary.RateLimitedModelsCount++
		a.limitedMap[m]++
		if a.reasonMap[m] == "" && rm.Reason != "" {
			a.reasonMap[m] = rm.Reason
		}
	}
	for _, mc := range costs {
		m := strings.TrimSpace(mc.Model)
		if m == "" {
			continue
		}
		w := mc.Samples
		if w <= 0 {
			w = 1
		}
		a.costSumMap[m] += mc.CostPer1k * float64(w)
		a.costWgtMap[m] += w
		a.samplesMap[m] += mc.Samples
	}
}

func (a *realmQuotaAcc) finalize() RealmCreditSummary {
	out := a.summary
	out.Remain = roundSummary2(out.Remain)
	out.Total = roundSummary2(out.Total)
	out.Used = roundSummary2(out.Used)
	out.ActiveTotal = roundSummary2(out.ActiveTotal)
	out.BaseRemain = roundSummary2(out.BaseRemain)
	out.BonusRemain = roundSummary2(out.BonusRemain)
	out.PurchasedRemain = roundSummary2(out.PurchasedRemain)
	out.ExpiringRemain = roundSummary2(out.ExpiringRemain)

	modelSet := make(map[string]struct{})
	for m := range a.limitedMap {
		modelSet[m] = struct{}{}
	}
	for m := range a.costWgtMap {
		modelSet[m] = struct{}{}
	}
	if len(a.limitedMap) > 0 {
		limitedNames := make([]string, 0, len(a.limitedMap))
		for m := range a.limitedMap {
			limitedNames = append(limitedNames, m)
		}
		sort.Strings(limitedNames)
		out.RateLimitedModels = make([]string, 0, len(limitedNames))
		for _, m := range limitedNames {
			if cnt := a.limitedMap[m]; cnt > 1 {
				out.RateLimitedModels = append(out.RateLimitedModels, fmt.Sprintf("%s (×%d)", m, cnt))
			} else {
				out.RateLimitedModels = append(out.RateLimitedModels, m)
			}
		}
	}
	if len(modelSet) > 0 {
		names := make([]string, 0, len(modelSet))
		for m := range modelSet {
			names = append(names, m)
		}
		sort.Strings(names)
		out.ModelQuotas = make([]ModelQuotaSummary, 0, len(names))
		for _, m := range names {
			avgCost := 0.0
			if w := a.costWgtMap[m]; w > 0 {
				avgCost = roundSummary2(a.costSumMap[m] / float64(w))
			}
			out.ModelQuotas = append(out.ModelQuotas, ModelQuotaSummary{
				Model:        m,
				LimitedCount: a.limitedMap[m],
				Reason:       a.reasonMap[m],
				CostPer1k:    avgCost,
				Samples:      a.samplesMap[m],
			})
		}
	}
	return out
}

type creditSummaryBuilder struct {
	all    *realmQuotaAcc
	cn     *realmQuotaAcc
	global *realmQuotaAcc
}

func newCreditSummaryBuilder() *creditSummaryBuilder {
	return &creditSummaryBuilder{
		all:    newRealmQuotaAcc(),
		cn:     newRealmQuotaAcc(),
		global: newRealmQuotaAcc(),
	}
}

func (b *creditSummaryBuilder) add(realm string, disabled, manualDisabled, cooling bool, credits, expiring int64, bd *upstream.CreditBreakdown, rl []pool.RateLimitedModel, costs []pool.ModelCostStatus) {
	b.all.add(disabled, manualDisabled, cooling, credits, expiring, bd, rl, costs)
	switch strings.ToLower(strings.TrimSpace(realm)) {
	case "cn":
		b.cn.add(disabled, manualDisabled, cooling, credits, expiring, bd, rl, costs)
	case "global":
		b.global.add(disabled, manualDisabled, cooling, credits, expiring, bd, rl, costs)
	}
}

func (b *creditSummaryBuilder) build() CreditSummaryPayload {
	return CreditSummaryPayload{
		All:    b.all.finalize(),
		CN:     b.cn.finalize(),
		Global: b.global.finalize(),
	}
}

// fetchAndSyncCredit 安全地拉取单账号最新真实积分并更新池（自带 Token 过期自动续期、401 重试及精确分桶计算）
func (h *Handler) fetchAndSyncCredit(a *auth.Auth, uid string) bool {
	if h == nil || h.cfg.Upstream == nil || h.cfg.Pool == nil {
		return false
	}
	// 优先使用内存中最新鲜的凭证对象（已包含运行期刷新过的 Token）
	if live := h.cfg.Pool.AuthByUID(uid); live != nil {
		a = live
	}
	if a == nil || a.AccessTokenValue() == "" {
		return false
	}
	if a.NeedsRefresh(2 * 3600) {
		if err := h.cfg.Upstream.RefreshToken(a); err == nil {
			a.BackfillRealm()
			_ = a.SaveAtomic()
		}
	}
	bd, err := h.cfg.Upstream.UserResourceBreakdown(a, 7*24*time.Hour)
	if err != nil && a.RefreshTokenValue() != "" {
		// 若遇过期 401，强制刷新一次 Token 后重试
		if rerr := h.cfg.Upstream.RefreshToken(a); rerr == nil {
			a.BackfillRealm()
			_ = a.SaveAtomic()
			bd, err = h.cfg.Upstream.UserResourceBreakdown(a, 7*24*time.Hour)
		}
	}
	if err != nil {
		return false
	}
	creditDetailMu.Lock()
	creditDetailCache[uid] = bd
	creditDetailMu.Unlock()

	remain := int64(bd.Remain)
	h.cfg.Pool.SetCreditsDetailed(uid, remain, int64(bd.ExpiringRemain))
	if h.alertMgr != nil && remain < int64(h.alertMgr.cfg.MinCredit) {
		h.alertMgr.NotifyLowCredit(uid, a.Nickname, remain)
	}
	return true
}

// syncPoolCreditsWithTimeout 并发刷新所有在池账号的实时积分（带超时兜底），用于强制刷新时直接输出最新账单
func (h *Handler) syncPoolCreditsWithTimeout(timeout time.Duration) {
	if h == nil || h.cfg.Pool == nil || h.cfg.Upstream == nil {
		return
	}
	var wg sync.WaitGroup
	for _, st := range h.cfg.Pool.List() {
		a := h.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessTokenValue() == "" {
			continue
		}
		wg.Add(1)
		go func(authObj *auth.Auth, uid string) {
			defer wg.Done()
			h.fetchAndSyncCredit(authObj, uid)
		}(a, st.UID)
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

func (h *Handler) triggerAsyncCreditFetch(a *auth.Auth, uid string, force bool) {
	if a == nil || a.AccessTokenValue() == "" || h.cfg.Upstream == nil || h.cfg.Pool == nil {
		return
	}
	creditFetchMu.Lock()
	last, fetching := creditFetching[uid]
	// 默认防抖大幅优化为 15 秒（大幅增强积分实时性），force 为 true 时允许立即穿透强制拉取
	if !force && fetching && time.Since(last) < 15*time.Second {
		creditFetchMu.Unlock()
		return
	}
	creditFetching[uid] = time.Now()
	creditFetchMu.Unlock()

	go func() {
		h.fetchAndSyncCredit(a, uid)
	}()
}

//go:embed portal.html
var portalHTML string

//go:embed dashboard.html
var dashboardHTML string

func (h *Handler) RegisterWebUI() {
	// 前台炫酷门户展示页 (公开展示，拿得出手给别人看)
	h.mux.HandleFunc("GET /{$}", h.handlePortalHTML)
	// 公共网关健康与模型状态 (无需鉴权，脱敏安全)
	h.mux.HandleFunc("GET /ui/public/status", h.handlePublicStatus)

	// 管理控制台入口 (原控制台移至 /console 及 /dashboard)
	h.mux.HandleFunc("GET /console", h.handleDashboardHTML)
	h.mux.HandleFunc("GET /console/", h.handleDashboardHTML)
	h.mux.HandleFunc("GET /dashboard", h.handleDashboardHTML)
	h.mux.HandleFunc("GET /dashboard/", h.handleDashboardHTML)

	h.mux.HandleFunc("POST /ui/auth/login", h.handleWebLogin)
	h.mux.HandleFunc("POST /ui/auth/logout", h.handleWebLogout)
	h.mux.HandleFunc("POST /ui/auth/register", h.handleWebRegister)

	h.mux.HandleFunc("GET /ui/data", h.withWebAuth(h.handleDashboardData))
	h.mux.HandleFunc("POST /ui/oauth/start", h.withWebAuth(h.handleOAuthStart))
	h.mux.HandleFunc("POST /ui/oauth/poll", h.withWebAuth(h.handleOAuthPoll))
	h.mux.HandleFunc("POST /ui/oauth/import", h.withWebAuth(h.handleOAuthImport))
	h.mux.HandleFunc("POST /ui/account/admin", h.withWebAuth(h.handleAccountAdmin))
	h.mux.HandleFunc("POST /ui/account/update_token", h.withWebAuth(h.handleAccountUpdateToken))
	h.mux.HandleFunc("GET /ui/inflight/data", h.withWebAuth(h.handleInFlightData))
	h.mux.HandleFunc("POST /ui/action/signin", h.withWebAuth(h.handleActionSignin))
	h.mux.HandleFunc("POST /ui/action/trial", h.withWebAuth(h.handleActionTrial))
	h.mux.HandleFunc("POST /ui/action/welfare", h.withWebAuth(h.handleActionWelfare))
	h.mux.HandleFunc("POST /ui/action/travel", h.withWebAuth(h.handleActionTravel))
	h.mux.HandleFunc("POST /ui/action/tasks", h.withWebAuth(h.handleActionTasks))
	h.mux.HandleFunc("POST /ui/action/bind_invite", h.withWebAuth(h.handleActionBindInvite))
	h.mux.HandleFunc("POST /ui/action/delete", h.withWebAuth(h.handleActionDelete))
	h.mux.HandleFunc("POST /ui/config/apikey", h.withWebAuth(h.handleConfigAPIKey))
	h.mux.HandleFunc("POST /ui/config/password", h.withWebAuth(h.handleConfigPassword))
	h.mux.HandleFunc("GET /ui/action/backup", h.withWebAuth(h.handleActionBackup))
	h.mux.HandleFunc("GET /ui/backup/export", h.withWebAuth(h.handleActionBackup))
	h.mux.HandleFunc("POST /ui/backup/import", h.withWebAuth(h.handleOAuthBatchImport))
	h.mux.HandleFunc("POST /ui/oauth/batch_import", h.withWebAuth(h.handleOAuthBatchImport))
	h.mux.HandleFunc("GET /ui/system/update/check", h.withWebAuth(h.handleSystemUpdateCheck))
	h.mux.HandleFunc("POST /ui/system/update/do", h.withWebAuth(h.handleSystemUpdateDo))

	// 用户中心与多用户管理端点
	h.mux.HandleFunc("POST /ui/user/apikey/reset", h.withWebAuth(h.handleUserAPIKeyReset))
	h.mux.HandleFunc("GET /ui/admin/users", h.withWebAuth(h.handleAdminListUsers))
	h.mux.HandleFunc("POST /ui/admin/user/quota", h.withWebAuth(h.handleAdminSetUserQuota))
	h.mux.HandleFunc("POST /ui/admin/user/toggle", h.withWebAuth(h.handleAdminToggleUser))
	h.mux.HandleFunc("POST /ui/admin/user/role", h.withWebAuth(h.handleAdminSetUserRole))
	h.mux.HandleFunc("POST /ui/admin/user/delete", h.withWebAuth(h.handleAdminDeleteUser))
	h.mux.HandleFunc("POST /ui/admin/user/create", h.withWebAuth(h.handleAdminCreateUser))
	h.mux.HandleFunc("POST /ui/admin/user/reset_password", h.withWebAuth(h.handleAdminResetUserPassword))
	h.mux.HandleFunc("POST /ui/admin/user/reset_key", h.withWebAuth(h.handleAdminResetUserAPIKey))
	h.mux.HandleFunc("POST /ui/admin/system/allow_register", h.withWebAuth(h.handleAdminSetAllowRegister))

	// DeepSeek Harness (dsh) 进程管理端点
	h.mux.HandleFunc("GET /ui/dsh/status", h.withWebAuth(h.handleDshStatus))
	h.mux.HandleFunc("POST /ui/dsh/install", h.withWebAuth(h.handleDshInstall))
	h.mux.HandleFunc("POST /ui/dsh/reset", h.withWebAuth(h.handleDshReset))
	h.mux.HandleFunc("POST /ui/dsh/start", h.withWebAuth(h.handleDshStart))
	h.mux.HandleFunc("POST /ui/dsh/stop", h.withWebAuth(h.handleDshStop))
	h.mux.HandleFunc("POST /ui/dsh/export", h.withWebAuth(h.handleDshExport))
}

func (h *Handler) handlePortalHTML(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.Write([]byte(portalHTML))
}

func (h *Handler) handleDashboardHTML(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.Write([]byte(dashboardHTML))
}

// WarmupCreditsAsync 服务启动后异步全量同步全池账号（含 CN 与 GLOBAL）最新积分余额
func (h *Handler) WarmupCreditsAsync() {
	go h.syncPoolCreditsWithTimeout(15 * time.Second)
}

func (h *Handler) handlePublicStatus(w http.ResponseWriter, r *http.Request) {
	total, healthy := 0, 0
	if h.cfg.Pool != nil {
		total, healthy, _, _, _ = h.cfg.Pool.CountsDetailed()
	}
	modelsCount := 15
	if h.cfg.Upstream != nil {
		modelsCount = len(h.modelList())
	}

	cnServable := false
	globalServable := false
	if h.cfg.Pool != nil {
		cnServable = h.cfg.Pool.ServableForRealm("cn")
		globalServable = h.cfg.Pool.ServableForRealm("global")
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"service":      ServiceName,
		"version":      CurrentVersion,
		"status":       "online",
		"models_count": modelsCount,
		"dual_realm": map[string]bool{
			"cn":     cnServable,
			"global": globalServable,
		},
		"capacity": map[string]any{
			"ready":   healthy > 0,
			"healthy": healthy,
			"total":   total,
		},
	})
}

// isAccountOwnedBy 判定账号是否归属于当前用户（是否具有管理和删除权限）。
// 管理员（或单机免密未登录）对所有账号均具备管理权。普通用户仅对显式归属自己的账号具备管理权。
func (h *Handler) isAccountOwnedBy(a *auth.Auth, user *usermgr.User) bool {
	if a == nil {
		return false
	}
	if user == nil || user.Role == usermgr.RoleAdmin || user.Username == "admin" || user.ID == "u_admin" || user.ID == "admin" {
		return true
	}
	owner := a.OwnerValue()
	return owner == user.ID || owner == user.Username
}

// isAccountVisibleTo 判定账号是否对当前用户可见。
// 管理员恒可见全部账号；自己绑定的账号恒可见；
// 公共/系统账号（owner 为空、"public"、"admin"、"u_admin"）对所有用户均可见。
func (h *Handler) isAccountVisibleTo(a *auth.Auth, user *usermgr.User) bool {
	if a == nil {
		return false
	}
	if user == nil || user.Role == usermgr.RoleAdmin || user.Username == "admin" || user.ID == "u_admin" || user.ID == "admin" {
		return true
	}
	owner := a.OwnerValue()
	if owner == user.ID || owner == user.Username {
		return true
	}
	return owner == "" || owner == "public" || owner == "admin" || owner == "u_admin"
}

func (h *Handler) handleDashboardData(w http.ResponseWriter, r *http.Request) {
	isFresh := r.URL.Query().Get("fresh") == "1" || r.URL.Query().Get("force") == "1"
	if isFresh {
		h.syncPoolCreditsWithTimeout(4 * time.Second)
	}
	total, healthy, cooling, disabled, inFlightFull := h.cfg.Pool.CountsDetailed()
	sticky := 0
	if h.cfg.StickyCount != nil {
		sticky = h.cfg.StickyCount()
	}

	user := h.getWebSessionUser(r)

	authDir := h.getAuthDir()
	files, _ := auth.LoadAuthFiles(authDir)
	type AccountItem struct {
		UID               string                    `json:"uid"`
		Realm             string                    `json:"realm"`
		Nickname          string                    `json:"nickname"`
		Provider          string                    `json:"provider,omitempty"`
		Filename          string                    `json:"filename"`
		Credits           int64                     `json:"credits"`
		CreditsExpiring   int64                     `json:"credits_expiring,omitempty"`
		CreditDetail      *upstream.CreditBreakdown `json:"credit_detail,omitempty"`
		Disabled          bool                    `json:"disabled"`
		DisabledReason    string                  `json:"disabled_reason"`
		ManualDisabled    bool                    `json:"manual_disabled"`
		ManualReason      string                  `json:"manual_reason"`
		Cooling           bool                    `json:"cooling"`
		CoolKind          string                  `json:"cool_kind,omitempty"`
		CoolRemaining     int64                   `json:"cool_remaining_sec,omitempty"`
		Reason            string                  `json:"reason,omitempty"`
		RateLimitedModels []pool.RateLimitedModel `json:"rate_limited_models,omitempty"`
		ModelCosts        []pool.ModelCostStatus  `json:"model_costs,omitempty"`
		Owner             string                  `json:"owner"`
		IsMyAccount       bool                    `json:"is_my_account"`
		InviteCode        string                  `json:"invite_code,omitempty"`
		InviteURL         string                  `json:"invite_url,omitempty"`
	}

	poolList := h.cfg.Pool.List()
	poolMap := make(map[string]pool.Status)
	for _, p := range poolList {
		poolMap[p.UID] = p
		poolMap[p.Realm+":"+p.UID] = p
		if p.FilePath != "" {
			poolMap[filepath.Base(p.FilePath)] = p
		}
	}

	seenKey := make(map[string]bool)
	seenAccount := make(map[string]bool)
	accounts := make([]AccountItem, 0, len(files)+len(poolList))
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil {
			continue
		}
		a.FilePath = f
		if a.Provider() == "" {
			base := strings.ToLower(filepath.Base(f))
			if strings.Contains(base, "twitter") || strings.Contains(base, "-x-") {
				a.SetProvider("twitter")
			} else if strings.Contains(base, "google") {
				a.SetProvider("google")
			} else if strings.Contains(base, "github") {
				a.SetProvider("github")
			}
		}

		if !h.isAccountVisibleTo(a, user) {
			continue
		}

		// 账号唯一身份指纹：realm + provider + rawUID
		acctFingerprint := fmt.Sprintf("%s:%s:%s", a.Realm(), a.Provider(), strings.ToLower(a.RawUID()))
		if seenAccount[acctFingerprint] {
			// 磁盘上存在同一账号的历史冗余副本（如 -2.json），控制台去重绝不展示重复卡片
			continue
		}
		seenAccount[acctFingerprint] = true

		isMine := h.isAccountOwnedBy(a, user)

		key := fmt.Sprintf("%s:%s", a.Realm(), a.UID)
		rawKey := fmt.Sprintf("%s:%s", a.Realm(), a.RawUID())
		seenKey[key] = true
		seenKey[rawKey] = true
		seenKey[a.UID] = true
		seenKey[filepath.Base(f)] = true

		code := upstream.DefaultInviteCode(a)
		item := AccountItem{
			UID:         a.UID,
			Realm:       a.Realm(),
			Nickname:    a.Nickname,
			Provider:    a.Provider(),
			Filename:    filepath.Base(f),
			Owner:       a.Owner,
			IsMyAccount: isMine,
			InviteCode:  code,
			InviteURL:   upstream.DefaultInviteURL(a, code),
		}
		var matched *pool.Status
		if p, ok := poolMap[filepath.Base(f)]; ok {
			matched = &p
		} else if p, ok := poolMap[a.UID]; ok {
			matched = &p
		} else if p, ok := poolMap[key]; ok {
			matched = &p
		} else if p, ok := poolMap[rawKey]; ok {
			matched = &p
		}

		if matched != nil {
			item.UID = matched.UID
			if matched.Provider != "" {
				item.Provider = matched.Provider
			}
			item.Credits = matched.Credits
			item.CreditsExpiring = matched.CreditsExpiring
			item.Disabled = matched.Disabled
			item.DisabledReason = matched.DisabledReason
			item.ManualDisabled = matched.ManualDisabled
			item.ManualReason = matched.ManualReason
			item.Cooling = matched.Cooling
			item.CoolKind = matched.CoolKind
			item.CoolRemaining = matched.CoolRemaining
			item.Reason = matched.Reason
			item.RateLimitedModels = matched.RateLimitedModels
			item.ModelCosts = matched.ModelCosts

			seenKey[matched.UID] = true
			seenKey[matched.Realm+":"+matched.UID] = true
			if matched.FilePath != "" {
				seenKey[filepath.Base(matched.FilePath)] = true
			}
			matchedFP := fmt.Sprintf("%s:%s:%s", matched.Realm, matched.Provider, strings.ToLower(auth.CleanRawUID(matched.UID)))
			seenAccount[matchedFP] = true
		}
		// 实时积分自动同步：若当前账号有 token，异步拉取最新真实积分与即将到期积分（带 15s 平滑防抖，前端传 fresh=1 时强制立即拉取），绝不阻塞 Web 控制台响应！
		if a.AccessTokenValue() != "" {
			h.triggerAsyncCreditFetch(a, item.UID, isFresh)
		}
		accounts = append(accounts, item)
	}

	// 兜底与并集补齐：如果有任何在内存账号池（Pool）中已存在但在磁盘扫描中遗漏的账号，
	// 无论文件路径差异或磁盘读取延时，均保证在账号池中 100% 完整展示给用户！
	for _, p := range poolList {
		poolKey := fmt.Sprintf("%s:%s", p.Realm, p.UID)
		if seenKey[p.UID] || seenKey[poolKey] {
			continue
		}
		authObj := h.cfg.Pool.AuthByUID(p.UID)
		if authObj != nil {
			rawKey := fmt.Sprintf("%s:%s", authObj.Realm(), authObj.RawUID())
			if seenKey[rawKey] || seenKey[authObj.RawUID()] {
				continue
			}
			fp := fmt.Sprintf("%s:%s:%s", authObj.Realm(), authObj.Provider(), strings.ToLower(authObj.RawUID()))
			if seenAccount[fp] {
				continue
			}
		} else {
			clean := auth.CleanRawUID(p.UID)
			fp := fmt.Sprintf("%s:%s:%s", p.Realm, p.Provider, strings.ToLower(clean))
			if seenAccount[fp] {
				continue
			}
		}
		if authObj != nil && !h.isAccountVisibleTo(authObj, user) {
			continue
		}

		owner := ""
		realm := p.Realm
		nickname := p.Nickname
		filename := fmt.Sprintf("workbuddy-%s.json", p.UID)
		if authObj != nil {
			owner = authObj.Owner
			realm = authObj.Realm()
			if authObj.Nickname != "" {
				nickname = authObj.Nickname
			}
			if authObj.FilePath != "" {
				filename = filepath.Base(authObj.FilePath)
			}
		}

		isMine := false
		if authObj != nil {
			isMine = h.isAccountOwnedBy(authObj, user)
		} else if user == nil || user.Role == usermgr.RoleAdmin {
			isMine = true
		}

		seenKey[poolKey] = true
		seenKey[p.UID] = true
		prov := p.Provider
		if prov == "" && authObj != nil {
			prov = authObj.Provider()
		}
		poolInviteCode := ""
		if authObj != nil {
			poolInviteCode = upstream.DefaultInviteCode(authObj)
		} else {
			poolInviteCode = auth.CleanRawUID(p.UID)
		}
		poolInviteURL := ""
		if authObj != nil {
			poolInviteURL = upstream.DefaultInviteURL(authObj, poolInviteCode)
		} else if realm == "global" {
			poolInviteURL = "https://www.workbuddy.ai/i/" + poolInviteCode
		} else {
			poolInviteURL = "https://workbuddy.cn/i/" + poolInviteCode
		}
		item := AccountItem{
			UID:               p.UID,
			Realm:             realm,
			Nickname:          nickname,
			Provider:          prov,
			Filename:          filename,
			Credits:           p.Credits,
			CreditsExpiring:   p.CreditsExpiring,
			Disabled:          p.Disabled,
			DisabledReason:    p.DisabledReason,
			ManualDisabled:    p.ManualDisabled,
			ManualReason:      p.ManualReason,
			Cooling:           p.Cooling,
			CoolKind:          p.CoolKind,
			CoolRemaining:     p.CoolRemaining,
			Reason:            p.Reason,
			RateLimitedModels: p.RateLimitedModels,
			ModelCosts:        p.ModelCosts,
			Owner:             owner,
			IsMyAccount:       isMine,
			InviteCode:        poolInviteCode,
			InviteURL:         poolInviteURL,
		}
		if (item.Credits == 0 || getCachedCreditBreakdown(p.UID) == nil) && authObj != nil && authObj.AccessTokenValue() != "" {
			h.triggerAsyncCreditFetch(authObj, p.UID, isFresh)
		}
		accounts = append(accounts, item)
	}

	// 最终全局防重保障：确保相同 UID 或相同 (realm, provider, rawUID) 的账号卡片绝不重复出现
	dedupedAccounts := make([]AccountItem, 0, len(accounts))
	finalSeenUID := make(map[string]bool)
	finalSeenFP := make(map[string]bool)
	creditSummary := newCreditSummaryBuilder()
	for _, item := range accounts {
		fp := fmt.Sprintf("%s:%s:%s", item.Realm, item.Provider, strings.ToLower(auth.CleanRawUID(item.UID)))
		if finalSeenUID[item.UID] || (item.Provider != "" && finalSeenFP[fp]) {
			continue
		}
		finalSeenUID[item.UID] = true
		if item.Provider != "" {
			finalSeenFP[fp] = true
		}
		if bd := getCachedCreditBreakdown(item.UID); bd != nil {
			item.CreditDetail = bd
		}
		creditSummary.add(item.Realm, item.Disabled, item.ManualDisabled, item.Cooling, item.Credits, item.CreditsExpiring, item.CreditDetail, item.RateLimitedModels, item.ModelCosts)
		dedupedAccounts = append(dedupedAccounts, item)
	}
	accounts = dedupedAccounts


	// 状态统计：普通用户只统计属于自己的账号状态，管理员统计全局
	statTotal, statHealthy, statCooling, statDisabled, statInFlight := total, healthy, cooling, disabled, inFlightFull
	if user != nil && user.Role != usermgr.RoleAdmin {
		statTotal = len(accounts)
		statHealthy, statCooling, statDisabled = 0, 0, 0
		for _, it := range accounts {
			if it.Disabled || it.ManualDisabled {
				statDisabled++
			} else if it.Cooling {
				statCooling++
			} else {
				statHealthy++
			}
		}
	}

	models := h.modelList()
	statsSnap := MetricsSnapshotOf()
	h.enrichCredits(&statsSnap)

	// APIKey：普通用户显示用户自己的 APIKey，管理员显示全局 APIKey
	displayAPIKey := h.GetAPIKey()
	if user != nil && user.APIKey != "" {
		displayAPIKey = user.APIKey
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"service": ServiceName,
		"status": map[string]any{
			"total":           statTotal,
			"healthy":         statHealthy,
			"cooling":         statCooling,
			"disabled":        statDisabled,
			"in_flight_full":  statInFlight,
			"sticky_sessions": sticky,
		},
		"accounts":       accounts,
		"creditSummary":  creditSummary.build(),
		"models":         models,
		"apiKey":         displayAPIKey,
		"hasPassword":    h.GetWebPassword() != "",
		"currentVersion": CurrentVersion,
		"isDocker":       isDockerEnvironment(),
		"recentLogs":     GetRecentLogs(),
		"stats":          statsSnap,
		"currentUser":    user,
		"allowRegister":  h.cfg.UserMgr != nil && h.cfg.UserMgr.AllowRegister(),
	})
}
