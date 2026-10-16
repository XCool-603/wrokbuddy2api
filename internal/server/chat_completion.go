package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/prompt"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/usermgr"
)

// rewriteModel 把 outbound chat body 的 model 字段替换为 bare（保留其余字段原样）。
// 仅当 bare != 原 model 时由 chatCompletions 调用；body 不可解析时原样返回（不二次错误化）。
func rewriteModel(body []byte, bare string) []byte {
	if len(body) == 0 || bare == "" {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if cur, ok := obj["model"].(string); !ok || cur == bare {
		return body
	}
	obj["model"] = bare
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	// 请求体无大小上限（max_body_mb 已移除）：完整读入，超限类问题交由上游自然返回
	// 错误（其响应经既有错误分类链路透出，信息量更大）。#41 的截断防御语义保留在
	// 读错误路径——移除预拦截后，截断只可能来自客户端自己断流，读 body 出错就地 400，
	// 不把半截 JSON 喂上游 unmarshal 报 unexpected EOF 冤枉罚号。
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	// 调试开关：设置 WB2A_DUMP_REQ 即把上游侧收到的原始请求体落盘，供离线二分定位指纹命中行。
	// 仅在排查上游指纹拦截时开启；不设置时零开销、不落盘。
	// 只落"大请求"（≥4MB 固定阈值，原 max_body_mb/2 语义的接替）：小探针
	// （{"input":"hi"} 之类）会覆盖掉真正要看的对话请求。
	if os.Getenv("WB2A_DUMP_REQ") != "" && len(body) >= dumpReqMinBytes {
		dumpDir := "./data"
		if h.cfg.StateFile != "" {
			dumpDir = filepath.Dir(h.cfg.StateFile)
		}
		_ = os.MkdirAll(dumpDir, 0755)
		if err := os.WriteFile(filepath.Join(dumpDir, "last_request.json"), body, 0o600); err != nil {
			log.Printf("ERR: [server] dump req: %v", err)
		}
	}
	var peek struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)

	realm, bareModel := h.resolveModelWithAliases(peek.Model)
	if bareModel == "auto" || bareModel == "default-model" || bareModel == "" {
		bareModel = "deepseek-v4.1-flash"
	}
	fullModel := bareModel
	if realm != "" {
		fullModel = realm + ":" + bareModel
	}

	// 若客户端请求的是 dsh 虚拟智能体模型，直接转发至本地或容器内的 dsh 服务端口
	if strings.EqualFold(bareModel, "dsh") || strings.EqualFold(peek.Model, "dsh") || strings.EqualFold(bareModel, "deepseek-harness") {
		h.proxyToDsh(w, r, body)
		return
	}

	// 请求级统计：出口即打一行表格日志（任何路径都会走到）。
	st := newChatStat(time.Now(), body, peek.Stream)
	defer st.done()

	tried := map[string]bool{}
	var lastErr error

	// 获取当前请求用户（多租户/角色隔离）
	reqOwner := ""
	if u, ok := r.Context().Value(userContextKey).(*usermgr.User); ok && u != nil {
		if u.Role != usermgr.RoleAdmin {
			// 普通用户：带上 ID 和 Username 组合键，兼容凭证以 ID 或 Username 绑定的私有账号
			reqOwner = u.ID + "|" + u.Username
		}
	}

	// 会话粘性：从请求体提取会话键并解析绑定号（找不到/无效则 stickyUID 为空，走普通轮换）。
	// 按模型解析：同一个会话可能换模型，绑定号若在当前模型上被 6004 限额（对其他模型
	// 仍可用），必须重分配——否则会被钉在这个号上反复失败。
	// 提取与下方会话头族的聚合键共用同一结果，故**不受粘性开关影响**：粘性未启用
	// （Session==nil）时聚合键仍应是会话级，而不是退化成轮级。
	sessKey := session.ExtractKey(body)
	// stickyKey 是**粘性专用**键，与 sessKey（会话头族聚合用）分开：
	// sessKey 为空时（OpenAI 兼容客户端——dsh / Codex 等既无 conversationId 也无
	// metadata）用首条 user 消息派生会话级 fallback 键，使粘性仍能生效。
	// 不能直接改 sessKey：那会连带改变上游头族轮级复合键（sessKey 入键）的聚合
	// 语义，属于另一条链路的契约。
	stickyKey := sessKey
	if stickyKey == "" {
		stickyKey = session.StickyFallbackKey(body)
	}
	// 多租户隔离：会话粘性键加上用户身份前缀，防止不同用户相同 conversationId 互相绑定
	if stickyKey != "" && reqOwner != "" {
		stickyKey = reqOwner + ":" + stickyKey
	}
	stickyUID := ""
	if h.cfg.Session != nil && stickyKey != "" {
		// 传给 ResolveForModel 的是**完整**模型名（peek.Model，含 realm 前缀）。
		// 粘性命中校验走 injected AvailableForModel 闭包 → 闭包内部 resolveModel 剥前缀
		// 得 realm+bare，再按 realm 过滤可用集合。若传已剥前缀的 bareModel，闭包对裸名
		// 恒剥出 realm=cn，跨 realm 粘性会话会被错误钉回 CN 集合；完整前缀才能让
		// 闭包正确过滤到 global 集合（见 cmd/server/wiring.go realmAwareAvailableForModel）。
		// 模型名也参与成本账本与选号过滤，不能用 "-" 占位污染模型键。
		if uid, ok := h.cfg.Session.ResolveForModel(stickyKey, fullModel); ok {
			stickyUID = uid
		}
	}

	// 轮级聚合键：按 body 里最后一条 user 消息派生（同轮内所有上游调用同键，
	// 换 user 消息换键）。#170 起带会话键的客户端也统一走轮级（与官方桌面 CLI 的
	// X-Conversation-Request-ID 轮级语义对齐），故不再限 sessKey=="" 才计算；
	// sessKey 由调用侧以复合键方式入键（防不同会话同轮文本互撞）。
	// 必须在下方 prompt.Rewrite / rewriteModel 之前取——改写会动 messages 内容。
	turnKey := session.TurnKey(body)

	// gateway_hint 判定所需的请求形态（image_url part）：在改写前取（与 turnKey
	// 同理）。11133「模型不支持图片」指向的前提。
	reqHasImage := hasImagePart(body)

	// 在途租约：成功选中即占名额；函数出口（含成功 return 与 panic）统一释放。
	var heldUID string
	var endInFlight func()
	defer func() {
		if endInFlight != nil {
			endInFlight()
		}
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
		}
	}()
	releaseHeld := func() {
		if endInFlight != nil {
			endInFlight()
			endInFlight = nil
		}
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
			heldUID = ""
		}
	}
	// unbindSticky 解绑当前会话粘性号（stickyUID 非空时）。供「粘性号不可用/被抢」与 fail 共用。
	// 幂等：stickyUID 已空则空操作；不会误解绑其他轮的绑定。仅当 Session != nil 时 stickyUID 才会非空。
	unbindSticky := func() {
		if stickyUID != "" {
			h.cfg.Session.Unbind(stickyKey)
			stickyUID = ""
		}
	}
	// fail 在轮转失败分支统一：释放租约 + 若失败号正是粘性号则解绑（下次请求重新分配）。
	fail := func(uid string) {
		releaseHeld()
		if stickyUID != "" && uid == stickyUID {
			unbindSticky()
		}
	}

	// 系统提示词改写（出站前、轮转前；每个请求一次）。
	//   - custom：用自有提示词替换客户端 system/developer（从源头消灭 system 指纹误报）。
	//   - append：开头连续 system/developer 块后插自有提示词，既有消息逐字不动
	//     （客户端项目规范/工具约定与网关提示词并用，issue #129）。
	//   - passthrough + 降级期：换 Degraded 中性提示词直达，不再先撞 400。
	//   - passthrough / append 非降级期：透传客户端原始 system（append 则再插一条网关 system）。
	// 降级裁决：append 在降级期退化为 replace（Rewrite(Degraded)）——append 带
	// 指纹原文重试是确定性再撞墙，replace 是一次性最小抢救（issue #129 设计 §4）。
	degradedApplied := false
	if h.cfg.PromptMode == "custom" && h.cfg.PromptText != "" {
		body = prompt.Rewrite(body, h.cfg.PromptText)
	} else if h.cfg.PromptMode == "append" && h.cfg.PromptText != "" && !h.degrade.Active() {
		body = prompt.Append(body, h.cfg.PromptText)
	} else if (h.cfg.PromptMode == "passthrough" || h.cfg.PromptMode == "append") && h.degrade.Active() {
		body = prompt.Rewrite(body, prompt.Degraded)
		degradedApplied = true
	}

	// outbound model 名重写为 bareModel（D6）：realm 前缀是网关侧路由协议，
	// 上游不认前缀（global 账号也请求裸模型名）。裸名时 bareModel==peek.Model 恒等。
	if bareModel != peek.Model {
		body = rewriteModel(body, bareModel)
	}

	// 会话头族（issue #35 / #170）：后台按 X-Conversation-Request-ID 聚合请求，官方
	// 客户端一次 user send 内所有 tool call/重试/换号复用同一个 ID。**统一轮级**
	// （对齐官方桌面 CLI：TraceStartHook 每次 USER_PROMPT_SUBMIT 清空重生成
	// conversationRequestId，同轮内复用、跨轮必换；官方云链路 lfConvReqId 的会话级
	// 是服务端指令，走本网关的客户端不属于该形态）。此处**轮转循环外**生成一次，
	// 循环内每次出站原样复用 → 换号/重试/降级全部同 ID，后台不再碎片化（此前网关
	// 一个都不发，上游按 HTTP 请求逐条记账，同一对话几十上百个 RequestID）。
	//   - conversationID：body 提取（透传客户端原值，缺省空串——不伪造，见
	//     ResolveConversationID；官方后台不校验一致，空会话则不建立聚合键）；
	//   - conversationRequestID：入站 X-Conversation-Request-ID 透传优先（客户端已
	//     有自己的对话轮 ID 则以客户端为准）。派生分两态：
	//     * turnKey 非空（有末条 user 消息）→ 带会话键客户端走 TurnRequestID(
	//       sessKey+":"+turnKey) 复合键（会话段入键保证不同会话同轮文本不互撞，
	//       轮级粒度对齐官方 CLI）；无会话键客户端走既有 TurnRequestID(turnKey)
	//       纯轮级键（存量会话键值零漂移）。
	//     * turnKey 为空（残留空态：无 user 消息/无可签名内容）→ sessKey 非空时
	//       回落 RequestIDForKey(sessKey)（会话级兜底，好于请求级随机）；sessKey
	//       也空走 NewMessageID 请求级（TurnRequestID 空键行为）——轮转内捕获
	//   - messageID 在 ChatHeaders 内每条消息生成（消息级独立，无需外部可见）。
	convID := session.ResolveConversationID(body)
	if convID == "" {
		convID = r.Header.Get("X-Conversation-ID")
	}
	if convID == "" {
		if stickyKey != "" {
			convID = session.UUIDForKey(stickyKey)
		} else {
			convID = session.NewUUID()
		}
	}
	chatMeta := upstream.ChatMeta{ConversationID: convID}
	if v := r.Header.Get("X-Conversation-Request-ID"); v != "" {
		chatMeta.ConversationRequestID = v
	} else if turnKey != "" && sessKey != "" {
		// 轮级复合键：sessKey 入键防跨会话同轮文本互撞。
		chatMeta.ConversationRequestID = session.TurnRequestID(sessKey + ":" + turnKey)
	} else if turnKey != "" {
		// 无会话键客户端：纯轮级键（既有兜底语义不变，存量会话键值零漂移）。
		chatMeta.ConversationRequestID = session.TurnRequestID(turnKey)
	} else if sessKey != "" {
		// 残留空态兜底：无轮可聚合时维持会话级聚合（同会话恒同值）。
		chatMeta.ConversationRequestID = session.RequestIDForKey(sessKey)
	} else {
		// 无会话键也无轮级键：请求级随机（轮转内捕获一次即共享）。
		chatMeta.ConversationRequestID = session.TurnRequestID("")
	}
	chatMeta.TraceID = r.Header.Get("X-Trace-ID")
	streamHeadersSent := false

	for i := 0; i < h.cfg.MaxRotate; i++ {
		// 选号：粘性号优先（PickByUIDForModel 已校验该模型可用性 + 在途未满），否则普通轮换。
		var acct *auth.Auth
		if stickyUID != "" {
			acct = h.cfg.Pool.PickByUIDForModel(stickyUID, bareModel)
			if acct == nil || (realm != "" && acct.Realm() != realm) {
				// 粘性号在当前模型不可用（冷却/占满/该模型被 6004 限额）或 realm 不符 → 解绑。
				unbindSticky()
			} else if reqOwner != "" && reqOwner != "admin" && reqOwner != "u_admin" {
				// 普通用户隔离校验：若粘性号不属于该普通用户且非公共号，解绑并重新轮换
				if !pool.IsOwnerMatch(acct.OwnerValue(), reqOwner) {
					unbindSticky()
					acct = nil
				}
			}
		}
		if acct == nil {
			// 模型感知 + realm 感知 + 用户隔离选号：模型非空时启用 6004 模型级冷却豁免
			// （healthyForModel），realm 谓词过滤跨域账号。
			acct = h.cfg.Pool.PickExcludingForRealmAndOwner(tried, bareModel, realm, reqOwner)
			// 双域动态容灾互补：当首选 realm 无可用健康账号时（全在冷却中或占满），
			// 自动尝试在另一个域选号（global <-> cn），实现跨域容灾互补与高可用。
			if acct == nil {
				totalInRealm, _, _, _, _ := h.cfg.Pool.CountsDetailedForRealm(realm)
				hasExplicitPrefix := strings.HasPrefix(peek.Model, "cn:") || strings.HasPrefix(peek.Model, "global:")
				// 若未带显式域前缀，或者该域本身配置了账号但当前全部不可用（限流/冷却/占满），
				// 则动态故障转移到另一个域（如 cn <-> global），实现双域容灾互补。
				// 仅在显式指定了域前缀且池中根本没有任何该域账号时，严格返回 503（测试 TestChatRealmGlobalAbortedNoGlobalAccount 保险契约）。
				if !hasExplicitPrefix || totalInRealm > 0 {
					otherRealm := "global"
					if realm == "global" {
						otherRealm = "cn"
					}
					// 尝试在另一个域选号
					acct = h.cfg.Pool.PickExcludingForRealmAndOwner(tried, bareModel, otherRealm, reqOwner)
					if acct == nil {
						// 兜底：全池任意可用健康号（仍受 reqOwner 隔离限制）
						acct = h.cfg.Pool.PickExcludingForRealmAndOwner(tried, bareModel, "", reqOwner)
					}
					if acct != nil {
						log.Printf("INFO: [server] realm failover %s -> %s for model=%s (acct=%s, owner=%s)",
							realm, acct.Realm(), bareModel, acct.UID, acct.OwnerValue())
					}
				}
			}
		}
		if acct == nil {
			// 并发流量排队容灾：若池中有健康账号但当前全部在途满载（常见于多项目同时用多角色流水线），
			// 在轮转第 1 轮内排队等待名额释放（最长等待 queue_timeout 秒，微轮询 60ms），一旦有在途请求完成释放租约，
			// 立即接管执行，绝不在突发高并发时粗暴返回 503 打崩客户端流水线！流式长排队期间每 3 秒下发 SSE ping 注释心跳防客户端超时。
			if i == 0 && (h.cfg.Pool.HasInFlightBusy(bareModel, realm, reqOwner) || h.cfg.Pool.HasInFlightBusy(bareModel, "", reqOwner)) {
				waitStart := time.Now()
				waitTimeout := h.cfg.QueueTimeout
				if waitTimeout <= 0 {
					waitTimeout = 60 * time.Second
				}
				lastPing := waitStart
				flusher, _ := w.(http.Flusher)
				for time.Since(waitStart) < waitTimeout {
					select {
					case <-r.Context().Done():
						break
					case <-time.After(60 * time.Millisecond):
					}
					if r.Context().Err() != nil {
						break
					}
					// 流式请求若排队等待且支持 flush，周期性（3s）发送 SSE 注释心跳防客户端断连超时
					if peek.Stream && flusher != nil && time.Since(lastPing) >= 3*time.Second {
						if !streamHeadersSent {
							w.Header().Set("Content-Type", "text/event-stream")
							w.Header().Set("Cache-Control", "no-cache")
							w.Header().Set("Connection", "keep-alive")
							w.Header().Set("X-Accel-Buffering", "no")
							w.WriteHeader(http.StatusOK)
							streamHeadersSent = true
						}
						_, _ = io.WriteString(w, ": ping queue wait slot...\n\n")
						flusher.Flush()
						lastPing = time.Now()
					}
					// 重试选号
					acct = h.cfg.Pool.PickExcludingForRealmAndOwner(tried, bareModel, realm, reqOwner)
					if acct == nil {
						otherRealm := "global"
						if realm == "global" {
							otherRealm = "cn"
						}
						acct = h.cfg.Pool.PickExcludingForRealmAndOwner(tried, bareModel, otherRealm, reqOwner)
						if acct == nil {
							acct = h.cfg.Pool.PickExcludingForRealmAndOwner(tried, bareModel, "", reqOwner)
						}
					}
					if acct != nil {
						log.Printf("INFO: [server] concurrency queue slot acquired after %v: acct=%s",
							time.Since(waitStart).Round(time.Millisecond), logfmt.Label(acct.UID, acct.Nickname))
						break
					}
					if !h.cfg.Pool.HasInFlightBusy(bareModel, realm, reqOwner) && !h.cfg.Pool.HasInFlightBusy(bareModel, "", reqOwner) {
						// 已无在途忙碌账号，不必继续等待
						break
					}
				}
			}
		}
		if acct == nil {
			diagRealm := realm
			totalInRealm, _, _, _, _ := h.cfg.Pool.CountsDetailedForRealm(realm)
			if totalInRealm == 0 {
				otherRealm := "global"
				if realm == "global" {
					otherRealm = "cn"
				}
				totalOther, _, _, _, _ := h.cfg.Pool.CountsDetailedForRealm(otherRealm)
				if totalOther > 0 {
					diagRealm = otherRealm
				}
			}
			diag := h.cfg.Pool.DiagnosticsForModelRealmAndOwner(bareModel, diagRealm, reqOwner, tried)
			log.Printf("WARN: [server] chat pick failed (turn=%d/%d): model=%s (realm=%s bare=%s diagRealm=%s) owner=%q -> %s",
				i+1, h.cfg.MaxRotate, peek.Model, realm, bareModel, diagRealm, reqOwner, diag.Summary)
			st.errDetail = diag.Summary
			if diag.IsRateLimited {
				st.status = http.StatusTooManyRequests
				if h.alertMgr != nil {
					h.alertMgr.NotifyAllCooling(realm, bareModel)
				}
				if !diag.EarliestReset.IsZero() {
					delta := int(time.Until(diag.EarliestReset).Seconds())
					if delta > 0 {
						w.Header().Set("Retry-After", strconv.Itoa(delta))
					}
				}
			} else {
				st.status = http.StatusServiceUnavailable
			}
			break
		}
		st.uid = acct.UID
		// 同步昵称：请求流水行只写 uid8 时无法直观看是哪一号，昵称随本次选号带入日志行。
		st.nick = acct.Nickname
		tried[acct.UID] = true

		// 占用在途名额：Pick 已跳过满额账号，此处 CAS 兜底并发抢名额的竞态。
		if !h.cfg.Pool.Acquire(acct.UID) {
			// 若被抢的正是粘性号，立即解绑并回落普通轮换，避免下一轮仍撞同一个
			// 满载粘性号再浪费一次粘性命中往返（语义与 fail()/粘性命中-nil 的解绑一致）。
			if stickyUID != "" && acct.UID == stickyUID {
				unbindSticky()
			}
			if !rotateBackoff(i, r.Context()) {
				// 客户端已断连：换号重试无意义，终止轮转走末端错误透传。
				break
			}
			continue // 最后一个名额被并发抢走 → 换号
		}
		heldUID = acct.UID
		if endInFlight != nil {
			endInFlight()
			endInFlight = nil
		}
		reqID := fmt.Sprintf("req-%d", time.Now().UnixNano())
		callerIP := r.RemoteAddr
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			callerIP = strings.TrimSpace(strings.Split(xff, ",")[0])
		}
		endInFlight = h.inFlightTracker.Begin(reqID, bareModel, acct.UID, acct.Nickname, peek.Stream, callerIP, reqOwner)
		log.Printf("INFO: [server] chat dispatch (turn=%d/%d): acct=%s realm=%s model=%s owner=%q",
			i+1, h.cfg.MaxRotate, logfmt.Label(acct.UID, acct.Nickname), acct.Realm(), bareModel, reqOwner)

		// token 临近过期 → 先 refresh（失败冷却换号）
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
				// 若仅仅是因为账号没有配置 refreshToken，当前 accessToken 仍可能有效，直接放行继续请求上游
				if strings.Contains(err.Error(), "no refreshToken") {
					log.Printf("INFO: [server] acct=%s has no refreshToken, continuing with existing accessToken", logfmt.Label(acct.UID, acct.Nickname))
				} else {
					lastErr = err
					log.Printf("WARN: [server] token refresh failed: acct=%s err=%v", logfmt.Label(acct.UID, acct.Nickname), err)
					st.errDetail = fmt.Sprintf("token 刷新失败: %v", err)
					var ue *upstream.Error
					if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
						// 12153 一次失败不杀号（临时触发会误杀）：与 scheduler keepalive/checkin
						// 同口径走连续计数，达到 sessionDeadThreshold 才禁用。
						h.cfg.Pool.NoteSessionDead(acct.UID)
					} else {
						h.cfg.Pool.NoteError(acct.UID)
					}
					fail(acct.UID)
					if !rotateBackoff(i, r.Context()) {
						break // ctx 取消：终止轮转（refresh 失败换号退避，WAF P0-2）
					}
					continue
				}
			} else {
				acct.BackfillRealm() // 老 auth 空 realm → 落盘前补标识（幂等：已有不动）
				if err := acct.SaveAtomic(); err != nil {
					// 刷新成功但落盘失败：下次启动会用旧 token，必须暴露
					log.Printf("ERR: [server] chat refresh acct=%s: save auth failed: %v", logfmt.Label(acct.UID, acct.Nickname), err)
				}
			}
		}

		// 客户端 IP 透传（仅 PassthroughIP 开启）：按请求取首段作为参数传入 ChatStream，
		// 不再读写共享字段——并发请求各自携带独立 IP，互不串扰（issue：ClientIP 竞态）。
		var clientIP string
		if h.cfg.Upstream.PassthroughIP {
			clientIP = upstream.ExtractClientIP(r)
		}
		// 传 r.Context()：客户端断连/请求取消立即中断在途上游调用并释放租约，
		// 不再让"幽灵请求"占满账号在途名额直到 IdleTimeout。
		rc, status, respBody, terr := h.cfg.Upstream.ChatStreamContext(r.Context(), acct, body, clientIP, chatMeta)
		// 分类信封一次成型：upstream 已在错误路径返回 *upstream.Error（Kind +
		// Retry-After 头解析，见 ChatStreamContext 注释）。传输层错误（非 *Error）走
		// 抖动换号分支；防御分支（terr 为 nil 但 status>=400，如 ErrNone 兜底）回落
		// 本地 Classify，双保险不改变语义。
		var uerr *upstream.Error
		if errors.As(terr, &uerr) {
			status = uerr.Status
		}
		if uerr == nil && terr != nil {
			// 若为客户端自身断开连接或超时取消（context.Canceled / DeadlineExceeded），属于客户端主动行为，
			// 不作为上游或账号故障，绝不调用 NoteFailures 冤枉惩罚账号
			if errors.Is(terr, context.Canceled) || errors.Is(terr, context.DeadlineExceeded) || r.Context().Err() != nil {
				log.Printf("INFO: [server] client canceled request: acct=%s model=%s err=%v", logfmt.Label(acct.UID, acct.Nickname), bareModel, terr)
				st.status = 499
				st.errDetail = "客户端断开连接或超时取消"
				fail(acct.UID)
				return
			}
			// 网络层抖动：只换号，不喂熔断计数（传输层错误对连续失败连坐熔断过于严苛）。
			// 连败兜底（issue #114）：喂连败计数——连不上上游是「不知道原因的失败」，
			// 连败 N 次临时出池，单次/偶发不罚（NoteFailures 内部达阈才动作）。
			// 上游 client 已打 transport error 日志。
			log.Printf("WARN: [server] upstream transport error: acct=%s model=%s err=%v", logfmt.Label(acct.UID, acct.Nickname), bareModel, terr)
			st.status = http.StatusServiceUnavailable
			st.errDetail = fmt.Sprintf("上游连接异常: %v", terr)
			lastErr = terr
			h.cfg.Pool.NoteFailures(acct.UID)
			fail(acct.UID)
			if !rotateBackoff(i, r.Context()) {
				break // ctx 取消：终止轮转（传输层错误换号退避，WAF P0-2）
			}
			continue
		}
		if status >= 400 {
			st.status = status
			var kind upstream.ErrKind
			if uerr != nil {
				kind = uerr.Kind
			} else {
				kind = upstream.Classify(status, string(respBody))
				uerr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
			}
			if status == http.StatusUnauthorized || kind == upstream.ErrSessionDead {
				// 若为 401 鉴权失效，且账号配有 refreshToken，先尝试静默刷新一次并救活账号
				if acct.RefreshToken != "" {
					if refErr := h.cfg.Upstream.RefreshToken(acct); refErr == nil {
						_ = acct.SaveAtomic()
						log.Printf("INFO: [server] acct=%s recovered from 401 via token refresh", logfmt.Label(acct.UID, acct.Nickname))
						h.cfg.Pool.ClearSessionDead(acct.UID)
						fail(acct.UID)
						if !rotateBackoff(i, r.Context()) {
							break
						}
						continue
					}
				}
				// 无 refreshToken 或刷新失败：该账号凭证已彻底失效，立即禁用，防继续污染池子与报错
				log.Printf("WARN: [server] acct=%s session dead (401 unauthorized), disabling account", logfmt.Label(acct.UID, acct.Nickname))
				h.cfg.Pool.Disable(acct.UID, "401 unauthorized session dead")
				fail(acct.UID)
				if !rotateBackoff(i, r.Context()) {
					break
				}
				continue
			}
			// 内容拦截误报（passthrough/append 模式首遇）：判定为 system 指纹误报，
			// 触发降级到次日 00:00 CST，换 Degraded 中性提示词同请求内重试（append
			// 降级重试同样退化为 replace——原文在场只会确定性再撞 400）。
			// 第二次仍被拦（用户内容本身触发审核）→ 回内容防火墙错误（见下分支）。
			// 内容问题非账号问题：applyErrorPolicy 不罚账号（见 ErrContentBlocked 分支）。
			if kind == upstream.ErrContentBlocked && (h.cfg.PromptMode == "passthrough" || h.cfg.PromptMode == "append") && !degradedApplied {
				h.degrade.Trigger()
				body = prompt.Rewrite(body, prompt.Degraded)
				degradedApplied = true
				delete(tried, acct.UID) // 单账号池也能拿到重试机会（降级重试占一次名额）
				releaseHeld()
				log.Printf("WARN: [server] content-blocked (likely fingerprint false positive) -> degraded prompt retry")
				continue
			}
			if kind == upstream.ErrContentBlocked {
				// 内容命中网关内容防火墙：立即回客户端，**不轮转**——换任何账号都会撞同一
				// 审核，轮转纯属浪费时间。不罚账号（ErrContentBlocked 分支无冷却/熔断/NoteError）。
				// error-passthrough：message 装上游 body 原文（code/msg/requestId 原样，
				// 任务书授权上游错误码/账号语义对客户端可见），不再改写成网关固定文案。
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					// 空 body 兜底：无上游原文可透传，保留可读分类文案（不编造原文）。
					msg = "content blocked by upstream content firewall"
				}
				log.Printf("WARN: [server] upstream content blocked: acct=%s model=%s msg=%s", logfmt.Label(acct.UID, acct.Nickname), bareModel, logfmt.Truncate(msg, 150))
				st.errDetail = fmt.Sprintf("内容审核拦截: %s", logfmt.Truncate(msg, 80))
				writeOpenAIErrorHint(w, http.StatusBadRequest, "content_blocked", msg,
					h.hintOf(upstream.ErrContentBlocked, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				return
			}
			// 11115「prompt is too long」：立即透传上游原文回客户端，**不罚号不轮转**
			// ——上下文超限是请求的问题（同一 body 换任何号都超限，白扔健康号配额；
			// 与 WAF IP fail-fast 同哲学：确定与账号无关的错误直接终止轮转）。
			// applyErrorPolicy ErrPromptTooLong 分支零动作（不冷却/不熔断/不 NoteError，
			// 不喂连败），fail 只释放租约。error-passthrough：message 装上游 body 原文
			// （code/msg/requestId 原样，含真实 token 数与上限值——上游原文是最有价值
			// 的错误信息，客户端必须看到，禁止固定词覆盖）。
			if kind == upstream.ErrPromptTooLong {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				longMsg := promptTooLongMessage(string(respBody))
				log.Printf("WARN: [server] prompt too long: acct=%s model=%s msg=%s", logfmt.Label(acct.UID, acct.Nickname), bareModel, logfmt.Truncate(longMsg, 150))
				st.errDetail = fmt.Sprintf("上下文超限: %s", logfmt.Truncate(longMsg, 80))
				writeOpenAIErrorHint(w, http.StatusBadRequest, "prompt_too_long", longMsg,
					h.hintOf(upstream.ErrPromptTooLong, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				return
			}
			// 图片格式/数据无效：立即透传上游原文回客户端，不罚号不轮转。
			// 同一 body 换账号仍是同样的解析结果，轮转只会放大无效请求。
			if kind == upstream.ErrImageInvalid {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					msg = "image request was rejected by upstream"
				}
				log.Printf("WARN: [server] image invalid: acct=%s model=%s msg=%s", logfmt.Label(acct.UID, acct.Nickname), bareModel, logfmt.Truncate(msg, 150))
				st.errDetail = fmt.Sprintf("图片无效: %s", logfmt.Truncate(msg, 80))
				writeOpenAIErrorHint(w, http.StatusBadRequest, "image_invalid", msg,
					h.hintOf(upstream.ErrImageInvalid, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				return
			}
			// lastErr 携带完整 body（uerr.Msg 在 upstream 侧截断 200 字符，透传语义
			// 5755fe3 要求原文全量）+ Kind/RetryAfter（末端映射与冷却时长共用）。
			log.Printf("WARN: [server] upstream error (turn=%d/%d): acct=%s model=%s status=%d kind=%s body=%s",
				i+1, h.cfg.MaxRotate, logfmt.Label(acct.UID, acct.Nickname), bareModel, status, kind, logfmt.Truncate(string(respBody), 200))
			lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody), RetryAfter: uerr.RetryAfter}
			st.errDetail = fmt.Sprintf("上游错误 %d (%s): %s", status, kind, logfmt.Truncate(string(respBody), 100))
			h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
			fail(acct.UID)
			// WAF IP 级 fail-fast（优先于 rotateBackoff 退避——IP 级拦截时退避无意义，
			// 任务书设计纪律）：该次 WAF 403 喂入 IP 级状态机，若激活（短窗多号命中，
			// IP 被拦而非账号）则立即终止轮转——继续换号只会把请求放大 MaxRotate 倍
			// 打同一出口 IP，加重风控。账号级软冷却已在上方 applyErrorPolicy 照常记账
			// （单号偶发 403 仍冷却），IP 级状态只改变「是否继续轮转」——协同不叠加。
			if kind == upstream.ErrWafBlock && h.wafIP.noteWaf(acct.UID) {
				break
			}
			if !rotateBackoff(i, r.Context()) {
				break // ctx 取消：终止轮转（分类错误换号退避，WAF P0-2）
			}
			continue
		}
		h.cfg.Pool.NoteSuccess(acct.UID)
		st.errDetail = "" // 成功则清空之前可能残留的轮转尝试错误信息
		// 11102 负缓存清命：该账号该模型实测成功，立即解除避让（不必等 TTL 到期）。
		// BlockModelClear 按 "11102" reason 前缀识别，只清 11102 条目、不碰 6004 独立冷却。
		h.cfg.Pool.BlockModelClear(acct.UID, bareModel)
		// 粘性跟随最终成功号：本轮成功的账号成为该会话的粘性绑定（覆盖旧绑定）。
		// 若 sticky 号失败、轮换到别的号成功，这里把会话重绑到新号，多轮对话下一跳不再随机抽。
		if stickyKey != "" && h.cfg.Session != nil {
			h.cfg.Session.Bind(stickyKey, acct.UID)
		}
		if peek.Stream {
			// 流式：透传结束后立即关闭上游 body，避免 defer 在轮转场景下堆积 fd。
			st.status = http.StatusOK
			stats := newChatStatsReaderSince(rc, st.start)
			// gateway_hint（SSE）：成功状态 200 已开流，中途 error 帧透传时附加
			// hint 字段（hintFn 惰性求值——正常流零开销，只有真撞到 error 帧才
			// 组装请求上下文做判定）。
			sErr := upstream.StreamHint(w, stats, upstream.FrameHintFunc(func() upstream.HintContext {
				return h.hintContext(bareModel, reqHasImage)
			}))
			if upstream.IsEmptyStreamError(sErr) {
				// 上游 200 但空流（0 有效帧）：StreamHint 已写 error 帧 + [DONE]
				// 兜底（HTTP 头已发出只能 200），但这是上游缺陷不是成功——日志/
				// 状态收敛到 502 观测，与非流式 Aggregate 空流→502 upstream_parse
				// 同语义（此前 `_ =` 吞错把失败流记成 200，运维看到假成功）。
				// 只认 IsEmptyStreamError：客户端断连的写失败不误标（人已走，
				// 502 观测没有意义）。
				st.status = http.StatusBadGateway
				log.Printf("WARN: [server] stream acct=%s model=%s: empty upstream stream (200+0 frames)", logfmt.Label(acct.UID, acct.Nickname), bareModel)
			}
			st.ttfb = stats.TTFB()
			// usage 缺失时保留 chatStat.toks 的 -1 哨兵（观测缺失 → 显示 "-"），
			// 不写入零值——否则「没观测到 usage」被伪造成「测得 0 token」，
			// 与非流式走 completionTokens 返回 -1 的口径不一致。
			toks, hasUsage := stats.Tokens()
			if hasUsage {
				st.toks = toks
			}
			// metrics 采集：token 三段 + 缓存三段 + 真实扣费（供 /v1/stats）。
			// 与成本账本同源同口径（都读末帧 usage），故此处一并带出，避免二次解析。
			st.hasUsage = hasUsage
			st.prompt = stats.PromptTokens()
			st.cacheHit, st.cacheMiss, st.cacheWr = stats.CacheTokens()
			if credit, ok := stats.Credit(); ok {
				st.credit = credit
				st.hasCredit = true
			}
			// 成本账本：末帧 usage 带 credit 与 token 总数时记录实测单价，
			// 供下次选号把免费/便宜的号排在前面。
			if credit, ok := stats.Credit(); ok {
				h.cfg.Pool.NoteModelCost(acct.UID, bareModel, credit, stats.TotalTokens())
				if u, ok := r.Context().Value(userContextKey).(*usermgr.User); ok && u != nil && h.cfg.UserMgr != nil {
					cost := int64(credit)
					if cost <= 0 && credit > 0 {
						cost = 1
					}
					h.cfg.UserMgr.DeductDailyCredit(u.APIKey, cost)
				}
			} else if hasUsage {
				// R9(c) 防护观测：usage 存在但 credit 缺失（如 global SSE 末帧未带 credit）。
				// 不算合法成本观测（缺失≠0），仅记一条 WARN 协助排障，绝不写入账本。
				log.Printf("WARN: [server] stream usage without credit acct=%s model=%s (no cost observation)", logfmt.Label(acct.UID, acct.Nickname), bareModel)
			}
			rc.Close()
			return
		}
		resp, err := upstream.Aggregate(rc)
		rc.Close()
		if err != nil {
			// 上游流解析失败：客户端还没看到任何输出，回 502 并告知原因。
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			st.status = http.StatusBadGateway
			return
		}
		writeJSON(w, http.StatusOK, resp)
		st.status = http.StatusOK
		st.toks = completionTokens(resp)
		// 成本账本（非流式）：从聚合响应的 usage 取 credit 与 token 总数。
		if credit, total, ok := usageCreditTotal(resp); ok {
			h.cfg.Pool.NoteModelCost(acct.UID, bareModel, credit, total)
			if u, ok := r.Context().Value(userContextKey).(*usermgr.User); ok && u != nil && h.cfg.UserMgr != nil {
				cost := int64(credit)
				if cost <= 0 && credit > 0 {
					cost = 1
				}
				h.cfg.UserMgr.DeductDailyCredit(u.APIKey, cost)
			}
		}
		// metrics 采集（非流式）：与流式同口径，从同一份 usage 带出。
		fillStatFromUsage(st, resp)
		return
	}
	// 末端错误透传（error-passthrough）：上游返回的错误原样透传，不再规范化成固定文案。
	//
	// 背景：此前把上游原始错误文本统一改写，防账号 UID / 上游内部错误码（11128 / 12153 /
	// 6004 后台措辞）泄露——但副作用是客户端看不到真实错误，根本没法排查上游问题。
	// 任务书规定上游错误码/账号语义**允许**泄露给客户端（有意为之），排查必须看到原文。
	//
	//   - 上游返回（*upstream.Error）→ error.message 装**上游 body 原文**（code/msg/
	//     requestId 原样保留，如 {"code":6004,"msg":"…","requestId":"…"}）。HTTP 状态码
	//     按 OpenAI 兼容口径映射类别：ErrSoftRate → 429（限流语义、客户端应等待重试），
	//     其余保持 503（网关侧无健康账号可用）。业务 code 取本地分类可读名
	//     （rate_limit_exceeded / no_healthy_account）。
	//   - 本地调度类错误（无可用账号 acct==nil、传输层抖动、非上游返回的 lastErr）
	//     → 保留自有文案 no_healthy_account（本地错误没有上游原文可透传，不编造）。
	status := http.StatusServiceUnavailable
	code := "no_healthy_account"
	msg := "all accounts are temporarily unavailable, please retry later"
	// gateway_hint（末端透传）：上游错误按 Kind + 原文 + 请求形态判定（11133/11135
	// 在 hint 层自带形态判定，ErrClient 家族也能带上 hint）；本地调度类错误
	// （无上游原文）固定 no_healthy_account hint。
	hint := upstream.NoHealthyAccountHint()
	var ue *upstream.Error
	if errors.As(lastErr, &ue) {
		hint = h.hintOf(ue.Kind, ue.Msg, bareModel, reqHasImage, ue)
		switch ue.Kind {
		case upstream.ErrSoftRate:
			status = http.StatusTooManyRequests
			code = "rate_limit_exceeded"
			msg = "rate limited: all accounts are cooling down, please wait a moment and try again"
		case upstream.ErrWafBlock:
			if h.wafIP.active() {
				// IP 级拦截措辞（fail-fast 终止路径）：空 body 时给出明确可读文案——
				// 网关出口 IP 被 WAF 拦截、轮转已止损、窗口 X 秒后自动解除。客户端
				// 提前重试无意义（换号不换 IP）；有上游原文时原文优先（下方统一）。
				code = "waf_ip_blocked"
				msg = "waf ip-level block: upstream firewall is blocking the gateway IP, rotation stopped; retry after the block window expires"
			}
		}
		if s := strings.TrimSpace(ue.Msg); s != "" {
			// 上游原文优先：透传 code/msg/requestId，不拼接本地前缀。
			msg = s
		}
	} else {
		// 本地选号/调度失败（无上游返回错误）
		if st.status == http.StatusTooManyRequests {
			status = http.StatusTooManyRequests
			code = "rate_limit_exceeded"
			if st.errDetail != "" {
				msg = fmt.Sprintf("rate limited: all accounts are cooling down: %s", st.errDetail)
			}
		} else if st.errDetail != "" {
			msg = fmt.Sprintf("all accounts are temporarily unavailable: %s", st.errDetail)
		}
	}
	if streamHeadersSent {
		errPayload, _ := json.Marshal(map[string]any{
			"error": map[string]any{
				"message":      msg,
				"type":         "server_error",
				"code":         code,
				"gateway_hint": hint,
			},
		})
		_, _ = io.WriteString(w, "data: "+string(errPayload)+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	} else {
		writeOpenAIErrorHint(w, status, code, msg, hint)
	}
	st.status = status
}

// promptTooLongMessage 11115 透传 message：上游 body 原文（含真实 token 数/
// 上限值/requestId，客户端自行排查）；空 body 兜底为可读分类短文案（不编造原文）。
func promptTooLongMessage(body string) string {
	if strings.TrimSpace(body) == "" {
		return "prompt is too long"
	}
	return body
}

// rotateBackoff 轮转间指数退避 + 抖动（WAF 403 修复 P0-2，报告 §6）：
// 第 i 次轮转失败（continue 换号前）等待 backoffAfter(i)（500ms·2^i 封顶 8s，
// ±25% 抖动），ctx 取消（客户端断连/优雅停机）返回 false——调用方立即终止轮转
// （客户端已走，换号重试无意义）。退避是「换号前歇一下」让上游频控窗口滑过；
// 正常单号请求（首次成功）不经过本函数，零开销。
func rotateBackoff(i int, ctx context.Context) bool {
	d := backoffAfter(i)
	if d <= 0 {
		return ctx.Err() == nil
	}
	if !sleepCtx(ctx, d) {
		log.Printf("WARN: [server] rotate backoff aborted: ctx cancelled")
		return false
	}
	return true
}

// applyErrorPolicy 按错误分类对账号施加冷却/禁用/熔断策略（最终版状态机）。
// kind 是唯一权威分类（来自 upstream.Classify / ChatStreamContext 的 *Error 信封），
// 此处不再按原始 status 二次判断。仅在 chatCompletions 轮转循环内调用：内容拦截
// 会立即 400 返回，其余种类 continue 换号（continue 前由 rotateBackoff 退避）。
//
// 十条路径，各司其职：
//   - ErrHardCredit → CooldownUntilTomorrow4AM：即时硬冷却到次日 04:00（等签到恢复）。
//   - ErrSoftRate → 优先对齐上游重置墙钟（带「将在 … 重置」时 6004 走模型级豁免、
//     非 6004 走账号级，均不指数堆加）；无重置时间才走有界退避（soft_rate 基数起、
//     softStreak 翻倍、封顶 soft_rate_max，冷却中兜底探测不翻倍）。P1-2 后冷却时长
//     优先采信 Retry-After 头（uerr.RetryAfter，body 文案墙钟之外的头形态来源）。
//   - ErrWafBlock → 账号级软冷却（WAF 403 修复 P0-1）：**不 Disable**——WAF 403 是
//     IP/指纹维频控信号（报告 §6：双账号 403 后账号本身健康），罚过即走、到期自愈。
//     时长优先 Retry-After 头（P1-2）；缺失按 wafCooldownBase(60s) 起 · 2^softStreak
//     封顶 soft_rate_max 的既有 CooldownSoftRate 有界退避（比 429 的 soft_rate 严：
//     基数小但响应快；WAF 信号带 IP 级粘性故指数升级保底存在）。基数经 jitterDur
//     抖动（复用 backoff.go 单一抖动来源，防多账号同相位冷却到期再聚团）。
//   - ErrNotFound → Cooldown(CoolSoft, notFoundCooldown 固定 60s)：短冷却防雪崩，不随 soft_rate 退避。
//   - ErrSessionDead → Disable：session 死亡，永久禁用（需人工重登）。
//   - ErrContentBlocked → 不罚账号（无冷却/熔断/NoteError）；passthrough 首遇触发
//     降级重试，最终仍拦则回 400 content_blocked（防火墙文案，不含账号/错误码）。
//   - ErrBadParams → 不罚账号（无冷却/熔断/NoteError，同 ErrContentBlocked 待遇），但仍轮转。
//   - ErrPromptTooLong → 11115「prompt is too long」：请求的问题不是账号的问题
//     （同一 body 换任何号都超限）。零动作（不冷却/不熔断/不 NoteError、不喂连败，
//     同 ErrContentBlocked 待遇），chatCompletions 已直接透传原文返回不轮转——
//     该分支只为文档完备，不指望走到换号路径。
//   - ErrImageInvalid → 图片格式/数据无效：请求的问题不是账号的问题（同一 body
//     换任何号都会得到相同的解析错误）。零动作（不冷却/不熔断/不 NoteError、
//     不喂连败），chatCompletions 已直接透传原文返回不轮转。
//   - ErrServer → NoteError：喂单一连续失败计数器 fails + 累计错误 errTotal，
//     达到 breakerThreshold 触发熔断（指数退避）。
//   - ErrModelBlocked → BlockModelBackoff：(账号, 模型) 11102 负缓存避让（复用 modelCooldowns
//     机制，Until=指数退避 TTL，选号侧 healthyForModel 避开，切模型即可用）。
//   - 其他（default：ErrClient/ErrNone）→ 只换号不罚（防雪崩），不喂熔断；ErrClient
//     额外喂连败计数（NoteFailures，issue #114）：未知 4xx 连败 N 次临时出池——
//     「不知道原因的兜底」，与冷却「知道原因的惩罚」并存取更长者不叠加（health
//     或门；带权威分类的错误不喂连败，防重复计罚）。ErrNone 零防御路径不喂。
//
// body 仅在 ErrSoftRate 分支用于识别上游 6004 模型级限流并解析重置时间；model 为请求
// 携带的模型名（触发 6004 时记录以便后续切模型豁免）。uerr 是 ChatStreamContext 返回的
// 分类信封（可携带 RetryAfter，P1-2）；零值/防御路径下为 nil，冷却时长回落既有计算。
//
// 恢复出口：CoolSoft/CoolHard 各自到期自动恢复；熔断按其指数退避截止到期；
// 成功（NoteSuccess）清 fails/熔断；签到解冻（ReenableIfCredits→reviveCoolingLocked）只清冷却，不动熔断。
func (h *Handler) applyErrorPolicy(uid string, kind upstream.ErrKind, body, model string, uerr *upstream.Error) {
	switch kind {
	case upstream.ErrHardCredit:
		// 402 + 余额关键词即积分耗尽：同步冷却到次日 04:00（签到任务 09/21 点恢复），
		// 不需要异步核查（冗余）。立即换号。
		h.cfg.Pool.CooldownUntilTomorrow4AM(uid, "余额不足")
	case upstream.ErrSoftRate:
		// 统一对齐上游重置时间（重构核心）：只要 body 带「将在 … 重置」，无论业务
		// code 是 6004 还是 11140 rate-limiting 等形态，都精确冷却到该墙钟、绝不
		// softStreak 指数堆加。
		//   - 模型级（6004）→ CooldownSoftForModel：写 modelCooldowns[model]，切模型
		//     豁免（既有 issue #31 语义）。
		//   - 账号级（非 6004）→ CooldownSoftRate：写账号级 until，不产生模型豁免
		//     （普通账号级限流不该因切模型绕过）。
		isModelLimit := upstream.IsModelRateLimit(body)
		if resetAt, ok := upstream.ParseRateReset(body); ok {
			if isModelLimit {
				h.cfg.Pool.CooldownSoftForModel(uid, h.cfg.SoftCooldown, resetAt, model, "6004 model rate limit")
				return
			}
			h.cfg.Pool.CooldownSoftRate(uid, h.cfg.SoftCooldown, resetAt, "429 rate limit")
			return
		}
		// P1-2：body 无重置文案但带 Retry-After 头 → 冷却到该时刻（不做指数堆加，
		// 与重置墙钟同一对齐语义）。头优先于「有界退避」，但**低于** body 重置文案
		// （上方已 return）——文案是上游更权威的口径（WAF 403 报告 §2.1：intl CLI
		// 同序，Retry-After 也只在无重置文案时兜底）。
		if uerr != nil && uerr.RetryAfter > 0 {
			h.cfg.Pool.CooldownSoftRate(uid, h.cfg.SoftCooldown, time.Now().Add(uerr.RetryAfter), "429 rate limit (retry-after)")
			return
		}
		// 无重置时间 → 账号级有界退避（soft_rate 基数起、softStreak 翻倍、封顶
		// soft_rate_max）；已在冷却中的兜底探测不翻倍（见 CooldownSoftRate）。
		h.cfg.Pool.CooldownSoftRate(uid, h.cfg.SoftCooldown, time.Time{}, "429 rate limit")
	case upstream.ErrWafBlock:
		// P0-1：WAF 403（无业务信封拦截形态）。软冷却复用 CooldownSoftRate 家族
		// （不新建平行冷却系统）：基数 wafCooldownBase（60s，抖动后落 [45s,75s]）、
		// softStreak 指数升级、封顶 soft_rate_max、冷却中兜底探测不翻倍——全部继承
		// 既有语义。Retry-After 头优先（P1-2，WAF 拦截页可能带该头）。不 Disable。
		if uerr != nil && uerr.RetryAfter > 0 {
			h.cfg.Pool.CooldownSoftRate(uid, jitterDur(wafCooldownBase), time.Now().Add(uerr.RetryAfter), "waf 403 block (retry-after)")
			return
		}
		h.cfg.Pool.CooldownSoftRate(uid, jitterDur(wafCooldownBase), time.Time{}, "waf 403 block")
	case upstream.ErrSessionDead:
		h.cfg.Pool.Disable(uid, "12153 session dead")
		if h.alertMgr != nil {
			nick := uid
			if a := h.cfg.Pool.AuthByUID(uid); a != nil {
				nick = a.Nickname
			}
			h.alertMgr.NotifyBan(uid, nick, "12153 session dead")
		}
	case upstream.ErrNotFound:
		// 404 短冷却（软冷却），防雪崩。固定 notFoundCooldown，不随 soft_rate 退避：
		// 偶发路径缺失不是限流信号，不该按限流惩罚升级。
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, notFoundCooldown, "upstream 404")
	case upstream.ErrAccountFault:
		// 账号级授权/配额故障按 msg 分野（口径与 Classify 的 accountFaultMarkers 一致）：
		//   - "request illegal"（code 11140）→ 账号级**授权封禁**：软冷却到期也不会自动
		//     恢复（需重新 OAuth 登录），到期后重新选号只会再撞 403 浪费一次轮换——
		//     硬禁用（Disable），不再参与选号。/status 以 disabled + disabled_reason 呈现。
		//   - 14017（trial not activated）→ register 未完成，补完 register 后可能自愈，
		//     **保持软冷却**（禁用会让用户补完 register 后仍无法用）。
		// 两条路径对坏号都立刻换号（同一请求轮转出池），只是后续可恢复性不同。
		// 大小写不敏感（与 Classify 的 marker 匹配同口径）。
		if strings.Contains(strings.ToLower(body), "request illegal") {
			reason := "account banned by upstream (11140 request illegal), re-login required"
			h.cfg.Pool.Disable(uid, reason)
			if h.alertMgr != nil {
				nick := uid
				if a := h.cfg.Pool.AuthByUID(uid); a != nil {
					nick = a.Nickname
				}
				h.alertMgr.NotifyBan(uid, nick, reason)
			}
			return
		}
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.cfg.SoftCooldown, "account fault (14017)")
	case upstream.ErrServer:
		// 5xx 上游故障：Classify 已把 ≥500 判为 ErrServer，在此喂熔断计数（不再手写 status>=500）。
		h.cfg.Pool.NoteError(uid)
	case upstream.ErrContentBlocked:
		// 内容策略拦截：内容问题非账号问题，不罚账号（无冷却/熔断/NoteError）。
		// passthrough 首遇由 chatCompletions 内降级重试处理；最终仍拦则回 400
		// content_blocked（防火墙文案），不再轮转、不暴露账号/冷却/错误码。
	case upstream.ErrPromptTooLong:
		// 11115「prompt is too long」：请求的问题不是账号的问题（同一 body 换任何
		// 号都超限）。零动作（不冷却/不熔断/不 NoteError，同 ErrContentBlocked
		// 待遇），chatCompletions 已直接透传原文返回不轮转——该分支只为文档完备，
		// 不指望走到换号路径。
	case upstream.ErrImageInvalid:
		// 图片格式/数据无效：请求的问题不是账号的问题（同一 body 换任何号都会
		// 得到相同解析错误）。零动作，chatCompletions 已 fail-fast 透传。
	case upstream.ErrBadParams:
		// 请求体解析失败（400 + Unmarshal chat params failed / 11101）：发给上游的 body
		// 有问题（网关截断已由 413 消灭，剩余为客户端畸形 JSON）。换了账号照样 400，
		// 不罚账号（无冷却/熔断/NoteError，同 ErrContentBlocked 待遇）；但**仍然轮转**
		// ——不同账号可能有不同的模型权限，值得换号再试一次。
	case upstream.ErrModelBlocked:
		// 11102「该后端无此模型」：(账号, 模型) 负缓存避让。复用 modelCooldowns 机制
		// （与 6004 同域），写 modelCooldowns[model]，Until 为指数退避 TTL（6h 起、封顶
		// 24h）。选号侧 healthyForModel 对该账号自动避开该模型；切模型/切账号即可用。
		// 立即换号（本轮 continue），该账号该模型冷却，下次选号避开。
		h.cfg.Pool.BlockModelBackoff(uid, model, upstream.ModelBlockReason)
	default:
		// 其余（ErrClient/ErrNone）：只换号不罚（防雪崩），不喂熔断。
		// ErrClient（未知 4xx）喂连败计数（issue #114）：连续 N 次该形态失败 →
		// 账号临时出池（NoteFailures 达阈降权），单次/偶发不罚（不误伤）。ErrNone
		// 到这里属防御路径（status>=400 但分类成功），语义不明不喂。
		if kind == upstream.ErrClient {
			h.cfg.Pool.NoteFailures(uid)
			if uerr != nil && uerr.Status == 401 && h.alertMgr != nil {
				nick := uid
				if a := h.cfg.Pool.AuthByUID(uid); a != nil {
					nick = a.Nickname
				}
				h.alertMgr.NotifyAuthFailed(uid, nick, uerr.Msg)
			}
		}
	}
}

