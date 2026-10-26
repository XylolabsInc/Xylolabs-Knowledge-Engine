import { describe, test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import net from 'node:net';
import { isPrivateAddress, privateReason, resolvePublic, startGuardProxy } from '../src/dao/egress.mjs';

/**
 * The capture browser's way out. Everything here runs against local servers; names like
 * `shop.test` exist only in the fake lookup, so a request that reached them went through the proxy.
 */

const FAKE = {
  'shop.test': ['127.0.0.1'],
  'public.test': ['93.184.216.34'],
  'internal.test': ['10.0.0.5'],
  'rebind.test': ['93.184.216.34', '10.0.0.5'],
};
const lookup = async (host) => {
  const list = FAKE[host];
  if (!list) throw Object.assign(new Error('ENOTFOUND'), { code: 'ENOTFOUND' });
  return list.map((address) => ({ address, family: net.isIP(address) }));
};

describe('address checks', () => {
  test('only public addresses: metadata, loopback, private, CGNAT, mapped IPv6 and local names are refused', async () => {
    for (const ip of ['127.0.0.1', '10.1.2.3', '172.20.0.1', '192.168.0.10', '169.254.169.254', '100.100.1.1', '0.0.0.0', '::1', 'fd00::1', 'fe80::1', '::ffff:10.0.0.1', '::ffff:7f00:1']) {
      assert.equal(isPrivateAddress(ip), true, ip);
    }
    for (const ip of ['93.184.216.34', '8.8.8.8', '2606:4700::1111']) assert.equal(isPrivateAddress(ip), false, ip);
    assert.equal(await privateReason('https://public.test/p/1', { lookup }), null);
    assert.match(await privateReason('https://rebind.test/', { lookup }), /내부망/);
    assert.match(await privateReason('http://169.254.169.254/latest/meta-data/', { lookup }), /내부망/);
    assert.match(await privateReason('http://[::ffff:127.0.0.1]:8080/', { lookup }), /내부망/);
    assert.match(await privateReason('http://intranet.local/', { lookup }), /내부 이름/);
    assert.match(await privateReason('file:///etc/passwd', { lookup }), /http, https/);
    assert.match(await privateReason('https://nowhere.test/', { lookup }), /찾지 못했습니다/);
  });

  test('resolvePublic hands out every checked address, and test allowances are explicit', async () => {
    assert.deepEqual(await resolvePublic('public.test', { lookup }), { addresses: ['93.184.216.34'] });
    assert.match((await resolvePublic('shop.test', { lookup })).reason, /내부망/);
    assert.deepEqual(await resolvePublic('shop.test', { lookup, allowAddresses: ['127.0.0.1'] }), { addresses: ['127.0.0.1'] });
    assert.match((await resolvePublic('rebind.test', { lookup, allowAddresses: ['127.0.0.1'] })).reason, /내부망/);
    assert.deepEqual(await resolvePublic('internal.test', { lookup, allowPrivate: true }), { addresses: ['10.0.0.5'] });
  });
});

describe('guard proxy', () => {
  let site;
  let port;
  const hits = [];

  before(async () => {
    site = http.createServer((req, res) => {
      hits.push(`${req.method} ${req.headers.host} ${req.url}`);
      res.writeHead(200, { 'Content-Type': 'text/plain' });
      res.end('hello');
    });
    await new Promise((r) => site.listen(0, '127.0.0.1', r));
    port = site.address().port;
  });
  after(() => site.close());

  /** A plain HTTP request through the proxy, the way Chromium sends one. */
  const viaProxy = (guard, url, { auth = true } = {}) => new Promise((resolve, reject) => {
    const p = new URL(guard.proxy.server);
    const headers = auth ? { 'Proxy-Authorization': `Basic ${Buffer.from(`${guard.proxy.username}:${guard.proxy.password}`).toString('base64')}` } : {};
    const req = http.request({ host: p.hostname, port: p.port, method: 'GET', path: url, headers }, (res) => {
      let body = '';
      res.on('data', (c) => { body += c; });
      res.on('end', () => resolve({ status: res.statusCode, body }));
    });
    req.on('error', reject);
    req.end();
  });
  /** A CONNECT through the proxy. Resolves with the status line. */
  const connect = (guard, target) => new Promise((resolve, reject) => {
    const p = new URL(guard.proxy.server);
    const s = net.connect({ host: p.hostname, port: Number(p.port) }, () => {
      const auth = Buffer.from(`${guard.proxy.username}:${guard.proxy.password}`).toString('base64');
      s.write(`CONNECT ${target} HTTP/1.1\r\nHost: ${target}\r\nProxy-Authorization: Basic ${auth}\r\n\r\n`);
    });
    s.once('data', (d) => { resolve(String(d).split('\r\n')[0]); s.destroy(); });
    s.on('error', reject);
  });

  test('a name is resolved by the proxy and connected to the checked address', async () => {
    const guard = await startGuardProxy({ lookup, allowAddresses: ['127.0.0.1'] });
    try {
      const r = await viaProxy(guard, `http://shop.test:${port}/item?id=1`);
      assert.deepEqual(r, { status: 200, body: 'hello' });
      assert.equal(hits.at(-1), `GET shop.test:${port} /item?id=1`);
      assert.match(await connect(guard, `shop.test:${port}`), /^HTTP\/1\.1 200/);
    } finally {
      await guard.close();
    }
  });

  test('private destinations, rebinding answers and missing credentials are refused', async () => {
    const guard = await startGuardProxy({ lookup });
    const before = hits.length;
    try {
      assert.equal((await viaProxy(guard, `http://shop.test:${port}/`)).status, 403);
      assert.equal((await viaProxy(guard, `http://127.0.0.1:${port}/`)).status, 403);
      assert.equal((await viaProxy(guard, 'http://rebind.test/')).status, 403);
      assert.equal((await viaProxy(guard, `http://public.test:${port}/`, { auth: false })).status, 407);
      assert.match(await connect(guard, '169.254.169.254:80'), /^HTTP\/1\.1 403/);
      assert.match(await connect(guard, `internal.test:${port}`), /^HTTP\/1\.1 403/);
      assert.equal(hits.length, before, 'nothing reached the server');
      assert.deepEqual(guard.blocked.map((b) => b.host), ['shop.test', '127.0.0.1', 'rebind.test', '169.254.169.254', 'internal.test']);
      assert.equal(await guard.reason('https://internal.test/'), '내부망 주소로 풀립니다');
    } finally {
      await guard.close();
    }
  });
});
