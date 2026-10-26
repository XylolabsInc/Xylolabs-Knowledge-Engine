import { describe, test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { Writable } from 'node:stream';
import sharp from 'sharp';
import { McpServer, InMemoryTransport } from '@modelcontextprotocol/server';
import { Client } from '@modelcontextprotocol/client';
import { createRuntime, createLogger } from '../src/runtime.mjs';
import { openDb } from '../src/db.mjs';
import { parseConfig } from '../src/config.mjs';
import { PluginError } from '../src/errors.mjs';
import { currentMode } from '../src/guard.mjs';
import * as purchase from '../src/tools/purchase.mjs';

const CREDS = { 'xylolabs/credentials': { username: 'hong', password: 'pw' } };
const EXECUTE = { ...CREDS, 'xylolabs/mode': 'execute' };
const FX_TEXT = '하나은행 고시회차 100회 매매기준율 기준 미국 USD 1 달러 대한민국 KRW 1,400.00 원 2026. 01. 15. 11:00 기준';

const cfg = parseConfig({
  baseUrl: 'https://example.daouoffice.com',
  people: { ceo: '김대표', cfo: '이재무' },
  cfoFrom: 1_000_000,
  projects: { 스마트농장: { reviewer: '최담당', subProjects: ['온실', '드론'] } },
  cards: { 1234: { project: '스마트농장', budget: '프로젝트 사업비', issuer: '신한' } },
  minPurchaseKrw: 100_000,
});

/**
 * Sessions stand-in. The scratch context serves the FX page. A DaouOffice session is refused
 * (`refuse`), or runs the handler on a fake page whose documents come from `docs`; the fake page
 * stops the submit at its first DOM step (`page.on`), so a test sees how far execute got.
 */
const STOP = '시험용: 여기서 멈춘다';
function fakeSessions() {
  const s = {
    scratch: 0,
    sessionCalls: [],
    mode: 'refuse',
    docs: {},
    onScratch: null,
    async withScratchContext(fn) {
      s.scratch++;
      await s.onScratch?.();
      const page = {
        async goto() {}, async waitForFunction() {}, async evaluate() { return FX_TEXT; },
      };
      return fn({ page, context: {} });
    },
    async withSession(owner, creds, opts, fn) {
      s.sessionCalls.push({ owner, mode: currentMode() });
      if (s.mode === 'refuse') throw new PluginError('UPSTREAM', '시험용: 다우오피스에 닿지 않는다');
      const api = async (p) => {
        const id = p.match(/document\/(\d+)$/)?.[1];
        const d = s.docs[id];
        if (!d) return { status: 404, ok: false, data: null };
        return {
          status: 200, ok: true,
          data: { data: {
            document: { title: '테스트 카메라 구매의 건', formName: '구매 품의서', docStatusName: d.status, docNum: d.docNum ?? '', docBodyContent: id, attaches: [] },
            apprFlow: { activityGroups: [{ name: '승인', activities: (d.line ?? ['홍길동', '최담당']).map((userName) => ({ userName })) }] },
            actionCheck: { isAllowCancelDraftUser: Boolean(d.canCancel) },
          } },
        };
      };
      const page = {
        async evaluate() { return { 구입일자: [] }; },
        on() { throw new PluginError('UPSTREAM', STOP); },
        off() {},
      };
      return fn({ page, api, context: {}, owner, baseUrl: 'https://example.daouoffice.com', fresh: false });
    },
  };
  return s;
}

describe('purchase tools over MCP', () => {
  let client;
  let db;
  let sessions;
  let dataDir;
  let inbox;
  const call = (name, args, meta = CREDS) => client.callTool({ name, arguments: args, _meta: meta });
  const text = (r) => r.content[0].text;

  before(async () => {
    dataDir = fs.mkdtempSync(path.join(os.tmpdir(), 'purchase-tools-'));
    inbox = fs.mkdtempSync(path.join(os.tmpdir(), 'purchase-inbox-'));
    db = openDb(':memory:');
    db.upsertAccount({ owner: 'hong', name: '홍길동', deptId: '10', deptName: '연구개발팀' });
    sessions = fakeSessions();
    const server = new McpServer({ name: 'daouoffice', version: '0.0.0' }, { capabilities: { tools: {} } });
    const sink = new Writable({ write(_c, _e, cb) { cb(); } });
    const rt = createRuntime({ config: cfg, db, sessions, dataDir, logger: createLogger({ stream: sink }) });
    purchase.register(server, rt);
    const [a, b] = InMemoryTransport.createLinkedPair();
    await server.connect(a);
    client = new Client({ name: 't', version: '0' });
    await client.connect(b);
  });
  after(async () => {
    await client.close();
    db.close();
    fs.rmSync(dataDir, { recursive: true, force: true });
    fs.rmSync(inbox, { recursive: true, force: true });
  });

  const DRAFT = {
    title: '테스트 카메라 구매의 건',
    subProject: '온실',
    item: '테스트 카메라 / 1개',
    vendor: '테스트상점',
    purpose: '온실 작물 생육 원격 관찰',
    paidAt: '2026-01-15',
    amount: 132_000,
    cardLast4: '1234',
    note: ['https://shop.example.com/camera', '배송비 없음'],
  };

  test('tools carry their kind', async () => {
    const { tools } = await client.listTools();
    const kind = Object.fromEntries(tools.map((t) => [t.name, t._meta?.['xylolabs/kind']]));
    assert.deepEqual(kind, { draft_purchase: 'local', list_purchases: 'read', capture_quote: 'local', submit_purchase: 'write' });
    const submit = tools.find((t) => t.name === 'submit_purchase');
    assert.equal(submit.annotations.destructiveHint, true);
    assert.match(tools.find((t) => t.name === 'draft_purchase').description, /지어내지 말고 사람에게 묻는다/);
  });

  test('draft_purchase fills card defaults, keys the draft and lists what is missing', async () => {
    const r = await call('draft_purchase', DRAFT);
    assert.ok(!r.isError, text(r));
    assert.equal(r.structuredContent.key, 'p-20260115-1');
    assert.equal(r.structuredContent.revision, 1);
    assert.equal(r.structuredContent.fields.budget, '프로젝트 사업비');
    assert.equal(r.structuredContent.fields.project, '스마트농장');
    assert.equal(r.structuredContent.fields.cardIssuer, '신한');
    assert.equal(r.structuredContent.fields.payWhen, '당월');
    assert.deepEqual(r.structuredContent.errors, ['판매 페이지 캡처가 없다. capture_quote 로 찍기 전에는 올리지 않는다']);
    assert.match(text(r), /결재선: 합의 최담당 → 결재 김대표/);
    assert.match(text(r), /capture_quote/);
  });

  test('draft_purchase update: only given fields change, revision goes up, small amounts warn', async () => {
    const r = await call('draft_purchase', { key: 'p-20260115-1', amount: 21_000 });
    assert.equal(r.structuredContent.revision, 2);
    assert.equal(r.structuredContent.fields.vendor, '테스트상점');
    assert.ok(r.structuredContent.warnings.some((w) => /100,000원 미만/.test(w)));
    const back = await call('draft_purchase', { key: 'p-20260115-1', amount: 132_000 });
    assert.equal(back.structuredContent.revision, 3);
  });

  test('unpaid foreign draft pins today\'s rate once', async () => {
    const before = sessions.scratch;
    const r = await call('draft_purchase', { ...DRAFT, key: 'usd-item', amount: null, usd: 100, paidAt: null, purchaseDate: '2026-01-20' });
    assert.ok(!r.isError, text(r));
    assert.equal(r.structuredContent.fields.amountNumber, 140_000);
    assert.match(r.structuredContent.fields.note, /USD 100\.00 × 1,400\.00원/);
    assert.equal(sessions.scratch, before + 1);
    await call('draft_purchase', { key: 'usd-item', vendor: '테스트상점 (테스트몰)' });
    assert.equal(sessions.scratch, before + 1);   // pinned, also cached for the day
    const paid = await call('draft_purchase', { key: 'usd-item', paidKrw: 138_000, paidAt: '2026-01-21' });
    assert.equal(paid.structuredContent.fields.amountNumber, 138_000);
    assert.match(paid.structuredContent.fields.note, /카드 승인 적용환율/);
  });

  test('draft_purchase saves on top of what changed while the rate was fetched', async () => {
    const first = await call('draft_purchase', { ...DRAFT, key: 'race' });
    assert.ok(!first.isError, text(first));
    // save_photos writes a serial and the row is filed while draft_purchase waits for the rate.
    sessions.onScratch = () => {
      db.patchPurchaseData('hong', 'race', { serials: { '테스트 카메라': ['SN-9'] } });
      const row = db.purchase('hong', 'race');
      db.upsertPurchase({ ...row, status: 'submitted', docId: '1000000000000031' });
    };
    try {
      const r = await call('draft_purchase', { key: 'race', amount: null, usd: 100, refreshFx: true });
      assert.ok(!r.isError, text(r));
    } finally {
      sessions.onScratch = null;
    }
    const row = db.purchase('hong', 'race');
    assert.deepEqual(row.data.serials, { '테스트 카메라': ['SN-9'] });
    assert.equal(row.status, 'submitted');
    assert.equal(row.docId, '1000000000000031');
    assert.equal(row.usd, 100);
    assert.equal(row.data.revision, 2);
    db.upsertPurchase({ ...row, status: 'draft', docId: null });   // later tests expect no filed rows
  });

  test('capture_quote refuses an order page before opening anything', async () => {
    const before = sessions.scratch;
    const r = await call('capture_quote', { key: 'p-20260115-1', url: 'https://shop.example.com/my/orders/1', expect: ['테스트 카메라'] });
    assert.equal(r.isError, true);
    assert.equal(r.structuredContent.code, 'VALIDATION');
    assert.match(text(r), /주문내역이나 영수증/);
    assert.equal(sessions.scratch, before);
  });

  test('capture_quote needs expect with a URL, and refuses receipt-like uploads', async () => {
    const r = await call('capture_quote', { key: 'p-20260115-1', url: 'https://shop.example.com/p/1' });
    assert.equal(r.structuredContent.code, 'VALIDATION');
    const file = path.join(inbox, 'receipt.png');
    await sharp({ create: { width: 10, height: 10, channels: 3, background: '#fff' } }).png().toFile(file);
    const up = await call('capture_quote', { key: 'p-20260115-1' }, { ...CREDS, 'xylolabs/attachments': [{ name: '영수증.png', mimeType: 'image/png', path: file }] });
    assert.equal(up.structuredContent.code, 'VALIDATION');
    assert.match(text(up), /영수증이나 주문내역/);
  });

  test('capture_quote stores a person\'s screenshot as a stripped JPEG, returns it and bumps the revision', async () => {
    const file = path.join(inbox, 'shop-page.png');
    await sharp({ create: { width: 400, height: 300, channels: 3, background: '#88aaff' } }).png().toFile(file);
    const r = await call('capture_quote', { key: 'p-20260115-1', name: '테스트상점_카메라' }, {
      ...CREDS, 'xylolabs/attachments': [{ name: 'shop-page.png', mimeType: 'image/png', path: file }],
    });
    assert.ok(!r.isError, text(r));
    assert.equal(r.structuredContent.revision, 4);
    const [q] = r.structuredContent.quotes;
    assert.match(q.file, /^테스트상점_카메라-[0-9a-f]{8}\.jpg$/);
    assert.equal(q.manual, true);
    assert.equal(r.content[1].type, 'image');
    assert.equal(r.content[1].mimeType, 'image/jpeg');
    assert.ok(fs.existsSync(path.join(dataDir, 'files', 'hong', 'quotes', q.file)));
    assert.ok(fs.existsSync(file), 'the inbox file is left for the bot');
  });

  test('list_purchases shows drafts with keys, revisions and quote counts', async () => {
    const r = await call('list_purchases', {});
    assert.ok(!r.isError, text(r));
    const row = r.structuredContent.purchases.find((p) => p.key === 'p-20260115-1');
    assert.deepEqual({ status: row.status, revision: row.revision, quotes: row.quotes }, { status: 'draft', revision: 4, quotes: 1 });
    const one = await call('list_purchases', { key: 'p-20260115-1' });
    assert.match(text(one), /판 4/);
    const none = await call('list_purchases', { status: 'submitted' });
    assert.match(text(none), /없습니다/);
  });

  test('submit_purchase preview: field table, line, quote images; nothing reaches DaouOffice', async () => {
    const r = await call('submit_purchase', { key: 'p-20260115-1', revision: 4 });
    assert.ok(!r.isError, text(r));
    assert.equal(r.structuredContent.preview, true);
    assert.equal(r.structuredContent.digest, undefined);
    assert.match(text(r), /다음 구매 품의서를 상신합니다/);
    assert.match(text(r), /결재선: 기안 홍길동 \(연구개발팀\) → 합의 최담당 → 결재 김대표/);
    assert.equal(r.content.filter((c) => c.type === 'image').length, 1);
    assert.equal(sessions.sessionCalls.length, 0);
    assert.equal(db.purchase('hong', 'p-20260115-1').status, 'draft');
  });

  test('submit_purchase preview refuses a stale revision, validation errors, and fix with refile', async () => {
    const stale = await call('submit_purchase', { key: 'p-20260115-1', revision: 3 });
    assert.equal(stale.structuredContent.code, 'CONFLICT');
    const bad = await call('draft_purchase', { ...DRAFT, key: 'no-quote' });
    const noQuote = await call('submit_purchase', { key: 'no-quote', revision: bad.structuredContent.revision });
    assert.equal(noQuote.structuredContent.code, 'VALIDATION');
    assert.match(text(noQuote), /캡처가 없다/);
    const both = await call('submit_purchase', { key: 'p-20260115-1', revision: 4, fix: true, refile: true });
    assert.equal(both.structuredContent.code, 'VALIDATION');
    const missing = await call('submit_purchase', { key: 'nope', revision: 1 });
    assert.equal(missing.structuredContent.code, 'NOT_FOUND');
  });

  test('execute runs once per approved preview: a second approval of the same preview is refused', async () => {
    await call('submit_purchase', { key: 'p-20260115-1', revision: 4 });
    sessions.mode = 'run';
    try {
      const [a, b] = await Promise.all([
        call('submit_purchase', { key: 'p-20260115-1', revision: 4 }, EXECUTE),
        call('submit_purchase', { key: 'p-20260115-1', revision: 4 }, EXECUTE),
      ]);
      const texts = [text(a), text(b)].sort();
      assert.equal(texts.filter((t) => t.includes(STOP)).length, 1, texts.join(' | '));
      const other = [a, b].find((r) => !text(r).includes(STOP));
      assert.equal(other.structuredContent.code, 'CONFLICT');
      assert.deepEqual(sessions.sessionCalls.at(-1).mode, 'execute');
      // Without a new preview nothing runs again.
      const again = await call('submit_purchase', { key: 'p-20260115-1', revision: 4 }, EXECUTE);
      assert.equal(again.structuredContent.code, 'CONFLICT');
      assert.equal(again.structuredContent.data.previewChanged, true);
    } finally {
      sessions.mode = 'refuse';
    }
    assert.equal(db.purchase('hong', 'p-20260115-1').status, 'draft');
  });

  test('a changed draft or a changed quote needs a new preview', async () => {
    const calls = sessions.sessionCalls.length;
    await call('submit_purchase', { key: 'p-20260115-1', revision: 4 });
    const changed = await call('draft_purchase', { key: 'p-20260115-1', purpose: '온실 작물 생육 원격 관찰 기록' });
    const r1 = await call('submit_purchase', { key: 'p-20260115-1', revision: 4 }, EXECUTE);
    assert.equal(r1.structuredContent.code, 'CONFLICT');
    assert.match(text(r1), /초안 판이 다릅니다/);

    const rev = changed.structuredContent.revision;
    await call('submit_purchase', { key: 'p-20260115-1', revision: rev });
    const q = db.quotes('hong', db.purchase('hong', 'p-20260115-1').id)[0];
    const file = path.join(dataDir, 'files', 'hong', 'quotes', q.file);
    const original = fs.readFileSync(file);
    fs.writeFileSync(file, 'tampered');
    const r3 = await call('submit_purchase', { key: 'p-20260115-1', revision: rev }, EXECUTE);
    assert.equal(r3.structuredContent.code, 'VALIDATION');
    assert.match(text(r3), /기록과 다릅니다/);
    fs.writeFileSync(file, original);
    assert.equal(sessions.sessionCalls.length, calls, 'nothing logged in to DaouOffice');
  });

  test('refile: when the old document changes after the preview, execute refuses instead of re-planning', async () => {
    const OLD = '1000000000000007';
    const row = db.purchase('hong', 'p-20260115-1');
    db.upsertPurchase({ ...row, status: 'submitted', docId: OLD });
    const rev = row.data.revision;
    sessions.docs[OLD] = { status: '진행중', canCancel: true };
    sessions.mode = 'run';
    try {
      const preview = await call('submit_purchase', { key: 'p-20260115-1', revision: rev, refile: true });
      assert.ok(!preview.isError, text(preview));
      assert.equal(preview.structuredContent.plan.withdraw, OLD);
      assert.match(text(preview), /상신취소하고 같은 문서를 새 결재선으로/);
      // An approver completes the old document before the person approves.
      sessions.docs[OLD] = { status: '완료', docNum: '테스트-2026-0001' };
      const r = await call('submit_purchase', { key: 'p-20260115-1', revision: rev, refile: true }, EXECUTE);
      assert.equal(r.structuredContent.code, 'CONFLICT', text(r));
      assert.equal(r.structuredContent.data.previewChanged, true);
      assert.doesNotMatch(text(r), new RegExp(STOP));

      // A fresh preview of the new plan (new document linked to the old one) runs.
      const again = await call('submit_purchase', { key: 'p-20260115-1', revision: rev, refile: true });
      assert.deepEqual(again.structuredContent.plan.links, [{ id: OLD, docNum: '테스트-2026-0001' }]);
      const go = await call('submit_purchase', { key: 'p-20260115-1', revision: rev, refile: true }, EXECUTE);
      assert.match(text(go), new RegExp(STOP));
    } finally {
      sessions.mode = 'refuse';
      db.upsertPurchase({ ...db.purchase('hong', 'p-20260115-1'), status: 'draft', docId: null });
    }
  });

  test('fix or refile of a revision that was already filed is refused', async () => {
    const row = db.purchase('hong', 'p-20260115-1');
    db.upsertPurchase({ ...row, status: 'submitted', docId: '1000000000000008', data: { ...row.data, filedRevision: row.data.revision } });
    for (const opt of [{ fix: true }, { refile: true }]) {
      const r = await call('submit_purchase', { key: 'p-20260115-1', revision: row.data.revision, ...opt });
      assert.equal(r.structuredContent.code, 'CONFLICT');
      assert.match(text(r), /이미 올렸습니다/);
    }
    db.upsertPurchase({ ...row });
  });

  test('a submitted requisition is not filed twice without fix or refile', async () => {
    const row = db.purchase('hong', 'usd-item');
    db.upsertPurchase({ ...row, status: 'submitted', docId: '1000000000000009' });
    const r = await call('submit_purchase', { key: 'usd-item', revision: row.data.revision });
    assert.equal(r.structuredContent.code, 'CONFLICT');
    assert.match(text(r), /이미 올린 품의/);
  });
});
