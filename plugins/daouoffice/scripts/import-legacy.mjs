#!/usr/bin/env node
/**
 * 한 사람의 옛 기록을 원본 저장소(xylolabs-daouoffice)에서 플러그인 DB 로 옮긴다. 한 번만 쓴다.
 *
 *   node --disable-warning=ExperimentalWarning scripts/import-legacy.mjs \
 *     --repo /path/to/xylolabs-daouoffice --owner <다우오피스 아이디> [--db <daouoffice.db>] \
 *     [--data-dir <DAOUOFFICE_DATA_DIR>] [--replace] [--dry-run]
 *
 * 옮기는 것 (원본 → DB)
 *
 *   src/purchases.mjs  PURCHASES (refiled.json 을 이미 합친 것)  → purchases, quotes, filings(kind purchase)
 *   src/trips.mjs      TRIPS                                      → trips, filings(kind trip)
 *   src/card-match.mjs REFUNDED, HANDLED_ELSEWHERE, PINNED, CLAIM_AS_IS → overrides
 *   src/inspection.mjs INSPECTION_DONE, INSPECTION_INCOMPLETE, ITEM_OVERRIDES → overrides
 *   src/trip-expenses.mjs CONFIRMED (내보내지 않는 표라 읽을 때만 export 를 덧붙인다) → overrides(kind tripExpense)
 *   records/           cards/<끝4자리>.json, ali-orders/<날짜>.json → filings(kind record:…)
 *   output/quotes/     견적 캡처 파일(있으면)                      → files/<owner>/quotes
 *
 * 원본 저장소는 읽기만 한다. 모듈이 시작할 때 상대 경로(`pictures/_matches.json` 등)를 읽으므로
 * 작업 디렉터리를 원본 저장소로 바꾼 뒤 불러온다. 원본에 `node_modules` 가 없으면 `playwright`
 * 를 빈 모듈로 바꿔 끼운다(옮기는 모듈은 브라우저를 쓰지 않는다).
 *
 * 이미 DB 에 있는 품의와 출장은 건너뛴다(`--replace` 면 덮어쓴다. 원본에 시리얼이 없으면 DB 의
 * `data.serials` 는 남긴다). 예외 표는 같은 줄이 있으면 건너뛴다. 출력은 건수뿐이다. 이름, 금액,
 * 문서 번호를 찍지 않는다.
 */
import fs from 'node:fs';
import path from 'node:path';
import module from 'node:module';
import { createHash } from 'node:crypto';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { openDb } from '../src/db.mjs';
import { ownerKey } from '../src/runtime.mjs';
import { purchaseRow, validatePurchase, RECEIPT_LIKE } from '../src/dao/purchases.mjs';
import { loadConfig } from '../src/config.mjs';
import { tripKey, TRIP_STATUS } from '../src/dao/trips.mjs';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');

export function parseArgs(argv) {
  const out = { dryRun: false, replace: false };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    const val = () => {
      const v = argv[++i];
      if (v === undefined || v.startsWith('--')) throw new Error(`${a} 에 값이 없다`);
      return v;
    };
    if (a === '--repo') out.repo = val();
    else if (a === '--owner') out.owner = val();
    else if (a === '--db') out.db = val();
    else if (a === '--data-dir') out.dataDir = val();
    else if (a === '--config') out.config = val();
    else if (a === '--dry-run') out.dryRun = true;
    else if (a === '--replace') out.replace = true;
    else throw new Error(`모르는 인자: ${a}`);
  }
  if (!out.repo) throw new Error('--repo <원본 저장소 경로> 가 필요하다');
  if (!out.owner) throw new Error('--owner <다우오피스 아이디> 가 필요하다');
  return out;
}

// ------------------------------------------------------------------ mapping (pure)

