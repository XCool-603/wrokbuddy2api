package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/usermgr"
)

type oauthSession struct {
	Realm     string
	CreatedAt time.Time
}

var (
	oauthSessionMu   sync.Mutex
	oauthSessions    = make(map[string]oauthSession) // state -> session
	oauthLatestState = make(map[string]string)       // realm -> latest state (for fallback)
)

func oauthConfig(realm string) (base, origin string) {
	if realm == "global" {
		return "https://www.workbuddy.ai", "https://www.workbuddy.ai"
	}
	return "https://copilot.tencent.com", "https://www.codebuddy.cn"
}

func oauthHeaders(origin string) func(*http.Request) {
	return func(req *http.Request) {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("Origin", origin)
		req.Header.Set("Referer", origin+"/")
		req.Header.Set("User-Agent", "CLI/2.63.2 CodeBuddy/2.63.2")
	}
}

type oauthEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

func oauthDoJSON(client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	if headers != nil {
		headers(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	var env oauthEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, resp.StatusCode, nil
}

func (h *Handler) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Realm string `json:"realm"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Realm != "global" {
		req.Realm = "cn"
	}

	log.Printf("INFO: [oauth] starting OAuth authorization for realm=%s", req.Realm)
	base, origin := oauthConfig(req.Realm)
	client := &http.Client{Timeout: 15 * time.Second}
	headers := oauthHeaders(origin)
	data, _, err := oauthDoJSON(client, http.MethodPost, base+"/v2/plugin/auth/state?platform=CLI", headers, bytes.NewReader([]byte("{}")))
	if err != nil {
		log.Printf("ERR: [oauth] get auth state failed: realm=%s err=%v", req.Realm, err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": fmt.Sprintf("获取授权状态失败: %v", err),
		})
		return
	}

	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		log.Printf("ERR: [oauth] unmarshal auth state failed: data=%s err=%v", string(data), err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "解析上游授权状态失败: 返回数据不完整",
		})
		return
	}

	oauthSessionMu.Lock()
	oauthSessions[st.State] = oauthSession{Realm: req.Realm, CreatedAt: time.Now()}
	oauthLatestState[req.Realm] = st.State
	// 定期清理 >15 分钟过期的老 session
	now := time.Now()
	for s, sess := range oauthSessions {
		if now.Sub(sess.CreatedAt) > 15*time.Minute {
			delete(oauthSessions, s)
		}
	}
	oauthSessionMu.Unlock()

	log.Printf("INFO: [oauth] state generated: realm=%s state=%s url=%s", req.Realm, st.State, st.AuthURL)
	writeJSON(w, http.StatusOK, map[string]any{
		"url":   strings.TrimSpace(st.AuthURL),
		"realm": req.Realm,
		"state": st.State,
	})
}

func (h *Handler) handleOAuthPoll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Realm string `json:"realm"`
		State string `json:"state"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Realm != "global" {
		req.Realm = "cn"
	}

	state := strings.TrimSpace(req.State)
	oauthSessionMu.Lock()
	if state == "" {
		state = oauthLatestState[req.Realm]
	}
	if sess, ok := oauthSessions[state]; ok && sess.Realm != "" {
		req.Realm = sess.Realm
	}
	oauthSessionMu.Unlock()

	if state == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "无正在进行的登录请求，请先点击发起登录",
		})
		return
	}

	base, origin := oauthConfig(req.Realm)
	client := &http.Client{Timeout: 10 * time.Second}
	headers := oauthHeaders(origin)

	tokRaw, status, errTok := oauthDoJSON(client, http.MethodGet, base+"/v2/plugin/auth/token?state="+state, headers, nil)
	if errTok != nil {
		// 网络短暂超时或上游瞬时 5xx 网关重试状态：视为就绪等待中，绝不向前端硬报 500 导致轮询中断
		if status == 0 || status >= 500 {
			writeJSON(w, http.StatusOK, map[string]any{
				"success": false,
				"waiting": true,
				"notice":  "正在等待上游响应...",
			})
			return
		}
		// 尚在等待用户在浏览器中授权完成
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false,
			"waiting": true,
		})
		return
	}

	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(tokRaw, &tok); err != nil || strings.TrimSpace(tok.AccessToken) == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false,
			"waiting": true,
		})
		return
	}

	// 保证 domain 非空，以确保正确的双域路由
	if tok.Domain == "" {
		if req.Realm == "global" {
			tok.Domain = "www.workbuddy.ai"
		} else {
			tok.Domain = "copilot.tencent.com"
		}
	}

	// 成功获取到 token，接着获取账号信息
	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
		Provider     string `json:"provider"`
		IDP          string `json:"idp"`
		LoginType    string `json:"loginType"`
		Type         string `json:"type"`
	}
	acctHeaders := func(req *http.Request) {
		headers(req)
		req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	}
	if acctRaw, _, errAcct := oauthDoJSON(client, http.MethodGet, base+"/v2/plugin/login/account?state="+state, acctHeaders, nil); errAcct == nil {
		_ = json.Unmarshal(acctRaw, &acct)
	}

	uid := strings.TrimSpace(acct.UID)
	nickname := strings.TrimSpace(acct.Nickname)
	entID := strings.TrimSpace(acct.EnterpriseID)
	provider := auth.NormalizeProvider(acct.Provider)
	if provider == "" {
		provider = auth.NormalizeProvider(acct.IDP)
	}
	if provider == "" {
		provider = auth.NormalizeProvider(acct.LoginType)
	}
	if provider == "" {
		provider = auth.NormalizeProvider(acct.Type)
	}

	// 若未从 account 接口获取到 uid/nickname/provider，尝试从 JWT claims 中解析补充
	if claims := auth.ParseJWTClaims(tok.AccessToken); claims != nil {
		if uid == "" {
			if sub, ok := claims["sub"].(string); ok && sub != "" {
				uid = sub
			} else if u, ok := claims["uid"].(string); ok && u != "" {
				uid = u
			}
		}
		if nickname == "" {
			if name, ok := claims["name"].(string); ok && name != "" {
				nickname = name
			} else if nick, ok := claims["nickname"].(string); ok && nick != "" {
				nickname = nick
			}
		}
		if provider == "" {
			for _, k := range []string{"identity_provider", "idp", "federated_identity_provider", "auth_provider", "provider"} {
				if p, ok := claims[k].(string); ok && p != "" {
					provider = auth.NormalizeProvider(p)
					break
				}
			}
		}
	}

	if uid == "" {
		uid = fmt.Sprintf("account_%d", time.Now().Unix())
	}

	expiresAt := time.Now().Unix() + 7200
	if tok.ExpiresIn > 0 {
		expiresAt = time.Now().Unix() + tok.ExpiresIn
	}

	user := h.getWebSessionUser(r)
	ownerID := "public"
	if user != nil && user.Role != usermgr.RoleAdmin && user.Username != "admin" && user.ID != "u_admin" && user.ID != "admin" {
		ownerID = user.ID
	}

	docAccount := map[string]any{
		"uid":          uid,
		"enterpriseId": entID,
		"nickname":     nickname,
	}
	if provider != "" {
		docAccount["provider"] = provider
	}
	docAuth := map[string]any{
		"accessToken":  tok.AccessToken,
		"refreshToken": tok.RefreshToken,
		"expiresAt":    expiresAt,
		"domain":       tok.Domain,
		"realm":        req.Realm,
	}
	if provider != "" {
		docAuth["provider"] = provider
	}

	doc := map[string]any{
		"account": docAccount,
		"auth":    docAuth,
		"owner":   ownerID,
	}
	docBytes, _ := json.MarshalIndent(doc, "", "  ")

	authDir := h.getAuthDir()
	_ = os.MkdirAll(authDir, 0755)
	targetFile := resolveAuthFilePath(authDir, req.Realm, uid, provider, tok.AccessToken, tok.RefreshToken)
	if err := os.WriteFile(targetFile, docBytes, 0644); err != nil {
		log.Printf("WARN: [oauth] 保存授权文件 %s 异常: %v（仍将其载入内存账号池）", targetFile, err)
	}

	// 立即将新账号同步入账号池，清除历史冷却与禁用状态
	if newAuth, err := auth.Parse(docBytes); err == nil {
		newAuth.FilePath = targetFile
		if provider != "" && newAuth.Provider() == "" {
			newAuth.SetProvider(provider)
		}
		if h.cfg.Pool != nil {
			h.cfg.Pool.Add(newAuth)
			h.cfg.Pool.ReviveDisabled(newAuth.UID)
			h.cfg.Pool.ClearCooling(newAuth.UID)
			// 异步回填积分
			if h.cfg.Upstream != nil {
				go func(a *auth.Auth, u string) {
					if rem, _, _, _, e := h.cfg.Upstream.ResourceSummary(a); e == nil {
						h.cfg.Pool.SetCredits(u, rem)
					}
				}(newAuth, newAuth.UID)
			}
		}
	}
	log.Printf("INFO: [oauth] successfully authenticated account: uid=%s, nickname=%s, provider=%s, realm=%s, owner=%s, file=%s",
		uid, nickname, provider, req.Realm, ownerID, filepath.Base(targetFile))

	// 清理当前状态
	oauthSessionMu.Lock()
	delete(oauthSessions, state)
	if oauthLatestState[req.Realm] == state {
		delete(oauthLatestState, req.Realm)
	}
	oauthSessionMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"success":  true,
		"uid":      uid,
		"filename": filepath.Base(targetFile),
		"nickname": nickname,
		"realm":    req.Realm,
		"provider": provider,
	})
}
