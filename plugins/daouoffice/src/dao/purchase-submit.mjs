import path from 'node:path';
import { PluginError } from '../errors.mjs';
import { requireExecute, currentMode, markSent } from '../guard.mjs';
import { dismissDialogs } from './dialogs.mjs';
import { findForm } from './forms.mjs';
import { setApprovalLine, linkDocuments, withdrawDocument } from './approval.mjs';
import { readPurchaseDoc, diffPurchaseDoc, diffLine, isNewForm, describeDiff } from './purchase-doc.mjs';

/**
 * 구매 품의서를 기안해 상신한다. 원본 저장소 `submit-purchases.mjs` 의 DOM 흐름을 옮겼다.
 * 바뀐 것은 기안 부서(계정 프로필에서 읽는다), 결재선(회사 설정), 견적 파일 위치, 실패를
 * `PluginError` 로 던지는 것이다.
 *
 * 한 건마다 두 번 대조한다.
 *
 *   1. 올리기 전   화면에 채운 값을 다시 읽어 `formValues()` 와 비교
 *   2. 올린 뒤     서버 문서(`readPurchaseDoc`)를 칸별로 읽어 비교, 상태 `진행중`, 결재선
 *
 * 하나라도 다르면 실패다. 상태가 `진행중` 이라는 것만으로 성공이라고 하지 않는다.
 *
 * 선택(`option`)은 원본 CLI 의 두 플래그와 같다.
 *
 *   fix      이미 올린 문서를 고친다. 진행중이고 상신취소가 되면 취소하고 같은 문서로 다시 올린다.
 *            임시저장이면 그대로 고친다. 결재자가 처리했으면 멈춘다.
 *   refile   최종 결재자가 빠진 옛 문서를 새 결재선으로 다시 올린다. 옛 문서가 진행중이고 상신취소가
 *            되면 취소하고 같은 문서로, 임시저장이면 그 문서로, 아니면 새 문서로 올리고 결재가 끝난
 *            옛 문서를 관련문서로 연결한다. 옛 문서에 이미 최종 결재자가 있으면 거부한다.
 *
 * 상신취소로 돌아온 문서에는 **예전 첨부가 그대로 남는다.** 원하는 목록에 없는 첨부는 먼저 지운다.
 */

const conflict = (message) => new PluginError('CONFLICT', message);
const upstream = (message) => new PluginError('UPSTREAM', message);

/**
 * 무엇을 할지 정한다. **읽기만 한다.** 미리보기와 실행이 같은 함수를 부른다(실행은 새로 읽는다).
 *
 * 돌려주는 값 `{ option, editId, withdraw, links, refileFrom, newForm, existing, steps, old }`
 *   editId    고쳐 올릴 문서. null 이면 새 문서
 *   withdraw  먼저 상신취소할 문서
 *   links     새 문서에 연결할 옛 문서 `[{ id, docNum }]`
 *   newForm   고칠 문서가 구입일자 칸을 가진 새 양식인가(새 문서면 true)
 */
