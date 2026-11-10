// tasks.go 内置纯 Go 成长任务与最新活动执行器：
// 使 Windows 单文件绿色版等无 scripts/Python 的环境同样可一键完成成长任务、活动任务与领猫。
package upstream

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"workbuddy2api/internal/auth"
)

const (
	growthTasksListPath   = "/v2/activity/growth/tasks"
	growthTasksAcceptPath = "/v2/activity/growth/tasks/accept"
	defaultWebBase        = "https://www.workbuddy.cn"
	desktopUA             = "WorkBuddy/5.5.6 WorkBuddy/5.5.6 CLI/2.137.1"
	webBrowserUA          = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"
	libraryDocURL         = "https://www.workbuddy.cn/space/d/o0KWYeynteVv06UnAZqIFm"
)

// taskActionGap 单次动作之间默认防风控间隔（测试可置 0）。
var taskActionGap = 350 * time.Millisecond

// NativeTaskResult 单账号内置纯 Go 任务执行汇总。
type NativeTaskResult struct {
	UID            string
	Total          int
	OK             int
	Already        int
	Skipped        int
	Failed         int
	ClaimedCredits int64
	ClaimedEnergy  int64
	Logs           []string
}

type growthTaskItem struct {
	TaskCode     string `json:"task_code"`
	AcceptStatus string `json:"accept_status"`
	Progress     struct {
		Current int `json:"current"`
		Target  int `json:"target"`
	} `json:"progress"`
}

type nativeTaskSpec struct {
	Kind      string
	Target    int
	ExpertID  string
	SkipNote  string
	NightOnly bool
}

var nativeTaskSpecs = map[string]nativeTaskSpec{
	"create_canvas":                 {Kind: "canvas", Target: 1},
	"template_5":                    {Kind: "template", Target: 5},
	"expert_5":                      {Kind: "expert", Target: 5},
	"Expert_team_use_3":             {Kind: "team", Target: 3},
	"skill_1":                       {Kind: "skill", Target: 1},
	"automation_1":                  {Kind: "automation", Target: 1},
	"playbook_prompt":               {Kind: "playbook", Target: 1},
	"Expert_lighthouse":             {Kind: "lighthouse", Target: 1, ExpertID: "ex_2cvvUZQhDyeJ"},
	"Buddy_App":                     {Kind: "buddy5", Target: 1},
	"Buddy_App_QQ":                  {Kind: "buddy5", Target: 1},
	"Hp_Appearance":                 {Kind: "skin", Target: 1},
	"chat_5":                        {Kind: "chat", Target: 5},
	"Model_chat_GLM5.2":             {Kind: "glmchat", Target: 1},
	"black_cat":                     {Kind: "cat", Target: 3, NightOnly: true},
	"first_buddy":                   {Kind: "buddyfirst", Target: 1},
	"RichMeow_Chat":                 {Kind: "richmeow", Target: 1},
	"Library_read":                  {Kind: "library", Target: 1},
	"expert_use_maidehao":           {Kind: "fixed_expert", Target: 1, ExpertID: "ex_GDl31WahyIH0"},
	"expert_use_douyinyin":          {Kind: "fixed_expert", Target: 1, ExpertID: "ex_FbY5nlWTqDod"},
	"expert_use_shangjia_xuanchuan": {Kind: "fixed_expert", Target: 1, ExpertID: "ex_Weo3x2EZuSDm"},
	"expert_use_bowangwang":         {Kind: "fixed_expert", Target: 1, ExpertID: "ex_WZY136wV5aMt"},
	"WorkBuddy_Valorant":            {Kind: "fixed_expert", Target: 1, ExpertID: "ex_xQHHD1u5Jkua"},
	"Sequential_Tasks_1":            {Kind: "minichat", Target: 1},
	"school_season":                 {Kind: "schoolseason", Target: 1},
	"wb_wechat_oa_subscribe_task":   {SkipNote: "关注微信公众号需外部回调"},
	"Expert_Philanthropy":           {SkipNote: "真实捐款动作不可自动执行"},
}

var defaultTaskExperts = []struct {
	ID   string
	Name string
}{
	{"ContentCreator", "内容创作专家"},
	{"UiDesigner", "UI设计师"},
	{"DataAnalyticsReporter", "数据分析报告师"},
	{"ChinaEcommerceOperationsExpert", "中国电商运营专家"},
	{"DouyinStrategist", "抖音策略师"},
}

var defaultTaskTeams = []string{
	"CloudOpsTeam", "SoftwareCompany", "TradingAgentTeam", "GPTResearcherTeam", "MarketingCampaignTeam",
}

