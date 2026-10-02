package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/usermgr"
)

type importItem struct {
	name string
	data []byte
}

// extractAuthItems 将任意来源的原始 JSON 凭证字节解构成独立的凭证项，支持：
// 1. JSON 数组: [ {...}, {...} ]
// 2. 包装对象: {"accounts": [...]}, {"auths": [...]}, {"data": [...]}, {"items": [...]}
// 3. 多段换行/连续 JSON (NDJSON / Concatenated JSON)
// 4. 单个 JSON 对象
func extractAuthItems(data []byte, defaultName string) []importItem {
	data = auth.CleanRawInput(data)
	if len(data) == 0 {
		return nil
	}

	// 1. JSON 数组: [ {...}, {...} ]
	if bytes.HasPrefix(data, []byte("[")) {
		var rawList []json.RawMessage
		if err := json.Unmarshal(data, &rawList); err == nil && len(rawList) > 0 {
			items := make([]importItem, 0, len(rawList))
			for idx, raw := range rawList {
				rawTrimmed := bytes.TrimSpace(raw)
				if len(rawTrimmed) > 0 {
					items = append(items, importItem{
						name: fmt.Sprintf("%s#%d", defaultName, idx+1),
						data: rawTrimmed,
					})
				}
			}
			return items
		}
	}

	// 2. 检查是否为常见包含账号列表的包装对象
	if bytes.HasPrefix(data, []byte("{")) {
		var wrap map[string]json.RawMessage
		if err := json.Unmarshal(data, &wrap); err == nil {
			for _, key := range []string{"accounts", "auths", "data", "items", "list", "account_list", "tokens"} {
				if rawArr, ok := wrap[key]; ok {
					var list []json.RawMessage
					if err := json.Unmarshal(rawArr, &list); err == nil && len(list) > 0 {
						items := make([]importItem, 0, len(list))
						for idx, raw := range list {
							rawTrimmed := bytes.TrimSpace(raw)
							if len(rawTrimmed) > 0 {
								items = append(items, importItem{
									name: fmt.Sprintf("%s#%d", defaultName, idx+1),
									data: rawTrimmed,
								})
							}
						}
						return items
					}
				}
			}
		}
	}

	// 3. 检查是否是多段连续或换行分隔的 JSON 流 (NDJSON / Concatenated JSON)
	dec := json.NewDecoder(bytes.NewReader(data))
	var streamItems []importItem
	idx := 1
	for dec.More() {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err == nil {
			rawTrimmed := bytes.TrimSpace(raw)
			if len(rawTrimmed) > 0 {
				streamItems = append(streamItems, importItem{
					name: fmt.Sprintf("%s#%d", defaultName, idx),
					data: rawTrimmed,
				})
				idx++
			}
		} else {
			break
		}
	}
	if len(streamItems) > 1 {
		return streamItems
	}

	// 4. 若非标准 JSON 块，检查是否为多行 Token / 凭证组合（按行切割）
	if !bytes.HasPrefix(data, []byte("{")) && !bytes.HasPrefix(data, []byte("[")) {
		lines := bytes.Split(data, []byte("\n"))
		var lineItems []importItem
		lineIdx := 1
		for _, l := range lines {
			trimmed := bytes.TrimSpace(l)
			if len(trimmed) == 0 || bytes.HasPrefix(trimmed, []byte("#")) || bytes.HasPrefix(trimmed, []byte("//")) {
				continue
			}
			lineItems = append(lineItems, importItem{
				name: fmt.Sprintf("%s#%d", defaultName, lineIdx),
				data: trimmed,
			})
			lineIdx++
		}
		if len(lineItems) > 1 {
			return lineItems
		}
	}

	// 5. 单个 JSON 对象或单条原始凭证文本
	return []importItem{{name: defaultName, data: data}}
}

