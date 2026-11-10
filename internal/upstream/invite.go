// invite.go 官方邀请体系（专属邀请码/邀请链接查询、受邀绑定）与全套新人/每日福利一键领取。
package upstream

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"workbuddy2api/internal/auth"
)

// InviteInfo 账号专属官方邀请信息。
type InviteInfo struct {
	InviteCode   string `json:"invite_code"`             // 专属邀请码
	InviteURL    string `json:"invite_url"`              // 专属邀请注册链接
	InvitedCount int    `json:"invited_count,omitempty"` // 已成功邀请人数
	RewardCredit int64  `json:"reward_credit,omitempty"` // 累计获得的邀请奖励积分
}

// 内存安全缓存，避免重复高频调用上游。
var (
	inviteCacheMu sync.RWMutex
	inviteCache   = make(map[string]*InviteInfo)
)

// DefaultInviteCode 由账号信息派生标准可用的邀请码（优先使用去除前缀的 RawUID）。
func DefaultInviteCode(a *auth.Auth) string {
	if a == nil {
		return ""
	}
	code := auth.CleanRawUID(a.RawUID())
	if code == "" {
		code = auth.CleanRawUID(a.UID)
	}
	if code == "" {
		code = a.UID
	}
	return code
}

// DefaultInviteURL 由账号信息和邀请码生成官方标准邀请注册链接。
func DefaultInviteURL(a *auth.Auth, code string) string {
	if code == "" {
		code = DefaultInviteCode(a)
	}
	if a != nil && a.IsGlobal() {
		return "https://www.workbuddy.ai/i/" + code
	}
	return "https://workbuddy.cn/i/" + code
}

// FetchInviteInfo 查询或生成指定账号的专属邀请链接与邀请码。
// 优先请求上游活动中心端点，若接口暂未开放或发生限流，自动回退至官方标准链接规则，保证 100% 可用。
func (c *Client) FetchInviteInfo(a *auth.Auth) (*InviteInfo, error) {
	if a == nil {
		return nil, errors.New("auth is nil")
	}

	cacheKey := fmt.Sprintf("%s:%s", a.Realm(), a.UID)
	inviteCacheMu.RLock()
	if cached, ok := inviteCache[cacheKey]; ok && cached != nil {
		inviteCacheMu.RUnlock()
		return cached, nil
	}
	inviteCacheMu.RUnlock()

	code := DefaultInviteCode(a)
	targetURL := DefaultInviteURL(a, code)

	info := &InviteInfo{
		InviteCode: code,
		InviteURL:  targetURL,
	}

	// 尝试向上游活动/成长端点嗅探专属邀请数据
	probeEndpoints := []string{
		"/activity/growth/user/invite",
		"/activity/growth/invite/info",
		"/portal/activity/share/info",
		"/activity/growth/share/info",
	}

	for _, ep := range probeEndpoints {
		raw, err := c.growthJSON(a, http.MethodGet, ep, nil)
		if err == nil && len(raw) > 0 {
			var resp struct {
				InviteCode   string `json:"invite_code"`
				Code         string `json:"code"`
				ShareCode    string `json:"share_code"`
				InviteURL    string `json:"invite_url"`
				ShareURL     string `json:"share_url"`
				InvitedCount int    `json:"invited_count"`
				RewardCredit int64  `json:"reward_credit"`
			}
			if jerr := json.Unmarshal(raw, &resp); jerr == nil {
				if resp.InviteCode != "" {
					info.InviteCode = resp.InviteCode
				} else if resp.Code != "" {
					info.InviteCode = resp.Code
				} else if resp.ShareCode != "" {
					info.InviteCode = resp.ShareCode
				}

				if resp.InviteURL != "" {
					info.InviteURL = resp.InviteURL
				} else if resp.ShareURL != "" {
					info.InviteURL = resp.ShareURL
				} else {
					info.InviteURL = DefaultInviteURL(a, info.InviteCode)
				}

				if resp.InvitedCount > 0 {
					info.InvitedCount = resp.InvitedCount
				}
				if resp.RewardCredit > 0 {
					info.RewardCredit = resp.RewardCredit
				}
				break
			}
		}
	}

	inviteCacheMu.Lock()
	inviteCache[cacheKey] = info
	inviteCacheMu.Unlock()

	return info, nil
}

