import fs from 'node:fs';
import path from 'node:path';
import { createHash } from 'node:crypto';
import * as z from 'zod';
import { PluginError } from './errors.mjs';
import { runWithMode } from './guard.mjs';
import { kstIso } from './dao/kst.mjs';
import { readProfile } from './dao/org.mjs';

/**
 * Glue between the MCP server and the domain handlers.
 *
 *   rt.tool(server, name, { description, input, kind, role?, account? }, handler)
 *
 * kind     'read'  → readOnlyHint: true; runs right away
 *          'local' → writes only the plugin DB; not gated by the bot
 *          'write' → destructiveHint: true; the bot gates it behind a person's approval.
 *                    The handler must be `{ preview(args, call), execute(args, call) }`.
 *                    `execute` runs only when `_meta["xylolabs/mode"] === "execute"`; any other value
 *                    (or none) runs `preview`.
 *          Every handler runs inside the write guard (src/guard.mjs). Only an approved `execute` may
 *          reach a DAO function that changes DaouOffice; a preview, read or local tool that does
 *          throws instead of submitting. Once a write has started (`assertExecute()`, `markSent()`),
 *          every error carries `data.afterWrite` (outcome unknown; login codes become UPSTREAM), keeps
 *          its original `data`, and its message tells the person to check DaouOffice, not to retry.
 * previewDigest  true for write tools whose execute must do exactly what the preview showed.
 *          The preview returns `digest`: the plan object (title, fields, lines, approvers, file
 *          hashes, target ids…), which the runtime hashes with `digestOf()`, or that hash as a string.
 *          It is stored per (owner, tool, arguments) for 35 minutes in the plugin DB and never sent to
 *          the bot. Execute recomputes the plan and calls `call.assertPreviewed(plan)` (sync; object or
 *          string) before writing: a missing, expired, used or different digest is CONFLICT, and the
 *          stored digest is consumed. Until it passes the guard refuses writes.
 * role     'connect' | 'login' → tool `_meta["xylolabs/role"]`; the bot hides these from the model
 * account  false for read/local tools that work without a connected account (guide); the tool's
 *          `_meta["xylolabs/account"] = false` tells the bot not to ask the person to connect first
 *
 * The handler gets `(args, call)` with
 *   call = { user, creds, allowKick, mode, attachments, owner }
 * where `mode` is 'preview' | 'execute' for write tools and 'read' | 'local' for the others.
 * and returns `{ text, images?: [{ data: Buffer, mimeType }], files?: [{ name, data, mimeType, private }], data? }`.
 * A thrown `PluginError(code, message)` becomes
 *   { isError: true, structuredContent: { code, message }, content: [{ type: 'text', text: message }] }.
 * Anything else becomes INTERNAL with a generic message; the detail goes to the log only.
 *
 * Logs never contain arguments, credentials or DaouOffice responses.
 */

const ANNOTATIONS = {
  read: { readOnlyHint: true, destructiveHint: false },
  local: { readOnlyHint: false, destructiveHint: false },
  write: { readOnlyHint: false, destructiveHint: true },
};

const META = {
  user: 'xylolabs/user',
  credentials: 'xylolabs/credentials',
  allowKick: 'xylolabs/allowKick',
  mode: 'xylolabs/mode',
  attachments: 'xylolabs/attachments',
  role: 'xylolabs/role',
  kind: 'xylolabs/kind',
  account: 'xylolabs/account',
  private: 'xylolabs/private',
  name: 'xylolabs/name',
};

// ------------------------------------------------------------------ logger

const LEVELS = { debug: 10, info: 20, warn: 30, error: 40 };
const SECRET_KEY = /pass|credential|secret|token|cookie/i;

/**
 * JSON lines on stderr (stdout is the MCP channel). Passwords registered with `addSecret` are
 * masked in every string field while a call is running; keys that look like secrets are dropped.
 */
