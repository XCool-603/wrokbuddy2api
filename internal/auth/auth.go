// Package auth 解析 WorkBuddy auth 文件（嵌套形/扁平形双形态），
// 提供 refresh 后的原子写回。
package auth

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/logfmt"
)

// Auth 是归一化后的账号凭证（来源可以是插件 OAuth 嵌套形或手写扁平形）。
type Auth struct {
	// mu 串行化 RefreshToken 写与 SaveAtomic 读，防止并发写回半更新 token。
	mu sync.Mutex

	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // Unix 秒
	Domain       string
	// realm 账号域（"cn" / "global"），落盘于 auth.realm（嵌套形）或顶层 realm（扁平形）。
	// 空 = 缺省：Realm() 按 domain 后缀回落，最终恒非空。
	//
	// 命名注记：Go 不允许字段与方法同名，持久化字段用未导出 realm，计算访问器用
	// 导出的 Realm()（跨包调用全部走方法）。Parse/SaveAtomic/login 在包内读写字段。
	realm          string
	UID            string
	EnterpriseID   string
	Nickname       string
	FilePath       string // 来源文件；refresh 后原子写回此处

	// DeviceToken 设备风控 Token（X-Device-Token 头），来源 auth 文件的 device_token 键。
	// 缺省为空 = 不注入该头（容器内无桌面端 Turing SDK 的常见部署）。
	// 手写扁平形 auth 文件可直接写 "device_token": "..."；插件 OAuth 嵌套形
	// 顶层 device_token 也会被解析（与桌面端共用状态文件的部署方式）。
	DeviceToken string

	// Owner 所属用户账号 ID/用户名（多租户/用户角色隔离）。
	// 为空或 "admin" 表示公共/系统账号（管理员或共享池）；若为特定用户（如 "u_xxx" 或 "alice"），
	// 则仅该用户的请求或 API Key 可以调用此账号。
	Owner string
}

// OwnerValue 加锁读取 Owner。
func (a *Auth) OwnerValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Owner
}

// SetOwner 加锁设置 Owner。
func (a *Auth) SetOwner(owner string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Owner = strings.TrimSpace(owner)
}

// Lock 供同进程内其他包（upstream.RefreshToken）在改写 Auth 字段期间加锁。
func (a *Auth) Lock() { a.mu.Lock() }

// Unlock 释放 a.Lock 获取的锁。
func (a *Auth) Unlock() { a.mu.Unlock() }

// AccessTokenValue 加锁读取 AccessToken（出站请求头一律经此取值，勿直读字段）。
//
// 为什么必须加锁：RefreshToken 在 a.mu 内改写 AccessToken/RefreshToken/Domain/ExpiresAt
// （client.go「第 2 段（锁内）：校验快照一致后写回」），而所有出站请求头构造
// （ChatHeaders / BillingHeaders / fetchEnterpriseModels / fetchV3Models /
// global_models）与调度器的 token 检查都在锁外直读这些字段。生产上两侧真会并发：
// Scheduler.RunKeepaliveNow 定时对**每个**非禁用账号刷新（与是否有在途请求无关），
// 而 handler 正基于**同一个** *auth.Auth 指针构造请求头（Pool.AuthByUID/List 返回的
// 就是池内同一个对象）。无同步直读构成数据竞争，go test -race 实证：
//
//	WARNING: DATA RACE
//	Write at ... by goroutine:
//	  (*Client).RefreshToken()  internal/upstream/client.go:929
//	Previous read at ... by goroutine:
//	  (*Client).ChatHeaders()   internal/upstream/headers.go:224
//
// （回归测试 upstream.TestChatHeadersRacesRefreshToken）。
func (a *Auth) AccessTokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.AccessToken
}

// DomainValue 加锁读取 Domain（同 AccessTokenValue：RefreshToken 在锁内改写它）。
func (a *Auth) DomainValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Domain
}

