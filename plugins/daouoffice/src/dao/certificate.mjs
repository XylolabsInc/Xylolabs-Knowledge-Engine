import fs from 'node:fs/promises';
import { PluginError } from '../errors.mjs';
import { assertExecute } from '../guard.mjs';
import { dismissDialogs } from './dialogs.mjs';
import { approveDocument, readDocumentApprovalLine, setSelfApprovalLine, submitDocument } from './approval.mjs';

/**
 * 증명서 발급(재직증명서, 경력증명서). 원본 저장소 `certificate.mjs` 에서 왔다.
 *
 * 인사 앱(`/ess/app/#hr`)의 증명서 발급 신청에서 처리한다. 여기서 신청하면 전자결재가 따라
 * 올라간다. PDF 는 전자결재가 승인된 뒤에야 나온다. 그래서 "증명서를 뽑는다" 는 조회가 아니라
 * 네 단계 쓰기다.
 *
 *   저장 → 전자결재 상신 → 승인 → 출력
 *
 * 인사 앱은 ExtJS 다. 숫자 id 는 렌더링마다 바뀌므로 `Ext.ComponentQuery` 로 라벨을 보고 잡는다.
 * 콤보 값(코드)은 인스턴스 설정이라 하드코딩하지 않고 store 에서 표시명으로 찾는다.
 *
 * 셀프승인: 결재선을 본인 단독(기안, 결재 모두 나)으로 바꿔 상신하고 본인이 승인한다. 결재선
 * 편집은 화면 안에서만 유지되므로 편집과 상신을 같은 창에서 끝낸다. 회사가 허락할 때만 쓴다
 * (회사 설정 `certificate.selfApproval`).
 *
 * 목록 API 응답에 사번과 UUID 가 섞여 있다. 밖으로 내보낼 때는 `normalizeCert()` 의 칸만 쓴다.
 * PDF 에는 주민번호 뒷자리(기본 마스킹), 주소, 생년월일이 들어 있다. 디스크에 남기지 않고, 로그에
 * 내용을 쓰지 않는다.
 */

const fail = (message) => new PluginError('UPSTREAM', message);

/** 이 플러그인이 다루는 증명서 종류. 원천징수부는 소득년도 칸을 채우는 흐름이 확인되지 않아 뺐다. */
export const CERT_KINDS = Object.freeze(['재직증명서', '경력증명서']);

/** 용도 콤보에서 본 값. 실제 선택지는 신청 창의 콤보에서 다시 확인한다. */
export const CERT_PURPOSES = Object.freeze([
  '대출용', '은행 제출용', '비자발급용', '경력증명용', '제안발표용', '제안서 제출용', '입찰 참가용', '기타', '조달청 제출용',
]);

/** 상태 코드. 목록 API 가 `docStatusName` 을 채우지 않아 코드로 푼다. */
export const CERT_STATUS = Object.freeze({ '': '미상신', '01': '결재중', '02': '결재완료' });

const ymd = (v) => {
  const s = String(v ?? '').trim();
  if (/^\d{8}$/.test(s)) return `${s.slice(0, 4)}-${s.slice(4, 6)}-${s.slice(6, 8)}`;
  return s.slice(0, 10);
};
const codeOf = (r) => String(r?.docStatusCode ?? '');

/** 회사 설정 `certificate.selfApproval` (기본 false). */
export const selfApprovalAllowed = (cfg) => cfg?.certificate?.selfApproval === true;

// ------------------------------------------------------------------ 조회

/** 발급 신청 이력 원본 행. 밖으로 내보내지 않는다(사번, UUID). */
export async function listCertRequests(api) {
  const res = await api('/eacc/api/ess/certIsu/getIssueCertList', {
    method: 'POST',
    body: { fromDate: '20200101', toDate: '20991231', page: 1, start: 0, limit: 200 },
  });
  const list = res.data?.data?.list;
  if (res.status !== 200 || !Array.isArray(list)) throw fail(`증명서 발급 이력을 읽지 못했습니다 (HTTP ${res.status}).`);
  return list;
}