export async function planSubmission({ page, api, entry, option = null, formName, approvers }) {
  const plan = {
    option, editId: entry.docId ?? null, withdraw: null, links: [], refileFrom: null,
    newForm: true, existing: [], steps: [], old: null,
  };

  if (option === 'refile') {
    const oldId = entry.refiledFrom ? null : entry.docId;
    if (!oldId) {
      throw conflict(entry.refiledFrom
        ? `이미 다시 올린 품의입니다 (옛 문서 ${entry.refiledFrom}, 새 문서 ${entry.docId}).`
        : '다시 올릴 옛 문서가 없습니다. 아직 올리지 않은 품의는 refile 없이 올립니다.');
    }
    const old = await readPurchaseDoc(page, api, oldId);
    plan.old = { id: oldId, status: old.status, docNum: old.docNum, line: old.line.map((a) => a.name) };
    const last = approvers[approvers.length - 1];
    if (last && old.line.slice(1).some((a) => a.name === last.name)) {
      throw conflict(`옛 문서에 이미 최종 결재자(${last.name})가 있습니다. 다시 올리지 않습니다.`);
    }
    if (old.status === '임시저장') {
      // 앞선 실행에서 상신취소만 되고 멈춘 문서다. 같은 문서로 이어서 올린다.
      plan.editId = oldId;
      plan.steps.push(`옛 문서 ${oldId} 가 임시저장이라 같은 문서를 고쳐 새 결재선으로 올립니다.`);
    } else if (old.status === '진행중' && old.canCancel) {
      plan.editId = oldId;
      plan.withdraw = oldId;
      plan.steps.push(`옛 문서 ${oldId} 를 상신취소하고 같은 문서를 새 결재선으로 다시 올립니다.`);
    } else {
      plan.editId = null;
      if (old.status === '완료' && old.docNum) plan.links.push({ id: String(oldId), docNum: old.docNum });
      plan.steps.push(`옛 문서 ${oldId} (${old.status || '상태 모름'}) 는 그대로 두고 새 문서로 올립니다`
        + `${plan.links.length ? `. 옛 문서 ${old.docNum} 를 관련문서로 연결합니다.` : '.'}`);
    }
    plan.refileFrom = oldId;
  } else if (option === 'fix') {
    if (!plan.editId) throw new PluginError('VALIDATION', '고칠 문서 ID 가 없습니다. 아직 올리지 않은 품의는 fix 없이 올립니다.');
    const cur = await readPurchaseDoc(page, api, plan.editId);
    if (cur.status === '진행중') {
      if (!cur.canCancel) throw conflict(`결재가 이미 진행돼 상신취소가 안 됩니다 (문서 ${plan.editId}). 고칠 수 없습니다.`);
      plan.withdraw = plan.editId;
      plan.steps.push(`문서 ${plan.editId} 를 상신취소하고 같은 문서를 고쳐 다시 올립니다.`);
    } else if (cur.status !== '임시저장') {
      throw conflict(`${cur.status || '상태를 모르는'} 문서는 고치지 않습니다 (문서 ${plan.editId}).`);
    }
  }

  if (plan.editId) {
    const doc = await readPurchaseDoc(page, api, plan.editId);
    if (doc.formName !== formName) throw new PluginError('VALIDATION', `문서 ${plan.editId} 는 ${doc.formName || '다른 양식'} 문서입니다.`);
    if (!plan.withdraw && doc.status !== '임시저장') {
      throw conflict(`문서 ${plan.editId} 가 임시저장이 아닙니다 (${doc.status || '상태 모름'}). 이미 올라갔는지 확인해 주세요. 고치려면 fix 를 씁니다.`);
    }
    plan.newForm = isNewForm(doc.fields);
    plan.existing = doc.attachments;
    if (!plan.withdraw && option !== 'refile') plan.steps.push(`임시저장 문서 ${plan.editId} 를 고쳐 올립니다.`);
    if (!plan.newForm) plan.steps.push('옛 양식 문서라 배송일자 칸을 채웁니다(구입일자, 구입수량, 공급가액 칸이 없습니다).');
  } else if (option !== 'refile') {
    plan.steps.push('새 문서로 기안합니다.');
  }
  return plan;
}

/**
 * 결재요청을 누른 뒤나 상신취소한 뒤의 실패를 사람이 판단할 수 있는 오류로 바꾼다.
 *
 *   sent      확인 창의 버튼을 눌렀다. 올라갔는지 모른다. `afterWrite` 로 알려 다시 올리지 않게 한다
 *   withdrew  상신취소는 됐고 다시 올리지는 못했다. 문서는 임시저장이다
 */