func (h *Handler) handleOAuthImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		JSON string `json:"json"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.JSON) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "JSON 凭证内容为空"})
		return
	}

	raw := []byte(strings.TrimSpace(req.JSON))
	a, err := auth.Parse(raw)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("解析凭证失败: %v", err)})
		return
	}

	uid := a.UID
	if uid == "" {
		uid = fmt.Sprintf("account_%d", time.Now().Unix())
	}
	a.UID = uid

	user := h.getWebSessionUser(r)
	if user != nil && user.Role != usermgr.RoleAdmin && user.Username != "admin" && user.ID != "u_admin" && user.ID != "admin" {
		a.Owner = user.ID
	} else if a.Owner == "" {
		a.Owner = "public"
	}

	authDir := h.getAuthDir()
	_ = os.MkdirAll(authDir, 0755)
	targetFile := filepath.Join(authDir, fmt.Sprintf("workbuddy-%s.json", uid))
	a.FilePath = targetFile
	if err := a.SaveAtomic(); err != nil {
		log.Printf("ERR: [import] single import save failed: uid=%s err=%v", uid, err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": fmt.Sprintf("保存失败: %v", err)})
		return
	}

	// 立即将新账号同步入账号池并清除历史冷却与禁用状态
	if h.cfg.Pool != nil {
		h.cfg.Pool.Add(a)
		h.cfg.Pool.ReviveDisabled(uid)
		h.cfg.Pool.ClearCooling(uid)
	}
	log.Printf("INFO: [import] single import success: uid=%s owner=%s realm=%s file=%s", uid, a.Owner, a.Realm(), filepath.Base(targetFile))

	writeJSON(w, http.StatusOK, map[string]any{
		"success":  true,
		"uid":      uid,
		"filename": filepath.Base(targetFile),
		"realm":    a.Realm(),
		"nickname": a.Nickname,
	})
}

func (h *Handler) handleActionBackup(w http.ResponseWriter, r *http.Request) {
	authDir := h.getAuthDir()
	files, err := auth.LoadAuthFiles(authDir)
	if err != nil {
		http.Error(w, "无法读取凭证目录: "+err.Error(), http.StatusInternalServerError)
		return
	}

	user := h.getWebSessionUser(r)

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=workbuddy2api_auths_backup_%s.zip", time.Now().Format("20060102_150405")))

	zw := zip.NewWriter(w)
	defer zw.Close()

	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		// 如果是普通租户，只备份属于自己的账号
		if user != nil && user.Role != usermgr.RoleAdmin {
			if a, parseErr := auth.Parse(data); parseErr == nil {
				if a.Owner != user.ID && a.Owner != user.Username {
					continue
				}
			}
		}
		base := filepath.Base(f)
		fw, err := zw.Create(base)
		if err != nil {
			continue
		}
		_, _ = fw.Write(data)
	}
}

func (h *Handler) handleOAuthBatchImport(w http.ResponseWriter, r *http.Request) {
	// 支持多种导入方式：
	// 1. multipart/form-data 上传 zip 压缩包或多个 json 文件
	// 2. application/json 批量传入 json array、包装对象或单/多 JSON 文本
	contentType := r.Header.Get("Content-Type")
	user := h.getWebSessionUser(r)
	ownerID := "public"
	if user != nil && user.Role != usermgr.RoleAdmin && user.Username != "admin" && user.ID != "u_admin" && user.ID != "admin" {
		ownerID = user.ID
	}

	var items []importItem
	reqRealm := ""

	if strings.HasPrefix(contentType, "multipart/form-data") {
		// 限制最大 50MB 上传
		if err := r.ParseMultipartForm(50 << 20); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "解析上传文件失败: " + err.Error()})
			return
		}
		reqRealm = strings.ToLower(strings.TrimSpace(r.FormValue("realm")))
		files := r.MultipartForm.File["file"]
		if len(files) == 0 {
			files = r.MultipartForm.File["files"]
		}
		for _, fileHeader := range files {
			f, err := fileHeader.Open()
			if err != nil {
				continue
			}
			data, err := io.ReadAll(f)
			_ = f.Close()
			if err != nil {
				continue
			}
			name := strings.ToLower(fileHeader.Filename)
			if strings.HasSuffix(name, ".zip") {
				// 解开 zip
				zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
				if err == nil {
					for _, zf := range zr.File {
						if zf.FileInfo().IsDir() {
							continue
						}
						baseName := filepath.Base(zf.Name)
						// 过滤 macOS AppleDouble 资源分支与隐藏文件
						if strings.HasPrefix(baseName, ".") || strings.Contains(zf.Name, "__MACOSX") {
							continue
						}
						lowerZf := strings.ToLower(zf.Name)
						if strings.HasSuffix(lowerZf, ".json") || strings.HasSuffix(lowerZf, ".txt") {
							rc, err := zf.Open()
							if err == nil {
								zData, _ := io.ReadAll(rc)
								_ = rc.Close()
								if len(zData) > 0 {
									items = append(items, extractAuthItems(zData, baseName)...)
								}
							}
						}
					}
				}
			} else if strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".txt") {
				baseName := filepath.Base(fileHeader.Filename)
				if !strings.HasPrefix(baseName, ".") {
					items = append(items, extractAuthItems(data, baseName)...)
				}
			}
		}
	} else {
		// 尝试解析为 JSON
		var req struct {
			Content string `json:"content"`
			JSON    string `json:"json"`
			Realm   string `json:"realm"`
		}
		bodyBytes, err := io.ReadAll(r.Body)
		if err == nil && len(bodyBytes) > 0 {
			if jsonErr := json.Unmarshal(bodyBytes, &req); jsonErr == nil {
				if req.Realm != "" {
					reqRealm = strings.ToLower(strings.TrimSpace(req.Realm))
				}
				if req.Content != "" {
					bodyBytes = []byte(strings.TrimSpace(req.Content))
				} else if req.JSON != "" {
					bodyBytes = []byte(strings.TrimSpace(req.JSON))
				}
			}
		}
		items = append(items, extractAuthItems(bodyBytes, "pasted.json")...)
	}

	if len(items) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "未检测到有效的 JSON 凭证或 ZIP 压缩包"})
		return
	}

	authDir := h.getAuthDir()
	_ = os.MkdirAll(authDir, 0755)

	successCount := 0
	failedCount := 0
	var successUIDs []string
	var failReasons []string

	for _, item := range items {
		a, err := auth.Parse(item.data)
		if err != nil {
			failedCount++
			failReasons = append(failReasons, fmt.Sprintf("%s: 解析失败 (%v)", item.name, err))
			continue
		}

		// 若用户在导入界面显式指定了环境 (如 "global" 或 "cn")
		if reqRealm == "global" {
			a.SetRealm("global")
		} else if reqRealm == "cn" {
			a.SetRealm("cn")
		}

		if a.Domain == "" {
			if a.Realm() == "global" {
				a.Domain = "www.workbuddy.ai"
			} else {
				a.Domain = "copilot.tencent.com"
			}
		}

		uid := a.UID
		if uid == "" {
			base := strings.TrimSuffix(item.name, filepath.Ext(item.name))
			if hashIdx := strings.LastIndex(base, "#"); hashIdx > 0 {
				base = base[:hashIdx]
			}
			base = strings.TrimPrefix(base, "workbuddy-")
			base = strings.TrimPrefix(base, "workbuddy_")
			if base != "" && !strings.HasPrefix(base, "item_") && !strings.HasPrefix(base, "single") && !strings.HasPrefix(base, "pasted") {
				uid = base
			} else {
				uid = fmt.Sprintf("account_%d_%d", time.Now().UnixNano(), successCount)
			}
		}
		a.UID = uid
		if a.Owner == "" {
			a.Owner = ownerID
		} else if user != nil && user.Role != usermgr.RoleAdmin && user.Username != "admin" && user.ID != "u_admin" && user.ID != "admin" {
			a.Owner = user.ID
		}
		targetFile := filepath.Join(authDir, fmt.Sprintf("workbuddy-%s.json", uid))
		a.FilePath = targetFile
		if err := a.SaveAtomic(); err != nil {
			failedCount++
			failReasons = append(failReasons, fmt.Sprintf("%s: 落盘失败 (%v)", item.name, err))
			continue
		}
		if h.cfg.Pool != nil {
			h.cfg.Pool.Add(a)
			h.cfg.Pool.ReviveDisabled(uid)
			h.cfg.Pool.ClearCooling(uid)
			// 异步回填积分
			if h.cfg.Upstream != nil && a.AccessTokenValue() != "" {
				go func(acct *auth.Auth, u string) {
					if rem, _, _, _, e := h.cfg.Upstream.ResourceSummary(acct); e == nil {
						h.cfg.Pool.SetCredits(u, rem)
					}
				}(a, uid)
			}
		}
		successCount++
		successUIDs = append(successUIDs, uid)
	}

	log.Printf("INFO: [import] batch import finished: total=%d, success=%d, failed=%d, owner=%s, uids=%v",
		len(items), successCount, failedCount, ownerID, successUIDs)

	if successCount == 0 && failedCount > 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"success":       false,
			"error":         fmt.Sprintf("导入失败: 全部 %d 个凭证均解析或落盘失败", failedCount),
			"total":         len(items),
			"success_count": 0,
			"failed_count":  failedCount,
			"success_uids":  successUIDs,
			"fail_reasons":  failReasons,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success":       true,
		"total":         len(items),
		"success_count": successCount,
		"failed_count":  failedCount,
		"success_uids":  successUIDs,
		"fail_reasons":  failReasons,
	})
}