const DOC_IN_SKIP = /\((\d{15,})\)/;
/** 원본 품의 한 건의 상태. 문서 ID 가 있거나 skip 에 문서 ID 가 있으면 올린 것이다. */
export function purchaseStatus(p) {
  const docId = p.docId ?? String(p.skip ?? '').match(DOC_IN_SKIP)?.[1] ?? null;
  if (docId) return { status: 'submitted', docId: String(docId), hold: null };
  // 카드 승인을 기다리는 건처럼 skip 만 있고 문서가 없으면 아직 올리지 않은 초안이다.
  return { status: 'draft', docId: null, hold: p.skip ? String(p.skip) : null };
}

/** 원본 품의 칸 가운데 DB 엔트리로 옮기는 것. `quotes`, `skip` 은 따로 다룬다. */
const PURCHASE_FIELDS = [
  'title', 'budget', 'project', 'subProject', 'item', 'vendor', 'purpose', 'paidAt', 'purchaseDate',
  'deliveryDate', 'qty', 'amount', 'usd', 'paidKrw', 'vat', 'payWhen', 'payMethod', 'cardIssuer',
  'cardLast4', 'account', 'accountHolder', 'note', 'serials', 'refiledFrom',
];

/**
 * 원본 품의 → 플러그인 엔트리. 결제 전 외화 건은 원본이 고정해 둔 `FX` 로 환산했으므로 그 값을
 * 초안의 `fx` 로 남긴다. 그래야 금액이 올린 문서와 같다.
 */
export function mapPurchase(p, { fx } = {}) {
  const { status, docId, hold } = purchaseStatus(p);
  const entry = { key: String(p.key), status, docId: docId ?? undefined, revision: 1, imported: true };
  for (const f of PURCHASE_FIELDS) if (p[f] !== undefined && p[f] !== null) entry[f] = p[f];
  if (entry.cardLast4 != null) entry.cardLast4 = String(entry.cardLast4);
  if (!Array.isArray(entry.note)) entry.note = entry.note ? [String(entry.note)] : [];
  if (entry.usd != null && entry.paidKrw == null && fx?.rate) entry.fx = { rate: fx.rate, at: fx.at, source: fx.source };
  if (hold) entry.hold = hold;
  if (status === 'submitted' && p.skip) entry.filedNote = String(p.skip);
  return entry;
}

/** 원본 skip 문구 앞의 날짜(`2026-09-21 재상신 …`). 없으면 null. */
export const skipDate = (skip) => String(skip ?? '').match(/^(\d{4}-\d{2}-\d{2})/)?.[1] ?? null;

/**
 * 출장 상태(`TRIP_STATUS`). 문서가 있으면 submitted, 다른 사람이 올렸으면 elsewhere, 취소했으면
 * cancelled, 그 밖의 skip 은 올린 것(submitted), skip 이 없으면 draft.
 */
export function tripStatus(t) {
  const skip = String(t.skip ?? '');
  if (t.docId) return TRIP_STATUS.submitted;
  if (/다른 (사람|분)/.test(skip)) return TRIP_STATUS.elsewhere;
  if (/취소|안 가|가지 않/.test(skip)) return TRIP_STATUS.cancelled;
  return skip ? TRIP_STATUS.submitted : TRIP_STATUS.draft;
}

/**
 * 원본 가맹점 정규식 → 대소문자를 가리지 않는 글자 조각들(`overrideMatches` 는 글자 조각만 본다).
 * `A|B` 는 둘로 나눈다. 글자 조각으로 옮길 수 없는 식이면 null.
 */
export function regexToSubstrings(re) {
  const raw = String(re?.source ?? re ?? '').split('|').map((x) => x.replace(/^\^|\$$/g, '').replace(/\\s[+*]?/g, ' '));
  // 이스케이프하지 않은 메타 문자가 남으면 글자 조각이 아니다(`\.` 같은 이스케이프는 글자로 푼다).
  if (raw.some((x) => /(^|[^\\])[.*+?()[\]{}^$]/.test(x))) return null;
  const parts = raw.map((x) => x.replace(/\\(.)/g, '$1').trim());
  return parts.length && parts.every(Boolean) ? parts : null;
}

