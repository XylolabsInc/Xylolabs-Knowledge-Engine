import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { parseConfig } from '../src/config.mjs';
import { openDb } from '../src/db.mjs';
import { describeLine } from '../src/dao/approvers.mjs';
import {
  formValues, validatePurchase, purchaseApprovers, purchaseRules, totalQuantity, splitVat, withWeekday,
  fieldTable, purchaseEntry, purchaseRow, amountKrw, RECEIPT_LIKE,
} from '../src/dao/purchases.mjs';
import { parseFxText, toKrw, paidNote, fxNote, todayRate } from '../src/dao/fx.mjs';
import {
  detectBlock, missingExpect, quoteFileName, checkQuoteFiles, storeQuote, alternativeHint, sha256,
} from '../src/dao/quotes.mjs';
import { diffPurchaseDoc, diffLine, amountOf } from '../src/dao/purchase-doc.mjs';
import { compareForm, planSubmission } from '../src/dao/purchase-submit.mjs';
import { mergeDraft, applyCardDefaults } from '../src/tools/purchase.mjs';

// Made-up company: names, projects and cards are placeholders.
const cfg = parseConfig({
  people: { ceo: '김대표', cfo: '이재무', cto: '박기술' },
  cfoFrom: 1_000_000,
  projects: {
    스마트농장: { reviewer: '최담당', subProjects: ['온실', '드론'] },
    해양로봇: { reviewer: '정담당' },
  },
  budgets: ['운영자금', '프로젝트 사업비'],
  cards: {
    1234: { project: '스마트농장', budget: '프로젝트 사업비', issuer: '신한' },
    5678: { project: '해양로봇', budget: '프로젝트 사업비', issuer: 'KB' },
  },
  lines: { purchase: 'project' },
  minPurchaseKrw: 100_000,
});

const FX = { rate: 1400, at: '2026-01-15 11:00', source: '하나은행 매매기준율 (고시 100회차)' };

/** A filed-ready domestic card purchase. Every test changes one thing. */
const base = (over = {}) => ({
  key: 'test-camera',
  title: '테스트 카메라 구매의 건',
  budget: '프로젝트 사업비',
  project: '스마트농장',
  subProject: '온실',
  item: '테스트 카메라 / 1개',
  vendor: '테스트상점',
  purpose: '온실 작물 생육 원격 관찰',
  paidAt: '2026-01-15',
  amount: 132_000,
  payWhen: '당월',
  payMethod: '법인카드',
  cardIssuer: '신한',
  cardLast4: '1234',
  note: ['https://shop.example.com/camera', '본체 132,000원, 배송비 없음'],
  quotes: [{ file: 'shop_camera-1a2b3c4d.png', url: 'https://shop.example.com/camera' }],
  ...over,
});

const errorsOf = (p, opts = {}) => validatePurchase(p, { config: cfg, ...opts }).errors;
const warningsOf = (p, opts = {}) => validatePurchase(p, { config: cfg, ...opts }).warnings;

