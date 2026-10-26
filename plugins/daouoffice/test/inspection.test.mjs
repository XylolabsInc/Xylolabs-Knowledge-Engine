import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import {
  itemLines, unitCount, parseLine, reportRows, splitVat, headOf, withSerials, serialNeeds, photosFor,
  inspectionUnits, inspectionMarks, photosOfKeys, intangible, purchaseView, rowKey,
} from '../src/dao/inspection.mjs';
import { planReport, describePlan, takenDateOf, expectedValues, diffValues } from '../src/dao/inspection-report.mjs';
import { paymentStatus, describeStatus } from '../src/dao/status.mjs';
import { receiptFileName } from '../src/tools/inspection.mjs';
import { parseConfig } from '../src/config.mjs';

/** A purchase in the shape purchaseView() returns (legacy fields from `data`, plus columns). */
const P = (key, fields) => ({
  key, title: `${key} 구매의 건`, docId: null, usd: null, paidKrw: null, paidAt: '2026-03-02', amountKrw: null,
  vendor: '테스트상점', note: [], project: '스마트농장', cardIssuer: '테스트카드', ...fields,
});
const row = (useDate, merchant, amount, extra = {}) => ({ id: `${useDate}/${amount}`, useDate: `${useDate} 10:00:00`, merchant, amount, status: '작성대기', docId: null, ...extra });
const noMarks = inspectionMarks([]);