/** 이력 한 줄을 보여 줄 칸만 남긴다. 사번, 신청 건 UUID, 리포트 식별자는 뺀다. */
export function normalizeCert(r) {
  const code = codeOf(r);
  return {
    kind: String(r.certName ?? ''),
    purpose: String(r.issuePurposeName ?? ''),
    target: String(r.submitPlace ?? ''),
    requestDate: ymd(r.requestDate),
    status: CERT_STATUS[code] ?? `알수없음(${code})`,
    docId: r.docId ? String(r.docId) : null,
    approvalCode: r.approvalCode ? String(r.approvalCode) : null,
    expireDate: ymd(r.expireDate) || null,
  };
}

/** 최신 신청이 앞에 오게 정렬한다. */
const newestFirst = (rows) => [...rows].sort((a, b) => ymd(b.requestDate).localeCompare(ymd(a.requestDate)));

/** 승인된 증명서가 유효일자를 지났는가. 유효일자를 모르면 지나지 않은 것으로 본다. */
export const isExpired = (r, today) => Boolean(r?.expireDate) && ymd(r.expireDate) < today;

/**
 * 같은 종류, 용도, 제출처의 신청 가운데 이어서 처리할 것. 미상신, 결재중, 유효일자가 남은
 * 결재완료 순으로 최신 것을 고른다. 없으면 null (새로 신청한다).
 */
export function pickRequest(rows, { kind, purpose, target }, today) {
  const same = newestFirst(rows).filter((r) => r.certName === kind
    && String(r.issuePurposeName ?? '') === purpose
    && String(r.submitPlace ?? '').trim() === String(target ?? '').trim());
  return same.find((r) => codeOf(r) !== '02' || !isExpired(r, today)) ?? null;
}

/**
 * 남은 단계. `save`, `submit`, `approve`, `print`. 셀프승인이 아니면 상신 뒤 결재를 기다린다
 * (`wait`).
 */
export function certSteps(row, { self }) {
  const code = row ? codeOf(row) : null;
  if (code === '02') return ['print'];
  if (code === '01') return self ? ['approve', 'print'] : ['wait'];
  const rest = self ? ['submit', 'approve', 'print'] : ['submit', 'wait'];
  return row ? rest : ['save', ...rest];
}

/** 단계 이름 → 사람이 읽는 말. */
export const STEP_LABEL = Object.freeze({
  save: '신청 저장', submit: '전자결재 상신', approve: '본인 승인', print: 'PDF 출력', wait: '결재 대기',
});

// ------------------------------------------------------------------ 화면

/** 인사 앱을 연다. */
async function openHrApp(page, baseUrl) {
  await page.goto(`${baseUrl}/ess/app/#hr`, { waitUntil: 'domcontentloaded', timeout: 60_000 });
  await page.waitForLoadState('networkidle', { timeout: 30_000 }).catch(() => {});
  await page.waitForTimeout(4000);
  await dismissDialogs(page);
}

/** 인사 앱 → 증명서 발급 신청 목록. */
export async function openCertList(page, baseUrl) {
  await openHrApp(page, baseUrl);
  await page.evaluate(() => {
    const items = [...document.querySelectorAll('a')]
      .filter((e) => (e.innerText || '').trim() === '증명서 발급 신청' && e.offsetParent !== null);
    items[items.length - 1]?.click();
  });
  await page.waitForTimeout(7000);
}

/** 인사 앱 → 증명서 발급 신청 팝업. */
export async function openRequestDialog(page, baseUrl) {
  await openHrApp(page, baseUrl);
  await page.getByRole('button', { name: '증명서 발급 신청' }).first().click({ timeout: 20_000 });
  await page.waitForTimeout(5000);
  const ready = await page.evaluate(() => !!window.Ext
    && window.Ext.ComponentQuery.query('combobox').some((c) => c.isVisible() && /증명서 종류/.test(c.getFieldLabel() || '')));
  if (!ready) throw fail('증명서 발급 신청 창이 열리지 않았습니다.');
}

/**
 * 신청 창의 칸을 채운다(화면 안에서만, 저장하지 않는다). 콤보에 원하는 값이 없으면 고를 수 있는
 * 값을 알려 주며 VALIDATION.
 */