describe('formValues', () => {
  test('domestic card purchase: VAT 10/110, weekday, 해당없음, quantity', () => {
    const v = formValues(base(), { config: cfg });
    assert.equal(v.amount, '132,000');
    assert.equal(v.supply, '120,000');
    assert.equal(v.vat, '12,000');
    assert.equal(v.purchaseDate, '2026-01-15(목)');
    assert.equal(v.account, '해당없음');
    assert.equal(v.accountHolder, '해당없음');
    assert.equal(v.cardLast4, '1234');
    assert.equal(v.quantity, '1개');
    assert.equal(v.project, '스마트농장');
    assert.equal(v.subProject, '온실');
    assert.deepEqual(v.attachments, ['shop_camera-1a2b3c4d.png']);
    assert.equal(v.note, 'https://shop.example.com/camera\n본체 132,000원, 배송비 없음');
  });

  test('foreign, not yet paid: converted at the pinned rate, VAT 0, conversion line in 기타', () => {
    const v = formValues(base({ amount: undefined, usd: 100.5, fx: FX, paidAt: undefined, purchaseDate: '2026-01-20' }), { config: cfg });
    assert.equal(v.amountNumber, 140_700);
    assert.equal(v.vat, '0');
    assert.equal(v.purchaseDate, '2026-01-20(화)');
    assert.match(v.note.split('\n').at(-1), /^USD 100\.50 × 1,400\.00원 \(하나은행 매매기준율 \(고시 100회차\), 2026-01-15 11:00\) = 140,700원$/);
  });

  test('foreign, paid by card: the card charge as is, implied rate in 기타', () => {
    const v = formValues(base({ amount: undefined, usd: 20, paidKrw: 27_500, fx: FX }), { config: cfg });
    assert.equal(v.amountNumber, 27_500);
    assert.equal(v.note.split('\n').at(-1), 'USD 20.00 × 1,375.0원 (2026-01-15 카드 승인 적용환율) = 27,500원');
  });

  test('non-project budget clears project and sub-project; account payment keeps the account', () => {
    const v = formValues(base({ budget: '운영자금', payMethod: '세금계산서', account: '테스트은행 000-00-000000', accountHolder: '테스트상점' }), { config: cfg });
    assert.equal(v.project, '');
    assert.equal(v.subProject, '');
    assert.equal(v.cardLast4, '');
    assert.equal(v.account, '테스트은행 000-00-000000');
  });

  test('sub-project only for projects that have sub-projects', () => {
    assert.equal(formValues(base({ project: '해양로봇', subProject: '온실' }), { config: cfg }).subProject, '');
  });

  test('quantity: sums parts, "총 N개" wins, qty overrides, goods units mix into 개, others do not', () => {
    const cases = [
      ['카메라 / 1개, 렌즈 / 2개', '3개'],
      ['케이블 외 2종 / 총 5개', '5개'],
      ['카메라 / 1대, 렌즈 / 2개', '3개'],
      ['테스트 구독 / 1개월', '1개월'],
      ['테스트 서비스 / 3계정', '3계정'],
      ['테스트 장갑 / 2개입', '2개입'],
      ['테스트 충전기 6개입 / 1세트', '1세트'],
      ['테스트 구독 / 1개월, 테스트 카메라 / 1개', ''],
      ['테스트 크레딧 / USD 50.00', ''],
    ];
    for (const [item, want] of cases) assert.equal(totalQuantity({ item }), want, item);
    assert.equal(totalQuantity({ item: '카메라 / 1개', qty: '8개' }), '8개');
  });

  test('VAT override and weekday helper', () => {
    assert.deepEqual(splitVat({ vat: 12_340 }, 135_740), { supply: 123_400, vat: 12_340 });
    assert.equal(withWeekday('2026-02-01'), '2026-02-01(일)');
    assert.equal(withWeekday('해당없음'), '해당없음');
  });
});

