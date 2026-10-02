#!/usr/bin/env node
/**
 * dsh-bridge.js - 0.0.0.0 端口反向代理桥接器与自动免密认证注入器
 *
 * 彻底解决 DeepSeek Harness (@deepseek-ai/dsh) 三大运行与网络限制：
 * 1. 官方强制仅监听 127.0.0.1 阻断局域网/公网访问 -> 本桥接器在 0.0.0.0:3080 监听并全透明转发至 127.0.0.1:3081；
 * 2. 官方要求必须带 ?token=... 校验并下发签名 Cookie，否则 401 "authentication required" ->
 *    本桥接器预初始化持久凭证密钥 (Secret)，并自动为所有发往 DSH 的上游请求注入权威签名的合法 Session Cookie，
 *    实现无论何时何地打开均 100% 自动认证通过，告别 401 报错；
 * 3. 官方内部 Browser-Trust Fence 拦截跨域请求导致 /api 报 403 Forbidden 错误 ->
 *    本桥接器在转发前彻底剥离 Origin、Referer 及 Sec-Fetch-* 跨域指示头，DSH 直接无条件判定为内部受信请求，彻底根除 403！
 */

const http = require('http');
const net = require('net');
const fs = require('fs');
const path = require('path');
const os = require('os');
const crypto = require('crypto');
const { spawn } = require('child_process');

const LISTEN_HOST = process.env.BRIDGE_HOST || '0.0.0.0';
const LISTEN_PORT = parseInt(process.env.BRIDGE_PORT || '3080', 10);
const TARGET_HOST = '127.0.0.1';
const TARGET_PORT = parseInt(process.env.TARGET_PORT || '3081', 10);
const TARGET_AUTHORITY = `${TARGET_HOST}:${TARGET_PORT}`;

// 凭据持久化存储目录（优先读取 DSH_HOME，默认 ~/.dsh）
const CREDENTIALS_DIR = process.env.DSH_HOME || path.join(os.homedir(), '.dsh');
const CREDENTIALS_FILE = path.join(CREDENTIALS_DIR, '.credentials.yaml');

function encodeBase64Url(buf) {
  return Buffer.from(buf).toString('base64').replaceAll('+', '-').replaceAll('/', '_').replace(/=+$/u, '');
}

function decodeBase64Url(str) {
  const padding = '='.repeat((4 - str.length % 4) % 4);
  return Buffer.from(str.replaceAll('-', '+').replaceAll('_', '/') + padding, 'base64');
}

// 预初始化或读取 DSH 的会话加密密钥，保证桥接层能直接签发与 DSH 完全一致的 Session Cookie
function getOrInitSecret() {
  try {
    if (!fs.existsSync(CREDENTIALS_DIR)) {
      fs.mkdirSync(CREDENTIALS_DIR, { recursive: true, mode: 0o700 });
    }
    if (fs.existsSync(CREDENTIALS_FILE)) {
      const content = fs.readFileSync(CREDENTIALS_FILE, 'utf8');
      const m = content.match(/secret:\s*([A-Za-z0-9_\-]+)/);
      if (m && m[1]) {
        console.log(`[dsh-bridge] 🔐 成功加载 DSH 会话凭证密钥: ${CREDENTIALS_FILE}`);
        return m[1];
      }
    }
    // 不存在则生成持久化凭据（32字节随机密钥），与 DSH 保持相同格式
    const secret = encodeBase64Url(crypto.randomBytes(32));
    const yaml = `version: 1\nrecords:\n  client-connection/browser-session:\n    kind: grant\n    payload:\n      version: 1\n      secret: ${secret}\n`;
    fs.writeFileSync(CREDENTIALS_FILE, yaml, { mode: 0o600 });
    console.log(`[dsh-bridge] 🔐 已预初始化会话凭据密钥: ${CREDENTIALS_FILE}`);
    return secret;
  } catch (e) {
    console.warn(`[dsh-bridge] 会话密钥初始化提示: ${e.message}`);
    return '';
  }
}

// 签发 DSH 认可的权威 Session Cookie（严格符合 DSH 算法，有效期 29 天）
function mintAuthCookie(authority, secretBase64) {
  if (!secretBase64) return '';
  try {
    const secretBytes = decodeBase64Url(secretBase64);
    const name = 'dsh-auth-' + encodeBase64Url(crypto.createHash('sha256').update(authority).digest());
    const now = Date.now();
    const payload = {
      version: 1,
      authority: authority,
      issuedAt: now - 1000,
      expiresAt: now + 29 * 24 * 3600 * 1000 // 29 天有效（DSH 源码上限 30 天）
    };
    const body = encodeBase64Url(Buffer.from(JSON.stringify(payload), 'utf8'));
    const sig = encodeBase64Url(crypto.createHmac('sha256', secretBytes).update(body).digest());
    return `${name}=v1.${body}.${sig}`;
  } catch (e) {
    console.warn(`[dsh-bridge] Cookie 签发失败: ${e.message}`);
    return '';
  }
}

