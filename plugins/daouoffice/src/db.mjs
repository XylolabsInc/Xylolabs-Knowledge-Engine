import fs from 'node:fs';
import path from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { kstIso } from './dao/kst.mjs';

/**
 * Plugin-owned state (`$DAOUOFFICE_DATA_DIR/daouoffice.db`). It replaces the data that lived in
 * the private repo's source files and folders: `PURCHASES`, `TRIPS`, the override tables, the
 * receipt folder naming convention, `pictures/_matches.json`, `_taken.json`, filing logs, `FX`.
 *
 * Every row carries `owner` (the DaouOffice login id). JSON columns are parsed on read; rows come
 * back camelCased. All statements are prepared; nothing is interpolated into SQL.
 */

const SCHEMA_VERSION = 1;

const SCHEMA = `
CREATE TABLE IF NOT EXISTS accounts (
  owner      TEXT PRIMARY KEY,
  name       TEXT,
  user_id    TEXT,
  dept_id    TEXT,
  dept_code  TEXT,
  dept_name  TEXT,
  data       TEXT NOT NULL DEFAULT '{}',
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS purchases (
  id         INTEGER PRIMARY KEY,
  owner      TEXT NOT NULL,
  key        TEXT NOT NULL,
  status     TEXT NOT NULL DEFAULT 'draft',
  title      TEXT NOT NULL DEFAULT '',
  paid_at    TEXT,
  amount_krw INTEGER,
  usd        REAL,
  paid_krw   INTEGER,
  doc_id     TEXT,
  data       TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE (owner, key)
);
CREATE INDEX IF NOT EXISTS purchases_owner_doc ON purchases (owner, doc_id);

CREATE TABLE IF NOT EXISTS quotes (
  id          INTEGER PRIMARY KEY,
  purchase_id INTEGER NOT NULL REFERENCES purchases (id) ON DELETE CASCADE,
  file        TEXT NOT NULL,
  url         TEXT,
  captured_at TEXT,
  digest      TEXT,
  seen_at     TEXT,
  data        TEXT NOT NULL DEFAULT '{}',
  UNIQUE (purchase_id, file)
);

CREATE TABLE IF NOT EXISTS receipts (
  id         INTEGER PRIMARY KEY,
  owner      TEXT NOT NULL,
  file       TEXT NOT NULL,
  date       TEXT,
  category   TEXT,
  item       TEXT,
  vendor     TEXT,
  amount     REAL,
  currency   TEXT,
  kind       TEXT,
  order_id   TEXT,
  sha256     TEXT,
  data       TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL,
  UNIQUE (owner, file)
);
CREATE INDEX IF NOT EXISTS receipts_owner_date ON receipts (owner, date);

CREATE TABLE IF NOT EXISTS photos (
  id           INTEGER PRIMARY KEY,
  owner        TEXT NOT NULL,
  file         TEXT NOT NULL,
  sha256       TEXT NOT NULL,
  taken_date   TEXT,
  purchase_key TEXT,
  also_for     TEXT NOT NULL DEFAULT '[]',
  shows        TEXT,
  other_card   TEXT,
  comments     TEXT NOT NULL DEFAULT '[]',
  data         TEXT NOT NULL DEFAULT '{}',
  created_at   TEXT NOT NULL,
  UNIQUE (owner, sha256)
);
CREATE INDEX IF NOT EXISTS photos_owner_purchase ON photos (owner, purchase_key);

CREATE TABLE IF NOT EXISTS overrides (
  id         INTEGER PRIMARY KEY,
  owner      TEXT NOT NULL,
  kind       TEXT NOT NULL,
  match      TEXT NOT NULL DEFAULT '{}',
  data       TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS overrides_owner_kind ON overrides (owner, kind);

CREATE TABLE IF NOT EXISTS trips (
  id         INTEGER PRIMARY KEY,
  owner      TEXT NOT NULL,
  key        TEXT NOT NULL,
  data       TEXT NOT NULL DEFAULT '{}',
  doc_id     TEXT,
  status     TEXT NOT NULL DEFAULT 'draft',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE (owner, key)
);

CREATE TABLE IF NOT EXISTS filings (
  id       INTEGER PRIMARY KEY,
  owner    TEXT NOT NULL,
  kind     TEXT NOT NULL,
  ref      TEXT,
  doc_id   TEXT,
  title    TEXT,
  status   TEXT,
  filed_at TEXT NOT NULL,
  detail   TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS filings_owner_kind ON filings (owner, kind, ref);

CREATE TABLE IF NOT EXISTS fx_rates (
  day    TEXT PRIMARY KEY,
  rate   REAL NOT NULL,
  at     TEXT,
  source TEXT
);
`;

