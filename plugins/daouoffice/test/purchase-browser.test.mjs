import { describe, test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import http from 'node:http';
import { Writable } from 'node:stream';
import { McpServer, InMemoryTransport } from '@modelcontextprotocol/server';
import { Client } from '@modelcontextprotocol/client';
import { parseConfig } from '../src/config.mjs';
import { openDb } from '../src/db.mjs';
import { SessionManager } from '../src/sessions.mjs';
import { createRuntime, createLogger } from '../src/runtime.mjs';
import * as purchaseTools from '../src/tools/purchase.mjs';
import { fetchUsdKrw } from '../src/dao/fx.mjs';
import { formValues } from '../src/dao/purchases.mjs';
import { captureQuote, captureContextOptions } from '../src/dao/quotes.mjs';
import { withGuardedContext } from '../src/dao/egress.mjs';
import { parsePurchaseBody, LABELS, diffPurchaseDoc } from '../src/dao/purchase-doc.mjs';
import { fillFormInPage, readFormInPage, compareForm } from '../src/dao/purchase-submit.mjs';

/**
 * Runs the capture checks and the form fill/read code in real Chromium against local pages.
 * Nothing here contacts a shop or DaouOffice. Skips when Chromium is not installed.
 */

const page = (title, body, extra = '') => `<!doctype html><html lang="ko"><head><meta charset="utf-8"><title>${title}</title>${extra}</head><body>${body}</body></html>`;

const PRODUCT = page('테스트 카메라 : 테스트상점', `
  <h1>테스트 카메라</h1><p>판매자 테스트상점</p><p>132,000원</p>
  <div><button class="opt">흰색</button><button class="opt">검정</button></div>
  <a href="/login">로그인</a>
  <script>for (const b of document.querySelectorAll('.opt')) b.onclick = () => { b.classList.add('selected'); };</script>`);

const SITES = {
  '/product': [200, PRODUCT],
  '/login': [200, page('로그인', '<form><input name="id"><input type="password"></form>')],
  '/captcha': [200, page('보안 확인', '<p>Please complete the security verification</p>')],
  '/blocked': [403, page('Access Denied', '<h1>Access Denied</h1>')],
  '/gone': [404, page('없음', '<p>페이지를 찾을 수 없습니다</p>')],
  '/fx': [200, page('환율', '<p>하나은행 고시회차 100회</p><p>매매기준율 기준 미국 USD 1 달러 대한민국 KRW 1,385.20 원</p><p>2026. 01. 15. 11:00 기준</p>')],
  '/covered': [200, page('테스트 카메라 : 테스트상점', `<h1>테스트 카메라</h1><p>테스트상점</p>
    <div style="position:fixed;inset:0;background:rgba(0,0,0,.6);color:#fff">오늘만 쿠폰 받기 이벤트</div>`)],
  '/closable': [200, page('테스트 카메라 : 테스트상점', `<h1>테스트 카메라</h1><p>테스트상점</p>
    <div id="pop" style="position:fixed;inset:0;background:rgba(0,0,0,.6)">쿠폰 받기
      <button aria-label="닫기" onclick="document.getElementById('pop').remove()">x</button></div>`)],
};

function server() {
  const s = http.createServer((req, res) => {
    const url = new URL(req.url, 'http://x');
    const redirects = {
      '/redirect-login': '/login?next=%2Fproduct',
      '/to-internal': `http://internal.test:${s.address().port}/product`,
      '/to-metadata': 'http://169.254.169.254/latest/meta-data/',
    };
    if (redirects[url.pathname]) {
      res.writeHead(302, { Location: redirects[url.pathname] });
      res.end();
      return;
    }
    const [status, body] = SITES[url.pathname] ?? [404, page('404', 'not found')];
    res.writeHead(status, { 'Content-Type': 'text/html; charset=utf-8' });
    res.end(body);
  });
  return s;
}

const cfg = parseConfig({
  people: { ceo: '김대표', cfo: '이재무' },
  projects: { 스마트농장: { reviewer: '최담당', subProjects: ['온실', '드론'] }, 해양로봇: { reviewer: '정담당' } },
});

/** A purchase form shaped like DaouOffice's: label cells, then the input cell. */
function formHtml({ oldForm = false, attachments = [] } = {}) {
  const row = (label, cell) => `<tr><td>${label}</td><td>${cell}</td></tr>`;
  const radios = (name, values) => values.map((v) => `<label><input type="radio" name="${name}" value="${v}">${v}</label>`).join('');
  const select = (values) => `<select>${['선택', ...values].map((v, i) => `<option value="v${i}">${v}</option>`).join('')}</select>`;
  return page('구매 품의서', `
    <input id="subject" type="text">
    <table>
      ${row('예 산', `${radios('budget', ['운영자금', '프로젝트 사업비'])} ${select(['스마트농장', '해양로봇'])} ${oldForm ? '' : radios('sub', ['온실', '드론'])}`)}
      ${row('구매품명', '<input type="text">')}
      ${row('업체명', '<input type="text">')}
      ${oldForm ? row('배송일자', '<input type="text">') : `${row('구입수량', '<input type="text">')}${row('구입일자', '<input type="text">')}`}
      ${row('구입목적', '<textarea></textarea>')}
      ${oldForm ? row('금액(vat포함)', '<input type="text">') : row('총금액(vat포함)', '<input type="text"><input type="text"><input type="text">')}
      ${row('지급요청일', radios('when', ['당월', '익월']))}
      ${row('지급방법', `${select(['세금계산서', '법인카드', '현금지급', '기타/사업소득 지급'])}<input type="text">${select(['연구비카드', 'KB', '신한', 'BNK'])}`)}
      ${row('지급계좌(은행)', '<input type="text">')}
      ${row('예금주', '<input type="text">')}
      ${row('기타(링크)', '<textarea></textarea>')}
    </table>
    <table><tr><th>파일첨부</th><td><ul id="fileWrap">${attachments.map((n) => `<li data-name="${n}">${n}</li>`).join('')}</ul></td></tr></table>`);
}

const entry = {
  key: 'k', title: '테스트 카메라 구매의 건', budget: '프로젝트 사업비', project: '스마트농장', subProject: '드론',
  item: '테스트 카메라 / 1개, 테스트 렌즈 / 2개', vendor: '테스트상점 (테스트몰)', purpose: '온실 작물 생육 원격 관찰',
  paidAt: '2026-01-15', deliveryDate: '2026-01-16', amount: 132_000, payWhen: '익월', payMethod: '법인카드', cardIssuer: 'KB', cardLast4: '1234',
  note: ['https://shop.example.com/camera', '본체 100,000원, 렌즈 32,000원'], quotes: [{ file: 'shop-1a2b3c4d.png', url: 'https://shop.example.com/camera' }],
};

describe('in Chromium', () => {
  let browser = null;
  let site;
  let base;

  before(async () => {
    process.env.DAOUOFFICE_QUOTE_ALLOW_PRIVATE = '1';   // the test pages live on 127.0.0.1
    try {
      const { chromium } = await import('playwright');
      browser = await chromium.launch({ headless: true });
    } catch {
      browser = null;
    }
    site = server();
    await new Promise((r) => site.listen(0, '127.0.0.1', r));
    base = `http://127.0.0.1:${site.address().port}`;
  });
  after(async () => {
    delete process.env.DAOUOFFICE_QUOTE_ALLOW_PRIVATE;
    await browser?.close();
    site.close();
  });

  const withPage = async (t, fn) => {
    if (!browser) return t.skip('chromium is not installed');
    const context = await browser.newContext(captureContextOptions(browser));
    try {
      return await fn(await context.newPage());
    } finally {
      await context.close();
    }
  };
  /** The shared-browser seam (`withScratchContext`) over this test's browser. */
  const sessions = {
    async withScratchContext(fn, { contextOptions } = {}) {
      const context = await browser.newContext(typeof contextOptions === 'function' ? await contextOptions(browser) : contextOptions);
      try {
        return await fn({ context, page: await context.newPage(), browser });
      } finally {
        await context.close();
      }
    },
  };
  /** Captures through the guard proxy. `policy` overrides the test switch (lookup, allowAddresses, allowPrivate). */
  const shoot = async (t, pathname, over = {}, policy = {}) => {
    if (!browser) return t.skip('chromium is not installed');
    return withGuardedContext(sessions, ({ page: p, guard }) => captureQuote(p, {
      url: `${over.host ?? base}${pathname}`, expect: ['테스트 카메라', '테스트상점'], waitMs: 100, ...over,
    }, { guard }), { contextOptions: captureContextOptions, ...policy });
  };
  const lookup = async (host) => {
    const a = { 'shop.test': '127.0.0.1', 'internal.test': '10.0.0.5' }[host];
    if (!a) throw Object.assign(new Error('ENOTFOUND'), { code: 'ENOTFOUND' });
    return [{ address: a, family: 4 }];
  };

  test('capture context hides the headless marker in the user agent', async (t) => {
    await withPage(t, async (p) => {
      const ua = await p.evaluate(() => navigator.userAgent);
      assert.doesNotMatch(ua, /Headless/);
    });
  });

  test('a product page is captured as PNG after picking the ordered option', async (t) => {
    const r = await shoot(t, '/product', { options: ['검정'] });
    if (!r) return;
    assert.equal(r.ok, true, r.reason);
    assert.equal(r.shot.subarray(1, 4).toString(), 'PNG');
    assert.equal(r.priceSeen, true);
  });

  test('refusals: option, expect, login, captcha, block, missing, overlay, order page', async (t) => {
    const cases = [
      ['/product', { options: ['빨강'] }, 'option'],
      ['/product', { expect: ['다른상점'] }, 'expect'],
      ['/redirect-login', {}, 'login'],
      ['/captcha', {}, 'captcha'],
      ['/blocked', {}, 'blocked'],
      ['/gone', {}, 'missing'],
      ['/covered', {}, 'overlay'],
      ['/my/orders/1', {}, 'receipt'],
      ['/product', { expect: [] }, 'input'],
    ];
    for (const [pathname, over, kind] of cases) {
      const r = await shoot(t, pathname, over);
      if (!r) return;
      assert.equal(r.ok, false, pathname);
      assert.equal(r.kind, kind, `${pathname}: ${r.reason}`);
      assert.ok(r.reason, pathname);
    }
  });

  test('without the test switch a loopback page is refused before it is opened', async (t) => {
    delete process.env.DAOUOFFICE_QUOTE_ALLOW_PRIVATE;
    try {
      const r = await shoot(t, '/product');
      if (!r) return;
      assert.equal(r.ok, false);
      assert.equal(r.kind, 'private');
      assert.match(r.reason, /공개된 상품 페이지만/);
    } finally {
      process.env.DAOUOFFICE_QUOTE_ALLOW_PRIVATE = '1';
    }
  });

  test('the browser never resolves names: a name only the proxy knows still loads', async (t) => {
    const port = new URL(base).port;
    const r = await shoot(t, '/product', { host: `http://shop.test:${port}` }, { lookup, allowPrivate: false, allowAddresses: ['127.0.0.1'] });
    if (!r) return;
    assert.equal(r.ok, true, r.reason);
    assert.equal(new URL(r.landedUrl).hostname, 'shop.test');
  });

  test('redirects to internal hosts and cloud metadata are blocked by the proxy', async (t) => {
    const port = new URL(base).port;
    for (const pathname of ['/to-internal', '/to-metadata']) {
      const r = await shoot(t, pathname, { host: `http://shop.test:${port}` }, { lookup, allowPrivate: false, allowAddresses: ['127.0.0.1'] });
      if (!r) return;
      assert.equal(r.ok, false, pathname);
      assert.equal(r.kind, 'private', `${pathname}: ${r.reason}`);
    }
  });

  test('a capture without the guard proxy is refused', async (t) => {
    await withPage(t, async (p) => {
      await assert.rejects(captureQuote(p, { url: `${base}/product`, expect: ['테스트'] }), (e) => e.code === 'INTERNAL');
    });
  });

  test('a popup with a close button is closed before the shot', async (t) => {
    const r = await shoot(t, '/closable');
    if (!r) return;
    assert.equal(r.ok, true, r.reason);
  });

  test('fill and read back: new form, every field matches formValues', async (t) => {
    await withPage(t, async (p) => {
      await p.setContent(formHtml({ attachments: ['shop-1a2b3c4d.png'] }));
      const want = formValues(entry, { config: cfg });
      assert.deepEqual(await p.evaluate(fillFormInPage, want), []);
      const got = await p.evaluate(readFormInPage);
      assert.equal(got.newForm, true);
      assert.equal(got.subProject, '드론');
      assert.equal(got.quantity, '3개');
      assert.equal(got.cardIssuer, 'KB');
      assert.deepEqual(compareForm(got, want), []);
    });
  });

  test('fill and read back: old form fills 배송일자 and the single amount', async (t) => {
    await withPage(t, async (p) => {
      await p.setContent(formHtml({ oldForm: true, attachments: ['shop-1a2b3c4d.png', 'stale.png'] }));
      const want = formValues(entry, { config: cfg });
      assert.deepEqual(await p.evaluate(fillFormInPage, want), []);
      const got = await p.evaluate(readFormInPage);
      assert.equal(got.newForm, false);
      assert.equal(got.deliveryDate, '2026-01-16(금)');
      assert.equal(got.amount, '132,000');
      assert.deepEqual(compareForm(got, want).map((d) => `${d.label}:${d.got}`), ['첨부:stale.png']);
    });
  });

  test('fill reports a missing option instead of choosing another', async (t) => {
    await withPage(t, async (p) => {
      await p.setContent(formHtml());
      const want = formValues({ ...entry, project: '해양로봇', cardIssuer: '없는카드' }, { config: cfg });
      assert.deepEqual(await p.evaluate(fillFormInPage, want), ['옵션없음 지급방법: 없는카드']);
    });
  });

  test('the stored body (span[data-type]) is parsed and compared field by field', async (t) => {
    await withPage(t, async (p) => {
      const want = formValues(entry, { config: cfg });
      const span = (type, value, extra = '') => `<span data-type="${type}" data-value="${value}" ${extra}>${value}</span>`;
      const html = `<table>
        <tr><td>예 산</td><td>${span('radio', '운영자금')}${span('radio', '프로젝트 사업비', 'data-select-option="true"')}
          <span data-type="cSel" data-selectval="스마트농장" value="해양로봇"></span>${span('radio', '드론', 'data-select-option="true"')}</td></tr>
        <tr><td>구매품명</td><td>${span('text', want.item)}</td></tr>
        <tr><td>업체명</td><td>${span('text', want.vendor)}</td></tr>
        <tr><td>구입수량</td><td>${span('text', want.quantity)}</td></tr>
        <tr><td>구입일자</td><td>${span('calendar', want.purchaseDate)}</td></tr>
        <tr><td>구입목적</td><td>${span('textarea', want.purpose)}</td></tr>
        <tr><td>총금액(vat포함)</td><td>${span('text', want.amount)}${span('text', want.supply)}${span('text', want.vat)}</td></tr>
        <tr><td>지급요청일</td><td>${span('radio', '당월')}${span('radio', '익월', 'data-select-option="true"')}</td></tr>
        <tr><td>지급방법</td><td><span data-type="cSel" data-selectval="법인카드"></span>${span('text', '1234')}<span data-type="cSel" data-selectval="KB"></span></td></tr>
        <tr><td>지급계좌(은행)</td><td>${span('text', '해당없음')}</td></tr>
        <tr><td>예금주</td><td>${span('text', '해당없음')}</td></tr>
        <tr><td>기타(링크)</td><td>${span('textarea', want.note.replace(/\n/g, '&#10;'))}</td></tr>
        <img src="x" onerror="document.title='ran'">
      </table>`;
      await p.setContent(page('doc', ''));
      const fields = await p.evaluate(parsePurchaseBody, { html, labels: LABELS });
      assert.equal(await p.title(), 'doc');   // the body is parsed, never run
      assert.equal(fields['배송일자'], null);
      assert.deepEqual(fields['예산'], [{ type: 'radio', value: '프로젝트 사업비' }, { type: 'select', value: '스마트농장' }, { type: 'radio', value: '드론' }]);
      const actual = { title: want.title, fields, attachments: ['shop-1a2b3c4d.png'] };
      assert.deepEqual(diffPurchaseDoc(actual, want), []);
    });
  });
});

describe('capture_quote through the shared browser', () => {
  let chromium = null;
  let site;
  let base;
  let sessions;
  let client;
  let db;
  let dataDir;

  before(async () => {
    process.env.DAOUOFFICE_QUOTE_ALLOW_PRIVATE = '1';
    try {
      ({ chromium } = await import('playwright'));
      const b = await chromium.launch({ headless: true });
      await b.close();
    } catch {
      chromium = null;
      return;
    }
    site = server();
    await new Promise((r) => site.listen(0, '127.0.0.1', r));
    base = `http://127.0.0.1:${site.address().port}`;
    dataDir = fs.mkdtempSync(path.join(os.tmpdir(), 'capture-'));
    db = openDb(':memory:');
    sessions = new SessionManager({ launch: () => chromium.launch({ headless: true }) });
    const mcp = new McpServer({ name: 'daouoffice', version: '0.0.0' }, { capabilities: { tools: {} } });
    const sink = new Writable({ write(_c, _e, cb) { cb(); } });
    const rt = createRuntime({ config: cfg, db, sessions, dataDir, logger: createLogger({ stream: sink }) });
    purchaseTools.register(mcp, rt);
    const [a, b] = InMemoryTransport.createLinkedPair();
    await mcp.connect(a);
    client = new Client({ name: 't', version: '0' });
    await client.connect(b);
  });
  after(async () => {
    delete process.env.DAOUOFFICE_QUOTE_ALLOW_PRIVATE;
    if (!chromium) return;
    await client.close();
    await sessions.close();
    db.close();
    site.close();
    fs.rmSync(dataDir, { recursive: true, force: true });
  });

  const META = { 'xylolabs/credentials': { username: 'hong', password: 'pw' } };

  test('captures, stores and returns the page; a blocked page is refused with a reason', async (t) => {
    if (!chromium) return t.skip('chromium is not installed');
    const draft = await client.callTool({ name: 'draft_purchase', arguments: { key: 'cam', title: '테스트 카메라 구매의 건', item: '테스트 카메라 / 1개' }, _meta: META });
    assert.ok(!draft.isError, draft.content[0].text);
    const r = await client.callTool({
      name: 'capture_quote',
      arguments: { key: 'cam', url: `${base}/product`, expect: ['테스트 카메라', '테스트상점'], options: ['검정'], waitMs: 1000, name: '테스트상점_카메라' },
      _meta: META,
    });
    assert.ok(!r.isError, r.content[0].text);
    assert.equal(r.content[1].type, 'image');
    assert.equal(r.content[1].mimeType, 'image/png');
    const [q] = r.structuredContent.quotes;
    assert.equal(q.url, `${base}/product`);
    assert.ok(fs.existsSync(path.join(dataDir, 'files', 'hong', 'quotes', q.file)));
    assert.equal(sessions.stats().contexts, 0, 'the scratch context is closed');

    const blocked = await client.callTool({ name: 'capture_quote', arguments: { key: 'cam', url: `${base}/blocked`, expect: ['테스트'], waitMs: 1000 }, _meta: META });
    assert.equal(blocked.structuredContent.code, 'VALIDATION');
    assert.match(blocked.content[0].text, /막았습니다/);
  });

  test('the exchange rate page is read in a scratch context', async (t) => {
    if (!chromium) return t.skip('chromium is not installed');
    const fx = await withGuardedContext(sessions, ({ page }) => fetchUsdKrw(page, { url: `${base}/fx` }));
    assert.deepEqual(fx, { rate: 1385.2, at: '2026-01-15 11:00', source: '하나은행 매매기준율 (고시 100회차)' });
  });
});
