package usermgr

import (
	"path/filepath"
	"testing"
)

func TestUserManager(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "users.json")

	// 1. 初始化
	mgr, err := New(dbPath, "admin_pwd_123", "sk-custom-admin")
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	// 2. 检查默认 admin
	admin, err := mgr.Authenticate("admin", "admin_pwd_123")
	if err != nil {
		t.Fatalf("Admin auth failed: %v", err)
	}
	if admin.Role != RoleAdmin || admin.APIKey != "sk-custom-admin" {
		t.Fatalf("Admin properties mismatch: %+v", admin)
	}

	// 验证 APIKey 查找
	uByKey, ok := mgr.FindByAPIKey("sk-custom-admin")
	if !ok || uByKey.Username != "admin" {
		t.Fatalf("FindByAPIKey failed")
	}

	// 3. 用户自主注册
	user1, err := mgr.Register("alice", "alice123456")
	if err != nil {
		t.Fatalf("Register alice failed: %v", err)
	}
	if user1.Role != RoleUser || user1.APIKey == "" {
		t.Fatalf("Alice properties mismatch: %+v", user1)
	}

	// 登录 alice
	authAlice, err := mgr.Authenticate("alice", "alice123456")
	if err != nil {
		t.Fatalf("Auth alice failed: %v", err)
	}
	if authAlice.ID != user1.ID {
		t.Fatalf("Alice ID mismatch")
	}

	// 4. 重置 API Key
	newKey, err := mgr.ResetUserAPIKey("alice")
	if err != nil {
		t.Fatalf("Reset key failed: %v", err)
	}
	if newKey == user1.APIKey {
		t.Fatalf("New key should differ from old key")
	}
	if _, ok := mgr.FindByAPIKey(user1.APIKey); ok {
		t.Fatalf("Old key should no longer work")
	}
	if _, ok := mgr.FindByAPIKey(newKey); !ok {
		t.Fatalf("New key should work")
	}

	// 5. 禁用用户
	if err := mgr.ToggleUserDisabled("alice", true); err != nil {
		t.Fatalf("Toggle disable failed: %v", err)
	}
	if _, err := mgr.Authenticate("alice", "alice123456"); err == nil {
		t.Fatalf("Disabled user should not authenticate")
	}
	if _, ok := mgr.FindByAPIKey(newKey); ok {
		t.Fatalf("Disabled user's API Key should not work")
	}

	// 重新启用
	if err := mgr.ToggleUserDisabled("alice", false); err != nil {
		t.Fatalf("Toggle enable failed: %v", err)
	}
	if _, err := mgr.Authenticate("alice", "alice123456"); err != nil {
		t.Fatalf("Enabled user should authenticate")
	}

	// 6. 测试持久化重新加载
	mgr2, err := New(dbPath, "other_pass", "other_key")
	if err != nil {
		t.Fatalf("Reload failed: %v", err)
	}
	if _, err := mgr2.Authenticate("admin", "admin_pwd_123"); err != nil {
		t.Fatalf("Reloaded admin auth failed: %v", err)
	}
	if _, err := mgr2.Authenticate("alice", "alice123456"); err != nil {
		t.Fatalf("Reloaded alice auth failed: %v", err)
	}
	if _, ok := mgr2.FindByAPIKey(newKey); !ok {
		t.Fatalf("Reloaded newKey should work")
	}
}
