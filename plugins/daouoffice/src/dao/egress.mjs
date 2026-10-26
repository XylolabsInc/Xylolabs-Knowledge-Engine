import http from 'node:http';
import net from 'node:net';
import dns from 'node:dns/promises';
import { randomBytes } from 'node:crypto';
import { PluginError } from '../errors.mjs';

/**
 * 다우오피스 밖의 공개 페이지(상품 페이지, 환율 페이지)를 여는 브라우저 컨텍스트의 출구.
 *
 * 캡처 브라우저는 서버 안에서 돈다. 사람이나 모델이 준 주소, 또는 그 페이지가 시키는 요청으로
 * 내부망(클라우드 메타데이터, localhost 의 서비스, 사설망, Tailscale)을 열어 그 화면을 돌려주면
 * 안 된다.
 *
 * 그래서 컨텍스트의 모든 연결을 이 프로세스의 작은 HTTP 프록시로 보낸다. Chromium 은 프록시를
 * 쓰면 이름을 스스로 풀지 않는다. 프록시가 이름을 풀고 **모든** 주소가 공개 주소일 때만, 방금 확인한
 * 그 주소로 직접 연결한다. 두 번째 DNS 응답(리바인딩)이 끼어들 틈이 없고, 리다이렉트, 페이지 안의
 * 요청, WebSocket(CONNECT) 모두 같은 길로 나간다. `route.continue()` 로 검사하면 Chromium 이 다시
 * 이름을 풀고, `route.fulfill()` 로 보낸 3xx 는 Chromium 이 가로채기 없이 따라가서 쓰지 않는다.
 *
 * 프록시는 127.0.0.1 에만 열고 호출마다 새 아이디와 비밀번호를 쓴다. 서비스 워커는 막는다.
 */

// 한 BlockList 에 IPv6 규칙을 넣으면 IPv4 주소도 그 규칙(::ffff:a.b.c.d)으로 걸린다. 둘을 나눈다.
const PRIVATE4 = new net.BlockList();
for (const [ip, bits] of [
  ['0.0.0.0', 8], ['10.0.0.0', 8], ['100.64.0.0', 10], ['127.0.0.0', 8], ['169.254.0.0', 16], ['172.16.0.0', 12],
  ['192.0.0.0', 24], ['192.0.2.0', 24], ['192.168.0.0', 16], ['198.18.0.0', 15], ['198.51.100.0', 24], ['203.0.113.0', 24], ['224.0.0.0', 3],
]) PRIVATE4.addSubnet(ip, bits, 'ipv4');
const PRIVATE6 = new net.BlockList();
for (const [ip, bits] of [['::', 96], ['fc00::', 7], ['fe80::', 10], ['ff00::', 8], ['64:ff9b::', 96], ['2001:db8::', 32]]) {
  PRIVATE6.addSubnet(ip, bits, 'ipv6');
}
const LOCAL_NAME = /^(localhost|.*\.localhost|.*\.local|.*\.internal|.*\.lan|.*\.home\.arpa)$/i;

/** IPv4 를 품은 IPv6(`::ffff:10.0.0.1`, URL 이 바꿔 놓는 `::ffff:a00:1`)의 IPv4. 아니면 null. */
function mappedV4(ip) {
  const dotted = ip.match(/^::ffff:(\d+\.\d+\.\d+\.\d+)$/i)?.[1];
  if (dotted) return dotted;
  const hex = ip.match(/^::ffff:([0-9a-f]{1,4}):([0-9a-f]{1,4})$/i);
  if (!hex) return null;
  const [a, b] = [parseInt(hex[1], 16), parseInt(hex[2], 16)];
  return `${a >> 8}.${a & 255}.${b >> 8}.${b & 255}`;
}

/** 사설, 루프백, 링크 로컬, 예약 주소인가. IP 가 아니면 true(열지 않는다). */
export function isPrivateAddress(ip) {
  const v = net.isIP(ip);
  if (v === 4) return PRIVATE4.check(ip, 'ipv4');
  if (v === 6) {
    const v4 = mappedV4(ip);
    return v4 ? PRIVATE4.check(v4, 'ipv4') : PRIVATE6.check(ip, 'ipv6');
  }
  return true;
}