// RefreshTokenValue 加锁读取 RefreshToken（同 AccessTokenValue：RefreshToken 在锁内
// 改写它）。调度器的「有无凭证」前置守卫（checkin/keepalive/travel 的
// `a.RefreshToken == ""`）必须经此取值，勿直读字段。
//
// 与 #125 修的 AccessToken/Domain 属同一类：守卫是纯读、刷新是纯写，二者无同步即
// 构成数据竞争（RefreshToken 写回在 client.go「第 2 段（锁内）」的 `if tok.RefreshToken
// != ""` 分支）。go test -race 实证（回归测试 scheduler.TestKeepaliveGuardRacesRefreshToken）：
//
//	WARNING: DATA RACE
//	Read at ... by goroutine:
//	  (*Scheduler).RunKeepaliveNow()  internal/scheduler/scheduler.go:654
//	Previous write at ... by goroutine:
//	  (*Client).RefreshToken()        internal/upstream/client.go:931
func (a *Auth) RefreshTokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.RefreshToken
}

// globalEnabled 全局开关：global realm 是否路由（D5 双保险）。
// 默认开启（与 config global.enabled 缺省 true 一致）：Realm() 正常按显式 realm/
// domain 判定 global/cn。显式 SetGlobalEnabled(false)（config "enabled": false）关闭
// → 逃生门：纯 CN 部署，即便 auth 文件写了 realm=global 或 domain 为 .workbuddy.ai
// 也恒判 cn——「关了才锁死」的单一闸口集中收敛在 Realm()/IsGlobal() 里。
var globalEnabled atomic.Bool

func init() { globalEnabled.Store(true) }

// SetGlobalEnabled 注入 global realm 路由开关（false = 锁死纯 CN，逃生门）。
func SetGlobalEnabled(enabled bool) { globalEnabled.Store(enabled) }

// Realm 返回账号的归一化域：显式 Realm=="global" 或 domain 后缀 .workbuddy.ai → "global"，
// 否则 "cn"。显式 global 优先于 domain 回落（D1）。
// 全局开关 SetGlobalEnabled(false) 时恒 "cn"（逃生门：纯 CN 锁定，不影响默认行为）。
// 空 realm + 空 domain → "cn"（老 CN 凭证零回归）。
func (a *Auth) Realm() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.realmLocked()
}

// realmLocked Realm 的无锁内部实现：仅限**已持 a.mu** 的调用方使用（sync.Mutex 不可重入，
// 锁内再调 Realm() 会自锁）。realm 由 BackfillRealm 改写、Domain 由 RefreshToken 在锁内
// 改写，故读取必须与写方同锁（理由见 AccessTokenValue 注释）。
func (a *Auth) realmLocked() string {
	if !globalEnabled.Load() {
		return "cn"
	}
	if strings.TrimSpace(a.realm) == "global" || isGlobalDomain(a.Domain) {
		return "global"
	}
	return "cn"
}

// ResolveRealm 归一化 realm（cn/global）：显式非空优先，否则按原始 domain 推断
// （isGlobalDomain）。不受逃生门影响（逃生门是路由锁，不应影响标识判定）；
// domain 也为空 → "cn"（老 CN 凭证零回归）。
func ResolveRealm(explicit, domain string) string {
	if r := strings.TrimSpace(explicit); r != "" {
		return r
	}
	if isGlobalDomain(domain) {
		return "global"
	}
	return "cn"
}

// BackfillRealm 为缺省 realm 标识的账号持久化补标识：a.realm 为空时按「原始 domain 推断」
// 写回（cn/global），返回 (是否有变更, 归一化后的 realm)。已有标识不动（幂等）。
//
// 注意用 isGlobalDomain(a.Domain) 直接推断，而非 Realm()——Realm() 在逃生门
// （SetGlobalEnabled(false)）下恒降级 cn，把 global 账号写死成 cn 会永久污染凭证
// （逃生门是纯 CN 部署的临时锁，不应改写落盘数据）。domain 也为空时写 "cn"（老 CN 凭证）。
func (a *Auth) BackfillRealm() (bool, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.realm) != "" {
		return false, a.realm
	}
	r := ResolveRealm("", a.Domain)
	a.realm = r
	return true, r
}

