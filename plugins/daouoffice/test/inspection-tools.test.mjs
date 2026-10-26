import { test, describe, before, after } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import http from 'node:http';
import { Writable } from 'node:stream';
import sharp from 'sharp';
import { PDFDocument, PDFName } from 'pdf-lib';
import { McpServer, InMemoryTransport } from '@modelcontextprotocol/server';
import { Client } from '@modelcontextprotocol/client';
import { createRuntime, createLogger } from '../src/runtime.mjs';
import { openDb } from '../src/db.mjs';
import { parseConfig } from '../src/config.mjs';
import { makeApi } from '../src/dao/api.mjs';
import { jpegSegments } from '../src/photos.mjs';
import { kstIso } from '../src/dao/kst.mjs';
import { registerAll } from '../src/tools/index.mjs';

/**
 * The inspection tools against a local stand-in for DaouOffice: the card ledger, the document APIs,
 * and, for the write tools, the 검수조서 draft page, the document page (withdraw, comments) built
 * from what the ported DOM code expects. Everything here is made up; nothing talks to DaouOffice.
 */

const CLAIM = '1000000000000099';
const LINES = ['프로젝트명', '결제방식', '결제 카드번호', '거래처명', '사업자등록번호', '(은행)계좌번호', '총 금액(VAT포함)', '입고일자', '비고'];