describe('validatePurchase', () => {
  test('a complete entry passes', () => {
    assert.deepEqual(errorsOf(base()), []);
  });

  test('title must end like the document type', () => {
    for (const title of ['테스트 구독 구독의 건', '테스트 도메인 갱신의 건', '테스트 클라우드 사용료 지출의 건', '테스트 서비스 구독의 건 (2026-01-15 결제분)']) {
      assert.deepEqual(errorsOf(base({ title })).filter((e) => e.startsWith('제목은')), [], title);
    }
    assert.ok(errorsOf(base({ title: '테스트 카메라 구매' })).some((e) => e.startsWith('제목은')));
  });

  test('item needs a count with a known unit', () => {
    assert.ok(errorsOf(base({ item: '테스트 카메라' })).some((e) => /수량이 없다/.test(e)));
    assert.ok(errorsOf(base({ item: '테스트 장갑 100개입 / 1봉' })).some((e) => /수량이 없다/.test(e)));
    // A credit counts in its foreign amount; the 구입수량 cell then needs qty from a person.
    assert.ok(errorsOf(base({ item: '테스트 크레딧 / USD 50.00' })).some((e) => /구입수량을 품명에서 셀 수 없다/.test(e)));
    assert.deepEqual(errorsOf(base({ item: '테스트 크레딧 / USD 50.00', qty: '1건' })).filter((e) => /수량/.test(e)), []);
    assert.ok(!errorsOf(base({ item: '테스트 크레딧 / USD 50.00', docId: '1000000000000001' })).some((e) => /구입수량/.test(e)), 'old-form documents have no 구입수량 cell');
  });

  test('money: VAT over the total, and an amount that rounds to 0원, are refused', () => {
    assert.ok(errorsOf(base({ vat: 140_000 })).some((e) => /부가세/.test(e)));
    assert.deepEqual(errorsOf(base({ vat: 12_000 })), []);
    const tiny = base({ amount: undefined, usd: 0.0001, fx: FX, paidAt: undefined, purchaseDate: '2026-01-20' });
    assert.ok(errorsOf(tiny).some((e) => /0원이다/.test(e)));
  });

  test('purpose that repeats the item is refused; target and use passes', () => {
    const p = base({ item: '테스트 전자저울 / 1개', purpose: '전자저울 구매' });
    assert.ok(errorsOf(p).some((e) => /품명을 되풀이/.test(e)));
    assert.ok(!errorsOf({ ...p, purpose: '실험실 시료 무게 측정' }).some((e) => /품명을 되풀이/.test(e)));
  });

  test('empty required fields are listed by Korean label and stop the rest', () => {
    const errs = errorsOf(base({ purpose: '', vendor: ' ' }));
    assert.deepEqual(errs, ['업체명 칸이 비어 있다 (vendor)', '구입목적 칸이 비어 있다 (purpose)']);
  });

  test('sub-project required for a new document; a filed (old-form) document is spared', () => {
    assert.ok(errorsOf(base({ subProject: undefined })).some((e) => /세부 과제/.test(e)));
    assert.ok(errorsOf(base({ subProject: '없는세부' })).some((e) => /세부 과제/.test(e)));
    assert.ok(!errorsOf(base({ subProject: undefined, docId: '1000000000000001' })).some((e) => /세부 과제/.test(e)));
  });

  test('purchase date required for a new document', () => {
    assert.ok(errorsOf(base({ paidAt: undefined })).some((e) => /구입일자가 없다/.test(e)));
    assert.ok(!errorsOf(base({ paidAt: undefined, purchaseDate: '2026-01-20' })).some((e) => /구입일자/.test(e)));
    assert.ok(errorsOf(base({ paidAt: '2026/01/15' })).some((e) => /paidAt 는 YYYY-MM-DD/.test(e)));
  });

  test('project must be configured; no projects in config is reported, not guessed', () => {
    assert.ok(errorsOf(base({ project: '없는과제' })).some((e) => /프로젝트가 선택지에 없다/.test(e)));
    const empty = parseConfig({});
    assert.ok(validatePurchase(base(), { config: empty }).errors.some((e) => /과제 목록\(projects\)이 없다/.test(e)));
  });

  test('exactly one of amount and usd', () => {
    assert.ok(errorsOf(base({ usd: 10, fx: FX })).some((e) => /정확히 하나/.test(e)));
    assert.ok(errorsOf(base({ amount: undefined })).some((e) => /정확히 하나/.test(e)));
  });

  test('paid foreign entry: paidKrw needs usd and paidAt, implied rate in range', () => {
    const paid = base({ amount: undefined, usd: 20, paidKrw: 27_500 });
    assert.deepEqual(errorsOf(paid), []);
    assert.ok(errorsOf({ ...paid, paidKrw: 2_750 }).some((e) => /적용환율이 정상 범위/.test(e)));
    assert.ok(errorsOf({ ...paid, paidAt: undefined, purchaseDate: '2026-01-15' }).some((e) => /paidAt/.test(e)));
    assert.ok(errorsOf(base({ paidKrw: 27_500 })).some((e) => /usd 와 함께/.test(e)));
  });

  test('unpaid foreign entry needs a pinned rate; paid by card without the charge warns', () => {
    const unpaid = base({ amount: undefined, usd: 120, paidAt: undefined, purchaseDate: '2026-01-20' });
    assert.ok(errorsOf(unpaid).some((e) => /환율이 없다/.test(e)));
    assert.deepEqual(errorsOf({ ...unpaid, fx: FX }), []);
    const charged = base({ amount: undefined, usd: 120, fx: FX });
    assert.ok(warningsOf(charged).some((w) => /paidKrw/.test(w)));
  });

  test('amounts under the configured minimum warn but do not block', () => {
    const small = base({ amount: 21_000 });
    assert.deepEqual(errorsOf(small), []);
    assert.ok(warningsOf(small).some((w) => /100,000원 미만/.test(w)));
    assert.ok(!warningsOf(base()).some((w) => /미만/.test(w)));
    const lower = parseConfig({ ...structuredClone(cfg), minPurchaseKrw: 10_000 });
    assert.ok(!validatePurchase(small, { config: lower }).warnings.some((w) => /미만/.test(w)));
  });

  test('payment fields follow the form options', () => {
    assert.ok(errorsOf(base({ payWhen: '다음달' })).some((e) => /지급요청일/.test(e)));
    assert.ok(errorsOf(base({ cardIssuer: '없는카드' })).some((e) => /카드사/.test(e)));
    assert.ok(errorsOf(base({ cardLast4: '12' })).some((e) => /네 자리/.test(e)));
    assert.ok(errorsOf(base({ payMethod: '세금계산서' })).some((e) => /지급계좌와 예금주/.test(e)));
  });

  test('writing checks on purpose, 기타 and title; 기타 lines are items, not sentences', () => {
    assert.ok(errorsOf(base({ purpose: '온실 작물 관찰·기록' })).some((e) => /가운뎃점/.test(e)));
    assert.ok(errorsOf(base({ note: ['https://shop.example.com/camera', '두 건을 한 번에 결제했다'] })).some((e) => /문장형/.test(e)));
    assert.ok(errorsOf(base({ title: '테스트 카메라를 활용한 구매의 건' })).some((e) => /번역체/.test(e)));
  });

  test('quotes are mandatory, never receipt-like, and need a URL unless a person took them', () => {
    assert.ok(errorsOf(base({ quotes: [] })).some((e) => /캡처가 없다/.test(e)));
    assert.ok(errorsOf(base({ quotes: [{ file: '영수증_카메라.png', url: 'https://x.example' }] })).some((e) => /영수증과 주문내역/.test(e)));
    assert.ok(errorsOf(base({ quotes: [{ file: 'order_detail.png', url: 'https://x.example' }] })).some((e) => /영수증과 주문내역/.test(e)));
    for (const file of ['receipt.png', 'Invoice-12.pdf', 'my-orders.png', 'orderdetail.png']) assert.match(file, RECEIPT_LIKE, file);
    for (const file of ['recorder.png', 'border-box.png', 'reorder-shelf.png', 'shop.png']) assert.doesNotMatch(file, RECEIPT_LIKE, file);
    assert.ok(errorsOf(base({ quotes: [{ file: 'shop.png' }] })).some((e) => /주소가 없다/.test(e)));
    assert.deepEqual(errorsOf(base({ quotes: [{ file: 'shop.jpg', manual: true }] })), []);
    assert.ok(warningsOf(base({ note: ['본체 132,000원'] })).some((w) => /상품 링크/.test(w)));
  });
});

