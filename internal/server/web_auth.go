package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/usermgr"
)

// webSessionCookieName Web 控制台会话 Cookie 名
const webSessionCookieName = "wb2a_session"

type authRateLimiter struct {
	mu           sync.Mutex
	failedLogins map[string][]time.Time
	registers    map[string][]time.Time
}

var globalAuthLimiter = &authRateLimiter{
	failedLogins: make(map[string][]time.Time),
	registers:    make(map[string][]time.Time),
}

func getClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		if len(parts) > 0 {
			ip := strings.TrimSpace(parts[0])
			if ip != "" {
				return ip
			}
		}
	}
	if xrip := strings.TrimSpace(r.Header.Get("X-Real-IP")); xrip != "" {
		return xrip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	return r.RemoteAddr
}

func (l *authRateLimiter) isLoginLocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	times, ok := l.failedLogins[ip]
	if !ok {
		return false
	}
	cutoff := time.Now().Add(-5 * time.Minute)
	valid := make([]time.Time, 0, len(times))
	for _, t := range times {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}
	l.failedLogins[ip] = valid
	return len(valid) >= 5
}

func (l *authRateLimiter) recordFailedLogin(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-5 * time.Minute)
	times := l.failedLogins[ip]
	valid := make([]time.Time, 0, len(times)+1)
	for _, t := range times {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}
	valid = append(valid, time.Now())
	l.failedLogins[ip] = valid
}

func (l *authRateLimiter) resetLogin(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failedLogins, ip)
}

func (l *authRateLimiter) isRegisterLimited(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	times, ok := l.registers[ip]
	if !ok {
		return false
	}
	cutoff := time.Now().Add(-10 * time.Minute)
	valid := make([]time.Time, 0, len(times))
	for _, t := range times {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}
	l.registers[ip] = valid
	return len(valid) >= 5
}

func (l *authRateLimiter) recordRegister(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-10 * time.Minute)
	times := l.registers[ip]
	valid := make([]time.Time, 0, len(times)+1)
	for _, t := range times {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}
	valid = append(valid, time.Now())
	l.registers[ip] = valid
}

type webSessionInfo struct {
	User      *usermgr.User
	ExpiresAt time.Time
}

// webSessions 存储已登录的 Web 会话 (token -> webSessionInfo)
var webSessions sync.Map

// cleanExpiredSessions 清理已过期的 Web 会话，防止长期运行内存泄漏
func cleanExpiredSessions() {
	now := time.Now()
	webSessions.Range(func(key, val any) bool {
		if sess, ok := val.(webSessionInfo); ok {
			if now.After(sess.ExpiresAt) {
				webSessions.Delete(key)
			}
		}
		return true
	})
}

func generateSessionToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (h *Handler) getWebSessionUser(r *http.Request) *usermgr.User {
	cookie, err := r.Cookie(webSessionCookieName)
	if err == nil && cookie.Value != "" {
		if val, ok := webSessions.Load(cookie.Value); ok {
			if sess, ok := val.(webSessionInfo); ok {
				if time.Now().Before(sess.ExpiresAt) {
					// 刷新最新用户状态（从 UserMgr 获取以防被禁用或被改角色）
					if h.cfg.UserMgr != nil && sess.User != nil {
						if fresh, ok := h.cfg.UserMgr.FindByUsername(sess.User.Username); ok {
							return fresh
						}
					}
					if sess.User != nil {
						return sess.User
					}
					// sess.User == nil，说明是单机管理密码登录建立的管理员会话
					if h.cfg.UserMgr != nil {
						if admin, ok := h.cfg.UserMgr.FindByUsername("admin"); ok {
							return admin
						}
					}
					return &usermgr.User{
						ID:       "u_admin",
						Username: "admin",
						Role:     usermgr.RoleAdmin,
					}
				}
				webSessions.Delete(cookie.Value)
			}
		}
	}

	// 兼容 X-Web-Password 或者是免密单机模式
	pw := h.GetWebPassword()
	if customPw := r.Header.Get("X-Web-Password"); customPw != "" && pw != "" {
		if subtle.ConstantTimeCompare([]byte(customPw), []byte(pw)) == 1 {
			if h.cfg.UserMgr != nil {
				if admin, ok := h.cfg.UserMgr.FindByUsername("admin"); ok {
					return admin
				}
			}
			return &usermgr.User{
				ID:       "u_admin",
				Username: "admin",
				Role:     usermgr.RoleAdmin,
			}
		}
	}

	// 如果没有登录会话（免密单机模式，或尚未强制登录），默认作为管理员 admin 身份，确保所有账号和功能均立即可用
	if h.cfg.UserMgr != nil {
		if admin, ok := h.cfg.UserMgr.FindByUsername("admin"); ok {
			return admin
		}
	}

	return &usermgr.User{
		ID:       "u_admin",
		Username: "admin",
		Role:     usermgr.RoleAdmin,
	}
}