/** 시험에서만 켠다. 로컬 시험 서버(127.0.0.1)를 열 수 있게 한다. 프록시는 그대로 거친다. */
export const allowPrivateForTests = () => process.env.DAOUOFFICE_QUOTE_ALLOW_PRIVATE === '1';

/**
 * 이름(또는 IP)을 풀어 연결해도 되는 주소 목록을 돌려준다. 안 되면 `{ reason }`.
 * 이름이 내부 주소를 하나라도 가리키면 거부한다(섞어 돌려주는 리바인딩 응답도 막는다).
 * `allowAddresses` 는 시험용으로 특정 주소만 내부망이어도 허락한다.
 */
export async function resolvePublic(host, { lookup = dns.lookup, allowPrivate = false, allowAddresses = [] } = {}) {
  const allowed = new Set(allowAddresses);
  const refused = (ip) => !allowPrivate && !allowed.has(ip) && isPrivateAddress(ip);
  const h = String(host ?? '').replace(/^\[|\]$/g, '');
  if (!h) return { reason: '주소가 아닙니다' };
  if (net.isIP(h)) return refused(h) ? { reason: '내부망 주소입니다' } : { addresses: [h] };
  if (!allowPrivate && LOCAL_NAME.test(h)) return { reason: '내부 이름입니다' };
  let found;
  try { found = await lookup(h, { all: true }); } catch { return { reason: '주소를 찾지 못했습니다' }; }
  const addresses = (found ?? []).map((a) => (typeof a === 'string' ? a : a.address)).filter((a) => net.isIP(a));
  if (!addresses.length) return { reason: '주소를 찾지 못했습니다' };
  if (addresses.some(refused)) return { reason: '내부망 주소로 풀립니다' };
  return { addresses };
}

/**
 * 공개 주소가 아니면 이유를, 맞으면 null. 사람에게 왜 안 되는지 먼저 알리려고 쓴다.
 * 실제로 막는 것은 프록시다.
 */
export async function privateReason(url, policy = {}) {
  let u;
  try { u = new URL(url); } catch { return '주소가 아닙니다'; }
  if (!/^https?:$/.test(u.protocol)) return 'http, https 주소만 엽니다';
  return (await resolvePublic(u.hostname, policy)).reason ?? null;
}

/** 확인한 주소에 차례로 연결한다. 모두 실패하면 마지막 오류. */
async function connectAny(addresses, port) {
  let last;
  for (const host of addresses) {
    try {
      return await new Promise((resolve, reject) => {
        const s = net.connect({ host, port });
        s.once('connect', () => { s.off('error', reject); resolve(s); });
        s.once('error', reject);
        s.setTimeout(30_000, () => s.destroy(new Error('connect timeout')));
      });
    } catch (e) {
      last = e;
    }
  }
  throw last ?? new Error('no address');
}

const HOP = new Set(['proxy-authorization', 'proxy-connection', 'connection', 'keep-alive', 'transfer-encoding', 'te', 'trailer', 'upgrade']);

/**
 * 127.0.0.1 의 임의 포트에 검사 프록시를 띄운다.
 *
 *   const guard = await startGuardProxy();
 *   // guard.proxy → BrowserContext 의 proxy 옵션, guard.blocked → 막은 목적지, guard.close()
 *
 * `lookup`, `allowPrivate`, `allowAddresses` 는 시험용이다.
 */