describe('approval line and review table', () => {
  test('project line with the CFO from the threshold', () => {
    assert.equal(describeLine(purchaseApprovers(cfg, base())), '합의 최담당 → 결재 김대표');
    assert.equal(describeLine(purchaseApprovers(cfg, base({ amount: 1_200_000 }))), '합의 최담당 → 합의 이재무 → 결재 김대표');
  });

  test('a non-project budget has no rule under the project line: stop and ask', () => {
    assert.throws(() => purchaseApprovers(cfg, base({ budget: '운영자금' })), (e) => e.code === 'VALIDATION' && /사람에게 묻고/.test(e.message));
    const listed = parseConfig({ ...structuredClone(cfg), lines: { purchase: [{ role: 'ceo', type: '결재' }] } });
    assert.equal(describeLine(purchaseApprovers(listed, base({ budget: '운영자금' }))), '결재 김대표');
  });

  test('review table shows every field', () => {
    const text = fieldTable(formValues(base(), { config: cfg })).join('\n');
    for (const s of ['테스트 카메라 구매의 건', '프로젝트 사업비 / 스마트농장 / 온실', '132,000원 (공급가액 120,000원, 부가세액 12,000원)', '당월, 법인카드, 신한, 1234', 'https://shop.example.com/camera', 'shop_camera-1a2b3c4d.png']) {
      assert.ok(text.includes(s), s);
    }
  });

  test('rules come from config with form defaults', () => {
    const r = purchaseRules(cfg);
    assert.deepEqual(r.projects, ['스마트농장', '해양로봇']);
    assert.deepEqual(r.subProjectsOf('스마트농장'), ['온실', '드론']);
    assert.deepEqual(r.payWhen, ['당월', '익월']);
    assert.equal(r.minPurchaseKrw, 100_000);
  });
});

describe('DB rows', () => {
  test('entry → row → entry keeps every field; amountKrw is the requisition total', () => {
    const db = openDb(':memory:');
    const entry = base({ amount: undefined, usd: 20, paidKrw: 27_500, serials: { '테스트 카메라': ['SN-0001'] }, revision: 3, status: 'draft' });
    const { quotes, ...rest } = entry;
    const row = db.upsertPurchase(purchaseRow('hong', rest));
    assert.equal(row.amountKrw, 27_500);
    assert.equal(row.paidKrw, 27_500);
    assert.equal(row.usd, 20);
    assert.equal(row.title, entry.title);
    assert.equal(row.data.key, undefined);
    const back = purchaseEntry(db.purchase('hong', 'test-camera'), [{ file: 'a.png', url: 'https://x.example', digest: 'd', data: {} }]);
    assert.deepEqual(back.serials, { '테스트 카메라': ['SN-0001'] });
    assert.equal(back.revision, 3);
    assert.deepEqual(back.quotes, [{ file: 'a.png', url: 'https://x.example', manual: false, digest: 'd' }]);
    assert.deepEqual(formValues(back, { config: cfg }), formValues({ ...entry, quotes: back.quotes }, { config: cfg }));
    db.close();
  });

  test('amountKrw without a rate for unpaid foreign is NaN, stored as null', () => {
    assert.ok(Number.isNaN(amountKrw({ usd: 10 })));
    assert.equal(purchaseRow('hong', { key: 'k', usd: 10 }).amountKrw, null);
  });
});