func (h *Handler) checkWebAuth(r *http.Request) bool {
	pw := h.GetWebPassword()

	// 1. 支持 Header 鉴权: X-Web-Password
	if customPw := r.Header.Get("X-Web-Password"); customPw != "" && pw != "" {
		if subtle.ConstantTimeCompare([]byte(customPw), []byte(pw)) == 1 {
			return true
		}
	}

	// 2. Cookie 会话校验
	cookie, err := r.Cookie(webSessionCookieName)
	if err == nil && cookie.Value != "" {
		if val, ok := webSessions.Load(cookie.Value); ok {
			if sess, ok := val.(webSessionInfo); ok {
				if time.Now().Before(sess.ExpiresAt) {
					// 检查用户是否被禁用
					if h.cfg.UserMgr != nil && sess.User != nil {
						if fresh, ok := h.cfg.UserMgr.FindByUsername(sess.User.Username); ok && fresh.Disabled {
							webSessions.Delete(cookie.Value)
							return false
						}
					}
					return true
				}
				webSessions.Delete(cookie.Value)
			}
		}
	}

	// 未设密码时（单机模式），免密放行进入控制台（自动作为 admin 身份）
	if pw == "" {
		return true
	}
	return false
}

func (h *Handler) withWebAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.checkWebAuth(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error":          "需要登录控制台",
				"need_login":     true,
				"has_password":   h.GetWebPassword() != "",
				"allow_register": h.cfg.UserMgr != nil && h.cfg.UserMgr.AllowRegister(),
			})
			return
		}
		next(w, r)
	}
}

func (h *Handler) handleWebLogin(w http.ResponseWriter, r *http.Request) {
	cleanExpiredSessions()

	ip := getClientIP(r)
	if globalAuthLimiter.isLoginLocked(ip) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": "登录失败次数过多，为保障账户安全已暂时锁定，请 5 分钟后再试",
		})
		return
	}

	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}

	req.Username = strings.TrimSpace(req.Username)
	req.Password = strings.TrimSpace(req.Password)

	var loggedUser *usermgr.User

	// 1. 如果启用了 UserMgr，优先走多用户体系
	if h.cfg.UserMgr != nil {
		uname := req.Username
		if uname == "" {
			uname = "admin"
		}
		u, err := h.cfg.UserMgr.Authenticate(uname, req.Password)
		if err != nil {
			pw := h.GetWebPassword()
			if uname == "admin" && pw != "" && subtle.ConstantTimeCompare([]byte(req.Password), []byte(pw)) == 1 {
				if admin, ok := h.cfg.UserMgr.FindByUsername("admin"); ok {
					loggedUser = admin
				}
			}
			if loggedUser == nil {
				globalAuthLimiter.recordFailedLogin(ip)
				writeJSON(w, http.StatusUnauthorized, map[string]any{"error": err.Error()})
				return
			}
		} else {
			loggedUser = u
		}
	} else {
		// 传统单机模式：核验 WebPassword
		pw := h.GetWebPassword()
		if pw != "" && subtle.ConstantTimeCompare([]byte(req.Password), []byte(pw)) != 1 {
			globalAuthLimiter.recordFailedLogin(ip)
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "控制台访问密码错误"})
			return
		}
		loggedUser = &usermgr.User{
			ID:       "u_admin",
			Username: "admin",
			Role:     usermgr.RoleAdmin,
		}
	}

	if loggedUser == nil {
		if h.cfg.UserMgr != nil {
			if admin, ok := h.cfg.UserMgr.FindByUsername("admin"); ok {
				loggedUser = admin
			}
		}
		if loggedUser == nil {
			loggedUser = &usermgr.User{
				ID:       "u_admin",
				Username: "admin",
				Role:     usermgr.RoleAdmin,
			}
		}
	}

	globalAuthLimiter.resetLogin(ip)

	token := generateSessionToken()
	exp := time.Now().Add(7 * 24 * time.Hour) // 7 天有效期
	webSessions.Store(token, webSessionInfo{
		User:      loggedUser,
		ExpiresAt: exp,
	})

	http.SetCookie(w, &http.Cookie{
		Name:     webSessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  exp,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"user":    loggedUser,
	})
}