export function failureAfter(e, { sent = false, withdrew = false, docId = null, withdrawnId = null } = {}) {
  const cause = e instanceof PluginError ? e.message : String(e?.message ?? e).split('\n')[0].slice(0, 160);
  if (sent) {
    return new PluginError('UPSTREAM',
      `결재요청을 누른 뒤 결과를 확인하지 못했습니다${docId ? ` (문서 ${docId})` : ''}. 올라갔을 수 있으니 다시 올리기 전에 다우오피스 내 기안 문서에서 확인해 주세요. 원인: ${cause}`,
      { cause: e, data: { afterWrite: true, docId: docId ?? null, withdrew } });
  }
  if (withdrew) {
    return new PluginError('UPSTREAM',
      `${cause}${cause.includes('\n') ? '\n' : ' '}문서는 상신취소된 상태입니다 (문서 ${withdrawnId}). 고쳐 다시 올리려면 fix 로 다시 요청해 주세요.`,
      { cause: e, data: { afterWrite: true, withdrew: true, docId: withdrawnId } });
  }
  return e;
}

/**
 * 문서함의 `draftedAt` 을 ms 로. 시간대가 없으면 KST 로 읽는다(다우오피스 화면의 시각은 KST 다).
 * UNVERIFIED: 문서함 행의 draftedAt 형식은 관측하지 못했다. ISO(시간대 포함)와 `YYYY-MM-DD HH:mm(:ss)` 를 받는다.
 */
export function draftedAtMs(s) {
  const t = String(s ?? '').trim();
  if (/(Z|[+-]\d{2}:?\d{2})$/i.test(t)) return Date.parse(t);
  const m = t.match(/^(\d{4}-\d{2}-\d{2})[ T](\d{2}:\d{2})(:\d{2}(\.\d+)?)?$/);
  return m ? Date.parse(`${m[1]}T${m[2]}${m[3] ?? ':00'}+09:00`) : NaN;
}

/**
 * 방금 올린 문서를 내 기안 문서 목록에서 고른다. 양식과 제목이 같고, 임시저장이 아니고, 옛 문서가
 * 아니고, 10분 안에 올라간 것. 다우오피스 시계가 조금 빠를 수 있어 미래 쪽으로 1분까지 받는다.
 */
export function pickSubmittedDocId(list, { formName, title, exclude, now = Date.now(), maxAgeMs = 10 * 60_000, skewMs = 60_000 }) {
  const hit = (list ?? []).find((d) => {
    if (d.formName !== formName || d.title !== title || d.docStatusName === '임시저장') return false;
    if (exclude != null && String(d.documentId) === String(exclude)) return false;
    const age = now - draftedAtMs(d.draftedAt);
    return age >= -skewMs && age < maxAgeMs;
  });
  return hit ? String(hit.documentId) : null;
}

/**
 * 채우고, 첨부하고, 화면 대조, 결재선, 상신, 문서 ID 찾기, 서버 대조까지 한다. 승인된 실행에서만 돈다.
 *
 * `onWithdrawn({ docId })` 는 상신취소가 끝난 직후, `onSubmitted({ docId })` 는 결재요청 확인을 누른
 * 직후에 불린다(문서 ID 를 아직 모를 수 있다). 부르는 쪽은 여기서 기록해, 뒤에서 실패해도 같은 품의가
 * 두 번 올라가지 않게 하고 문서가 상신취소된 채라는 것을 남긴다.
 *
 * 확인 버튼을 누른 뒤의 실패는 `afterWrite` 가 붙은 UPSTREAM 이다(`failureAfter`).
 *
 * 돌려주는 값 `{ docId, status, docNum, diffs, line, withdrew, review }`. 서버 대조가 어긋나도
 * 문서는 이미 올라갔으므로 던지지 않고 `diffs` 로 돌려준다.
 */
