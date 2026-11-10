#!/usr/bin/env python3
"""一次性积分任务脚本的共享函数库.

与 probe_active.py 同风格：从 auths/ 读账号凭证，封装 growth 域 / report 域
请求，供 task_*.py 复用。全部默认 dry-run（写动作由调用脚本显式 --yes 放行）。

端点权威来源（Go 代码实测 + 本次实测确认）：
  - chat 域（copilot.tencent.com）：growth / tasks / buddy / streak / chat/completions
  - billing 域（www.codebuddy.cn）：/v2/report
  - accept : POST /v2/activity/growth/tasks/accept  {"task_codes":[code]}
  - claim  : POST /activity/growth/tasks/{task_code}/claim  （路径含 code、无 body；
     chat 域 400 时降级 web 域 www.workbuddy.cn 同路径 + x-client-platform: web）
  - 领养   : POST /activity/growth/buddy/agreement + /buddy/first（幂等，+300c+8e）
"""
import json, os, time, glob, urllib.request, urllib.error


def _resolve_auths_dir() -> str:
    """解析 auths 凭证目录：WB2A_AUTHS > 仓库根 auths/ > /root/workbuddy2api/auths 兜底。

    env 显式覆盖最优先；本地仓库 auths/ 按 __file__ 自定位（脚本位于 scripts/ 下，
    仓库根为其上两级），非 Linux 部署（auth 不在 /root/workbuddy2api）自动回落
    本地 auths/；兜底保持 Linux 服务器行为不变。
    """
    env = os.environ.get("WB2A_AUTHS")
    if env:
        return env
    local = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "auths")
    if os.path.isdir(local):
        return local
    return "/root/workbuddy2api/auths"


AUTHS = _resolve_auths_dir()
CHAT_BASE = "https://copilot.tencent.com"   # growth / tasks / buddy / streak / chat
BILL_BASE = "https://www.codebuddy.cn"      # report / billing

# growth 域常量（travel.go / report.go 与本次实测对齐）
PATH_LIST_TASKS     = "/v2/activity/growth/tasks"
PATH_ACCEPT_TASKS   = "/v2/activity/growth/tasks/accept"
PATH_CLAIM_REWARD   = "/activity/growth/tasks"   # + "/{code}/claim"（M15 chat 域实测口径）
PATH_BUDDY_FIRST    = "/activity/growth/buddy/first"
PATH_BUDDY_AGREEMENT = "/activity/growth/buddy/agreement"
PATH_STREAK         = "/activity/growth/streak"
PATH_REPORT         = "/v2/report"
PATH_CHAT           = "/v2/chat/completions"

CLIENT_UA = "CLI/2.63.2 CodeBuddy/2.63.2"


def clean_raw_uid(uid: str) -> str:
    """清理 UID 前缀（cn-/global-/personal-/google- 等），还原真实原始 UID。"""
    if not uid:
        return ""
    for p in ("cn-", "global-", "cn:", "global:"):
        if uid.lower().startswith(p):
            uid = uid[len(p):]
            break
    for p in ("google-", "twitter-", "github-", "copilot-", "personal-", "wechat-", "qq-", "phone-", "email-"):
        if uid.lower().startswith(p):
            uid = uid[len(p):]
            break
    if "#" in uid:
        uid = uid.split("#", 1)[0]
    return uid


def _extract_token_fields(d: dict) -> tuple:
    """兼容嵌套格式 ({auth, account}) 与扁平格式读取凭证字段。"""
    a = d.get("auth") if isinstance(d.get("auth"), dict) else d
    acc = d.get("account") if isinstance(d.get("account"), dict) else d
    token = a.get("accessToken") or a.get("access_token") or d.get("accessToken") or d.get("access_token") or ""
    rt = a.get("refreshToken") or a.get("refresh_token") or d.get("refreshToken") or d.get("refresh_token") or ""
    exp = a.get("expiresAt") or a.get("expires_at") or d.get("expiresAt") or d.get("expires_at") or 0
    try:
        exp = int(exp)
    except Exception:
        exp = 0
    uid = acc.get("uid") or d.get("uid") or ""
    nick = acc.get("nickname") or d.get("nickname") or ""
    domain = a.get("domain") or d.get("domain") or ""
    realm = a.get("realm") or d.get("realm") or ""
    return token, rt, exp, uid, nick, domain, realm


def list_all_auth_files() -> list:
    """扫描 AUTHS 目录下所有有效账号凭证 JSON 文件的完整路径（不限于 workbuddy-*.json 前缀）。"""
    skip_names = {"state.json", "users.json", "settings.json", "config.json", "metrics.json", "web_sessions.json"}
    out = []
    if not os.path.isdir(AUTHS):
        return out
    for p in sorted(glob.glob(os.path.join(AUTHS, "*.json"))):
        if os.path.basename(p).lower() in skip_names:
            continue
        try:
            with open(p, encoding="utf-8") as f:
                d = json.load(f)
            token, rt, _, uid, _, _, _ = _extract_token_fields(d)
            if token or rt or uid:
                out.append(p)
        except Exception:
            continue
    return out