// AcceptInvite 新账号绑定邀请人（填入老号的邀请码），触发双方奖励。
func (c *Client) AcceptInvite(a *auth.Auth, inviteCode string) (int64, error) {
	if a == nil {
		return 0, errors.New("auth is nil")
	}
	inviteCode = strings.TrimSpace(inviteCode)
	if inviteCode == "" {
		return 0, errors.New("邀请码不能为空")
	}

	// 尝试向绑定接口提交
	bindEndpoints := []string{
		"/activity/growth/invite/accept",
		"/activity/growth/user/bind-inviter",
		"/billing/meter/invite/bind",
	}

	var lastErr error
	for _, ep := range bindEndpoints {
		raw, err := c.growthJSON(a, http.MethodPost, ep, map[string]string{
			"invite_code": inviteCode,
			"code":        inviteCode,
		})
		if err == nil {
			var resp struct {
				Credit int64 `json:"credit"`
				Reward int64 `json:"reward_credit"`
			}
			_ = json.Unmarshal(raw, &resp)
			reward := resp.Credit
			if reward <= 0 {
				reward = resp.Reward
			}
			return reward, nil
		}
		lastErr = err
	}

	return 0, lastErr
}

// WelfareResult 单个账号执行「吃满全套福利」的结果明细。
type WelfareResult struct {
	UID             string `json:"uid"`
	Nickname        string `json:"nickname"`
	Realm           string `json:"realm"`
	GiftCredit      int64  `json:"gift_credit"`       // 新手礼包到账积分
	GiftStatus      string `json:"gift_status"`       // OK / ALREADY / FAIL / N/A
	CompCredit      int64  `json:"comp_credit"`       // 活动补偿到账积分
	CompStatus      string `json:"comp_status"`       // OK / ALREADY / FAIL / N/A
	CheckinStatus   string `json:"checkin_status"`    // OK / ALREADY / FAIL / SKIPPED
	CheckinDetail   string `json:"checkin_detail"`    // 签到详情
	TrialCredit     int64  `json:"trial_credit"`      // 国际版试用额度
	TrialStatus     string `json:"trial_status"`      // OK / ALREADY / FAIL / N/A
	BuddyStatus     string `json:"buddy_status"`      // OK / FAIL / SKIPPED
	TotalNewCredits int64  `json:"total_new_credits"` // 本次执行累计到账积分
	FinalCredits    int64  `json:"final_credits"`     // 当前账号最新积分余额
	Error           string `json:"error,omitempty"`
}

// IsAlreadyWelfareErr 报告错误是否表示福利此前已经领取过（业务幂等错误）。
func IsAlreadyWelfareErr(err error) bool {
	if err == nil {
		return false
	}
	var ue *Error
	msg := err.Error()
	if errors.As(err, &ue) {
		msg = ue.Msg
	}
	low := strings.ToLower(msg)
	for _, code := range []string{"10001", "14001", "14051", "duplicate", "already"} {
		if strings.Contains(low, code) {
			return true
		}
	}
	for _, marker := range []string{"已领取", "已领", "already claimed", "已经领过", "不可重复"} {
		if strings.Contains(low, strings.ToLower(marker)) {
			return true
		}
	}
	return false
}

