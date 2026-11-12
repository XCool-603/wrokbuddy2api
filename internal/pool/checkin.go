package pool

import "time"

var checkinCSTZone = time.FixedZone("CST", 8*3600)

// CheckinRecord 记录单个账号在当前北京时间自然日内的签到状态与回执。
type CheckinRecord struct {
	Status    string    `json:"status"`
	Detail    string    `json:"detail,omitempty"`
	DateCST   string    `json:"date_cst"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CheckinRunSummary 记录最近一次全池签到的执行摘要。
type CheckinRunSummary struct {
	LastRunAt time.Time `json:"last_run_at"`
	DateCST   string    `json:"date_cst"`
	Trigger   string    `json:"trigger,omitempty"`
	Total     int       `json:"total"`
	OK        int       `json:"ok"`
	Already   int       `json:"already"`
	Fail      int       `json:"fail"`
	Skipped   int       `json:"skipped"`
}

func todayCSTDate() string {
	return time.Now().In(checkinCSTZone).Format("2006-01-02")
}

// SetCheckinResult 写入账号今日签到状态（仅内存运行态，次日自然日 CST 自动失效）。
func (p *Pool) SetCheckinResult(uid, status, detail string) {
	if p == nil || uid == "" || status == "" {
		return
	}
	now := time.Now()
	p.checkinMu.Lock()
	if p.checkins == nil {
		p.checkins = make(map[string]CheckinRecord)
	}
	p.checkins[uid] = CheckinRecord{
		Status:    status,
		Detail:    detail,
		DateCST:   now.In(checkinCSTZone).Format("2006-01-02"),
		UpdatedAt: now,
	}
	p.checkinMu.Unlock()
}

// GetCheckinResultToday 返回账号在今日北京时间（CST）内的签到状态。
func (p *Pool) GetCheckinResultToday(uid string) (CheckinRecord, bool) {
	if p == nil || uid == "" {
		return CheckinRecord{}, false
	}
	today := todayCSTDate()
	p.checkinMu.RLock()
	defer p.checkinMu.RUnlock()
	if p.checkins == nil {
		return CheckinRecord{}, false
	}
	rec, ok := p.checkins[uid]
	if !ok || rec.DateCST != today {
		return CheckinRecord{}, false
	}
	return rec, true
}

// SetCheckinRunSummary 记录最近一次全池签到统计摘要。
func (p *Pool) SetCheckinRunSummary(trigger string, total, okN, alreadyN, failN, skipN int) {
	if p == nil {
		return
	}
	now := time.Now()
	p.checkinMu.Lock()
	p.checkinSummary = &CheckinRunSummary{
		LastRunAt: now,
		DateCST:   now.In(checkinCSTZone).Format("2006-01-02"),
		Trigger:   trigger,
		Total:     total,
		OK:        okN,
		Already:   alreadyN,
		Fail:      failN,
		Skipped:   skipN,
	}
	p.checkinMu.Unlock()
}

// GetCheckinRunSummary 返回最近一次全池签到摘要（若已跨日仍保留上次运行记录供界面参考）。
func (p *Pool) GetCheckinRunSummary() *CheckinRunSummary {
	if p == nil {
		return nil
	}
	p.checkinMu.RLock()
	defer p.checkinMu.RUnlock()
	if p.checkinSummary == nil {
		return nil
	}
	cp := *p.checkinSummary
	return &cp
}
