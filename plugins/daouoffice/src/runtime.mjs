import fs from 'node:fs';
import path from 'node:path';
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
 *          throws instead of submitting. Once a write has started, a later AUTH_FAILED,
 *          DUPLICATE_SESSION, NOT_CONNECTED or RATE_LIMITED is reported as UPSTREAM with
 *          `data.afterWrite`, because the bot re-offers actions that fail with those codes.
 * role     'connect' | 'login' → tool `_meta["xylolabs/role"]`; the bot hides these from the model
 * account  false for tools that work without a connected account (guide)
 *
 * The handler gets `(args, call)` with
 *   call = { user, creds, allowKick, mode, attachments, owner }
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
  const mask = (s) => {
    let out = String(s);
    for (const secret of secrets.keys()) if (secret) out = out.split(secret).join('***');
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
    mode: spec.kind === 'write' ? (meta[META.mode] === 'execute' ? 'execute' : 'preview') : 'execute',
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

// ------------------------------------------------------------------ runtime

/** Codes the bot reads as "nothing happened, offer it again"; unsafe once a write has started. */
const AFTER_WRITE_UNSAFE = new Set(['AUTH_FAILED', 'DUPLICATE_SESSION', 'NOT_CONNECTED', 'RATE_LIMITED']);

export function createRuntime({ config, db, sessions, dataDir, logger = createLogger() }) {
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
        _meta: { [META.kind]: kind, ...(role ? { [META.role]: role } : {}) },
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
        // Only an approved write may change DaouOffice; read and local tools run under their own
        // kind so the guard refuses them too.
        const out = await runWithMode(write ? call.mode : spec.kind, () => fn(args, call), guard);
        logger.info('tool call', { tool: name, user: call.user.key, owner: call.owner, mode: call.mode, ms: Date.now() - started, outcome: 'ok' });
        return toToolResult(out, { preview: write && !execute });
      } catch (e) {
        let err = toPluginError(e);
        if (guard.wrote && AFTER_WRITE_UNSAFE.has(err.code)) {
          // The bot treats these codes as "nothing was submitted" and re-offers the action. Once a
          // write has started that is no longer known, so report an unknown outcome instead.
          err = new PluginError('UPSTREAM',
            '제출을 시작한 뒤 다우오피스 로그인이 끊겨 결과를 확인하지 못했습니다. 다시 올리기 전에 다우오피스에서 문서를 확인해 주세요.',
            { cause: err, data: { afterWrite: true, original: err.code } });
        }
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
