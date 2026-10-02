#!/usr/bin/env node
/**
 * dsh-bridge.js - 0.0.0.0 端口反向代理桥接器
 *
 * 解决 DeepSeek Harness (@deepseek-ai/dsh) 官方强制仅监听 127.0.0.1 并拒绝 --host 0.0.0.0，
 * 导致在 Docker 容器、云服务器、局域网或虚拟机环境中无法被外部 IP 或宿主机访问的问题。
 *
 * 工作原理：
 * 1. 本桥接器在 0.0.0.0:3080 监听，接收来自任何网卡、局域网或外部容器的请求；
 * 2. 将流量透明转发给本地 127.0.0.1:3081 运行的 dsh 进程；
 * 3. 自动将 HTTP/WebSocket 请求头 Host 重写为 127.0.0.1:3081，完美通过 dsh 内部的 trustedHosts 安全白名单；
 * 4. 完整支持 HTTP 流式传输与 WebSocket 双向连接（升级协议）。
 */

const http = require('http');
const net = require('net');

const LISTEN_HOST = process.env.BRIDGE_HOST || '0.0.0.0';
const LISTEN_PORT = parseInt(process.env.BRIDGE_PORT || '3080', 10);
const TARGET_HOST = '127.0.0.1';
const TARGET_PORT = parseInt(process.env.TARGET_PORT || '3081', 10);

const server = http.createServer((req, res) => {
  const options = {
    hostname: TARGET_HOST,
    port: TARGET_PORT,
    path: req.url,
    method: req.method,
    headers: {
      ...req.headers,
      host: `${TARGET_HOST}:${TARGET_PORT}`,
    },
  };

  const proxy = http.request(options, (upstreamRes) => {
    res.writeHead(upstreamRes.statusCode, upstreamRes.headers);
    upstreamRes.pipe(res);
  });

  proxy.on('error', (err) => {
    if (!res.headersSent) {
      res.writeHead(502, { 'Content-Type': 'text/plain; charset=utf-8' });
      res.end(`[dsh-bridge] 正在等待 DeepSeek Harness 在 127.0.0.1:${TARGET_PORT} 就绪... (错误: ${err.message})`);
    }
  });

  req.pipe(proxy);
});

server.on('upgrade', (req, clientSocket, head) => {
  const upstream = net.connect(TARGET_PORT, TARGET_HOST, () => {
    let raw = `${req.method} ${req.url} HTTP/1.1\r\n`;
    for (const [key, val] of Object.entries(req.headers)) {
      if (key.toLowerCase() === 'host') {
        raw += `host: ${TARGET_HOST}:${TARGET_PORT}\r\n`;
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
