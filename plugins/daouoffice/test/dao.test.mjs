import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { findDepartment, readProfile, sessionUser } from '../src/dao/org.mjs';
import { listForms, findForm } from '../src/dao/forms.mjs';
import { listUsage, listCards, pickCard, last4Of, statusCodeOf } from '../src/dao/cards.mjs';
import { listDrafts, todoCount, listTodo, leaveBalance, attendanceToday } from '../src/dao/documents.mjs';
import { loadConfig, parseConfig } from '../src/config.mjs';
import { kstDate, kstIso, monthRange, addDays } from '../src/dao/kst.mjs';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');

/** A fake `api()` answering from a table of path → body (or function of path, options). */
function fakeApi(routes) {
  const calls = [];
  const api = async (p, opts = {}) => {
    calls.push({ path: p, ...opts });
    const key = Object.keys(routes).find((k) => p === k || p.startsWith(`${k}?`)) ?? Object.keys(routes).find((k) => p.startsWith(k));
    if (!key) return { status: 404, ok: false, data: null };
    const v = typeof routes[key] === 'function' ? routes[key](p, opts) : routes[key];
    return { status: 200, ok: true, data: v };
  };
  api.calls = calls;
  return api;
}

describe('org: profile from session and org tree', () => {
  const tree = {
    data: [{
      nodeType: 'COMPANY', id: '1', name: '테스트회사', children: [
        { nodeType: 'DEPARTMENT', id: '10', name: '연구개발팀', code: 'rnd', children: [
          { nodeType: 'MEMBER', id: 'u1', name: '홍길동' },
          { nodeType: 'MEMBER', id: 'u2', name: '김철수' },
        ] },
        { nodeType: 'DEPARTMENT', id: '20', name: '경영지원팀', code: 'mgmt', children: [
          { nodeType: 'MEMBER', id: 'u3', name: '김철수' },
        ] },
      ],
    }],
  };

  test('finds the department of a unique name', () => {
    assert.deepEqual(findDepartment(tree, { name: '홍길동' }), { deptId: '10', deptCode: 'rnd', deptName: '연구개발팀' });
  });

  test('a duplicate name is resolved by user id, or left unknown', () => {
    assert.deepEqual(findDepartment(tree, { name: '김철수', userId: 'u3' }), { deptId: '20', deptCode: 'mgmt', deptName: '경영지원팀' });
    assert.deepEqual(findDepartment(tree, { name: '김철수' }), { deptId: null, deptCode: null, deptName: null });
  });

  test('a known department id wins', () => {
    assert.equal(findDepartment(tree, { name: '김철수', deptId: '10' }).deptCode, 'rnd');
  });

  test('flat lists with a parent reference work too', () => {
    const flat = [
      { nodeType: 'DEPT', id: 'd1', title: '연구소', code: 'rnd' },
      { nodeType: 'USER', id: 'p1', title: '홍길동', deptId: 'd1' },
    ];
    assert.deepEqual(findDepartment(flat, { name: '홍길동' }), { deptId: 'd1', deptCode: 'rnd', deptName: '연구소' });
  });

  test('readProfile combines both and falls back to the config code', async () => {
    const api = fakeApi({ '/gw/api/user/session': { code: '200', data: { name: '홍길동', id: 'u1' } }, '/api/portal/common/organization/tree': tree });
    assert.deepEqual(await readProfile(api), { name: '홍길동', userId: 'u1', deptId: '10', deptCode: 'rnd', deptName: '연구개발팀' });

    const lazy = fakeApi({
      '/gw/api/user/session': { data: { name: '홍길동', deptMembers: [{ deptId: '10', deptName: '연구개발팀' }] } },
      '/api/portal/common/organization/tree': { data: [{ nodeType: 'COMPANY', id: '1', name: '테스트회사' }] },
    });
    assert.deepEqual(await readProfile(lazy, { fallbacks: { 연구개발팀: { code: 'rnd' } } }),
      { name: '홍길동', userId: null, deptId: '10', deptCode: 'rnd', deptName: '연구개발팀' });
  });

  test('a session without a name is an upstream failure', async () => {
    await assert.rejects(sessionUser(fakeApi({ '/gw/api/user/session': { data: {} } })), (e) => e.code === 'UPSTREAM');
  });
});