// RealmStored 直读持久化的 realm 标识（可能为空 = 未 backfill 的旧文件，Realm() 会 fallback）。
func (a *Auth) RealmStored() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.realm
}

// IsGlobal 报告账号是否属于 global realm（= Realm() == "global"）。
func (a *Auth) IsGlobal() bool { return a.Realm() == "global" }

// isGlobalDomain 判定 domain 是否指向 www.workbuddy.ai 家族。
// 同时接受裸域 workbuddy.ai 与任意子域（HasSuffix("www.workbuddy.ai") 或裸域本身）。
func isGlobalDomain(d string) bool {
	d = strings.ToLower(strings.TrimSpace(d))
	return d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai")
}

// NeedsRefresh 报告 token 是否将在 within 内过期（或已过期/无 expiry）。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// CleanRawInput 清理 UTF-8 BOM、Markdown 代码块包裹、全角中文引号及 Windows 换行符。
func CleanRawInput(raw []byte) []byte {
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")) // UTF-8 BOM
	raw = bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	raw = bytes.ReplaceAll(raw, []byte("\r"), []byte("\n"))
	raw = bytes.TrimSpace(raw)
	// Markdown 代码块清理: ```json ... ``` 或 ``` ... ```
	if bytes.HasPrefix(raw, []byte("```")) {
		if idx := bytes.IndexByte(raw, '\n'); idx != -1 {
			raw = raw[idx+1:]
		}
		if idx := bytes.LastIndex(raw, []byte("```")); idx != -1 {
			raw = raw[:idx]
		}
		raw = bytes.TrimSpace(raw)
	}
	// 中文全角引号转义
	raw = bytes.ReplaceAll(raw, []byte("“"), []byte("\""))
	raw = bytes.ReplaceAll(raw, []byte("”"), []byte("\""))
	raw = bytes.ReplaceAll(raw, []byte("‘"), []byte("'"))
	raw = bytes.ReplaceAll(raw, []byte("’"), []byte("'"))
	return raw
}

// SanitizeFilename 清理 UID/文件名中的非法字符与路径遍历，防止在 Linux/Windows 或 Docker 挂载目录下落盘失败。
func SanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "account"
	}
	// 过滤路径遍历
	name = filepath.Base(name)
	name = strings.TrimPrefix(name, "..")
	// 替换 Windows/Linux 文件系统禁用字符: / \ : * ? " < > | 以及控制字符
	var sb strings.Builder
	for _, r := range name {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', '\r', '\n', '\t', 0:
			sb.WriteRune('_')
		default:
			sb.WriteRune(r)
		}
	}
	res := strings.TrimSpace(sb.String())
	res = strings.Trim(res, "._ ")
	if res == "" {
		return "account"
	}
	return res
}

// SetRealm 加锁设置账号域（"cn" 或 "global"）。
func (a *Auth) SetRealm(realm string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.realm = strings.TrimSpace(realm)
}

// ParseJWTClaims 解析并提取 JWT Payload 中的 claims 字典。
func ParseJWTClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	payload := parts[1]
	b, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		b, err = base64.URLEncoding.DecodeString(payload)
		if err != nil {
			return nil
		}
	}
	var claims map[string]any
	_ = json.Unmarshal(b, &claims)
	return claims
}

func parseJWTClaims(token string) map[string]any {
	return ParseJWTClaims(token)
}

