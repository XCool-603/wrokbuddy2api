// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/dshmgr"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/usermgr"
)

// contextKey 用于在 HTTP Context 中传递用户信息
type contextKey string

const userContextKey contextKey = "wb2a_user"

// Config handler 依赖。
type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	APIKey    string // 空 = 不鉴权（兼容单机模式）
	MaxRotate int    // 单请求最多换号次数，默认 3
	// Session 会话粘性路由器（可选；nil = 关闭粘性，纯 Pick 轮换）。
	Session *session.Router
	// StickyCount 返回当前粘性会话绑定数（供 /status）；nil 时报告 0。
	StickyCount func() int
	// RedisMode 观测字段（"upstash" / "noop"），供 /status 透出。
	RedisMode    string
	SoftCooldown time.Duration // 429/限流文案软冷却基数，默认 600s（连续触发指数退避，封顶 soft_rate_max）
	RefreshSkew  time.Duration // token 提前刷新窗口，默认 10m

	// PromptMode "passthrough"（默认，透传客户端原始 system）/ "custom"（网关替换）。
	PromptMode string
	// PromptText custom 模式下注入的系统提示词文本（来自 config.PromptText）。
	PromptText string

	// GlobalEnabled global realm 路由开关（config global.enabled，缺省 true）。
	// handler 侧第三道闸（与 main 注入 auth 开关、upstream.GlobalEnabled 呼应）：
	// false（显式逃生门）时即便 auth realm=global 也不提供 global: 模型名
	// （modelList 不列 global 名单）。
	GlobalEnabled bool

	// AdminEnabled 运维管理端点开关（config admin.enabled，默认 false）。
	// 关闭时 /admin/* 一律 404（而非 403——不向外暴露"这里存在管理面"）。
	AdminEnabled bool

	// ConfigPath 配置文件路径（用于面板动态修改保存配置，缺省 config.json）
	ConfigPath string

	// StateFile 状态文件路径（用于推导 persistent settings.json，位于 ./data/）
	StateFile string

	// WebPassword 控制台访问密码（非空时启用 WebUI 登录鉴权，空则单机免密）
	WebPassword string

	// UserMgr 用户与角色权限管理器
	UserMgr *usermgr.Manager

	// AuthDir 账号授权文件存放目录（对应 config.json 的 auth_dir，默认 ./auths）
	AuthDir string

	// DshMgr DeepSeek Harness 进程管理器
	DshMgr *dshmgr.Manager

	// StopFunc 终止服务上下文（用于升级完成后触发优雅重启或退出）
	StopFunc func()

	// QueueTimeout 账号在途满载排队等待超时时间，默认 60s
	QueueTimeout time.Duration

	// ModelAliases 虚拟模型别名映射表
	ModelAliases map[string]string

	// AlertMgr 运维风控告警通知管理器（可选）
	AlertMgr *AlertManager
}

// notFoundCooldown 上游 404 的固定短冷却时长。
const notFoundCooldown = 60 * time.Second

// wafCooldownBase WAF 403 软冷却基数。
const wafCooldownBase = 60 * time.Second

// ServiceName 网关身份标识。经 /healthz 响应体 service 字段与 X-Service 头同时透出。
const ServiceName = "workbuddy2api"

// CurrentVersion 当前发布版本
const CurrentVersion = "v1.2.27"

// dumpReqMinBytes WB2A_DUMP_REQ 调试落盘的"大请求"固定阈值（4MB）。
const dumpReqMinBytes = 4 << 20

// Handler 主路由。
type Handler struct {
	cfg     Config
	keyMu   sync.RWMutex
	webMu   sync.RWMutex
	mux     *http.ServeMux
	degrade degradeGate
	// wafIP WAF IP 级拦截状态机（fail-fast，wafip.go）：短窗多号 WAF 403 →
	// 激活期轮转遇 WAF 403 直接终止（不放大请求量）。进程内状态、重启清零。
	wafIP           wafIPGate
	inFlightTracker *InFlightTracker
	alertMgr        *AlertManager
}

func (h *Handler) getAuthDir() string {
	if h.cfg.AuthDir != "" {
		return h.cfg.AuthDir
	}
	return "./auths"
}

func (h *Handler) GetAPIKey() string {
	h.keyMu.RLock()
	defer h.keyMu.RUnlock()
	return h.cfg.APIKey
}

func (h *Handler) SetAPIKey(k string) {
	h.keyMu.Lock()
	defer h.keyMu.Unlock()
	h.cfg.APIKey = k
}

func (h *Handler) GetWebPassword() string {
	h.webMu.RLock()
	defer h.webMu.RUnlock()
	return h.cfg.WebPassword
}

