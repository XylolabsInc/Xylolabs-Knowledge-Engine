import { test, describe, before, after } from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { login } from '../src/dao/login.mjs';
import { SessionManager } from '../src/sessions.mjs';
import { runWithMode } from '../src/guard.mjs';

/**
 * A local stand-in for the DaouOffice login screen, served to a real headless Chromium. It mimics
 * what docs/02-auth-flow.md describes: the form selectors, `POST /api/portal/public/auth/login`,
 * the "동시 접근 제한" modal with its single `기존 접속 종료` button after a correct password, and
 * the badge endpoint that answers 401 without a session. Nothing here talks to DaouOffice.
 */
function fakeSite() {
  const state = { password: 'right-pw', otherSession: false, kicks: 0, logins: 0, sessions: new Set() };
  const page = (body) => `<!doctype html><meta charset="utf-8"><body>${body}</body>`;
  const LOGIN = page(`
    <form onsubmit="return false">
      <input class="input_txt" type="text">
      <input class="input_txt password" type="password">
      <button type="submit" id="go">로그인</button>
    </form>
    <p id="msg"></p>
    <script>
      document.getElementById('go').addEventListener('click', async () => {
        const [u, p] = document.querySelectorAll('input.input_txt');
        const r = await fetch('/api/portal/public/auth/login', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ username: u.value, password: p.value }) });
        const j = await r.json();
        if (!r.ok) { document.getElementById('msg').textContent = '아이디 또는 비밀번호를 확인해 주세요'; return; }
        if (j.duplicate) {
          const d = document.createElement('div');
          d.setAttribute('role', 'dialog');
          d.innerHTML = '<p>동시 접근 제한</p><button id="kick">기존 접속 종료</button>';
          document.body.appendChild(d);
          document.getElementById('kick').addEventListener('click', async () => {
            await fetch('/api/portal/public/auth/duplicate/block/login', { method: 'POST' });
            location.href = '/home';
          });
          return;
        }
        location.href = '/home';
      });
    </script>`);
  const cookieOf = (req) => /sid=(\w+)/.exec(req.headers.cookie ?? '')?.[1];
  const server = http.createServer((req, res) => {
    const url = new URL(req.url, 'http://x');
    const send = (status, body, headers = {}) => { res.writeHead(status, headers); res.end(body); };
    let chunks = '';
    req.on('data', (c) => { chunks += c; });
    req.on('end', () => {
      if (url.pathname === '/login') return send(200, LOGIN, { 'Content-Type': 'text/html; charset=utf-8' });
      if (url.pathname === '/home') return send(200, page('<h1>home</h1>'), { 'Content-Type': 'text/html; charset=utf-8' });
      if (url.pathname === '/api/portal/public/auth/login') {
        state.logins++;
        const { password } = JSON.parse(chunks || '{}');
        if (password !== state.password) return send(401, '{"code":"401"}', { 'Content-Type': 'application/json' });
        const sid = `s${state.logins}`;
        state.sessions.add(sid);
        return send(200, JSON.stringify({ duplicate: state.otherSession }), { 'Content-Type': 'application/json', 'Set-Cookie': `sid=${sid}; Path=/` });
      }
      if (url.pathname === '/api/portal/public/auth/duplicate/block/login') {
        state.kicks++;
        state.otherSession = false;
        return send(200, '{}', { 'Content-Type': 'application/json' });
      }
      if (url.pathname === '/api/portal/common/notification/badge') {
        return state.sessions.has(cookieOf(req)) ? send(200, '{"data":{}}') : send(401, '{}');
      }
      if (url.pathname === '/gw/api/approval/todo/count') {
        return state.sessions.has(cookieOf(req)) ? send(200, '{"code":"200","data":{"docCount":2}}') : send(401, '{}');
      }
      return send(404, 'not found');
    });
  });
  return { state, server };
}

describe('login against a fake DaouOffice in real Chromium', () => {
  let browser = null;
  let site;
  let base;

  before(async () => {
    try {
      const { chromium } = await import('playwright');
      browser = await chromium.launch({ headless: true });
    } catch {
      browser = null;   // `npx playwright install chromium` not run here
    }
    site = fakeSite();
    await new Promise((r) => site.server.listen(0, '127.0.0.1', r));
    base = `http://127.0.0.1:${site.server.address().port}`;
  });
  after(async () => {
    await browser?.close();
    site.server.close();
  });

  const withPage = async (t, fn) => {
    if (!browser) return t.skip('chromium is not installed');
    const context = await browser.newContext();
    try { return await fn(await context.newPage()); } finally { await context.close(); }
  };

  test('a correct password lands on /home', (t) => withPage(t, async (page) => {
    site.state.otherSession = false;
    const r = await login(page, base, { username: 'hong', password: 'right-pw' });
    assert.deepEqual(r, { status: 'ok', kicked: false });
    assert.equal(new URL(page.url()).pathname, '/home');
  }));

  test('a wrong password is AUTH_FAILED and does not retry', (t) => withPage(t, async (page) => {
    const before = site.state.logins;
    await assert.rejects(login(page, base, { username: 'hong', password: 'wrong' }), (e) => e.code === 'AUTH_FAILED');
    assert.equal(site.state.logins - before, 1);
  }));

  test('the duplicate modal without consent is DUPLICATE_SESSION and nobody is kicked', (t) => withPage(t, async (page) => {
    site.state.otherSession = true;
    const kicks = site.state.kicks;
    await assert.rejects(login(page, base, { username: 'hong', password: 'right-pw' }), (e) => e.code === 'DUPLICATE_SESSION');
    assert.equal(site.state.kicks, kicks);
  }));

  test('report mode returns duplicate without clicking', (t) => withPage(t, async (page) => {
    site.state.otherSession = true;
    const kicks = site.state.kicks;
    assert.deepEqual(await login(page, base, { username: 'hong', password: 'right-pw' }, { onDuplicate: 'report' }), { status: 'duplicate', kicked: false });
    assert.equal(site.state.kicks, kicks);
  }));

  test('kick mode presses 기존 접속 종료 and gets in', (t) => withPage(t, async (page) => {
    site.state.otherSession = true;
    const kicks = site.state.kicks;
    assert.deepEqual(await login(page, base, { username: 'hong', password: 'right-pw' }, { onDuplicate: 'kick' }), { status: 'ok', kicked: true });
    assert.equal(site.state.kicks, kicks + 1);
    assert.equal(new URL(page.url()).pathname, '/home');
  }));

  test('the session manager drives the real flow end to end', async (t) => {
    if (!browser) return t.skip('chromium is not installed');
    site.state.otherSession = false;
    const { chromium } = await import('playwright');
    const m = new SessionManager({ baseUrl: base, loginGapMs: 0, launch: () => chromium.launch({ headless: true }) });
    try {
      assert.deepEqual(await m.verify('hong', { username: 'hong', password: 'right-pw' }), { duplicate: false, fresh: true });
      const count = await m.withSession('hong', { username: 'hong', password: 'right-pw' }, {}, async ({ api, fresh }) => {
        assert.equal(fresh, false, 'the verified session is reused');
        return (await api('/gw/api/approval/todo/count')).data.data.docCount;
      });
      assert.equal(count, 2);
      // Expire the session on the server: the next call checks the badge, sees 401 and logs in again.
      site.state.sessions.clear();
      const fresh = await runWithMode('preview', () => m.withSession('hong', { username: 'hong', password: 'right-pw' }, {}, async (s) => s.fresh));
      assert.equal(fresh, true);
    } finally {
      await m.close();
    }
  });
});