def refresh_auth(auth: dict) -> bool:
    """若账号配有 refresh_token，主动向 /v2/plugin/auth/token/refresh 刷新 accessToken 并落盘。"""
    rt = auth.get("refresh_token") or ""
    if not rt or auth_is_global(auth):
        return False
    url = chat_base(auth) + "/v2/plugin/auth/token/refresh"
    hdr = {
        "Content-Type": "application/json",
        "Accept": "application/json",
        "User-Agent": CLIENT_UA,
        "Origin": "https://www.codebuddy.cn",
        "Referer": "https://www.codebuddy.cn/",
        "X-Requested-With": "XMLHttpRequest",
        "X-CodeBuddy-Request": "1",
        "X-Refresh-Token": rt,
        "X-Auth-Refresh-Source": "plugin",
    }
    req = urllib.request.Request(url, data=None, headers=hdr, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=15) as r:
            res = json.loads(r.read().decode("utf-8", "replace"))
        data = res.get("data") or res
        new_at = data.get("accessToken") or ""
        if not new_at:
            return False
        auth["token"] = new_at
        if data.get("refreshToken"):
            auth["refresh_token"] = data["refreshToken"]
        exp_in = int(data.get("expiresIn") or 7200)
        new_exp = int(time.time()) + exp_in
        auth["expires_at"] = new_exp
        # 原子更新磁盘凭证文件
        fp = auth.get("_path")
        if fp and os.path.isfile(fp):
            try:
                with open(fp, encoding="utf-8") as f:
                    d = json.load(f)
                if isinstance(d.get("auth"), dict):
                    d["auth"]["accessToken"] = new_at
                    if data.get("refreshToken"):
                        d["auth"]["refreshToken"] = data["refreshToken"]
                    d["auth"]["expiresAt"] = new_exp
                else:
                    d["accessToken"] = new_at
                    if "access_token" in d:
                        d["access_token"] = new_at
                    if data.get("refreshToken"):
                        d["refreshToken"] = data["refreshToken"]
                    d["expiresAt"] = new_exp
                tmp = fp + ".tmp"
                with open(tmp, "w", encoding="utf-8") as f:
                    json.dump(d, f, indent=2, ensure_ascii=False)
                os.replace(tmp, fp)
            except Exception:
                pass
        return True
    except Exception:
        return False


def load_auth(uid_or_file: str) -> dict:
    """从 auths/ 加载账号凭证，uid_or_file 为文件路径、文件名、uid 或 uid 前缀。

    返回字典含 {token, uid, domain, nick, file, realm, refresh_token, expires_at, _path}。
    支持嵌套形与扁平形 JSON，自动剥离 cn-/personal- 前缀还原干净 uid，并在过期时自动刷新 token。
    """
    if os.path.sep in uid_or_file or "/" in uid_or_file or uid_or_file.endswith(".json"):
        p = uid_or_file
        if not os.path.isabs(p):
            p = os.path.join(AUTHS, p)
        if not os.path.isfile(p):
            raise SystemExit(f"no auth file: {p}")
    else:
        pre = uid_or_file.lower()
        hits = []
        for cand in list_all_auth_files():
            bname = os.path.basename(cand).lower()
            if pre in bname:
                hits.append(cand)
                continue
            try:
                with open(cand, encoding="utf-8") as f:
                    raw = json.load(f)
                _, _, _, cuid, _, _, _ = _extract_token_fields(raw)
                if cuid and (cuid.lower().startswith(pre) or clean_raw_uid(cuid).lower().startswith(pre)):
                    hits.append(cand)
            except Exception:
                pass
        if not hits:
            raise SystemExit(f"no auth for {uid_or_file}")
        p = hits[0]

    with open(p, encoding="utf-8") as f:
        d = json.load(f)
    token, rt, exp, uid, nick, domain, realm = _extract_token_fields(d)
    if not token and not rt:
        raise SystemExit(f"no accessToken in {os.path.basename(p)}")
    clean_uid = clean_raw_uid(uid) or uid
    info = {
        "token": token,
        "refresh_token": rt,
        "expires_at": exp,
        "domain": domain,
        "uid": clean_uid,
        "nick": nick,
        "file": os.path.basename(p),
        "realm": realm,
        "_path": p,
    }
    if not auth_is_global(info) and rt and (not token or (exp > 0 and time.time() >= exp - 1800)):
        refresh_auth(info)
    return info