export async function submitPurchase({
  page, api, baseUrl, deptId, want, approvers, plan, quoteDir, formName,
  projectBudget, cardMethod, onWithdrawn = () => {}, onSubmitted = () => {}, logger, now = Date.now,
  deps = {},
}) {
  // 시험이 결재선 편집기 같은 큰 DOM 흐름을 바꿔 끼운다. 기본은 실제 함수다.
  const { withdraw = withdrawDocument, findFormBy = findForm, link = linkDocuments, setLine = setApprovalLine } = deps;
  if (currentMode() !== 'execute') throw new PluginError('INTERNAL', '승인 전에는 실행할 수 없는 동작입니다: 구매 품의서 상신');
  const log = (msg, fields) => logger?.info?.(`purchase: ${msg}`, fields);
  const accept = (d) => { d.accept().catch(() => {}); };
  page.on('dialog', accept);
  let withdrew = false;
  let sent = false;
  let docId = plan.editId ?? null;
  try {
    if (plan.withdraw) {
      const status = await withdraw(page, api, baseUrl, plan.withdraw);
      if (status !== '임시저장') throw upstream(`상신취소 뒤 문서 ${plan.withdraw} 가 임시저장으로 돌아가지 않았습니다 (${status || '상태 모름'}).`);
      withdrew = true;
      log('withdrawn', { docId: plan.withdraw });
      await onWithdrawn({ docId: plan.withdraw });
    }

    let existing = [];
    let editId = plan.editId;
    if (editId) {
      const doc = await readPurchaseDoc(page, api, editId);
      if (doc.formName !== formName) throw new PluginError('VALIDATION', `문서 ${editId} 는 ${doc.formName || '다른 양식'} 문서입니다.`);
      if (doc.status !== '임시저장') throw conflict(`문서 ${editId} 가 임시저장이 아닙니다 (${doc.status || '상태 모름'}). 이미 올라갔는지 확인해 주세요.`);
      existing = doc.attachments;
      await page.goto(`${baseUrl}/gw/app/approval/document/${editId}`, { waitUntil: 'domcontentloaded', timeout: 60_000 });
    } else {
      if (!deptId) throw new PluginError('VALIDATION', '기안 부서를 모릅니다. 조직도에서 부서를 읽지 못했으면 관리자가 회사 설정 departments 에 넣어야 합니다.');
      const form = await findFormBy(api, formName);
      await page.goto(`${baseUrl}/gw/app/approval/document/new/${encodeURIComponent(deptId)}/${encodeURIComponent(form.id)}`,
        { waitUntil: 'domcontentloaded', timeout: 60_000 });
    }
    await page.waitForLoadState('networkidle', { timeout: 30_000 }).catch(() => {});
    await page.waitForTimeout(5000);
    // 편집 화면이 늦게 뜨면 칸을 하나도 못 찾는다(상신취소 직후 한 번 있었다).
    await page.waitForSelector('#subject', { timeout: 30_000 }).catch(() => {});
    await dismissDialogs(page);

    const fillErrors = await page.evaluate(fillFormInPage, want);
    if (fillErrors.length) throw upstream(`구매 품의서 칸을 채우지 못했습니다: ${fillErrors.join(', ')}. 양식이 바뀌었을 수 있습니다.`);

    // 원하는 목록에 없는 첨부(상신취소 전의 옛 캡처)는 먼저 지운다.
    for (const stale of existing.filter((f) => !want.attachments.includes(f))) {
      await removeAttachment(page, stale);
      log('stale attachment removed');
    }
    existing = existing.filter((f) => want.attachments.includes(f));
    for (const file of want.attachments) {
      if (existing.includes(file)) continue;
      await page.locator('input[type=file][name=file]').first().setInputFiles(path.join(quoteDir, file));
      await page.waitForFunction(
        (name) => {
          const th = [...document.querySelectorAll('th')].find((t) => t.innerText.trim() === '파일첨부');
          return (th?.nextElementSibling?.innerText ?? '').includes(name);
        },
        file, { timeout: 60_000, polling: 1000 },
      ).catch(() => { throw upstream(`견적 파일이 올라가지 않았습니다: ${file}`); });
    }

    // 1차 대조: 화면에 들어간 값
    const onScreen = await page.evaluate(readFormInPage);
    if (!onScreen.newForm && !want.deliveryDate) {
      throw new PluginError('VALIDATION', '옛 양식 문서라 배송일자가 필요합니다. draft_purchase 로 deliveryDate 를 넣어 주세요.');
    }
    const screenDiffs = compareForm(onScreen, want);
    if (screenDiffs.length) {
      throw upstream(`화면에 채운 값이 기대와 달라 상신하지 않았습니다:\n${screenDiffs.map((d) => `- ${describeDiff(d)}`).join('\n')}`);
    }
    log('screen check passed');
    const review = await page.screenshot({ fullPage: true, type: 'jpeg', quality: 70 }).catch(() => null);

    if (plan.links.length) {
      requireExecute('관련문서 연결');
      const linked = await link(page, plan.links);
      const missing = plan.links.filter((d) => !linked.includes(d.id));
      if (missing.length) throw upstream(`관련문서 연결이 빠졌습니다: ${missing.map((d) => d.docNum).join(', ')}`);
    }

    const line = await setLine(page, approvers);

    // 결재요청은 확인 창만 연다. 서버가 바뀌는 것은 확인을 누를 때라 그 직전에 markSent 를 부른다.
    requireExecute('구매 품의서 결재요청');
    await page.evaluate(() => {
      const b = [...document.querySelectorAll('a')].find((e) => e.id === 'act_draft' && e.getBoundingClientRect().width > 0);
      if (!b) throw new Error('결재요청 버튼이 없다');
      b.click();
    }).catch((e) => { throw upstream(`결재요청 버튼을 누르지 못했습니다: ${String(e?.message ?? e).split('\n')[0].slice(0, 80)}`); });
    // 결재자가 있으면 기안의견 창(.layer_approval), 없으면 바로 완료 경고(#gpopupLayer p.q)
    const kind = await page.waitForFunction(() => {
      const shown = (el) => el && el.getBoundingClientRect().width > 0;
      if (shown(document.querySelector('#gpopupLayer p.q'))) return 'warn';
      if (shown(document.querySelector('.go_popup.layer_approval'))) return 'ap';
      return false;
    }, null, { timeout: 60_000, polling: 500 }).then((h) => h.jsonValue());
    if (kind === 'warn') throw upstream('결재자가 없다는 경고가 떴습니다. 상신하지 않았습니다.');
    // 여기서부터 되돌릴 수 없다. 이 뒤의 실패는 "올라갔는지 모른다" 로 알린다.
    markSent('구매 품의서 결재요청 확인');
    sent = true;
    await page.click('.go_popup.layer_approval a.btn_major_s');
    await onSubmitted({ docId });
    await page.waitForTimeout(10_000);

    // 새 문서는 상신 뒤 주소에 ID 가 실리는데, 늦게 바뀌면 주소에 없다. 그때는 내 기안함에서 같은
    // 제목으로 방금 올라간 문서를 찾는다.
    docId ??= page.url().match(/document\/(\d{15,})/)?.[1] ?? null;
    for (let i = 0; !docId && i < 10; i++) {
      const l = (await api('/gw/api/approval/doclist/draft/all?page=0&offset=10&property=draftedAt&direction=desc&searchtype=&keyword=&duration=all')).data?.data ?? [];
      docId = pickSubmittedDocId(l, { formName, title: want.title, exclude: plan.refileFrom, now: now() });
      if (!docId) await page.waitForTimeout(3000);
    }
    if (!docId) throw upstream('문서 ID 를 읽지 못했습니다');
    docId = String(docId);
    await onSubmitted({ docId });

    // 2차 대조: 서버에 저장된 문서
    const doc = await readPurchaseDoc(page, api, docId);
    const diffs = diffPurchaseDoc(doc, want, { projectBudget, cardMethod });
    if (doc.status !== '진행중') diffs.push({ label: '상태', got: doc.status, want: '진행중' });
    const lineDiff = diffLine(doc, approvers);
    if (lineDiff) diffs.push(lineDiff);
    log('server check', { docId, mismatches: diffs.length });
    return { docId, status: doc.status, docNum: doc.docNum, diffs, line, withdrew, review };
  } catch (e) {
    throw failureAfter(e, { sent, withdrew, docId, withdrawnId: plan.withdraw });
  } finally {
    page.off('dialog', accept);
  }
}

