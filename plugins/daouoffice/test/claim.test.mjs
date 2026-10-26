import { test, describe, before, after, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { Writable } from 'node:stream';
import { McpServer, InMemoryTransport } from '@modelcontextprotocol/server';
import { Client } from '@modelcontextprotocol/client';
import { createRuntime, createLogger } from '../src/runtime.mjs';
import { openDb } from '../src/db.mjs';
import { parseConfig } from '../src/config.mjs';
import { PluginError } from '../src/errors.mjs';
import { runWithMode, assertExecute } from '../src/guard.mjs';
import { READ_ONLY_POSTS } from '../src/dao/api.mjs';
import { kstDate } from '../src/dao/kst.mjs';
import { tripKey } from '../src/dao/trips.mjs';
import { register } from '../src/tools/claim.mjs';
import {
  parsePurposeMap, purposeNameFor, detailFor, effectivePurposeName, resolvePurpose, DEFAULT_CATEGORIES,
} from '../src/dao/expense-map.mjs';
import {
  claimMonth, assertMonthOver, claimTitle, claimRef, noteMatches, savedNotes, sortClaimRows, planClaim,
  groupSummary, claimStatement, statementLine, statementSum, linkCandidates, decideLinks, opinionText, savePurposes,
  claimMismatches, verifyClaim, claimLedger, CLAIM_NOTE_KIND,
} from '../src/dao/expense-claim.mjs';

const code = (fn) => { try { fn(); } catch (e) { return e instanceof PluginError ? e.code : e.name; } return null; };

describe('expense-map: 비목 → 사용용도, 상세 문구', () => {
  const map = parsePurposeMap([
    { category: '비품', purpose: '비품비' },
    { match: '태블릿', unless: '케이스|커버', purpose: '장비자산' },
    { vendor: '^테스트API', detail: '테스트 검색 API 사용료' },
    { purpose: '재료비', fallback: '소모품비' },
    { purpose: '수수료비용', prefer: '지급수수료' },
  ]);

  test('default categories, config overrides and item exceptions with unless', () => {
    assert.equal(purposeNameFor({ category: '재료비', item: '테스트 부품' }, parsePurposeMap([])), DEFAULT_CATEGORIES.재료비);
    assert.equal(purposeNameFor({ category: '비품', item: '의자' }, map), '비품비');
    assert.equal(purposeNameFor({ category: '재료비', item: '테스트 태블릿 본체' }, map), '장비자산');
    assert.equal(purposeNameFor({ category: '재료비', item: '테스트 태블릿 케이스' }, map), '재료비');
  });

  test('an unknown category stops instead of guessing', () => {
    assert.equal(code(() => purposeNameFor({ category: '모르는비목', item: 'x' }, map)), 'VALIDATION');
  });

  test('the example config\'s purposeMap parses', () => {
    const example = JSON.parse(fs.readFileSync(new URL('../config.example.json', import.meta.url), 'utf8'));
    const m = parsePurposeMap(parseConfig(example).purposeMap);
    assert.equal(m.categories.비품, '소모품비');
    assert.equal(m.prefer.수수료비용, '지급수수료');
  });

  test('bad purposeMap entries are a config error', () => {
    assert.equal(code(() => parsePurposeMap([{ match: 'x', unless: '(', purpose: 'y' }])), 'INTERNAL');
    assert.equal(code(() => parsePurposeMap([{ purpose: 'x' }])), 'INTERNAL');
  });

  test('detail comes from the requisition title, counted by items when there are several', () => {
    assert.equal(detailFor({ category: '재료비', purchase: { title: '테스트 센서 구매의 건' } }, map), '테스트 센서 구매');
    assert.equal(detailFor({
      category: '재료비',
      // `품명 / 수량` 한 쌍이 품목 하나다.
      purchases: [{ title: '테스트 센서 외 1종 구매의 건', item: '테스트 센서 / 1개, 테스트 케이블 / 2개' }, { title: '테스트 보드 구매의 건', item: '테스트 보드 / 1개' }],
    }, map), '테스트 센서 외 2종 구매');
    assert.equal(detailFor({ category: '재료비', item: '테스트-부품' }, map), '테스트 부품 구매');
  });

  test('subscription details use the configured wording or "<vendor> 이용료"', () => {
    assert.equal(detailFor({ category: '클라우드구독료', vendor: '테스트API' }, map), '테스트 검색 API 사용료');
    assert.equal(detailFor({ category: '소프트웨어', vendor: '테스트클라우드' }, map), '테스트클라우드 이용료');
  });

  test('prefer and fallback follow the live purpose list', () => {
    const live = [{ code: 'P1', name: '소모품비' }, { code: 'P2', name: '지급수수료' }];
    assert.equal(effectivePurposeName(live, '재료비', map), '소모품비');
    assert.equal(effectivePurposeName(live, '수수료비용', map), '지급수수료');
    assert.equal(effectivePurposeName([{ code: 'P3', name: '재료비' }], '재료비', map), '재료비');
    assert.equal(resolvePurpose(live, '재료비', map), 'P1');
  });

  test('a purpose missing from the list stops with what to do', () => {
    const err = (() => { try { resolvePurpose([{ code: 'P1', name: '사무용품' }], '재료비', parsePurposeMap([])); } catch (e) { return e; } })();
    assert.equal(err.code, 'VALIDATION');
    assert.match(err.message, /재료비.*목록에 없습니다/);
    assert.match(err.message, /fallback/);
  });
});

describe('expense-claim: month rules and title', () => {
  test('a month is over only after its last day (KST)', () => {
    const m = claimMonth('2026-03', '2026-03-31');
    assert.deepEqual([m.from, m.to, m.over], ['2026-03-01', '2026-03-31', false]);
    assert.equal(code(() => assertMonthOver(m, '2026-03-31')), 'VALIDATION');
    assert.equal(code(() => assertMonthOver(claimMonth('2026-03', '2026-04-01'), '2026-04-01')), null);
    assert.equal(claimMonth('2026-02', '2026-04-01').to, '2026-02-28');
  });

  test('a month that is not YYYY-MM is refused', () => {
    for (const bad of ['2026-13', '2026-3', '202603', '']) assert.equal(code(() => claimMonth(bad)), 'VALIDATION', bad);
  });

  test('title follows accounting\'s format, no leading zero, purpose last', () => {
    assert.equal(
      claimTitle({ year: 2026, month: 3, cardLast4: '1234', deptCode: 'rnd', holder: '홍길동', purpose: '재료비' }),
      '2026년 3월 (1234/rnd/홍길동) 법인카드 지출의 건 (재료비)',
    );
    assert.equal(claimTitle({ year: 2026, month: 11, cardLast4: '1234', deptCode: 'ABC', holder: '홍길동' }), '2026년 11월 (1234/ABC/홍길동) 법인카드 지출의 건');
    assert.equal(code(() => claimTitle({ year: 2026, month: 3, cardLast4: '1234', holder: '홍길동' })), 'VALIDATION');
    assert.equal(claimRef('1234', '2026-03', '재료비'), '1234/2026-03/재료비');
  });
});

let seq = 0;
const row = (useDate, merchant, amount, extra = {}) => ({
  id: `r${++seq}`, useDate: `${useDate} 12:00:00`, merchant, amount, status: '작성대기', docId: null, purpose: '', raw: { purchaseId: `r${seq}` }, ...extra,
});
const audit = (r, extra = {}) => ({ row: r, receipt: undefined, receipts: undefined, purchase: undefined, purchases: [], kind: '구매', claimAsIs: null, claimed: Boolean(r.docId), ...extra });

describe('expense-claim: sorting the month\'s rows', () => {
  const rc = { file: 'a.pdf', date: '2026-03-02', vendor: '테스트상점', amount: 45_000, currency: '원', category: '재료비', item: '테스트-부품', data: {} };
  const rcInvoice = { ...rc, file: 'a-invoice.pdf' };
  const trips = [{ key: 'k1', start: '2026-03-05', end: '2026-03-06', place: '테스트시청 (테스트시)', claimName: '테스트시' }];
  const confirmed = [{ kind: 'tripExpense', match: { date: '2026-03-01', merchant: '테스트호텔' }, data: { kind: '숙박', trip: '2026-03-05' } }];

  function fixture() {
    const r = {
      receipt: row('2026-03-02', '테스트상점', 45_000),
      fuel: row('2026-03-05', '테스트주유소', 50_000),
      lodging: row('2026-03-01', '테스트호텔', 80_000),
      purchase: row('2026-03-10', '테스트스토어', 30_000),
      asIs: row('2026-03-11', '테스트전자상가', 100_000),
      claimed: row('2026-03-12', '테스트상점', 9_000, { status: '결재완료', docId: '1000000000000900' }),
      mystery: row('2026-03-13', '테스트카페', 5_000),
      otherCard: row('2026-03-14', '테스트몰', 7_000),
      april: row('2026-04-01', '테스트상점', 1_000),
    };
    const audited = [
      audit(r.receipt, { receipt: rc }),
      audit(r.fuel, { kind: '주유' }),
      audit(r.lodging, { kind: '경비' }),
      audit(r.purchase, { purchase: { key: 'sensor', title: '테스트 센서 구매의 건', docId: '1000000000000101' }, purchases: [{ key: 'sensor', title: '테스트 센서 구매의 건', docId: '1000000000000101' }] }),
      audit(r.asIs, { claimAsIs: { kind: 'claimAsIs', data: { purposeName: '소모품비', detail: '테스트 결제 취소 및 환불' } } }),
      audit(r.claimed, { claimed: true }),
      audit(r.mystery, { kind: '식대' }),
      audit(r.otherCard, { receipt: { ...rc, file: 'b.pdf', date: '2026-03-14', amount: 7_000, data: { cardLast4: '9999' } } }),
      audit(r.april),
    ];
    return { r, audited };
  }

  test('each row lands in exactly one bucket', () => {
    const { r, audited } = fixture();
    const s = sortClaimRows({ audited, from: '2026-03-01', to: '2026-03-31', receipts: [rc, rcInvoice], trips, confirmed, cardLast4: '1234' });
    const by = Object.fromEntries(s.items.map((i) => [i.row.id, i]));
    assert.equal(s.inMonth.length, 8, 'April is cut on the KST date');
    assert.equal(by[r.receipt.id].source, 'receipt');
    assert.deepEqual(by[r.receipt.id].receipts.map((x) => x.file).sort(), ['a-invoice.pdf', 'a.pdf'], 'invoice and receipt of one payment go together');
    assert.equal(by[r.fuel.id].detail, '테스트시 출장 유류비');
    assert.equal(by[r.fuel.id].purposeName, '차량유지비');
    assert.equal(by[r.lodging.id].detail, '테스트시 출장 숙박비', 'lodging bought before the trip matches only when confirmed');
    assert.equal(by[r.purchase.id].source, 'purchase');
    assert.equal(by[r.asIs.id].purposeName, '소모품비');
    assert.deepEqual(s.claimed.map((a) => a.row.id), [r.claimed.id]);
    assert.deepEqual(s.unexplained.map((u) => u.row.id).sort(), [r.mystery.id, r.otherCard.id].sort());
    assert.match(s.unexplained.find((u) => u.row.id === r.otherCard.id).reason, /다른 카드\(9999\)/);
  });

  test('notes explain or exclude rows; notes of this call win over saved ones', () => {
    const { r, audited } = fixture();
    const notes = [
      { date: '2026-03-13', amount: 5_000, detail: '테스트 회의 다과', purpose: '회의비' },
      { date: '2026-03-14', amount: 7_000, exclude: true, reason: '개인 결제' },
      { date: '2026-03-20', amount: 1, detail: '없는 결제', purpose: '소모품비' },
      ...savedNotes([{ id: 9, kind: CLAIM_NOTE_KIND, match: { date: '2026-03-13', amount: 5_000 }, data: { detail: '옛 메모', purpose: '식대' } }]),
    ];
    const s = sortClaimRows({ audited, from: '2026-03-01', to: '2026-03-31', receipts: [rc], trips, confirmed, notes, cardLast4: '1234' });
    assert.equal(s.unexplained.length, 0);
    const mystery = s.items.find((i) => i.row.id === r.mystery.id);
    assert.deepEqual([mystery.source, mystery.detail, mystery.purposeName], ['note', '테스트 회의 다과', '회의비']);
    assert.deepEqual(s.excluded.map((e) => [e.row.id, e.reason]), [[r.otherCard.id, '개인 결제']]);
    assert.deepEqual(s.unusedNotes.map((n) => n.date), ['2026-03-20']);
  });

  test('a note needs both detail and purpose to file an unexplained row', () => {
    const { r, audited } = fixture();
    const s = sortClaimRows({ audited, from: '2026-03-01', to: '2026-03-31', receipts: [rc], trips, confirmed, notes: [{ date: '2026-03-13', amount: 5_000, detail: '다과' }], cardLast4: '1234' });
    assert.match(s.unexplained.find((u) => u.row.id === r.mystery.id).reason, /모두 있어야/);
  });

  test('note matching uses date, amount and an optional merchant part', () => {
    const r = row('2026-03-13', 'TEST CAFE', 5_000);
    assert.ok(noteMatches({ date: '2026-03-13', amount: 5_000 }, r));
    assert.ok(noteMatches({ date: '2026-03-13', amount: 5_000, merchant: 'cafe' }, r));
    assert.ok(!noteMatches({ date: '2026-03-13', amount: 5_000, merchant: '테스트' }, r));
    assert.ok(!noteMatches({ date: '2026-03-14', amount: 5_000 }, r));
  });
});

describe('expense-claim: per-purpose split, totals and links', () => {
  const purposes = [{ code: 'P1', name: '재료비' }, { code: 'P2', name: '차량유지비' }, { code: 'P3', name: '소모품비' }];
  const map = parsePurposeMap([]);
  const items = [
    { row: row('2026-03-02', '테스트상점', 45_000), source: 'receipt', category: '재료비', item: '테스트-부품', receipts: [{ file: 'a.pdf' }], purchases: [] },
    { row: row('2026-03-10', '테스트스토어', 30_000), source: 'purchase', category: '재료비', item: '', purchase: { key: 'sensor', title: '테스트 센서 구매의 건', docId: '1000000000000101' }, purchases: [], receipts: [] },
    { row: row('2026-03-05', '테스트주유소', 50_000), source: 'trip', purposeName: '차량유지비', detail: '테스트시 출장 유류비', receipts: [], purchases: [] },
  ];

  test('rows are grouped by the effective purpose and details are checked', () => {
    const p = planClaim({ items, purposes, map });
    assert.deepEqual(groupSummary(p.groups), [
      { purpose: '재료비', count: 2, amount: 75_000 },
      { purpose: '차량유지비', count: 1, amount: 50_000 },
    ]);
    assert.deepEqual(p.groups.get('재료비').map((r) => r.detail), ['테스트 부품 구매', '테스트 센서 구매']);
    assert.equal(p.problems.length, 0);
  });

  test('config can rename the purpose of a trip expense kind', () => {
    const tripItem = { ...items[2], tripKind: '주유' };
    const p = planClaim({ items: [tripItem], purposes: [...purposes, { code: 'P9', name: '유류비' }], map: parsePurposeMap([{ category: '주유', purpose: '유류비' }]) });
    assert.deepEqual([...p.groups.keys()], ['유류비']);
  });

  test('a detail that breaks the writing rules blocks its purpose; an unknown category blocks every purpose', () => {
    const bad = [
      ...items,
      { row: row('2026-03-07', '테스트상점', 1_000), source: 'note', purposeName: '소모품비', detail: '다양한 부품 구매', receipts: [], purchases: [] },
      { row: row('2026-03-08', '테스트상점', 2_000), source: 'receipt', category: '모르는비목', item: 'x', receipts: [], purchases: [] },
    ];
    const p = planClaim({ items: bad, purposes, map });
    assert.deepEqual(p.problems.map((x) => x.purpose), ['소모품비', null]);
    assert.match(p.problems[0].text, /빈 수식어/);
  });

  test('the statement adds up to the month\'s card total', () => {
    const planned = planClaim({ items, purposes, map });
    const month = [
      ...items.map((i) => i.row), row('2026-03-12', '테스트상점', 9_000), row('2026-03-14', '테스트몰', 7_000), row('2026-03-15', 'OTHERSUB', 20_000),
      row('2026-03-16', '테스트취소', 4_000), row('2026-03-17', '테스트취소', -4_000), row('2026-03-18', '테스트반품', 6_000),
    ];
    const sorted = {
      month,
      claimed: [{ row: month[3] }],
      items,
      excluded: [{ row: month[4] }],
      elsewhere: [{ row: month[5] }],
      cancelled: [{ row: month[6] }, { row: month[7] }],
      refunded: [{ row: month[8] }],
      unexplained: [],
    };
    const st = claimStatement({ sorted, planned, purpose: '재료비' });
    assert.deepEqual(st, {
      claimed: 9_000, thisDoc: 75_000, otherPurposes: 50_000, elsewhere: 20_000, excluded: 7_000,
      cancelled: 0, cancelledCount: 2, refunded: 6_000, unexplained: 0, total: 167_000,
    });
    assert.equal(statementSum(st), st.total);
    assert.equal(statementLine(st), '이 달 카드 내역 167,000원 = 이미 청구 9,000원 + 이번 문서 75,000원 + 다른 사용용도 50,000원 + 다른 사람 몫 20,000원 + 뺀 결제 7,000원 + 취소 2건 0원 + 환불 6,000원');
  });

  test('link candidates are the rows\' requisitions and their inspection reports', () => {
    const rows = planClaim({ items, purposes, map }).groups.get('재료비');
    const c = linkCandidates(rows, {
      inspections: [
        { id: 1, docId: '1000000000000102', ref: '2026-03-02/45000', filedAt: '2026-04-01T10:00', title: '테스트 검수조서' },
        { id: 2, docId: '1000000000000105', ref: '2026-03-02/45000', filedAt: '2026-04-02T10:00', title: '고쳐 올린 검수조서' },
        { id: 3, docId: '1000000000000106', ref: '2026-03-10/30000#sensor', filedAt: '2026-04-01T10:00', title: '품의별 검수조서' },
        { id: 4, docId: '1000000000000103', ref: '2026-03-11/9999', title: '다른 결제의 검수조서' },
        { id: 5, docId: '1000000000000104', ref: 'legacy', detail: { purchaseKeys: ['sensor'] }, title: '옛 검수조서' },
      ],
    });
    assert.deepEqual(c.map((x) => [x.kind, x.docId]).sort(), [
      ['검수조서', '1000000000000104'],
      ['검수조서', '1000000000000105'],
      ['검수조서', '1000000000000106'],
      ['구매 품의서', '1000000000000101'],
    ]);
  });

  test('unfinished documents block, unless the person chose linkDone or withoutLinks', () => {
    const cands = [{ docId: '1', title: '끝난 품의', done: true }, { docId: '2', title: '진행중 품의', done: false }];
    assert.deepEqual(decideLinks(cands).blocked.map((c) => c.docId), ['2']);
    const done = decideLinks(cands, { linkDone: true });
    assert.deepEqual([done.links.map((c) => c.docId), done.opinion.map((c) => c.docId)], [['1'], ['2']]);
    const none = decideLinks(cands, { withoutLinks: true, linkDone: true });
    assert.deepEqual([none.links.length, none.opinion.length], [0, 2]);
    assert.equal(opinionText(done.opinion), '결재가 끝나지 않아 문서 연결 대신 적는다.\n2 진행중 품의');
    assert.equal(opinionText([]), '');
  });

  test('server verification reports every difference', () => {
    const ok = { status: '진행중', title: 'T', attachCount: 2, references: ['1'] };
    assert.deepEqual(claimMismatches(ok, { title: 'T', files: ['a', 'b'], links: [{ id: '1' }] }), []);
    const bad = claimMismatches({ status: '', title: '법인카드 지출품의서', attachCount: 0, references: [] }, { title: 'T', files: ['a'], links: [{ id: '1', docNum: 'D-1' }] });
    assert.equal(bad.length, 4);
  });

  test('verifyClaim waits while the status is still empty', async () => {
    let n = 0;
    const api = async () => ({ status: 200, ok: true, data: { data: { document: n++ < 2 ? {} : { docStatusName: '진행중', title: 'T', attachCount: 1, references: [{ id: 7 }] } } } });
    const v = await verifyClaim(api, '1000000000000300', { waitMs: 1000, sleep: async () => {} });
    assert.deepEqual([v.status, v.attachCount, v.references], ['진행중', 1, ['7']]);
  });
});

describe('expense-claim: every ledger row is accounted for', () => {
  const asIs = (purposeName) => ({ claimAsIs: { kind: 'claimAsIs', data: { purposeName, detail: '테스트 비품 구매' } } });
  function ledger() {
    const r = {
      paid: row('2026-03-05', '테스트상점', 10_000),
      lateCancelled: row('2026-03-31', '테스트몰', 30_000),
      pairPlus: row('2026-03-10', '테스트취소', 4_000),
      pairMinus: row('2026-03-11', '테스트취소', -4_000),
      refund: row('2026-03-12', '테스트반품', 6_000),
      elsewhere: row('2026-03-13', 'OTHERSUB', 20_000),
      negPrevOpen: row('2026-03-01', '테스트앞달', -2_000),
      negPrevClaimed: row('2026-03-02', '테스트청구', -3_000),
      negLonely: row('2026-03-03', '테스트환불', -1_500),
      claimedLate: row('2026-03-30', '테스트구독', 8_000, { docId: '1000000000000901', status: '결재중' }),
      zero: row('2026-03-20', '테스트영원', 0),
    };
    const edge = [
      row('2026-04-01', '테스트몰', -30_000),
      row('2026-02-27', '테스트앞달', 2_000),
      row('2026-02-26', '테스트청구', 3_000, { docId: '1000000000000902', status: '결재완료' }),
      row('2026-04-02', '테스트구독', -8_000),
    ];
    const audited = [audit(r.paid, asIs('소모품비')), audit(r.lateCancelled), audit(r.claimedLate, { claimed: true })];
    const overrides = [
      { kind: 'refunded', match: { date: '2026-03-12', merchant: '테스트반품', amount: 6_000 }, data: { why: '반품 확인' } },
      { kind: 'elsewhere', match: { date: '2026-03-13', merchant: 'OTHERSUB', amount: 20_000 }, data: {} },
    ];
    return {
      r, edge, audited, overrides, ledger: Object.values(r), cancelled: new Set([r.pairPlus.id, r.pairMinus.id]),
    };
  }
  const ids = (xs) => xs.map((x) => x.row.id).sort();

  test('cancellations, refunds, other people\'s charges and negatives each get a bucket', () => {
    const f = ledger();
    const s = sortClaimRows({ ...f, from: '2026-03-01', to: '2026-03-31', cardLast4: '1234' });
    assert.deepEqual(ids(s.items), [f.r.paid.id]);
    assert.deepEqual(ids(s.claimed), [f.r.claimedLate.id]);
    assert.deepEqual(ids(s.cancelled), ids([f.r.pairPlus, f.r.pairMinus, f.r.zero, f.r.negPrevOpen, f.r.lateCancelled].map((x) => ({ row: x }))));
    assert.match(s.cancelled.find((c) => c.row.id === f.r.lateCancelled.id).reason, /2026-04-01 에 취소됨/);
    assert.match(s.cancelled.find((c) => c.row.id === f.r.negPrevOpen.id).reason, /앞 달 결제/);
    assert.deepEqual(ids(s.refunded), [f.r.refund.id]);
    assert.deepEqual(ids(s.elsewhere), [f.r.elsewhere.id]);
    assert.deepEqual(ids(s.unexplained), ids([f.r.negPrevClaimed, f.r.negLonely].map((x) => ({ row: x }))));
    assert.match(s.unexplained.find((u) => u.row.id === f.r.negPrevClaimed.id).reason, /앞 달에 청구한 결제/);
    assert.match(s.unexplained.find((u) => u.row.id === f.r.negLonely.id).reason, /짝이 되는 결제가 없는/);
    assert.equal(s.warnings.length, 1);
    assert.match(s.warnings[0], /이미 청구한 결제 2026-03-30 테스트구독 8,000원 가 2026-04-02 에 취소/);
  });

  test('the statement equals the raw ledger sum, negatives included', () => {
    const f = ledger();
    const notes = [
      { date: '2026-03-02', amount: -3_000, exclude: true, reason: '회계 확인: 다음 청구에서 상계' },
      { date: '2026-03-03', amount: -1_500, exclude: true, reason: '개인 결제 환불' },
    ];
    const s = sortClaimRows({ ...f, notes, from: '2026-03-01', to: '2026-03-31', cardLast4: '1234' });
    assert.equal(s.unexplained.length, 0);
    assert.deepEqual(ids(s.excluded), ids([f.r.negPrevClaimed, f.r.negLonely].map((x) => ({ row: x }))));
    const planned = planClaim({ items: s.items, purposes: [{ code: 'P3', name: '소모품비' }], map: parsePurposeMap([]) });
    const st = claimStatement({ sorted: s, planned, purpose: '소모품비' });
    const raw = f.ledger.reduce((t, r) => t + r.amount, 0);
    assert.equal(st.total, raw);
    assert.equal(statementSum(st), raw);
    assert.equal(st.thisDoc, 10_000);
    assert.equal(st.excluded, -4_500);
  });

  test('a note cannot file a negative row; only exclude is accepted', () => {
    const f = ledger();
    const notes = [{ date: '2026-03-03', amount: -1_500, detail: '환불', purpose: '소모품비' }];
    const s = sortClaimRows({ ...f, notes, from: '2026-03-01', to: '2026-03-31', cardLast4: '1234' });
    assert.match(s.unexplained.find((u) => u.row.id === f.r.negLonely.id).reason, /음수 결제/);
  });

  test('claimLedger counts the rows tied to the document; a difference is a mismatch', async () => {
    const usage = async () => ({ rows: [
      { id: 'a', amount: 45_000, docId: '1000000000000555' }, { id: 'c', amount: 30_000, docId: '1000000000000555' }, { id: 'x', amount: 1, docId: null },
    ] });
    const l = await claimLedger(null, { cardId: 'card-1', from: '2026-03-01', to: '2026-03-31', docId: '1000000000000555', usage });
    assert.deepEqual(l, { ids: ['a', 'c'], amount: 75_000 });
    const ok = { status: '진행중', title: 'T', attachCount: 0, references: [] };
    assert.deepEqual(claimMismatches(ok, { title: 'T', files: [], links: [], ledger: l, rows: [{ id: 'c', amount: 30_000 }, { id: 'a', amount: 45_000 }] }), []);
    assert.match(claimMismatches(ok, { title: 'T', files: [], links: [], ledger: l, rows: [{ id: 'a', amount: 45_000 }] })[0], /문서에 묶인 카드 행이 다릅니다: 2건 75,000원 \(기대 1건 45,000원\)/);
  });
});

describe('expense-claim: saving purposes is a server write', () => {
  const rows = [{ raw: { purchaseId: 'r1', bpName: '테스트상점' }, purchaseId: 'r1', purposeCode: 'P1', detail: '테스트 부품 구매' }];

  test('refused outside an approved execute', async () => {
    const api = async (p, opts) => { if (opts?.method === 'POST' && !READ_ONLY_POSTS.includes(p)) assertExecute(p); return { status: 200, ok: true, data: {} }; };
    await assert.rejects(runWithMode('preview', () => savePurposes(api, rows)), (e) => e.code === 'INTERNAL');
  });

  test('sends the row with purpose, detail and reference', async () => {
    const calls = [];
    const api = async (p, opts) => { calls.push({ p, ...opts }); return { status: 200, ok: true, data: {} }; };
    assert.equal(await runWithMode('execute', () => savePurposes(api, rows)), 1);
    assert.equal(calls[0].p, '/eacc/api/ess/myExp/saveCompanyCard');
    assert.deepEqual(calls[0].body.paramList[0], { purchaseId: 'r1', bpName: '테스트상점', usePurpose: 'P1', usePurposeDetail: '테스트 부품 구매', referenceId: 'r1', referenceObject: '10' });
  });
});

// ------------------------------------------------------------------ the tool over MCP

const BASE = 'https://example.daouoffice.com';
const CREDS = { 'xylolabs/credentials': { username: 'hong', password: 'pw' } };
const SENSOR_DOC = '1000000000000101';
const INSPECTION_DOC = '1000000000000102';

function fakeDaou() {
  const state = {
    rows: [
      { purchaseId: 'a', useDate: '2026-03-02 10:00', bpName: '테스트상점', grossAmount: 45_000, status: '9' },
      { purchaseId: 'b', useDate: '2026-03-05 09:00', bpName: '테스트주유소', grossAmount: 50_000, status: '9' },
      { purchaseId: 'c', useDate: '2026-03-10 15:00', bpName: '테스트스토어', grossAmount: 30_000, status: '0' },
      { purchaseId: 'd', useDate: '2026-03-15 11:00', bpName: 'OTHERSUB', grossAmount: 20_000, status: '9' },
      { purchaseId: 'e', useDate: '2026-03-20 11:00', bpName: '테스트문구', grossAmount: 12_000, status: '2', docId: '1000000000000900', usePurposeName: '소모품비' },
      { purchaseId: 'f', useDate: '2026-04-01 00:30', bpName: '테스트상점', grossAmount: 3_000, status: '9' },
    ],
    docs: {
      [SENSOR_DOC]: { docStatusName: '완료', docNum: 'TEST-0001', title: '테스트 센서 구매의 건' },
      [INSPECTION_DOC]: { docStatusName: '완료', docNum: 'TEST-0002', title: '테스트 검수조서' },
      1000000000000900: { docStatusName: '완료', docNum: 'TEST-0009', title: '2026년 3월 (1234/rnd/홍길동) 법인카드 지출의 건 (소모품비)' },
    },
    drafts: [],
    calls: [],
    created: [],
    submitted: [],
    draftPages: [],
    attachOverride: null,
  };
  const raw = async (p, opts = {}) => {
    state.calls.push({ path: p, method: opts.method ?? 'GET', body: opts.body });
    const ok = (data) => ({ status: 200, ok: true, data });
    if (p === '/eacc/api/ess/custom/code/getList' && opts.body?.comboType === 'CREDIT_CARD') {
      return ok({ data: { list: [{ valueField: 'card-1', displayField: '연구카드(홍길동) [홍길동]', creditcardNo: '1111-22**-****-1234', description2: '테스트카드' }] } });
    }
    if (p === '/eacc/api/ess/custom/code/getList' && opts.body?.comboType === 'USE_PURPOSE') {
      return ok({ data: { list: [['P1', '재료비'], ['P2', '차량유지비'], ['P3', '소모품비'], ['P4', '회의비'], ['P5', '사무용품']].map(([valueField, displayField]) => ({ valueField, displayField })) } });
    }
    if (p === '/eacc/api/ess/myExp/getCompanyCardList') {
      // Like the server: only rows inside fromDate..toDate (YYYYMMDD).
      const day = (r) => r.useDate.slice(0, 10).replace(/-/g, '');
      return ok({ data: { list: state.rows.filter((r) => day(r) >= opts.body.fromDate && day(r) <= opts.body.toDate) } });
    }
    if (p === '/eacc/api/ess/myExp/saveCompanyCard') {
      const names = { P1: '재료비', P2: '차량유지비', P3: '소모품비', P4: '회의비', P5: '사무용품' };
      for (const x of opts.body.paramList) {
        Object.assign(state.rows.find((r) => r.purchaseId === x.purchaseId), { status: '0', usePurpose: x.usePurpose, usePurposeName: names[x.usePurpose], usePurposeDetail: x.usePurposeDetail });
      }
      return ok({ data: {} });
    }
    if (p.startsWith('/gw/api/approval/doclist/draft/all')) {
      const page = Number(p.match(/page=(\d+)/)[1]);
      state.draftPages.push(page);
      return ok({ data: state.drafts.slice(page * 100, page * 100 + 100) });
    }
    const m = p.match(/^\/gw\/api\/approval\/document\/(\d+)$/);
    if (m && state.docs[m[1]]) return ok({ data: { document: state.docs[m[1]] } });
    return { status: 404, ok: false, data: null };
  };
  // Like makeApi(): anything that is not a GET or a known read-only POST needs an approved execute.
  const api = async (p, opts = {}) => {
    const method = String(opts.method ?? 'GET').toUpperCase();
    if (method !== 'GET' && !READ_ONLY_POSTS.includes(p.split('?')[0])) assertExecute(`${method} ${p}`);
    return raw(p, opts);
  };
  const sessions = {
    sessions: 0,
    async withSession(owner, creds, opts, fn) { this.sessions++; return fn({ page: {}, api, context: {}, owner, baseUrl: BASE, fresh: false }); },
  };
  // The draft window steps, replaced: they check the guard like the real ones and leave a server document.
  const dom = {
    async createClaimDocument(page, baseUrl, { cardId, purchaseIds, progress }) {
      assertExecute('지출결의 문서 생성');
      const docId = String(1_000_000_000_000_550 + state.created.length);
      state.created.push({ docId, cardId, purchaseIds });
      // Like the server: the document exists (임시저장) and the card rows carry its id.
      progress.created = true;
      state.docs[docId] = { docStatusName: '임시저장', title: '' };
      for (const id of purchaseIds) Object.assign(state.rows.find((r) => r.purchaseId === id), { docId, status: '1' });
      if (state.failAfterCreate) throw new PluginError('UPSTREAM', '기안 창이 열리지 않았습니다.');
      return { docId, docPage: { async close() {} } };
    },
    async attachAndSubmit(docPage, opts) {
      assertExecute('지출결의 결재요청');
      state.submitted.push(opts);
      if (state.failBeforeSend) throw new PluginError('UPSTREAM', '결재선 편집기가 열리지 않았습니다.');
      opts.progress.sent = true;
      if (state.failAfterSend) throw new PluginError('UPSTREAM', '화면이 제때 응답하지 않았습니다.');
      const { docId } = state.created.at(-1);
      state.docs[docId] = {
        docStatusName: '진행중', title: opts.title, attachCount: state.attachOverride ?? opts.files.length,
        references: opts.links.map((l) => ({ id: l.id })),
      };
      return `기안 홍길동 → ${opts.approvers.map((a) => `${a.type} ${a.name}`).join(' → ')}`;
    },
  };
  return { state, sessions, dom };
}

describe('file_expense_claim over MCP with a fake DaouOffice', () => {
  let client;
  let daou;
  let db;
  let dataDir;
  const call = (args, meta = CREDS) => client.callTool({ name: 'file_expense_claim', arguments: args, _meta: meta });

  before(async () => {
    daou = fakeDaou();
    dataDir = fs.mkdtempSync(path.join(os.tmpdir(), 'dao-claim-'));
    db = openDb(':memory:');
    db.upsertAccount({ owner: 'hong', name: '홍길동', deptId: '10', deptCode: 'rnd', deptName: '연구개발팀' });
    const config = parseConfig({ baseUrl: BASE, people: { ceo: '김대표' }, cards: { 1234: { project: '테스트과제' } } });
    const rt = createRuntime({ config, db, sessions: daou.sessions, dataDir, logger: createLogger({ stream: new Writable({ write(_c, _e, cb) { cb(); } }) }) });
    const receipts = rt.filesDir('hong', 'receipts');
    fs.writeFileSync(path.join(receipts, '2026-03-02_테스트상점.pdf'), '%PDF-1.4 test');
    db.addReceipt({ owner: 'hong', file: '2026-03-02_테스트상점.pdf', date: '2026-03-02', category: '재료비', item: '테스트-부품', vendor: '테스트상점', amount: 45_000, currency: '원' });
    db.upsertPurchase({ owner: 'hong', key: 'sensor', status: 'submitted', title: '테스트 센서 구매의 건', paidAt: '2026-03-10', amountKrw: 30_000, docId: SENSOR_DOC });
    db.addFiling({ owner: 'hong', kind: 'inspection', ref: '2026-03-10/30000', docId: INSPECTION_DOC, title: '테스트 검수조서', status: '완료' });
    const trip = { start: '2026-03-05', end: '2026-03-05', place: '테스트시청 (테스트시)', claimName: '테스트시', transport: '기타', goal: '실증 부지 인허가 협의' };
    db.upsertTrip({ owner: 'hong', key: tripKey(trip), data: trip, status: 'submitted' });
    db.addOverride({ owner: 'hong', kind: 'elsewhere', match: { date: '2026-03-15', merchant: 'OTHERSUB', amount: 20_000 }, data: {} });

    const server = new McpServer({ name: 'daouoffice', version: '0.0.0' }, { capabilities: { tools: {} } });
    register(server, rt, { dom: daou.dom });
    const [a, b] = InMemoryTransport.createLinkedPair();
    await server.connect(a);
    client = new Client({ name: 't', version: '0' });
    await client.connect(b);
  });
  after(async () => { await client.close(); db.close(); fs.rmSync(dataDir, { recursive: true, force: true }); });
  beforeEach(() => {
    daou.state.calls.length = 0;
    daou.state.drafts = [];
    daou.state.docs[INSPECTION_DOC].docStatusName = '완료';
  });

  test('the tool is a gated write tool', async () => {
    const { tools } = await client.listTools();
    const t = tools.find((x) => x.name === 'file_expense_claim');
    assert.deepEqual(t.annotations, { readOnlyHint: false, destructiveHint: true });
    assert.equal(t._meta['xylolabs/kind'], 'write');
  });

  test('preview shows the document, the split, the statement, the line and the links, and writes nothing', async () => {
    const r = await call({ card: '1234', month: '2026-03', purpose: '재료비' });
    assert.ok(!r.isError, r.content[0].text);
    const text = r.content[0].text;
    assert.match(text, /2026년 3월 \(1234\/rnd\/홍길동\) 법인카드 지출의 건 \(재료비\)/);
    assert.match(text, /이번 문서: 재료비 2건 75,000원 · 영수증 1개/);
    assert.match(text, /재료비 2건 75,000원, 차량유지비 1건 50,000원/);
    assert.match(text, /이 달 카드 내역 157,000원 = 이미 청구 12,000원 \+ 이번 문서 75,000원 \+ 다른 사용용도 50,000원 \+ 다른 사람 몫 20,000원/);
    assert.match(text, /결재선: 기안 나 → 결재 김대표/);
    assert.match(text, /문서 연결 2건: 구매 품의서 .*TEST-0001.*검수조서 .*TEST-0002/);
    assert.equal(r.structuredContent.preview, true);
    assert.deepEqual(r.structuredContent.rows.map((x) => x.detail), ['테스트 부품 구매', '테스트 센서 구매']);
    assert.ok(!daou.state.calls.some((c) => c.path.includes('saveCompanyCard')), 'preview never saves purposes');
    assert.equal(r.structuredContent.rows.some((x) => 'raw' in x), false);
  });

  test('trip expenses without receipts are their own purpose', async () => {
    const r = await call({ card: '1234', month: '2026-03', purpose: '차량유지비' });
    assert.ok(!r.isError, r.content[0].text);
    assert.match(r.content[0].text, /테스트시 출장 유류비 · 출장 경비/);
    assert.match(r.content[0].text, /문서 연결: 없음/);
  });

  test('a month that is not over is refused before anyone logs in', async () => {
    const before = daou.sessions.sessions;
    const r = await call({ card: '1234', month: kstDate().slice(0, 7), purpose: '재료비' });
    assert.equal(r.structuredContent.code, 'VALIDATION');
    assert.match(r.content[0].text, /아직 끝나지 않았습니다/);
    assert.equal(daou.sessions.sessions, before);
  });

  test('without a purpose the tool lists the groups', async () => {
    const r = await call({ card: '1234', month: '2026-03' });
    assert.equal(r.structuredContent.code, 'VALIDATION');
    assert.match(r.content[0].text, /재료비 2건 75,000원, 차량유지비 1건 50,000원/);
  });

  test('a charge on the last day cancelled on the 1st is shown as cancelled, not claimed', async () => {
    daou.state.rows.push(
      { purchaseId: 'x1', useDate: '2026-03-31 23:00', bpName: '테스트몰', grossAmount: 30_000, status: '9' },
      { purchaseId: 'x2', useDate: '2026-04-01 09:00', bpName: '테스트몰', grossAmount: -30_000, status: '9' },
    );
    try {
      const r = await call({ card: '1234', month: '2026-03', purpose: '재료비' });
      assert.ok(!r.isError, r.content[0].text);
      assert.match(r.content[0].text, /취소: 2026-03-31 테스트몰 30,000원 \(2026-04-01 에 취소됨\)/);
      assert.match(r.content[0].text, /이 달 카드 내역 187,000원 = .* \+ 취소 1건 30,000원/);
      assert.equal(r.structuredContent.statement.total, 187_000);
    } finally {
      daou.state.rows.splice(-2);
    }
  });

  test('an unpaired negative row in the month stops the claim and is listed', async () => {
    daou.state.rows.push({ purchaseId: 'x3', useDate: '2026-03-21 09:00', bpName: '테스트환불처', grossAmount: -9_000, status: '9' });
    try {
      const r = await call({ card: '1234', month: '2026-03', purpose: '재료비' });
      assert.equal(r.structuredContent.code, 'VALIDATION');
      assert.match(r.content[0].text, /2026-03-21 테스트환불처 -9,000원: 짝이 되는 결제가 없는 취소나 환불/);
      const ok = await call({ card: '1234', month: '2026-03', purpose: '재료비', notes: [{ date: '2026-03-21', amount: -9_000, exclude: true, reason: '개인 결제 환불' }] });
      assert.ok(!ok.isError, ok.content[0].text);
      assert.match(ok.content[0].text, /뺀 결제: 2026-03-21 테스트환불처 -9,000원 \(개인 결제 환불\)/);
    } finally {
      daou.state.rows.pop();
    }
  });

  test('an unexplained row stops every purpose and is listed; a note lets it through', async () => {
    daou.state.rows.push({ purchaseId: 'g', useDate: '2026-03-25 13:00', bpName: '테스트카페', grossAmount: 5_000, status: '9' });
    try {
      const r = await call({ card: '1234', month: '2026-03', purpose: '재료비' });
      assert.equal(r.structuredContent.code, 'VALIDATION');
      assert.match(r.content[0].text, /2026-03-25 테스트카페 5,000원/);
      const ok = await call({ card: '1234', month: '2026-03', purpose: '회의비', notes: [{ date: '2026-03-25', amount: 5_000, detail: '테스트 회의 다과', purpose: '회의비' }] });
      assert.ok(!ok.isError, ok.content[0].text);
      assert.match(ok.content[0].text, /테스트 회의 다과 · 메모/);
      assert.equal(db.overrides('hong', CLAIM_NOTE_KIND).length, 0, 'notes are saved only after an approved execute');
    } finally {
      daou.state.rows.pop();
    }
  });

  test('a live claim with the same title is a second document and is refused', async () => {
    daou.state.drafts = [{ documentId: '1000000000000300', title: '2026년 3월 (1234/rnd/홍길동) 법인카드 지출의 건 (재료비)', docStatusName: '진행중', formName: '법인카드 지출품의서' }];
    const r = await call({ card: '1234', month: '2026-03', purpose: '재료비' });
    assert.equal(r.structuredContent.code, 'CONFLICT');
    assert.match(r.content[0].text, /두 번째 문서/);
    daou.state.drafts = [{ ...daou.state.drafts[0], docStatusName: '반려' }];
    const again = await call({ card: '1234', month: '2026-03', purpose: '재료비' });
    assert.ok(!again.isError, 'a returned claim can be filed again');
  });

  test('the duplicate check reads the draft box back to the claim month, not just the newest pages', async () => {
    const filler = (n, date) => Array.from({ length: n }, (_, i) => ({ documentId: String(1_000_000_000_001_000 + i), title: `테스트 품의 ${i}`, docStatusName: '완료', draftedAt: `${date} 09:00` }));
    daou.state.drafts = [
      ...filler(350, '2026-04-10'),
      { documentId: '1000000000000301', title: '2026년 3월 (1234/rnd/홍길동) 법인카드 지출의 건 (재료비)', docStatusName: '진행중', draftedAt: '2026-04-02 10:00' },
      ...filler(100, '2026-02-20'),
      ...filler(100, '2026-01-20'),
    ];
    daou.state.draftPages = [];
    const r = await call({ card: '1234', month: '2026-03', purpose: '재료비' });
    assert.equal(r.structuredContent.code, 'CONFLICT');
    assert.match(r.content[0].text, /문서 ID 1000000000000301/);
    assert.deepEqual(daou.state.draftPages, [0, 1, 2, 3], 'page 3 already reaches February, so it stops there');
  });

  test('a purpose whose rows are all claimed already says so', async () => {
    const r = await call({ card: '1234', month: '2026-03', purpose: '소모품비' });
    assert.equal(r.structuredContent.code, 'CONFLICT');
    assert.match(r.content[0].text, /이미 올라가 있습니다/);
  });

  test('an inspection report still in progress blocks the claim unless linkDone', async () => {
    daou.state.docs[INSPECTION_DOC].docStatusName = '진행중';
    const r = await call({ card: '1234', month: '2026-03', purpose: '재료비' });
    assert.equal(r.structuredContent.code, 'VALIDATION');
    assert.match(r.content[0].text, /결재가 끝나지 않은 문서 1건/);
    const ok = await call({ card: '1234', month: '2026-03', purpose: '재료비', linkDone: true });
    assert.ok(!ok.isError, ok.content[0].text);
    assert.match(ok.content[0].text, /문서 연결 1건/);
    assert.match(ok.content[0].text, /기안의견에 적을 문서 1건/);
  });

  test('a card that is not visible is NOT_FOUND', async () => {
    const r = await call({ card: '9999', month: '2026-03', purpose: '재료비' });
    assert.equal(r.structuredContent.code, 'NOT_FOUND');
  });

  // Execute tests run last: each one leaves a filed claim behind. Execute needs the approved preview first.
  const EXECUTE = { ...CREDS, 'xylolabs/mode': 'execute' };
  const execute = async (args) => { await call(args); return call(args, EXECUTE); };

  test('execute without a matching preview, or after the plan changed, writes nothing', async () => {
    // Arguments nobody previewed before (the earlier tests previewed the plain ones).
    const args = { card: '1234', month: '2026-03', purpose: '재료비', linkDone: true };
    const none = await call(args, EXECUTE);
    assert.equal(none.structuredContent.code, 'CONFLICT');
    assert.equal(none.structuredContent.data.previewChanged, true);
    await call(args);
    // A charge posted after the preview (the ledger runs 2-3 business days late).
    daou.state.rows.push({ purchaseId: 'late', useDate: '2026-03-29 10:00', bpName: '테스트상점', grossAmount: 45_000, status: '9' });
    const receipts = path.join(dataDir, 'files', 'hong', 'receipts');
    fs.writeFileSync(path.join(receipts, 'late.pdf'), '%PDF-1.4 late');
    db.addReceipt({ owner: 'hong', file: 'late.pdf', date: '2026-03-29', category: '재료비', item: '테스트-나사', vendor: '테스트상점', amount: 45_000, currency: '원' });
    try {
      const r = await call(args, EXECUTE);
      assert.equal(r.structuredContent.code, 'CONFLICT');
      assert.match(r.content[0].text, /미리보기 뒤에 내용이 바뀌었습니다/);
      assert.ok(!daou.state.calls.some((c) => c.path.includes('saveCompanyCard')), 'nothing saved');
      assert.equal(daou.state.created.length, 0);
    } finally {
      daou.state.rows.pop();
    }
  });

  test('execute saves purposes, creates, submits, verifies and records the filing', async () => {
    const r = await execute({ card: '1234', month: '2026-03', purpose: '재료비' });
    assert.ok(!r.isError, r.content[0].text);
    const saves = daou.state.calls.filter((c) => c.path === '/eacc/api/ess/myExp/saveCompanyCard');
    assert.equal(saves.length, 1);
    assert.deepEqual(saves[0].body.paramList.map((x) => [x.purchaseId, x.usePurpose, x.usePurposeDetail, x.referenceId]), [
      ['a', 'P1', '테스트 부품 구매', 'a'],
      ['c', 'P1', '테스트 센서 구매', 'c'],
    ]);
    const created = daou.state.created.at(-1);
    assert.deepEqual([created.cardId, created.purchaseIds], ['card-1', ['a', 'c']]);
    const sent = daou.state.submitted.at(-1);
    assert.equal(sent.title, '2026년 3월 (1234/rnd/홍길동) 법인카드 지출의 건 (재료비)');
    assert.deepEqual(sent.approvers, [{ name: '김대표', type: '결재' }]);
    assert.deepEqual(sent.links, [{ id: SENSOR_DOC, docNum: 'TEST-0001' }, { id: INSPECTION_DOC, docNum: 'TEST-0002' }]);
    assert.equal(sent.opinion, '');
    assert.equal(sent.files.length, 1);
    assert.ok(path.isAbsolute(sent.files[0]) && fs.existsSync(sent.files[0]));
    assert.match(r.content[0].text, /지출결의를 올렸습니다/);
    assert.equal(r.structuredContent.verified, true);
    const [f] = db.filings('hong', { kind: 'claim', ref: '1234/2026-03/재료비' });
    assert.deepEqual([f.docId, f.status, f.detail.verified, f.detail.rows.length], [created.docId, '진행중', true, 2]);
  });

  test('a second execute for the same month and purpose is refused by the filing record', async () => {
    const r = await execute({ card: '1234', month: '2026-03', purpose: '재료비' });
    assert.equal(r.structuredContent.code, 'CONFLICT');
    assert.equal(daou.state.created.length, 1);
  });

  test('a server document that differs from the plan is reported as a failure with its id', async () => {
    daou.state.attachOverride = 3;
    try {
      const r = await execute({ card: '1234', month: '2026-03', purpose: '차량유지비' });
      assert.equal(r.structuredContent.code, 'UPSTREAM');
      assert.match(r.content[0].text, /첨부 수가 다릅니다: 3\/0/);
      const docId = daou.state.created.at(-1).docId;
      assert.equal(r.structuredContent.data.docId, docId);
      const [f] = db.filings('hong', { kind: 'claim', ref: '1234/2026-03/차량유지비' });
      assert.deepEqual([f.docId, f.detail.verified], [docId, false]);
    } finally {
      daou.state.attachOverride = null;
    }
  });

  test('notes are saved after an approved execute and apply to the next claim', async () => {
    daou.state.rows.push(
      { purchaseId: 'g', useDate: '2026-03-25 13:00', bpName: '테스트카페', grossAmount: 5_000, status: '9' },
      { purchaseId: 'h', useDate: '2026-03-26 13:00', bpName: '테스트주차장', grossAmount: 3_000, status: '9' },
    );
    try {
      const notes = [
        { date: '2026-03-25', amount: 5_000, detail: '테스트 회의 다과', purpose: '회의비' },
        { date: '2026-03-26', amount: 3_000, exclude: true, reason: '개인 결제' },
      ];
      const r = await execute({ card: '1234', month: '2026-03', purpose: '회의비', notes });
      assert.ok(!r.isError, r.content[0].text);
      assert.equal(db.overrides('hong', CLAIM_NOTE_KIND).length, 2);
      const again = await call({ card: '1234', month: '2026-03', purpose: '회의비' });
      assert.equal(again.structuredContent.code, 'CONFLICT', 'the saved notes explain the rows; the purpose is already filed');
    } finally {
      daou.state.rows.splice(-2);
    }
  });

  // The failure paths use their own row and purpose (사무용품) so the claims filed above stay as they are.
  const officeRow = (id, date, amount) => ({ purchaseId: id, useDate: `${date} 10:00`, bpName: '테스트철물', grossAmount: amount, status: '9' });
  const officeNote = (date, amount) => [{ date, amount, detail: '테스트 공구 구매', purpose: '사무용품' }];

  test('a failure after the document was created is an unknown outcome, and the next preview names the draft', async () => {
    const snap = structuredClone(daou.state.rows);
    daou.state.rows.push(officeRow('k1', '2026-03-27', 22_000));
    daou.state.failAfterCreate = true;
    try {
      const r = await execute({ card: '1234', month: '2026-03', purpose: '사무용품', notes: officeNote('2026-03-27', 22_000) });
      assert.equal(r.structuredContent.code, 'UPSTREAM');
      assert.equal(r.structuredContent.data.afterWrite, true);
      assert.match(r.content[0].text, /도중 멈췄습니다.*임시저장 문서를 확인/);
      assert.ok(db.filings('hong', { kind: 'claim', ref: '1234/2026-03/사무용품' }).some((f) => f.status === '생성 중 멈춤'));
      const { docId } = daou.state.created.at(-1);
      daou.state.failAfterCreate = false;
      const again = await call({ card: '1234', month: '2026-03', purpose: '사무용품', notes: officeNote('2026-03-27', 22_000) });
      assert.equal(again.structuredContent.code, 'CONFLICT');
      assert.match(again.content[0].text, new RegExp(`임시저장 문서에서 멈췄습니다: .*문서 ID ${docId}`));
      delete daou.state.docs[docId];
    } finally {
      daou.state.failAfterCreate = false;
      daou.state.rows = snap;
    }
  });

  test('a failure before the final click says it was not submitted; after the click, that the outcome is unknown', async () => {
    const snap = structuredClone(daou.state.rows);
    daou.state.rows.push(officeRow('k2', '2026-03-28', 15_000));
    try {
      daou.state.failBeforeSend = true;
      const before = await execute({ card: '1234', month: '2026-03', purpose: '사무용품', notes: officeNote('2026-03-28', 15_000) });
      assert.equal(before.structuredContent.code, 'UPSTREAM');
      assert.match(before.content[0].text, /만들었지만 상신하지 못했습니다/);
      const first = daou.state.created.at(-1).docId;
      assert.equal(db.filings('hong', { kind: 'claim', ref: '1234/2026-03/사무용품' }).find((f) => f.docId === first).status, '상신 실패');
      delete daou.state.docs[first];
      daou.state.rows = structuredClone(snap);
      daou.state.rows.push(officeRow('k2', '2026-03-28', 15_000));

      daou.state.failBeforeSend = false;
      daou.state.failAfterSend = true;
      const after = await execute({ card: '1234', month: '2026-03', purpose: '사무용품', notes: officeNote('2026-03-28', 15_000) });
      assert.equal(after.structuredContent.code, 'UPSTREAM');
      assert.equal(after.structuredContent.data.afterWrite, true);
      assert.match(after.content[0].text, /결재요청을 누른 뒤 결과를 확인하지 못했습니다/);
      const second = daou.state.created.at(-1).docId;
      assert.equal(db.filings('hong', { kind: 'claim', ref: '1234/2026-03/사무용품' }).find((f) => f.docId === second).status, '확인 못 함');
      delete daou.state.docs[second];
    } finally {
      daou.state.failBeforeSend = false;
      daou.state.failAfterSend = false;
      daou.state.rows = snap;
    }
  });
});
