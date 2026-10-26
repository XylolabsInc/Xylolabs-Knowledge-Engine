import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { openDb } from '../src/db.mjs';

const fresh = () => openDb(':memory:');

test('creates every table, also on a file, and reopens it', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'dao-db-'));
  const file = path.join(dir, 'nested', 'daouoffice.db');
  const db = openDb(file);
  db.upsertAccount({ owner: 'hong', name: '홍길동' });
  db.close();
  const again = openDb(file);
  assert.equal(again.account('hong').name, '홍길동');
  again.close();
  fs.rmSync(dir, { recursive: true, force: true });
});

test('accounts: upsert keeps known fields when a later write lacks them', () => {
  const db = fresh();
  db.upsertAccount({ owner: 'hong', name: '홍길동', deptId: '100', deptCode: 'rnd', deptName: '연구개발팀', data: { a: 1 } });
  const a = db.upsertAccount({ owner: 'hong', data: { b: 2 } });
  assert.equal(a.name, '홍길동');
  assert.equal(a.deptCode, 'rnd');
  assert.deepEqual(a.data, { a: 1, b: 2 });
  assert.equal(db.account('nobody'), null);
});

test('purchases: unique per owner and key, JSON round trip, status filter', () => {
  const db = fresh();
  const p = db.upsertPurchase({ owner: 'hong', key: 'cam', title: '카메라 구매의 건', amountKrw: 120_000, paidAt: '2026-03-02', data: { item: '카메라 / 1개', note: ['a'] } });
  assert.equal(p.status, 'draft');
  assert.deepEqual(p.data, { item: '카메라 / 1개', note: ['a'] });
  const q = db.upsertPurchase({ owner: 'hong', key: 'cam', status: 'filed', title: '카메라 구매의 건', amountKrw: 120_000, docId: '1000000000000001', data: {} });
  assert.equal(q.id, p.id);
  assert.equal(q.docId, '1000000000000001');
  db.upsertPurchase({ owner: 'kim', key: 'cam', title: 'x', usd: 12.5, paidKrw: 17_000 });
  assert.equal(db.purchases('hong').length, 1);
  assert.equal(db.purchases('hong', { status: 'draft' }).length, 0);
  assert.equal(db.purchase('kim', 'cam').usd, 12.5);
  assert.equal(db.purchase('kim', 'cam').paidKrw, 17_000);
  assert.equal(db.purchaseById(p.id).key, 'cam');
});

test('quotes hang off a purchase and are removed with it', () => {
  const db = fresh();
  const p = db.upsertPurchase({ owner: 'hong', key: 'k', title: 't' });
  db.addQuote({ purchaseId: p.id, file: 'shot.png', url: 'https://example.com/p', digest: 'abc' });
  const q = db.addQuote({ purchaseId: p.id, file: 'shot.png', url: 'https://example.com/p', digest: 'def', seenAt: '2026-03-02' });
  assert.equal(db.quotes(p.id).length, 1);
  assert.equal(q.digest, 'def');
  assert.throws(() => db.addQuote({ purchaseId: 999, file: 'x.png' }), /FOREIGN KEY/);
});

test('receipts: date range, order id kept as text', () => {
  const db = fresh();
  db.addReceipt({ owner: 'hong', file: 'a.pdf', date: '2026-03-01', vendor: '테스트상점', amount: 1000, currency: '원', orderId: 1234567890123456n });
  db.addReceipt({ owner: 'hong', file: 'b.pdf', date: '2026-03-20', vendor: '테스트상점', amount: 2000, currency: '원' });
  db.addReceipt({ owner: 'hong', file: 'b.pdf', date: '2026-03-21', vendor: '테스트상점', amount: 2000, currency: '원' });
  assert.equal(db.receipts('hong').length, 2);
  assert.deepEqual(db.receipts('hong', { from: '2026-03-10' }).map((r) => r.file), ['b.pdf']);
  assert.equal(db.receipts('hong', { to: '2026-03-10' })[0].orderId, '1234567890123456');
  assert.equal(db.receipts('hong', { from: '2026-03-21', to: '2026-03-21' })[0].date, '2026-03-21');
});