/** 라벨 칸을 찾아 값을 넣는다(페이지 안에서 돈다). 제목은 `#subject` 다. 못 넣은 칸 목록을 돌려준다. */
export function fillFormInPage(v) {
  const norm = (t) => String(t ?? '').replace(/[\s ]/g, '');
  const cell = (label) => [...document.querySelectorAll('td')].find((c) => norm(c.innerText) === norm(label) && c.nextElementSibling)?.nextElementSibling;
  const fire = (el) => { el.dispatchEvent(new Event('input', { bubbles: true })); el.dispatchEvent(new Event('change', { bubbles: true })); };
  const errors = [];
  const text = (label, value, nth = 0) => {
    const el = cell(label)?.querySelectorAll('input[type=text], textarea')[nth];
    if (!el) { errors.push(`없음 ${label}[${nth}]`); return; }
    el.value = value; fire(el);
  };
  const select = (label, value, nth = 0) => {
    const el = cell(label)?.querySelectorAll('select')[nth];
    if (!el) { errors.push(`없음 ${label} select[${nth}]`); return; }
    const opt = [...el.options].find((o) => o.text === value);
    if (!opt) { errors.push(`옵션없음 ${label}: ${value}`); return; }
    el.value = opt.value; fire(el);
  };
  const radio = (label, value) => {
    const el = [...(cell(label)?.querySelectorAll('input[type=radio]') ?? [])].find((r) => r.value === value);
    if (!el) { errors.push(`옵션없음 ${label}: ${value}`); return; }
    el.click(); el.checked = true; fire(el);
  };

  const subject = document.getElementById('subject');
  if (!subject) errors.push('없음 #subject'); else { subject.value = v.title; fire(subject); }

  // 새 양식: 배송일자 대신 구입일자(결제일), 구입수량, 총금액을 공급가액과 부가세로 나눈다.
  // 그전에 올린 문서는 상신취소해도 옛 칸을 그대로 갖고 있어 두 양식을 다 채운다.
  const newForm = Boolean(cell('구입일자'));
  radio('예산', v.budget);
  if (v.project) select('예산', v.project);
  if (newForm && v.subProject) radio('예산', v.subProject);
  text('구매품명', v.item);
  text('업체명', v.vendor);
  if (newForm) {
    text('구입수량', v.quantity);
    text('구입일자', v.purchaseDate);
  } else {
    text('배송일자', v.deliveryDate);
  }
  text('구입목적', v.purpose);
  if (newForm) {
    text('총금액(vat포함)', v.amount, 0);
    text('총금액(vat포함)', v.supply, 1);
    text('총금액(vat포함)', v.vat, 2);
  } else {
    text('금액(vat포함)', v.amount);
  }
  radio('지급요청일', v.payWhen);
  select('지급방법', v.payMethod, 0);
  if (v.cardLast4) { text('지급방법', v.cardLast4); select('지급방법', v.cardIssuer, 1); }
  text('지급계좌(은행)', v.account);
  text('예금주', v.accountHolder);
  text('기타(링크)', v.note);
  return errors;
}

