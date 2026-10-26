import { describe, test } from 'node:test';
import assert from 'node:assert/strict';
import { PluginError } from '../src/errors.mjs';
import { runWithMode } from '../src/guard.mjs';
import { parseConfig } from '../src/config.mjs';
import { formValues } from '../src/dao/purchases.mjs';
import {
  submitPurchase, failureAfter, draftedAtMs, pickSubmittedDocId, fillFormInPage, readFormInPage,
} from '../src/dao/purchase-submit.mjs';

/**
 * The submit flow on a fake page. The approval line editor, the form tree and the withdraw dialog
 * are replaced through `deps`; everything else (fill, compare, the 결재요청 popup, the doc-id lookup,
 * the server readback) runs the real code against canned answers.
 */

const cfg = parseConfig({ projects: { 스마트농장: { reviewer: '최담당', subProjects: ['온실'] } } });
const entry = {
  key: 'k', title: '테스트 카메라 구매의 건', budget: '프로젝트 사업비', project: '스마트농장', subProject: '온실',
  item: '테스트 카메라 / 1개', vendor: '테스트상점', purpose: '온실 작물 생육 원격 관찰', paidAt: '2026-01-15',
  amount: 132_000, payWhen: '당월', payMethod: '법인카드', cardIssuer: '신한', cardLast4: '1234',
  note: ['https://shop.example.com/camera'], quotes: [{ file: 'shop-1a2b3c4d.png', url: 'https://shop.example.com/camera' }],
};
const want = formValues(entry, { config: cfg });
const approvers = [{ name: '최담당', type: '합의' }, { name: '김대표', type: '결재' }];
const NEW_DOC = '1000000000000021';
const OLD_DOC = '1000000000000020';
const NOW = Date.parse('2026-01-15T03:00:00Z');   // 12:00 KST

/** What the form shows after filling: the expected values, or `over` to make the screen differ. */
const screen = (over = {}) => ({
  ...want, newForm: true, filesText: want.attachments.join(' '), fileNames: [...want.attachments], ...over,
});

function fakeDaou({ onScreen = screen(), list = [], docStatus = '진행중', readFails = false, url = 'https://x/gw/app/approval/document/new', popup = 'ap', attachments } = {}) {
  const calls = { clicks: 0, evaluates: 0 };
  const page = {
    on() {}, off() {},
    url: () => url,
    async goto() {}, async waitForLoadState() {}, async waitForTimeout() {}, async waitForSelector() {},
    keyboard: { press: async () => {} },
    locator: () => ({ count: async () => 0, first: () => ({ setInputFiles: async () => {}, count: async () => 0, click: async () => {} }) }),
    async evaluate(fn) {
      calls.evaluates++;
      if (fn === fillFormInPage) return [];
      if (fn === readFormInPage) return onScreen;
      if (typeof fn === 'function' && /act_draft/.test(String(fn))) return undefined;
      return { 구입일자: [] };   // parsePurchaseBody of the server document: fields checked below
    },
    async waitForFunction(fn) {
      if (/layer_approval/.test(String(fn))) return { jsonValue: async () => popup };
      return true;
    },
    async screenshot() { return Buffer.from('jpeg'); },
    async click() { calls.clicks++; },
  };
  const api = async (p) => {
    if (p.startsWith('/gw/api/approval/doclist/draft/all')) return { status: 200, ok: true, data: { data: list } };
    const id = p.match(/document\/(\d+)$/)?.[1];
    if (readFails && id === NEW_DOC) return { status: 500, ok: false, data: null };
    return {
      status: 200, ok: true,
      data: { data: {
        document: { title: want.title, formName: '구매 품의서', docStatusName: id === OLD_DOC ? '임시저장' : docStatus, attaches: (attachments ?? want.attachments).map((name) => ({ name })) },
        apprFlow: { activityGroups: [{ name: '승인', activities: ['홍길동', '최담당', '김대표'].map((userName) => ({ userName })) }] },
      } },
    };
  };
  return { page, api, calls };
}

const deps = {
  findFormBy: async () => ({ id: '42', title: '구매 품의서' }),
  setLine: async () => '합의 최담당 → 결재 김대표',
  link: async (_p, links) => links.map((d) => d.id),
  withdraw: async () => '임시저장',
};
const run = (daou, { plan = { editId: null, withdraw: null, links: [], refileFrom: null }, hooks = {}, state = {} } = {}) => runWithMode('execute', () => submitPurchase({
  page: daou.page, api: daou.api, baseUrl: 'https://x', deptId: '10', want, approvers, plan, quoteDir: '/tmp',
  formName: '구매 품의서', now: () => NOW, deps, ...hooks,
}), state);

