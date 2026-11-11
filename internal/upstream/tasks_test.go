package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"workbuddy2api/internal/auth"
)

func TestRunGrowthTasksNativeFlow(t *testing.T) {
	origGap, origSettle := taskActionGap, taskClaimSettleDelay
	taskActionGap, taskClaimSettleDelay = 0, 0
	t.Cleanup(func() {
		taskActionGap, taskClaimSettleDelay = origGap, origSettle
	})

	var reportCount, acceptCount, firstCalls int32
	var claimedCodes []string
	var seenSkillShare, seenLighthouse, seenTemplate, seenBuddy bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == growthTasksListPath && r.Method == http.MethodGet:
			if r.Header.Get("X-Client-Platform") == "miniprogram" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"code": 0,
					"data": map[string]any{
						"tasks": []map[string]any{
							{"task_code": "Sequential_Tasks_1", "accept_status": "not_accepted", "progress": map[string]int{"current": 0, "target": 1}},
						},
					},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{
					"tasks": []map[string]any{
						{"task_code": "expert_use_maidehao", "accept_status": "not_accepted", "progress": map[string]int{"current": 0, "target": 1}},
						{"task_code": "Expert_lighthouse", "accept_status": "accepted", "progress": map[string]int{"current": 0, "target": 1}},
						{"task_code": "template_5", "accept_status": "accepted", "progress": map[string]int{"current": 0, "target": 5}},
						{"task_code": "Buddy_App", "accept_status": "accepted", "progress": map[string]int{"current": 0, "target": 1}},
						{"task_code": "skill_share", "accept_status": "not_accepted", "progress": map[string]int{"current": 0, "target": 1}},
						{"task_code": "RichMeow_Chat", "accept_status": "accepted", "progress": map[string]int{"current": 0, "target": 1}},
						{"task_code": "Library_read", "accept_status": "accepted", "progress": map[string]int{"current": 0, "target": 1}},
						{"task_code": "first_buddy", "accept_status": "not_accepted", "progress": map[string]int{"current": 0, "target": 1}},
						{"task_code": "chat_5", "accept_status": "claimed", "progress": map[string]int{"current": 5, "target": 5}},
						{"task_code": "wb_wechat_oa_subscribe_task", "accept_status": "not_accepted", "progress": map[string]int{"current": 0, "target": 1}},
					},
				},
			})
		case r.URL.Path == "/console/as/support/scenes":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{
					"scenes": []map[string]any{
						{"id": 0, "name": "幻灯片"}, {"id": 2, "name": "视频"}, {"id": 4, "name": "研究"}, {"id": 6, "name": "文档"}, {"id": 8, "name": "数据"},
					},
				},
			})
		case r.URL.Path == marketExpertListPath:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{
					"experts": []map[string]any{
						{"expert_id": "ex_2cvvUZQhDyeJ", "display_name_zh": "腾讯轻量云专家", "categories": []string{"13-TencentZone"}, "expert_type": "agent", "version": "1.1.1"},
					},
				},
			})
		case r.URL.Path == marketSkillListPath:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{
					"skills": []map[string]any{{"skill_id": "skill_2096525080079265792", "name": "pptx"}},
				},
			})
		case r.URL.Path == growthTasksAcceptPath:
			atomic.AddInt32(&acceptCount, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{}})
		case r.URL.Path == reportPath:
			atomic.AddInt32(&reportCount, 1)
			raw, _ := io.ReadAll(r.Body)
			s := string(raw)
			if strings.Contains(s, `"skill_share_channel_action"`) && strings.Contains(r.Header.Get("X-Domain"), srvURLHost(r)) {
				seenSkillShare = true
			}
			if strings.Contains(s, `"ex_2cvvUZQhDyeJ"`) && strings.Contains(s, `"13-TencentZone"`) {
				seenLighthouse = true
			}
			if strings.Contains(s, `"agent_task_created_with_template"`) {
				seenTemplate = true
			}
			if strings.Contains(s, `"buddyapp_enter_click"`) && strings.Contains(s, `"5.9.3"`) {
				seenBuddy = true
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{}})
		case r.URL.Path == buddyAgreementPath:
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{}})
		case r.URL.Path == buddyFirstPath:
			if atomic.AddInt32(&firstCalls, 1) == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 400, "msg": "first_buddy task not completed yet"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"buddy": map[string]any{"id": 1}}})
		case strings.HasPrefix(r.URL.Path, "/activity/growth/tasks/") && strings.HasSuffix(r.URL.Path, "/claim"):
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			code := parts[len(parts)-2]
			if code == "RichMeow_Chat" && r.Header.Get("x-client-platform") != "web" {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 400, "msg": "use web"})
				return
			}
			claimedCodes = append(claimedCodes, code)
			if code == "first_buddy" {
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"already_claimed": true}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"credit": 50, "energy": 5}})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cl := &Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	res, err := cl.RunGrowthTasks(context.Background(), &auth.Auth{UID: "u_native_1", AccessToken: "at", Domain: "www.codebuddy.cn"})
	if err != nil {
		t.Fatalf("RunGrowthTasks err=%v", err)
	}
	if res.Failed != 0 {
		t.Errorf("failed=%d logs=%v", res.Failed, res.Logs)
	}
	if res.ClaimedCredits != 400 { // 8 claimed new * 50
		t.Errorf("credits=%d want 400 (logs=%v)", res.ClaimedCredits, res.Logs)
	}
	if !seenSkillShare || !seenLighthouse || !seenTemplate || !seenBuddy {
		t.Errorf("missing expected events: skillShare=%v lighthouse=%v template=%v buddy=%v",
			seenSkillShare, seenLighthouse, seenTemplate, seenBuddy)
	}
	if res.Already != 2 { // chat_5 + first_buddy(already_claimed)
		t.Errorf("already=%d want 2", res.Already)
	}
	if res.Skipped != 1 { // wb_wechat_oa_subscribe_task
		t.Errorf("skipped=%d want 1", res.Skipped)
	}
	if atomic.LoadInt32(&firstCalls) != 2 {
		t.Errorf("firstCalls=%d want 2", firstCalls)
	}
	if len(claimedCodes) != 9 {
		t.Errorf("claimedCodes=%v want 9", claimedCodes)
	}
}

func srvURLHost(r *http.Request) string {
	return r.Host
}

func TestRunGrowthTasksRejectsGlobal(t *testing.T) {
	cl := New()
	_, err := cl.RunGrowthTasks(context.Background(), &auth.Auth{
		UID:         "g1",
		AccessToken: "at",
		Domain:      "www.workbuddy.ai",
	})
	if err == nil {
		t.Fatal("expected error for global account")
	}
}
