import { createHash } from 'node:crypto';
import { PluginError } from './errors.mjs';
import { login as defaultLogin } from './dao/login.mjs';
import { makeApi, sessionStatus } from './dao/api.mjs';

/**
 * One headless Chromium shared by everyone, one BrowserContext per DaouOffice account.
 *
 * Facts this is built around (from the private repo's docs/02-auth-flow.md):
 *
 * - One session per account. Logging in while the account is signed in elsewhere shows the
 *   "동시 접근 제한" modal; accepting it ends the other session. `allowKick` is the person's
 *   consent; without it a duplicate fails with DUPLICATE_SESSION.
 * - Sessions expire about 30 minutes after login no matter how busy they are, and a 401 cannot
 *   tell expiry from someone else logging in. The session is checked lazily before each use
 *   with `GET /api/portal/common/notification/badge`; there is no keepalive polling and no
 *   pre-emptive re-login (a re-login while our own session lives would hit our own modal).
 * - Closing a context does not end the server session, and no logout endpoint is known.
 *   UNVERIFIED: no DaouOffice logout call or menu has been observed (docs/02-auth-flow.md,
 *   docs/03-api-catalog.md), so a closed context strands its server session until it expires,
 *   and a new login within that time meets our own leftover as "another session".
 *
 * So a context is never closed while its session may still live, unless there is no other way:
 *
 * - Active contexts (a page with the SPA loaded) are limited to `maxContexts`
 *   (DAOUOFFICE_MAX_CONTEXTS, default 2; each costs roughly 150–300 MB).
 * - An idle context is **parked**: its page is closed, the context and its cookies stay. A parked
 *   context has no renderer and costs little. It is resumed with a new page on the next call and
 *   closed when its session has expired anyway.
 * - A request that needs an active slot parks the least recently used idle context, or waits in
 *   a FIFO queue while every active context is busy.
 * - At most `maxParked` (DAOUOFFICE_MAX_PARKED, default 6) parked contexts are kept. Beyond that
 *   the one whose session is closest to expiry is closed; that strands at most a few minutes of
 *   a session. The browser closes when no context is left.
 *
 * Work for one account runs one call at a time (per-owner lock).
 *
 * Login rate limit per account, and per requester for the connect check: one attempt per
 * `loginGapMs` and `loginsPerHour` attempts per hour. An attempt that only met the duplicate
 * modal does not start the gap, so the person's "끊고 진행" right after it is not refused.
 * Disconnecting an account does not reset the limiter.
 */

export const SESSION_TTL_MS = 30 * 60_000;

const silent = { debug() {}, info() {}, warn() {}, error() {} };

const credKeyOf = (creds) => createHash('sha256')
  .update(`${creds?.username ?? ''}\u0000${creds?.password ?? ''}`).digest('hex');

const isLoginUrl = (url) => {
  try { return new URL(url).pathname.startsWith('/login'); } catch { return false; }
};

async function launchChromium() {
  const { chromium } = await import('playwright');
  return chromium.launch({ headless: true, args: ['--disable-dev-shm-usage'] });
}

const CONTEXT_DEFAULTS = { locale: 'ko-KR', timezoneId: 'Asia/Seoul' };

export class SessionManager {
  #entries = new Map();     // owner → entry
  #locks = new Map();       // owner → tail promise
  #waiters = [];            // FIFO of { resolve, reject, timer }
  #active = 0;              // active slots held (entries with a page, scratch contexts, contexts being opened)
  #browser = null;          // Promise<Browser>
  #attempts = new Map();    // rate-limit key → [{ at, gap }]
  #closed = false;

  constructor({
    baseUrl,
    maxContexts = 2,
    maxParked = 6,
    idleMs = 600_000,
    sessionTtlMs = SESSION_TTL_MS,
    queueTimeoutMs = 300_000,
    loginGapMs = 60_000,
    loginsPerHour = 10,
    launch = launchChromium,
    login = defaultLogin,
    checkSession = sessionStatus,
    contextOptions = {},
    logger = silent,
    now = Date.now,
  } = {}) {
    this.baseUrl = baseUrl ? String(baseUrl).replace(/\/+$/, '') : '';
    Object.assign(this, {
      maxContexts: Math.max(1, maxContexts), maxParked: Math.max(0, maxParked), idleMs, sessionTtlMs, queueTimeoutMs,
      loginGapMs, loginsPerHour, launch, login, checkSession, contextOptions, logger, now,
    });
  }

