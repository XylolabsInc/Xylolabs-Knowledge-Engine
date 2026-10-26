import { test, describe, before, after, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import { Writable } from 'node:stream';
import { McpServer, InMemoryTransport } from '@modelcontextprotocol/server';
import { Client } from '@modelcontextprotocol/client';
import { createRuntime, createLogger } from '../src/runtime.mjs';
import { openDb } from '../src/db.mjs';
import { parseConfig } from '../src/config.mjs';
import { registerAll } from '../src/tools/index.mjs';

const BASE = 'https://example.daouoffice.com';
const CREDS = { 'xylolabs/credentials': { username: 'hong', password: 'pw' } };
const EXECUTE = { ...CREDS, 'xylolabs/mode': 'execute' };
const DOC = '1000000000000001';

/**
 * A DaouOffice stand-in: `api` answers from `state`, `page` records DOM actions.
 * The withdraw button flips the document to 임시저장.
 */
function fakeDaou() {
  const state = {
    status: '진행중',
    canCancel: true,
    clicks: 0,
    gotos: [],
    session: { data: { name: '홍길동', id: 'u1' } },
    tree: { data: [{ nodeType: 'DEPARTMENT', id: '10', name: '연구개발팀', code: 'rnd', children: [{ nodeType: 'MEMBER', id: 'u1', name: '홍길동' }] }] },
  };
  const api = async (p) => {
    const ok = (data) => ({ status: 200, ok: true, data });
    if (p === '/gw/api/user/session') return ok(state.session);
    if (p === '/api/portal/common/organization/tree') return ok(state.tree);
    if (p === `/gw/api/approval/document/${DOC}`) {
      return ok({ data: {
        document: { title: '카메라 구매의 건', formName: '구매 품의서', docStatusName: state.status, docBodyContent: '<table><tr><td>구매품명</td><td><span data-type="text" data-value="카메라 / 1개"></span></td></tr></table>' },
        apprFlow: { activityGroups: [{ name: '승인', activities: [{ userName: '김대표', displayName: '김대표', statusName: '대기' }] }] },
        docInfo: {},
        actionCheck: { isAllowCancelDraftUser: state.canCancel },
      } });
    }
    if (p === '/eacc/api/ess/custom/code/getList') {
      return ok({ data: { list: [{ valueField: 'c-1', displayField: '연구카드(홍길동) [홍길동]', creditcardNo: '1111-22**-****-1234', description2: '테스트카드' }] } });
    }
    if (p === '/eacc/api/ess/myExp/getCompanyCardList') {
      return ok({ data: { list: [
        { purchaseId: 'a', useDate: '2026-03-02 10:00', bpName: '테스트상점', grossAmount: 12_000, status: '9', usePurposeName: '' },
        { purchaseId: 'b', useDate: '2026-03-03 10:00', bpName: '테스트상점', grossAmount: 8_000, status: '2', usePurposeName: '재료비', adminName: '홍길동' },
      ] } });
    }
    return { status: 404, ok: false, data: null };
  };
  const page = {
    on() {}, off() {},
    url: () => `${BASE}/home`,
    async goto(u) { state.gotos.push(u); },
    async waitForLoadState() {},
    async waitForTimeout() {},
    async waitForFunction() {},
    locator: () => ({ count: async () => 0 }),
    keyboard: { press: async () => {} },
    async click() { state.clicks++; state.status = '임시저장'; },
    async evaluate(fn, arg) {
      if (typeof arg === 'string') return [{ label: '구매품명', values: ['카메라 / 1개'] }];   // body fields
      return true;                                                                           // withdraw button found
    },
  };
  const sessions = {
    verifyResult: { duplicate: false, fresh: true },
    logins: 0,
    async verify() { this.logins++; return this.verifyResult; },
    async withSession(owner, creds, opts, fn) {
      return fn({ page, api, context: {}, owner, baseUrl: BASE, fresh: false });
    },
  };
  return { state, sessions, page, api };
}

describe('tools against a fake DaouOffice', () => {
  let client;
  let daou;
  let db;

  before(async () => {
    daou = fakeDaou();
    db = openDb(':memory:');
    const server = new McpServer({ name: 'daouoffice', version: '0.0.0' }, { capabilities: { tools: {} } });
    const sink = new Writable({ write(_c, _e, cb) { cb(); } });
    const rt = createRuntime({ config: parseConfig({ baseUrl: BASE }), db, sessions: daou.sessions, dataDir: '/tmp/dao-test', logger: createLogger({ stream: sink }) });
    registerAll(server, rt);
    const [a, b] = InMemoryTransport.createLinkedPair();
    await server.connect(a);
    client = new Client({ name: 't', version: '0' });
    await client.connect(b);
  });
  after(async () => { await client.close(); db.close(); });
  beforeEach(() => { daou.state.status = '진행중'; daou.state.canCancel = true; daou.state.clicks = 0; });

  test('connect stores the profile and names the account', async () => {
    const r = await client.callTool({ name: 'connect', arguments: { username: 'Hong', password: 'pw', allowKick: false } });
    assert.ok(!r.isError, r.content[0].text);
    assert.deepEqual(r.structuredContent, { accountId: 'hong', accountName: '홍길동 (연구개발팀)', duplicate: false });
    assert.equal(db.account('hong').deptCode, 'rnd');
  });

  test('connect that meets the duplicate modal still succeeds and kicks nobody', async () => {
    daou.sessions.verifyResult = { duplicate: true, fresh: false };
    const r = await client.callTool({ name: 'connect', arguments: { username: 'kim', password: 'pw' } });
    daou.sessions.verifyResult = { duplicate: false, fresh: true };
    assert.ok(!r.isError);
    assert.equal(r.structuredContent.duplicate, true);
    assert.equal(r.structuredContent.accountName, 'kim');
    assert.match(r.content[0].text, /끊지 않았습니다/);
  });

  test('me reads the stored account', async () => {
    const r = await client.callTool({ name: 'me', arguments: {}, _meta: CREDS });
    assert.match(r.content[0].text, /홍길동/);
    assert.equal(r.structuredContent.deptCode, 'rnd');
  });

  test('read_document shows the title, line and body fields', async () => {
    const r = await client.callTool({ name: 'read_document', arguments: { docId: DOC }, _meta: CREDS });
    assert.ok(!r.isError, r.content[0].text);
    assert.match(r.content[0].text, /카메라 구매의 건 \(진행중\)/);
    assert.match(r.content[0].text, /구매품명: 카메라 \/ 1개/);
    assert.match(r.content[0].text, /결재선: 김대표\(대기\)/);
  });

  test('read_document refuses a non-numeric id before calling DaouOffice', async () => {
    const r = await client.callTool({ name: 'read_document', arguments: { docId: '../../etc' }, _meta: CREDS });
    assert.equal(r.isError, true);
  });

  test('card_usage totals by status and hides raw rows', async () => {
    const r = await client.callTool({ name: 'card_usage', arguments: { from: '2026-03-01', to: '2026-03-31', card: '1234' }, _meta: CREDS });
    assert.ok(!r.isError, r.content[0].text);
    assert.match(r.content[0].text, /합계 2건 20,000원/);
    assert.match(r.content[0].text, /작성대기 1건 12,000원/);
    assert.equal(r.structuredContent.card.last4, '1234');
    assert.equal(r.structuredContent.rows[0].raw, undefined);
  });

  test('withdraw_document preview reads but never clicks', async () => {
    const r = await client.callTool({ name: 'withdraw_document', arguments: { docId: DOC }, _meta: CREDS });
    assert.ok(!r.isError, r.content[0].text);
    assert.equal(r.structuredContent.preview, true);
    assert.match(r.content[0].text, /상신취소합니다/);
    assert.equal(daou.state.clicks, 0);
    assert.equal(daou.state.status, '진행중');
  });

  test('withdraw_document preview refuses a document that cannot be withdrawn', async () => {
    daou.state.canCancel = false;
    const r = await client.callTool({ name: 'withdraw_document', arguments: { docId: DOC }, _meta: CREDS });
    assert.equal(r.structuredContent.code, 'CONFLICT');
  });

  test('withdraw_document execute withdraws, verifies and records the filing', async () => {
    const r = await client.callTool({ name: 'withdraw_document', arguments: { docId: DOC }, _meta: EXECUTE });
    assert.ok(!r.isError, r.content[0].text);
    assert.equal(daou.state.clicks, 1);
    assert.deepEqual(r.structuredContent, { docId: DOC, title: '카메라 구매의 건', status: '임시저장', verified: true });
    assert.equal(db.filings('hong', { kind: 'withdraw' })[0].docId, DOC);
  });

  test('guide works without an account', async () => {
    const r = await client.callTool({ name: 'guide', arguments: {} });
    assert.ok(!r.isError);
    assert.match(r.content[0].text, /다우오피스 안내/);
    const bad = await client.callTool({ name: 'guide', arguments: { topic: 'nope' } });
    assert.equal(bad.isError, true);
  });
});