describe('draft merging', () => {
  test('only given fields change, null clears, strings are trimmed, serials map', () => {
    const prev = { title: '옛 제목 구매의 건', usd: 10, vendor: '테스트상점' };
    const next = mergeDraft(prev, { title: ' 새 제목 구매의 건 ', usd: null, amount: 5000, serials: [{ item: '카메라', numbers: [' SN-1 ', ''] }] });
    assert.deepEqual(next, { title: '새 제목 구매의 건', vendor: '테스트상점', amount: 5000, serials: { 카메라: ['SN-1'] } });
    assert.equal(mergeDraft(prev, { noSerials: true }).serials, 'none');
  });

  test('card last four fill budget, project, issuer, method and 당월 without overriding given values', () => {
    assert.deepEqual(applyCardDefaults({ cardLast4: '1234' }, cfg), {
      cardLast4: '1234', payMethod: '법인카드', budget: '프로젝트 사업비', project: '스마트농장', cardIssuer: '신한', payWhen: '당월',
    });
    assert.equal(applyCardDefaults({ cardLast4: '1234', project: '해양로봇', payWhen: '익월' }, cfg).project, '해양로봇');
    assert.equal(applyCardDefaults({ cardLast4: '1234', payWhen: '익월' }, cfg).payWhen, '익월');
    assert.deepEqual(applyCardDefaults({ amount: 1 }, cfg), { amount: 1 });
  });

  test('changing the card replaces what the old card filled in, but not what a person set', () => {
    const previous = applyCardDefaults({ cardLast4: '1234' }, cfg);
    const switched = applyCardDefaults({ ...previous, cardLast4: '5678' }, cfg, { previous });
    assert.deepEqual([switched.project, switched.cardIssuer, switched.budget], ['해양로봇', 'KB', '프로젝트 사업비']);
    const chosen = { ...previous, project: '스마트농장 시험' };
    const kept = applyCardDefaults({ ...chosen, cardLast4: '5678' }, cfg, { previous: chosen });
    assert.equal(kept.project, '스마트농장 시험');
    assert.equal(kept.cardIssuer, 'KB');
  });
});

describe('fx', () => {
  const PAGE = '하나은행 고시회차 100회 기준 … 매매기준율 기준 미국 USD 1 달러 대한민국 KRW 1,385.20 원 … 2026. 01. 15. 11:00 기준';

  test('parses the Naver finance text; anything else is null', () => {
    assert.deepEqual(parseFxText(PAGE), { rate: 1385.2, at: '2026-01-15 11:00', source: '하나은행 매매기준율 (고시 100회차)' });
    assert.equal(parseFxText('점검 중입니다'), null);
  });

  test('integer cent arithmetic and note lines', () => {
    assert.equal(toKrw(123.45, 1388.8), 171_447);
    assert.equal(toKrw(0.1, 1000), 100);
    assert.equal(paidNote({ usd: 30, krw: 41_250, at: '2026-01-15' }), 'USD 30.00 × 1,375.0원 (2026-01-15 카드 승인 적용환율) = 41,250원');
    assert.equal(fxNote({ usd: 10, fx: FX }), 'USD 10.00 × 1,400.00원 (하나은행 매매기준율 (고시 100회차), 2026-01-15 11:00) = 14,000원');
  });

  test('todayRate caches per KST day and fetches once', async () => {
    const db = openDb(':memory:');
    let fetched = 0;
    const fetch = async () => { fetched++; return { rate: 1390, at: '2026-01-15 09:00', source: '하나은행 매매기준율' }; };
    const now = new Date('2026-01-15T01:00:00Z');
    assert.equal((await todayRate({ db, fetch, now })).rate, 1390);
    assert.equal((await todayRate({ db, fetch, now })).rate, 1390);
    assert.equal(fetched, 1);
    assert.equal(db.fxRate('2026-01-15').rate, 1390);
    await todayRate({ db, fetch, now: new Date('2026-01-15T16:00:00Z') });   // next day in KST
    assert.equal(fetched, 2);
    const sessions = { withScratchContext: async (fn) => fn({ page: { goto: async () => {}, waitForFunction: async () => {}, evaluate: async () => PAGE } }) };
    assert.equal((await todayRate({ db, sessions, now: new Date('2026-01-20T01:00:00Z') })).rate, 1385.2);
    assert.equal(db.latestFxRate().day, '2026-01-20');
    db.close();
  });
});