func (h *Handler) handleWebRegister(w http.ResponseWriter, r *http.Request) {
	cleanExpiredSessions()

	ip := getClientIP(r)
	if globalAuthLimiter.isRegisterLimited(ip) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": "注册请求过于频繁，请稍后再试",
		})
		return
	}

	if h.cfg.UserMgr == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "未启用用户系统"})
		return
	}

	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}

	u, err := h.cfg.UserMgr.Register(req.Username, req.Password)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	globalAuthLimiter.recordRegister(ip)
	globalAuthLimiter.resetLogin(ip)

	// 注册成功自动创建会话登录
	token := generateSessionToken()
	exp := time.Now().Add(7 * 24 * time.Hour)
	webSessions.Store(token, webSessionInfo{
		User:      u,
		ExpiresAt: exp,
	})

	http.SetCookie(w, &http.Cookie{
		Name:     webSessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  exp,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"user":    u,
	})
}

func (h *Handler) handleWebLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(webSessionCookieName); err == nil && cookie.Value != "" {
		webSessions.Delete(cookie.Value)
	}

	http.SetCookie(w, &http.Cookie{
		Name:     webSessionCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
	})

	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (h *Handler) handleUserAPIKeyReset(w http.ResponseWriter, r *http.Request) {
	user := h.getWebSessionUser(r)
	if user == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "未登录"})
		return
	}

	if h.cfg.UserMgr == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "未启用用户系统"})
		return
	}

	newKey, err := h.cfg.UserMgr.ResetUserAPIKey(user.Username)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	// 如果当前是 admin 用户，同时更新并持久化主全局 APIKey
	if user.Role == usermgr.RoleAdmin || user.Username == "admin" {
		h.SetAPIKey(newKey)
		h.savePersistentSetting("api_key", newKey)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"api_key": newKey,
	})
}

func (h *Handler) handleConfigPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体 JSON 解析失败"})
		return
	}

	newPw := strings.TrimSpace(req.NewPassword)
	// 如果新密码非空，检查长度至少 6 位；如果为空，表示清空密码恢复免密模式
	if newPw != "" && len(newPw) < 6 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "新密码长度至少 6 位（若需清除密码恢复免密请留空）"})
		return
	}

	currentPw := h.GetWebPassword()
	// 如果当前已设置了管理密码，修改或清空时均需核验旧密码
	if currentPw != "" {
		if subtle.ConstantTimeCompare([]byte(req.OldPassword), []byte(currentPw)) != 1 {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "原管理密码不正确"})
			return
		}
	}

	user := h.getWebSessionUser(r)
	if user != nil && h.cfg.UserMgr != nil {
		isAdmin := user.Role == usermgr.RoleAdmin || user.Username == "admin" || user.ID == "u_admin"

		// 普通租户不能清空密码（普通租户必须有密码）
		if newPw == "" && !isAdmin {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "普通租户密码不可为空，长度至少 6 位"})
			return
		}

		// 用户中心修改个人密码（若当前 currentPw != ""，上方已核验原密码，此处 bypassOld 设为 true；若 currentPw == ""，免密首次设密也 bypassOld = true）
		if err := h.cfg.UserMgr.ChangePassword(user.Username, req.OldPassword, newPw, true); err != nil {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": err.Error()})
			return
		}

		// 如果是 admin，同步更新并持久化 web_password
		if isAdmin {
			h.SetWebPassword(newPw)
			h.savePersistentSetting("web_password", newPw)
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"success":      true,
			"has_password": h.GetWebPassword() != "",
		})
		return
	}

	h.SetWebPassword(newPw)
	h.savePersistentSetting("web_password", newPw)

	writeJSON(w, http.StatusOK, map[string]any{
		"success":      true,
		"has_password": newPw != "",
	})
}