/** card-match.mjs, inspection.mjs 의 표 → overrides 행 `{ kind, match, data }`. */
export function mapOverrides({ REFUNDED = [], HANDLED_ELSEWHERE = [], PINNED = [], CLAIM_AS_IS = [], INSPECTION_DONE = {}, INSPECTION_INCOMPLETE = {}, ITEM_OVERRIDES = {}, TRIP_CONFIRMED = [] }, { warn = () => {} } = {}) {
  const card = (x) => ({ ...(x.date ? { date: x.date } : {}), merchant: x.merchant, amount: x.amount });
  const rest = (x, drop) => Object.fromEntries(Object.entries(x).filter(([k]) => !drop.includes(k)));
  const rows = [];
  for (const x of REFUNDED) rows.push({ kind: 'refunded', match: card(x), data: rest(x, ['date', 'merchant', 'amount']) });
  for (const x of HANDLED_ELSEWHERE) rows.push({ kind: 'elsewhere', match: card(x), data: rest(x, ['date', 'merchant', 'amount']) });
  for (const x of PINNED) rows.push({ kind: 'pinned', match: card(x), data: rest(x, ['date', 'merchant', 'amount']) });
  for (const x of CLAIM_AS_IS) rows.push({ kind: 'claimAsIs', match: card(x), data: rest(x, ['date', 'merchant', 'amount']) });
  for (const [key, why] of Object.entries(INSPECTION_DONE)) rows.push({ kind: 'inspectionDone', match: { key }, data: { why } });
  for (const [key, missing] of Object.entries(INSPECTION_INCOMPLETE)) rows.push({ kind: 'inspectionIncomplete', match: { key }, data: { missing } });
  for (const [key, items] of Object.entries(ITEM_OVERRIDES)) rows.push({ kind: 'itemOverride', match: { key }, data: { rows: items } });
  // 출장 경비 확인(사람이 알려 준 기간 밖 결제, 출장이 아닌 업무 경비). 출장은 시작일로 잇는다.
  for (const x of TRIP_CONFIRMED) {
    const merchants = regexToSubstrings(x.merchant);
    if (!merchants || !x.date || !x.kind) { warn('출장 경비 확인 한 줄을 옮기지 못했다(날짜, 종류가 없거나 가맹점 식이 글자 조각이 아니다)'); continue; }
    const data = { kind: x.kind, ...(x.trip ? { trip: x.trip } : {}), ...(x.detail ? { detail: x.detail } : {}) };
    for (const merchant of merchants) rows.push({ kind: 'tripExpense', match: { date: x.date, merchant }, data });
  }
  return rows;
}

const stable = (v) => JSON.stringify(v, Object.keys(v ?? {}).sort());
const sameOverride = (a, b) => a.kind === b.kind && stable(a.match) === stable(b.match) && JSON.stringify(a.data) === JSON.stringify(b.data);

// ------------------------------------------------------------------ loading the private repo

/**
 * 원본 모듈을 불러온다. 작업 디렉터리를 원본으로 바꾸고, `playwright` 가 없으면 빈 모듈로 바꾼다.
 * 모듈이 없거나 실패하면 그 모듈만 빈 값으로 두고 경고를 남긴다.
 */