describe('quote checks', () => {
  test('detectBlock: receipts, login walls, captcha, blocks and missing items', () => {
    const cases = [
      [{ url: 'https://shop.example.com/my/orders/123' }, 'receipt'],
      [{ url: 'https://shop.example.com/p/1', title: '주문 상세' }, 'receipt'],
      [{ url: 'https://shop.example.com/p/1?orderId=9' }, 'receipt'],
      [{ url: 'https://smartstore.naver.com/x/products/1', landedUrl: 'https://nid.naver.com/nidlogin.login' }, 'login'],
      [{ url: 'https://shop.example.com/p/1', title: '로그인', text: '아이디 비밀번호' }, 'login'],
      [{ url: 'https://shop.example.com/p/1', text: '로그인이 필요합니다' }, 'login'],
      [{ url: 'https://shop.example.com/p/1', text: 'Please complete the security verification' }, 'captcha'],
      [{ url: 'https://shop.example.com/p/1', landedUrl: 'https://shop.example.com/captcha?x=1' }, 'captcha'],
      [{ url: 'https://www.coupang.com/vp/products/1', text: 'Access Denied' }, 'blocked'],
      [{ url: 'https://shop.example.com/p/1', status: 403 }, 'blocked'],
      [{ url: 'https://shop.example.com/p/1', status: 404 }, 'missing'],
      [{ url: 'https://shop.example.com/p/1', text: 'Sorry, this item is no longer available' }, 'missing'],
    ];
    for (const [input, kind] of cases) assert.equal(detectBlock(input)?.kind, kind, JSON.stringify(input));
  });

  test('detectBlock: an ordinary product page passes, even with login links and a reCAPTCHA footer', () => {
    const text = `테스트 카메라 132,000원 장바구니 로그인 회원가입 로그인 후 확인 가능한 쿠폰 ${'상품 설명 '.repeat(800)} This site is protected by reCAPTCHA`;
    assert.equal(detectBlock({ url: 'https://shop.example.com/p/1', title: '테스트 카메라 : 테스트상점', text, status: 200 }), null);
  });

  test('blocked shops get an alternative that worked before', () => {
    assert.match(alternativeHint('https://www.coupang.com/vp/products/1'), /다나와/);
    assert.match(alternativeHint('https://smartstore.naver.com/x/products/1'), /제조사/);
    assert.match(detectBlock({ url: 'https://www.coupang.com/vp/products/1', status: 403 }).reason, /다나와/);
  });

  test('expect texts are case-insensitive and all required', () => {
    assert.equal(missingExpect('Test Camera by 테스트상점', ['test camera', '테스트상점']), null);
    assert.equal(missingExpect('Test Camera', ['Test Camera', '다른상점']), '다른상점');
  });

  test('quote file names: label or host, hash suffix, receipt-like names refused', () => {
    assert.equal(quoteFileName({ label: '테스트상점 카메라', digest: 'abcdef0123', ext: 'png' }), '테스트상점_카메라-abcdef01.png');
    assert.equal(quoteFileName({ url: 'https://www.shop.example.com/p/1', digest: '12345678ff' }), 'shop.example.com-12345678.png');
    assert.throws(() => quoteFileName({ label: '카메라 영수증', digest: 'aa' }), (e) => e.code === 'VALIDATION');
    assert.throws(() => quoteFileName({ label: 'invoice', digest: 'aa' }), (e) => e.code === 'VALIDATION');
  });

  test('stored quotes are found unchanged; a changed or missing file is reported', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'quotes-'));
    const db = openDb(':memory:');
    const p = db.upsertPurchase({ owner: 'hong', key: 'k', data: {} });
    const q = storeQuote({ db, owner: 'hong', dir, purchaseId: p.id, buffer: Buffer.from('png-bytes'), label: '테스트', url: 'https://x.example', ext: 'png' });
    assert.equal(q.digest, sha256(Buffer.from('png-bytes')));
    assert.deepEqual(checkQuoteFiles(dir, [q]), []);
    assert.deepEqual(checkQuoteFiles(dir, [q], { previewed: { [q.file]: q.digest } }), []);
    assert.match(checkQuoteFiles(dir, [q], { previewed: {} })[0], /미리보기 뒤에/);
    fs.writeFileSync(path.join(dir, q.file), 'other');
    assert.match(checkQuoteFiles(dir, [q])[0], /기록과 다릅니다/);
    fs.rmSync(path.join(dir, q.file));
    assert.match(checkQuoteFiles(dir, [q])[0], /없습니다/);
    db.close();
    fs.rmSync(dir, { recursive: true, force: true });
  });
});

