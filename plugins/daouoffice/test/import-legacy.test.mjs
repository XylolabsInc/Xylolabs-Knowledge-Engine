import { describe, test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { openDb } from '../src/db.mjs';
import { formValues } from '../src/dao/purchases.mjs';
import { parseConfig } from '../src/config.mjs';
import { tripKey } from '../src/dao/trips.mjs';
import {
  parseArgs, purchaseStatus, mapPurchase, tripStatus, mapOverrides, loadLegacy, loadRecords, importLegacy, regexToSubstrings,
} from '../scripts/import-legacy.mjs';

/**
 * A made-up private repo: the same module names and export shapes as the real one, placeholder data.
 * `fx.mjs` imports `playwright`, which does not resolve from the temp dir, and `inspection.mjs`
 * reads a cwd-relative file at load time, like the real modules do.
 */
function fakeRepo() {
  const repo = fs.mkdtempSync(path.join(os.tmpdir(), 'legacy-'));
  const w = (rel, text) => {
    fs.mkdirSync(path.dirname(path.join(repo, rel)), { recursive: true });
    fs.writeFileSync(path.join(repo, rel), text);
  };
  w('src/fx.mjs', "import { chromium } from 'playwright';\nexport const toKrw = (usd, rate) => Math.round(usd * rate);\n");
  w('src/purchases.mjs', `
import fs from 'node:fs';
import { toKrw } from './fx.mjs';
export const FX = { rate: 1400, at: '2026-01-10 10:00', source: '하나은행 매매기준율 (고시 1회차)' };
const CARD = { budget: '프로젝트 사업비', project: '스마트농장', payWhen: '당월', payMethod: '법인카드', cardIssuer: '신한', cardLast4: 1234 };
export const PURCHASES = [
  { ...CARD, key: 'camera', paidAt: '2026-01-05', subProject: '온실', docId: '1000000000000001', skip: '2026-01-06 상신 (1000000000000001)',
    title: '테스트 카메라 구매의 건', item: '테스트 카메라 / 1개', vendor: '테스트상점', deliveryDate: '2026-01-07', purpose: '온실 작물 생육 원격 관찰',
    amount: 132000, note: ['https://shop.example.com/camera'], serials: { '테스트 카메라': ['SN-1'] },
    quotes: [{ url: 'https://shop.example.com/camera', file: 'shop_camera.png', expect: ['테스트 카메라'] }] },
  { ...CARD, key: 'cable', paidAt: '2026-01-08', subProject: '드론', skip: '2026-01-09 재상신 (1000000000000003)',
    title: '테스트 케이블 구매의 건', item: '테스트 케이블 / 2개', vendor: '테스트상점 (테스트몰)', deliveryDate: '해당없음', purpose: '드론 기체 배선 연결',
    usd: 20, paidKrw: 27500, note: ['https://shop.example.com/cable'], quotes: [{ url: 'https://shop.example.com/cable', file: 'cable.png', expect: ['케이블'] }] },
  { ...CARD, key: 'waiting', subProject: '온실', skip: '카드 승인 대기. 결제가 대장에 올라오면 paidKrw 를 넣는다',
    title: '테스트 센서 구매의 건', item: '테스트 센서 / 3개', vendor: '테스트상점', deliveryDate: '2026-01-20', purpose: '온실 온습도 측정',
    usd: 50, paidAt: '2026-01-18', note: ['https://shop.example.com/sensor'], quotes: [{ url: 'https://shop.example.com/sensor', file: 'sensor.png', expect: ['센서'] }, { file: '영수증.png', manual: true }] },
];
export const REFILED = (() => { try { return JSON.parse(fs.readFileSync(new URL('./refiled.json', import.meta.url), 'utf8')); } catch { return {}; } })();
for (const p of PURCHASES) {
  const r = REFILED[p.key];
  if (!r) continue;
  p.refiledFrom = r.from;
  p.docId = r.to;
  p.skip = \`\${r.at} 재상신 (\${r.to}), 옛 문서 \${r.from}\`;
}
export const unused = toKrw(1, 1);
`);
  w('src/refiled.json', JSON.stringify({ camera: { from: '1000000000000001', to: '1000000000000002', at: '2026-01-21' } }));
  w('src/trips.mjs', `
export const TRIPS = [
  { start: '2026-01-05', end: '2026-01-05', place: '테스트시청', transport: '기타', goal: '실증 부지 인허가 협의', skip: '2026-01-04 상신됨', docId: '1000000000000010' },
  { start: '2026-01-12', end: '2026-01-13', place: '테스트항만', transport: '고속철도(KTX/SRT)', goal: '현장 시험 입회', skip: '다른 사람이 이미 상신함' },
  { start: '2026-01-20', end: '2026-01-20', place: '테스트공단', transport: '기타', goal: '장비 설치 협의' },
  { start: '2026-01-25', end: '2026-01-25', place: '테스트연구소', transport: '기타', goal: '시험 결과 협의', skip: '출장 취소' },
];
`);
  // CONFIRMED is not exported in the real module either.
  w('src/trip-expenses.mjs', `
import { TRIPS } from './trips.mjs';
const CONFIRMED = [
  { date: '2026-01-02', merchant: /TESTHOTEL|테스트호텔/i, kind: '숙박', trip: '2026-01-05' },
  { date: '2026-01-03', merchant: /테스트\\s*주차장/, kind: '주차', detail: '고객 미팅 주차' },
  { date: '2026-01-04', merchant: /T.*X/, kind: '철도', trip: '2026-01-12' },
];
export const tripCount = TRIPS.length + CONFIRMED.length * 0;
`);
  w('src/card-match.mjs', `
export const REFUNDED = [{ merchant: '테스트몰', amount: 5000, why: '반품 (사람 확인)' }];
export const HANDLED_ELSEWHERE = [{ date: '2026-01-03', merchant: 'TESTCLOUD', amount: 10000, why: '다른 분이 처리' }];
export const PINNED = [{ date: '2026-01-04', merchant: 'TESTSHOP', amount: 30000, receipts: ['테스트-주문'], why: '한 번에 결제' }];
export const CLAIM_AS_IS = [{ date: '2026-01-05', merchant: '테스트스토어', amount: 1000, detail: '결제 취소 및 환불', purposeName: '소모품비', why: '그대로 청구' }];
`);
  w('src/inspection.mjs', `
import fs from 'node:fs';
export const MATCHES = (() => { try { return JSON.parse(fs.readFileSync('pictures/_matches.json', 'utf8')); } catch { return {}; } })();
export const INSPECTION_DONE = { camera: '다른 분이 처리' };
export const INSPECTION_INCOMPLETE = { cable: '케이블 사진 없음' };
export const ITEM_OVERRIDES = { cable: [{ name: '테스트 케이블', qty: 2, unitWord: '개', amount: 27500 }] };
export const matchesSeen = Object.keys(MATCHES).length;
`);
  w('pictures/_matches.json', JSON.stringify({ 'a.jpg': { key: 'camera' } }));
  w('records/cards/1234.json', JSON.stringify({ card: '1234', rows: [] }));
  w('records/ali-orders/2026-01-10.json', JSON.stringify({ date: '2026-01-10', orders: [] }));
  w('records/cards/broken.json', '{');
  w('output/quotes/shop_camera.png', 'png-bytes');
  return repo;
}

describe('legacy import', () => {
  let repo;
  let legacy;
  const warnings = [];
  const cfg = parseConfig({ projects: { 스마트농장: { reviewer: '최담당', subProjects: ['온실', '드론'] } } });

  before(async () => {
    repo = fakeRepo();
    legacy = await loadLegacy(repo, { warn: (m) => warnings.push(m) });
  });
  after(() => fs.rmSync(repo, { recursive: true, force: true }));

  test('arguments', () => {
    assert.deepEqual(parseArgs(['--repo', '/r', '--owner', 'Hong', '--dry-run']), { repo: '/r', owner: 'Hong', dryRun: true, replace: false });
    assert.throws(() => parseArgs(['--repo', '/r']), /--owner/);
    assert.throws(() => parseArgs(['--repo', '--owner', 'x']), /값이 없다/);
    assert.throws(() => parseArgs(['--repo', '/r', '--owner', 'x', '--what']), /모르는 인자/);
  });

  test('modules load from the repo cwd, with playwright stubbed and refiled.json merged', () => {
    assert.deepEqual(warnings, []);
    assert.equal(legacy.PURCHASES.length, 3);
    assert.equal(legacy.FX.rate, 1400);
    const camera = legacy.PURCHASES.find((p) => p.key === 'camera');
    assert.equal(camera.docId, '1000000000000002');
    assert.equal(camera.refiledFrom, '1000000000000001');
    assert.equal(legacy.TRIPS.length, 4);
    assert.equal(legacy.TRIP_CONFIRMED.length, 3);
    assert.equal(legacy.REFUNDED.length, 1);
    assert.equal(Object.keys(legacy.ITEM_OVERRIDES).length, 1);
    assert.equal(process.cwd() === repo, false, 'cwd is restored');
  });

  test('purchase status: filed when a document id is known, draft with the hold reason otherwise', () => {
    assert.deepEqual(purchaseStatus({ docId: '1000000000000001' }), { status: 'submitted', docId: '1000000000000001', hold: null });
    assert.deepEqual(purchaseStatus({ skip: '2026-01-09 재상신 (1000000000000003)' }), { status: 'submitted', docId: '1000000000000003', hold: null });
    assert.deepEqual(purchaseStatus({ skip: '카드 승인 대기' }), { status: 'draft', docId: null, hold: '카드 승인 대기' });
    assert.deepEqual(purchaseStatus({}), { status: 'draft', docId: null, hold: null });
  });

  test('mapped purchases keep the amounts the source filed', () => {
    for (const p of legacy.PURCHASES) {
      const e = mapPurchase(p, { fx: legacy.FX });
      assert.equal(e.cardLast4, '1234');
      const want = p.paidKrw ?? (p.usd != null ? Math.round(p.usd * 1400) : p.amount);
      assert.equal(formValues(e, { config: cfg }).amountNumber, want, p.key);
    }
    const waiting = mapPurchase(legacy.PURCHASES[2], { fx: legacy.FX });
    assert.equal(waiting.fx.rate, 1400);
    assert.match(waiting.hold, /카드 승인 대기/);
    assert.equal(mapPurchase(legacy.PURCHASES[1], { fx: legacy.FX }).fx, undefined);
  });

  test('trips and override tables map to rows', () => {
    assert.deepEqual(legacy.TRIPS.map(tripStatus), ['submitted', 'elsewhere', 'draft', 'cancelled']);
    assert.equal(tripStatus({ skip: '2026-01-04 정리함' }), 'submitted');
    assert.equal(tripKey(legacy.TRIPS[0]), '2026-01-05/2026-01-05/테스트시청');
    const warns = [];
    const rows = mapOverrides(legacy, { warn: (m) => warns.push(m) });
    assert.deepEqual(rows.map((r) => r.kind), ['refunded', 'elsewhere', 'pinned', 'claimAsIs', 'inspectionDone', 'inspectionIncomplete', 'itemOverride', 'tripExpense', 'tripExpense', 'tripExpense']);
    assert.deepEqual(rows[0], { kind: 'refunded', match: { merchant: '테스트몰', amount: 5000 }, data: { why: '반품 (사람 확인)' } });
    assert.deepEqual(rows[2].data, { receipts: ['테스트-주문'], why: '한 번에 결제' });
    assert.deepEqual(rows[4], { kind: 'inspectionDone', match: { key: 'camera' }, data: { why: '다른 분이 처리' } });
    assert.deepEqual(rows[5].data, { missing: '케이블 사진 없음' });
    assert.equal(rows[6].data.rows[0].amount, 27_500);
    assert.deepEqual(rows.slice(7).map((r) => [r.match, r.data]), [
      [{ date: '2026-01-02', merchant: 'TESTHOTEL' }, { kind: '숙박', trip: '2026-01-05' }],
      [{ date: '2026-01-02', merchant: '테스트호텔' }, { kind: '숙박', trip: '2026-01-05' }],
      [{ date: '2026-01-03', merchant: '테스트 주차장' }, { kind: '주차', detail: '고객 미팅 주차' }],
    ]);
    assert.equal(warns.length, 1, 'the T.*X pattern cannot be a substring');
  });

  test('regexes become plain substrings only when they are plain', () => {
    assert.deepEqual(regexToSubstrings(/TEST\.AI/i), ['TEST.AI']);
    assert.deepEqual(regexToSubstrings(/^테스트$/), ['테스트']);
    assert.equal(regexToSubstrings(/(a)|b/), null);
  });

  test('records: readable JSON only', () => {
    const warns = [];
    const recs = loadRecords(repo, { warn: (m) => warns.push(m) });
    assert.deepEqual(recs.map((r) => `${r.kind}:${r.ref}`), ['record:cards:1234', 'record:ali-orders:2026-01-10']);
    assert.equal(warns.length, 1);
  });

  test('dry run counts and writes nothing', () => {
    const db = openDb(':memory:');
    const filesDir = fs.mkdtempSync(path.join(os.tmpdir(), 'legacy-files-'));
    const counts = importLegacy({ legacy, records: loadRecords(repo), db, owner: 'hong', filesDir, repo, dryRun: true, config: cfg });
    assert.deepEqual(counts.purchases, { found: 3, submitted: 2, draft: 1, inserted: 3, replaced: 0, skippedExisting: 0, refiled: 1, withFx: 1 });
    assert.deepEqual(counts.quotes, { found: 4, copied: 1, missingFile: 2, receiptLike: 1 });
    assert.equal(db.purchases('hong').length, 0);
    assert.equal(db.overrides('hong').length, 0);
    assert.deepEqual(fs.readdirSync(filesDir), []);
    db.close();
  });

  test('import writes rows, copies quotes and is idempotent', () => {
    const db = openDb(':memory:');
    const filesDir = fs.mkdtempSync(path.join(os.tmpdir(), 'legacy-files-'));
    const run = () => importLegacy({ legacy, records: loadRecords(repo), db, owner: 'hong', filesDir, repo });
    const first = run();
    assert.equal(first.purchases.inserted, 3);
    assert.deepEqual(first.trips, { found: 4, submitted: 1, elsewhere: 1, cancelled: 1, draft: 1, inserted: 4, replaced: 0, skippedExisting: 0 });
    assert.deepEqual(first.filings, { purchase: 2, trip: 1, record: 2, skippedExisting: 0 });

    const camera = db.purchase('hong', 'camera');
    assert.equal(camera.status, 'submitted');
    assert.equal(camera.docId, '1000000000000002');
    assert.equal(camera.amountKrw, 132_000);
    assert.equal(camera.data.refiledFrom, '1000000000000001');
    assert.deepEqual(camera.data.serials, { '테스트 카메라': ['SN-1'] });
    const waiting = db.purchase('hong', 'waiting');
    assert.equal(waiting.status, 'draft');
    assert.equal(waiting.amountKrw, 70_000);
    const [q] = db.quotes('hong', camera.id);
    assert.equal(q.file, 'shop_camera.png');
    assert.equal(fs.readFileSync(path.join(filesDir, q.file), 'utf8'), 'png-bytes');
    const other = db.trip('hong', tripKey(legacy.TRIPS[1]));
    assert.equal(other.status, 'elsewhere');
    assert.equal(other.data.skip, undefined);
    assert.equal(other.data.legacySkip, '다른 사람이 이미 상신함');
    assert.equal(db.overrides('hong').length, 10);
    assert.equal(db.overrides('hong', 'tripExpense').length, 3);
    assert.equal(db.filings('hong', { kind: 'purchase', ref: 'camera' })[0].filedAt, '2026-01-21');

    const second = run();
    assert.equal(second.purchases.inserted, 0);
    assert.equal(second.purchases.skippedExisting, 3);
    assert.equal(second.trips.skippedExisting, 4);
    assert.equal(Object.values(second.overrides).reduce((n, c) => n + c.inserted, 0), 0);
    assert.equal(second.filings.record, 0);
    assert.equal(db.overrides('hong').length, 10);
    assert.equal(db.filings('hong').length, 5);

    // Serials written later by save_photos survive a replace when the source has none.
    const cable = db.purchase('hong', 'cable');
    db.upsertPurchase({ ...cable, data: { ...cable.data, serials: { '테스트 케이블': ['C-1'] } } });
    const replaced = importLegacy({ legacy, records: [], db, owner: 'hong', filesDir, repo, replace: true });
    assert.equal(replaced.purchases.replaced, 3);
    assert.deepEqual(db.purchase('hong', 'cable').data.serials, { '테스트 케이블': ['C-1'] });
    assert.deepEqual(db.purchase('hong', 'camera').data.serials, { '테스트 카메라': ['SN-1'] });
    db.close();
  });
});