function fakeDaou() {
  const state = { docs: new Map(), comments: new Map(), seq: 2000000000000000 };
  state.docs.set('1000000000000011', { title: '테스트 카메라 외 1종 구매의 건', docNum: 'TEST-11', drafter: '홍길동', status: '완료', form: '구매 품의서' });
  state.docs.set(CLAIM, { title: '2026년 3월 (1234/rnd/홍길동) 법인카드 지출의 건', docNum: 'TEST-99', drafter: '홍길동', status: '진행중', form: '법인카드 지출품의서' });
  state.docs.set('1000000000000013', { title: 'A 구매의 건', docNum: 'TEST-13', drafter: '홍길동', status: '완료', form: '구매 품의서' });
  state.docs.set('1000000000000014', { title: 'B 구매의 건', docNum: 'TEST-14', drafter: '홍길동', status: '완료', form: '구매 품의서' });

  const html = (body) => `<!doctype html><meta charset="utf-8"><style>
    table{border-collapse:collapse} td,th{border:1px solid #ccc;padding:2px;width:120px} input,select,textarea{width:110px;box-sizing:border-box}
    .go_popup{display:none} .go_popup.show{display:block}</style><body>${body}</body>`;
  const esc = (s) => String(s).replace(/&/g, '&amp;').replace(/"/g, '&quot;').replace(/</g, '&lt;');
  const cell = (cls = 'v') => `<td><input class="${cls}"></td>`;
  const draft = html(`
    <div><input id="subject"></div>
    <table id="head">${LINES.map((l) => `<tr><td>${l}</td><td>${l === '결제방식'
      ? '<select class="v"><option>선택</option><option>법인카드</option><option>세금계산서</option><option>기타</option></select>'
      : l === '비고' ? '<textarea class="v"></textarea>' : '<input class="v">'}</td></tr>`).join('')}</table>
    <table id="detail">
      <tr><th>번호</th><th>품명</th><th>거래처명</th><th>수량</th><th>공급가액</th><th>부가세액금</th><th>합계</th></tr>
      ${Array.from({ length: 20 }, (_, i) => `<tr><td>${i + 1}</td>${cell()}${cell()}${cell('v qty')}${cell()}${cell()}${cell()}</tr>`).join('')}
      <tr><td>합계</td><td></td><td></td>${cell('v qty')}${cell()}${cell()}${cell()}</tr>
    </table>
    <table><tr><th>파일첨부</th><td><input type="file" name="file"><ul id="files"></ul></td></tr></table>
    <a id="act_edit_apprflow" href="#" onclick="document.getElementById('editor').classList.add('show');return false">결재 정보</a>
    <div class="go_popup" id="editor">
      <div class="activity_group">신청<table class="tb_approval_line"><tbody><tr><td class="kind">기안</td><td class="name">홍길동</td><td class="depart">연구개발팀</td></tr></tbody></table></div>
      <a href="#" onclick="document.getElementById('editor').classList.remove('show');return false">확인</a>
    </div>
    <a href="#" id="go" onclick="document.getElementById('confirm').classList.add('show');return false">결재요청</a>
    <div class="go_popup" id="confirm"><a class="btn_major_s" href="#" id="yes">확인</a></div>
    <script>
      window.__files = [];
      document.querySelector('input[name=file]').addEventListener('change', (e) => {
        for (const f of e.target.files) { const li = document.createElement('li'); li.textContent = f.name; files.appendChild(li); window.__files.push(f.name); }
      });
      document.getElementById('yes').addEventListener('click', async (e) => {
        e.preventDefault();
        const values = [...document.querySelectorAll('.v')].map((el) => {
          const v = el.tagName === 'SELECT' ? el.options[el.selectedIndex].text : el.value;
          return el.classList.contains('qty') ? v.replace(/\\D/g, '') : v;
        });
        const r = await fetch('/fake/submit', { method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ title: subject.value, values, files: window.__files }) });
        location.href = '/gw/app/approval/document/' + (await r.json()).id;
      });
    </script>`);
  const docPage = (id) => html(`
    <h1>문서 ${esc(id)}</h1>
    <a id="act_draft_withdrawal" href="#" onclick="document.getElementById('wd').classList.add('show');return false">상신취소</a>
    <div class="go_popup layer_approval" id="wd"><a class="btn_major_s" href="#" onclick="fetch('/fake/withdraw/${esc(id)}',{method:'POST'}).then(()=>document.getElementById('wd').classList.remove('show'));return false">확인</a></div>
    <div class="compo_reply_create"><input type="file" multiple><div class="progress" style="display:none"></div></div>
    <ul id="commentAttachPart"></ul>
    <textarea id="formApprComment"></textarea>
    <span id="apprComment" class="btn_minor_s">등록</span>
    <script>
      const input = document.querySelector('.compo_reply_create input[type=file]');
      input.addEventListener('change', () => {
        window.__cfiles = [...input.files].map((f) => f.name);
        for (const n of window.__cfiles) { const li = document.createElement('li'); li.textContent = n; commentAttachPart.appendChild(li); }
      });
      apprComment.addEventListener('click', () => fetch('/gw/api/approval/document/${esc(id)}/comment', { method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ message: formApprComment.value, attaches: window.__cfiles ?? [] }) }));
    </script>`);

  const usage = [
    { purchaseId: 'u1', useDate: '2026-03-02 10:00:00', bpName: '테스트상점', grossAmount: 120000, status: '1', docId: CLAIM, usePurposeName: '재료비' },
    { purchaseId: 'u2', useDate: '2026-03-10 11:00:00', bpName: 'ALIEXPRESS', grossAmount: 70000, status: '9' },
    { purchaseId: 'u3', useDate: '2026-03-12 12:00:00', bpName: '테스트주유소', grossAmount: 50000, status: '9' },
  ];
  const json = (res, body, status = 200) => { res.writeHead(status, { 'Content-Type': 'application/json' }); res.end(JSON.stringify(body)); };
  const body = (req) => new Promise((r) => { let s = ''; req.on('data', (c) => { s += c; }); req.on('end', () => r(s ? JSON.parse(s) : {})); });
  const server = http.createServer(async (req, res) => {
    const url = new URL(req.url, 'http://x');
    const p = url.pathname;
    const b = req.method === 'POST' ? await body(req) : {};
    if (p === '/home') return res.end(html('<h1>home</h1>'));
    if (p.startsWith('/gw/app/approval/document/new/')) { res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' }); return res.end(draft); }
    let m = p.match(/^\/gw\/app\/approval\/document\/(\d+)$/);
    if (m) { res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' }); return res.end(docPage(m[1])); }
    if (p === '/fake/submit') {
      const id = String(++state.seq);
      state.docs.set(id, { title: b.title, values: b.values, files: b.files, status: '진행중', form: '검수조서', docNum: `INS-${id.slice(-3)}`, drafter: '홍길동', canCancel: true });
      return json(res, { id });
    }
    m = p.match(/^\/fake\/withdraw\/(\d+)$/);
    if (m) { state.docs.get(m[1]).status = '임시저장'; return json(res, {}); }
    if (p === '/eacc/api/ess/custom/code/getList') {
      return json(res, { data: { list: [{ valueField: 'c-1', displayField: '연구카드(홍길동)   [홍길동]', creditcardNo: '1111-22**-****-1234', description2: '테스트카드' }] } });
    }
    if (p === '/eacc/api/ess/myExp/getCompanyCardList') {
      const from = `${b.fromDate.slice(0, 4)}-${b.fromDate.slice(4, 6)}-${b.fromDate.slice(6)}`;
      const to = `${b.toDate.slice(0, 4)}-${b.toDate.slice(4, 6)}-${b.toDate.slice(6)}`;
      return json(res, { data: { list: usage.filter((u) => u.useDate.slice(0, 10) >= from && u.useDate.slice(0, 10) <= to) } });
    }
    if (p === '/gw/api/approval/doclist/draft/all') {
      const list = [...state.docs.entries()].filter(([, d]) => d.form === '검수조서').reverse()
        .map(([id, d]) => ({ documentId: id, formName: d.form, title: d.title, docStatusName: d.status }));
      return json(res, { data: url.searchParams.get('page') === '0' ? list : [] });
    }
    if (p === '/gw/api/approval/apprform/tree') return json(res, { data: [{ data: { title: '검수조서' }, metadata: { id: 'F-INS' }, attr: { rel: 'FORM' } }] });
    m = p.match(/^\/gw\/api\/approval\/document\/(\d+)\/comment$/);
    if (m) {
      const list = state.comments.get(m[1]) ?? [];
      if (req.method === 'POST') {
        list.unshift({ id: `c${list.length + 1}`, message: b.message, attaches: (b.attaches ?? []).map((name) => ({ name })) });
        state.comments.set(m[1], list);
        return json(res, { code: '200' });
      }
      return json(res, { data: list });
    }
    m = p.match(/^\/gw\/api\/approval\/document\/(\d+)$/);
    if (m) {
      const d = state.docs.get(m[1]);
      if (!d) return json(res, {}, 404);
      const meta = [d.docNum, '2026-03-20', '연구개발팀', d.drafter, d.title];
      const spans = [...meta, ...(d.values ?? [])].map((v) => `<span data-type="text" data-value="${esc(v)}"></span>`).join('');
      return json(res, { code: '200', data: {
        document: { title: d.title, docNum: d.docNum, drafterName: d.drafter, docStatusName: d.status, formName: d.form, attachCount: d.files?.length ?? 0, docBodyContent: `<div>${spans}</div>` },
        actionCheck: { isAllowCancelDraftUser: d.status === '진행중' },
        apprFlow: { activityGroups: [] },
      } });
    }
    return json(res, {}, 404);
  });
  return { state, server };
}

async function jpeg(color, date) {
  return sharp({ create: { width: 320, height: 240, channels: 3, background: color } })
    .jpeg().withMetadata({ orientation: 1 })
    .withExifMerge({ IFD0: { Make: 'TestPhone' }, IFD2: { DateTimeOriginal: `${date.replace(/-/g, ':')} 09:00:00` }, IFD3: { GPSLatitudeRef: 'N', GPSLatitude: '37/1 0/1 0/1' } })
    .toBuffer();
}

describe('inspection tools', () => {
  let site; let base; let browser = null; let page = null;
  let client; let db; let dataDir; let inbox;
  const attach = (files) => ({ 'xylolabs/credentials': { username: 'hong', password: 'pw' }, 'xylolabs/attachments': files });
  const CREDS = attach([]);

  before(async () => {
    site = fakeDaou();
    await new Promise((r) => site.server.listen(0, '127.0.0.1', r));
    base = `http://127.0.0.1:${site.server.address().port}`;
    try {
      const { chromium } = await import('playwright');
      browser = await chromium.launch({ headless: true });
      page = await (await browser.newContext({ locale: 'ko-KR' })).newPage();
      await page.goto(`${base}/home`);
    } catch { browser = null; page = null; }

    // Without a browser the read tools still run: api() over plain fetch.
    const nodeApi = async (p, { method = 'GET', body } = {}) => {
      const r = await fetch(`${base}${p}`, { method, headers: { 'Content-Type': 'application/json' }, body: body ? JSON.stringify(body) : undefined });
      const text = await r.text();
      let data = null; try { data = JSON.parse(text); } catch { /* not JSON */ }
      return { status: r.status, ok: r.ok, data };
    };
    const sessions = {
      async withSession(owner, creds, opts, fn) {
        if (page && new URL(page.url()).origin !== base) await page.goto(`${base}/home`);
        return fn({ page, api: page ? makeApi(page) : nodeApi, context: null, owner, baseUrl: base, fresh: false });
      },
    };

    dataDir = fs.mkdtempSync(path.join(os.tmpdir(), 'dao-insp-'));
    inbox = path.join(dataDir, 'inbox');
    fs.mkdirSync(inbox);
    db = openDb(':memory:');
    db.upsertAccount({ owner: 'hong', name: '홍길동', deptId: '10', deptCode: 'rnd', deptName: '연구개발팀' });
    const legacy = { vendor: '테스트상점', project: '스마트농장', subProject: '온실', cardIssuer: '테스트카드', payMethod: '법인카드', cardLast4: '1234' };
    db.upsertPurchase({ owner: 'hong', key: 'cam', status: 'filed', title: '테스트 카메라 외 1종 구매의 건', paidAt: '2026-03-02', amountKrw: 120000, docId: '1000000000000011',
      data: { ...legacy, item: '테스트 카메라 / 1개, 렌즈 필터 / 2개', note: ['테스트 카메라 / 1개 100,000원', '렌즈 필터 / 2개 20,000원'], purpose: '현장 기록용' } });
    db.upsertPurchase({ owner: 'hong', key: 'ali-a', status: 'filed', title: 'A 구매의 건', paidAt: '2026-03-10', usd: 20, paidKrw: 28000, amountKrw: 28000, docId: '1000000000000013',
      data: { ...legacy, vendor: '테스트판매자 (알리익스프레스)', item: 'A / 1개' } });
    db.upsertPurchase({ owner: 'hong', key: 'ali-b', status: 'filed', title: 'B 구매의 건', paidAt: '2026-03-10', usd: 30, paidKrw: 42000, amountKrw: 42000, docId: '1000000000000014',
      data: { ...legacy, vendor: '테스트판매자 (알리익스프레스)', item: 'B / 1개' } });

    const server = new McpServer({ name: 'daouoffice', version: '0.0.0' }, { capabilities: { tools: {} } });
    // baseUrl is https-only in config; the fake sessions hand the tools the local address.
    const config = parseConfig({
      people: { cto: '홍길동' }, cards: { 1234: { project: '스마트농장', issuer: '테스트카드' } },
      lines: { inspection: 'ctoFinal' }, inspection: { perOrderMerchants: ['ALIEXPRESS'] },
    });
    const sink = new Writable({ write(_c, _e, cb) { cb(); } });
    registerAll(server, createRuntime({ config, db, sessions, dataDir, logger: createLogger({ stream: sink }) }));
    const [a, b] = InMemoryTransport.createLinkedPair();
    await server.connect(a);
    client = new Client({ name: 't', version: '0' });
    await client.connect(b);

    fs.writeFileSync(path.join(inbox, 'IMG_1.jpg'), await jpeg('#c33', '2026-03-05'));
    fs.writeFileSync(path.join(inbox, 'IMG_2.jpg'), await jpeg('#3c3', '2026-03-06'));
    // A receipt PDF with an author, a creator tool and an XMP packet, as scanners and shops write them.
    const pdf = await PDFDocument.create();
    pdf.addPage([200, 200]);
    pdf.setAuthor('Test Author'); pdf.setCreator('TestScanner'); pdf.setTitle('테스트 영수증');
    const xmp = pdf.context.stream('<x:xmpmeta xmlns:x="adobe:ns:meta/"><dc:creator>XMPAUTHOR</dc:creator></x:xmpmeta>', { Type: 'Metadata', Subtype: 'XML' });
    pdf.catalog.set(PDFName.of('Metadata'), pdf.context.register(xmp));
    fs.writeFileSync(path.join(inbox, 'receipt.pdf'), await pdf.save({ useObjectStreams: false }));
    fs.writeFileSync(path.join(inbox, 'shot.png'), await sharp({ create: { width: 60, height: 40, channels: 3, background: '#fff' } }).png().toBuffer());
  });

  after(async () => {
    await client?.close();
    await browser?.close();
    site.server.close();
    fs.rmSync(dataDir, { recursive: true, force: true });
  });

  const call = (name, args, meta = CREDS) => client.callTool({ name, arguments: args, _meta: meta });
  const execute = (meta) => ({ ...meta, 'xylolabs/mode': 'execute' });

  test('save_receipt stores a PDF and a stripped image under the naming convention, once', async () => {
    const meta = attach([
      { name: 'receipt.pdf', mimeType: 'application/pdf', path: path.join(inbox, 'receipt.pdf') },
      { name: 'shot.png', mimeType: 'image/png', path: path.join(inbox, 'shot.png') },
    ]);
    const receipts = [
      { index: 1, date: '2026-03-02', category: '재료비', item: '테스트 카메라 외 1건', vendor: '테스트상점', amount: 120000, currency: '원', kind: '영수증' },
      { index: 2, date: '2026-03-10', category: '재료비', item: 'A', vendor: '알리익스프레스', amount: 20, currency: 'USD', kind: '주문내역', orderId: '1000000000000000', cardLast4: '1234' },
    ];
    const r = await call('save_receipt', { receipts }, meta);
    assert.ok(!r.isError, r.content[0].text);
    const rows = db.receipts('hong');
    assert.deepEqual(rows.map((x) => x.file), [
      '2026-03-02_재료비_테스트-카메라-외-1건_테스트상점_120000원_영수증.pdf',
      '2026-03-10_재료비_A_알리익스프레스_20USD_주문내역.jpg',
    ]);
    assert.ok(fs.existsSync(path.join(dataDir, 'files', 'hong', 'receipts', rows[0].file)));
    const original = fs.readFileSync(path.join(inbox, 'receipt.pdf')).toString('latin1');
    assert.ok(original.includes('XMPAUTHOR') && original.includes('/Author'), 'the fixture carries metadata');
    const kept = fs.readFileSync(path.join(dataDir, 'files', 'hong', 'receipts', rows[0].file));
    for (const bit of ['XMPAUTHOR', '/Author', '/Creator', '/Metadata', '/Title']) assert.ok(!kept.toString('latin1').includes(bit), `${bit} removed`);
    assert.equal((await PDFDocument.load(kept)).getPageCount(), 1, 'the pages survive');
    assert.equal(rows[1].data.cardLast4, '1234');
    assert.equal(rows[0].data.cardLast4, undefined);
    const again = await call('save_receipt', { receipts: [receipts[0]] }, meta);
    assert.match(again.content[0].text, /이미 저장한 영수증/);
    assert.equal(db.receipts('hong').length, 2);
    const bad = await call('save_receipt', { receipts: [{ ...receipts[0], index: 5 }] }, meta);
    assert.equal(bad.structuredContent.code, 'VALIDATION');
  });

  test('save_photos strips, dates and links photos, and records serials on the requisition', async () => {
    const meta = attach([
      { name: 'IMG_1.jpg', mimeType: 'image/jpeg', path: path.join(inbox, 'IMG_1.jpg') },
      { name: 'IMG_2.jpg', mimeType: 'image/jpeg', path: path.join(inbox, 'IMG_2.jpg') },
    ]);
    const unsure = await call('save_photos', { photos: [{ index: 1, shows: '상자' }] }, meta);
    assert.equal(unsure.structuredContent.code, 'VALIDATION');
    assert.match(unsure.content[0].text, /물어/);
    const r = await call('save_photos', { photos: [
      { index: 1, name: 'IMG_1.jpg', purchaseKey: 'cam', shows: '카메라 상자와 모델명 라벨', serials: [{ item: '테스트 카메라', numbers: ['TC-0001'] }] },
      { index: 2, purchaseKey: 'cam', shows: '렌즈 필터 두 개' },
    ] }, meta);
    assert.ok(!r.isError, r.content[0].text);
    const photos = db.photos('hong');
    assert.deepEqual(photos.map((p) => [p.purchaseKey, p.takenDate]), [['cam', '2026-03-05'], ['cam', '2026-03-06']]);
    const stored = fs.readFileSync(path.join(dataDir, 'files', 'hong', 'photos', photos[0].file));
    assert.ok(!jpegSegments(stored).includes(0xE1), 'no EXIF left');
    assert.deepEqual(db.purchase('hong', 'cam').data.serials, { '테스트 카메라': ['TC-0001'] });
    const fix = await call('save_photos', { photos: [{ photoId: photos[1].id, shows: '렌즈 필터 두 개와 포장' }] });
    assert.ok(!fix.isError, fix.content[0].text);
    assert.equal(db.photo('hong', photos[1].id).shows, '렌즈 필터 두 개와 포장');
    // Sending the same picture again changes only what was given: date, links and serials stay.
    const again = await call('save_photos', { photos: [{ index: 1, purchaseKey: 'cam', shows: '카메라 상자와 모델명 라벨' }] }, meta);
    assert.ok(!again.isError, again.content[0].text);
    const kept = db.photo('hong', photos[0].id);
    assert.equal(kept.takenDate, '2026-03-05');
    assert.deepEqual(kept.data.serials, [{ item: '테스트 카메라', numbers: ['TC-0001'] }]);
    assert.deepEqual(db.purchase('hong', 'cam').data.serials, { '테스트 카메라': ['TC-0001'] });
  });

  test('mark_inspection and mark_payment record what a person said', async () => {
    const done = await call('mark_inspection', { purchaseKey: 'ali-b', action: 'done', reason: '다른 분이 처리' });
    assert.ok(!done.isError, done.content[0].text);
    const items = await call('mark_inspection', { purchaseKey: 'ali-a', action: 'items', items: [{ name: 'A 모듈', qty: 1, amount: 27000 }] });
    assert.match(items.content[0].text, /품의 금액/);
    const pay = await call('mark_payment', { date: '2026-03-12', merchant: '테스트주유소', amount: 50000, action: 'elsewhere', reason: '다른 분이 청구' });
    assert.ok(!pay.isError, pay.content[0].text);
    assert.equal(db.overrides('hong', 'elsewhere').length, 1);
    await call('mark_payment', { date: '2026-03-12', merchant: '테스트주유소', amount: 50000, action: 'clear' });
    assert.equal(db.overrides('hong', 'elsewhere').length, 0);
    const pin = await call('mark_payment', { date: '2026-03-12', merchant: '테스트상점', amount: 1, action: 'pin', reason: 'r', receipts: ['없는파일'] });
    assert.equal(pin.structuredContent.code, 'NOT_FOUND');
  });

  test('inspection_candidates shows what can be filed and what is missing', async () => {
    // The second order receipt of the AliExpress checkout and a reference rate, so the charge pairs with both orders.
    db.addReceipt({ owner: 'hong', file: '2026-03-10_재료비_B_알리익스프레스_30USD_주문내역.jpg', date: '2026-03-10', category: '재료비', item: 'B',
      vendor: '알리익스프레스', amount: 30, currency: 'USD', kind: '주문내역', orderId: '1000000000020000' });
    db.addFxRate({ day: '2026-03-09', rate: 1400, source: '테스트' });
    const r = await call('inspection_candidates', { month: '2026-03' });
    assert.ok(!r.isError, r.content[0].text);
    const by = Object.fromEntries(r.structuredContent.units.map((u) => [u.key, u]));
    assert.equal(by['2026-03-02/120000'].state, '올릴 수 있음');
    assert.equal(by['2026-03-02/120000'].title, '2026년 3월 (1234/rnd/홍길동) 검수조서 - 테스트 카메라 외 1종');
    assert.equal(by['2026-03-10/70000#ali-a'].state, '아직');
    assert.ok(by['2026-03-10/70000#ali-a'].shots.length > 0);
    assert.equal(by['2026-03-10/70000#ali-b'].state, '따로 처리됨');
    assert.match(r.content[0].text, /주문마다 한 장/);
  });

  test('payment_status reports the pipeline per payment', async () => {
    const r = await call('payment_status', { from: '2026-03-01', to: '2026-03-31', card: '1234' });
    assert.ok(!r.isError, r.content[0].text);
    const cam = r.structuredContent.rows.find((x) => x.key === '2026-03-02/120000');
    assert.equal(cam.photo, '2장');
    assert.equal(cam.claim, CLAIM);
    assert.match(r.content[0].text, /남은 일/);
  });

  test('file_inspection_report previews the plan with thumbnails and changes nothing', async () => {
    const before = site.state.docs.size;
    const r = await call('file_inspection_report', { payment: '2026-03-02/120000' });
    assert.ok(!r.isError, r.content[0].text);
    assert.equal(r.structuredContent.preview, true);
    assert.match(r.content[0].text, /검수조서: 2026년 3월 \(1234\/rnd\/홍길동\) 검수조서 - 테스트 카메라 외 1종/);
    assert.match(r.content[0].text, /테스트 카메라 \(S\/N TC-0001\)/);
    assert.match(r.content[0].text, /비고: 구매 품의 TEST-11, 지출결의 TEST-99/);
    assert.equal(r.content.filter((c) => c.type === 'image').length, 2);
    assert.equal(site.state.docs.size, before);
    const split = await call('file_inspection_report', { payment: '2026-03-10/70000' });
    assert.equal(split.structuredContent.code, 'VALIDATION');
    assert.match(split.content[0].text, /ali-a/);
    const nophoto = await call('file_inspection_report', { payment: '2026-03-10/70000', purchaseKey: 'ali-a' });
    assert.equal(nophoto.structuredContent.code, 'VALIDATION');
  });

  test('execute refuses when the plan changed after the preview, or when there was no preview', async () => {
    const args = { payment: '2026-03-02/120000' };
    const before = site.state.docs.size;
    assert.ok(!(await call('file_inspection_report', args)).isError);
    const photo = db.photos('hong')[1];
    await call('save_photos', { photos: [{ photoId: photo.id, takenDate: '2026-03-07' }] });   // 입고일자 moves
    const changed = await call('file_inspection_report', args, execute(CREDS));
    assert.equal(changed.structuredContent.code, 'CONFLICT');
    assert.equal(changed.structuredContent.data.previewChanged, true);
    assert.match(changed.content[0].text, /미리보기 뒤에 내용이 바뀌었습니다/);
    const unseen = await call('file_inspection_report', { ...args, card: '1234' }, execute(CREDS));
    assert.equal(unseen.structuredContent.code, 'CONFLICT', 'no preview for these arguments');
    assert.equal(site.state.docs.size, before, 'nothing reached DaouOffice');
    await call('save_photos', { photos: [{ photoId: photo.id, takenDate: '2026-03-06' }] });
  });

  test('an unconfirmed submission blocks the payment for a while, then settles', async () => {
    const args = { payment: '2026-03-02/120000' };
    const title = '2026년 3월 (1234/rnd/홍길동) 검수조서 - 테스트 카메라 외 1종';
    const f = db.addFiling({ owner: 'hong', kind: 'inspection', ref: '2026-03-02/120000', title, status: 'submitting', detail: { withdrawn: ['1999999999999999'] } });
    const blocked = await call('file_inspection_report', args);
    assert.equal(blocked.structuredContent.code, 'CONFLICT');
    assert.match(blocked.content[0].text, /내 기안함/);
    assert.match(blocked.content[0].text, /1999999999999999/);
    db.updateFiling('hong', f.id, { filedAt: kstIso(new Date(Date.now() - 20 * 60_000)) });
    const ok = await call('file_inspection_report', args);
    assert.ok(!ok.isError, ok.content[0].text);
    assert.equal(db.filings('hong', { kind: 'inspection' }).find((x) => x.id === f.id).status, 'lost');
  });

  test('file_inspection_report execute fills, attaches, submits and verifies on the server', { timeout: 120_000 }, async (t) => {
    if (!browser) return t.skip('chromium is not installed');
    const r = await call('file_inspection_report', { payment: '2026-03-02/120000' }, execute(CREDS));
    assert.ok(!r.isError, r.content[0].text);
    assert.equal(r.structuredContent.verified, true);
    const doc = site.state.docs.get(r.structuredContent.docId);
    assert.equal(doc.title, '2026년 3월 (1234/rnd/홍길동) 검수조서 - 테스트 카메라 외 1종');
    assert.deepEqual(doc.files, ['2026-03-02_재료비_테스트-카메라-외-1건_테스트상점_120000원_영수증.pdf', '제품사진_1.jpg', '제품사진_2.jpg']);
    assert.equal(doc.values[1], '법인카드');
    assert.equal(doc.values[9], '테스트 카메라 (S/N TC-0001)');
    assert.equal(doc.values[11], '1', '수량 keeps digits only');
    const filing = db.filings('hong', { kind: 'inspection' }).find((f) => f.docId === r.structuredContent.docId);
    assert.equal(filing.status, '진행중', 'the submitting row was completed with the document');
    assert.equal(filing.ref, '2026-03-02/120000');
    assert.deepEqual(filing.detail.purchaseKeys, ['cam']);
    assert.equal(filing.detail.purchaseId, 'u1');

    const again = await call('file_inspection_report', { payment: '2026-03-02/120000' }, execute(CREDS));
    assert.equal(again.structuredContent.code, 'CONFLICT');
    assert.match(again.content[0].text, /fix/);
  });

  test('file_inspection_report fix withdraws the filed report and files it again', { timeout: 120_000 }, async (t) => {
    if (!browser) return t.skip('chromium is not installed');
    const old = db.filings('hong', { kind: 'inspection' }).find((f) => f.docId).docId;
    // Another payment's report with the very same title: the filings, not the title, pick what to withdraw.
    const title = site.state.docs.get(old).title;
    const foreign = '2999999999999999';
    site.state.docs.set(foreign, { title, form: '검수조서', status: '진행중', drafter: '홍길동', docNum: 'INS-999', values: [], files: [] });
    const preview = await call('file_inspection_report', { payment: '2026-03-02/120000', action: 'fix' });
    assert.ok(!preview.isError, preview.content[0].text);
    assert.match(preview.content[0].text, new RegExp(`${old} \\(`));
    assert.doesNotMatch(preview.content[0].text, new RegExp(foreign));
    const r = await call('file_inspection_report', { payment: '2026-03-02/120000', action: 'fix' }, execute(CREDS));
    assert.ok(!r.isError, r.content[0].text);
    assert.equal(site.state.docs.get(old).status, '임시저장');
    assert.equal(site.state.docs.get(foreign).status, '진행중', 'the other payment\'s report is untouched');
    assert.notEqual(r.structuredContent.docId, old);
    assert.deepEqual(r.structuredContent.withdrawn, [old]);
    // A report drafted by someone else is never withdrawn.
    const theirs = '2999999999999998';
    site.state.docs.set(theirs, { title, form: '검수조서', status: '진행중', drafter: '김철수', docNum: 'INS-998', values: [], files: [] });
    const refused = await call('file_inspection_report', { payment: '2026-03-02/120000', action: 'fix', oldDocId: theirs });
    assert.equal(refused.structuredContent.code, 'CONFLICT');
    assert.match(refused.content[0].text, /다른 사람/);
  });

  test('attach_photos_to_claim previews, posts one comment with both photos, and never posts twice', { timeout: 120_000 }, async (t) => {
    const preview = await call('attach_photos_to_claim', { from: '2026-03-01', to: '2026-03-31' });
    assert.ok(!preview.isError, preview.content[0].text);
    assert.match(preview.content[0].text, new RegExp(`지출결의 ${CLAIM}`));
    assert.match(preview.content[0].text, /- 테스트 카메라 외 1종 구매: 카메라 상자와 모델명 라벨, 렌즈 필터 두 개와 포장/);
    assert.equal(site.state.comments.size, 0);
    if (!browser) return t.skip('chromium is not installed');
    const r = await call('attach_photos_to_claim', { from: '2026-03-01', to: '2026-03-31' }, execute(CREDS));
    assert.ok(!r.isError, r.content[0].text);
    const comments = site.state.comments.get(CLAIM);
    assert.equal(comments.length, 1);
    assert.equal(comments[0].attaches.length, 2);
    assert.match(comments[0].message, /^검수 사진을 첨부합니다\./);
    assert.ok(db.photos('hong').every((p) => p.comments.some((c) => c.target === 'claim' && c.docId === CLAIM)));
    const none = await call('attach_photos_to_claim', { from: '2026-03-01', to: '2026-03-31' });
    assert.equal(none.structuredContent.code, 'VALIDATION');
    // If the records were lost after posting, the same comment on the server is found and not posted again.
    for (const p of db.photos('hong')) db.updatePhoto('hong', p.id, { comments: [] });
    assert.ok(!(await call('attach_photos_to_claim', { from: '2026-03-01', to: '2026-03-31' })).isError);
    const again = await call('attach_photos_to_claim', { from: '2026-03-01', to: '2026-03-31' }, execute(CREDS));
    assert.ok(!again.isError, again.content[0].text);
    assert.match(again.content[0].text, /이미 있어/);
    assert.equal(site.state.comments.get(CLAIM).length, 1, 'still one comment');
  });
});