describe('document comparison', () => {
  const want = formValues(base(), { config: cfg });
  const t = (value) => ({ type: 'text', value });
  const r = (value) => ({ type: 'radio', value });
  const s = (value) => ({ type: 'select', value });
  const newFormDoc = (over = {}) => ({
    title: want.title,
    status: '진행중',
    attachments: [...want.attachments],
    line: [{ name: '홍길동' }, { name: '최담당' }, { name: '김대표' }],
    fields: {
      예산: [r('프로젝트 사업비'), s('스마트농장'), r('온실')],
      구매품명: [t(want.item)], 업체명: [t(want.vendor)], 구입수량: [t('1개')], 구입일자: [t(want.purchaseDate)],
      배송일자: null, 구입목적: [t(want.purpose)],
      '총금액(vat포함)': [t('132,000'), t('120,000'), t('12,000')], '금액(vat포함)': null,
      지급요청일: [r('당월')], 지급방법: [s('법인카드'), t('1234'), s('신한')],
      '지급계좌(은행)': [t('해당없음')], 예금주: [t('해당없음')], '기타(링크)': [t(want.note)],
      ...over,
    },
  });

  test('a matching new-form document has no differences', () => {
    assert.deepEqual(diffPurchaseDoc(newFormDoc(), want), []);
    assert.equal(amountOf(newFormDoc().fields), '132,000');
  });

  test('every field is compared, including the split amount and attachments', () => {
    const doc = newFormDoc({ '총금액(vat포함)': [t('132,000'), t('120,001'), t('12,000')] });
    doc.title = '구매 품의서';
    doc.attachments.push('old_capture.png');
    const labels = diffPurchaseDoc(doc, want).map((d) => d.label);
    assert.deepEqual(labels, ['제목', '총금액 공급가액', '첨부']);
  });

  test('old-form documents are read by their own fields (배송일자, 금액)', () => {
    const old = newFormDoc({ 구입수량: undefined, 구입일자: undefined, 배송일자: [t('2026-01-16(금)')], '총금액(vat포함)': undefined, '금액(vat포함)': [t('132,000')] });
    delete old.fields.구입수량;
    delete old.fields.구입일자;
    delete old.fields['총금액(vat포함)'];
    const labels = diffPurchaseDoc(old, { ...want, deliveryDate: '2026-01-16(금)' }).map((d) => d.label);
    assert.deepEqual(labels, []);
    assert.equal(amountOf(old.fields), '132,000');
  });

  test('approval line after the drafter must match names and order', () => {
    const approvers = [{ name: '최담당', type: '합의' }, { name: '김대표', type: '결재' }];
    assert.equal(diffLine(newFormDoc(), approvers), null);
    assert.equal(diffLine(newFormDoc(), [...approvers].reverse()).label, '결재선');
  });

  test('screen comparison catches stale and missing attachments', () => {
    const got = {
      ...want, newForm: true, filesText: 'shop_camera-1a2b3c4d.png old.png', fileNames: ['shop_camera-1a2b3c4d.png', 'old.png'],
    };
    assert.deepEqual(compareForm(got, want).map((d) => `${d.label}:${d.got}`), ['첨부:old.png']);
    const missing = { ...got, filesText: '', fileNames: [] };
    assert.deepEqual(compareForm(missing, want).map((d) => d.want), ['shop_camera-1a2b3c4d.png']);
    const oldForm = { ...got, newForm: false, deliveryDate: '', fileNames: ['shop_camera-1a2b3c4d.png'] };
    assert.deepEqual(compareForm(oldForm, { ...want, deliveryDate: '2026-01-16(금)' }).map((d) => d.label), ['deliveryDate']);
  });
});