export function createLogger({ stream = process.stderr, level = process.env.DAOUOFFICE_LOG_LEVEL ?? 'info' } = {}) {
  const min = LEVELS[level] ?? LEVELS.info;
  const secrets = new Map();
  /** The secret as written, JSON-escaped and URL-encoded: the forms it can take in a message. */
  const forms = (secret) => [...new Set([secret, JSON.stringify(secret).slice(1, -1), encodeURIComponent(secret)])]
    .filter(Boolean).sort((a, b) => b.length - a.length);
  const mask = (s) => {
    let out = String(s);
    for (const secret of secrets.keys()) for (const f of forms(secret)) out = out.split(f).join('***');
    return out;
  };
  const clean = (v, depth = 0) => {
    if (v instanceof Error) return { message: mask(v.message), code: v.code };
    if (typeof v === 'string') return mask(v);
    if (Array.isArray(v)) return depth > 3 ? '[…]' : v.map((x) => clean(x, depth + 1));
    if (v && typeof v === 'object') {
      if (depth > 3) return '{…}';
      return Object.fromEntries(Object.entries(v).filter(([k]) => !SECRET_KEY.test(k)).map(([k, x]) => [k, clean(x, depth + 1)]));
    }
    return v;
  };
  const write = (lvl, msg, fields = {}) => {
    if (LEVELS[lvl] < min) return;
    try {
      stream.write(`${JSON.stringify({ time: kstIso(), level: lvl, msg: mask(msg), ...clean(fields) })}\n`);
    } catch { /* logging must never break a call */ }
  };
  return {
    debug: (m, f) => write('debug', m, f),
    info: (m, f) => write('info', m, f),
    warn: (m, f) => write('warn', m, f),
    error: (m, f) => write('error', m, f),
    addSecret(s) { if (s) secrets.set(s, (secrets.get(s) ?? 0) + 1); },
    removeSecret(s) {
      if (!s || !secrets.has(s)) return;
      const left = secrets.get(s) - 1;
      if (left > 0) secrets.set(s, left); else secrets.delete(s);
    },
  };
}

// ------------------------------------------------------------------ call object

/** Owner key: the DaouOffice login id, trimmed and lower-cased. */
export function ownerKey(username) {
  const id = String(username ?? '').trim().toLowerCase();
  if (!id || id.length > 128 || /\s/.test(id)) return null;
  return id;
}

function parseUser(v) {
  if (!v || typeof v !== 'object') return { key: null, name: null, platform: null };
  return { key: v.key ?? null, name: v.name ?? null, platform: v.platform ?? null };
}

function parseCreds(v) {
  if (!v || typeof v !== 'object') return null;
  const username = typeof v.username === 'string' ? v.username.trim() : '';
  const password = typeof v.password === 'string' ? v.password : '';
  return username && password ? { username, password } : null;
}

function parseAttachments(v) {
  if (v === undefined || v === null) return [];
  if (!Array.isArray(v)) throw new PluginError('VALIDATION', '첨부 목록 형식이 잘못됐습니다.');
  return v.map((a) => {
    if (!a || typeof a.path !== 'string' || !path.isAbsolute(a.path)) {
      throw new PluginError('VALIDATION', '첨부 파일 경로가 잘못됐습니다.');
    }
    return {
      name: path.basename(String(a.name ?? path.basename(a.path))),
      mimeType: String(a.mimeType ?? 'application/octet-stream'),
      path: a.path,
    };
  });
}

/** Builds the call object from request `_meta` (and, for the connect tool, from the arguments). */
export function buildCall(meta = {}, spec = {}, args = {}) {
  const connect = spec.role === 'connect';
  const creds = connect ? parseCreds(args) : parseCreds(meta[META.credentials]);
  const call = {
    user: parseUser(meta[META.user]),
    creds,
    allowKick: connect ? args.allowKick === true : meta[META.allowKick] === true,
    // Only a write tool has a preview and an execute; read and local tools run as what they are.
    mode: spec.kind === 'write' ? (meta[META.mode] === 'execute' ? 'execute' : 'preview') : spec.kind,
    attachments: parseAttachments(meta[META.attachments]),
    owner: creds ? ownerKey(creds.username) : null,
  };
  if (spec.account !== false && !call.owner) {
    throw new PluginError('NOT_CONNECTED', '다우오피스 계정이 연결되지 않았습니다.');
  }
  return call;
}