export async function fillRequestDialog(page, req) {
  const out = await page.evaluate((r) => {
    const Ext = window.Ext;
    const byLabel = (xtype, text) => Ext.ComponentQuery.query(xtype)
      .filter((c) => c.isVisible())
      .find((c) => (c.getFieldLabel?.() || '').includes(text));
    const setCombo = (labelText, displayName) => {
      const combo = byLabel('combobox', labelText);
      if (!combo) return { error: `콤보를 찾지 못했습니다: ${labelText}` };
      const rec = combo.getStore().getRange().find((x) => x.get('displayField') === displayName);
      if (!rec) {
        const options = combo.getStore().getRange().map((x) => x.get('displayField')).join(', ');
        return { invalid: `${labelText}에 "${displayName}" 이 없습니다. 고를 수 있는 값: ${options}` };
      }
      combo.setValue(rec.get('valueField'));
      return { value: combo.getRawValue() };
    };
    const setText = (labelText, text) => {
      if (!text) return { value: '' };
      const field = byLabel('textfield', labelText);
      if (!field) return { error: `입력란을 찾지 못했습니다: ${labelText}` };
      field.setValue(text);
      return { value: field.getValue() };
    };
    return {
      kind: setCombo('증명서 종류', r.kind),
      purpose: setCombo('용도', r.purpose),
      target: setText('제출처', r.target),
      note: setText('비고', r.note),
    };
  }, req);
  const invalid = Object.values(out).map((x) => x.invalid).filter(Boolean);
  if (invalid.length) throw new PluginError('VALIDATION', invalid.join(' '));
  const broken = Object.values(out).map((x) => x.error).filter(Boolean);
  if (broken.length) throw fail(`증명서 신청 창이 예상과 다릅니다: ${broken.join(', ')}`);
  return Object.fromEntries(Object.entries(out).map(([k, v]) => [k, v.value]));
}

/** ExtJS 버튼은 `getByRole('button')` 으로 안 잡히는 것이 있어 컴포넌트로 누른다. */
async function clickDialogButton(page, text) {
  const ok = await page.evaluate((label) => {
    const btn = window.Ext.ComponentQuery.query('button')
      .filter((b) => b.isVisible() && !b.isDisabled())
      .find((b) => (b.getText?.() || '').trim() === label);
    if (!btn) return false;
    btn.fireHandler ? btn.fireHandler() : btn.el.dom.click();
    return true;
  }, text);
  if (!ok) throw fail(`버튼을 찾지 못했습니다: ${text}`);
}

/** 확인용 ExtJS MessageBox 를 처리한다. 없으면 넘어간다. 메시지 문구를 돌려준다. */
async function confirmMessageBox(page) {
  const text = await page.evaluate(() => {
    const box = window.Ext.ComponentQuery.query('messagebox').find((m) => m.isVisible());
    if (!box) return null;
    const msg = (box.msg?.el?.dom?.innerText ?? '').trim();
    const btn = box.query('button').filter((b) => b.isVisible()).find((b) => /^(확인|예|OK)$/.test((b.getText?.() || '').trim()));
    if (btn) (btn.fireHandler ? btn.fireHandler() : btn.el.dom.click());
    return msg || '(빈 메시지)';
  });
  await page.waitForTimeout(2000);
  return text;
}

/**
 * 목록 그리드에서 신청 건 하나를 고른다. `삭제`, `전자결재` 는 행을 골라야 켜진다.
 * 원본은 첫 행을 골랐다. 여기서는 신청 건 ID 로 고르고, 못 찾으면 행이 하나뿐일 때만 그 행을 쓴다.
 */
