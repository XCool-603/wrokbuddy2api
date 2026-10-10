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
// 2. 若磁盘上已有相同 realm + provider + uid (或 rawUID/email/nickname) 的凭证文件，视为该同一账号重新授权/重新登录（Token 已换新），
//    必须就地复用更新该文件，绝不产生 -2.json 等副本；若存在历史遗留的 -2.json 副本，优先复用主文件并清理副本；
// 3. 计算首选标准规范文件名，若该目标文件已存在且同属该账号/provider，直接就地更新；
// 4. 绝不覆盖属于其他不同账号/不同 provider 的文件。
func resolveAuthFilePath(authDir, realm, uid, provider, accessToken, refreshToken string) string {
	safeUID := auth.SanitizeFilename(uid)
	provider = auth.NormalizeProvider(provider)
	if realm == "" {
		realm = "cn"
	}

	cleanUID := strings.ToLower(auth.CleanRawUID(uid))

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

	// 2. 账号身份与提供商匹配：
	// 若已有相同 realm + provider + 相同账号身份（UID、RawUID、Nickname 或文件名）的文件，
	// 无论上游 OAuth 换发了什么新 Token，均判定为同账号重新登录，就地更新！
	var matchedFile string
	var duplicateFiles []string
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		existing, err := auth.Parse(raw)
		if err != nil || existing == nil {
			continue
		}
		if existing.Realm() != realm {
			continue
		}

		eProv := existing.Provider()
		if eProv == "" {
			base := strings.ToLower(filepath.Base(f))
			if strings.Contains(base, "google") {
				eProv = "google"
			} else if strings.Contains(base, "twitter") || strings.Contains(base, "-x-") {
				eProv = "twitter"
			} else if strings.Contains(base, "github") {
				eProv = "github"
			}
		}

		// 仅当明确指定 provider 且两者完全一致时才按身份归并就地更新；
		// 若未指定 provider（如通用匿名/测试凭证），不同 Token 需保留各自独立文件（防跨会话误覆盖）
		if provider == "" || eProv == "" || provider != eProv {
			continue
		}

		// 检查身份一致性
		isMatch := false
		eRaw := strings.ToLower(existing.RawUID())
		eUID := strings.ToLower(existing.UID)
		eNick := strings.ToLower(existing.Nickname)

		if cleanUID != "" {
			if cleanUID == eRaw || cleanUID == eUID || cleanUID == eNick {
				isMatch = true
			}
		}
		if !isMatch && safeUID != "" {
			if strings.EqualFold(auth.SanitizeFilename(eUID), safeUID) ||
				strings.EqualFold(auth.SanitizeFilename(eRaw), safeUID) {
				isMatch = true
			}
			base := strings.ToLower(filepath.Base(f))
			if strings.Contains(base, strings.ToLower(safeUID)) {
				isMatch = true
			}
		}

		if isMatch {
			base := filepath.Base(f)
			// 如果此文件不是 -2.json 等副本，设为主文件；如果是 -2.json 则记录为 duplicateFiles 待清理
			if strings.Contains(base, "-2.json") || strings.Contains(base, "-3.json") || strings.Contains(base, "#") {
				duplicateFiles = append(duplicateFiles, f)
			} else if matchedFile == "" {
				matchedFile = f
			} else {
				duplicateFiles = append(duplicateFiles, f)
			}
		}
	}

	if matchedFile != "" {
		// 清理同账号历史遗留的 -2.json 副本，恢复单文件规范
		for _, dup := range duplicateFiles {
			if dup != matchedFile {
				_ = os.Remove(dup)
			}
		}
		return matchedFile
	}
	if len(duplicateFiles) > 0 {
		return duplicateFiles[0]
	}

	// 3. 计算首选文件名候选
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

	// 若目标路径已存在，检查它是否就是该 provider 的账号
	if raw, err := os.ReadFile(targetPath); err == nil {
		if existing, err := auth.Parse(raw); err == nil && existing != nil {
			eProv := existing.Provider()
			if eProv == "" {
				base := strings.ToLower(filepath.Base(targetPath))
				if strings.Contains(base, "google") {
					eProv = "google"
				} else if strings.Contains(base, "twitter") {
					eProv = "twitter"
				}
			}
			if existing.Realm() == realm && provider != "" && eProv == provider {
				return targetPath
			}
		}
	}

	// 4. 首选文件已存在且属于不同 provider 时，寻找可用序号后缀
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
