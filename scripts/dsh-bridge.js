#!/usr/bin/env node
/**
 * dsh-bridge.js - 0.0.0.0 端口反向代理桥接器与自动令牌注入器
 *
 * 解决 DeepSeek Harness (@deepseek-ai/dsh) 两大网络限制：
 * 1. 官方强制仅监听 127.0.0.1 并拒绝 --host 0.0.0.0 -> 本桥接器在 0.0.0.0:3080 监听，转发至 127.0.0.1:3081；
 * 2. 官方要求首次必须带 ?token=... 访问，否则报 "dsh web authentication required" 401 错误 ->
 *    本桥接器自动捕获 dsh 生成的 Launch Token，当检测到客户端首次裸访问根路径时，自动 302 重定向补全 Token，
 *    实现真正的全自动免密登录与 Cookie 下发！
 */

const http = require('http');
const net = require('net');
const { spawn } = require('child_process');

const LISTEN_HOST = process.env.BRIDGE_HOST || '0.0.0.0';
const LISTEN_PORT = parseInt(process.env.BRIDGE_PORT || '3080', 10);
const TARGET_HOST = '127.0.0.1';
const TARGET_PORT = parseInt(process.env.TARGET_PORT || '3081', 10);

let currentLaunchToken = process.env.DSH_TOKEN || '';

// 捕获日志中的 Token
function checkLogForToken(text) {
  const match = text.match(/dsh web:.*?[\?&]token=([A-Za-z0-9_\-]+)/);
  if (match && match[1]) {
    currentLaunchToken = match[1];
    console.log(`[dsh-bridge] 🔑 成功捕获 DSH 启动安全令牌: ${currentLaunchToken}`);
    console.log(`[dsh-bridge] 🌐 外部免密直连地址: http://${LISTEN_HOST === '0.0.0.0' ? '你的服务器IP' : LISTEN_HOST}:${LISTEN_PORT}/?token=${currentLaunchToken}`);
  }
}

// 若传入 --spawn 参数或环境变量指定，由桥接器直接托管拉起 dsh 子进程
const shouldSpawn = process.argv.includes('--spawn') || process.env.SPAWN_DSH === 'true';
if (shouldSpawn) {
  console.log(`[dsh-bridge] 启动 DeepSeek Harness 子进程 (监听 127.0.0.1:${TARGET_PORT})...`);
  const npxCmd = process.platform === 'win32' ? 'npx.cmd' : 'npx';
  const child = spawn(npxCmd, ['-y', '@deepseek-ai/dsh', 'web', '--port', String(TARGET_PORT)], {
    stdio: ['inherit', 'pipe', 'pipe'],
    env: process.env,
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
  const cookieHeader = req.headers.cookie || '';
  const hasAuthCookie = cookieHeader.includes('dsh-auth-');

  // 若捕获到了 Launch Token，且客户端首次直接访问根路径（无 token 且无 cookie），自动补全 Token 重定向下发 Cookie
  if (req.method === 'GET' && (parsedUrl.pathname === '/' || parsedUrl.pathname === '/index.html') && !hasTokenParam && !hasAuthCookie && currentLaunchToken) {
    parsedUrl.searchParams.set('token', currentLaunchToken);
    res.writeHead(302, {
      'Location': parsedUrl.pathname + parsedUrl.search,
      'Cache-Control': 'no-store',
    });
    res.end();
    return;
  }

  const headers = { ...req.headers };
  headers.host = `${TARGET_HOST}:${TARGET_PORT}`;
  // 彻底解除 DSH 内部的浏览器安全藩篱 (Browser-Trust Fence) 校验
  // DSH 对 /api 请求执行 isTrustedApiRequest 检验：
  // 1. 若 origin 为 undefined，直接无条件信任返回 true；
  // 2. 若存在 origin，比对 new URL(origin).host === hostUrl.host。
  // 因此，直接移除 origin、referer 及 sec-fetch-* 等跨域标记，并将 host 设为 127.0.0.1:TARGET_PORT，
  // 从而让 DSH 100% 判定为合法同源内部请求，彻底根除 /api 403 错误！
  delete headers.origin;
  delete headers['sec-fetch-site'];
  delete headers['sec-fetch-mode'];
  delete headers['sec-fetch-dest'];
  delete headers.referer;

  const options = {
    hostname: TARGET_HOST,
    port: TARGET_PORT,
    path: req.url,
    method: req.method,
    headers: headers,
  };

  const proxy = http.request(options, (upstreamRes) => {
    // 关键自愈：如果 upstream DSH 返回 401（说明历史 Cookie 已失效/过期），且有 Launch Token 且是根路径访问，
    // 立即自动 302 重定向到 /?token=... 重新铸造有效 Session Cookie，防止用户陷入 401 死循环
    if (upstreamRes.statusCode === 401 && currentLaunchToken && req.method === 'GET' && (parsedUrl.pathname === '/' || parsedUrl.pathname === '/index.html')) {
      parsedUrl.searchParams.set('token', currentLaunchToken);
      res.writeHead(302, {
        'Location': parsedUrl.pathname + parsedUrl.search,
        'Cache-Control': 'no-store',
      });
      res.end();
      return;
    }
    res.writeHead(upstreamRes.statusCode, upstreamRes.headers);
    upstreamRes.pipe(res);
  });

  proxy.on('error', (err) => {
    if (!res.headersSent) {
      res.writeHead(502, { 'Content-Type': 'text/plain; charset=utf-8' });
      res.end(`[dsh-bridge] 正在等待 DeepSeek Harness 在 127.0.0.1:${TARGET_PORT} 就绪... (错误: ${err.message})\n`);
    }
  });

  req.pipe(proxy);
});

server.on('upgrade', (req, clientSocket, head) => {
  const upstream = net.connect(TARGET_PORT, TARGET_HOST, () => {
    let raw = `${req.method} ${req.url} HTTP/1.1\r\n`;
    for (const [key, val] of Object.entries(req.headers)) {
      const lk = key.toLowerCase();
      if (lk === 'host') {
        raw += `host: ${TARGET_HOST}:${TARGET_PORT}\r\n`;
      } else if (lk === 'origin' || lk === 'sec-fetch-site' || lk === 'sec-fetch-mode' || lk === 'sec-fetch-dest' || lk === 'referer') {
        // 过滤跨域标记，避免 upstream 拦截
      } else {
        raw += `${key}: ${val}\r\n`;
      }
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
  console.log(`[dsh-bridge] 🚀 0.0.0.0 桥接已启动: http://${LISTEN_HOST}:${LISTEN_PORT} -> http://${TARGET_HOST}:${TARGET_PORT}`);
});