describe('planSubmission (reads only)', () => {
  const approvers = [{ name: '최담당', type: '합의' }, { name: '김대표', type: '결재' }];
  const OLD = '1000000000000001';

  /** api answers the document; page.evaluate returns the body fields of that document. */
  function fakeDocs(docs) {
    const api = async (p) => {
      const id = p.match(/document\/(\d+)$/)?.[1];
      const d = docs[id];
      if (!d) return { status: 404, ok: false, data: null };
      return {
        status: 200, ok: true,
        data: { data: {
          document: { title: d.title ?? '테스트 카메라 구매의 건', formName: d.formName ?? '구매 품의서', docStatusName: d.status, docNum: d.docNum ?? '', docBodyContent: id, attaches: (d.attachments ?? []).map((name) => ({ name })) },
          apprFlow: { activityGroups: [{ name: '승인', activities: (d.line ?? ['홍길동', '최담당']).map((userName) => ({ userName })) }] },
          actionCheck: { isAllowCancelDraftUser: Boolean(d.canCancel) },
        } },
      };
    };
    const page = { evaluate: async (fn, { html }) => (docs[html]?.oldForm ? { 배송일자: [] } : { 구입일자: [] }) };
    return { api, page };
  }
  const plan = (docs, entry, option) => {
    const { api, page } = fakeDocs(docs);
    return planSubmission({ page, api, entry, option, formName: '구매 품의서', approvers });
  };

  test('new document when nothing was filed', async () => {
    const p = await plan({}, { key: 'k' }, null);
    assert.equal(p.editId, null);
    assert.deepEqual(p.steps, ['새 문서로 기안합니다.']);
  });

  test('refile: in progress and cancellable → withdraw and resubmit the same document', async () => {
    const p = await plan({ [OLD]: { status: '진행중', canCancel: true } }, { key: 'k', docId: OLD }, 'refile');
    assert.equal(p.editId, OLD);
    assert.equal(p.withdraw, OLD);
    assert.equal(p.refileFrom, OLD);
  });

  test('refile: a draft left by an earlier run is continued', async () => {
    const p = await plan({ [OLD]: { status: '임시저장' } }, { key: 'k', docId: OLD }, 'refile');
    assert.equal(p.editId, OLD);
    assert.equal(p.withdraw, null);
  });

  test('refile: completed → new document linked to the old one; old form is detected', async () => {
    const p = await plan({ [OLD]: { status: '완료', docNum: '테스트-2026-0001' } }, { key: 'k', docId: OLD }, 'refile');
    assert.equal(p.editId, null);
    assert.deepEqual(p.links, [{ id: OLD, docNum: '테스트-2026-0001' }]);
    const q = await plan({ [OLD]: { status: '임시저장', oldForm: true } }, { key: 'k', docId: OLD }, 'refile');
    assert.equal(q.newForm, false);
  });

  test('refile refuses when the old document already has the final approver, or was already refiled', async () => {
    await assert.rejects(plan({ [OLD]: { status: '진행중', canCancel: true, line: ['홍길동', '최담당', '김대표'] } }, { key: 'k', docId: OLD }, 'refile'), (e) => e.code === 'CONFLICT');
    await assert.rejects(plan({}, { key: 'k', docId: '1000000000000002', refiledFrom: OLD }, 'refile'), (e) => e.code === 'CONFLICT' && /이미 다시 올린/.test(e.message));
    await assert.rejects(plan({}, { key: 'k' }, 'refile'), (e) => e.code === 'CONFLICT');
  });

  test('fix: in progress → withdraw first; already handled by an approver → stop', async () => {
    const p = await plan({ [OLD]: { status: '진행중', canCancel: true } }, { key: 'k', docId: OLD }, 'fix');
    assert.equal(p.withdraw, OLD);
    await assert.rejects(plan({ [OLD]: { status: '진행중', canCancel: false } }, { key: 'k', docId: OLD }, 'fix'), (e) => e.code === 'CONFLICT');
    await assert.rejects(plan({ [OLD]: { status: '완료' } }, { key: 'k', docId: OLD }, 'fix'), (e) => e.code === 'CONFLICT');
    await assert.rejects(plan({}, { key: 'k' }, 'fix'), (e) => e.code === 'VALIDATION');
  });

  test('editing an existing draft checks the form and status', async () => {
    await assert.rejects(plan({ [OLD]: { status: '임시저장', formName: '지출결의서' } }, { key: 'k', docId: OLD }, null), (e) => e.code === 'VALIDATION');
    await assert.rejects(plan({ [OLD]: { status: '진행중' } }, { key: 'k', docId: OLD }, null), (e) => e.code === 'CONFLICT');
    const p = await plan({ [OLD]: { status: '임시저장', attachments: ['old.png'] } }, { key: 'k', docId: OLD }, null);
    assert.deepEqual(p.existing, ['old.png']);
  });
});
