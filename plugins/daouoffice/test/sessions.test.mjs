import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import { setTimeout as sleep } from 'node:timers/promises';
import { SessionManager } from '../src/sessions.mjs';
import { PluginError } from '../src/errors.mjs';

const BASE = 'https://example.daouoffice.com';
const CREDS = { username: 'hong', password: 'pw' };

/** A browser that records what happens to it. Pages answer the session check with `page.status`. */
function fakeWorld() {
  const events = [];
  const world = { events, browsers: [], contexts: [], logins: [], checks: 0, loginBehavior: () => 'ok' };
  world.launch = async () => {
    const browser = {
      closed: false,
      handlers: {},
      on(ev, fn) { this.handlers[ev] = fn; },
      isConnected() { return !this.closed; },
      async newContext(opts) {
        const page = {
          href: 'about:blank', closed: false, status: 200,
          url() { return this.href; },
          isClosed() { return this.closed; },
          async goto(u) { this.href = u; },
          async evaluate() { return { status: 200, text: '{}' }; },
        };
        const context = {
          opts, page, closed: false,
          browser: () => browser,
          async newPage() { return page; },
          async close() { this.closed = true; page.closed = true; events.push('context.close'); },
        };
        world.contexts.push(context);
        events.push('newContext');
        return context;
      },
      async close() { this.closed = true; events.push('browser.close'); },
    };
    world.browsers.push(browser);
    events.push('launch');
    return browser;
  };
  world.login = async (page, baseUrl, creds, { onDuplicate }) => {
    world.logins.push({ user: creds.username, onDuplicate });
    const how = world.loginBehavior(creds, onDuplicate);
    if (how === 'auth') throw new PluginError('AUTH_FAILED', 'bad password');
    if (how === 'duplicate') {
      if (onDuplicate === 'report') return { status: 'duplicate', kicked: false };
      if (onDuplicate !== 'kick') throw new PluginError('DUPLICATE_SESSION', 'in use elsewhere');
      page.href = `${baseUrl}/home`;
      return { status: 'ok', kicked: true };
    }
    page.href = `${baseUrl}/home`;
    return { status: 'ok', kicked: false };
  };
  world.checkSession = async (page) => { world.checks++; return page.status; };
  world.manager = (opts = {}) => new SessionManager({
    baseUrl: BASE, launch: world.launch, login: world.login, checkSession: world.checkSession, loginGapMs: 0, ...opts,
  });
  return world;
}

const deferred = () => {
  let resolve;
  const promise = new Promise((r) => { resolve = r; });
  return { promise, resolve };
};