const json = (v, fallback) => JSON.stringify(v ?? fallback);
const parse = (text, fallback) => {
  if (text === null || text === undefined || text === '') return fallback;
  try { return JSON.parse(text); } catch { return fallback; }
};
const n = (v) => (v === undefined ? null : v);
const num = (v) => (v === undefined || v === null || v === '' ? null : Number(v));
const plain = (row) => (row ? { ...row } : null);

const toAccount = (r) => (r ? {
  owner: r.owner, name: r.name, userId: r.user_id, deptId: r.dept_id, deptCode: r.dept_code,
  deptName: r.dept_name, data: parse(r.data, {}), updatedAt: r.updated_at,
} : null);
const toPurchase = (r) => (r ? {
  id: r.id, owner: r.owner, key: r.key, status: r.status, title: r.title, paidAt: r.paid_at,
  amountKrw: r.amount_krw, usd: r.usd, paidKrw: r.paid_krw, docId: r.doc_id, data: parse(r.data, {}),
  createdAt: r.created_at, updatedAt: r.updated_at,
} : null);
const toQuote = (r) => (r ? {
  id: r.id, purchaseId: r.purchase_id, file: r.file, url: r.url, capturedAt: r.captured_at,
  digest: r.digest, seenAt: r.seen_at, data: parse(r.data, {}),
} : null);
const toReceipt = (r) => (r ? {
  id: r.id, owner: r.owner, file: r.file, date: r.date, category: r.category, item: r.item,
  vendor: r.vendor, amount: r.amount, currency: r.currency, kind: r.kind, orderId: r.order_id,
  sha256: r.sha256, data: parse(r.data, {}), createdAt: r.created_at,
} : null);
const toPhoto = (r) => (r ? {
  id: r.id, owner: r.owner, file: r.file, sha256: r.sha256, takenDate: r.taken_date,
  purchaseKey: r.purchase_key, alsoFor: parse(r.also_for, []), shows: r.shows, otherCard: r.other_card,
  comments: parse(r.comments, []), data: parse(r.data, {}), createdAt: r.created_at,
} : null);
const toOverride = (r) => (r ? {
  id: r.id, owner: r.owner, kind: r.kind, match: parse(r.match, {}), data: parse(r.data, {}), createdAt: r.created_at,
} : null);
const toTrip = (r) => (r ? {
  id: r.id, owner: r.owner, key: r.key, data: parse(r.data, {}), docId: r.doc_id, status: r.status,
  createdAt: r.created_at, updatedAt: r.updated_at,
} : null);
const toFiling = (r) => (r ? {
  id: r.id, owner: r.owner, kind: r.kind, ref: r.ref, docId: r.doc_id, title: r.title, status: r.status,
  filedAt: r.filed_at, detail: parse(r.detail, {}),
} : null);
const toFx = (r) => (r ? { day: r.day, rate: r.rate, at: r.at, source: r.source } : null);

/** Fields `updatePhoto` / `updateFiling` may change, with their column and encoder. */
const PHOTO_FIELDS = {
  file: ['file', n], takenDate: ['taken_date', n], purchaseKey: ['purchase_key', n],
  alsoFor: ['also_for', (v) => json(v, [])], shows: ['shows', n], otherCard: ['other_card', n],
  comments: ['comments', (v) => json(v, [])], data: ['data', (v) => json(v, {})],
};
const FILING_FIELDS = {
  ref: ['ref', n], docId: ['doc_id', n], title: ['title', n], status: ['status', n],
  filedAt: ['filed_at', n], detail: ['detail', (v) => json(v, {})],
};

export class Db {
  #db;
  #stmts = new Map();

  constructor(file) {
    if (file !== ':memory:') fs.mkdirSync(path.dirname(file), { recursive: true });
    this.#db = new DatabaseSync(file);
    this.#db.exec('PRAGMA journal_mode = WAL; PRAGMA foreign_keys = ON; PRAGMA busy_timeout = 5000;');
    this.#db.exec(SCHEMA);
    this.#db.exec(`PRAGMA user_version = ${SCHEMA_VERSION}`);
  }