// hasImagePart 报告聊天请求体是否携带多模态 image_url / image part（OpenAI 兼容形态
// messages[].content[] {type:"image_url"} 或 {type:"image"}）。
func hasImagePart(body []byte) bool {
	var peek struct {
		Messages []struct {
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &peek) != nil {
		return false
	}
	for _, m := range peek.Messages {
		for _, p := range m.Content {
			if p.Type == "image_url" || p.Type == "image" {
				return true
			}
		}
	}
	return false
}

// hintContext 组装 chatCompletions 的 gateway_hint 判定上下文：请求裸模型名 +
// 是否带图 + 模型目录 supports_images 声明（目录未收录 → ModelInCatalog=false，
// 不做「不支持」判定，防查不到误判）。仅错误路径调用（成功请求零开销）。
//
// 目录查询只读既有缓存快照（cachedModelsSnapshot），**不触发上游拉取**：错误路径
// 加一次 FetchModels 网络调用既拖慢错误响应、又污染上游调用语义（错误风暴时放大
// 请求量——与 WAF IP fail-fast 的「不放大请求量」哲学相悖）。缓存冷（最近 1h 未
// 拉过）→ ModelInCatalog=false，11133 退中性 hint（宁缺勿滥，不编造能力事实）。
func (h *Handler) hintContext(bareModel string, hasImage bool) upstream.HintContext {
	ctx := upstream.HintContext{Model: bareModel, HasImage: hasImage}
	if bareModel == "" {
		return ctx
	}
	for _, mi := range cachedModelsSnapshot() {
		if mi.ID == bareModel {
			ctx.ModelInCatalog = true
			ctx.ModelSupportsImages = mi.SupportsImages
			return ctx
		}
	}
	return ctx
}

// hintOf 末端错误透传的统一 hint 入口：kind + 上游原文 + 请求上下文 →
// gateway_hint 文案（upstream.GatewayHint 单一事实来源）。uerr 为 nil 时回落
// body 原文判定（防御路径）。transport 层错误（lastErr 非 *upstream.Error 且
// 上游没回 body）→ 无 hint（不编造）。
func (h *Handler) hintOf(kind upstream.ErrKind, body, bareModel string, hasImage bool, uerr *upstream.Error) string {
	msg := body
	if uerr != nil && uerr.Msg != "" {
		msg = uerr.Msg
	}
	return upstream.GatewayHint(kind, msg, h.hintContext(bareModel, hasImage))
}

var dshProxyClient = &http.Client{
	Timeout: 10 * time.Minute,
	Transport: &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
	},
}