describe('SessionManager', () => {
  test('logs in lazily once and reuses the session', async () => {
    const w = fakeWorld();
    const m = w.manager();
    const first = await m.withSession('hong', CREDS, {}, async (s) => s.fresh);
    const second = await m.withSession('hong', CREDS, {}, async (s) => s.fresh);
    assert.equal(first, true);
    assert.equal(second, false);
    assert.equal(w.logins.length, 1);
    assert.equal(w.checks, 1, 'checked before the second use');
    assert.deepEqual(w.contexts[0].opts, { locale: 'ko-KR', timezoneId: 'Asia/Seoul', viewport: { width: 1440, height: 900 }, acceptDownloads: true });
    await m.close();
  });

  test('a 401 on the check means log in again', async () => {
    const w = fakeWorld();
    const m = w.manager();
    await m.withSession('hong', CREDS, {}, async () => {});
    w.contexts[0].page.status = 401;
    const fresh = await m.withSession('hong', CREDS, {}, async (s) => { s.page.status = 200; return s.fresh; });
    assert.equal(fresh, true);
    assert.equal(w.logins.length, 2);
    await m.close();
  });

  test('an api 401 marks the session stale', async () => {
    const w = fakeWorld();
    const m = w.manager();
    await m.withSession('hong', CREDS, {}, async (s) => {
      s.page.evaluate = async () => ({ status: 401, text: '' });
      await s.api('/gw/api/approval/todo/count');
    });
    w.contexts[0].page.evaluate = async () => ({ status: 200, text: '{}' });
    const fresh = await m.withSession('hong', CREDS, {}, async (s) => s.fresh);
    assert.equal(fresh, true);
    assert.equal(w.checks, 0, 'stale sessions skip the check');
    await m.close();
  });

  test('a session older than the TTL is not checked, it logs in again', async () => {
    let t = 1_000_000;
    const w = fakeWorld();
    const m = w.manager({ now: () => t, sessionTtlMs: 1000 });
    await m.withSession('hong', CREDS, {}, async () => {});
    t += 1001;
    assert.equal(await m.withSession('hong', CREDS, {}, async (s) => s.fresh), true);
    assert.equal(w.checks, 0);
    await m.close();
  });

  test('calls for one account run one at a time, in order', async () => {
    const w = fakeWorld();
    const m = w.manager();
    const order = [];
    let running = 0;
    let peak = 0;
    const job = (n) => m.withSession('hong', CREDS, {}, async () => {
      running++; peak = Math.max(peak, running);
      order.push(`start ${n}`);
      await sleep(10);
      order.push(`end ${n}`);
      running--;
    });
    await Promise.all([job(1), job(2), job(3)]);
    assert.equal(peak, 1);
    assert.deepEqual(order, ['start 1', 'end 1', 'start 2', 'end 2', 'start 3', 'end 3']);
    await m.close();
  });

  test('different accounts run in parallel up to the capacity, the rest wait in FIFO order', async () => {
    const w = fakeWorld();
    const m = w.manager({ maxContexts: 2 });
    const gateA = deferred();
    const gateB = deferred();
    const order = [];
    const a = m.withSession('a', { username: 'a', password: 'p' }, {}, async () => { order.push('a'); await gateA.promise; });
    const b = m.withSession('b', { username: 'b', password: 'p' }, {}, async () => { order.push('b'); await gateB.promise; });
    await sleep(5);
    const c = m.withSession('c', { username: 'c', password: 'p' }, {}, async () => { order.push('c'); });
    const d = m.withSession('d', { username: 'd', password: 'p' }, {}, async () => { order.push('d'); });
    await sleep(5);
    assert.deepEqual(order, ['a', 'b']);
    assert.equal(m.stats().waiting, 2);
    gateA.resolve();
    await a;
    await c;
    assert.deepEqual(order, ['a', 'b', 'c']);
    gateB.resolve();
    await Promise.all([b, d]);
    assert.deepEqual(order, ['a', 'b', 'c', 'd']);
    assert.ok(m.stats().contexts <= 2);
    assert.equal(w.contexts.filter((x) => !x.closed).length <= 2, true);
    await m.close();
  });

  test('an idle context is evicted (least recently used first) when a new account needs a slot', async () => {
    const w = fakeWorld();
    const m = w.manager({ maxContexts: 2 });
    await m.withSession('a', { username: 'a', password: 'p' }, {}, async () => {});
    await sleep(2);
    await m.withSession('b', { username: 'b', password: 'p' }, {}, async () => {});
    await m.withSession('c', { username: 'c', password: 'p' }, {}, async () => {});
    assert.deepEqual(m.stats().owners.sort(), ['b', 'c']);
    assert.equal(w.contexts[0].closed, true, 'a was the least recently used');
    assert.equal(w.browsers.length, 1, 'the browser is shared');
    await m.close();
  });

  test('idle contexts are parked, reused without a new login, and closed when the session expires', async () => {
    const w = fakeWorld();
    const m = w.manager({ idleMs: 15, sessionTtlMs: 120 });
    await m.withSession('hong', CREDS, {}, async () => {});
    await sleep(40);
    assert.deepEqual(m.stats().parked, ['hong']);
    assert.equal(w.contexts[0].page.href, 'about:blank');
    const fresh = await m.withSession('hong', CREDS, {}, async (s) => {
      assert.equal(s.page.url(), `${BASE}/home`, 'resumed on the DaouOffice origin');
      return s.fresh;
    });
    assert.equal(fresh, false);
    assert.equal(w.logins.length, 1);
    await sleep(200);
    assert.equal(m.stats().contexts, 0);
    assert.equal(w.contexts[0].closed, true);
    assert.equal(w.browsers[0].closed, true, 'the browser closes with its last context');
    await m.close();
  });

  test('wrong password → AUTH_FAILED, context closed, and an immediate retry is rate limited', async () => {
    let t = 5_000_000;
    const w = fakeWorld();
    const m = w.manager({ now: () => t, loginGapMs: 60_000 });
    w.loginBehavior = () => 'auth';
    await assert.rejects(m.withSession('hong', CREDS, {}, async () => {}), (e) => e.code === 'AUTH_FAILED');
    assert.equal(w.contexts[0].closed, true);
    await assert.rejects(m.withSession('hong', CREDS, {}, async () => {}), (e) => e.code === 'RATE_LIMITED' && /초 뒤/.test(e.message));
    assert.equal(w.logins.length, 1, 'the refused attempt never reached the login page');
    t += 60_001;
    w.loginBehavior = () => 'ok';
    await m.withSession('hong', CREDS, {}, async () => {});
    assert.equal(w.logins.length, 2);
    await m.close();
  });

  test('at most ten login attempts per hour', async () => {
    let t = 9_000_000;
    const w = fakeWorld();
    const m = w.manager({ now: () => t, loginGapMs: 60_000, loginsPerHour: 10 });
    w.loginBehavior = () => 'auth';
    for (let i = 0; i < 10; i++) {
      await assert.rejects(m.withSession('hong', CREDS, {}, async () => {}), (e) => e.code === 'AUTH_FAILED');
      t += 61_000;
    }
    await assert.rejects(m.withSession('hong', CREDS, {}, async () => {}), (e) => e.code === 'RATE_LIMITED' && /한 시간/.test(e.message));
    t += 3600_000;
    await assert.rejects(m.withSession('hong', CREDS, {}, async () => {}), (e) => e.code === 'AUTH_FAILED');
    await m.close();
  });

  test('duplicate session without consent → DUPLICATE_SESSION; the kick retry right after is allowed', async () => {
    let t = 7_000_000;
    const w = fakeWorld();
    const m = w.manager({ now: () => t, loginGapMs: 60_000 });
    w.loginBehavior = (creds, mode) => (mode === 'kick' ? 'ok' : 'duplicate');
    await assert.rejects(m.withSession('hong', CREDS, {}, async () => {}), (e) => e.code === 'DUPLICATE_SESSION');
    assert.equal(w.contexts[0].closed, true);
    assert.equal(w.logins[0].onDuplicate, 'fail');
    t += 1000;
    await m.withSession('hong', CREDS, { allowKick: true }, async () => {});
    assert.equal(w.logins[1].onDuplicate, 'kick');
    await m.close();
  });

  test('verify reports a duplicate without kicking and closes the context', async () => {
    const w = fakeWorld();
    const m = w.manager();
    w.loginBehavior = () => 'duplicate';
    assert.deepEqual(await m.verify('hong', CREDS), { duplicate: true, fresh: false });
    assert.equal(w.logins[0].onDuplicate, 'report');
    assert.equal(m.stats().contexts, 0);
    assert.equal(w.browsers[0].closed, true);
    await m.close();
  });

  test('verify keeps a good session and re-checks when the password changes', async () => {
    const w = fakeWorld();
    const m = w.manager();
    assert.deepEqual(await m.verify('hong', CREDS), { duplicate: false, fresh: true });
    assert.deepEqual(await m.verify('hong', CREDS), { duplicate: false, fresh: false });
    assert.equal(w.logins.length, 1);
    assert.equal(await m.withSession('hong', CREDS, {}, async (s) => s.fresh), false);
    await m.verify('hong', { username: 'hong', password: 'new' });
    assert.equal(w.logins.length, 2);
    assert.equal(w.contexts[0].closed, true);
    await m.close();
  });

  test('a waiter gives up after the queue timeout', async () => {
    const w = fakeWorld();
    const m = w.manager({ maxContexts: 1, queueTimeoutMs: 20 });
    const gate = deferred();
    const a = m.withSession('a', { username: 'a', password: 'p' }, {}, async () => gate.promise);
    await sleep(5);
    await assert.rejects(m.withSession('b', { username: 'b', password: 'p' }, {}, async () => {}), (e) => e.code === 'RATE_LIMITED');
    gate.resolve();
    await a;
    await m.close();
  });

  test('refuses without baseUrl or credentials', async () => {
    const w = fakeWorld();
    await assert.rejects(new SessionManager({ launch: w.launch }).withSession('hong', CREDS, async () => {}), (e) => e.code === 'INTERNAL');
    await assert.rejects(w.manager().withSession('hong', null, async () => {}), (e) => e.code === 'NOT_CONNECTED');
  });

  test('a crashed browser is replaced on the next call', async () => {
    const w = fakeWorld();
    const m = w.manager();
    await m.withSession('hong', CREDS, {}, async () => {});
    w.browsers[0].closed = true;
    w.browsers[0].handlers.disconnected();
    await sleep(1);
    assert.equal(m.stats().contexts, 0);
    assert.equal(await m.withSession('hong', CREDS, {}, async (s) => s.fresh), true);
    assert.equal(w.browsers.length, 2);
    await m.close();
  });

  test('close() closes every context and the browser and rejects waiters', async () => {
    const w = fakeWorld();
    const m = w.manager({ maxContexts: 1 });
    const gate = deferred();
    const a = m.withSession('a', { username: 'a', password: 'p' }, {}, async () => gate.promise);
    await sleep(5);
    const b = m.withSession('b', { username: 'b', password: 'p' }, {}, async () => {});
    await sleep(5);
    await m.close();
    await assert.rejects(b, (e) => e.code === 'INTERNAL');
    gate.resolve();
    await a;
    assert.equal(w.contexts.every((c) => c.closed), true);
    assert.equal(w.browsers[0].closed, true);
  });
});