  #q(sql) {
    let s = this.#stmts.get(sql);
    if (!s) { s = this.#db.prepare(sql); this.#stmts.set(sql, s); }
    return s;
  }

  close() { this.#db.close(); }

  /** Runs `fn` in a transaction. Nested calls join the outer one. */
  transaction(fn) {
    if (this.#db.isTransaction) return fn();
    this.#db.exec('BEGIN IMMEDIATE');
    try {
      const out = fn();
      this.#db.exec('COMMIT');
      return out;
    } catch (e) {
      this.#db.exec('ROLLBACK');
      throw e;
    }
  }

  // ------------------------------------------------------------------ accounts

  upsertAccount({ owner, name, userId, deptId, deptCode, deptName, data }) {
    return toAccount(this.#q(`
      INSERT INTO accounts (owner, name, user_id, dept_id, dept_code, dept_name, data, updated_at)
      VALUES (:owner, :name, :userId, :deptId, :deptCode, :deptName, :data, :at)
      ON CONFLICT (owner) DO UPDATE SET
        name = COALESCE(excluded.name, accounts.name),
        user_id = COALESCE(excluded.user_id, accounts.user_id),
        dept_id = COALESCE(excluded.dept_id, accounts.dept_id),
        dept_code = COALESCE(excluded.dept_code, accounts.dept_code),
        dept_name = COALESCE(excluded.dept_name, accounts.dept_name),
        data = json_patch(accounts.data, excluded.data),
        updated_at = excluded.updated_at
      RETURNING *`).get({
      owner, name: n(name), userId: n(userId), deptId: n(deptId), deptCode: n(deptCode),
      deptName: n(deptName), data: json(data, {}), at: kstIso(),
    }));
  }

  account(owner) {
    return toAccount(this.#q('SELECT * FROM accounts WHERE owner = ?').get(owner));
  }

  // ------------------------------------------------------------------ purchases

  purchases(owner, { status } = {}) {
    const rows = status
      ? this.#q('SELECT * FROM purchases WHERE owner = ? AND status = ? ORDER BY paid_at, id').all(owner, status)
      : this.#q('SELECT * FROM purchases WHERE owner = ? ORDER BY paid_at, id').all(owner);
    return rows.map(toPurchase);
  }

  purchase(owner, key) {
    return toPurchase(this.#q('SELECT * FROM purchases WHERE owner = ? AND key = ?').get(owner, key));
  }

  purchaseById(id) {
    return toPurchase(this.#q('SELECT * FROM purchases WHERE id = ?').get(id));
  }

  /** Inserts or replaces the purchase `(owner, key)`. Every column is written as given. */
  upsertPurchase({ owner, key, status = 'draft', title = '', paidAt, amountKrw, usd, paidKrw, docId, data }) {
    const at = kstIso();
    return toPurchase(this.#q(`
      INSERT INTO purchases (owner, key, status, title, paid_at, amount_krw, usd, paid_krw, doc_id, data, created_at, updated_at)
      VALUES (:owner, :key, :status, :title, :paidAt, :amountKrw, :usd, :paidKrw, :docId, :data, :at, :at)
      ON CONFLICT (owner, key) DO UPDATE SET
        status = excluded.status, title = excluded.title, paid_at = excluded.paid_at,
        amount_krw = excluded.amount_krw, usd = excluded.usd, paid_krw = excluded.paid_krw,
        doc_id = excluded.doc_id, data = excluded.data, updated_at = excluded.updated_at
      RETURNING *`).get({
      owner, key, status, title, paidAt: n(paidAt), amountKrw: num(amountKrw), usd: num(usd),
      paidKrw: num(paidKrw), docId: n(docId), data: json(data, {}), at,
    }));
  }

  /**
   * Merges `patch` into the purchase's `data` (JSON merge patch: keys set to null are removed) and
   * leaves every column alone. For additions made outside the requisition flow, such as serial
   * numbers read off inspection photos. Returns the row, or null when the purchase does not exist.
   */
  patchPurchaseData(owner, key, patch) {
    return toPurchase(this.#q(`
      UPDATE purchases SET data = json_patch(data, :patch), updated_at = :at
      WHERE owner = :owner AND key = :key RETURNING *`).get({ owner, key, patch: json(patch, {}), at: kstIso() }));
  }

  // ------------------------------------------------------------------ quotes

  quotes(purchaseId) {
    return this.#q('SELECT * FROM quotes WHERE purchase_id = ? ORDER BY id').all(purchaseId).map(toQuote);
  }

  addQuote({ purchaseId, file, url, capturedAt, digest, seenAt, data }) {
    return toQuote(this.#q(`
      INSERT INTO quotes (purchase_id, file, url, captured_at, digest, seen_at, data)
      VALUES (:purchaseId, :file, :url, :capturedAt, :digest, :seenAt, :data)
      ON CONFLICT (purchase_id, file) DO UPDATE SET
        url = excluded.url, captured_at = excluded.captured_at, digest = excluded.digest,
        seen_at = excluded.seen_at, data = excluded.data
      RETURNING *`).get({
      purchaseId, file, url: n(url), capturedAt: n(capturedAt), digest: n(digest), seenAt: n(seenAt), data: json(data, {}),
    }));
  }

  // ------------------------------------------------------------------ receipts

  receipts(owner, { from, to } = {}) {
    return this.#q(`
      SELECT * FROM receipts WHERE owner = :owner
        AND (:from IS NULL OR date >= :from) AND (:to IS NULL OR date <= :to)
      ORDER BY date, id`).all({ owner, from: n(from), to: n(to) }).map(toReceipt);
  }

  /** Inserts or updates the receipt `(owner, file)`. */
  addReceipt({ owner, file, date, category, item, vendor, amount, currency, kind, orderId, sha256, data }) {
    return toReceipt(this.#q(`
      INSERT INTO receipts (owner, file, date, category, item, vendor, amount, currency, kind, order_id, sha256, data, created_at)
      VALUES (:owner, :file, :date, :category, :item, :vendor, :amount, :currency, :kind, :orderId, :sha256, :data, :at)
      ON CONFLICT (owner, file) DO UPDATE SET
        date = excluded.date, category = excluded.category, item = excluded.item, vendor = excluded.vendor,
        amount = excluded.amount, currency = excluded.currency, kind = excluded.kind,
        order_id = excluded.order_id, sha256 = excluded.sha256, data = excluded.data
      RETURNING *`).get({
      owner, file, date: n(date), category: n(category), item: n(item), vendor: n(vendor),
      amount: num(amount), currency: n(currency), kind: n(kind), orderId: orderId == null ? null : String(orderId),
      sha256: n(sha256), data: json(data, {}), at: kstIso(),
    }));
  }

  // ------------------------------------------------------------------ photos

  /** All photos of `owner`, or those linked to `purchaseKey` (directly or through `alsoFor`). */
  photos(owner, { purchaseKey } = {}) {
    const rows = purchaseKey
      ? this.#q(`
          SELECT * FROM photos WHERE owner = :owner AND (purchase_key = :key
            OR EXISTS (SELECT 1 FROM json_each(photos.also_for) WHERE value = :key))
          ORDER BY id`).all({ owner, key: purchaseKey })
      : this.#q('SELECT * FROM photos WHERE owner = ? ORDER BY id').all(owner);
    return rows.map(toPhoto);
  }

  photo(id) {
    return toPhoto(this.#q('SELECT * FROM photos WHERE id = ?').get(id));
  }

  /**
   * Adds a photo. The same image (same sha256) is stored once per owner: the existing row comes
   * back with `created: false`.
   */
  addPhoto({ owner, file, sha256, takenDate, purchaseKey, alsoFor, shows, otherCard, comments, data }) {
    const r = this.#q(`
      INSERT INTO photos (owner, file, sha256, taken_date, purchase_key, also_for, shows, other_card, comments, data, created_at)
      VALUES (:owner, :file, :sha256, :takenDate, :purchaseKey, :alsoFor, :shows, :otherCard, :comments, :data, :at)
      ON CONFLICT (owner, sha256) DO NOTHING`).run({
      owner, file, sha256, takenDate: n(takenDate), purchaseKey: n(purchaseKey), alsoFor: json(alsoFor, []),
      shows: n(shows), otherCard: otherCard == null ? null : String(otherCard), comments: json(comments, []),
      data: json(data, {}), at: kstIso(),
    });
    const row = toPhoto(this.#q('SELECT * FROM photos WHERE owner = ? AND sha256 = ?').get(owner, sha256));
    return { ...row, created: r.changes > 0 };
  }

  updatePhoto(id, patch) {
    return toPhoto(this.#update('photos', PHOTO_FIELDS, id, patch));
  }

  // ------------------------------------------------------------------ overrides

  overrides(owner, kind) {
    const rows = kind
      ? this.#q('SELECT * FROM overrides WHERE owner = ? AND kind = ? ORDER BY id').all(owner, kind)
      : this.#q('SELECT * FROM overrides WHERE owner = ? ORDER BY id').all(owner);
    return rows.map(toOverride);
  }

  addOverride({ owner, kind, match, data }) {
    return toOverride(this.#q(`
      INSERT INTO overrides (owner, kind, match, data, created_at) VALUES (:owner, :kind, :match, :data, :at)
      RETURNING *`).get({ owner, kind, match: json(match, {}), data: json(data, {}), at: kstIso() }));
  }

  removeOverride(owner, id) {
    return this.#q('DELETE FROM overrides WHERE owner = ? AND id = ?').run(owner, id).changes > 0;
  }

  // ------------------------------------------------------------------ trips

  trips(owner, { status } = {}) {
    const rows = status
      ? this.#q('SELECT * FROM trips WHERE owner = ? AND status = ? ORDER BY id').all(owner, status)
      : this.#q('SELECT * FROM trips WHERE owner = ? ORDER BY id').all(owner);
    return rows.map(toTrip);
  }

  trip(owner, key) {
    return toTrip(this.#q('SELECT * FROM trips WHERE owner = ? AND key = ?').get(owner, key));
  }

  upsertTrip({ owner, key, data, docId, status = 'draft' }) {
    const at = kstIso();
    return toTrip(this.#q(`
      INSERT INTO trips (owner, key, data, doc_id, status, created_at, updated_at)
      VALUES (:owner, :key, :data, :docId, :status, :at, :at)
      ON CONFLICT (owner, key) DO UPDATE SET
        data = excluded.data, doc_id = excluded.doc_id, status = excluded.status, updated_at = excluded.updated_at
      RETURNING *`).get({ owner, key, data: json(data, {}), docId: n(docId), status, at }));
  }

  // ------------------------------------------------------------------ filings

  filings(owner, { kind, ref } = {}) {
    return this.#q(`
      SELECT * FROM filings WHERE owner = :owner
        AND (:kind IS NULL OR kind = :kind) AND (:ref IS NULL OR ref = :ref)
      ORDER BY filed_at, id`).all({ owner, kind: n(kind), ref: ref == null ? null : String(ref) }).map(toFiling);
  }

  addFiling({ owner, kind, ref, docId, title, status, filedAt, detail }) {
    return toFiling(this.#q(`
      INSERT INTO filings (owner, kind, ref, doc_id, title, status, filed_at, detail)
      VALUES (:owner, :kind, :ref, :docId, :title, :status, :filedAt, :detail)
      RETURNING *`).get({
      owner, kind, ref: ref == null ? null : String(ref), docId: n(docId), title: n(title), status: n(status),
      filedAt: filedAt ?? kstIso(), detail: json(detail, {}),
    }));
  }

  updateFiling(id, patch) {
    return toFiling(this.#update('filings', FILING_FIELDS, id, patch));
  }

  // ------------------------------------------------------------------ fx

  fxRate(day) {
    return toFx(this.#q('SELECT * FROM fx_rates WHERE day = ?').get(day));
  }

  latestFxRate() {
    return toFx(this.#q('SELECT * FROM fx_rates ORDER BY day DESC LIMIT 1').get());
  }

  addFxRate({ day, rate, at, source }) {
    return toFx(this.#q(`
      INSERT INTO fx_rates (day, rate, at, source) VALUES (:day, :rate, :at, :source)
      ON CONFLICT (day) DO UPDATE SET rate = excluded.rate, at = excluded.at, source = excluded.source
      RETURNING *`).get({ day, rate: Number(rate), at: n(at), source: n(source) }));
  }

  // ------------------------------------------------------------------ helpers

  /** Updates whitelisted fields only; the column names never come from the caller. */
  #update(table, fields, id, patch = {}) {
    const keys = Object.keys(patch).filter((k) => fields[k]);
    if (!keys.length) return this.#q(`SELECT * FROM ${table} WHERE id = ?`).get(id);
    const sets = keys.map((k) => `${fields[k][0]} = :${k}`).join(', ');
    const params = Object.fromEntries(keys.map((k) => [k, fields[k][1](patch[k])]));
    return plain(this.#q(`UPDATE ${table} SET ${sets} WHERE id = :id RETURNING *`).get({ ...params, id }));
  }
}

/** Opens (and creates) the plugin database. */
export function openDb(file) {
  return new Db(file);
}