def auth_is_global(auth: dict) -> bool:
    """判定账号是否属于 global realm：realm==global 或 domain 后缀 .workbuddy.ai。

    与 Go auth.Realm() 的判定口径一致（显式 realm 优先于 domain 回落）。
    供 CN-only 任务脚本跳过 global 账号、明确提示，避免把 global token 打向
    copilot.tencent.com/codebuddy.cn（全球版无任务中心，打 CN 端点属错误行为）。
    """
    if (auth.get("realm") or "").strip().lower() == "global":
        return True
    d = (auth.get("domain") or "").strip().lower()
    return d == "workbuddy.ai" or d.endswith(".workbuddy.ai")


def chat_base(auth: dict) -> str:
    return auth.get("chat_base") or CHAT_BASE


def billing_base(auth: dict) -> str:
    return auth.get("billing_base") or BILL_BASE


def _headers(auth: dict) -> dict:
    hdr = {"Authorization": "Bearer " + auth["token"],
           "Accept": "application/json",
           "Content-Type": "application/json",
           "User-Agent": CLIENT_UA,
           "Origin": "https://www.codebuddy.cn",
           "Referer": "https://www.codebuddy.cn/"}
    if auth.get("uid"):
        hdr["X-User-Id"] = auth["uid"]
    if auth.get("domain"):
        hdr["X-Domain"] = auth["domain"]
    return hdr


def _request(auth, method, base, path, body=None, headers=None, timeout=30, _retried=False):
    url = path if path.startswith("http") else base + path
    hdr = _headers(auth)
    if headers:
        hdr.update(headers)
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, headers=hdr, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, json.loads(r.read().decode("utf-8", "replace"))
    except urllib.error.HTTPError as e:
        if e.code == 401 and not _retried and auth.get("refresh_token") and refresh_auth(auth):
            return _request(auth, method, base, path, body, headers, timeout, _retried=True)
        t = e.read().decode("utf-8", "replace")
        try:
            return e.code, json.loads(t)
        except Exception:
            return e.code, {"raw": t[:300]}
    except Exception as e:
        return -1, {"err": repr(e)}


def do_get(auth, base, path, headers=None) -> tuple:
    """GET 读请求，返回 (status, dict)。"""
    return _request(auth, "GET", base, path, None, headers)


def do_post(auth, base, path, body, headers=None) -> tuple:
    """POST 写请求，返回 (status, dict)。body 为 dict。"""
    return _request(auth, "POST", base, path, body, headers)


def list_tasks(auth) -> list:
    """GET /v2/activity/growth/tasks 全量任务列表（元素为原始 dict）。"""
    st, d = do_get(auth, chat_base(auth), PATH_LIST_TASKS)
    if st != 200:
        raise RuntimeError(f"list_tasks http={st}")
    tasks = (d.get("data", {}) or {}).get("tasks") or []
    return tasks


def task_status(auth, task_code) -> dict | None:
    """查单个任务当前状态；找不到返回 None。"""
    for t in list_tasks(auth):
        if t.get("task_code") == task_code:
            return t
    return None


def accept_tasks(auth, task_codes) -> tuple:
    """POST accept 任务（not_accepted → accepted）。返回 (status, resp)。"""
    return do_post(auth, chat_base(auth), PATH_ACCEPT_TASKS,
                   {"task_codes": task_codes})


def get_streak(auth) -> int:
    """GET /activity/growth/streak 连登天数（只读 oracle）。失败返回 -1 记日志。"""
    st, d = do_get(auth, chat_base(auth), PATH_STREAK)
    if st != 200:
        return -1
    return (d.get("data", {}).get("streak", {}) or {}).get("days", 0)


def claim_reward(auth, task_code, web_fallback=True) -> tuple:
    """POST {chat}/activity/growth/tasks/{code}/claim 领奖（M15 实测端点。

    路径含 task_code、无 body。chat 域 400 时降级 web 域 www.workbuddy.cn 同路径，
    带 Origin/Referer/x-client-platform: web 头（fork ClaimReward 同款）。
    重复领返回业务错误码（already_claimed），幂等安全。
    """
    path = f"{PATH_CLAIM_REWARD}/{task_code}/claim"
    st, r = do_post(auth, chat_base(auth), path, None)
    if web_fallback and st == 400:
        # 400 降级 web 域（fork 实测 Web 成长中心端点）：chat 域路径对部分任务 400
        web_hdr = {
            "Authorization": "Bearer " + auth["token"],
            "Accept": "application/json, text/plain, */*",
            "Content-Type": "application/json",
            "Origin": "https://www.workbuddy.cn",
            "Referer": "https://www.workbuddy.cn/profile/growth-center",
            "x-client-platform": "web",
            "User-Agent": ("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
                           "(KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"),
            "X-User-Id": auth["uid"],
            "X-Domain": "https://www.workbuddy.cn",
        }
        st, r = do_post(auth, "https://www.workbuddy.cn", path, None, headers=web_hdr)
    return st, r