describe('items and amounts', () => {
  test('item lines come from the note when it lists items, else from the item field', () => {
    const p = P('cam', { item: '테스트 카메라 / 1개, 렌즈 필터 / 2개', note: ['테스트 카메라 / 1개 100,000원', '렌즈 필터 / 2개 20,000원', 'https://example.com/cam'] });
    assert.deepEqual(itemLines(p), ['테스트 카메라 / 1개 100,000원', '렌즈 필터 / 2개 20,000원']);
    assert.deepEqual(itemLines(P('x', { item: '케이블 (흰색, 2m) / 3개' })), ['케이블 (흰색, 2m) / 3개']);
    assert.equal(unitCount(P('x', { item: '충전기 6개입 / 1세트, 케이블 타이 100개입 / 1개' })), 7);
    assert.equal(unitCount(P('x', { item: '나사 세트 총 12개' })), 12);
  });

  test('parseLine reads USD, KRW, unit price and quantity', () => {
    assert.deepEqual(parseLine('모듈 A / 2개 USD 12.50'), { name: '모듈 A', usd: 12.5, krw: null, unit: null, qty: 2, unitWord: '개' });
    assert.deepEqual(parseLine('보드 / 1대 45,000원'), { name: '보드', usd: null, krw: 45000, unit: null, qty: 1, unitWord: '대' });
    assert.deepEqual(parseLine('볼트 10개, 단가 300원'), { name: '볼트', usd: null, krw: null, unit: 300, qty: 10, unitWord: '개' });
  });

  test('KRW items add up; a 10% gap is VAT, a smaller one shipping, a negative one a discount', () => {
    const p = P('cam', { item: '테스트 카메라 / 1개, 렌즈 필터 / 2개', note: ['테스트 카메라 / 1개 100,000원', '렌즈 필터 / 2개 20,000원'] });
    const exact = reportRows(row('2026-03-02', '테스트상점', 120000), [{ ...p, amountKrw: 120000 }]);
    assert.deepEqual(exact.map((l) => [l.name, l.qty, l.amount]), [['테스트 카메라', '1개', 100000], ['렌즈 필터', '2개', 20000]]);
    const vat = reportRows(row('2026-03-02', '테스트상점', 132000), [{ ...p, amountKrw: 132000 }]);
    assert.deepEqual(vat.at(-1), { name: '부가세', vendor: '테스트상점', qty: '', unit: '', amount: 12000, note: '2026-03-02' });
    assert.equal(reportRows(row('2026-03-02', '테스트상점', 123000), [{ ...p, amountKrw: 123000 }]).at(-1).name, '배송비');
    assert.equal(reportRows(row('2026-03-02', '테스트상점', 118000), [{ ...p, amountKrw: 118000 }]).at(-1).name, '할인 및 적립금');
  });

  test('USD items split the requisition amount by USD share; a foreign gap is the exchange difference', () => {
    const p = P('ali', { item: 'A / 1개, B / 1개', note: ['A / 1개 USD 10.00', 'B / 1개 USD 30.00'], usd: 40, amountKrw: 56000, paidKrw: 56000 });
    const lines = reportRows(row('2026-03-06', 'ALIEXPRESS', 56000), [p]);
    assert.deepEqual(lines.map((l) => l.amount), [14000, 42000]);
    assert.equal(reportRows(row('2026-03-06', 'ALIEXPRESS', 56500), [p]).at(-1).name, '환율 차이');
  });

  test('a single item takes the whole amount; unknown splits ask instead of guessing; overrides win', () => {
    assert.deepEqual(reportRows(row('2026-03-02', 'X', 50000), [P('one', { item: '센서 / 4개', amountKrw: 50000 })]).map((l) => [l.qty, l.amount, l.unit]), [['4개', 50000, 12500]]);
    const two = P('two', { item: '부품 X / 1개, 부품 Y / 1개', amountKrw: 30000 });
    assert.throws(() => reportRows(row('2026-03-02', 'X', 30000), [two]), (e) => e.code === 'VALIDATION' && /mark_inspection/.test(e.message));
    const marks = inspectionMarks([{ kind: 'itemOverride', match: { key: 'two' }, data: { rows: [{ name: '부품 X', qty: 1, unitWord: '개', amount: 10000 }, { name: '부품 Y', qty: 1, unitWord: '개', amount: 20000 }] } }]);
    assert.deepEqual(reportRows(row('2026-03-02', 'X', 30000), [two], marks.itemRows).map((l) => l.amount), [10000, 20000]);
  });

  test('more than 20 rows is refused', () => {
    const many = P('many', { item: Array.from({ length: 21 }, (_, i) => `부품${i} / 1개`).join(', '), note: Array.from({ length: 21 }, (_, i) => `부품${i} / 1개 1,000원`), amountKrw: 21000 });
    assert.throws(() => reportRows(row('2026-03-02', 'X', 21000), [many]), /20줄/);
  });

  test('VAT split: foreign has no VAT, a VAT row spreads 10%, otherwise prices include VAT', () => {
    const foreign = splitVat([{ name: 'A', qty: '1개', amount: 14000 }], true);
    assert.deepEqual([foreign[0].supply, foreign[0].vat, foreign[0].total], [14000, 0, 14000]);
    const incl = splitVat([{ name: 'A', qty: '1개', amount: 11000 }], false);
    assert.deepEqual([incl[0].supply, incl[0].vat, incl[0].total], [10000, 1000, 11000]);
    const withVat = splitVat([{ name: 'A', qty: '1개', amount: 100000 }, { name: 'B', qty: '2개', amount: 20000 }, { name: '부가세', qty: '', amount: 12001 }], false);
    assert.equal(withVat.length, 2);
    assert.equal(withVat.reduce((t, l) => t + l.total, 0), 132001);
  });

  test('title head, serials on item names, intangible purchases', () => {
    const lines = [{ name: 'a', qty: '1개' }, { name: 'b', qty: '1개' }, { name: '배송비', qty: '' }];
    assert.equal(headOf([P('a', { title: '테스트 라우터 외 1종 구매의 건' })], lines), '테스트 라우터 외 1종');
    assert.equal(headOf([P('a', { title: '테스트 라우터 외 1종 구매의 건' }), P('b')], lines), '테스트 라우터 외 1종');
    assert.equal(headOf([P('a', { title: '테스트 라우터 구매의 건' }), P('b')], [...lines, { name: 'c', qty: '2개' }]), '테스트 라우터 외 2종');
    const named = withSerials({ serials: { '테스트 라우터': ['SN123'] } }, [{ name: '테스트 라우터' }, { name: '전원 케이블' }]);
    assert.equal(named[0].name, '테스트 라우터 (S/N SN123)');
    assert.equal(withSerials({ serials: 'none' }, [{ name: 'x' }])[0].name, 'x');
    assert.ok(intangible(P('s', { title: '테스트 클라우드 구독의 건' })));
    assert.ok(intangible(P('s', { item: 'API 크레딧 / 1건' })));
    assert.ok(!intangible(P('s', { item: '보드 / 1개' })));
  });

  test('serial needs: device words or the amount threshold, unless the requisition says none', () => {
    const router = P('r', { item: '테스트 라우터 / 1개', amountKrw: 90000, serials: { '테스트 라우터': ['SN1'] } });
    assert.deepEqual(serialNeeds(router), [{ item: '테스트 라우터', numbers: ['SN1'] }]);
    assert.deepEqual(serialNeeds({ ...router, serials: undefined }), [{ item: '테스트 라우터', numbers: [] }]);
    assert.deepEqual(serialNeeds({ ...router, serials: 'none' }), []);
    const pricey = P('t', { item: '측정 장치 / 1개', amountKrw: 900000 });
    assert.deepEqual(serialNeeds(pricey), []);
    assert.deepEqual(serialNeeds(pricey, { serialFromKrw: 500000 }), [{ item: '측정 장치', numbers: [] }]);
  });

  test('photo checklist follows the kind of item', () => {
    const shots = photosFor([P('r', { item: '테스트 라우터 / 2개, 충전기 / 1개', purpose: '현장 로봇 통신용' })]);
    assert.match(shots[0], /3개를 셀 수 있게/);
    assert.ok(shots.some((s) => /시리얼 번호가 읽히게/.test(s)));
    assert.ok(shots.some((s) => /정격/.test(s)));
    assert.ok(shots.some((s) => /장착된 모습/.test(s)));
    assert.ok(photosFor([P('p', { item: '테스트 스마트폰 SM-X123 / 1대' })]).some((s) => /IMEI/.test(s)));
  });
});