// proxyToDsh 将发往 dsh 模型的 chat/completions 请求反向代理至正在运行的 DeepSeek Harness 实例。
func (h *Handler) proxyToDsh(w http.ResponseWriter, r *http.Request, body []byte) {
	if h.cfg.DshMgr == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "dsh_disabled", "DeepSeek Harness (dsh) 未在当前服务中启用")
		return
	}
	st := h.cfg.DshMgr.GetStatus()
	if !st.Running {
		writeOpenAIError(w, http.StatusServiceUnavailable, "dsh_not_running", "DeepSeek Harness 尚未启动。请在 Web 控制台的 Agent 标签页点击启动，或通过 Docker Compose 启动")
		return
	}

	targetURL := fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", st.Port)
	// 尝试向 127.0.0.1 代理，若连接失败且在 Docker 环境则尝试 dsh:3080
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "proxy_error", "create proxy req: "+err.Error())
		return
	}
	for k, vv := range r.Header {
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}

	resp, err := dshProxyClient.Do(req)
	if err != nil {
		// 备用 1：尝试直连内部进程端口（st.Port+1，如 3081）
		targetURLAlt := fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", st.Port+1)
		if reqAlt, errAlt := http.NewRequestWithContext(r.Context(), http.MethodPost, targetURLAlt, bytes.NewReader(body)); errAlt == nil {
			for k, vv := range r.Header {
				for _, v := range vv {
					reqAlt.Header.Add(k, v)
				}
			}
			resp, err = dshProxyClient.Do(reqAlt)
		}
	}
	if err != nil && st.ExternalRunning {
		// 备用 2：Docker 网络模式尝试直连 dsh 容器
		targetURLDocker := fmt.Sprintf("http://dsh:%d/v1/chat/completions", st.Port)
		if reqDocker, errD := http.NewRequestWithContext(r.Context(), http.MethodPost, targetURLDocker, bytes.NewReader(body)); errD == nil {
			for k, vv := range r.Header {
				for _, v := range vv {
					reqDocker.Header.Add(k, v)
				}
			}
			resp, err = dshProxyClient.Do(reqDocker)
		}
	}

	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "dsh_unreachable", "无法连接到本地 dsh 智能体服务: "+err.Error())
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
