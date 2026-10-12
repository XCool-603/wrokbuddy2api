package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// AlertConfig 告警配置。
type AlertConfig struct {
	Enabled   bool
	Webhooks  []string
	Events    []string // ban, 401, cooling, low_credit; 空 = 全部
	MinCredit int
}

// AlertManager 多通道运维风控告警通知管理器。
type AlertManager struct {
	cfg       AlertConfig
	client    *http.Client
	mu        sync.Mutex
	lastAlert map[string]time.Time // key -> 上次告警时间 (防风暴限频)
}

// NewAlertManager 创建 AlertManager 实例。
func NewAlertManager(cfg AlertConfig) *AlertManager {
	if cfg.MinCredit <= 0 {
		cfg.MinCredit = 100
	}
	return &AlertManager{
		cfg: cfg,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
		lastAlert: make(map[string]time.Time),
	}
}

func (m *AlertManager) isEventAllowed(event string) bool {
	if !m.cfg.Enabled || len(m.cfg.Webhooks) == 0 {
		return false
	}
	if len(m.cfg.Events) == 0 {
		return true
	}
	for _, e := range m.cfg.Events {
		if strings.EqualFold(e, event) {
			return true
		}
	}
	return false
}

func (m *AlertManager) shouldThrottle(key string, interval time.Duration) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	last, ok := m.lastAlert[key]
	if ok && time.Since(last) < interval {
		return true
	}
	m.lastAlert[key] = time.Now()
	return false
}

// NotifyBan 账号被封禁/禁用告警 (11140 等)。
func (m *AlertManager) NotifyBan(uid, nick, reason string) {
	if !m.isEventAllowed("ban") {
		return
	}
	throttleKey := "ban:" + uid
	if m.shouldThrottle(throttleKey, 15*time.Minute) {
		return
	}
	msg := fmt.Sprintf("🚨【WorkBuddy 网关告警】账号被封禁\n• 账号: %s (%s)\n• 原因: %s\n• 时间: %s\n• 请登录控制台检查或执行一键重新授权/更换 Token。",
		nick, uid, reason, time.Now().Format("2006-01-02 15:04:05"))
	m.dispatch(msg)
}

// NotifyAuthFailed 401 凭证失效告警。
func (m *AlertManager) NotifyAuthFailed(uid, nick, errMsg string) {
	if !m.isEventAllowed("401") {
		return
	}
	throttleKey := "401:" + uid
	if m.shouldThrottle(throttleKey, 15*time.Minute) {
		return
	}
	msg := fmt.Sprintf("⚠️【WorkBuddy 网关告警】账号凭证失效 (401)\n• 账号: %s (%s)\n• 错误: %s\n• 时间: %s\n• 提示: 访问令牌已过期或失效，请在控制台更新该账号 Token。",
		nick, uid, errMsg, time.Now().Format("2006-01-02 15:04:05"))
	m.dispatch(msg)
}

// NotifyAllCooling 全部账号冷却/限流告警 (6004 等)。
func (m *AlertManager) NotifyAllCooling(realm, model string) {
	if !m.isEventAllowed("cooling") {
		return
	}
	throttleKey := "cooling:" + realm + ":" + model
	if m.shouldThrottle(throttleKey, 10*time.Minute) {
		return
	}
	msg := fmt.Sprintf("🔥【WorkBuddy 网关告警】全池冷却/无可用健康账号\n• 域: %s\n• 模型: %s\n• 时间: %s\n• 提示: 该模型下所有可用账号当前均处于冷却或频控期，建议调增账号数或降低调用频率。",
		realm, model, time.Now().Format("2006-01-02 15:04:05"))
	m.dispatch(msg)
}

// NotifyLowCredit 积分不足告警。
func (m *AlertManager) NotifyLowCredit(uid, nick string, remainCredits int64) {
	if !m.isEventAllowed("low_credit") {
		return
	}
	throttleKey := "low_credit:" + uid
	if m.shouldThrottle(throttleKey, 1*time.Hour) {
		return
	}
	msg := fmt.Sprintf("💰【WorkBuddy 网关告警】账号积分余额不足\n• 账号: %s (%s)\n• 剩余积分: %d (低于阈值 %d)\n• 时间: %s\n• 提示: 请及时签到或补充积分，以免影响生产调用。",
		nick, uid, remainCredits, m.cfg.MinCredit, time.Now().Format("2006-01-02 15:04:05"))
	m.dispatch(msg)
}

// dispatch 异步向所有配置的 Webhook 发送告警通知。
func (m *AlertManager) dispatch(text string) {
	hooks := append([]string(nil), m.cfg.Webhooks...)
	go func() {
		for _, url := range hooks {
			if err := m.sendWebhook(url, text); err != nil {
				log.Printf("WARN: [alert] webhook dispatch failed (%s): %v", maskURL(url), err)
			}
		}
	}()
}

func (m *AlertManager) sendWebhook(rawURL, text string) error {
	var payload []byte
	var err error

	lower := strings.ToLower(rawURL)
	switch {
	case strings.Contains(lower, "feishu.cn") || strings.Contains(lower, "larksuite.com"):
		// 飞书自定义机器人协议
		body := map[string]any{
			"msg_type": "text",
			"content": map[string]any{
				"text": text,
			},
		}
		payload, err = json.Marshal(body)
	case strings.Contains(lower, "qyapi.weixin.qq.com"):
		// 企业微信群机器人协议
		body := map[string]any{
			"msgtype": "text",
			"text": map[string]any{
				"content": text,
			},
		}
		payload, err = json.Marshal(body)
	case strings.Contains(lower, "dingtalk.com"):
		// 钉钉群机器人协议
		body := map[string]any{
			"msgtype": "text",
			"text": map[string]any{
				"content": text,
			},
		}
		payload, err = json.Marshal(body)
	case strings.Contains(lower, "api.telegram.org"):
		// Telegram Bot API
		body := map[string]any{
			"text": text,
		}
		payload, err = json.Marshal(body)
	default:
		// 通用 Webhook 结构
		body := map[string]any{
			"event":     "alert",
			"text":      text,
			"timestamp": time.Now().Unix(),
		}
		payload, err = json.Marshal(body)
	}

	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, rawURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return nil
}

func maskURL(u string) string {
	if len(u) <= 20 {
		return u
	}
	return u[:15] + "..." + u[len(u)-5:]
}