export async function startGuardProxy({ lookup = dns.lookup, allowPrivate = allowPrivateForTests(), allowAddresses = [] } = {}) {
  const policy = { lookup, allowPrivate, allowAddresses };
  const username = randomBytes(6).toString('hex');
  const password = randomBytes(18).toString('hex');
  const expected = `Basic ${Buffer.from(`${username}:${password}`).toString('base64')}`;
  const blocked = [];
  const sockets = new Set();
  const authorized = (req) => req.headers['proxy-authorization'] === expected;
  const refuse = (host, reason) => blocked.push({ host, reason });

  const server = http.createServer(async (req, res) => {
    if (!authorized(req)) {
      res.writeHead(407, { 'Proxy-Authenticate': 'Basic realm="daouoffice"' });
      res.end();
      return;
    }
    let u;
    try { u = new URL(req.url); } catch { res.writeHead(400); res.end(); return; }
    if (u.protocol !== 'http:') { res.writeHead(400); res.end(); return; }
    const r = await resolvePublic(u.hostname, policy);
    if (r.reason) {
      refuse(u.hostname, r.reason);
      res.writeHead(403, { 'Content-Type': 'text/plain; charset=utf-8' });
      res.end('blocked');
      return;
    }
    const port = Number(u.port || 80);
    const headers = Object.fromEntries(Object.entries(req.headers).filter(([k]) => !HOP.has(k)));
    headers.host = u.host;
    let socket;
    try { socket = await connectAny(r.addresses, port); } catch {
      res.writeHead(502);
      res.end();
      return;
    }
    // 확인한 주소의 소켓을 그대로 쓴다. http.request 가 이름을 다시 풀지 않게 한다.
    const up = http.request({ createConnection: () => socket, method: req.method, path: `${u.pathname}${u.search}`, headers }, (ur) => {
      const out = Object.fromEntries(Object.entries(ur.headers).filter(([k]) => !HOP.has(k)));
      res.writeHead(ur.statusCode ?? 502, out);
      ur.pipe(res);
    });
    up.setTimeout(60_000, () => up.destroy(new Error('upstream timeout')));
    up.on('error', () => { if (!res.headersSent) res.writeHead(502); res.end(); });
    req.pipe(up);
  });

  server.on('connect', async (req, client, head) => {
    sockets.add(client);
    client.on('close', () => sockets.delete(client));
    client.on('error', () => {});
    if (!authorized(req)) {
      client.end('HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic realm="daouoffice"\r\n\r\n');
      return;
    }
    const m = String(req.url).match(/^\[?([^\]]+?)\]?:(\d{1,5})$/);
    if (!m) { client.end('HTTP/1.1 400 Bad Request\r\n\r\n'); return; }
    const [, host, port] = m;
    const r = await resolvePublic(host, policy);
    if (r.reason) {
      refuse(host, r.reason);
      client.end('HTTP/1.1 403 Forbidden\r\n\r\n');
      return;
    }
    let up;
    try { up = await connectAny(r.addresses, Number(port)); } catch {
      client.end('HTTP/1.1 502 Bad Gateway\r\n\r\n');
      return;
    }
    sockets.add(up);
    up.on('close', () => sockets.delete(up));
    up.on('error', () => client.destroy());
    up.setTimeout(0);
    client.write('HTTP/1.1 200 Connection Established\r\n\r\n');
    if (head?.length) up.write(head);
    up.pipe(client);
    client.pipe(up);
  });
  server.on('connection', (s) => { sockets.add(s); s.on('close', () => sockets.delete(s)); });
  server.on('clientError', (_e, s) => s.destroy());

  await new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', resolve);
  });
  const { port } = server.address();
  return {
    allowPrivate,
    blocked,
    /** 이 프록시의 규칙으로 본 주소의 거부 이유. 열어도 되면 null. */
    reason: (url) => privateReason(url, policy),
    proxy: { server: `http://127.0.0.1:${port}`, username, password, bypass: '<-loopback>' },
    async close() {
      for (const s of sockets) s.destroy();
      await new Promise((r) => server.close(() => r()));
    },
  };
}

/**
 * 검사 프록시를 거치는 빈 컨텍스트(다우오피스 쿠키도 로그인도 없다)에서 `fn({ page, context, browser, guard })` 를 돌린다.
 * `contextOptions` 는 객체나 `(browser) => 객체` 이고, 프록시와 서비스 워커 설정은 덮어쓸 수 없다.
 */
export async function withGuardedContext(sessions, fn, { contextOptions = {}, ...policy } = {}) {
  if (typeof sessions?.withScratchContext !== 'function') throw new PluginError('INTERNAL', '공개 페이지를 열 브라우저가 준비되지 않았습니다.');
  const guard = await startGuardProxy(policy);
  try {
    return await sessions.withScratchContext((s) => fn({ ...s, guard }), {
      contextOptions: async (browser) => ({
        ...(typeof contextOptions === 'function' ? await contextOptions(browser) : contextOptions),
        proxy: guard.proxy,
        serviceWorkers: 'block',
      }),
    });
  } finally {
    await guard.close();
  }
}