// ClaimAllWelfare 一键为账号领取全套新手礼包、活动补偿、每日签到及试用福利，并激活猫猫成长旅途。
func (c *Client) ClaimAllWelfare(a *auth.Auth) *WelfareResult {
	res := &WelfareResult{
		UID:           a.UID,
		Nickname:      a.Nickname,
		Realm:         a.Realm(),
		GiftStatus:    "N/A",
		CompStatus:    "N/A",
		CheckinStatus: "N/A",
		TrialStatus:   "N/A",
		BuddyStatus:   "SKIPPED",
	}

	if a.Nickname == "" {
		res.Nickname = "-"
	}

	// 1. 若 Token 即将过期，先行刷新
	if a.NeedsRefresh(2 * 3600) {
		if err := c.RefreshToken(a); err == nil {
			a.BackfillRealm()
			_ = a.SaveAtomic()
		}
	}

	// 2. 国际版：领取海外试用额度
	if a.IsGlobal() {
		res.CheckinStatus = "SKIPPED"
		res.CheckinDetail = "国际版无每日签到"

		claimed, err := c.ClaimTrial(a)
		if err == nil {
			if claimed {
				res.TrialStatus = "OK"
				res.TrialCredit = 50
				res.TotalNewCredits += 50
			} else {
				res.TrialStatus = "ALREADY"
			}
		} else if IsAlreadyWelfareErr(err) {
			res.TrialStatus = "ALREADY"
		} else {
			res.TrialStatus = "FAIL"
		}
	} else {
		// 3. 国内版：全套新手福利闭环
		// A. 新手礼包（claim-gift）
		if gift, err := c.ClaimGift(a); err == nil && gift > 0 {
			res.GiftStatus = "OK"
			res.GiftCredit = gift
			res.TotalNewCredits += gift
		} else if IsAlreadyWelfareErr(err) {
			res.GiftStatus = "ALREADY"
		} else if err != nil {
			res.GiftStatus = "FAIL"
		} else {
			res.GiftStatus = "ALREADY"
		}

		// B. 活动补偿（claim-compensation）
		if comp, err := c.ClaimCompensation(a); err == nil && comp > 0 {
			res.CompStatus = "OK"
			res.CompCredit = comp
			res.TotalNewCredits += comp
		} else if IsAlreadyWelfareErr(err) {
			res.CompStatus = "ALREADY"
		} else if err != nil {
			res.CompStatus = "FAIL"
		} else {
			res.CompStatus = "ALREADY"
		}

		// C. 每日签到（checkin）
		if err := c.DailyCheckin(a); err == nil {
			res.CheckinStatus = "OK"
			res.CheckinDetail = "签到成功"
		} else {
			low := strings.ToLower(err.Error())
			if strings.Contains(low, "已签到") || strings.Contains(low, "already") || strings.Contains(low, "10001") || strings.Contains(low, "14001") {
				res.CheckinStatus = "ALREADY"
				res.CheckinDetail = "今日已签到"
			} else {
				res.CheckinStatus = "FAIL"
				res.CheckinDetail = err.Error()
			}
		}

		// D. 激活新手猫猫成长旅途与协议，并自动派出或领奖
		buddy, _ := c.BuddyInfo(a)
		if buddy == nil {
			if err := c.AdoptBuddyWithUnlock(a); err == nil {
				res.BuddyStatus = "ADOPTED"
				res.TotalNewCredits += 300
			} else {
				res.BuddyStatus = "NO_CAT"
			}
		} else {
			res.BuddyStatus = "READY"
		}

		// 检查猫猫旅行状态机并自动派猫或领奖
		if ts, err := c.TravelStatus(a); err == nil && ts != nil {
			switch ts.State {
			case "arrived":
				if ts.RecordID > 0 {
					if reward, err := c.TravelClaim(a, ts.RecordID); err == nil {
						res.BuddyStatus = "CLAIMED"
						res.TotalNewCredits += reward
					}
				}
			case "idle":
				if !ts.DailyLimitReached {
					if err := c.TravelDepart(a, 4); err == nil {
						res.BuddyStatus = "DEPARTED"
					}
				} else {
					if res.BuddyStatus != "ADOPTED" {
						res.BuddyStatus = "LIMIT"
					}
				}
			case "traveling":
				if res.BuddyStatus != "ADOPTED" {
					res.BuddyStatus = "TRAVELING"
				}
			}
		}
	}

	// 4. 查询并同步账号最新积分余额
	if rem, err := c.UserResource(a); err == nil {
		res.FinalCredits = rem
	}

	return res
}