async function selectRequestRow(page, issuecertId) {
  const r = await page.evaluate((id) => {
    const grid = window.Ext.ComponentQuery.query('grid').filter((g) => g.isVisible())[0];
    if (!grid) return 'no-grid';
    const store = grid.getStore();
    const all = store.getRange();
    // UNVERIFIED: the grid store's field names are assumed to match the list API rows (issuecertId).
    const rec = all.find((x) => String(x.get('issuecertId') ?? '') === id) ?? (all.length === 1 ? all[0] : null);
    if (!rec) return `not-found:${all.length}`;
    grid.getSelectionModel().select(rec);
    return 'ok';
  }, String(issuecertId));
  if (r !== 'ok') throw fail(`증명서 신청 목록에서 신청 건을 고르지 못했습니다 (${r}).`);
  await page.waitForTimeout(1500);
}

/** 그 신청 건 행의 증명서 종류 칸(`td.cell_link`)을 누른다. 출력이 시작된다. */
async function clickPrintCell(page, issuecertId) {
  const r = await page.evaluate((id) => {
    const grid = window.Ext?.ComponentQuery.query('grid').filter((g) => g.isVisible())[0];
    document.querySelectorAll('[data-xylo-print]').forEach((e) => e.removeAttribute('data-xylo-print'));
    // UNVERIFIED: row lookup through the grid view; the source clicked the first td.cell_link.
    const rec = grid?.getStore().getRange().find((x) => String(x.get('issuecertId') ?? '') === id);
    const node = rec ? grid.getView().getNode(rec) : null;
    let cell = node?.querySelector('td.cell_link') ?? null;
    if (!cell) {
      const cells = [...document.querySelectorAll('td.cell_link')].filter((c) => c.getBoundingClientRect().width > 0);
      if (cells.length === 1) [cell] = cells;
    }
    if (!cell) return false;
    cell.setAttribute('data-xylo-print', '1');
    return true;
  }, String(issuecertId));
  if (!r) throw fail('출력할 증명서 칸을 찾지 못했습니다.');
  const cell = page.locator('[data-xylo-print="1"]').first();
  await cell.scrollIntoViewIfNeeded().catch(() => {});
  await cell.click({ timeout: 20_000, force: true });
}

/** 신청 창을 채워 보고 취소한다. 아무것도 남지 않는다. 미리보기에서 콤보 값을 확인할 때 쓴다. */
export async function checkRequestDialog(page, baseUrl, req) {
  await openRequestDialog(page, baseUrl);
  try {
    return await fillRequestDialog(page, req);
  } finally {
    await clickDialogButton(page, '취소').catch(() => {});
    await page.waitForTimeout(1000);
  }
}

// ------------------------------------------------------------------ 쓰기 (승인된 실행에서만)

/** 신청 건을 저장한다. 결재는 올라가지 않는다. */
export async function saveRequest(page, baseUrl, req) {
  await openRequestDialog(page, baseUrl);
  await fillRequestDialog(page, req);
  assertExecute('증명서 발급 신청 저장');
  await clickDialogButton(page, '저장');
  await page.waitForTimeout(2500);
  await confirmMessageBox(page);
  await page.waitForTimeout(4000);
}

/**
 * 저장된 건을 전자결재로 올린다. 목록에서 행을 고르고 `전자결재` → 확인을 누르면 결재 문서가
 * 새 창으로 열린다. 셀프승인이면 그 창에서 결재선을 본인 단독으로 바꾸고, 다른 사람이 남아
 * 있지 않은지 확인한 뒤 상신한다. 결재선 문자열을 돌려준다.
 */
export async function submitRequest(page, baseUrl, issuecertId, { self, me, myDept }) {
  await openCertList(page, baseUrl);
  await selectRequestRow(page, issuecertId);

  assertExecute('증명서 전자결재 요청');
  // 창이 열리기를 먼저 기다린다. 버튼 단계가 실패해도 이 약속이 혼자 거부되지 않게 받아 둔다.
  const opened = page.context().waitForEvent('page', { timeout: 60_000 }).catch((e) => e);
  await clickDialogButton(page, '전자결재');
  await page.waitForTimeout(2500);
  await confirmMessageBox(page);
  const doc = await opened;
  if (doc instanceof Error) throw fail('증명서 결재 문서 창이 열리지 않았습니다.');
  try {
    await doc.waitForLoadState('domcontentloaded').catch(() => {});
    await doc.waitForTimeout(8000);
    await dismissDialogs(doc);

    if (self) {
      const editorLine = await setSelfApprovalLine(doc, me);
      const others = selfLineOthers(editorLine, me);
      if (others.length) throw fail(`본인 단독 결재선에 다른 사람이 남아 있어 상신하지 않았습니다: ${others.join(', ')}`);
      const shown = await readDocumentApprovalLine(doc);
      const stray = strayNames(shown, { me, myDept });
      if (stray.length) throw fail(`문서 화면의 결재선에 다른 사람이 보여 상신하지 않았습니다: ${stray.join(', ')}`);
    }
    const line = await readDocumentApprovalLine(doc);
    await submitDocument(doc);
    return line;
  } finally {
    await doc.close().catch(() => {});
  }
}