def buddy_adopt(auth, gap=1.05) -> tuple:
    """领养第一只 Buddy：agree 协议 → buddy/first（幂等，无猫时送 +300c+8e）。

    前置是当日活跃上报（chat_request_send）——缺口门槛返回 HTTP 400
    "first_buddy task not completed yet"，调用方按预期跳过。返回 (st_first, resp_first)。
    """
    do_post(auth, chat_base(auth), PATH_BUDDY_AGREEMENT, {"agree": True})
    time.sleep(gap)
    return do_post(auth, chat_base(auth), PATH_BUDDY_FIRST, {})


def chat_event(auth, conversation_id=None, model_id="deepseek-v4-flash",
               model_name="DeepSeek V4 Flash", mode="craft"):
    """客户端 chat_request_send 事件完整形状（照抄 report.go / probe_active.py）。

    必须带 userId（=账号 uid），缺失则服务端 200 但静默丢弃。
    model_id/name 可换（如 GLM-5.2），供 model_chat 对齐实际模型。
    """
    now = int(time.time() * 1000)
    cid = conversation_id or f"task-{now}"
    return {"eventCode": "chat_request_send", "timestamp": now, "reportDelay": 0,
            "mode": mode, "conversationId": cid, "requestId": cid,
            "inputLength": 12, "requestModelId": model_id,
            "requestModelName": model_name, "isPlan": False,
            "isAutoExecuteTerminal": False, "isAutoModify": False,
            "codebaseEnable": False, "maxToken": 0, "maxSteps": 0, "temperature": 0,
            "maxRetries": 0, "mentionContexts": [], "knowledgeId": [],
            "knowledgeName": [], "codebaseId": "", "mentionContextCount": 0,
            "command": "", "expertId": "", "recommendId": "", "skillId": "",
            "skillCount": 0, "totalCount": 0, "fileUri": "", "presentAt": now,
            "traceId": "", "rootRequestId": cid, "parentConversationId": cid,
            "agentName": "default", "agentType": "conversation", "userId": auth["uid"]}


def report_activity(auth, count=1, gap=1.05, model_id="deepseek-v4-flash",
                    model_name="DeepSeek V4 Flash", mode="craft") -> list:
    """向 {billing}/v2/report 上报 count 条 chat_request_send。

    每次间隔 >= gap 秒（默认 1.05，匹配 probe_active.py 的同接口限速口径）。
    返回 [(status, code), ...] 汇总。
    """
    out = []
    for i in range(count):
        ev = chat_event(auth, model_id=model_id, model_name=model_name, mode=mode)
        st, r = do_post(auth, billing_base(auth), PATH_REPORT, [ev])
        out.append((st, r.get("code") if isinstance(r, dict) else None))
        if i < count - 1:
            time.sleep(gap)
    return out


def chat_completion(auth, model_id="glm-5.2", prompt="hi", max_tokens=32,
                    timeout=60, extra_var=None) -> tuple:
    """POST {chat}/v2/chat/completions 真实对话一次（stream:true）。

    服务端强制流式（payload.go 同款口径），这里逐行读 SSE 直到 done。
    返回 (status, first_content)。用于 Model_chat_GLM5.2 的“真实对话一次”。

    extra_var（可选）：顶层 extra_vars 覆盖/新增字段（如 growthEvent），
    模拟桌面端 requestOptions.providerData 的透传形状。
    """
    body = {"model": model_id, "messages": [{"role": "user", "content": prompt}],
            "stream": True, "max_tokens": max_tokens}
    if extra_var:
        body["extra_vars"] = {**(body.get("extra_vars") or {}), **extra_var}
    hdr = {"Accept": "text/event-stream"}  # SSE
    url = chat_base(auth) + PATH_CHAT
    req_headers = _headers(auth)
    req_headers.update(hdr)
    req = urllib.request.Request(url, data=json.dumps(body).encode(),
                                 headers=req_headers, method="POST")
    first = ""
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            status = r.status
            for raw in r:
                line = raw.decode("utf-8", "replace")
                if line.startswith("data: "):
                    payload = line[6:].strip()
                    if payload in ("[DONE]", ""):
                        continue
                    try:
                        obj = json.loads(payload)
                        delta = (obj.get("choices") or [{}])[0].get("delta") or {}
                        content = delta.get("content") or ""
                        if content and not first:
                            first = content
                    except Exception:
                        pass
            return status, first
    except urllib.error.HTTPError as e:
        t = e.read().decode("utf-8", "replace")
        return e.code, t[:200]
    except Exception as e:
        return -1, repr(e)[:200]