describe('units, marks and photos', () => {
  const ali = row('2026-03-10', 'ALIEXPRESS', 70000);
  const shop = row('2026-03-11', '테스트상점', 30000);
  const pa = P('ali-a', { usd: 20, amountKrw: 28000, paidKrw: 28000, paidAt: '2026-03-10' });
  const pb = P('ali-b', { usd: 30, amountKrw: 42000, paidKrw: 42000, paidAt: '2026-03-10' });
  const pc = P('shop-c', { amountKrw: 30000 });
  const audited = [{ row: ali, purchases: [pa, pb] }, { row: shop, purchases: [pc] }];
  const byKey = new Map([pa, pb, pc].map((p) => [p.key, p]));

  test('per-order merchants give one report per requisition with its share', () => {
    const units = inspectionUnits(audited, byKey, { perOrderMerchants: ['ALIEXPRESS'] });
    assert.deepEqual(units.map((u) => [u.key, u.a.row.amount, u.perOrder]), [
      ['2026-03-10/70000#ali-a', 28000, true], ['2026-03-10/70000#ali-b', 42000, true], ['2026-03-11/30000', 30000, false],
    ]);
    const before = inspectionUnits(audited, byKey, { perOrderMerchants: ['ALIEXPRESS'], perOrderFrom: '2026-03-20' });
    assert.equal(before[0].key, '2026-03-10/70000');
    assert.equal(before[0].ps.length, 2);
    assert.equal(rowKey(shop), '2026-03-11/30000');
  });

  test('marks and photos by purchase key', () => {
    const marks = inspectionMarks([
      { kind: 'inspectionDone', match: { key: 'ali-a' }, data: { why: '다른 분이 처리' } },
      { kind: 'inspectionIncomplete', match: { key: 'shop-c' }, data: { missing: '전원 케이블' } },
    ]);
    assert.equal(marks.doneOf([pa, pb]), '다른 분이 처리');
    assert.equal(marks.incompleteOf([pc]), '전원 케이블');
    assert.equal(marks.itemRows('shop-c'), null);
    const photos = [
      { id: 1, purchaseKey: 'ali-a', alsoFor: [], otherCard: null },
      { id: 2, purchaseKey: 'x', alsoFor: ['ali-b'], otherCard: null },
      { id: 3, purchaseKey: 'ali-a', alsoFor: [], otherCard: '9999' },
    ];
    assert.deepEqual(photosOfKeys(photos, ['ali-a', 'ali-b']).map((p) => p.id), [1, 2]);
  });

  test('purchaseView lays the legacy fields out flat', () => {
    const v = purchaseView({ key: 'k', title: 't', docId: '1000000000000001', usd: null, paidKrw: null, paidAt: '2026-03-02', amountKrw: 1000, status: 'filed', data: { item: '보드 / 1개', vendor: '테스트상점' } });
    assert.equal(v.item, '보드 / 1개');
    assert.equal(v.amountKrw, 1000);
    assert.equal(v.docId, '1000000000000001');
  });
});

