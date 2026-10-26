import { test, describe, before, after, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import { Writable } from 'node:stream';
import { McpServer, InMemoryTransport } from '@modelcontextprotocol/server';
import { Client } from '@modelcontextprotocol/client';
import { createRuntime, createLogger } from '../src/runtime.mjs';
import { openDb } from '../src/db.mjs';
import { parseConfig } from '../src/config.mjs';
import { PluginError } from '../src/errors.mjs';
import { assertExecute } from '../src/guard.mjs';
import { READ_ONLY_POSTS } from '../src/dao/api.mjs';
import { register } from '../src/tools/trip.mjs';
import {
  validateTrip, tripTitle, tripKey, placeName, tripTransports, tripFormName, TRANSPORTS, TRIP_STATUS,
} from '../src/dao/trips.mjs';
import { parseTripBody, findDuplicates, placeTokens, listSubmittedTrips } from '../src/dao/trip-docs.mjs';
import { tripsForExpenses, tripExpense, tripKind, TRIP_EXPENSE_KIND } from '../src/dao/trip-expenses.mjs';
import { compareTripFields, verifyTrip, setTripApprovalInfo, checkTripApprovalInfo, refererNames } from '../src/dao/trip-submit.mjs';

const code = (fn) => { try { fn(); } catch (e) { return e instanceof PluginError ? e.code : e.name; } return null; };
const good = { start: '2026-03-09', end: '2026-03-09', place: '테스트시청 (테스트시)', transport: '기타', goal: '실증 부지 인허가 협의' };

describe('trips: purpose rules', () => {
  test('a goal that only repeats the place is refused', () => {
    for (const goal of ['테스트시청 방문', '테스트시 출장', '테스트시청 미팅 및 회의']) {
      const { errors } = validateTrip({ ...good, goal });
      assert.match(errors.join(' '), /되풀이하기만/, goal);
    }
  });

  test('an event name without an activity is refused', () => {
    const { errors } = validateTrip({ ...good, place: '테스트시', goal: '테스트축제' });
    assert.match(errors.join(' '), /하는 일이 없습니다/);
  });

  test('a goal that says what is done passes', () => {
    for (const goal of ['실증 부지 인허가 협의', '테스트축제 부스 운영', '시제품 성능 시험 입회', '사업 중간 발표']) {
      assert.deepEqual(validateTrip({ ...good, goal }).errors, [], goal);
    }
  });

  test('dates, order and the transport options are checked', () => {
    assert.match(validateTrip({ ...good, start: '2026-02-30' }).errors.join(' '), /시작일이 날짜/);
    assert.match(validateTrip({ ...good, start: '2026-03-10' }).errors.join(' '), /거꾸로/);
    assert.match(validateTrip({ ...good, transport: '자차' }).errors.join(' '), /자차는 기타/);
    assert.match(validateTrip({ ...good, place: '' }).errors.join(' '), /출장지가 비어/);
  });

  test('goal and note go through the writing check', () => {
    assert.match(validateTrip({ ...good, goal: '실증 부지 인허가 협의를 통해 일정 확정' }).errors.join(' '), /번역체/);
    assert.match(validateTrip({ ...good, note: '동행 김철수·이영희' }).errors.join(' '), /비고 가운뎃점/);
  });

  test('config can name the form and the transport options', () => {
    assert.equal(tripFormName({}), '국내출장신청');
    assert.equal(tripFormName({ trip: { form: '출장신청서' } }), '출장신청서');
    assert.deepEqual(tripTransports({}), [...TRANSPORTS]);
    assert.deepEqual(tripTransports({ trip: { transports: ['기타'] } }), ['기타']);
  });
});

describe('trips: title and key', () => {
  test('one day, several days, place without the region, titlePlace', () => {
    const who = { dept: 'rnd', name: '홍길동' };
    assert.equal(tripTitle(good, who), "'26.03.09/테스트시청/rnd/홍길동");
    assert.equal(tripTitle({ ...good, end: '2026-03-10' }, who), "'26.03.09-03.10/테스트시청/rnd/홍길동");
    assert.equal(tripTitle({ ...good, titlePlace: '테스트시청, 테스트항만' }, who), "'26.03.09/테스트시청, 테스트항만/rnd/홍길동");
    assert.equal(tripTitle({ ...good, place: '테스트공장 (테스트시) 및 테스트연구소 (테스트군)' }, who), "'26.03.09/테스트공장 및 테스트연구소/rnd/홍길동");
  });

  test('a missing department code is asked, not invented', () => {
    assert.equal(code(() => tripTitle(good, { name: '홍길동' })), 'VALIDATION');
  });

  test('key and place name', () => {
    assert.equal(tripKey(good), '2026-03-09/2026-03-09/테스트시청 (테스트시)');
    assert.equal(placeName('테스트전자 제2공장 (테스트시 테스트구)'), '테스트전자 제2공장');
  });
});

const body = ({ start, end, place, transport, goal, note = '' }) => `<table>
  <tr><th>출장내용</th></tr>
  <tr><td>출장기간</td><td><span data-type="calendar">${start}(월)</span> ~ <span>${end}(화)</span></td></tr>
  <tr><td>출장지</td><td>${place}</td></tr>
  <tr><td>교통편</td><td>${transport}</td></tr>
  <tr><td>출장목적</td><td>${goal}</td></tr>
  <tr><td>비고</td><td>${note}</td></tr>
  <tr><td>동행자 출장자 정보</td></tr></table>`;

describe('trip documents: body parsing and duplicates', () => {
  test('parses the trip cells by label', () => {
    assert.deepEqual(parseTripBody(body({ ...good, end: '2026-03-10', note: '동행 김철수 연구원' })), {
      start: '2026-03-09', end: '2026-03-10', place: '테스트시청 (테스트시)', transport: '기타', goal: '실증 부지 인허가 협의', note: '동행 김철수 연구원',
    });
    assert.equal(parseTripBody(body(good)).note, '', 'an empty cell followed by a label is empty');
    assert.deepEqual(parseTripBody(''), { start: '', end: '', place: '', transport: '', goal: '', note: '' });
  });

  test('overlapping dates and a shared place word are a duplicate; dates alone are only a hint', () => {
    const submitted = [
      { documentId: '1', start: '2026-03-08', end: '2026-03-09', place: '테스트시청', status: '진행중' },
      { documentId: '2', start: '2026-03-09', end: '2026-03-09', place: '테스트항만공사', status: '완료' },
      { documentId: '3', start: '2026-03-01', end: '2026-03-02', place: '테스트시청', status: '완료' },
      { documentId: '4', start: '2026-03-09', end: '2026-03-09', place: '테스트시 장터', status: '반려' },
    ];
    const { strong, weak } = findDuplicates(submitted, good);
    assert.deepEqual(strong.map((d) => d.documentId), ['1']);
    assert.deepEqual(weak.map((d) => d.documentId), ['2', '4'], 'a returned document does not block');
    assert.deepEqual(placeTokens('테스트공장(테스트시), ABC(테스트군)'), ['테스트공장', '테스트시', 'abc', '테스트군']);
  });
});

describe('trip documents: reading the mailboxes back to the trip window', () => {
  const doc = (id, formName, draftedAt) => ({ documentId: id, formName, title: '', drafterName: '홍길동', docStatusName: '진행중', draftedAt: `${draftedAt} 09:00` });
  const others = (n, start, date) => Array.from({ length: n }, (_, i) => doc(String(start + i), '구매 품의서', date));

  test('pages until documents are older than the window; a box that ignores the page stops at no new rows', async () => {
    const mine = [...others(100, 1_000_000_000_002_000, '2026-03-20'), doc('1000000000000710', '국내출장신청', '2026-02-10'), ...others(99, 1_000_000_000_003_000, '2025-08-01'), ...others(100, 1_000_000_000_004_000, '2025-07-01')];
    const dept = [doc('1000000000000711', '국내출장신청', '2026-03-01'), ...others(99, 1_000_000_000_005_000, '2026-03-01')];
    const seen = [];
    const api = async (p) => {
      seen.push(p.replace(/&.*/, ''));
      const page = Number(p.match(/page=(\d+)/)?.[1] ?? 0);
      if (p.startsWith('/gw/api/approval/doclist/draft/all')) return { status: 200, ok: true, data: { data: mine.slice(page * 100, page * 100 + 100) } };
      if (p.startsWith('/gw/api/approval/deptfolder/draft/10')) return { status: 200, ok: true, data: { data: dept } };
      return { status: 200, ok: true, data: { data: { document: { docBodyContent: body(good) } } } };
    };
    const trips = await listSubmittedTrips(api, { deptId: '10', since: '2025-09-15' });
    assert.deepEqual(trips.map((t) => t.documentId).sort(), ['1000000000000710', '1000000000000711']);
    const lists = seen.filter((x) => !x.includes('/document/'));
    assert.deepEqual(lists, [
      '/gw/api/approval/doclist/draft/all?page=0', '/gw/api/approval/doclist/draft/all?page=1',
      '/gw/api/approval/deptfolder/draft/10?page=0', '/gw/api/approval/deptfolder/draft/10?page=1',
    ]);
  });

  test('without a window only the first page is read', async () => {
    const pages = [];
    const api = async (p) => { pages.push(p); return { status: 200, ok: true, data: { data: p.includes('/document/') ? {} : others(100, 1_000_000_000_006_000, '2026-03-01') } }; };
    await listSubmittedTrips(api, {});
    assert.equal(pages.length, 1);
  });
});

describe('trip expenses: receipt-less charges by trip date', () => {
  const tripRows = [
    { key: 'a', status: TRIP_STATUS.submitted, data: { start: '2026-03-09', end: '2026-03-10', claimUntil: '2026-03-11', place: '테스트시청 (테스트시)', claimName: '테스트시' } },
    { key: 'b', status: TRIP_STATUS.elsewhere, data: { start: '2026-03-20', end: '2026-03-20', place: '테스트연구원 (테스트군)' } },
    { key: 'c', status: TRIP_STATUS.cancelled, data: { start: '2026-03-25', end: '2026-03-25', place: '취소된 곳' } },
    { key: 'd', status: TRIP_STATUS.draft, data: { start: '2026-03-28', end: '2026-03-28', place: '테스트공단' } },
    { key: 'e', status: TRIP_STATUS.submitted, data: { start: '2026-03-28', end: '2026-03-28', place: '테스트대학' } },
  ];
  const trips = tripsForExpenses(tripRows);
  const r = (date, merchant) => ({ useDate: `${date} 10:00`, merchant, amount: 10_000 });
  const confirmed = [
    { kind: TRIP_EXPENSE_KIND, match: { date: '2026-03-01', merchant: '테스트호텔' }, data: { kind: '숙박', trip: 'a' } },
    { kind: TRIP_EXPENSE_KIND, match: { date: '2026-03-02', merchant: '테스트공사' }, data: { kind: '철도', detail: '테스트 행사 출장 철도 교통비' } },
  ];

  test('cancelled trips are not used', () => {
    assert.deepEqual(trips.map((t) => t.key), ['a', 'b', 'd', 'e']);
  });

  test('a charge inside the trip (up to claimUntil) is that trip\'s expense', () => {
    assert.deepEqual(tripExpense(r('2026-03-11', '테스트주유소'), '주유', { trips }), {
      kind: '주유', trip: trips[0], purposeName: '차량유지비', detail: '테스트시 출장 유류비',
    });
    assert.equal(tripExpense(r('2026-03-20', '테스트김밥'), '식대', { trips }).detail, '테스트연구원 출장 식대');
  });

  test('outside every trip, or on a day with two trips, nothing is guessed', () => {
    assert.equal(tripExpense(r('2026-03-15', '테스트주유소'), '주유', { trips }), null);
    assert.equal(tripExpense(r('2026-03-28', '테스트주차장'), '주차', { trips }), null);
    assert.equal(tripExpense(r('2026-03-11', '테스트상점'), '구매', { trips }), null, 'not a trip kind');
  });

  test('lodging needs a confirmation; confirmations can carry a detail instead of a trip', () => {
    assert.equal(tripKind(r('2026-03-10', '테스트호텔'), '경비'), '숙박');
    assert.equal(tripExpense(r('2026-03-10', '테스트호텔'), '경비', { trips }), null);
    assert.equal(tripExpense(r('2026-03-01', '테스트호텔'), '경비', { trips, confirmed }).detail, '테스트시 출장 숙박비');
    assert.deepEqual(tripExpense(r('2026-03-02', '테스트공사'), '철도', { trips, confirmed }), {
      kind: '철도', trip: null, purposeName: '출장교통비', detail: '테스트 행사 출장 철도 교통비',
    });
  });
});

describe('trip submit: read-back and server verification', () => {
  test('fields are compared one by one', () => {
    const expected = { ...good, title: 'T' };
    assert.deepEqual(compareTripFields(expected, { ...expected }), []);
    assert.deepEqual(compareTripFields(expected, { ...expected, start: '2026-03-10', title: '국내출장신청' }), [
      '제목: "국내출장신청" (기대 "T")',
      '시작일: "2026-03-10" (기대 "2026-03-09")',
    ]);
  });

  test('verifyTrip checks status, title, body, line and referers', async () => {
    const doc = {
      docStatusName: '진행중', title: 'T', docBodyContent: body(good),
    };
    const data = {
      document: doc,
      apprFlow: { activityGroups: [{ name: '승인', activities: [{ displayName: '김대표 대표이사' }] }] },
      docInfo: { docReferenceReaders: [{ reader: { fullName: '이참조 매니저' } }] },
    };
    const api = async () => ({ status: 200, ok: true, data: { data } });
    const ok = await verifyTrip(api, '1000000000000500', { ...good, title: 'T' }, { approvers: [{ name: '김대표' }], referers: [{ name: '이참조' }], waitMs: 0 });
    assert.deepEqual(ok.mismatches, []);
    doc.docBodyContent = body({ ...good, goal: '다른 목적 협의' });
    data.docInfo.docReferenceReaders = [];
    const bad = await verifyTrip(api, '1000000000000500', { ...good, title: 'T' }, { approvers: [{ name: '김대표' }], referers: [{ name: '이참조' }], waitMs: 0 });
    assert.deepEqual(bad.mismatches, ['출장목적: "다른 목적 협의" (기대 "실증 부지 인허가 협의")', '참조자: (없음) (기대 이참조)']);
  });

  test('verifyTrip matches names exactly and refuses anyone not in the preview', async () => {
    const data = {
      document: { docStatusName: '진행중', title: 'T', docBodyContent: body(good) },
      apprFlow: { activityGroups: [
        { name: '신청', activities: [{ userName: '홍길동' }] },
        { name: '승인', activities: [{ userName: '김철수' }, { userName: '김대표' }] },
      ] },
      docInfo: { docReferenceReaders: [{ reader: { name: '이참조' } }, { reader: { fullName: '박기본 팀장' } }] },
    };
    const api = async () => ({ status: 200, ok: true, data: { data } });
    const v = await verifyTrip(api, '1000000000000500', { ...good, title: 'T' }, { approvers: [{ name: '김철' }, { name: '김대표' }], referers: [{ name: '이참조' }], drafter: '홍길동', waitMs: 0 });
    assert.deepEqual(v.mismatches, ['결재선: 김철수 → 김대표 (기대 김철 → 김대표)', '참조자: 이참조, 박기본 (기대 이참조)']);
  });
});

/**
 * A stand-in for the approval editor: semantic state instead of DOM. It keeps the real primitives'
 * weak spots, so the checks around them are tested: removal and the referer skip match by substring,
 * and a drop can bring in a different person when names overlap.
 */
function fakeEditor({ line = [{ group: '신청', kind: '기안', name: '홍길동' }], referers = [], dropAs = {} } = {}) {
  const st = { line: line.map((r) => ({ ...r })), referers: referers.map((r) => ({ ...r })), saved: 0, opened: 0 };
  const text = () => st.referers.map((r) => `${r.name}${r.dept ? `(${r.dept})` : ''}`).join(', ') || '(비어 있음)';
  const ed = {
    async openApprovalLineEditor() { st.opened++; },
    async saveApprovalLineEditor() { st.saved++; },
    async approvalLineRows() { return st.line.map((r) => ({ ...r })); },
    async removeFromApprovalLine(page, name) {
      const i = st.line.findIndex((r) => r.name.includes(name));
      if (i < 0 || st.line[i].kind === '기안') return false;
      st.line.splice(i, 1);
      return true;
    },
    async dropIntoApprovalLine(page, name) { st.line.push({ group: '승인', kind: '결재', name: dropAs[name] ?? name }); },
    async setApprovalLineType(page, name, want) {
      const row = st.line.findLast((r) => r.name.includes(name));
      row.kind = want;
      return want;
    },
    async readApprovalLine() { return st.line.map((r) => `${r.kind} ${r.name}`).join(' → '); },
    async addRecipients(page, entries) {
      for (const e of entries) if (!st.referers.some((r) => r.name.startsWith(e.name))) st.referers.push({ name: e.name, dept: e.dept });
      return text();
    },
    async readRecipients() { return text(); },
    async switchEditorTab() {},
  };
  return { st, ed };
}

describe('trip submit: the approval line and referers are exactly the previewed ones', () => {
  const approvers = [{ name: '김대표', type: '결재' }];
  const referers = [{ name: '이참조', dept: '경영지원팀' }];

  test('the form\'s default line is cleared before the configured approvers go in', async () => {
    const { st, ed } = fakeEditor({ line: [{ group: '신청', kind: '기안', name: '홍길동' }, { group: '확인', kind: '확인', name: '박퇴사' }, { group: '승인', kind: '결재', name: '김대표' }] });
    const r = await setTripApprovalInfo(null, { approvers, referers }, ed);
    assert.deepEqual(st.line.map((x) => `${x.kind} ${x.name}`), ['기안 홍길동', '결재 김대표']);
    assert.deepEqual(r.referers, ['이참조']);
    assert.equal(st.saved, 1);
  });

  test('a default referer that is not in the config stops before saving', async () => {
    const { st, ed } = fakeEditor({ referers: [{ name: '박기본', dept: '재무팀' }] });
    await assert.rejects(setTripApprovalInfo(null, { approvers, referers }, ed), /참조자가 설정과 다릅니다.*박기본 은 양식 기본 참조자/);
    assert.equal(st.saved, 0);
  });

  test('a referer whose name is a prefix of someone already there is not taken as present', async () => {
    const { ed } = fakeEditor({ referers: [{ name: '이참조원', dept: '재무팀' }] });
    await assert.rejects(setTripApprovalInfo(null, { approvers, referers }, ed), /참조자가 설정과 다릅니다/);
  });

  test('a drop that brings in a different person with an overlapping name is refused', async () => {
    const { ed } = fakeEditor({ dropAs: { 김대: '김대표' } });
    await assert.rejects(setTripApprovalInfo(null, { approvers: [{ name: '김대', type: '결재' }], referers: [] }, ed), /결재선에 "김대" 이 들어가지 않았습니다/);
  });

  test('no configured referers means none may be left on the form', async () => {
    const { ed } = fakeEditor({ referers: [{ name: '박기본' }] });
    await assert.rejects(setTripApprovalInfo(null, { approvers, referers: [] }, ed), /참조자가 설정과 다릅니다: 박기본/);
  });

  test('the check right before submitting catches a line that changed after saving', async () => {
    const { st, ed } = fakeEditor();
    await setTripApprovalInfo(null, { approvers, referers }, ed);
    await checkTripApprovalInfo(null, { approvers, referers }, ed);
    st.line.push({ group: '승인', kind: '합의', name: '최추가' });
    await assert.rejects(checkTripApprovalInfo(null, { approvers, referers }, ed), /결재선 결재 김대표 → 합의 최추가/);
  });

  test('referer names come from the referer table text', () => {
    assert.deepEqual(refererNames('이참조(경영지원팀), 박기본(재무팀)'), ['이참조', '박기본']);
    assert.deepEqual(refererNames('(비어 있음)'), []);
    assert.deepEqual(refererNames('(referer 탭 없음)'), []);
  });
});

// ------------------------------------------------------------------ the tools over MCP

const BASE = 'https://example.daouoffice.com';
const CREDS = { 'xylolabs/credentials': { username: 'hong', password: 'pw' } };

function fakeDaou() {
  const state = {
    mine: [],
    dept: [],
    bodies: {},
    calls: [],
    filed: {},
    submits: [],
    goalOnServer: null,
  };
  const ok = (data) => ({ status: 200, ok: true, data });
  const raw = async (p, opts = {}) => {
    state.calls.push({ path: p, method: opts.method ?? 'GET' });
    if (p === '/gw/api/approval/apprform/tree') return ok([{ data: { title: '일반' }, metadata: { id: 'f1' }, attr: { rel: 'FOLDER' } }]);
    if (p === '/gw/api/approval/apprform/tree?folderId=f1') return ok([{ data: { title: '국내출장신청' }, metadata: { id: 'form-1' }, attr: { rel: 'FORM' } }]);
    if (p.startsWith('/gw/api/approval/doclist/draft/all')) return ok({ data: state.mine });
    if (p.startsWith('/gw/api/approval/deptfolder/draft/10')) return ok({ data: state.dept });
    const m = p.match(/^\/gw\/api\/approval\/document\/(\d+)$/);
    if (m && state.filed[m[1]]) return ok({ data: state.filed[m[1]] });
    if (m && state.bodies[m[1]]) return ok({ data: { document: { docBodyContent: state.bodies[m[1]], docStatusName: '진행중' } } });
    return { status: 404, ok: false, data: null };
  };
  const api = async (p, opts = {}) => {
    const method = String(opts.method ?? 'GET').toUpperCase();
    if (method !== 'GET' && !READ_ONLY_POSTS.includes(p.split('?')[0])) assertExecute(`${method} ${p}`);
    return raw(p, opts);
  };
  const sessions = { async withSession(owner, creds, opts, fn) { return fn({ page: {}, api, context: {}, owner, baseUrl: BASE, fresh: false }); } };
  // The draft window, replaced: checks the guard like the real one and leaves a server document.
  const dom = {
    async submitTrip(page, api_, baseUrl, args) {
      if (state.failBeforeSend) throw new PluginError('UPSTREAM', '결재 정보 편집기가 열리지 않았습니다.');
      assertExecute('결재요청');
      if (state.failAfterSend) throw new PluginError('UPSTREAM', '화면이 제때 응답하지 않았습니다.');
      state.submits.push(args);
      const docId = String(1_000_000_000_000_650 + state.submits.length);
      state.filed[docId] = {
        document: { docStatusName: '진행중', title: args.title, docBodyContent: body({ ...args.trip, goal: state.goalOnServer ?? args.trip.goal }) },
        apprFlow: { activityGroups: [{ name: '승인', activities: args.approvers.map((a) => ({ displayName: a.name })) }] },
        docInfo: { docReferenceReaders: args.referers.map((x) => ({ reader: { fullName: x.name } })) },
      };
      return { docId, line: '기안 홍길동 → 결재 김대표', referers: '이참조(경영지원팀)' };
    },
  };
  return { state, sessions, dom };
}

describe('file_trip and list_trips over MCP with a fake DaouOffice', () => {
  let client;
  let daou;
  let db;
  const fileTrip = (args) => client.callTool({ name: 'file_trip', arguments: args, _meta: CREDS });
  const executeTrip = async (args) => {
    await fileTrip(args);
    return client.callTool({ name: 'file_trip', arguments: args, _meta: { ...CREDS, 'xylolabs/mode': 'execute' } });
  };

  before(async () => {
    daou = fakeDaou();
    db = openDb(':memory:');
    db.upsertAccount({ owner: 'hong', name: '홍길동', deptId: '10', deptCode: 'rnd', deptName: '연구개발팀' });
    const config = parseConfig({
      baseUrl: BASE, people: { ceo: '김대표' },
      trip: { approvers: [{ role: 'ceo', type: '결재' }], referers: [{ name: '이참조', dept: '경영지원팀' }] },
    });
    const rt = createRuntime({ config, db, sessions: daou.sessions, dataDir: '/tmp/dao-trip-test', logger: createLogger({ stream: new Writable({ write(_c, _e, cb) { cb(); } }) }) });
    const server = new McpServer({ name: 'daouoffice', version: '0.0.0' }, { capabilities: { tools: {} } });
    register(server, rt, { dom: daou.dom });
    const [a, b] = InMemoryTransport.createLinkedPair();
    await server.connect(a);
    client = new Client({ name: 't', version: '0' });
    await client.connect(b);
  });
  after(async () => { await client.close(); db.close(); });
  beforeEach(() => { daou.state.mine = []; daou.state.dept = []; daou.state.bodies = {}; daou.state.calls.length = 0; });

  test('kinds: file_trip is gated, list_trips is a read', async () => {
    const { tools } = await client.listTools();
    const by = Object.fromEntries(tools.map((t) => [t.name, t]));
    assert.equal(by.file_trip._meta['xylolabs/kind'], 'write');
    assert.equal(by.file_trip.annotations.destructiveHint, true);
    assert.equal(by.list_trips._meta['xylolabs/kind'], 'read');
    assert.deepEqual(by.file_trip.inputSchema.properties.transport.enum, [...TRANSPORTS]);
  });

  test('preview shows title, fields, line and referers, and only reads', async () => {
    const r = await fileTrip({ ...good, note: '동행 김철수 연구원' });
    assert.ok(!r.isError, r.content[0].text);
    const text = r.content[0].text;
    assert.match(text, /출장 신청 미리보기: '26\.03\.09\/테스트시청\/rnd\/홍길동/);
    assert.match(text, /출장목적: 실증 부지 인허가 협의/);
    assert.match(text, /결재선: 기안 홍길동 → 결재 김대표/);
    assert.match(text, /참조: 이참조/);
    assert.match(text, /다른 부서 사람이 올린 같은 출장은 잡히지 않으니/);
    assert.equal(r.structuredContent.preview, true);
    assert.ok(daou.state.calls.every((c) => c.method === 'GET'));
    assert.ok(daou.state.calls.some((c) => c.path.startsWith('/gw/api/approval/deptfolder/draft/10')), 'reads the department box');
  });

  test('a goal that repeats the place is a VALIDATION asking the person', async () => {
    const r = await fileTrip({ ...good, goal: '테스트시청 방문' });
    assert.equal(r.structuredContent.code, 'VALIDATION');
    assert.match(r.content[0].text, /사람에게 물어/);
  });

  test('a transport outside the form options is rejected by the schema', async () => {
    const r = await fileTrip({ ...good, transport: '자차' });
    assert.equal(r.isError, true);
  });

  test('a colleague\'s trip in the department box is a duplicate; allowOverlap shows it in the preview', async () => {
    daou.state.dept = [{ documentId: '1000000000000700', formName: '국내출장신청', drafterName: '김철수', docStatusName: '진행중', draftedAt: '2026-03-01 10:00' }];
    daou.state.bodies['1000000000000700'] = body(good);
    const r = await fileTrip(good);
    assert.equal(r.structuredContent.code, 'CONFLICT');
    assert.match(r.content[0].text, /김철수 · 진행중 · 부서문서함 · 문서 ID 1000000000000700/);
    const forced = await fileTrip({ ...good, allowOverlap: true });
    assert.ok(!forced.isError, forced.content[0].text);
    assert.match(forced.content[0].text, /겹치는 문서 \(사람이 다른 출장이라고 확인함\)/);
  });

  test('documents of another form are ignored', async () => {
    daou.state.mine = [{ documentId: '1000000000000701', formName: '구매 품의서', title: '테스트 구매의 건', docStatusName: '진행중' }];
    const r = await fileTrip(good);
    assert.ok(!r.isError, r.content[0].text);
    assert.ok(!daou.state.calls.some((c) => c.path.endsWith('/1000000000000701')), 'no detail read for other forms');
  });

  test('execute without the approved preview is refused before the draft window opens', async () => {
    const trip = { ...good, start: '2026-03-24', end: '2026-03-24' };
    const before = daou.state.submits.length;
    const r = await client.callTool({ name: 'file_trip', arguments: trip, _meta: { ...CREDS, 'xylolabs/mode': 'execute' } });
    assert.equal(r.structuredContent.code, 'CONFLICT');
    assert.equal(daou.state.submits.length, before);
  });

  test('execute files the trip in the draft department, records it and verifies the server document', async () => {
    const trip = { ...good, start: '2026-03-16', end: '2026-03-17' };
    const r = await executeTrip(trip);
    assert.ok(!r.isError, r.content[0].text);
    const sent = daou.state.submits.at(-1);
    assert.deepEqual([sent.deptId, sent.formId, sent.title], ['10', 'form-1', "'26.03.16-03.17/테스트시청/rnd/홍길동"]);
    assert.deepEqual(sent.approvers, [{ name: '김대표', type: '결재' }]);
    assert.deepEqual(sent.referers, [{ name: '이참조', dept: '경영지원팀' }]);
    const saved = db.trip('hong', tripKey(trip));
    assert.deepEqual([saved.status, saved.docId, saved.data.title], [TRIP_STATUS.submitted, r.structuredContent.docId, sent.title]);
    const [f] = db.filings('hong', { kind: 'trip', ref: tripKey(trip) });
    assert.deepEqual([f.status, f.detail.verified], ['진행중', true]);
  });

  test('execute keeps what was saved about the trip and only moves a draft to submitted', async () => {
    const draft = { ...good, start: '2026-03-18', end: '2026-03-18' };
    db.upsertTrip({ owner: 'hong', key: tripKey(draft), data: { ...draft, goal: '옛 목적', claimName: '테스트시', claimUntil: '2026-03-19', imported: true }, status: TRIP_STATUS.draft });
    const r = await executeTrip(draft);
    assert.ok(!r.isError, r.content[0].text);
    const saved = db.trip('hong', tripKey(draft));
    assert.deepEqual([saved.status, saved.data.goal, saved.data.claimName, saved.data.claimUntil, saved.data.imported], [TRIP_STATUS.submitted, good.goal, '테스트시', '2026-03-19', true]);

    const other = { ...good, start: '2026-03-19', end: '2026-03-19' };
    db.upsertTrip({ owner: 'hong', key: tripKey(other), data: other, status: TRIP_STATUS.elsewhere });
    const r2 = await executeTrip(other);
    assert.ok(!r2.isError, r2.content[0].text);
    assert.equal(db.trip('hong', tripKey(other)).status, TRIP_STATUS.elsewhere);
  });

  test('a failure before the final click is a plain failure; after it, an unknown outcome', async () => {
    const trip = { ...good, start: '2026-03-25', end: '2026-03-25' };
    try {
      daou.state.failBeforeSend = true;
      const before = await executeTrip(trip);
      assert.equal(before.structuredContent.code, 'UPSTREAM');
      assert.equal(before.structuredContent.data?.afterWrite, undefined);
      assert.match(before.content[0].text, /편집기가 열리지 않았습니다/);
      daou.state.failBeforeSend = false;
      daou.state.failAfterSend = true;
      const after = await executeTrip(trip);
      assert.equal(after.structuredContent.data.afterWrite, true);
      assert.match(after.content[0].text, /다시 올리기 전에 다우오피스에서 문서를 확인해 주세요/);
      assert.doesNotMatch(after.content[0].text, /다시 시도/);
    } finally {
      daou.state.failBeforeSend = false;
      daou.state.failAfterSend = false;
    }
  });

  test('execute reports a server document that differs as a failure', async () => {
    daou.state.goalOnServer = '다른 목적 협의';
    try {
      const trip = { ...good, start: '2026-03-23', end: '2026-03-23' };
      const r = await executeTrip(trip);
      assert.equal(r.structuredContent.code, 'UPSTREAM');
      assert.match(r.content[0].text, /출장목적: "다른 목적 협의"/);
      assert.equal(db.trip('hong', tripKey(trip)).status, TRIP_STATUS.submitted, 'the submitted document is still recorded');
    } finally {
      daou.state.goalOnServer = null;
    }
  });

  test('list_trips shows saved trips and, on request, the trip documents', async () => {
    db.upsertTrip({ owner: 'hong', key: tripKey(good), data: good, docId: '1000000000000702', status: TRIP_STATUS.submitted });
    const r = await client.callTool({ name: 'list_trips', arguments: {}, _meta: CREDS });
    assert.ok(!r.isError, r.content[0].text);
    assert.match(r.content[0].text, /2026-03-09 테스트시청 \(테스트시\) · 실증 부지 인허가 협의 · 기타 · 올림 · 문서 ID 1000000000000702/);
    daou.state.mine = [{ documentId: '1000000000000702', formName: '국내출장신청', drafterName: '홍길동', docStatusName: '진행중', draftedAt: '2026-03-01' }];
    daou.state.bodies['1000000000000702'] = body(good);
    const live = await client.callTool({ name: 'list_trips', arguments: { includeSubmitted: true }, _meta: CREDS });
    assert.match(live.content[0].text, /다우오피스 출장 문서 1건/);
    assert.equal(live.structuredContent.documents[0].documentId, '1000000000000702');
  });
});
