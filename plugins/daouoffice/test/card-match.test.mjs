import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  matchRows, matchCard, assignByAmount, classify, sameVendor, markCancelled, overrideMatches, isSubscription,
} from '../src/dao/card-match.mjs';

let seq = 0;
const row = (useDate, merchant, amount, extra = {}) => ({
  id: `row-${++seq}`, useDate: `${useDate} 12:00:00`, merchant, amount, status: '작성대기', docId: null, ...extra,
});
const receipt = (file, date, vendor, amount, currency, extra = {}) => ({
  file, date, vendor, amount, currency, item: file, category: '재료비', orderId: null, ...extra,
});
const purchase = (key, extra) => ({ key, title: `${key} 구매의 건`, amountKrw: null, usd: null, paidKrw: null, paidAt: null, ...extra });

function fixture() {
  const r = {
    krw: row('2026-03-02', '테스트상점', 45_000),
    usdSingle: row('2026-03-05', 'TESTSHOP*ONLINE', 14_000),
    aliCheckout: row('2026-03-06', 'ALIEXPRESS', 70_000),
    aliSeparate: row('2026-03-06', 'ALIEXPRESS', 21_000),
    sameAmountEarly: row('2026-03-10', '테스트스토어', 55_000),
    sameAmountLate: row('2026-03-20', '테스트스토어', 55_000),
    monthlyFirst: row('2026-03-01', 'MONTHLY', 33_000),
    monthlyLast: row('2026-03-31', 'MONTHLY', 33_000),
    refunded: row('2026-03-07', '테스트상점', 9_900),
    elsewhere: row('2026-03-15', 'OTHERSUB', 20_000),
    cancelPlus: row('2026-03-08', 'CANCELME', 12_345),
    cancelMinus: row('2026-03-09', 'CANCELME', -12_345),
    pinned: row('2026-03-25', 'PINSHOP', 30_000),
    asIs: row('2026-03-03', '애플스토어', 100_000),
    fuel: row('2026-03-04', '테스트주유소', 50_000),
  };
  const receipts = [
    receipt('krw-45000.pdf', '2026-03-03', '테스트상점', 45_000, '원'),
    receipt('usd-10.png', '2026-03-05', 'TESTSHOP', 10, 'USD'),
    receipt('ali-a.png', '2026-03-06', '알리익스프레스', 20, 'USD', { orderId: '1000000000000000' }),
    receipt('ali-b.png', '2026-03-06', '알리익스프레스', 30, 'USD', { orderId: '1000000000020000' }),
    receipt('ali-c.png', '2026-03-06', '알리익스프레스', 15, 'USD', { orderId: '1000000099990000' }),
    receipt('same-early.pdf', '2026-03-10', '테스트스토어', 55_000, 'KRW'),
    receipt('same-late.pdf', '2026-03-20', '테스트스토어', 55_000, 'KRW'),
    receipt('pin-a.png', '2026-03-24', '핀상점', 5, 'USD'),
    receipt('pin-b.png', '2026-03-24', '핀상점', 23_000, '원'),
  ];
  const purchases = [
    purchase('usd-single', { amountKrw: 14_000, paidAt: '2026-03-05' }),
    // Same amount, listed late-first: the original kept only the last one for both charges.
    purchase('same-late', { amountKrw: 55_000, paidAt: '2026-03-20' }),
    purchase('same-early', { amountKrw: 55_000, paidAt: '2026-03-10' }),
    purchase('monthly', { amountKrw: 33_000, paidAt: '2026-03-01' }),
    purchase('ali-a', { usd: 20, paidKrw: 28_000, amountKrw: 28_000, paidAt: '2026-03-06' }),
    purchase('ali-b', { usd: 30, paidKrw: 42_000, amountKrw: 42_000, paidAt: '2026-03-06' }),
  ];
  const overrides = [
    { kind: 'refunded', match: { merchant: '테스트상점', amount: 9_900 }, data: { why: '반품' } },
    { kind: 'elsewhere', match: { date: '2026-03-15', merchant: 'OTHERSUB', amount: 20_000 }, data: { why: '다른 사람이 처리' } },
    { kind: 'pinned', match: { date: '2026-03-25', merchant: 'PINSHOP', amount: 30_000 }, data: { receipts: ['pin-a', 'pin-b'] } },
    { kind: 'claimAsIs', match: { date: '2026-03-03', merchant: '애플스토어', amount: 100_000 }, data: { purposeName: '소모품비' } },
  ];
  return { r, rows: Object.values(r), receipts, purchases, overrides };
}