test('photos: one row per image, alsoFor lookups, whitelisted updates', () => {
  const db = fresh();
  const a = db.addPhoto({ owner: 'hong', file: 'p1.jpg', sha256: 'aa', takenDate: '2026-03-03', purchaseKey: 'cam', alsoFor: ['lens'] });
  assert.equal(a.created, true);
  const dup = db.addPhoto({ owner: 'hong', file: 'p1-again.jpg', sha256: 'aa' });
  assert.equal(dup.created, false);
  assert.equal(dup.id, a.id);
  db.addPhoto({ owner: 'hong', file: 'p2.jpg', sha256: 'bb', purchaseKey: 'lens' });
  assert.deepEqual(db.photos('hong', { purchaseKey: 'lens' }).map((p) => p.file), ['p1.jpg', 'p2.jpg']);
  assert.deepEqual(db.photos('hong', { purchaseKey: 'cam' }).map((p) => p.file), ['p1.jpg']);
  const u = db.updatePhoto(a.id, { comments: [{ docId: '1', at: 'x' }], otherCard: '1234', bogus: 'ignored' });
  assert.deepEqual(u.comments, [{ docId: '1', at: 'x' }]);
  assert.equal(u.otherCard, '1234');
  assert.equal(db.photo(a.id).alsoFor[0], 'lens');
});

test('overrides, trips, filings and fx rates round trip', () => {
  const db = fresh();
  const o = db.addOverride({ owner: 'hong', kind: 'refunded', match: { merchant: '테스트상점', amount: 9900 }, data: { why: '반품' } });
  db.addOverride({ owner: 'hong', kind: 'elsewhere', match: { merchant: 'X' } });
  assert.deepEqual(db.overrides('hong', 'refunded')[0].match, { merchant: '테스트상점', amount: 9900 });
  assert.equal(db.overrides('hong').length, 2);
  assert.equal(db.removeOverride('kim', o.id), false, 'owner scoped');
  assert.equal(db.removeOverride('hong', o.id), true);

  db.upsertTrip({ owner: 'hong', key: '2026-03-02-busan', data: { place: '부산', purpose: '실증 협의' } });
  const t = db.upsertTrip({ owner: 'hong', key: '2026-03-02-busan', data: { place: '부산' }, docId: '1000000000000002', status: 'filed' });
  assert.equal(db.trips('hong').length, 1);
  assert.equal(t.status, 'filed');
  assert.equal(db.trip('hong', '2026-03-02-busan').docId, '1000000000000002');

  const f = db.addFiling({ owner: 'hong', kind: 'inspection', ref: '2026-03-02/45000', title: '검수조서', status: '진행중', detail: { photos: 2 } });
  db.addFiling({ owner: 'hong', kind: 'claim', ref: '2026-03' });
  assert.equal(db.filings('hong', { kind: 'inspection' }).length, 1);
  assert.equal(db.filings('hong', { ref: '2026-03' })[0].kind, 'claim');
  assert.equal(db.updateFiling(f.id, { status: '완료', docId: '1000000000000003' }).status, '완료');
  assert.ok(f.filedAt.endsWith('+09:00'), 'timestamps are KST');

  db.addFxRate({ day: '2026-03-01', rate: 1400.5, at: '2026-03-01 10:00', source: '테스트' });
  db.addFxRate({ day: '2026-03-02', rate: 1401, source: '테스트' });
  db.addFxRate({ day: '2026-03-02', rate: 1402, source: '테스트' });
  assert.equal(db.fxRate('2026-03-01').rate, 1400.5);
  assert.equal(db.latestFxRate().rate, 1402);
});

test('transactions roll back together', () => {
  const db = fresh();
  assert.throws(() => db.transaction(() => {
    db.upsertPurchase({ owner: 'hong', key: 'a', title: 'a' });
    db.transaction(() => db.upsertPurchase({ owner: 'hong', key: 'b', title: 'b' }));
    throw new Error('stop');
  }), /stop/);
  assert.equal(db.purchases('hong').length, 0);
  db.transaction(() => db.upsertPurchase({ owner: 'hong', key: 'c', title: 'c' }));
  assert.equal(db.purchases('hong').length, 1);
});