func tryParseRawToken(str string) *Auth {
	str = strings.TrimSpace(str)
	if str == "" {
		return nil
	}
	str = strings.TrimPrefix(str, "Bearer ")
	str = strings.TrimPrefix(str, "bearer ")

	var at, rt, explicitRealm, explicitUID, explicitNick string

	// 支持 key-value 格式：例如 access_token=xxx 或 accessToken: xxx
	if strings.Contains(str, "\n") || strings.Contains(str, "=") || (strings.Contains(str, ":") && !strings.HasPrefix(str, "http")) {
		lines := strings.Split(str, "\n")
		var kvAt, kvRt string
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
				continue
			}
			var k, v string
			if strings.Contains(line, "=") {
				parts := strings.SplitN(line, "=", 2)
				k = strings.ToLower(strings.TrimSpace(parts[0]))
				v = strings.TrimSpace(parts[1])
			} else if strings.Contains(line, ":") {
				parts := strings.SplitN(line, ":", 2)
				k = strings.ToLower(strings.TrimSpace(parts[0]))
				v = strings.TrimSpace(parts[1])
			}
			v = strings.Trim(v, `"'`)
			switch k {
			case "access_token", "accesstoken", "token", "jwt", "apikey":
				kvAt = v
			case "refresh_token", "refreshtoken", "refresh":
				kvRt = v
			case "realm":
				explicitRealm = v
			case "uid", "user_id", "userid", "id":
				explicitUID = v
			case "nickname", "nick", "name":
				explicitNick = v
			}
		}
		if kvAt != "" {
			at = kvAt
			if kvRt != "" {
				rt = kvRt
			}
		}
	}

	if at == "" {
		// 支持 token----refreshToken, token---refreshToken, token|refreshToken, token\trefreshToken, token,refreshToken, token#refreshToken
		for _, sep := range []string{"----", "---", "|", "\t", "#", ","} {
			if strings.Contains(str, sep) {
				parts := strings.SplitN(str, sep, 2)
				p0 := strings.TrimSpace(parts[0])
				p1 := strings.TrimSpace(parts[1])
				if p0 != "" && p1 != "" {
					at = p0
					rt = p1
					break
				}
			}
		}
	}

	if at == "" {
		at = str
	}

	if len(at) < 10 {
		return nil
	}

	a := &Auth{
		AccessToken:  at,
		RefreshToken: rt,
		ExpiresAt:    time.Now().Add(30 * 24 * time.Hour).Unix(),
		realm:        explicitRealm,
		UID:          explicitUID,
		Nickname:     explicitNick,
	}

	// 尝试解构 JWT Payload
	claims := ParseJWTClaims(at)
	if claims != nil {
		if a.UID == "" {
			a.UID = extractValString(claims, "sub", "uid", "id", "user_id", "userId", "account_id")
		}
		if exp, ok := claims["exp"].(float64); ok && exp > 0 {
			a.ExpiresAt = int64(exp)
		}
		if a.Nickname == "" {
			a.Nickname = extractValString(claims, "name", "nickname", "nick_name", "username")
		}
		if iss, ok := claims["iss"].(string); ok && iss != "" {
			if strings.Contains(iss, "workbuddy.ai") {
				a.Domain = iss
				if a.realm == "" {
					a.realm = "global"
				}
			}
		}
	}

	if a.UID == "" {
		a.UID = fmt.Sprintf("token_%d", time.Now().UnixNano()%100000000)
	}

	return a
}

func extractValString(m map[string]any, keys ...string) string {
	if m == nil {
		return ""
	}
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			switch val := v.(type) {
			case string:
				if s := strings.TrimSpace(val); s != "" {
					return s
				}
			case json.Number:
				return val.String()
			case float64:
				return fmt.Sprintf("%.0f", val)
			case int:
				return strconv.Itoa(val)
			case int64:
				return strconv.FormatInt(val, 10)
			}
		}
	}
	return ""
}

func extractValInt64(m map[string]any, keys ...string) int64 {
	if m == nil {
		return 0
	}
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			switch val := v.(type) {
			case json.Number:
				if n, err := val.Int64(); err == nil {
					return n
				}
				if f, err := val.Float64(); err == nil {
					return int64(f)
				}
			case float64:
				return int64(val)
			case int64:
				return val
			case int:
				return int64(val)
			case string:
				s := strings.TrimSpace(val)
				if n, err := strconv.ParseInt(s, 10, 64); err == nil {
					return n
				}
			}
		}
	}
	return 0
}