describe('doc id after submitting', () => {
  test('draftedAt without a zone is KST; with a zone it is taken as is', () => {
    assert.equal(draftedAtMs('2026-01-15 12:00'), Date.parse('2026-01-15T03:00:00Z'));
    assert.equal(draftedAtMs('2026-01-15 12:00:30'), Date.parse('2026-01-15T03:00:30Z'));
    assert.equal(draftedAtMs('2026-01-15T12:00:00+09:00'), Date.parse('2026-01-15T03:00:00Z'));
    assert.equal(draftedAtMs('2026-01-15T03:00:00Z'), Date.parse('2026-01-15T03:00:00Z'));
    assert.ok(Number.isNaN(draftedAtMs('어제')));
  });

  test('only a fresh, non-draft document with the same form and title, other than the old one', () => {
    const row = (over) => ({ formName: '구매 품의서', title: want.title, docStatusName: '진행중', documentId: NEW_DOC, draftedAt: '2026-01-15 11:58', ...over });
    const pick = (list) => pickSubmittedDocId(list, { formName: '구매 품의서', title: want.title, exclude: OLD_DOC, now: NOW });
    assert.equal(pick([row()]), NEW_DOC);
    assert.equal(pick([row({ draftedAt: '2026-01-15 11:49' })]), null, 'older than 10 minutes');
    assert.equal(pick([row({ draftedAt: '2026-01-15 12:03' })]), null, 'in the future beyond the skew');
    assert.equal(pick([row({ draftedAt: '2026-01-15 12:00:40' })]), NEW_DOC, 'a slightly fast clock');
    assert.equal(pick([row({ documentId: OLD_DOC })]), null);
    assert.equal(pick([row({ docStatusName: '임시저장' })]), null);
    assert.equal(pick([row({ title: '다른 제목' })]), null);
    // A UTC host reading a naive KST time as UTC would see it 9 hours ahead and let old documents through.
    assert.equal(pick([row({ draftedAt: '2026-01-15 03:00' })]), null);
  });
});

describe('submit failures', () => {
  test('a submit that goes through returns the server comparison', async () => {
    const list = [{ formName: '구매 품의서', title: want.title, docStatusName: '진행중', documentId: NEW_DOC, draftedAt: '2026-01-15 11:59' }];
    const submitted = [];
    const r = await run(fakeDaou({ list }), { hooks: { onSubmitted: (x) => submitted.push(x.docId) } });
    assert.equal(r.docId, NEW_DOC);
    assert.deepEqual(submitted, [null, NEW_DOC]);
    assert.equal(r.status, '진행중');
  });

  test('a lost doc id after the confirm click is an unknown outcome, never "not submitted"', async () => {
    const daou = fakeDaou({ list: [] });
    const submitted = [];
    await assert.rejects(run(daou, { hooks: { onSubmitted: (x) => submitted.push(x.docId) } }), (e) => {
      assert.equal(e.code, 'UPSTREAM');
      assert.equal(e.data.afterWrite, true);
      assert.equal(e.data.docId, null);
      assert.match(e.message, /결재요청을 누른 뒤 결과를 확인하지 못했습니다/);
      assert.match(e.message, /올라갔을 수 있으니/);
      return true;
    });
    assert.equal(daou.calls.clicks, 1);
    assert.deepEqual(submitted, [null], 'recorded as submitted at the click');
  });

  test('a failed readback after the doc id is known keeps the id', async () => {
    const list = [{ formName: '구매 품의서', title: want.title, docStatusName: '진행중', documentId: NEW_DOC, draftedAt: '2026-01-15 11:59' }];
    await assert.rejects(run(fakeDaou({ list, readFails: true })), (e) => e.code === 'UPSTREAM' && e.data.afterWrite === true && e.data.docId === NEW_DOC);
  });

  test('after a withdraw, a failure before submitting says the document is withdrawn and records it', async () => {
    const plan = { editId: OLD_DOC, withdraw: OLD_DOC, links: [], refileFrom: null };
    const withdrawn = [];
    const daou = fakeDaou({ onScreen: screen({ vendor: '다른상점' }) });
    await assert.rejects(run(daou, { plan, hooks: { onWithdrawn: (x) => withdrawn.push(x.docId) } }), (e) => {
      assert.equal(e.code, 'UPSTREAM');
      assert.equal(e.data.withdrew, true);
      assert.equal(e.data.docId, OLD_DOC);
      assert.match(e.message, /화면에 채운 값이 기대와 달라/);
      assert.match(e.message, new RegExp(`문서는 상신취소된 상태입니다 \\(문서 ${OLD_DOC}\\)`));
      return true;
    });
    assert.deepEqual(withdrawn, [OLD_DOC]);
    assert.equal(daou.calls.clicks, 0, 'never reached the confirm button');
  });

  test('failures before the confirm click are not marked as a started write', async () => {
    // The no-approver warning appears instead of the confirm popup: nothing was sent.
    const state = {};
    await assert.rejects(run(fakeDaou({ popup: 'warn' }), { state }), (e) => e.code === 'UPSTREAM' && /결재자가 없다는 경고/.test(e.message) && !e.data);
    assert.deepEqual([state.wrote, state.sent], [false, false]);
    // A stale attachment removed from the draft, then a screen mismatch: still nothing written.
    const stale = fakeDaou({ onScreen: screen({ vendor: '다른상점' }), attachments: ['old.png'] });
    stale.page.evaluate = (orig => async (fn, arg) => (typeof fn === 'function' && /ic_del/.test(String(fn)) ? true : orig(fn, arg)))(stale.page.evaluate);
    const s2 = {};
    await assert.rejects(run(stale, { plan: { editId: OLD_DOC, withdraw: null, links: [], refileFrom: null }, state: s2 }), (e) => e.code === 'UPSTREAM' && !e.data);
    assert.deepEqual([s2.wrote, s2.sent], [false, false]);
  });

  test('a failure before any write is passed through unchanged', async () => {
    const e = new PluginError('VALIDATION', '고칠 것');
    assert.equal(failureAfter(e, {}), e);
    await assert.rejects(run(fakeDaou({ onScreen: screen({ purpose: '다른 목적' }) })), (err) => err.code === 'UPSTREAM' && !err.data);
  });

  test('outside an approved execute nothing runs', async () => {
    await assert.rejects(runWithMode('preview', () => submitPurchase({ page: fakeDaou().page, plan: {}, want })), (e) => e.code === 'INTERNAL');
  });
});