/** 화면에 들어간 값을 다시 읽는다(페이지 안에서 돈다). */
export function readFormInPage() {
  const norm = (t) => String(t ?? '').replace(/[\s ]/g, '');
  const cell = (label) => [...document.querySelectorAll('td')].find((c) => norm(c.innerText) === norm(label) && c.nextElementSibling)?.nextElementSibling;
  const texts = (label) => [...(cell(label)?.querySelectorAll('input[type=text], textarea') ?? [])].map((e) => e.value);
  const selects = (label) => [...(cell(label)?.querySelectorAll('select') ?? [])].map((e) => e.options[e.selectedIndex]?.text ?? '');
  const radio = (label, nth = 0) => [...(cell(label)?.querySelectorAll('input[type=radio]') ?? [])].filter((r) => r.checked)[nth]?.value ?? '';
  const files = (() => {
    const th = [...document.querySelectorAll('th')].find((t) => t.innerText.trim() === '파일첨부');
    return th?.nextElementSibling?.innerText ?? '';
  })();
  const fileNames = [...document.querySelectorAll('#imgWrap li[data-name], #fileWrap li[data-name]')]
    .map((li) => li.getAttribute('data-name'));
  return {
    title: document.getElementById('subject')?.value ?? '',
    budget: radio('예산'), project: selects('예산')[0] ?? '', subProject: radio('예산', 1),
    item: texts('구매품명')[0], vendor: texts('업체명')[0],
    newForm: Boolean(cell('구입일자')),
    quantity: texts('구입수량')[0], purchaseDate: texts('구입일자')[0], deliveryDate: texts('배송일자')[0],
    purpose: texts('구입목적')[0], amount: texts('총금액(vat포함)')[0] ?? texts('금액(vat포함)')[0],
    supply: texts('총금액(vat포함)')[1], vat: texts('총금액(vat포함)')[2], payWhen: radio('지급요청일'),
    payMethod: selects('지급방법')[0], cardLast4: texts('지급방법')[0] ?? '', cardIssuer: selects('지급방법')[1] ?? '',
    account: texts('지급계좌(은행)')[0], accountHolder: texts('예금주')[0], note: texts('기타(링크)')[0],
    filesText: files, fileNames,
  };
}

