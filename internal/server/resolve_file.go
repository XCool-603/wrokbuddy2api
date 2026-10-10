package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
)

// resolveAuthFilePath 根据 realm、uid、provider 以及现有凭证文件，计算或查找合适且不发生覆盖的文件路径。
// 规则：
// 1. 若磁盘上已有相同 refresh_token 或 access_token 的凭证文件，返回该文件路径以就地更新（刷新凭证幂等）；
// 2. 若目标文件名已存在且包含不同 token（例如相同邮箱的 Google 与 Twitter 账号），则分配带有 provider 或递增序号的独立文件名；
// 3. 绝不覆盖属于其他有效账号的文件。
func resolveAuthFilePath(authDir, realm, uid, provider, accessToken, refreshToken string) string {
	safeUID := auth.SanitizeFilename(uid)
	provider = auth.NormalizeProvider(provider)
	if realm == "" {
		realm = "cn"
	}

	// 1. 扫描现有凭证文件：检查是否已有相同 token 的文件（就地更新）
	files, _ := auth.LoadAuthFiles(authDir)
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		existing, err := auth.Parse(raw)
		if err != nil || existing == nil {
			continue
		}
		eRT := existing.RefreshTokenValue()
		eAT := existing.AccessTokenValue()
		if (refreshToken != "" && eRT != "" && eRT == refreshToken) ||
			(accessToken != "" && eAT != "" && eAT == accessToken) {
			return f
		}
	}

	// 2. 计算首选文件名候选
	var candidate string
	if provider != "" {
		candidate = fmt.Sprintf("workbuddy-%s-%s-%s.json", realm, provider, safeUID)
	} else {
		candidate = fmt.Sprintf("workbuddy-%s-%s.json", realm, safeUID)
	}
	if strings.HasPrefix(strings.ToLower(safeUID), strings.ToLower(realm)+"-") {
		if provider != "" && !strings.Contains(strings.ToLower(safeUID), "-"+provider+"-") {
			candidate = fmt.Sprintf("workbuddy-%s-%s.json", provider, safeUID)
		} else {
			candidate = fmt.Sprintf("workbuddy-%s.json", safeUID)
		}
	}

	targetPath := filepath.Join(authDir, candidate)
	if _, err := os.Stat(targetPath); os.IsNotExist(err) {
		return targetPath
	}

	// 3. 首选文件已存在（但其 token 不同），寻找可用序号后缀
	for i := 2; i <= 99; i++ {
		var altName string
		if provider != "" {
			altName = fmt.Sprintf("workbuddy-%s-%s-%s-%d.json", realm, provider, safeUID, i)
		} else {
			altName = fmt.Sprintf("workbuddy-%s-%s-%d.json", realm, safeUID, i)
		}
		altPath := filepath.Join(authDir, altName)
		if _, err := os.Stat(altPath); os.IsNotExist(err) {
			return altPath
		}
	}

	return filepath.Join(authDir, fmt.Sprintf("workbuddy-%s-%s-%d.json", realm, safeUID, time.Now().UnixNano()%1000000))
}