const byRow = (m) => new Map(m.audited.map((a) => [a.row.id, a]));

test('KRW receipts match by exact amount within a day', () => {
  const f = fixture();
  const m = matchRows(f);
  assert.equal(byRow(m).get(f.r.krw.id).receipt.file, 'krw-45000.pdf');
});

test('cancelled pairs, refunds and charges handled elsewhere are left out', () => {
  const f = fixture();
  const m = matchRows(f);
  const live = new Set(m.live.map((x) => x.id));
  for (const k of ['refunded', 'elsewhere', 'cancelPlus', 'cancelMinus']) assert.ok(!live.has(f.r[k].id), k);
  assert.deepEqual(m.refunded.map((x) => x.id), [f.r.refunded.id]);
  assert.ok(m.cancelled.has(f.r.cancelPlus.id) && m.cancelled.has(f.r.cancelMinus.id));
});

test('a foreign charge picks the receipt combination closest to the local rate, one checkout only', () => {
  const f = fixture();
  const a = byRow(matchRows(f));
  assert.equal(a.get(f.r.usdSingle.id).receipt.file, 'usd-10.png');
  assert.deepEqual(a.get(f.r.aliCheckout.id).receipts.map((y) => y.file).sort(), ['ali-a.png', 'ali-b.png']);
  assert.equal(a.get(f.r.aliSeparate.id).receipt.file, 'ali-c.png');
  assert.equal(a.get(f.r.aliSeparate.id).receipts, undefined);
});

test('same-amount requisitions go to the charge with the nearest payment date (byAmount collision)', () => {
  const f = fixture();
  const a = byRow(matchRows(f));
  assert.equal(a.get(f.r.sameAmountEarly.id).purchase.key, 'same-early');
  assert.equal(a.get(f.r.sameAmountLate.id).purchase.key, 'same-late');
});

test('one requisition covers one charge only', () => {
  const f = fixture();
  const a = byRow(matchRows(f));
  assert.equal(a.get(f.r.monthlyFirst.id).purchase.key, 'monthly');
  assert.equal(a.get(f.r.monthlyLast.id).purchase, undefined);
});

test('foreign requisitions are found by USD and their KRW amounts must add up', () => {
  const f = fixture();
  const a = byRow(matchRows(f));
  assert.deepEqual(a.get(f.r.aliCheckout.id).purchases.map((p) => p.key), ['ali-b', 'ali-a']);
  assert.equal(a.get(f.r.usdSingle.id).purchase.key, 'usd-single');

  // Change one paidKrw so the sum no longer matches: no requisition is claimed for the charge.
  f.purchases.find((p) => p.key === 'ali-a').paidKrw = 27_000;
  assert.deepEqual(byRow(matchRows(f)).get(f.r.aliCheckout.id).purchases, []);
});

test('pinned charges take the receipts a person named', () => {
  const f = fixture();
  const p = byRow(matchRows(f)).get(f.r.pinned.id);
  assert.deepEqual(p.receipts.map((y) => y.file), ['pin-a.png', 'pin-b.png']);
});