// Parse 兼容多种凭证输入格式：
// 1. 嵌套形 {"auth":{...},"account":{...}}  （插件 OAuth 输出）
// 2. 扁平形 {"accessToken":...,"uid":...}   （手写/标准导出）
// 3. 原始 Token 字符串（JWT 或 token----refreshToken 组合）
// 4. 数字/字符串兼容类型容错与 Markdown/中文符号净化
func Parse(raw []byte) (*Auth, error) {
	raw = CleanRawInput(raw)
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}

	// 1. 如果不是以 '{' 开头，尝试作为原始 Token / 组合文本解析
	if !bytes.HasPrefix(raw, []byte("{")) {
		if a := tryParseRawToken(string(raw)); a != nil {
			return a, nil
		}
		return nil, fmt.Errorf("storage_parse_error: invalid character '%c' looking for beginning of JSON object", raw[0])
	}

	// 2. 作为 JSON 解析，采用 UseNumber 避免整型数字 ID/时间戳反序列化类型失败
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}

	var authMap, accountMap, dataMap map[string]any
	if sub, ok := m["auth"].(map[string]any); ok {
		authMap = sub
	}
	if sub, ok := m["account"].(map[string]any); ok {
		accountMap = sub
	}
	// 支持上游 API 响应包裹格式：例如 {"code": 0, "data": {"accessToken": "..."}}
	for _, k := range []string{"data", "result", "response", "token_info", "credentials"} {
		if sub, ok := m[k].(map[string]any); ok {
			dataMap = sub
			break
		}
	}

	at := extractValString(authMap, "accessToken", "access_token", "token", "jwt", "bearer_token", "apiKey", "auth_token")
	if at == "" {
		at = extractValString(dataMap, "accessToken", "access_token", "token", "jwt", "bearer_token", "apiKey", "auth_token")
	}
	if at == "" {
		at = extractValString(m, "accessToken", "access_token", "token", "jwt", "bearer_token", "apiKey", "auth_token")
	}

	rt := extractValString(authMap, "refreshToken", "refresh_token", "refresh")
	if rt == "" {
		rt = extractValString(dataMap, "refreshToken", "refresh_token", "refresh")
	}
	if rt == "" {
		rt = extractValString(m, "refreshToken", "refresh_token", "refresh")
	}

	expAt := extractValInt64(authMap, "expiresAt", "expires_at", "expire_at", "exp")
	if expAt == 0 {
		expAt = extractValInt64(dataMap, "expiresAt", "expires_at", "expire_at", "exp")
	}
	if expAt == 0 {
		expAt = extractValInt64(m, "expiresAt", "expires_at", "expire_at", "exp")
	}
	if expAt == 0 {
		expIn := extractValInt64(authMap, "expiresIn", "expires_in", "expire_in")
		if expIn == 0 {
			expIn = extractValInt64(dataMap, "expiresIn", "expires_in", "expire_in")
		}
		if expIn == 0 {
			expIn = extractValInt64(m, "expiresIn", "expires_in", "expire_in")
		}
		if expIn > 0 {
			expAt = time.Now().Unix() + expIn
		}
	}

	domain := extractValString(authMap, "domain", "api_domain", "host")
	if domain == "" {
		domain = extractValString(dataMap, "domain", "api_domain", "host")
	}
	if domain == "" {
		domain = extractValString(m, "domain", "api_domain", "host")
	}

	realm := extractValString(authMap, "realm")
	if realm == "" {
		realm = extractValString(dataMap, "realm")
	}
	if realm == "" {
		realm = extractValString(m, "realm")
	}

	uid := extractValString(accountMap, "uid", "user_id", "userId", "id", "sub", "account_id")
	if uid == "" {
		uid = extractValString(dataMap, "uid", "user_id", "userId", "id", "sub", "account_id")
	}
	if uid == "" {
		uid = extractValString(m, "uid", "user_id", "userId", "id", "sub", "account_id")
	}

	ent := extractValString(accountMap, "enterpriseId", "enterprise_id", "enterpriseID")
	if ent == "" {
		ent = extractValString(dataMap, "enterpriseId", "enterprise_id", "enterpriseID")
	}
	if ent == "" {
		ent = extractValString(m, "enterpriseId", "enterprise_id", "enterpriseID")
	}

	nick := extractValString(accountMap, "nickname", "nick_name", "name", "username", "email")
	if nick == "" {
		nick = extractValString(dataMap, "nickname", "nick_name", "name", "username", "email")
	}
	if nick == "" {
		nick = extractValString(m, "nickname", "nick_name", "name", "username", "email")
	}

	deviceToken := extractValString(m, "device_token", "deviceToken")
	if deviceToken == "" {
		deviceToken = extractValString(authMap, "device_token", "deviceToken")
	}
	if deviceToken == "" {
		deviceToken = extractValString(dataMap, "device_token", "deviceToken")
	}

	owner := extractValString(m, "owner")
	if owner == "" {
		owner = extractValString(accountMap, "owner")
	}

	// 如果 accessToken 为空但 refreshToken 存在，允许自动占位以供上游调度器刷新
	if strings.TrimSpace(at) == "" && strings.TrimSpace(rt) != "" {
		at = "pending_refresh"
		if expAt == 0 {
			expAt = 1 // 立即触发刷新
		}
	}

	if strings.TrimSpace(at) == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}

	// 若未指定 expAt 或 uid，尝试从 JWT claims 中解析补充
	if claims := parseJWTClaims(at); claims != nil {
		if expAt == 0 {
			if exp, ok := claims["exp"].(float64); ok && exp > 0 {
				expAt = int64(exp)
			}
		}
		if uid == "" {
			uid = extractValString(claims, "sub", "uid", "id", "user_id", "userId", "account_id")
		}
		if nick == "" {
			nick = extractValString(claims, "name", "nickname", "nick_name", "username")
		}
	}

	a := &Auth{
		AccessToken:  at,
		RefreshToken: rt,
		ExpiresAt:    expAt,
		Domain:       domain,
		realm:        realm,
		UID:          uid,
		EnterpriseID: ent,
		Nickname:     nick,
		DeviceToken:  deviceToken,
		Owner:        owner,
	}

	return a, nil
}