  /**
   * Runs `fn({ page, api, context, owner, baseUrl, fresh })` on a logged-in session of `owner`.
   * `fresh` is true when this call had to log in.
   */
  async withSession(owner, creds, opts, fn) {
    if (typeof opts === 'function') [opts, fn] = [{}, opts];
    const { allowKick = false } = opts ?? {};
    this.#assertUsable(owner, creds);
    return this.#withLock(owner, async () => {
      let entry = this.#entries.get(owner);
      if (entry && this.#dead(entry)) { await this.#discard(entry, 'dead'); entry = undefined; }
      if (entry) {
        this.#cancelTimers(entry);
        entry.inUse = true;
      } else {
        entry = await this.#open(owner);
      }
      try {
        if (entry.parked) await this.#unpark(entry);
        let fresh = false;
        if (!(await this.#alive(entry))) {
          await this.#login(entry, creds, { onDuplicate: allowKick ? 'kick' : 'fail' });
          fresh = true;
        }
        entry.lastUsed = this.now();
        return await fn({ page: entry.page, api: entry.api, context: entry.context, owner, baseUrl: this.baseUrl, fresh });
      } finally {
        this.#afterUse(entry);
      }
    });
  }

  /**
   * Checks credentials for the connect flow without touching the account's existing session.
   *
   * A live session opened with the same credentials needs no login. Otherwise the check runs in
   * a throwaway context, never kicking anyone; the existing entry is replaced only once the new
   * password is proven and no session of ours was found alive. `{ duplicate: true }` means the
   * password was right but the account is in use elsewhere (possibly by our own live session).
   * `requester` (the chat user) is rate limited too, so one person cannot try many accounts.
   */
  async verify(owner, creds, { requester } = {}) {
    this.#assertUsable(owner, creds);
    return this.#withLock(owner, async () => {
      const existing = this.#entries.get(owner);
      if (existing && !this.#dead(existing) && existing.credKey === credKeyOf(creds) && this.#sessionLive(existing)) {
        return { duplicate: false, fresh: false };
      }
      const attempts = [this.#checkRate(owner), ...(requester ? [this.#checkRate(`requester:${requester}`)] : [])];
      const probe = await this.#openContext();
      let result;
      try {
        result = await this.login(probe.page, this.baseUrl, creds, { onDuplicate: 'report' });
      } catch (e) {
        if (e?.code === 'DUPLICATE_SESSION') attempts.forEach((a) => { a.gap = false; });
        this.logger.info('daouoffice connect check failed', { owner, code: e?.code ?? 'INTERNAL' });
        await this.#closeContext(probe);
        throw e;
      }
      if (result?.status === 'duplicate') {
        attempts.forEach((a) => { a.gap = false; });
        this.logger.info('daouoffice connect check met duplicate session', { owner });
        await this.#closeContext(probe);
        return { duplicate: true, fresh: false };
      }
      // Proven, and no session of ours was alive (it would have shown the duplicate modal):
      // the old entry, if any, holds nothing worth keeping.
      if (existing) await this.#discard(existing, 'replaced');
      const entry = this.#adopt(owner, probe);
      entry.loggedInAt = this.now();
      entry.credKey = credKeyOf(creds);
      this.logger.info('daouoffice login', { owner, kicked: false, via: 'connect' });
      this.#afterUse(entry);
      return { duplicate: false, fresh: true };
    });
  }

  /**
   * Runs `fn({ context, page, browser })` in a fresh context of the shared Chromium with no
   * DaouOffice cookies (product pages, exchange-rate pages). It takes an active slot like an
   * account context (FIFO when full) and is closed afterwards. `contextOptions` may be an object
   * or `(browser) => options`.
   */
  async withScratchContext(fn, { contextOptions = {} } = {}) {
    if (this.#closed) throw new PluginError('INTERNAL', '플러그인이 종료되는 중입니다.');
    const scratch = await this.#openContext({
      viewport: { width: 1280, height: 1100 },
      options: contextOptions,
    });
    try {
      return await fn({ context: scratch.context, page: scratch.page, browser: scratch.browser });
    } finally {
      await this.#closeContext(scratch);
    }
  }

  /** Drops the account's context (after a disconnect). The login limiter is kept. */
  async forget(owner) {
    return this.#withLock(owner, async () => {
      const entry = this.#entries.get(owner);
      if (entry) await this.#discard(entry, 'forget');
    });
  }

  stats() {
    const entries = [...this.#entries.values()];
    return {
      contexts: entries.length,
      active: this.#active,
      slots: this.#active,
      waiting: this.#waiters.length,
      browser: Boolean(this.#browser),
      owners: entries.map((e) => e.owner),
      parked: entries.filter((e) => e.parked).map((e) => e.owner),
    };
  }

  async close() {
    this.#closed = true;
    for (const w of this.#waiters.splice(0)) {
      clearTimeout(w.timer);
      w.reject(new PluginError('INTERNAL', '플러그인이 종료되는 중입니다.'));
    }
    const entries = [...this.#entries.values()];
    this.#entries.clear();
    for (const e of entries) {
      this.#cancelTimers(e);
      e.discarded = true;
      e.closing = true;
      await e.context.close().catch(() => {});
    }
    this.#active = 0;
    await this.#closeBrowser();
  }

  // ------------------------------------------------------------------ internals

  #assertUsable(owner, creds) {
    if (this.#closed) throw new PluginError('INTERNAL', '플러그인이 종료되는 중입니다.');
    if (!this.baseUrl) throw new PluginError('INTERNAL', '다우오피스 주소(baseUrl)가 설정되지 않았습니다.');
    if (!owner || !creds?.username || !creds?.password) {
      throw new PluginError('NOT_CONNECTED', '다우오피스 계정이 연결되지 않았습니다.');
    }
  }

  async #withLock(owner, fn) {
    const prev = this.#locks.get(owner) ?? Promise.resolve();
    let release;
    const mine = new Promise((r) => { release = r; });
    const tail = prev.then(() => mine);
    this.#locks.set(owner, tail);
    await prev;
    try {
      return await fn();
    } finally {
      release();
      if (this.#locks.get(owner) === tail) this.#locks.delete(owner);
    }
  }

  #dead(entry) {
    if (entry.discarded || entry.closing) return true;
    if (!entry.parked && entry.page?.isClosed?.()) return true;
    return entry.context.browser?.()?.isConnected?.() === false;
  }

  /** The server session may still be alive (logged in, not marked stale, not past the TTL). */
  #sessionLive(entry) {
    return Boolean(entry.loggedInAt) && !entry.stale && this.now() - entry.loggedInAt < this.sessionTtlMs;
  }

  async #alive(entry) {
    if (!this.#sessionLive(entry)) return false;
    try {
      if (entry.needsHome) {
        await entry.page.goto(`${this.baseUrl}/home`, { waitUntil: 'domcontentloaded', timeout: 60_000 });
        entry.needsHome = false;
      }
      if (isLoginUrl(entry.page.url())) return false;
      const status = await this.checkSession(entry.page);
      if (status === 401 || status === 403) return false;
      if (status >= 200 && status < 300) return true;
      throw new PluginError('UPSTREAM', `다우오피스 세션 확인이 HTTP ${status} 로 끝났습니다.`);
    } catch (e) {
      if (e instanceof PluginError) throw e;
      throw new PluginError('UPSTREAM', '다우오피스에 연결하지 못했습니다. 잠시 뒤 다시 시도해 주세요.', { cause: e });
    }
  }

  #checkRate(key) {
    const now = this.now();
    const list = (this.#attempts.get(key) ?? []).filter((a) => now - a.at < 3600_000);
    this.#attempts.set(key, list);
    if (list.length >= this.loginsPerHour) {
      throw new PluginError('RATE_LIMITED',
        `한 시간에 로그인할 수 있는 횟수(${this.loginsPerHour}번)를 넘었습니다. 잠시 뒤 다시 시도해 주세요.`);
    }
    const last = [...list].reverse().find((a) => a.gap);
    if (last && now - last.at < this.loginGapMs) {
      const wait = Math.ceil((this.loginGapMs - (now - last.at)) / 1000);
      throw new PluginError('RATE_LIMITED', `방금 로그인을 시도했습니다. ${wait}초 뒤에 다시 시도해 주세요.`);
    }
    const attempt = { at: now, gap: true };
    list.push(attempt);
    return attempt;
  }

  async #login(entry, creds, { onDuplicate }) {
    const attempt = this.#checkRate(entry.owner);
    let r;
    try {
      r = await this.login(entry.page, this.baseUrl, creds, { onDuplicate });
    } catch (e) {
      if (e?.code === 'DUPLICATE_SESSION') attempt.gap = false;
      this.logger.info('daouoffice login failed', { owner: entry.owner, code: e?.code ?? 'INTERNAL' });
      await this.#discard(entry, 'login failed');
      throw e;
    }
    entry.loggedInAt = this.now();
    entry.stale = false;
    entry.needsHome = false;
    entry.credKey = credKeyOf(creds);
    this.logger.info('daouoffice login', { owner: entry.owner, kicked: Boolean(r?.kicked) });
  }

  async #getBrowser() {
    if (!this.#browser) {
      const p = Promise.resolve().then(() => this.launch());
      this.#browser = p;
      p.then((b) => b.on?.('disconnected', () => this.#onBrowserGone(p)), () => {});
      p.catch(() => { if (this.#browser === p) this.#browser = null; });
    }
    return this.#browser;
  }

  #onBrowserGone(p) {
    if (this.#browser !== p) return;
    this.#browser = null;
    this.logger.warn('chromium disconnected', { contexts: this.#entries.size });
    for (const e of [...this.#entries.values()]) {
      e.discarded = true;
      if (!e.inUse) void this.#discard(e, 'browser gone');   // in-use ones are cleaned up after their call
    }
  }

  async #closeBrowser() {
    const p = this.#browser;
    this.#browser = null;
    if (!p) return;
    const b = await p.catch(() => null);
    await b?.close?.().catch(() => {});
  }

  /** Takes an active slot: a free one, one freed by parking the least recently used idle context, or the next one in FIFO order. */
  async #acquireSlot() {
    for (;;) {
      if (this.#closed) throw new PluginError('INTERNAL', '플러그인이 종료되는 중입니다.');
      if (this.#active < this.maxContexts && !this.#waiters.length) {
        this.#active++;
        return;
      }
      if (!this.#waiters.length) {
        const idle = [...this.#entries.values()].filter((e) => e.slot && !e.inUse && !e.closing && !e.parked)
          .sort((a, b) => a.lastUsed - b.lastUsed)[0];
        if (idle) {
          await this.#park(idle, 'make room');
          continue;
        }
      }
      await new Promise((resolve, reject) => {
        const w = { resolve, reject, timer: null };
        w.timer = setTimeout(() => {
          const i = this.#waiters.indexOf(w);
          if (i >= 0) this.#waiters.splice(i, 1);
          reject(new PluginError('RATE_LIMITED', '봇이 다른 사람의 다우오피스 작업을 처리하고 있습니다. 잠시 뒤 다시 시도해 주세요.'));
        }, this.queueTimeoutMs);
        w.timer.unref?.();
        this.#waiters.push(w);
      });
      return;   // the slot was handed over
    }
  }

  #releaseSlot() {
    const next = this.#waiters.shift();
    if (next) {
      clearTimeout(next.timer);
      next.resolve();   // the slot passes to the waiter
      return;
    }
    this.#active = Math.max(0, this.#active - 1);
  }

  /** A new context with one page, holding an active slot. Not registered as an account entry. */
  async #openContext({ viewport = { width: 1440, height: 900 }, options = this.contextOptions } = {}) {
    await this.#acquireSlot();
    let context;
    try {
      const browser = await this.#getBrowser();
      const extra = typeof options === 'function' ? await options(browser) : options;
      context = await browser.newContext({ ...CONTEXT_DEFAULTS, viewport, acceptDownloads: true, ...extra });
      const page = await context.newPage();
      return { browser, context, page, slot: true };
    } catch (cause) {
      await context?.close().catch(() => {});
      this.#releaseSlot();
      await this.#maybeCloseBrowser();
      if (cause instanceof PluginError) throw cause;
      throw new PluginError('UPSTREAM', '브라우저를 띄우지 못했습니다.', { cause });
    }
  }

  async #closeContext(c) {
    await c.context.close().catch(() => {});
    if (c.slot) {
      c.slot = false;
      this.#releaseSlot();
    }
    await this.#maybeCloseBrowser();
  }

  /** Registers an opened context as the account's entry. It is in use until `#afterUse`. */
  #adopt(owner, c) {
    const entry = {
      owner, context: c.context, page: c.page, api: null, slot: c.slot,
      loggedInAt: 0, lastUsed: this.now(), credKey: null,
      inUse: true, stale: false, parked: false, needsHome: false, discarded: false, closing: false,
      idleTimer: null, expireTimer: null,
    };
    entry.api = makeApi(entry.page, { onUnauthorized: () => { entry.stale = true; } });
    this.#entries.set(owner, entry);
    return entry;
  }

  async #open(owner) {
    return this.#adopt(owner, await this.#openContext());
  }

  /** Gives a parked entry an active slot and a new page. The next check navigates it home. */
  async #unpark(entry) {
    await this.#acquireSlot();
    entry.slot = true;
    try {
      entry.page = await entry.context.newPage();
    } catch (cause) {
      entry.discarded = true;
      throw new PluginError('UPSTREAM', '브라우저 탭을 열지 못했습니다.', { cause });
    }
    entry.api = makeApi(entry.page, { onUnauthorized: () => { entry.stale = true; } });
    entry.parked = false;
    entry.needsHome = true;
  }

  #cancelTimers(entry) {
    clearTimeout(entry.idleTimer);
    clearTimeout(entry.expireTimer);
    entry.idleTimer = null;
    entry.expireTimer = null;
  }

  #afterUse(entry) {
    entry.inUse = false;
    entry.lastUsed = this.now();
    if (entry.closing || this.#closed) return;
    if (entry.discarded) {
      void this.#discard(entry, 'dead');
      return;
    }
    if (entry.parked) {   // the call failed before it got a page back; keep the context until its session ends
      this.#armExpire(entry);
      return;
    }
    if (this.#waiters.length) {
      // Someone is queued for a slot: park this one now (its session stays) and hand the slot over.
      void this.#park(entry, 'handoff');
      return;
    }
    entry.idleTimer = setTimeout(() => {
      void this.#withLock(entry.owner, async () => {
        if (!entry.inUse && this.#entries.get(entry.owner) === entry) await this.#park(entry, 'idle');
      });
    }, this.idleMs);
    entry.idleTimer.unref?.();
  }

  /**
   * Closes the page and keeps the context (cookies, server session). Frees the active slot.
   * A context whose session is already over is closed instead.
   */
  async #park(entry, reason) {
    if (entry.parked || entry.closing) return;
    if (!this.#sessionLive(entry)) {
      await this.#discard(entry, `${reason}, session over`);
      return;
    }
    this.#cancelTimers(entry);
    entry.parked = true;
    const page = entry.page;
    entry.page = null;
    entry.api = null;
    if (entry.slot) {
      entry.slot = false;
      this.#releaseSlot();
    }
    await page?.close().catch(() => {});
    this.#armExpire(entry);
    this.logger.debug('context parked', { owner: entry.owner, reason });
    await this.#trimParked();
  }

  /** Closes a parked context when its session would have ended anyway. */
  #armExpire(entry) {
    clearTimeout(entry.expireTimer);
    const left = entry.loggedInAt + this.sessionTtlMs - this.now();
    entry.expireTimer = setTimeout(() => { void this.#expire(entry); }, Math.max(0, left));
    entry.expireTimer.unref?.();
  }

  /** Keeps at most `maxParked` parked contexts; the one whose session ends soonest goes first. */
  async #trimParked() {
    const parked = [...this.#entries.values()].filter((e) => e.parked && !e.inUse && !e.closing)
      .sort((a, b) => a.loggedInAt - b.loggedInAt);
    while (parked.length > this.maxParked) {
      const victim = parked.shift();
      this.logger.warn('closing a parked context with a live session', { owner: victim.owner });
      await this.#discard(victim, 'too many parked');
    }
  }

  async #expire(entry) {
    await this.#withLock(entry.owner, async () => {
      if (entry.inUse || entry.closing || this.#entries.get(entry.owner) !== entry) return;
      await this.#discard(entry, 'expired');
    });
  }

  async #discard(entry, reason) {
    if (entry.closing) return;
    entry.closing = true;
    entry.discarded = true;
    this.#cancelTimers(entry);
    if (this.#entries.get(entry.owner) === entry) this.#entries.delete(entry.owner);
    this.logger.debug('context closed', { owner: entry.owner, reason });
    await entry.context.close().catch(() => {});
    if (entry.slot) {
      entry.slot = false;
      this.#releaseSlot();
    }
    await this.#maybeCloseBrowser();
  }

  async #maybeCloseBrowser() {
    if (this.#entries.size || this.#active || this.#waiters.length) return;
    await this.#closeBrowser();
  }
}