// ------------------------------------------------------------------ results

const isPlainObject = (v) => Boolean(v) && typeof v === 'object' && !Array.isArray(v);
const b64 = (data) => (Buffer.isBuffer(data) || data instanceof Uint8Array ? Buffer.from(data).toString('base64') : String(data));

function fileBlock(f) {
  const name = path.basename(String(f.name ?? 'file'));
  const priv = Boolean(f.private);
  return {
    type: 'resource',
    resource: {
      uri: `file:///${encodeURIComponent(name)}`,
      mimeType: f.mimeType ?? 'application/octet-stream',
      blob: b64(f.data),
      _meta: { [META.name]: name, [META.private]: priv },
    },
    _meta: { [META.private]: priv },
  };
}

/** Handler output → MCP CallToolResult. The text block is never empty. */
export function toToolResult(out, { preview = false } = {}) {
  const o = typeof out === 'string' ? { text: out } : (out ?? {});
  const content = [{ type: 'text', text: String(o.text ?? '').trim() || '완료했습니다.' }];
  for (const img of o.images ?? []) {
    content.push({ type: 'image', data: b64(img.data), mimeType: img.mimeType ?? 'image/jpeg' });
  }
  for (const f of o.files ?? []) content.push(fileBlock(f));
  const result = { content };
  let data = o.data;
  if (data !== undefined && !isPlainObject(data)) data = { result: data };
  if (preview) data = { ...(data ?? {}), preview: true };
  if (data !== undefined) result.structuredContent = data;
  return result;
}

export function errorResult(err) {
  const structured = { code: err.code, message: err.message };
  if (err.data && isPlainObject(err.data)) structured.data = err.data;
  return { isError: true, content: [{ type: 'text', text: err.message }], structuredContent: structured };
}

/** Anything thrown → PluginError. Unknown errors keep their detail out of the result. */
export function toPluginError(e) {
  if (e instanceof PluginError) return e;
  if (e instanceof z.ZodError) {
    return new PluginError('VALIDATION', e.issues.map((i) => `${i.path.join('.')}: ${i.message}`).join('; '));
  }
  if (e?.name === 'TimeoutError') {
    return new PluginError('UPSTREAM', '다우오피스 화면이 제때 응답하지 않았습니다. 잠시 뒤 다시 시도해 주세요.', { cause: e });
  }
  return new PluginError('INTERNAL', '플러그인 내부 오류가 발생했습니다.', { cause: e });
}

// ------------------------------------------------------------------ previews

/** JSON with object keys sorted at every level, so equal values give equal text. */
export function canonicalJson(value) {
  return JSON.stringify(value, (key, v) => (v && typeof v === 'object' && !Array.isArray(v) && !Buffer.isBuffer(v)
    ? Object.fromEntries(Object.keys(v).sort().map((k) => [k, v[k]]))
    : v));
}

/** sha256 of the canonical JSON. What a preview showed, as one string. */
export function digestOf(value) {
  return createHash('sha256').update(canonicalJson(value ?? null)).digest('hex');
}

export const PREVIEW_TTL_MS = 35 * 60_000;