func (h *Handler) SetWebPassword(p string) {
	h.webMu.Lock()
	defer h.webMu.Unlock()
	h.cfg.WebPassword = p
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 600 * time.Second // 软限流基数（连续触发按指数退避放大）
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	if cfg.PromptMode == "" {
		cfg.PromptMode = "passthrough" // 缺省 passthrough：透传客户端原始 system
	}
	if cfg.QueueTimeout <= 0 {
		cfg.QueueTimeout = 60 * time.Second
	}
	if cfg.AlertMgr == nil {
		cfg.AlertMgr = NewAlertManager(AlertConfig{Enabled: false})
	}
	if cfg.Pool == nil {
		cfg.Pool = pool.New("")
	}
	h := &Handler{
		cfg:             cfg,
		mux:             http.NewServeMux(),
		inFlightTracker: NewInFlightTracker(),
		alertMgr:        cfg.AlertMgr,
	}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("POST /v1/messages", h.withAuth(h.messagesHandler))
	h.mux.HandleFunc("POST /v1/videos/generations", h.withAuth(h.videoGenerations))
	h.mux.HandleFunc("POST /v2/videos/generations", h.withAuth(h.videoGenerations))
	h.mux.HandleFunc("POST /v1/videos/tasks", h.withAuth(h.videoTasksPost))
	h.mux.HandleFunc("POST /v2/videos/tasks", h.withAuth(h.videoTasksPost))
	h.mux.HandleFunc("GET /v1/videos/tasks/{id}", h.withAuth(h.videoTaskGet))
	h.mux.HandleFunc("GET /v1/videos/tasks", h.withAuth(h.videoTaskGet))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /v1/stats", h.withAuth(h.stats))
	h.mux.HandleFunc("POST /v1/stats/reset", h.withAuth(h.statsReset))
	// 运维管理端点（默认关闭，config admin.enabled 开启后生效）。
	if cfg.AdminEnabled {
		h.mux.HandleFunc("POST /admin/accounts/{uid}/disable", h.withAuth(h.adminAccountDisable))
		h.mux.HandleFunc("POST /admin/accounts/{uid}/enable", h.withAuth(h.adminAccountEnable))
		h.mux.HandleFunc("POST /admin/accounts/{uid}/revive", h.withAuth(h.adminAccountRevive))
	}
	h.mux.HandleFunc("GET /healthz", h.healthz)
	h.RegisterWebUI()
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 安全防护响应头 (Security Headers)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	w.Header().Set("X-XSS-Protection", "1; mode=block")
	w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")

	h.mux.ServeHTTP(w, r)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		currentKey := h.GetAPIKey()
		authz := r.Header.Get("Authorization")
		provided := strings.TrimPrefix(authz, "Bearer ")
		if provided == "" && r.Header.Get("x-api-key") != "" {
			provided = r.Header.Get("x-api-key")
		}

		// 1. 如果配置了 UserMgr，优先尝试校验用户专属 API Key
		if h.cfg.UserMgr != nil && provided != "" {
			if u, ok := h.cfg.UserMgr.FindByAPIKey(provided); ok {
				// 命中用户专属 API Key：校验 RPM 限频与每日积分配额 (F-07)
				if err := h.cfg.UserMgr.CheckRPM(provided); err != nil {
					writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit_exceeded", err.Error())
					return
				}
				if err := h.cfg.UserMgr.CheckDailyCredit(provided); err != nil {
					writeOpenAIError(w, http.StatusForbidden, "quota_exceeded", err.Error())
					return
				}
				// 注入 Request Context 并放行
				ctx := context.WithValue(r.Context(), userContextKey, u)
				next(w, r.WithContext(ctx))
				return
			}
		}

		// 2. 校验全局主 API Key（系统/管理员 Key）
		if currentKey != "" {
			if provided == "" ||
				subtle.ConstantTimeCompare([]byte(provided), []byte(currentKey)) != 1 {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
		}

		// 未设置 currentKey 或命中全局 Key，视为系统管理员身份
		if h.cfg.UserMgr != nil {
			if adminUser, ok := h.cfg.UserMgr.FindByUsername("admin"); ok {
				ctx := context.WithValue(r.Context(), userContextKey, adminUser)
				next(w, r.WithContext(ctx))
				return
			}
		}

		next(w, r)
	}
}

// ---------------------------------------------------------------------------
// 通用 JSON 与 OpenAI 错误响应助手
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
		},
	})
}

// writeOpenAIErrorHint 同 writeOpenAIError，另在 error 对象上附加 error.gateway_hint
func writeOpenAIErrorHint(w http.ResponseWriter, status int, code, msg, hint string) {
	if hint == "" {
		writeOpenAIError(w, status, code, msg)
		return
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message":      msg,
			"type":         "api_error",
			"code":         code,
			"gateway_hint": hint,
		},
	})
}