describe('forms', () => {
  const api = fakeApi({
    '/gw/api/approval/apprform/tree': (p) => {
      if (p.endsWith('folderId=F1')) {
        return { data: [
          { data: { title: '구매 품의서' }, metadata: { id: '101' }, attr: { rel: 'FORM' } },
          { data: { title: '구매 품의서(구)' }, metadata: { id: '102' }, attr: { rel: 'FORM' } },
        ] };
      }
      return { data: [{ data: { title: '일반' }, metadata: { id: 'F1' }, attr: { rel: 'FOLDER' } },
        { data: { title: '국내출장신청' }, metadata: { id: '103' }, attr: { rel: 'FORM' } }] };
    },
  });

  test('flattens the jsTree folders', async () => {
    const forms = await listForms(api);
    assert.deepEqual(forms.map((f) => f.path), ['일반', '일반 / 구매 품의서', '일반 / 구매 품의서(구)', '국내출장신청']);
  });

  test('finds by exact name, refuses an ambiguous part, reports a missing one', async () => {
    assert.equal((await findForm(api, '구매 품의서')).id, '101');
    assert.equal((await findForm(api, '출장')).id, '103');
    await assert.rejects(findForm(api, '품의'), (e) => e.code === 'VALIDATION');
    await assert.rejects(findForm(api, '없는양식'), (e) => e.code === 'NOT_FOUND');
  });
});

describe('cards', () => {
  const cardList = { data: { data: { list: [
    { valueField: 'c-1', displayField: '연구카드(홍길동)   [홍길동]', creditcardNo: '1111-22**-****-1234', description2: '테스트카드' },
    { valueField: 'c-2', displayField: '운영카드(김철수)   [김철수]', creditcardNo: '1111-22**-****-5678', description2: '테스트카드' },
  ] } } };

  test('lists and picks cards by last four digits, name or id', async () => {
    const cards = await listCards(fakeApi({ '/eacc/api/ess/custom/code/getList': cardList.data }));
    assert.equal(cards[0].holder, '홍길동');
    assert.equal(cards[0].name, '연구카드(홍길동)');
    assert.equal(pickCard(cards, '5678').id, 'c-2');
    assert.equal(pickCard(cards, '연구').id, 'c-1');
    assert.equal(pickCard(cards, 'c-1').id, 'c-1');
    assert.equal(last4Of(cards[0]), '1234');
    assert.throws(() => pickCard(cards, '카드'), (e) => e.code === 'VALIDATION');
    assert.throws(() => pickCard(cards, '9999'), (e) => e.code === 'NOT_FOUND');
    assert.equal(statusCodeOf('결재완료'), '2');
    assert.throws(() => statusCodeOf('승인'), (e) => e.code === 'VALIDATION');
  });

  test('usage splits a range that hits the observed maximum and de-duplicates rows', async () => {
    const day = (d) => Number(d.slice(6, 8));
    const api = fakeApi({
      '/eacc/api/ess/myExp/getCompanyCardList': (p, { body }) => {
        assert.deepEqual(body.status, ['']);
        assert.deepEqual(body.statuses, []);
        const from = day(body.fromDate); const to = day(body.toDate);
        if (from === 1 && to === 31) return { data: { list: Array.from({ length: 1001 }, (_, i) => ({ purchaseId: `x${i}`, useDate: '2026-03-01', grossAmount: 1, status: '9' })) } };
        const list = [];
        for (let d = from; d <= to; d++) list.push({ purchaseId: `p${d}`, useDate: `2026-03-${String(d).padStart(2, '0')} 10:00`, grossAmount: 1000, status: d % 2 ? '9' : '2', bpName: '테스트상점' });
        return { data: { list } };
      },
    });
    const r = await listUsage(api, { from: '2026-03-01', to: '2026-03-31' });
    assert.equal(r.rows.length, 31);
    assert.equal(r.totals.amount, 31_000);
    assert.equal(r.totals.byStatus.작성대기.count, 16);
    assert.equal(r.rows[0].useDate.slice(0, 10), '2026-03-31', 'newest first');
    assert.equal(r.rows[0].merchant, '테스트상점');
    assert.deepEqual(r.truncated, []);
  });

  test('bad ranges are validation errors', async () => {
    await assert.rejects(listUsage(fakeApi({}), { from: '2026-03-02', to: '2026-03-01' }), (e) => e.code === 'VALIDATION');
    await assert.rejects(listUsage(fakeApi({}), { from: '2026/03/01', to: '2026/03/02' }), (e) => e.code === 'VALIDATION');
  });
});