/** Preview digests in the plugin DB (they survive a restart); a Map when there is no DB (tests). */
function previewStore(db) {
  if (db?.putPreview) {
    return {
      put: (k, digest) => db.putPreview({ ...k, digest, ttlMs: PREVIEW_TTL_MS }),
      get: (k) => db.previewDigest(k),
      drop: (k) => db.dropPreview(k),
    };
  }
  const mem = new Map();
  const id = (k) => `${k.owner}\u0000${k.tool}\u0000${k.argsKey}`;
  return {
    put: (k, digest) => mem.set(id(k), { digest, until: Date.now() + PREVIEW_TTL_MS }),
    get: (k) => { const r = mem.get(id(k)); return r && r.until > Date.now() ? r.digest : null; },
    drop: (k) => mem.delete(id(k)),
  };
}

// ------------------------------------------------------------------ runtime

/** Codes the bot reads as "nothing happened, offer it again"; unsafe once a write has started. */
const AFTER_WRITE_UNSAFE = new Set(['AUTH_FAILED', 'DUPLICATE_SESSION', 'NOT_CONNECTED', 'RATE_LIMITED']);

const CHECK_FIRST = '다시 올리기 전에 다우오피스에서 문서를 확인해 주세요.';

/**
 * Any error once a write has started: the outcome is unknown. `data.afterWrite` makes the bot say so
 * and mark the action failed instead of offering it again, the original `data` (document ids and the
 * like) is kept, and the message never suggests simply retrying.
 */
export function afterWriteError(err, thrown) {
  let message;
  if (AFTER_WRITE_UNSAFE.has(err.code)) message = '다우오피스 로그인이 끊겨 결과를 확인하지 못했습니다.';
  else if (thrown?.name === 'TimeoutError') message = '다우오피스 화면이 제때 응답하지 않아 결과를 확인하지 못했습니다.';
  else if (err.code === 'INTERNAL') message = '플러그인 내부 오류로 결과를 확인하지 못했습니다.';
  else message = err.message.replace(/\s*(잠시 뒤 )?다시 시도해 주세요\.?/g, '').trim();
  if (!/다우오피스에서 (문서를 )?확인|내 기안함/.test(message)) message = `${message} ${CHECK_FIRST}`;
  return new PluginError(AFTER_WRITE_UNSAFE.has(err.code) ? 'UPSTREAM' : err.code, message, {
    cause: err.cause ?? err,
    data: { ...(err.data ?? {}), afterWrite: true, original: err.code },
  });
}