describe('planReport', () => {
  const config = parseConfig({
    people: { ceo: '김대표', cto: '홍길동' },
    projects: { 스마트농장: { reviewer: '최담당' } },
    cards: { 1234: { project: '스마트농장', issuer: '테스트카드' } },
    lines: { inspection: 'ctoFinal' },
  });
  const card = { id: 'c-1', name: '연구카드(홍길동)', holder: '홍길동', cardNo: '1111-22**-****-1234', issuer: '테스트카드' };
  const profile = { name: '홍길동', deptCode: 'rnd', deptId: '10' };
  const cam = P('cam', {
    title: '테스트 카메라 외 1종 구매의 건', item: '테스트 카메라 / 1개, 렌즈 필터 / 2개',
    note: ['테스트 카메라 / 1개 100,000원', '렌즈 필터 / 2개 20,000원'], amountKrw: 120000, docId: '1000000000000011', subProject: '온실',
  });
  const r = row('2026-03-02', '테스트상점', 120000, { docId: '1000000000000099' });
  const receipt = { file: '2026-03-02_재료비_테스트-카메라_테스트상점_120000원_영수증.pdf', currency: '원', amount: 120000 };
  const unit = { a: { row: r, receipt, purchases: [cam] }, ps: [cam], key: '2026-03-02/120000', perOrder: false, cardAmount: 120000 };
  const photos = [
    { id: 7, file: 'a.jpg', purchaseKey: 'cam', alsoFor: [], otherCard: null, takenDate: '2026-03-05', shows: '상자' },
    { id: 8, file: 'b.jpg', purchaseKey: 'cam', alsoFor: [], otherCard: null, takenDate: '2026-03-06', shows: '본체' },
  ];
  const base = {
    unit, card, photos, marks: noMarks, docNums: new Map([['1000000000000011', 'TEST-11'], ['1000000000000099', 'TEST-99']]),
    profile, config, receiptPath: (x) => `/data/receipts/${x.file}`, photoPath: (p) => `/data/photos/${p.file}`,
  };

  test('a complete plan', () => {
    const plan = planReport(base);
    assert.equal(plan.title, '2026년 3월 (1234/rnd/홍길동) 검수조서 - 테스트 카메라 외 1종');
    assert.deepEqual(plan.header, {
      프로젝트명: '스마트농장 (온실)', 결제방식: '법인카드', '결제 카드번호': '테스트카드 1111-22**-****-1234', 거래처명: '테스트상점',
      사업자등록번호: '해당없음', '(은행)계좌번호': '해당없음', '총 금액(VAT포함)': '120,000', 입고일자: '2026-03-06',
      비고: '구매 품의 TEST-11, 지출결의 TEST-99',
    });
    assert.deepEqual(plan.approvers, [], 'CTO 전결 with the CTO drafting');
    assert.deepEqual(plan.lines.map((l) => [l.name, l.supply, l.vat, l.total]), [['테스트 카메라', 90909, 9091, 100000], ['렌즈 필터', 18182, 1818, 20000]]);
    assert.deepEqual(plan.total, { 수량: '', 공급가액: '109,091', 부가세액금: '10,909', 합계: '120,000' });
    assert.deepEqual(plan.files.map((f) => [f.kind, f.name]), [['receipt', receipt.file], ['photo', '제품사진_1.jpg'], ['photo', '제품사진_2.jpg']]);
    assert.match(describePlan(plan), /결재선: 기안자 단독/);
    const values = expectedValues(plan);
    assert.equal(values.length, 9 + 20 * 6 + 4);
    assert.deepEqual(values.slice(9, 15), ['테스트 카메라', '테스트상점', '1', '90,909', '9,091', '100,000']);
    assert.deepEqual(diffValues(plan, values), []);
    assert.equal(diffValues(plan, [...values.slice(0, 3), 'X', ...values.slice(4)]).length, 1);
  });

  test('another drafter gets the CTO as approver and a warning when the card is someone else\'s', () => {
    const plan = planReport({ ...base, profile: { name: '김철수', deptCode: 'rnd' } });
    assert.deepEqual(plan.approvers, [{ name: '홍길동', type: '결재' }]);
    assert.ok(plan.warnings.some((w) => /명의자/.test(w)));
  });

  for (const [label, change, check] of [
    ['no photos', { photos: [] }, (e) => e.code === 'VALIDATION' && /찍을 사진/.test(e.message)],
    ['photos without a date', { photos: [{ ...photos[0], takenDate: null }] }, (e) => e.code === 'VALIDATION' && /촬영 날짜/.test(e.message)],
    ['a missing item', { marks: inspectionMarks([{ kind: 'inspectionIncomplete', match: { key: 'cam' }, data: { missing: '렌즈 필터' } }]) }, (e) => /렌즈 필터/.test(e.message)],
    ['handled elsewhere', { marks: inspectionMarks([{ kind: 'inspectionDone', match: { key: 'cam' }, data: { why: '다른 분이 처리' } }]) }, (e) => e.code === 'CONFLICT'],
    ['requisition not filed', { unit: { ...unit, ps: [{ ...cam, docId: null }] } }, (e) => /품의가 아직/.test(e.message)],
    ['no department code', { profile: { name: '홍길동' } }, (e) => /부서 코드/.test(e.message)],
    ['receipt file missing', { receiptPath: () => null }, (e) => /save_receipt/.test(e.message)],
  ]) {
    test(`refuses: ${label}`, () => assert.throws(() => planReport({ ...base, ...change }), check));
  }

  test('a subscription needs an invoice instead of photos and uses the payment date', () => {
    const sub = P('sub', { title: '테스트 클라우드 구독의 건', item: '테스트 클라우드 월 구독 / 1개월', amountKrw: 30000, docId: '1000000000000012' });
    const u = { a: { row: row('2026-03-03', 'TESTCLOUD', 30000) }, ps: [sub], key: '2026-03-03/30000', perOrder: false, cardAmount: 30000 };
    assert.throws(() => planReport({ ...base, unit: u }), /인보이스/);
    const plan = planReport({ ...base, unit: { ...u, a: { ...u.a, receipt: { file: 'inv.pdf', currency: 'USD', amount: 21 } } } });
    assert.equal(plan.header.입고일자, '2026-03-03');
    assert.equal(plan.files.length, 1);
    assert.equal(plan.service, true);
  });

  test('per-order units attach their own receipt when one matches the USD amount', () => {
    const pa = P('ali-a', { usd: 20, amountKrw: 28000, paidKrw: 28000, docId: '1000000000000013', item: 'A / 1개' });
    const recs = [{ file: 'ra.png', currency: 'USD', amount: 20 }, { file: 'rb.png', currency: 'USD', amount: 30 }];
    const u = { a: { row: { ...row('2026-03-10', 'ALIEXPRESS', 28000) }, receipts: recs }, ps: [pa], key: '2026-03-10/70000#ali-a', perOrder: true, cardAmount: 70000 };
    const plan = planReport({ ...base, unit: u, photos: [{ ...photos[0], purchaseKey: 'ali-a' }] });
    assert.deepEqual(plan.files.filter((f) => f.kind === 'receipt').map((f) => f.name), ['ra.png']);
    assert.equal(plan.header['총 금액(VAT포함)'], '28,000');
  });

  test('takenDateOf picks the latest day', () => {
    assert.equal(takenDateOf([{ id: 1, takenDate: '2026-03-01' }, { id: 2, takenDate: '2026-03-04' }]), '2026-03-04');
  });
});

