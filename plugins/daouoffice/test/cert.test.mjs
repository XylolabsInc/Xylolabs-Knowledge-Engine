import { test, describe, before, after, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import { Writable } from 'node:stream';
import { McpServer, InMemoryTransport } from '@modelcontextprotocol/server';
import { Client } from '@modelcontextprotocol/client';
import { createRuntime, createLogger } from '../src/runtime.mjs';
import { openDb } from '../src/db.mjs';
import { parseConfig } from '../src/config.mjs';
import { runWithMode, assertExecute } from '../src/guard.mjs';
import { register } from '../src/tools/cert.mjs';
import {
  normalizeCert, pickRequest, certSteps, isExpired, selfLineOthers, strayNames, selfApprovalAllowed, saveRequest,
} from '../src/dao/certificate.mjs';

// Made-up rows in the list API's shape, including the fields that must never leave the plugin.
const UUID = '0f6b1c2e-1111-4a2b-9c3d-123456789abc';
const EMP_NO = 'EMP-0000-TEST';
const rowOf = (extra) => ({
  issuecertId: UUID, empNo: EMP_NO, employeeId: EMP_NO, certType: '01', certName: '재직증명서', issuePurpose: 'CODE050',
  issuePurposeName: '기타', submitPlace: '테스트은행', requestDate: '20260302', reportUrl: 'EMPLOYEE_PROOF',
  docStatusCode: '', docId: null, approvalCode: null, expireDate: null, maskingYn: 'Y', ...extra,
});

describe('certificate: rows and steps', () => {
  test('normalized rows keep only what can be shown', () => {
    const c = normalizeCert(rowOf({ docStatusCode: '02', docId: 1000000000000800, approvalCode: 'A-1', expireDate: '20260402' }));
    assert.deepEqual(c, {
      kind: '재직증명서', purpose: '기타', target: '테스트은행', requestDate: '2026-03-02', status: '결재완료',
      docId: '1000000000000800', approvalCode: 'A-1', expireDate: '2026-04-02',
    });
    const text = JSON.stringify(c);
    assert.ok(!text.includes(UUID) && !text.includes(EMP_NO));
  });

  test('picks the newest request of the same kind, purpose and target, skipping expired approvals', () => {
    const rows = [
      rowOf({ issuecertId: 'old', requestDate: '20260101', docStatusCode: '02', expireDate: '20260201' }),
      rowOf({ issuecertId: 'other', requestDate: '20260310', submitPlace: '다른 곳' }),
      rowOf({ issuecertId: 'new', requestDate: '20260305', docStatusCode: '02', expireDate: '20260405' }),
    ];
    const req = { kind: '재직증명서', purpose: '기타', target: '테스트은행' };
    assert.equal(pickRequest(rows, req, '2026-03-20').issuecertId, 'new');
    assert.equal(pickRequest(rows, req, '2026-04-10'), null, 'both approvals have expired');
    assert.equal(pickRequest(rows, { ...req, kind: '경력증명서' }, '2026-03-20'), null);
    assert.ok(isExpired({ expireDate: '20260201' }, '2026-02-02'));
    assert.ok(!isExpired({ expireDate: null }, '2026-02-02'));
  });

  test('remaining steps follow the status and the route', () => {
    assert.deepEqual(certSteps(null, { self: true }), ['save', 'submit', 'approve', 'print']);
    assert.deepEqual(certSteps(null, { self: false }), ['save', 'submit', 'wait']);
    assert.deepEqual(certSteps(rowOf({}), { self: true }), ['submit', 'approve', 'print']);
    assert.deepEqual(certSteps(rowOf({ docStatusCode: '01' }), { self: true }), ['approve', 'print']);
    assert.deepEqual(certSteps(rowOf({ docStatusCode: '01' }), { self: false }), ['wait']);
    assert.deepEqual(certSteps(rowOf({ docStatusCode: '02' }), { self: false }), ['print']);
  });

  test('self-approval lines are checked for anyone else', () => {
    assert.deepEqual(selfLineOthers('기안 홍길동(연구개발팀) → 결재 홍길동(연구개발팀)', '홍길동'), []);
    assert.deepEqual(selfLineOthers('기안 홍길동(연구개발팀) → 결재 김철수(경영지원팀)', '홍길동'), ['김철수']);
    assert.deepEqual(strayNames('기안 홍길동 연구팀 결재 홍길동 대기', { me: '홍길동', myDept: '연구팀' }), []);
    assert.deepEqual(strayNames('기안 홍길동 결재 김철수 확인 예정', { me: '홍길동' }), ['김철수']);
  });

  test('self-approval is off unless the company config allows it', () => {
    assert.equal(selfApprovalAllowed({}), false);
    assert.equal(selfApprovalAllowed({ certificate: { selfApproval: true } }), true);
  });

  test('saving a request is refused outside an approved execute', async () => {
    const page = fakePage({ clicks: [] });
    await assert.rejects(runWithMode('preview', () => saveRequest(page, 'https://example.daouoffice.com', { kind: '재직증명서', purpose: '기타', target: '테스트은행' })), (e) => e.code === 'INTERNAL');
    assert.ok(!page.clicks.includes('저장'));
  });
});

/** A page that answers the HR app's ExtJS calls: the dialog opens, combos take known values, buttons are recorded. */
function fakePage(state) {
  const purposes = ['기타', '은행 제출용'];
  return {
    clicks: state.clicks,
    async goto() {}, async waitForLoadState() {}, async waitForTimeout() {},
    keyboard: { press: async () => {} },
    locator: () => ({ count: async () => 0, first() { return this; } }),
    getByRole: () => ({ first: () => ({ click: async () => { state.clicks.push('open'); } }) }),
    async evaluate(fn, arg) {
      if (arg && typeof arg === 'object' && 'kind' in arg) {
        return {
          kind: { value: arg.kind },
          purpose: purposes.includes(arg.purpose) ? { value: arg.purpose } : { invalid: `용도에 "${arg.purpose}" 이 없습니다. 고를 수 있는 값: ${purposes.join(', ')}` },
          target: { value: arg.target },
          note: { value: arg.note },
        };
      }
      if (typeof arg === 'string') { state.clicks.push(arg); return true; }
      return true;
    },
  };
}

const BASE = 'https://example.daouoffice.com';
const CREDS = { 'xylolabs/credentials': { username: 'hong', password: 'pw' } };

describe('issue_certificate and certificate_history over MCP', () => {
  const state = { rows: [], clicks: [], calls: [], steps: [] };
  let client;
  const issue = (args) => client.callTool({ name: 'issue_certificate', arguments: args, _meta: CREDS });

  // The HR app and approval window steps, replaced: they check the guard and move the list row along.
  const dom = {
    async checkRequestDialog(page, baseUrl, req) { state.clicks.push('check'); return req; },
    async saveRequest(page, baseUrl, req) {
      assertExecute('저장');
      state.steps.push('save');
      state.rows.push(rowOf({ issuecertId: 'new-1', certName: req.kind, issuePurposeName: req.purpose, submitPlace: req.target }));
    },
    async submitRequest(page, baseUrl, id, { self, me }) {
      assertExecute('전자결재');
      state.steps.push(`submit:${self}:${me}`);
      Object.assign(state.rows.find((r) => r.issuecertId === id), { docStatusCode: '01', docId: '1000000000000810' });
      return self ? `기안 ${me} → 결재 ${me}` : '기안 홍길동 → 결재 김결재';
    },
    async approveRequest(page, baseUrl, docId) {
      assertExecute('승인');
      state.steps.push(`approve:${docId}`);
      Object.assign(state.rows.find((r) => r.docId === docId), { docStatusCode: '02', approvalCode: 'A-2', expireDate: '20991231' });
    },
    async downloadCertificate(page, baseUrl, id) { state.steps.push(`print:${id === 'new-1' ? 'new' : id}`); return Buffer.from('%PDF-1.4 made-up certificate'); },
  };

  async function connect(config, { fakeDom = false } = {}) {
    const db = openDb(':memory:');
    db.upsertAccount({ owner: 'hong', name: '홍길동', deptId: '10', deptCode: 'rnd', deptName: '연구개발팀' });
    const api = async (p, opts = {}) => {
      state.calls.push({ path: p, method: opts.method ?? 'GET' });
      if (p === '/eacc/api/ess/certIsu/getIssueCertList') return { status: 200, ok: true, data: { data: { list: state.rows } } };
      return { status: 404, ok: false, data: null };
    };
    const page = fakePage(state);
    const sessions = { async withSession(owner, creds, opts, fn) { return fn({ page, api, context: {}, owner, baseUrl: BASE, fresh: false }); } };
    const rt = createRuntime({ config: parseConfig({ baseUrl: BASE, ...config }), db, sessions, dataDir: '/tmp/dao-cert-test', logger: createLogger({ stream: new Writable({ write(_c, _e, cb) { cb(); } }) }) });
    const server = new McpServer({ name: 'daouoffice', version: '0.0.0' }, { capabilities: { tools: {} } });
    register(server, rt, fakeDom ? { dom } : {});
    const [a, b] = InMemoryTransport.createLinkedPair();
    await server.connect(a);
    const c = new Client({ name: 't', version: '0' });
    await c.connect(b);
    return c;
  }

  before(async () => { client = await connect({ certificate: { selfApproval: true } }); });
  after(async () => { await client.close(); });
  beforeEach(() => { state.rows = []; state.clicks.length = 0; state.calls.length = 0; state.steps.length = 0; });

  test('kinds: issue_certificate is gated, certificate_history is a read', async () => {
    const { tools } = await client.listTools();
    const by = Object.fromEntries(tools.map((t) => [t.name, t]));
    assert.equal(by.issue_certificate._meta['xylolabs/kind'], 'write');
    assert.equal(by.certificate_history._meta['xylolabs/kind'], 'read');
    assert.deepEqual(by.issue_certificate.inputSchema.properties.kind.enum, ['재직증명서', '경력증명서']);
  });

  test('a new request: the preview fills the dialog, cancels it and explains self-approval', async () => {
    const r = await issue({ kind: '재직증명서', purpose: '은행 제출용', target: '테스트은행' });
    assert.ok(!r.isError, r.content[0].text);
    const text = r.content[0].text;
    assert.match(text, /남은 단계: 신청 저장 → 전자결재 상신 → 본인 승인 → PDF 출력/);
    assert.match(text, /본인 단독\(기안 홍길동 → 결재 홍길동\)/);
    assert.match(text, /승인하는 즉시 결재가 완료돼 PDF 가 나옵니다/);
    assert.match(text, /DM 으로만/);
    assert.deepEqual(state.clicks, ['open', '취소'], 'the dialog is cancelled, never saved');
    assert.equal(r.structuredContent.preview, true);
  });

  test('a purpose the dialog does not offer comes back as VALIDATION with the options', async () => {
    const r = await issue({ kind: '재직증명서', purpose: '여권 발급용', target: '테스트구청' });
    assert.equal(r.structuredContent.code, 'VALIDATION');
    assert.match(r.content[0].text, /고를 수 있는 값: 기타, 은행 제출용/);
    assert.ok(state.clicks.includes('취소'));
  });

  test('an approved request that has not expired is only printed again', async () => {
    state.rows = [rowOf({ docStatusCode: '02', docId: '1000000000000800', expireDate: '20991231', approvalCode: 'A-1' })];
    const r = await issue({ kind: '재직증명서', purpose: '기타', target: '테스트은행' });
    assert.ok(!r.isError, r.content[0].text);
    assert.match(r.content[0].text, /남은 단계: PDF 출력/);
    assert.ok(!r.content[0].text.includes(UUID));
    assert.deepEqual(state.clicks, [], 'no dialog for an existing request');
  });

  test('a request waiting for others is a CONFLICT when not self-approving', async () => {
    state.rows = [rowOf({ docStatusCode: '01', docId: '1000000000000801' })];
    const r = await issue({ kind: '재직증명서', purpose: '기타', target: '테스트은행', selfApprove: false });
    assert.equal(r.structuredContent.code, 'CONFLICT');
    assert.match(r.content[0].text, /이미 결재 중/);
  });

  test('certificate_history shows the requests without UUIDs or employee numbers', async () => {
    state.rows = [rowOf({}), rowOf({ requestDate: '20260310', certName: '경력증명서', docStatusCode: '02', docId: '1000000000000802', expireDate: '20260410' })];
    const r = await client.callTool({ name: 'certificate_history', arguments: {}, _meta: CREDS });
    assert.ok(!r.isError, r.content[0].text);
    assert.match(r.content[0].text, /증명서 발급 신청 2건/);
    assert.match(r.content[0].text, /2026-03-10 경력증명서 \/ 기타 \/ 테스트은행 · 결재완료 · 유효 2026-04-10까지/);
    const all = JSON.stringify(r);
    assert.ok(!all.includes(UUID) && !all.includes(EMP_NO) && !all.includes('EMPLOYEE_PROOF'));
  });

  test('execute with self-approval saves, submits, approves and returns the PDF as a private file', async () => {
    const c = await connect({ certificate: { selfApproval: true } }, { fakeDom: true });
    try {
      const r = await c.callTool({ name: 'issue_certificate', arguments: { kind: '재직증명서', purpose: '기타', target: '테스트은행' }, _meta: { ...CREDS, 'xylolabs/mode': 'execute' } });
      assert.ok(!r.isError, r.content[0].text);
      assert.deepEqual(state.steps, ['save', 'submit:true:홍길동', 'approve:1000000000000810', 'print:new']);
      const file = r.content.find((b) => b.type === 'resource');
      assert.equal(file.resource.mimeType, 'application/pdf');
      assert.equal(file._meta['xylolabs/private'], true);
      assert.equal(file.resource._meta['xylolabs/private'], true);
      assert.equal(Buffer.from(file.resource.blob, 'base64').toString('latin1').slice(0, 5), '%PDF-');
      assert.ok(!r.content[0].text.includes('%PDF'));
      assert.ok(!JSON.stringify(r.structuredContent).includes('new-1'), 'no request UUID in the result');
    } finally {
      await c.close();
    }
  });

  test('execute on the regular line stops after submitting, without a file', async () => {
    const c = await connect({}, { fakeDom: true });
    try {
      const r = await c.callTool({ name: 'issue_certificate', arguments: { kind: '경력증명서', purpose: '기타', target: '테스트은행' }, _meta: { ...CREDS, 'xylolabs/mode': 'execute' } });
      assert.ok(!r.isError, r.content[0].text);
      assert.deepEqual(state.steps, ['save', 'submit:false:홍길동']);
      assert.equal(r.content.some((b) => b.type === 'resource'), false);
      assert.match(r.content[0].text, /승인하면 같은 요청을 다시 해 PDF 를 받습니다/);
    } finally {
      await c.close();
    }
  });

  test('self-approval asked for but not allowed by the company is refused', async () => {
    const strict = await connect({});
    try {
      const r = await strict.callTool({ name: 'issue_certificate', arguments: { kind: '재직증명서', purpose: '기타', target: '테스트은행', selfApprove: true }, _meta: CREDS });
      assert.equal(r.structuredContent.code, 'VALIDATION');
      const regular = await strict.callTool({ name: 'issue_certificate', arguments: { kind: '재직증명서', purpose: '기타', target: '테스트은행' }, _meta: CREDS });
      assert.ok(!regular.isError, regular.content[0].text);
      assert.match(regular.content[0].text, /기본 결재선으로 상신합니다/);
      assert.match(regular.content[0].text, /남은 단계: 신청 저장 → 전자결재 상신 → 결재 대기/);
    } finally {
      await strict.close();
    }
  });
});
