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
 * - Closing a context does not end the server session. A new login within its lifetime sees
 *   our own leftover as "another session". So an idle context is first **parked** (its page goes
 *   to about:blank, freeing the heavy SPA renderer, cookies kept) and only closed once its
 *   session has expired or its slot is needed by someone else.
 *
 * Capacity: at most `maxContexts` live contexts. A request that needs a new one evicts the
 * least recently used idle context, or waits in a FIFO queue. The browser closes when the last
 * context closes. Work for one account runs one call at a time (per-owner lock).
 *
 * Login rate limit per account: one attempt per `loginGapMs` and `loginsPerHour` attempts per
 * hour. An attempt that only met the duplicate modal does not start the gap, so the person's
 * "끊고 진행" right after it is not refused.
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

export class SessionManager {
  #entries = new Map();     // owner → entry
  #locks = new Map();       // owner → tail promise
  #waiters = [];            // FIFO of { resolve, reject, timer }
  #slotsUsed = 0;
  #browser = null;          // Promise<Browser>
  #attempts = new Map();    // owner → [{ at, gap }]
  #closed = false;

  constructor({
    baseUrl,
    maxContexts = 2,
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
      maxContexts, idleMs, sessionTtlMs, queueTimeoutMs, loginGapMs, loginsPerHour,
      launch, login, checkSession, contextOptions, logger, now,
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
      entry ??= await this.#open(owner);
      this.#cancelTimers(entry);
      entry.inUse = true;
      try {
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
   * Checks credentials for the connect flow. Reuses a live session only when it was opened with
   * the same credentials; otherwise logs in again without kicking anyone. `{ duplicate: true }`
   * means the password was right but the account is in use elsewhere; the context is closed.
   */
  async verify(owner, creds) {
    this.#assertUsable(owner, creds);
    return this.#withLock(owner, async () => {
      let entry = this.#entries.get(owner);
      if (entry && (this.#dead(entry) || entry.credKey !== credKeyOf(creds))) {
        await this.#discard(entry, 'reverify');
        entry = undefined;
      }
      entry ??= await this.#open(owner);
      this.#cancelTimers(entry);
      entry.inUse = true;
      try {
        if (await this.#alive(entry)) return { duplicate: false, fresh: false };
        const r = await this.#login(entry, creds, { onDuplicate: 'report' });
        if (r.duplicate) {
          await this.#discard(entry, 'duplicate');
          return { duplicate: true, fresh: false };
        }
        return { duplicate: false, fresh: true };
      } finally {
        this.#afterUse(entry);
      }
    });
  }

  /** Drops the account's context (after a disconnect, for example). */
  async forget(owner) {
    return this.#withLock(owner, async () => {
      const entry = this.#entries.get(owner);
      if (entry) await this.#discard(entry, 'forget');
      this.#attempts.delete(owner);
    });
  }

  stats() {
    return {
      contexts: this.#entries.size,
      slots: this.#slotsUsed,
      waiting: this.#waiters.length,
      browser: Boolean(this.#browser),
      owners: [...this.#entries.keys()],
      parked: [...this.#entries.values()].filter((e) => e.parked).map((e) => e.owner),
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
    this.#slotsUsed = 0;
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
    if (entry.page.isClosed?.()) return true;
    return entry.context.browser?.()?.isConnected?.() === false;
  }

  async #alive(entry) {
    if (!entry.loggedInAt || entry.stale) return false;
    if (this.now() - entry.loggedInAt >= this.sessionTtlMs) return false;
    try {
      if (entry.parked) {
        await entry.page.goto(`${this.baseUrl}/home`, { waitUntil: 'domcontentloaded', timeout: 60_000 });
        entry.parked = false;
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

  #checkRate(owner) {
    const now = this.now();
    const list = (this.#attempts.get(owner) ?? []).filter((a) => now - a.at < 3600_000);
    this.#attempts.set(owner, list);
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
    if (r?.status === 'duplicate') {
      attempt.gap = false;
      this.logger.info('daouoffice login met duplicate session', { owner: entry.owner });
      return { duplicate: true };
    }
    entry.loggedInAt = this.now();
    entry.stale = false;
    entry.parked = false;
    entry.credKey = credKeyOf(creds);
    this.logger.info('daouoffice login', { owner: entry.owner, kicked: Boolean(r?.kicked) });
    return { duplicate: false };
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

  async #acquireSlot() {
    if (this.#slotsUsed < this.maxContexts && !this.#waiters.length) {
      this.#slotsUsed++;
      return;
    }
    if (!this.#waiters.length) {
      const idle = [...this.#entries.values()].filter((e) => !e.inUse && !e.closing)
        .sort((a, b) => a.lastUsed - b.lastUsed)[0];
      if (idle) {
        await this.#discard(idle, 'evicted', { handoff: true });
        return;
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
  }

  #releaseSlot() {
    const next = this.#waiters.shift();
    if (next) {
      clearTimeout(next.timer);
      next.resolve();   // the slot passes to the waiter
      return;
    }
    this.#slotsUsed = Math.max(0, this.#slotsUsed - 1);
  }

  async #open(owner) {
    await this.#acquireSlot();
    try {
      const browser = await this.#getBrowser();
      const context = await browser.newContext({
        locale: 'ko-KR',
        timezoneId: 'Asia/Seoul',
        viewport: { width: 1440, height: 900 },
        acceptDownloads: true,
        ...this.contextOptions,
      });
      const page = await context.newPage();
      const entry = {
        owner, context, page, api: null,
        loggedInAt: 0, lastUsed: this.now(), credKey: null,
        inUse: false, stale: false, parked: false, discarded: false, closing: false,
        idleTimer: null, expireTimer: null,
      };
      entry.api = makeApi(page, { onUnauthorized: () => { entry.stale = true; } });
      this.#entries.set(owner, entry);
      return entry;
    } catch (cause) {
      this.#releaseSlot();
      await this.#maybeCloseBrowser();
      if (cause instanceof PluginError) throw cause;
      throw new PluginError('UPSTREAM', '브라우저를 띄우지 못했습니다.', { cause });
    }
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
    if (this.#waiters.length) {
      // Someone is queued for a slot: hand this one over now instead of parking it.
      void this.#discard(entry, 'handoff');
      return;
    }
    entry.idleTimer = setTimeout(() => { void this.#park(entry); }, this.idleMs);
    entry.idleTimer.unref?.();
  }

  async #park(entry) {
    await this.#withLock(entry.owner, async () => {
      if (entry.inUse || entry.closing || this.#entries.get(entry.owner) !== entry) return;
      const left = entry.loggedInAt ? entry.loggedInAt + this.sessionTtlMs - this.now() : 0;
      if (left <= 0) {
        await this.#discard(entry, 'idle');
        return;
      }
      await entry.page.goto('about:blank').catch(() => {});
      if (entry.closing) return;
      entry.parked = true;
      entry.expireTimer = setTimeout(() => { void this.#expire(entry); }, left);
      entry.expireTimer.unref?.();
    });
  }

  async #expire(entry) {
    await this.#withLock(entry.owner, async () => {
      if (entry.inUse || entry.closing || this.#entries.get(entry.owner) !== entry) return;
      await this.#discard(entry, 'expired');
    });
  }

  async #discard(entry, reason, { handoff = false } = {}) {
    if (entry.closing) return;
    entry.closing = true;
    entry.discarded = true;
    this.#cancelTimers(entry);
    if (this.#entries.get(entry.owner) === entry) this.#entries.delete(entry.owner);
    this.logger.debug('context closed', { owner: entry.owner, reason });
    await entry.context.close().catch(() => {});
    if (!handoff) this.#releaseSlot();
    await this.#maybeCloseBrowser();
  }

  async #maybeCloseBrowser() {
    if (this.#entries.size || this.#slotsUsed || this.#waiters.length) return;
    await this.#closeBrowser();
  }
}