export async function loadLegacy(repo, { warn = () => {} } = {}) {
  const src = path.join(repo, 'src');
  if (!fs.existsSync(path.join(src, 'purchases.mjs'))) throw new Error(`원본 저장소가 아니다(src/purchases.mjs 없음): ${repo}`);
  // 모듈 URL 은 심볼릭 링크를 푼 경로다(macOS 의 /var → /private/var). 둘 다 받는다.
  const srcUrls = [...new Set([src, fs.realpathSync(src)])].map((d) => pathToFileURL(`${d}${path.sep}`).href);
  module.registerHooks?.({
    // trip-expenses.mjs 의 CONFIRMED 는 내보내지 않는 표다. 원본은 고치지 않고, 읽은 소스 끝에만 export 를 붙인다.
    load(url, context, next) {
      const out = next(url, context);
      if (srcUrls.some((u) => url.startsWith(u)) && url.endsWith('/trip-expenses.mjs')) {
        const text = String(out.source);
        if (/\bconst CONFIRMED\b/.test(text)) return { ...out, source: `${text}\nexport { CONFIRMED as __LEGACY_CONFIRMED__ };\n` };
      }
      return out;
    },
    resolve(specifier, context, next) {
      try {
        return next(specifier, context);
      } catch (e) {
        if (specifier === 'playwright' || specifier.startsWith('playwright/')) {
          return { url: 'data:text/javascript,export const chromium = {}; export default {};', shortCircuit: true };
        }
        throw e;
      }
    },
  });
  const cwd = process.cwd();
  process.chdir(repo);
  const load = async (file) => {
    try {
      return await import(pathToFileURL(path.join(src, file)).href);
    } catch (e) {
      warn(`${file} 을 불러오지 못했다: ${e?.code ?? e?.message ?? e}`);
      return {};
    }
  };
  try {
    const purchases = await load('purchases.mjs');
    const trips = await load('trips.mjs');
    const cardMatch = await load('card-match.mjs');
    const inspection = await load('inspection.mjs');
    const tripExpenses = fs.existsSync(path.join(src, 'trip-expenses.mjs')) ? await load('trip-expenses.mjs') : null;
    if (tripExpenses && !tripExpenses.__LEGACY_CONFIRMED__) warn('trip-expenses.mjs 에서 CONFIRMED 를 찾지 못했다');
    return {
      PURCHASES: purchases.PURCHASES ?? [],
      FX: purchases.FX ?? null,
      REFILED: purchases.REFILED ?? {},
      TRIPS: trips.TRIPS ?? [],
      REFUNDED: cardMatch.REFUNDED ?? [],
      HANDLED_ELSEWHERE: cardMatch.HANDLED_ELSEWHERE ?? [],
      PINNED: cardMatch.PINNED ?? [],
      CLAIM_AS_IS: cardMatch.CLAIM_AS_IS ?? [],
      INSPECTION_DONE: inspection.INSPECTION_DONE ?? {},
      INSPECTION_INCOMPLETE: inspection.INSPECTION_INCOMPLETE ?? {},
      ITEM_OVERRIDES: inspection.ITEM_OVERRIDES ?? {},
      TRIP_CONFIRMED: tripExpenses?.__LEGACY_CONFIRMED__ ?? [],
    };
  } finally {
    process.chdir(cwd);
  }
}

/** records/ 아래 JSON. `[{ kind, ref, detail }]`. 읽을 수 없는 파일은 건너뛴다. */
export function loadRecords(repo, { warn = () => {} } = {}) {
  const out = [];
  for (const [dir, kind] of [['cards', 'record:cards'], ['ali-orders', 'record:ali-orders']]) {
    const full = path.join(repo, 'records', dir);
    let names = [];
    try { names = fs.readdirSync(full).filter((n) => n.endsWith('.json')).sort(); } catch { continue; }
    for (const name of names) {
      try {
        out.push({ kind, ref: name.slice(0, -5), detail: JSON.parse(fs.readFileSync(path.join(full, name), 'utf8')) });
      } catch {
        warn(`records/${dir}/${name} 을 읽지 못했다`);
      }
    }
  }
  return out;
}

// ------------------------------------------------------------------ import

const safeName = (n) => path.basename(String(n)).normalize('NFC').replace(/[^0-9A-Za-z가-힣._-]+/g, '_');