// SaveAtomic 以嵌套形原子写回 FilePath（tmp + rename），保持嵌套形（插件可读）格式。
// 全程持 a.mu：防止与 RefreshToken 修改 token 字段并发，杜绝写回半更新。
// 防御：accessToken 与 refreshToken 均为空时拒绝写回，避免误用空凭证覆盖有效文件。
func (a *Auth) SaveAtomic() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.AccessToken) == "" && strings.TrimSpace(a.RefreshToken) == "" {
		return fmt.Errorf("save refused: empty accessToken and refreshToken (uid=%s)", a.UID)
	}
	if a.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  a.AccessToken,
			"refreshToken": a.RefreshToken,
			"expiresAt":    a.ExpiresAt,
			"domain":       a.Domain,
			"realm":        a.realm,
		},
		"account": map[string]any{
			"uid":          a.UID,
			"enterpriseId": a.EnterpriseID,
			"nickname":     a.Nickname,
		},
	}
	// DeviceToken 非空才写回顶层 device_token：避免在无该字段的旧文件里引入空键
	// （保持与插件 OAuth 输出形状一致，插件读取忽略未知键）。
	if a.DeviceToken != "" {
		doc["device_token"] = a.DeviceToken
	}
	if a.Owner != "" {
		doc["owner"] = a.Owner
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.FilePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		// 如果写 tmp 失败（例如跨文件系统或临时受限），回退直接写入目标文件
		if writeErr := os.WriteFile(a.FilePath, raw, 0o644); writeErr != nil {
			return fmt.Errorf("write tmp failed: %w (fallback direct write: %v)", err, writeErr)
		}
		return nil
	}
	defer func() { _ = os.Remove(tmp) }()
	if err := os.Rename(tmp, a.FilePath); err != nil {
		_ = os.Remove(a.FilePath)
		if err2 := os.Rename(tmp, a.FilePath); err2 != nil {
			// 在 Docker 挂载卷或 Windows/WSL2 上 rename 可能受限，回退为直接写入目标文件
			if writeErr := os.WriteFile(a.FilePath, raw, 0o644); writeErr != nil {
				return fmt.Errorf("rename failed (%v) and fallback direct write failed: %w", err, writeErr)
			}
		}
	}
	return nil
}