var defaultTaskScenes = []struct {
	ID   string
	Name string
}{
	{"0", "幻灯片"}, {"2", "视频生成"}, {"4", "深度研究"}, {"6", "文档处理"}, {"8", "数据分析"},
}

func (c *Client) webBase() string {
	if c != nil && c.BillingBaseCN != "" && c.ChatBaseCN != "" && c.ChatBaseCN == c.BillingBaseCN {
		// 单测场景通常将 ChatBaseCN/BillingBaseCN 同时指向 httptest.Server
		return c.ChatBaseCN
	}
	return defaultWebBase
}

// RunGrowthTasks 内置纯 Go 成长任务与最新活动自动化执行（零 Python/scripts 依赖）。
func (c *Client) RunGrowthTasks(ctx context.Context, a *auth.Auth) (NativeTaskResult, error) {
	if a == nil || a.IsGlobal() {
		return NativeTaskResult{}, fmt.Errorf("growth tasks: only cn accounts")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	uid := a.RawUID()
	uid8 := uid
	if len(uid8) > 8 {
		uid8 = uid8[:8]
	}
	res := NativeTaskResult{UID: a.UID}
	logf := func(format string, args ...any) {
		res.Logs = append(res.Logs, fmt.Sprintf("[native_tasks] "+uid8+" "+format, args...))
	}

	if a.NeedsRefresh(30*time.Minute) && a.RefreshTokenValue() != "" {
		if err := c.RefreshToken(a); err == nil {
			a.BackfillRealm()
			_ = a.SaveAtomic()
		}
	}

	tasks, err := c.listGrowthTasks(ctx, a, false)
	if err != nil {
		return res, err
	}
	for _, t := range tasks {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		c.processGrowthTask(ctx, a, t, false, &res, logf)
	}

	// 小程序渠道限定任务（Sequential_Tasks_1 / school_season）
	for _, code := range []string{"Sequential_Tasks_1", "school_season"} {
		if ctx.Err() != nil {
			break
		}
		mpTasks, mperr := c.listGrowthTasks(ctx, a, true)
		if mperr != nil {
			break
		}
		for _, t := range mpTasks {
			if t.TaskCode == code {
				c.processGrowthTask(ctx, a, t, true, &res, logf)
				break
			}
		}
	}

	logf("summary: total=%d ok=%d already=%d skipped=%d fail=%d credit=+%d energy=+%d",
		res.Total, res.OK, res.Already, res.Skipped, res.Failed, res.ClaimedCredits, res.ClaimedEnergy)
	return res, nil
}

func (c *Client) processGrowthTask(ctx context.Context, a *auth.Auth, t growthTaskItem, mp bool, res *NativeTaskResult, logf func(string, ...any)) {
	code := t.TaskCode
	if code == "" {
		return
	}
	res.Total++
	spec, known := nativeTaskSpecs[code]
	if !known {
		if t.AcceptStatus == "claimed" {
			res.Already++
			logf("%s: query claimed -> 已领(非映射任务)", code)
			return
		}
		res.Skipped++
		logf("%s: query %s -> 未映射任务，跳过", code, t.AcceptStatus)
		return
	}
	if spec.SkipNote != "" {
		res.Skipped++
		logf("%s: %s，跳过", code, spec.SkipNote)
		return
	}
	if t.AcceptStatus == "claimed" {
		res.Already++
		logf("%s: already_claimed", code)
		return
	}

	if t.AcceptStatus == "completed" {
		c.claimGrowthTask(ctx, a, code, mp, res, logf)
		return
	}

	if spec.NightOnly && !withinCSTNightWindow(time.Now()) {
		res.Skipped++
		logf("%s: 非夜猫时段(23:00-08:00 CST)，跳过", code)
		return
	}

	if t.AcceptStatus == "not_accepted" || t.AcceptStatus == "" {
		if err := c.acceptGrowthTask(ctx, a, code, mp); err != nil {
			res.Failed++
			logf("%s: accept failed: %v", code, err)
			return
		}
		sleepTaskGap(ctx)
	}

	if spec.Kind == "buddyfirst" {
		if err := c.AdoptBuddyWithUnlock(a); err != nil {
			res.Failed++
			logf("%s: adopt failed: %v", code, err)
			return
		}
		// 领养本身直给 +300，再幂等 claim 一次
		c.claimGrowthTask(ctx, a, code, mp, res, logf)
		return
	}

	target := t.Progress.Target
	if target <= 0 {
		target = spec.Target
	}
	need := target - t.Progress.Current
	if need <= 0 {
		need = 1
	}
	if spec.NightOnly {
		need = 1
	}

	if err := c.emitTaskEvents(ctx, a, code, spec, need); err != nil {
		res.Failed++
		logf("%s: report failed: %v", code, err)
		return
	}
	sleepTaskGap(ctx)
	c.claimGrowthTask(ctx, a, code, mp, res, logf)
}

func (c *Client) listGrowthTasks(ctx context.Context, a *auth.Auth, mp bool) ([]growthTaskItem, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.chatBase(a)+growthTasksListPath, nil)
	if err != nil {
		return nil, err
	}
	c.BillingHeaders(req, a)
	if mp {
		req.Header.Set("X-Client-Platform", "miniprogram")
	}
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Tasks []growthTaskItem `json:"tasks"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("list tasks parse: %w", err)
	}
	return resp.Tasks, nil
}

func (c *Client) acceptGrowthTask(ctx context.Context, a *auth.Auth, code string, mp bool) error {
	raw, _ := json.Marshal(map[string]any{"task_codes": []string{code}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatBase(a)+growthTasksAcceptPath, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	c.BillingHeaders(req, a)
	if mp {
		req.Header.Set("X-Client-Platform", "miniprogram")
	}
	_, err = c.doJSON(req)
	return err
}

func (c *Client) claimGrowthTask(ctx context.Context, a *auth.Auth, code string, mp bool, res *NativeTaskResult, logf func(string, ...any)) {
	path := "/activity/growth/tasks/" + code + "/claim"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatBase(a)+path, nil)
	if err != nil {
		res.Failed++
		logf("%s: build claim req err=%v", code, err)
		return
	}
	c.BillingHeaders(req, a)
	if mp {
		req.Header.Set("X-Client-Platform", "miniprogram")
	}
	data, err := c.doJSON(req)
	var ue *Error
	if err != nil && errors.As(err, &ue) && ue.Status == http.StatusBadRequest {
		// 400 自动降级 web 域（对齐 task_common.py web_claim_fallback）
		wb := c.webBase()
		reqWeb, werr := http.NewRequestWithContext(ctx, http.MethodPost, wb+path, nil)
		if werr == nil {
			c.BillingHeaders(reqWeb, a)
			reqWeb.Header.Set("x-client-platform", "web")
			reqWeb.Header.Set("Origin", wb)
			reqWeb.Header.Set("Referer", wb+"/profile/growth-center")
			reqWeb.Header.Set("User-Agent", webBrowserUA)
			reqWeb.Header.Set("X-Domain", wb)
			data, err = c.doJSON(reqWeb)
		}
	}
	if err != nil {
		res.Failed++
		logf("%s: claim err=%v", code, err)
		return
	}
	var cr struct {
		Credit         int64 `json:"credit"`
		Energy         int64 `json:"energy"`
		AlreadyClaimed bool  `json:"already_claimed"`
	}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &cr)
	}
	if cr.AlreadyClaimed {
		res.Already++
		logf("%s: claim already_claimed", code)
		return
	}
	res.OK++
	res.ClaimedCredits += cr.Credit
	res.ClaimedEnergy += cr.Energy
	logf("%s: claim ok (+%d credit, +%d energy)", code, cr.Credit, cr.Energy)
}

func (c *Client) emitTaskEvents(ctx context.Context, a *auth.Auth, code string, spec nativeTaskSpec, need int) error {
	uid := a.RawUID()
	switch spec.Kind {
	case "richmeow":
		return c.reportDesktopEvents(ctx, a, buildDesktopChatEvents(uid, a.Nickname))
	case "buddy5":
		return c.reportDesktopEvents(ctx, a, buildDesktopBuddy5Events(uid, a.Nickname, "cb_y5Dy46tPQGGWtueMxXbe", "企鹅教师助手"))
	case "library":
		return c.reportWebLibraryEvent(ctx, a, uid)
	}

	for i := 0; i < need; i++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		ev := buildStandardTaskEvent(uid, spec, i)
		raw, err := json.Marshal([]map[string]any{ev})
		if err != nil {
			return err
		}
		if _, err := c.billingJSON(a, http.MethodPost, reportPath, json.RawMessage(raw)); err != nil {
			return err
		}
		if i < need-1 {
			sleepTaskGap(ctx)
		}
	}
	return nil
}

func buildStandardTaskEvent(uid string, spec nativeTaskSpec, idx int) map[string]any {
	now := time.Now().UnixMilli()
	cid := fmt.Sprintf("wb-run-%d-%d", now, idx)
	rid := fmt.Sprintf("%s-%d", cid, now)
	switch spec.Kind {
	case "canvas":
		return map[string]any{
			"eventCode": "wbx_design_canvas_task_create", "timestamp": now, "reportDelay": 0,
			"conversationId": cid, "requestId": rid, "source": "summon_keyword",
			"isCustomModel": false, "name": "", "inputLength": 12,
			"id": fmt.Sprintf("wbx-canvas-%d", now), "cost": 0, "isSuccessful": true, "userId": uid,
		}
	case "template":
		sc := defaultTaskScenes[idx%len(defaultTaskScenes)]
		return map[string]any{
			"eventCode": "agent_task_created_with_template", "timestamp": now, "reportDelay": 0,
			"isCustomModel": true, "id": sc.ID, "name": sc.Name,
			"requestId": rid, "conversationId": cid, "userId": uid,
		}
	case "expert":
		ex := defaultTaskExperts[idx%len(defaultTaskExperts)]
		return buildExpertEvent(uid, cid, rid, now, ex.ID, ex.Name, "agent")
	case "fixed_expert", "lighthouse":
		return buildExpertEvent(uid, cid, rid, now, spec.ExpertID, spec.ExpertID, "agent")
	case "team":
		tid := defaultTaskTeams[idx%len(defaultTaskTeams)]
		return buildExpertEvent(uid, cid, rid, now, tid, tid, "team")
	case "skill":
		return map[string]any{
			"eventCode": "skill_info", "timestamp": now, "reportDelay": 0,
			"skillId": "skill_2096525080079265792", "skillName": "pptx", "skillVersion": "",
			"action": "use", "conversationId": cid, "requestId": rid, "userId": uid,
		}
	case "automation":
		return map[string]any{
			"eventCode": "automated_task_create_suc", "timestamp": now, "reportDelay": 0,
			"name": "每周五自动生成周报", "source": "manually",
			"modelId": "deepseek-v4-flash", "modelIsThinking": false,
			"expertId": "", "expertMarketplace": "", "connectorIds": "", "connectorCount": 0,
			"skills": "", "skillCount": 0, "scheduleType": "recurring",
			"pushToWeChat": false, "pushToWecomBot": false,
			"conversationId": cid, "requestId": rid,
			"schedule": map[string]any{"type": "recurring", "rrule": "FREQ=WEEKLY;BYDAY=FR;BYHOUR=9;BYMINUTE=0"},
			"prompt": "每周五自动整理本周工作，生成一份周报。", "userId": uid,
		}
	case "playbook":
		return map[string]any{
			"eventCode": "playbook_prompt_send", "timestamp": now, "reportDelay": 0,
			"id": "worker-ledger-freedom-dashboard", "name": "打工人小账本", "type": "other",
			"promptLength": 0, "isOfficial": 1, "skills": "", "skillNames": "",
			"expertId": "", "expertName": "", "categoryId": "", "categoryName": "",
			"query": "", "source": "discover", "conversationId": cid, "requestId": rid,
			"ext1": "discover", "userId": uid,
		}
	case "skin":
		return map[string]any{
			"eventCode": "appearance_skin_apply", "timestamp": now, "reportDelay": 0,
			"action": "apply", "source": "settings_close", "id": "theme-tkmw7j",
			"vipLevel": "free", "series": "craft", "type": "unknown", "name": "和平精英激战金秋", "userId": uid,
		}
	case "minichat":
		return map[string]any{
			"eventCode": "chat_request_send", "timestamp": now, "reportDelay": 0,
			"source": "mini_program", "ideName": "wx_app_cloud", "ideType": "WorkBuddy_MP",
			"extName": "workbuddy-mp", "extVersion": "2.4.0", "mode": "chat",
			"conversationId": cid, "requestId": cid, "inputLength": 12,
			"mentionContexts": []any{}, "mentionContextCount": 0, "userId": uid,
		}
	case "schoolseason":
		return map[string]any{
			"eventCode": "chat_request_send", "timestamp": now, "reportDelay": 0,
			"source": "mini_program", "ideName": "wx_app_cloud", "ideType": "WorkBuddy_MP",
			"extName": "workbuddy-mp", "extVersion": "2.4.0", "mode": "chat",
			"activityId": "school_open_day_2026",
			"conversationId": cid, "requestId": cid, "inputLength": 12,
			"mentionContexts": []any{}, "mentionContextCount": 0, "userId": uid,
		}
	default: // chat, glmchat, cat
		modelID, modelName, mode := "deepseek-v4-flash", "DeepSeek V4 Flash", "craft"
		if spec.Kind == "glmchat" || spec.Kind == "cat" {
			modelID, modelName = "glm-5.2", "GLM-5.2"
		}
		if spec.Kind == "cat" {
			mode = "night"
		}
		return map[string]any{
			"eventCode": "chat_request_send", "timestamp": now, "reportDelay": 0,
			"mode": mode, "conversationId": cid, "requestId": cid, "inputLength": 12,
			"requestModelId": modelID, "requestModelName": modelName,
			"isPlan": false, "isAutoExecuteTerminal": false, "isAutoModify": false,
			"codebaseEnable": false, "maxToken": 0, "maxSteps": 0, "temperature": 0, "maxRetries": 0,
			"mentionContexts": []any{}, "knowledgeId": []any{}, "knowledgeName": []any{},
			"codebaseId": "", "mentionContextCount": 0, "command": "",
			"expertId": "", "recommendId": "", "skillId": "", "skillCount": 0, "totalCount": 0,
			"fileUri": "", "presentAt": now, "traceId": "", "rootRequestId": cid,
			"parentConversationId": cid, "agentName": "default", "agentType": "conversation", "userId": uid,
		}
	}
}

func buildExpertEvent(uid, cid, rid string, now int64, id, name, expertType string) map[string]any {
	return map[string]any{
		"eventCode": "expert_actual_use", "timestamp": now, "reportDelay": 0,
		"mode": "CLOUD", "id": id, "name": name, "expertTitle": name,
		"type": "", "expertType": expertType, "source": "builtin", "version": "",
		"cost": 0, "characterCount": 12, "conversationId": cid, "requestId": rid, "messageId": rid,
		"requestModelId": "deepseek-v4-flash", "requestModelName": "DeepSeek V4 Flash", "userId": uid,
	}
}

func deriveTaskID(salt, uid string) string {
	sum := md5.Sum([]byte(salt + ":" + uid))
	return hex.EncodeToString(sum[:])[:32]
}

func buildDesktopChatEvents(uid, nick string) []map[string]any {
	now := time.Now().UnixMilli()
	cid := fmt.Sprintf("wb-run-rm-%d", now)
	rid := fmt.Sprintf("wb-run-rm-req-%d", now)
	mid := fmt.Sprintf("req-%d-user", now)
	events := []map[string]any{
		{
			"eventCode": "agent_task_created", "source": "LOCAL", "name": "working", "task_target": "local",
			"mode": "craft", "requestModelId": "fast-model", "requestModelName": "fast-model",
			"has_repo": false, "repo_type": "none", "workspace_type": "empty",
			"has_connector": false, "connector_types": []any{}, "has_mention": false, "mention_types": []any{},
			"has_template": false, "action": "", "template_name": "",
			"has_expert": false, "expert_id": "", "expert_name": "", "expert_industry_id": "",
			"has_skill": false, "skill_names": []any{},
			"conversationId": cid, "messageId": mid, "buddyId": "", "buddyName": "",
		},
		{
			"eventCode": "chat_message_send", "messageId": mid + "-assistant", "historyCount": 0,
			"isContextTruncated": false, "currentStepCount": 1, "traceId": rid, "rootRequestId": rid,
			"parentConversationId": cid, "agentName": "cli", "agentType": "main",
		},
		{
			"eventCode": "chat_request_send", "inputLength": 24, "traceId": rid, "rootRequestId": rid,
			"parentConversationId": cid, "agentName": "cli", "agentType": "main",
			"codebuddy.session_id": cid, "codebuddy.conversation_request_id": rid,
		},
		{
			"eventCode": "chat_message_response", "messageId": mid + "-assistant", "responseModelId": "fast-model",
			"inputToken": 120, "outputToken": 80, "totalToken": 200,
			"isSuccessful": true, "messageErrorCode": "", "finishReason": "stop", "firstTokenAt": now,
			"traceId": rid, "conversationId": cid, "rootRequestId": rid, "parentConversationId": cid,
			"agentName": "cli", "agentType": "main",
			"codebuddy.session_id": cid, "codebuddy.conversation_request_id": rid,
		},
		{
			"eventCode": "chat_message_status", "messageId": mid + "-assistant", "messageErrorCode": "0",
			"traceId": rid, "rootRequestId": rid, "parentConversationId": cid,
			"agentName": "cli", "agentType": "main",
		},
		{
			"eventCode": "chat_request_response", "mode": "craft", "toolCallCount": 0,
			"inputToken": 120, "outputToken": 80, "totalToken": 200,
			"isSuccessful": true, "messageErrorCode": "", "finishReason": "stop",
			"rootRequestId": rid, "parentConversationId": cid,
		},
	}
	return injectDesktopFingerprint(events, uid, nick, now)
}

func buildDesktopBuddy5Events(uid, nick, buddyID, buddyName string) []map[string]any {
	now := time.Now().UnixMilli()
	events := []map[string]any{
		{"eventCode": "buddyapp_discover_click", "mode": "LOCAL", "buddyId": buddyID, "buddyName": buddyName},
		{"eventCode": "buddyapp_show", "mode": "LOCAL", "buddyId": buddyID, "buddyName": buddyName, "elementId": buddyID, "elementName": buddyName, "position": 2},
		{"eventCode": "buddyapp_enter_click", "mode": "LOCAL", "buddyId": buddyID, "buddyName": buddyName, "elementId": buddyID, "elementName": buddyName, "position": 2, "isFirstPage": "1"},
		{"eventCode": "buddyapp_auth_confirm_click", "mode": "LOCAL", "buddyId": buddyID, "buddyName": buddyName, "elementId": buddyID, "elementName": buddyName},
		{"eventCode": "buddyapp_bindaccount_skip_click", "mode": "LOCAL", "buddyId": buddyID, "buddyName": buddyName, "elementId": buddyID, "elementName": buddyName},
	}
	return injectDesktopFingerprint(events, uid, nick, now)
}

func injectDesktopFingerprint(events []map[string]any, uid, nick string, now int64) []map[string]any {
	fp := map[string]any{
		"timezone": "Asia/Shanghai", "reportDelay": 2000,
		"userId": uid, "username": nick, "userNickname": nick,
		"product": "SaaS", "releaseDate": 1789036585355,
		"commit": "5f9692923c93033111c51ad7b003eb80204a9b75",
		"ideName": "WorkBuddy", "ideType": "WorkBuddy", "ideVersion": "5.5.6",
		"machineId": deriveTaskID("machine", uid), "sessionId": deriveTaskID("session", uid),
		"extName": "workbuddy-desktop", "extVersion": "5.5.6",
		"os": "win32", "arch": "x64", "osVersion": "10.0.26220",
		"cpuCores": 20, "memorySize": 24, "timestamp": now, "presentAt": now,
	}
	for _, ev := range events {
		for k, v := range fp {
			ev[k] = v
		}
	}
	return events
}

func (c *Client) reportDesktopEvents(ctx context.Context, a *auth.Auth, events []map[string]any) error {
	raw, err := json.Marshal(events)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatBase(a)+reportPath, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	c.BillingHeaders(req, a)
	req.Header.Set("User-Agent", desktopUA)
	req.Header.Set("X-Product", "SaaS")
	_, err = c.doJSON(req)
	return err
}

func (c *Client) reportWebLibraryEvent(ctx context.Context, a *auth.Auth, uid string) error {
	now := time.Now().UnixMilli()
	wb := c.webBase()
	ev := map[string]any{
		"eventCode": "web_element_click", "timestamp": now, "reportDelay": 0,
		"pageURL": libraryDocURL, "elementId": "library_doc_intro_click", "elementName": "WorkBuddy资料库介绍",
		"os": "Win32", "arch": "", "osVersion": "10.0", "userAgent": webBrowserUA,
		"machineId": deriveTaskID("webmachine", uid), "userId": uid, "userNickname": a.Nickname,
	}
	raw, err := json.Marshal([]map[string]any{ev})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, wb+reportPath, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	c.BillingHeaders(req, a)
	req.Header.Set("x-client-platform", "web")
	req.Header.Set("Origin", wb)
	req.Header.Set("Referer", libraryDocURL)
	req.Header.Set("User-Agent", webBrowserUA)
	req.Header.Set("X-Domain", wb)
	_, err = c.doJSON(req)
	return err
}

func withinCSTNightWindow(t time.Time) bool {
	h := t.In(time.FixedZone("CST", 8*3600)).Hour()
	return h >= 23 || h < 8
}

func sleepTaskGap(ctx context.Context) {
	if taskActionGap <= 0 {
		return
	}
	tm := time.NewTimer(taskActionGap)
	defer tm.Stop()
	select {
	case <-ctx.Done():
	case <-tm.C:
	}
}