test('payment status lists what is missing per payment', () => {
  const have = P('have', { amountKrw: 30000, docId: '1000000000000021', item: '보드 / 1개' });
  const nophoto = P('nophoto', { amountKrw: 40000, item: '테스트 라우터 / 1개' });
  const audited = [
    { row: row('2026-03-02', '테스트상점', 30000, { docId: '1000000000000031', status: '결재중' }), receipt: { file: 'r' }, purchases: [have], kind: '구매', claimed: true },
    { row: row('2026-03-03', '테스트상점', 40000), purchases: [nophoto], kind: '구매', claimed: false },
    { row: row('2026-03-04', '테스트주유소', 50000), purchases: [], kind: '주유', claimed: false },
    { row: row('2026-03-05', '모르는상점', 60000), purchases: [], kind: '구매', claimed: false },
  ];
  const s = paymentStatus({
    audited, purchasesByKey: new Map([[have.key, have], [nophoto.key, nophoto]]),
    photos: [{ id: 1, purchaseKey: 'have', alsoFor: [], otherCard: null }],
    overrides: [], filings: [{ kind: 'inspection', ref: '2026-03-02/30000', docId: '1000000000000041' }], marks: noMarks,
    config: parseConfig({}),
  });
  assert.deepEqual(s.rows.map((r) => [r.receipt, r.purchase, r.photo, r.report, r.claim]), [
    ['1', '1000000000000021', '1장', '1000000000000041', '1000000000000031'],
    ['없음', '임시', '없음', '기록 없음', '아직'],
    ['필요 없음', '필요 없음', '', '', '아직'],
    ['없음', '없음', '', '', '아직'],
  ]);
  assert.equal(s.todo.photo.length, 1);
  assert.equal(s.todo.serial.length, 1, 'the router needs a serial');
  assert.equal(s.todo.purchase.length, 1);
  assert.equal(s.todo.receipt.length, 2);
  assert.match(describeStatus(s, { title: '현황' }), /검수 사진 1건/);
});

test('receipt file names follow the folder convention the matcher reads', () => {
  assert.equal(receiptFileName({ date: '2026-03-02', category: '재료비', item: '테스트 보드 외 2건', vendor: '테스트_상점', amount: 45000, currency: '원', kind: '영수증' }, 'pdf'),
    '2026-03-02_재료비_테스트-보드-외-2건_테스트-상점_45000원_영수증.pdf');
  assert.equal(receiptFileName({ date: '2026-03-02', category: '소프트웨어', item: 'Plan/Pro', vendor: 'TestCo', amount: 20.5, currency: 'USD', kind: '인보이스' }, 'jpg', 2),
    '2026-03-02_소프트웨어_Plan-Pro_TestCo_20.5USD_인보이스-2.jpg');
});
