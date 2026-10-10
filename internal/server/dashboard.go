package server

import (
	_ "embed"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/usermgr"
)

var (
	creditFetchMu  sync.Mutex
	creditFetching = make(map[string]time.Time)
)

func (h *Handler) triggerAsyncCreditFetch(a *auth.Auth, uid string) {
	if a == nil || a.AccessTokenValue() == "" || h.cfg.Upstream == nil || h.cfg.Pool == nil {
		return
	}
	creditFetchMu.Lock()
	last, fetching := creditFetching[uid]
	if fetching && time.Since(last) < 2*time.Minute {
		creditFetchMu.Unlock()
		return
	}
	creditFetching[uid] = time.Now()
	creditFetchMu.Unlock()

	go func() {
		if remain, _, _, _, err := h.cfg.Upstream.ResourceSummary(a); err == nil {
			h.cfg.Pool.SetCredits(uid, remain)
		}
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
	h.mux.HandleFunc("POST /ui/action/signin", h.withWebAuth(h.handleActionSignin))
	h.mux.HandleFunc("POST /ui/action/trial", h.withWebAuth(h.handleActionTrial))
	h.mux.HandleFunc("POST /ui/action/delete", h.withWebAuth(h.handleActionDelete))
	h.mux.HandleFunc("POST /ui/config/apikey", h.withWebAuth(h.handleConfigAPIKey))
	h.mux.HandleFunc("POST /ui/config/password", h.withWebAuth(h.handleConfigPassword))
	h.mux.HandleFunc("GET /ui/action/backup", h.withWebAuth(h.handleActionBackup))
	h.mux.HandleFunc("POST /ui/oauth/batch_import", h.withWebAuth(h.handleOAuthBatchImport))
	h.mux.HandleFunc("GET /ui/system/update/check", h.withWebAuth(h.handleSystemUpdateCheck))
	h.mux.HandleFunc("POST /ui/system/update/do", h.withWebAuth(h.handleSystemUpdateDo))

	// 用户中心与多用户管理端点
	h.mux.HandleFunc("POST /ui/user/apikey/reset", h.withWebAuth(h.handleUserAPIKeyReset))
	h.mux.HandleFunc("GET /ui/admin/users", h.withWebAuth(h.handleAdminListUsers))
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
	w.Write([]byte(portalHTML))
}

func (h *Handler) handleDashboardHTML(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(dashboardHTML))
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
	total, healthy, cooling, disabled, inFlightFull := h.cfg.Pool.CountsDetailed()
	sticky := 0
	if h.cfg.StickyCount != nil {
		sticky = h.cfg.StickyCount()
	}

	user := h.getWebSessionUser(r)

	authDir := h.getAuthDir()
	files, _ := auth.LoadAuthFiles(authDir)
	type AccountItem struct {
		UID               string                  `json:"uid"`
		Realm             string                  `json:"realm"`
		Nickname          string                  `json:"nickname"`
		Provider          string                  `json:"provider,omitempty"`
		Filename          string                  `json:"filename"`
		Credits           int64                   `json:"credits"`
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

		item := AccountItem{
			UID:         a.UID,
			Realm:       a.Realm(),
			Nickname:    a.Nickname,
			Provider:    a.Provider(),
			Filename:    filepath.Base(f),
			Owner:       a.Owner,
			IsMyAccount: isMine,
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
		// 若池内积分为 0 或未初始化，后台异步向上游查询真实积分并回填到账号池，绝不阻塞 Web 控制台响应
		if item.Credits == 0 && a.AccessTokenValue() != "" {
			h.triggerAsyncCreditFetch(a, a.UID)
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
		item := AccountItem{
			UID:               p.UID,
			Realm:             realm,
			Nickname:          nickname,
			Provider:          prov,
			Filename:          filename,
			Credits:           p.Credits,
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
		}
		if item.Credits == 0 && authObj != nil && authObj.AccessTokenValue() != "" {
			h.triggerAsyncCreditFetch(authObj, p.UID)
		}
		accounts = append(accounts, item)
	}

	// 最终全局防重保障：确保相同 UID 或相同 (realm, provider, rawUID) 的账号卡片绝不重复出现
	dedupedAccounts := make([]AccountItem, 0, len(accounts))
	finalSeenUID := make(map[string]bool)
	finalSeenFP := make(map[string]bool)
	for _, item := range accounts {
		fp := fmt.Sprintf("%s:%s:%s", item.Realm, item.Provider, strings.ToLower(auth.CleanRawUID(item.UID)))
		if finalSeenUID[item.UID] || (item.Provider != "" && finalSeenFP[fp]) {
			continue
		}
		finalSeenUID[item.UID] = true
		if item.Provider != "" {
			finalSeenFP[fp] = true
		}
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