/**
 * 옮긴다. `dryRun` 이면 DB 와 파일을 건드리지 않고 건수만 센다. 돌려주는 값은 건수 표다.
 */
export function importLegacy({ legacy, records = [], db, owner, filesDir, repo, dryRun = false, replace = false, config = null, warn = () => {} }) {
  const counts = {
    purchases: { found: legacy.PURCHASES.length, submitted: 0, draft: 0, inserted: 0, replaced: 0, skippedExisting: 0, refiled: 0, withFx: 0 },
    drafts: { valid: 0, withErrors: 0 },
    quotes: { found: 0, copied: 0, missingFile: 0, receiptLike: 0 },
    trips: { found: legacy.TRIPS.length, submitted: 0, elsewhere: 0, cancelled: 0, draft: 0, inserted: 0, replaced: 0, skippedExisting: 0 },
    overrides: {},
    filings: { purchase: 0, trip: 0, record: 0, skippedExisting: 0 },
    records: { found: records.length },
  };
  const write = (fn) => (dryRun ? null : fn());
  const quoteSrc = path.join(repo, 'output', 'quotes');
  const existingFilings = (kind, ref) => db.filings(owner, { kind, ref });

  for (const p of legacy.PURCHASES) {
    const entry = mapPurchase(p, { fx: legacy.FX });
    counts.purchases[entry.status]++;
    if (entry.refiledFrom) counts.purchases.refiled++;
    if (entry.fx) counts.purchases.withFx++;
    if (entry.status === 'draft' && config) {
      const quotesForCheck = (p.quotes ?? []).map((q) => ({ file: q.file, url: q.url, manual: q.manual }));
      const { errors } = validatePurchase({ ...entry, quotes: quotesForCheck }, { config });
      counts.drafts[errors.length ? 'withErrors' : 'valid']++;
    }
    const had = db.purchase(owner, entry.key);
    if (had && !replace) { counts.purchases.skippedExisting++; continue; }
    counts.purchases[had ? 'replaced' : 'inserted']++;
    // 검수조서 쪽(save_photos)이 DB 에 적은 시리얼은 원본에 없으면 남긴다.
    if (entry.serials === undefined && had?.data?.serials !== undefined) entry.serials = had.data.serials;
    const row = write(() => db.upsertPurchase(purchaseRow(owner, entry)));

    for (const q of p.quotes ?? []) {
      counts.quotes.found++;
      if (RECEIPT_LIKE.test(q.file ?? '')) { counts.quotes.receiptLike++; continue; }
      const from = path.join(quoteSrc, String(q.file));
      if (!fs.existsSync(from)) { counts.quotes.missingFile++; continue; }
      counts.quotes.copied++;
      write(() => {
        const buf = fs.readFileSync(from);
        const name = safeName(q.file);
        fs.mkdirSync(filesDir, { recursive: true });
        fs.writeFileSync(path.join(filesDir, name), buf);
        db.addQuote({
          owner, purchaseId: row.id, file: name, url: q.url ?? null, capturedAt: null,
          digest: createHash('sha256').update(buf).digest('hex'), seenAt: null,
          data: { imported: true, manual: Boolean(q.manual), expect: q.expect ?? [], options: q.options ?? [], via: q.via ?? null },
        });
      });
    }

    if (entry.status === 'submitted') {
      if (existingFilings('purchase', entry.key).some((f) => f.docId === entry.docId)) { counts.filings.skippedExisting++; continue; }
      counts.filings.purchase++;
      write(() => db.addFiling({
        owner, kind: 'purchase', ref: entry.key, docId: entry.docId, title: entry.title ?? '', status: 'submitted',
        filedAt: skipDate(p.skip) ?? undefined, detail: { imported: true, ...(entry.refiledFrom ? { refiledFrom: entry.refiledFrom } : {}) },
      }));
    }
  }

  for (const t of legacy.TRIPS) {
    const status = tripStatus(t);
    counts.trips[status]++;
    const key = tripKey(t);
    const had = db.trip(owner, key);
    if (had && !replace) { counts.trips.skippedExisting++; continue; }
    counts.trips[had ? 'replaced' : 'inserted']++;
    // `skip` 은 원본 목록의 메모다. 상태로 옮겼으니 `legacySkip` 으로만 남긴다.
    const data = Object.fromEntries(Object.entries(t).filter(([k, v]) => !['docId', 'skip'].includes(k) && v !== undefined));
    if (t.skip) data.legacySkip = String(t.skip);
    write(() => db.upsertTrip({ owner, key, data: { ...data, imported: true }, docId: t.docId ? String(t.docId) : null, status }));
    if (status === 'submitted' && t.docId) {
      if (existingFilings('trip', key).some((f) => f.docId === String(t.docId))) { counts.filings.skippedExisting++; continue; }
      counts.filings.trip++;
      write(() => db.addFiling({ owner, kind: 'trip', ref: key, docId: String(t.docId), title: t.goal ?? '', status: 'submitted', filedAt: skipDate(t.skip) ?? undefined, detail: { imported: true } }));
    }
  }

  const current = db.overrides(owner);
  for (const o of mapOverrides(legacy, { warn })) {
    const c = (counts.overrides[o.kind] ??= { found: 0, inserted: 0, skippedExisting: 0 });
    c.found++;
    if (current.some((x) => sameOverride(x, o))) { c.skippedExisting++; continue; }
    c.inserted++;
    write(() => db.addOverride({ owner, ...o }));
  }

  for (const r of records) {
    if (existingFilings(r.kind, r.ref).length) { counts.filings.skippedExisting++; continue; }
    counts.filings.record++;
    write(() => db.addFiling({ owner, kind: r.kind, ref: r.ref, status: 'imported', detail: r.detail }));
  }
  return counts;
}