export function createRuntime({ config, db, sessions, dataDir, logger = createLogger() }) {
  const previews = previewStore(db);
  const rt = {
    config, db, sessions, dataDir, logger,

    tool(server, name, spec, handler) {
      const { description, input, kind, role, title } = spec;
      if (!ANNOTATIONS[kind]) throw new TypeError(`tool ${name}: kind must be read, local or write`);
      if (kind === 'write') {
        if (typeof handler?.preview !== 'function' || typeof handler?.execute !== 'function') {
          throw new TypeError(`write tool ${name} needs { preview, execute }`);
        }
      } else if (typeof handler !== 'function') {
        throw new TypeError(`tool ${name}: handler must be a function`);
      }
      return server.registerTool(name, {
        ...(title ? { title } : {}),
        description,
        inputSchema: input ?? z.object({}),
        annotations: ANNOTATIONS[kind],
        _meta: {
          [META.kind]: kind,
          ...(role ? { [META.role]: role } : {}),
          ...(spec.account === false && kind !== 'write' ? { [META.account]: false } : {}),
        },
      }, (args, ctx) => rt.invoke(name, spec, handler, args ?? {}, ctx?.mcpReq?._meta ?? {}));
    },

    /** Runs one call. Exposed for tests that do not go through a transport. */
    async invoke(name, spec, handler, args, meta) {
      const started = Date.now();
      let call = null;
      const guard = {};
      try {
        call = buildCall(meta, spec, args);
        logger.addSecret(call.creds?.password);
        const write = spec.kind === 'write';
        const execute = write && call.mode === 'execute';
        const fn = write ? (execute ? handler.execute : handler.preview) : handler;
        const key = { owner: call.owner ?? '', tool: name, argsKey: digestOf(args) };
        if (write && spec.previewDigest && execute) {
          guard.requirePreviewed = true;
          call.assertPreviewed = (plan) => {
            const digest = typeof plan === 'string' ? plan : digestOf(plan);
            const shown = previews.get(key);
            if (!shown || shown !== digest) {
              throw new PluginError('CONFLICT', shown
                ? '미리보기 뒤에 내용이 바뀌었습니다. 미리보기를 다시 만듭니다.'
                : '승인한 미리보기를 찾지 못했습니다(만료됐거나 다른 내용입니다). 미리보기를 다시 만듭니다.',
              { data: { previewChanged: true } });
            }
            previews.drop(key);   // one approval, one execute
            guard.previewed = true;
          };
        }
        // Only an approved write may change DaouOffice; read and local tools run under their own
        // kind so the guard refuses them too.
        let out = await runWithMode(write ? call.mode : spec.kind, () => fn(args, call), guard);
        if (write && spec.previewDigest && !execute) {
          if (out?.digest === undefined || out.digest === null) throw new TypeError(`write tool ${name}: preview must return a digest`);
          previews.put(key, typeof out.digest === 'string' ? out.digest : digestOf(out.digest));
        }
        if (out && typeof out === 'object' && 'digest' in out) {
          const { digest, ...rest } = out;
          out = rest;
        }
        logger.info('tool call', { tool: name, user: call.user.key, owner: call.owner, mode: call.mode, ms: Date.now() - started, outcome: 'ok' });
        return toToolResult(out, { preview: write && !execute });
      } catch (e) {
        let err = toPluginError(e);
        if (guard.wrote && !err.data?.afterWrite) err = afterWriteError(err, e);
        const cause = err.cause ?? (err === e ? null : e);
        const fields = { tool: name, user: call?.user.key ?? null, owner: call?.owner ?? null, ms: Date.now() - started, outcome: err.code };
        if (err.code === 'INTERNAL') logger.error('tool call failed', { ...fields, error: cause ?? err, stack: cause?.stack ?? err.stack });
        else logger.info('tool call refused', { ...fields, ...(cause ? { cause } : {}) });
        return errorResult(err);
      } finally {
        logger.removeSecret(call?.creds?.password);
      }
    },

    /**
     * Runs `fn({ page, api, context, owner, baseUrl, fresh })` on the caller's DaouOffice session.
     * After a fresh login the profile (name, department) is refreshed in `accounts`.
     */
    async session(call, fn, { allowKick } = {}) {
      return sessions.withSession(call.owner, call.creds, { allowKick: allowKick ?? call.allowKick }, async (s) => {
        if (s.fresh || !db.account(call.owner)?.name) {
          await rt.refreshProfile(call.owner, s.api)
            .catch((e) => logger.warn('profile refresh failed', { owner: call.owner, error: toPluginError(e).message }));
        }
        return fn(s);
      });
    },

    /** Reads name and department from DaouOffice and stores them. Returns the account row. */
    async refreshProfile(owner, api) {
      const p = await readProfile(api, { fallbacks: config.departments ?? {} });
      return db.upsertAccount({ owner, ...p, data: { profileAt: kstIso() } });
    },

    /** `홍길동 (연구개발팀)`, or the login id when the profile is unknown. */
    accountName(account, owner) {
      if (!account?.name) return owner ?? account?.owner ?? '';
      return account.deptName ? `${account.name} (${account.deptName})` : account.name;
    },

    /** `$DATA_DIR/files/<owner>/<kind>`, created on demand. */
    filesDir(owner, kind) {
      const safeOwner = String(owner).replace(/[^a-z0-9._@-]/gi, '_');
      const safeKind = String(kind).replace(/[^a-z0-9_-]/gi, '_');
      const dir = path.join(dataDir, 'files', safeOwner, safeKind);
      fs.mkdirSync(dir, { recursive: true });
      return dir;
    },
  };
  return rt;
}