/** 편집기 결재선(`기안 홍길동(부서) → 결재 홍길동(부서)`)에서 나 아닌 이름. */
export function selfLineOthers(line, me) {
  return String(line ?? '').split(' → ')
    .map((s) => s.trim().split(/\s+/)[1]?.split('(')[0] ?? '')
    .filter((n) => n && n !== me);
}

/** 문서 화면 결재선 글에서 나 아닌 사람 이름처럼 보이는 말. 원본의 검사를 옮기고 내 부서 이름은 뺐다. */
export function strayNames(text, { me, myDept } = {}) {
  const deptWords = new Set(String(myDept ?? '').split(/[\s()]+/).filter(Boolean));
  return String(text ?? '').split(/[\s()]+/)
    .filter((w) => /^[가-힣]{2,4}$/.test(w) && w !== me && !deptWords.has(w)
      && !/기안|결재|합의|확인|예정|본부|문서|정보|대기|완료|진행|승인|신청|수신|참조/.test(w));
}

/** 결재 대기 중인 증명서 문서를 승인한다. */
export async function approveRequest(page, baseUrl, docId) {
  if (!/^\d{6,25}$/.test(String(docId))) throw new PluginError('VALIDATION', `문서 ID 가 숫자가 아닙니다: ${docId}`);
  await page.goto(`${baseUrl}/gw/app/approval/document/${docId}/popup`, { waitUntil: 'domcontentloaded', timeout: 60_000 });
  await page.waitForLoadState('networkidle', { timeout: 30_000 }).catch(() => {});
  await page.waitForTimeout(6000);
  await dismissDialogs(page);
  return approveDocument(page);
}

/**
 * 결재완료된 건의 PDF 를 받는다. 목록에서 증명서 종류 칸을 누르면 새 창이 뜨며 다운로드가
 * 시작된다. 다운로드 주소가 따로 없어 브라우저의 download 이벤트를 받는다. 받은 파일은 읽고
 * 바로 지운다. 출력이 남긴 빈 창도 닫는다. `Buffer` 를 돌려준다.
 */
export async function downloadCertificate(page, baseUrl, issuecertId, { timeoutMs = 45_000 } = {}) {
  await openCertList(page, baseUrl);
  const context = page.context();
  const before = new Set(context.pages());
  const download = await new Promise((resolve, reject) => {
    const watched = new Set();
    const cleanup = () => {
      clearTimeout(timer);
      context.off('page', watch);
      for (const p of watched) p.off('download', done);
    };
    const done = (d) => { cleanup(); resolve(d); };
    const watch = (p) => { watched.add(p); p.on('download', done); };
    const timer = setTimeout(() => { cleanup(); reject(fail('증명서 다운로드가 시작되지 않았습니다.')); }, timeoutMs);
    for (const p of context.pages()) watch(p);
    context.on('page', watch);
    clickPrintCell(page, issuecertId).catch((e) => { cleanup(); reject(e); });
  });
  try {
    const file = await download.path();
    if (!file) throw fail('증명서 파일을 받지 못했습니다.');
    const data = await fs.readFile(file);
    if (data.subarray(0, 5).toString('latin1') !== '%PDF-') throw fail('받은 증명서 파일이 PDF 가 아닙니다.');
    return data;
  } finally {
    await download.delete().catch(() => {});
    for (const p of context.pages()) if (!before.has(p)) await p.close().catch(() => {});
  }
}