// ------------------------------------------------------------------ CLI

async function main(argv = process.argv.slice(2)) {
  const args = parseArgs(argv);
  const owner = ownerKey(args.owner);
  if (!owner) throw new Error(`아이디가 이상하다: ${args.owner}`);
  const repo = path.resolve(args.repo);
  const dataDir = path.resolve(args.dataDir ?? (args.db ? path.dirname(args.db) : (process.env.DAOUOFFICE_DATA_DIR || path.join(ROOT, 'data'))));
  const dbPath = path.resolve(args.db ?? path.join(dataDir, 'daouoffice.db'));
  const warnings = [];
  const warn = (m) => warnings.push(m);
  const config = args.config || process.env.DAOUOFFICE_CONFIG ? loadConfig(args.config ?? process.env.DAOUOFFICE_CONFIG).config : null;

  const legacy = await loadLegacy(repo, { warn });
  const records = loadRecords(repo, { warn });
  // 미리 보기만 할 때 DB 파일이 없으면 만들지 않는다.
  const db = openDb(args.dryRun && !fs.existsSync(dbPath) ? ':memory:' : dbPath);
  try {
    const safeOwner = owner.replace(/[^a-z0-9._@-]/gi, '_');
    const filesDir = path.join(dataDir, 'files', safeOwner, 'quotes');
    const counts = db.transaction(() => importLegacy({ legacy, records, db, owner, filesDir, repo, dryRun: args.dryRun, replace: args.replace, config, warn }));
    process.stdout.write(`${args.dryRun ? '[dry-run] 쓰지 않았다. ' : ''}DB ${dbPath}\n`);
    process.stdout.write(`${JSON.stringify(counts, null, 2)}\n`);
    for (const w of warnings) process.stderr.write(`주의: ${w}\n`);
  } finally {
    db.close();
  }
}

const isEntry = process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (isEntry) {
  main().catch((e) => {
    process.stderr.write(`중단: ${e?.message ?? e}\n`);
    process.exitCode = 1;
  });
}