describe('documents', () => {
  test('drafts and inbox use the doclist rows', async () => {
    const row = { documentId: 1000000000000001, formName: '구매 품의서', title: '카메라 구매의 건', drafterName: '홍길동', draftedAt: '2026-03-02 10:00', docStatusName: '진행중' };
    const api = fakeApi({
      '/gw/api/approval/doclist/draft/all': { data: [row] },
      '/gw/api/approval/todo/count': { data: { docCount: 3 } },
      '/gw/api/approval/todo': { data: [] },
      '/gw/api/approval/home/todo': { data: [row] },
    });
    assert.deepEqual(await listDrafts(api), [{ id: '1000000000000001', formName: '구매 품의서', title: '카메라 구매의 건', drafter: '홍길동', draftedAt: '2026-03-02 10:00', status: '진행중' }]);
    assert.ok(api.calls[0].path.includes('offset=20'));
    assert.equal(await todoCount(api), 3);
    const inbox = await listTodo(api);
    assert.equal(inbox.partial, true, 'fell back to the five-item gadget');
    assert.equal(inbox.docs.length, 1);
  });

  test('attendance and leave read the gadgets', async () => {
    const api = fakeApi({
      '/api/attend/normal/gadget/today': { data: { workStatusName: '근무중' } },
      '/api/leave/normal/gadget': { data: { formattedRemain: '12일' } },
    });
    assert.equal((await attendanceToday(api)).status, '근무중');
    assert.equal((await leaveBalance(api)).remain, '12일');
    await assert.rejects(leaveBalance(fakeApi({ '/api/leave/normal/gadget': { data: {} } })), (e) => e.code === 'UPSTREAM');
  });
});

describe('config', () => {
  test('the shipped example parses', () => {
    const { config, source } = loadConfig(path.join(ROOT, 'config.example.json'));
    assert.ok(source.endsWith('config.example.json'));
    assert.equal(config.baseUrl, 'https://example.daouoffice.com');
    assert.equal(config.lines.inspection, 'ctoFinal');
    assert.ok(Object.isFrozen(config.people));
  });

  test('no path → empty rules; a bad path or bad shape is loud', () => {
    assert.equal(loadConfig(undefined).config.baseUrl, undefined);
    assert.equal(loadConfig(undefined).config.cfoFrom, 1_000_000);
    assert.throws(() => loadConfig('/nonexistent/daouoffice.json'), (e) => e.code === 'INTERNAL');
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'dao-cfg-'));
    fs.writeFileSync(path.join(dir, 'bad.json'), '{ nope');
    assert.throws(() => loadConfig(path.join(dir, 'bad.json')), /JSON/);
    fs.rmSync(dir, { recursive: true, force: true });
    assert.throws(() => parseConfig({ baseUrl: 'ftp://x' }), /baseUrl/);
    assert.throws(() => parseConfig({ baseUrl: 'http://example.daouoffice.com' }), /baseUrl/, 'https only');
    assert.throws(() => parseConfig({ cards: { 12345: {} } }), /cards/);
    assert.throws(() => parseConfig({ lines: { claim: [{ type: '결재' }] } }), /name 또는 role/);
    assert.throws(() => parseConfig({ cardMatch: { subscriptionMerchants: ['('] } }), /정규식/);
    assert.equal(parseConfig({ extra: { kept: true } }).extra.kept, true, 'unknown sections are kept');
  });
});

test('kst helpers', () => {
  const d = new Date('2026-03-31T16:30:00Z');   // 01:30 KST on April 1st
  assert.equal(kstDate(d), '2026-04-01');
  assert.equal(kstIso(d), '2026-04-01T01:30:00.000+09:00');
  assert.deepEqual(monthRange('2026-02'), ['2026-02-01', '2026-02-28']);
  assert.equal(addDays('2026-03-01', -1), '2026-02-28');
});
