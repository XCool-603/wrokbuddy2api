// Package usermgr 提供轻量级用户管理、角色权限控制、API Key 授权与持久化。
// 存储文件默认保存于持久化数据目录（如 ./data/users.json），支持容器挂载与更新平滑保留。
package usermgr

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

// User 用户数据模型。
type User struct {
	ID           string    `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"password_hash"`
	Salt         string    `json:"salt"`
	Role         string    `json:"role"`    // "admin" | "user"
	APIKey       string    `json:"api_key"` // 用户专属 API Key，格式: sk-wb2a-...
	Disabled     bool      `json:"disabled"`
	CreatedAt    time.Time `json:"created_at"`
}

// Manager 用户管理器。
type Manager struct {
	mu            sync.RWMutex
	filePath      string
	users         map[string]*User // username -> User
	byAPIKey      map[string]*User // api_key -> User
	allowRegister bool             // 是否开放注册（默认 true）
}

type persistedStore struct {
	AllowRegister bool    `json:"allow_register"`
	Users         []*User `json:"users"`
}

// New 初始化或载入用户管理器。
func New(filePath string, defaultAdminPassword, defaultAPIKey string) (*Manager, error) {
	m := &Manager{
		filePath:      filePath,
		users:         make(map[string]*User),
		byAPIKey:      make(map[string]*User),
		allowRegister: true,
	}

	if err := m.load(); err != nil {
		// 如果文件不存在则初始化默认管理员
		if os.IsNotExist(err) {
			m.initDefaultAdmin(defaultAdminPassword, defaultAPIKey)
			if err := m.save(); err != nil {
				return nil, fmt.Errorf("init default admin save: %w", err)
			}
		} else {
			return nil, fmt.Errorf("load users: %w", err)
		}
	} else {
		// 如果加载成功但没有用户（比如空文件），补充默认 admin
		if len(m.users) == 0 {
			m.initDefaultAdmin(defaultAdminPassword, defaultAPIKey)
			_ = m.save()
		}
	}

	return m, nil
}

func (m *Manager) initDefaultAdmin(password, apiKey string) {
	if password == "" {
		password = "admin"
	}
	if apiKey == "" {
		apiKey = GenerateAPIKey()
	}

	salt := generateRandomHex(16)
	admin := &User{
		ID:           "u_admin",
		Username:     "admin",
		PasswordHash: hashPassword(password, salt),
		Salt:         salt,
		Role:         RoleAdmin,
		APIKey:       apiKey,
		Disabled:     false,
		CreatedAt:    time.Now(),
	}
	m.users[admin.Username] = admin
	m.byAPIKey[admin.APIKey] = admin
}

// AllowRegister 获取是否开放注册。
func (m *Manager) AllowRegister() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.allowRegister
}

// SetAllowRegister 设置是否开放注册并持久化。
func (m *Manager) SetAllowRegister(allow bool) error {
	m.mu.Lock()
	m.allowRegister = allow
	m.mu.Unlock()
	return m.save()
}

// Authenticate 用户登录验证，成功返回 User 副本。
func (m *Manager) Authenticate(username, password string) (*User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	u, ok := m.users[strings.ToLower(strings.TrimSpace(username))]
	if !ok {
		return nil, errors.New("用户不存在或密码错误")
	}
	if u.Disabled {
		return nil, errors.New("账号已被管理员禁用")
	}

	expectedHash := hashPassword(password, u.Salt)
	if subtle.ConstantTimeCompare([]byte(u.PasswordHash), []byte(expectedHash)) != 1 {
		return nil, errors.New("用户不存在或密码错误")
	}

	copied := *u
	return &copied, nil
}

// FindByAPIKey 根据 APIKey 查找用户。
func (m *Manager) FindByAPIKey(apiKey string) (*User, bool) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, false
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	u, ok := m.byAPIKey[apiKey]
	if !ok || u.Disabled {
		return nil, false
	}
	copied := *u
	return &copied, true
}

// FindByUsername 根据用户名查找用户。
func (m *Manager) FindByUsername(username string) (*User, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	u, ok := m.users[strings.ToLower(strings.TrimSpace(username))]
	if !ok {
		return nil, false
	}
	copied := *u
	return &copied, true
}