/** 1차 대조. 화면에서 읽은 값(`readFormInPage`)과 기대값의 다른 칸. */
export function compareForm(got, want) {
  const diffs = [];
  const formKeys = got.newForm ? ['subProject', 'quantity', 'purchaseDate', 'supply', 'vat'] : ['deliveryDate'];
  for (const k of ['title', 'budget', 'project', ...formKeys, 'item', 'vendor', 'purpose', 'amount',
    'payWhen', 'payMethod', 'cardLast4', 'cardIssuer', 'account', 'accountHolder', 'note']) {
    if (String(got[k] ?? '') !== String(want[k] ?? '')) diffs.push({ label: k, got: got[k], want: want[k] });
  }
  for (const f of want.attachments) {
    if (!String(got.filesText ?? '').includes(f)) diffs.push({ label: '첨부', got: '(없음)', want: f });
  }
  // 빠진 것만 보면 남은 옛 첨부를 놓친다. 넘치는 것도 본다.
  for (const f of got.fileNames ?? []) {
    if (!want.attachments.includes(f)) diffs.push({ label: '첨부', got: f, want: '(없어야 함)' });
  }
  return diffs;
}

/** 기안 화면에서 첨부 하나를 지운다. 확인 창이 뜨면 승인한다. */
async function removeAttachment(page, name) {
  requireExecute('옛 첨부 삭제');   // 화면 안의 편집이다. 서버는 상신할 때 바뀐다
  // 삭제 아이콘은 마우스를 올렸을 때만 엉뚱한 좌표에 그려진다. 좌표로 누르면 다른 것을 누른다.
  // 요소에 직접 click 을 보낸다.
  const found = await page.evaluate((n) => {
    const li = [...document.querySelectorAll('#imgWrap li[data-name], #fileWrap li[data-name]')]
      .find((x) => x.getAttribute('data-name') === n);
    li?.querySelector('.ic_del')?.click();
    return Boolean(li);
  }, name);
  if (!found) throw upstream(`옛 첨부 ${name} 을 화면에서 찾지 못했습니다.`);
  const confirm = page.locator('.go_popup a.btn_major_s:visible').first();
  if (await confirm.count().catch(() => 0)) await confirm.click({ timeout: 5000 }).catch(() => {});
  await page.waitForFunction(
    (n) => ![...document.querySelectorAll('#imgWrap li[data-name], #fileWrap li[data-name]')].some((li) => li.getAttribute('data-name') === n),
    name, { timeout: 15_000, polling: 500 },
  ).catch(() => { throw upstream(`옛 첨부 ${name} 이 지워지지 않았습니다.`); });
}