let currentSecret = getOrInitSecret();
let validUpstreamCookie = mintAuthCookie(TARGET_AUTHORITY, currentSecret);
let currentLaunchToken = process.env.DSH_TOKEN || '';

// 捕获日志中的 Launch Token（带缓冲区及 ANSI 转义字符过滤，防分包截断）
let logBuffer = '';
function checkLogForToken(chunk) {
  logBuffer += chunk;
  if (logBuffer.length > 50000) {
    logBuffer = logBuffer.slice(-20000);
  }
  const clean = logBuffer.replace(/\u001b\[[0-9;]*[a-zA-Z]/g, '');
  const match = clean.match(/[\?&]token=([A-Za-z0-9_\-]{16,})/);
  if (match && match[1]) {
    if (currentLaunchToken !== match[1]) {
      currentLaunchToken = match[1];
      console.log(`[dsh-bridge] 🔑 成功捕获 DSH 启动安全令牌: ${currentLaunchToken}`);
      console.log(`[dsh-bridge] 🌐 外部直连地址: http://${LISTEN_HOST === '0.0.0.0' ? '你的服务器IP' : LISTEN_HOST}:${LISTEN_PORT}/?token=${currentLaunchToken}`);
    }
  }

  // 若启动前未读取到密钥，尝试在子进程输出后重新加载
  if (!currentSecret) {
    currentSecret = getOrInitSecret();
    if (currentSecret) {
      validUpstreamCookie = mintAuthCookie(TARGET_AUTHORITY, currentSecret);
    }
  }
}

// 若传入 --spawn 参数或环境变量指定，由桥接器直接托管拉起 dsh 子进程
const shouldSpawn = process.argv.includes('--spawn') || process.env.SPAWN_DSH === 'true';
if (shouldSpawn) {
  console.log(`[dsh-bridge] 启动 DeepSeek Harness 子进程 (监听 ${TARGET_AUTHORITY})...`);
  const npxCmd = process.platform === 'win32' ? 'npx.cmd' : 'npx';
  const child = spawn(npxCmd, ['-y', '@deepseek-ai/dsh', 'web', '--port', String(TARGET_PORT), '--no-open'], {
    stdio: ['inherit', 'pipe', 'pipe'],
    shell: process.platform === 'win32',
    env: { ...process.env, DSH_HOME: CREDENTIALS_DIR },
  });

  child.stdout.on('data', (data) => {
    const s = data.toString();
    process.stdout.write(s);
    checkLogForToken(s);
  });

  child.stderr.on('data', (data) => {
    const s = data.toString();
    process.stderr.write(s);
    checkLogForToken(s);
  });

  child.on('error', (err) => {
    console.error(`[dsh-bridge] DSH 子进程拉起异常:`, err);
  });

  child.on('exit', (code, signal) => {
    console.log(`[dsh-bridge] DSH 子进程已退出 (code: ${code}, signal: ${signal})`);
    process.exit(code || 0);
  });
}

const server = http.createServer((req, res) => {
  const reqHost = req.headers.host || `${LISTEN_HOST}:${LISTEN_PORT}`;
  let parsedUrl;
  try {
    parsedUrl = new URL(req.url, `http://${reqHost}`);
  } catch (e) {
    parsedUrl = { pathname: '/', searchParams: new URLSearchParams() };
  }

  const hasTokenParam = parsedUrl.searchParams.has('token');

  // 构建发往 upstream DSH 的请求头
  const headers = { ...req.headers };
  headers.host = TARGET_AUTHORITY;

  // 1. 彻底解除 DSH 内部的浏览器安全藩篱 (Browser-Trust Fence) 校验（根除 403）
  // DSH 对 /api 请求执行 isTrustedApiRequest 检验：
  // 若 origin 为 undefined，直接无条件信任返回 true；
  // 因此主动剥离所有跨域标记，使 DSH 100% 判定为内部合法请求！
  delete headers.origin;
  delete headers['sec-fetch-site'];
  delete headers['sec-fetch-mode'];
  delete headers['sec-fetch-dest'];
  delete headers.referer;

  // 2. 关键自愈：为发往 DSH 的上游请求自动注入合法签名的 Session Cookie（根除 401）
  if (validUpstreamCookie) {
    const cookieKey = validUpstreamCookie.split('=')[0];
    if (!headers.cookie || !headers.cookie.includes(cookieKey)) {
      headers.cookie = headers.cookie ? `${headers.cookie}; ${validUpstreamCookie}` : validUpstreamCookie;
    }
  }

  const options = {
    hostname: TARGET_HOST,
    port: TARGET_PORT,
    path: req.url,
    method: req.method,
    headers: headers,
  };

  const proxy = http.request(options, (upstreamRes) => {
    // 3. 兜底保护：若 upstream DSH 仍返回 401，且当前访问根路径且捕获了 Launch Token，自动 302 补全 Token 重定向换取 Cookie
    if (upstreamRes.statusCode === 401 && currentLaunchToken && req.method === 'GET' && (parsedUrl.pathname === '/' || parsedUrl.pathname === '/index.html') && !hasTokenParam) {
      parsedUrl.searchParams.set('token', currentLaunchToken);
      res.writeHead(302, {
        'Location': parsedUrl.pathname + parsedUrl.search,
        'Cache-Control': 'no-store',
      });
      res.end();
      return;
    }

    // 4. 同步下发 Session Cookie 给浏览器客户端（SameSite=Lax），确保后续静态资源及 API 保持会话
    const resHeaders = { ...upstreamRes.headers };
    if (validUpstreamCookie && req.method === 'GET' && (parsedUrl.pathname === '/' || parsedUrl.pathname === '/index.html')) {
      const clientCookie = `${validUpstreamCookie}; Max-Age=2500000; Path=/; HttpOnly; SameSite=Lax`;
      if (resHeaders['set-cookie']) {
        if (Array.isArray(resHeaders['set-cookie'])) {
          resHeaders['set-cookie'].push(clientCookie);
        } else {
          resHeaders['set-cookie'] = [resHeaders['set-cookie'], clientCookie];
        }
      } else {
        resHeaders['set-cookie'] = [clientCookie];
      }
    }

    res.writeHead(upstreamRes.statusCode, resHeaders);
    upstreamRes.pipe(res);
  });

  proxy.on('error', (err) => {
    if (!res.headersSent) {
      res.writeHead(502, { 'Content-Type': 'text/plain; charset=utf-8' });
      res.end(`[dsh-bridge] 正在等待 DeepSeek Harness 在 ${TARGET_AUTHORITY} 就绪... (错误: ${err.message})\n`);
    }
  });

  req.pipe(proxy);
});

server.on('upgrade', (req, clientSocket, head) => {
  const upstream = net.connect(TARGET_PORT, TARGET_HOST, () => {
    let raw = `${req.method} ${req.url} HTTP/1.1\r\n`;
    let hasCookie = false;
    for (const [key, val] of Object.entries(req.headers)) {
      const lk = key.toLowerCase();
      if (lk === 'host') {
        raw += `host: ${TARGET_AUTHORITY}\r\n`;
      } else if (lk === 'origin' || lk === 'sec-fetch-site' || lk === 'sec-fetch-mode' || lk === 'sec-fetch-dest' || lk === 'referer') {
        // 过滤跨域标记
      } else if (lk === 'cookie') {
        hasCookie = true;
        let cVal = val;
        if (validUpstreamCookie && !val.includes('dsh-auth-')) {
          cVal = `${val}; ${validUpstreamCookie}`;
        }
        raw += `cookie: ${cVal}\r\n`;
      } else {
        raw += `${key}: ${val}\r\n`;
      }
    }
    if (!hasCookie && validUpstreamCookie) {
      raw += `cookie: ${validUpstreamCookie}\r\n`;
    }
    raw += '\r\n';
    upstream.write(raw);
    if (head && head.length > 0) {
      upstream.write(head);
    }
    clientSocket.pipe(upstream).pipe(clientSocket);
  });

  upstream.on('error', () => {
    clientSocket.destroy();
  });
  clientSocket.on('error', () => {
    upstream.destroy();
  });
});

server.listen(LISTEN_PORT, LISTEN_HOST, () => {
  console.log(`[dsh-bridge] 🚀 0.0.0.0 桥接已启动: http://${LISTEN_HOST}:${LISTEN_PORT} -> http://${TARGET_AUTHORITY}`);
  if (validUpstreamCookie) {
    console.log(`[dsh-bridge] ✅ 已就绪会话自动鉴权注入引擎 (无须手动携带 Token 访问)`);
  }
});