// AuthFileGlob auth 文件的统一 glob 模式（宽侧：workbuddy*.json）。
// 网关 LoadDir 与 cmd 运维工具（signin/credit/trial）共用此单一来源——
// 此前 cmd 侧私用 workbuddy-*.json 窄模式，不带连字符的文件（如
// workbuddy_new.json）被网关加载却被运维工具跳过，排障口径对不上
// （审查发现 10）。
const AuthFileGlob = "workbuddy*.json"

// LoadAuthFiles 返回 dir 下按 AuthFileGlob 匹配的 auth 文件清单（已排序）。
// 若存在其他非系统配置类的 *.json 文件，只要能解析为有效凭证也会被自动兼容发现。
func LoadAuthFiles(dir string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(dir, AuthFileGlob))
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(files))
	for _, f := range files {
		seen[filepath.Clean(f)] = true
	}

	// 额外宽容匹配：扫描 dir/*.json，兼容用户手动放置的未带 workbuddy 前缀的授权凭证文件
	allJSON, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	for _, f := range allJSON {
		clean := filepath.Clean(f)
		if seen[clean] {
			continue
		}
		base := strings.ToLower(filepath.Base(clean))
		// 忽略常见系统配置与状态文件，避免误判
		if base == "state.json" || base == "users.json" || base == "settings.json" || base == "config.json" || base == "metrics.json" {
			continue
		}
		if raw, err := os.ReadFile(clean); err == nil && len(raw) > 0 {
			if a, parseErr := Parse(raw); parseErr == nil && a != nil && a.AccessToken != "" {
				files = append(files, f)
				seen[clean] = true
			}
		}
	}

	sort.Strings(files)
	return files, nil
}

// LoadDir 扫描并解析 dir 下 workbuddy*.json；解析失败的文件静默跳过（启动日志由调用方统计）。
// 顺带做 realm 标识存量迁移：对空 realm 的 auth 自动 backfill（原始 domain 推断）并 SaveAtomic
// 落盘，一次性把旧文件补上 realm 键。单个文件写失败不阻断启动（log WARN 继续），
// 避免历史 auth 目录个别文件不可写时整个服务起不来。
func LoadDir(dir string) ([]*Auth, error) {
	files, err := LoadAuthFiles(dir)
	if err != nil {
		return nil, err
	}
	// seenUID 重复 UID 检测：同 UID 出现在多个文件时（双 realm 同名 UID 概率近零）
	// 打 WARN 告警含两文件路径，由「后载入者胜出」保持现状行为（不改变加载结果）。
	seenUID := make(map[string]string, len(files))
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			continue
		}
		a.FilePath = f
		if a.UID == "" {
			base := strings.TrimSuffix(filepath.Base(f), filepath.Ext(f))
			base = strings.TrimPrefix(base, "workbuddy-")
			base = strings.TrimPrefix(base, "workbuddy_")
			if base != "" {
				a.UID = base
			}
		}
		if prev, ok := seenUID[a.UID]; ok {
			log.Printf("WARN: uid %s duplicated across %s and %s — 后者覆盖（不同 realm 同名 UID？）",
				logfmt.Label(a.UID, a.Nickname), prev, f)
		}
		seenUID[a.UID] = f
		if a.RealmStored() == "" {
			if changed, r := a.BackfillRealm(); changed {
				if err := a.SaveAtomic(); err != nil {
					log.Printf("WARN: auth %s realm backfill save: %v", logfmt.Label(a.UID, a.Nickname), err)
				} else if r == "global" {
					log.Printf("auth %s 存量迁移: 补 realm=global（domain=%s）", logfmt.Label(a.UID, a.Nickname), a.Domain)
				}
			}
		}
		out = append(out, a)
	}
	return out, nil
}