// Register 注册新普通用户。
func (m *Manager) Register(username, password string) (*User, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if len(username) < 3 || len(username) > 32 {
		return nil, errors.New("用户名长度须在 3-32 个字符之间")
	}
	if len(password) < 6 {
		return nil, errors.New("密码长度至少 6 位")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.allowRegister {
		return nil, errors.New("系统当前已关闭自主注册，请联系管理员")
	}

	if _, exists := m.users[username]; exists {
		return nil, errors.New("该用户名已被注册")
	}

	salt := generateRandomHex(16)
	user := &User{
		ID:           "u_" + generateRandomHex(8),
		Username:     username,
		PasswordHash: hashPassword(password, salt),
		Salt:         salt,
		Role:         RoleUser,
		APIKey:       GenerateAPIKey(),
		Disabled:     false,
		CreatedAt:    time.Now(),
	}

	m.users[username] = user
	m.byAPIKey[user.APIKey] = user

	if err := m.saveLocked(); err != nil {
		delete(m.users, username)
		delete(m.byAPIKey, user.APIKey)
		return nil, fmt.Errorf("保存注册数据失败: %w", err)
	}

	copied := *user
	return &copied, nil
}

// CreateUser 由管理员直接创建新用户（不受 allowRegister 限制，可指定角色）。
func (m *Manager) CreateUser(username, password, role string) (*User, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if len(username) < 3 || len(username) > 32 {
		return nil, errors.New("用户名长度须在 3-32 个字符之间")
	}
	if len(password) < 6 {
		return nil, errors.New("密码长度至少 6 位")
	}
	if role != RoleAdmin && role != RoleUser {
		role = RoleUser
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.users[username]; exists {
		return nil, errors.New("该用户名已存在")
	}

	salt := generateRandomHex(16)
	user := &User{
		ID:           "u_" + generateRandomHex(8),
		Username:     username,
		PasswordHash: hashPassword(password, salt),
		Salt:         salt,
		Role:         role,
		APIKey:       GenerateAPIKey(),
		Disabled:     false,
		CreatedAt:    time.Now(),
	}

	m.users[username] = user
	m.byAPIKey[user.APIKey] = user

	if err := m.saveLocked(); err != nil {
		delete(m.users, username)
		delete(m.byAPIKey, user.APIKey)
		return nil, fmt.Errorf("保存新用户失败: %w", err)
	}

	copied := *user
	return &copied, nil
}

// ResetUserAPIKey 重置指定用户的 API Key。
func (m *Manager) ResetUserAPIKey(username string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	u, ok := m.users[strings.ToLower(strings.TrimSpace(username))]
	if !ok {
		return "", errors.New("用户不存在")
	}

	oldKey := u.APIKey
	newKey := GenerateAPIKey()

	delete(m.byAPIKey, oldKey)
	u.APIKey = newKey
	m.byAPIKey[newKey] = u

	if err := m.saveLocked(); err != nil {
		u.APIKey = oldKey
		m.byAPIKey[oldKey] = u
		delete(m.byAPIKey, newKey)
		return "", err
	}

	return newKey, nil
}

// ChangePassword 修改用户密码。
func (m *Manager) ChangePassword(username, oldPassword, newPassword string, bypassOld bool) error {
	if len(newPassword) < 6 {
		return errors.New("新密码长度至少 6 位")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	u, ok := m.users[strings.ToLower(strings.TrimSpace(username))]
	if !ok {
		return errors.New("用户不存在")
	}

	if !bypassOld {
		expectedHash := hashPassword(oldPassword, u.Salt)
		if subtle.ConstantTimeCompare([]byte(u.PasswordHash), []byte(expectedHash)) != 1 {
			return errors.New("原密码错误")
		}
	}

	salt := generateRandomHex(16)
	u.Salt = salt
	u.PasswordHash = hashPassword(newPassword, salt)

	return m.saveLocked()
}

// ToggleUserDisabled 启用/禁用用户（管理员操作）。
func (m *Manager) ToggleUserDisabled(username string, disabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	u, ok := m.users[strings.ToLower(strings.TrimSpace(username))]
	if !ok {
		return errors.New("用户不存在")
	}
	if u.Role == RoleAdmin && disabled {
		return errors.New("不能禁用超级管理员账号")
	}

	u.Disabled = disabled
	return m.saveLocked()
}

// SetUserRole 修改用户角色（管理员操作）。
func (m *Manager) SetUserRole(username, role string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	u, ok := m.users[strings.ToLower(strings.TrimSpace(username))]
	if !ok {
		return errors.New("用户不存在")
	}
	if u.Username == "admin" && role != RoleAdmin {
		return errors.New("默认 admin 用户角色不可更改")
	}

	if role != RoleAdmin && role != RoleUser {
		return errors.New("无效的角色类型")
	}

	u.Role = role
	return m.saveLocked()
}

// DeleteUser 删除用户（管理员操作）。
func (m *Manager) DeleteUser(username string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	username = strings.ToLower(strings.TrimSpace(username))
	u, ok := m.users[username]
	if !ok {
		return errors.New("用户不存在")
	}
	if u.Role == RoleAdmin || u.Username == "admin" {
		return errors.New("不能删除超级管理员账号")
	}

	delete(m.users, username)
	delete(m.byAPIKey, u.APIKey)

	return m.saveLocked()
}

// ListUsers 列出所有用户（管理员操作）。
func (m *Manager) ListUsers() []*User {
	m.mu.RLock()
	defer m.mu.RUnlock()

	list := make([]*User, 0, len(m.users))
	for _, u := range m.users {
		copied := *u
		// 脱敏处理，不传回密码 hash 和 salt
		copied.PasswordHash = ""
		copied.Salt = ""
		list = append(list, &copied)
	}
	return list
}

func (m *Manager) load() error {
	if m.filePath == "" {
		return nil
	}
	data, err := os.ReadFile(m.filePath)
	if err != nil {
		return err
	}

	var store persistedStore
	if err := json.Unmarshal(data, &store); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.allowRegister = store.AllowRegister
	m.users = make(map[string]*User)
	m.byAPIKey = make(map[string]*User)

	for _, u := range store.Users {
		m.users[strings.ToLower(u.Username)] = u
		if u.APIKey != "" {
			m.byAPIKey[u.APIKey] = u
		}
	}
	return nil
}

func (m *Manager) save() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saveLocked()
}

func (m *Manager) saveLocked() error {
	if m.filePath == "" {
		return nil
	}

	usersList := make([]*User, 0, len(m.users))
	for _, u := range m.users {
		usersList = append(usersList, u)
	}

	store := persistedStore{
		AllowRegister: m.allowRegister,
		Users:         usersList,
	}

	raw, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(m.filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	tmpFile := m.filePath + ".tmp"
	if err := os.WriteFile(tmpFile, raw, 0600); err != nil {
		return err
	}
	return os.Rename(tmpFile, m.filePath)
}

func hashPassword(password, salt string) string {
	sum := sha256.Sum256([]byte(password + ":" + salt))
	return hex.EncodeToString(sum[:])
}

func generateRandomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// GenerateAPIKey 生成形如 sk-wb2a-xxxxxxxx... 的高强度 API Key。
func GenerateAPIKey() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return "sk-wb2a-" + hex.EncodeToString(b)
}