test('claim-as-is charges are flagged and kinds are classified', () => {
  const f = fixture();
  const a = byRow(matchRows(f));
  assert.equal(a.get(f.r.asIs.id).claimAsIs.data.purposeName, '소모품비');
  assert.equal(a.get(f.r.fuel.id).kind, '주유');
  assert.equal(a.get(f.r.krw.id).kind, '구매');
  assert.equal(a.get(f.r.krw.id).claimed, false);
});

test('without any local sample the configured rate is the reference; without a rate the line stays unmatched', () => {
  const rows = [row('2026-04-01', 'FARSHOP', 15_000)];
  const receipts = [receipt('far.png', '2026-04-01', 'FARSHOP', 10, 'USD')];
  // 15,000 / 10 = 1,500 per USD: a sample of its own, so it matches regardless.
  assert.equal(matchRows({ rows, receipts, purchases: [] }).audited[0].receipt.file, 'far.png');
  const two = [row('2026-04-02', 'FARSHOP', 28_000)];
  const pair = [receipt('far-1.png', '2026-04-02', 'FARSHOP', 10, 'USD'), receipt('far-2.png', '2026-04-02', 'FARSHOP', 10, 'USD')];
  assert.equal(matchRows({ rows: two, receipts: pair, purchases: [] }).audited[0].receipt, undefined);
  assert.equal(matchRows({ rows: two, receipts: pair, purchases: [], fxRate: 1400 }).audited[0].receipts.length, 2);
});

test('helpers', () => {
  assert.equal(classify('한국철도공사'), '철도');
  assert.equal(classify('카카오T 택시'), '택시');
  assert.equal(classify('테스트호텔'), '경비');
  assert.ok(sameVendor('알리익스프레스', 'ALIEXPRESS.COM'));
  assert.ok(sameVendor('Foo Labs', 'FOOLABS INC', {}));
  assert.ok(!sameVendor('ab', 'ABC'));
  assert.equal(markCancelled([row('2026-01-01', 'X', 100), row('2026-01-02', 'Y', -100)]).size, 0);
  assert.ok(overrideMatches({ merchant: 'shop', amount: 10 }, row('2026-01-01', 'TESTSHOP', 10)));
  assert.ok(!overrideMatches({ date: '2026-01-02', merchant: 'shop', amount: 10 }, row('2026-01-01', 'TESTSHOP', 10)));
  assert.ok(isSubscription({ row: row('2026-01-01', 'CLOUDCO', 1), receipt: null }, ['^CLOUD']));
  assert.ok(isSubscription({ row: row('2026-01-01', 'X', 1), receipt: { category: '클라우드구독료' } }));
  const pairs = assignByAmount([row('2026-01-05', 'A', 1000)], [purchase('nodate', { amountKrw: 1000 }), purchase('dated', { amountKrw: 1000, paidAt: '2026-01-06' })]);
  assert.equal([...pairs.values()][0].key, 'dated', 'a dated requisition wins over an undated one');
});

test('matchCard reads the ledger through listUsage and the rest from the DB', async () => {
  const f = fixture();
  const calls = [];
  const db = {
    receipts: (owner) => { calls.push(['receipts', owner]); return f.receipts; },
    purchases: (owner) => { calls.push(['purchases', owner]); return f.purchases; },
    overrides: (owner) => { calls.push(['overrides', owner]); return f.overrides; },
    latestFxRate: () => ({ rate: 1400 }),
  };
  const usage = async (api, q) => { calls.push(['usage', q]); return { rows: f.rows, totals: {}, truncated: [] }; };
  const m = await matchCard({ api: {}, db, owner: 'hong', card: { id: 'card-1' }, from: '2026-03-01', to: '2026-03-31', usage });
  assert.deepEqual(calls[0], ['usage', { from: '2026-03-01', to: '2026-03-31', cardId: 'card-1' }]);
  assert.ok(calls.some((c) => c[0] === 'receipts' && c[1] === 'hong'));
  assert.equal(m.audited.length, matchRows(f).audited.length);
});
