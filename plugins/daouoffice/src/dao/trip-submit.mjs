import { PluginError } from '../errors.mjs';
import { dismissDialogs } from './dialogs.mjs';
import {
  openApprovalLineEditor, saveApprovalLineEditor, approvalLineRows, removeFromApprovalLine, dropIntoApprovalLine,
  setApprovalLineType, readApprovalLine, addRecipients, readRecipients, switchEditorTab, submitDocument,
} from './approval.mjs';
import { listDrafts } from './documents.mjs';
import { parseTripBody } from './trip-docs.mjs';

/**
 * 출장 신청서를 기안 창에서 채워 상신한다. 원본 저장소 `submit-trips.mjs` 의 DOM 흐름을 옮겼다.
 * 바뀐 것은 기안 부서(사람마다 계정 정보에서), 결재선과 참조자(회사 설정), 상신(`submitDocument()`,
 * 승인된 실행에서만 돈다), 그리고 상신 뒤 서버 문서를 칸별로 다시 읽어 대조하는 것이다.
 *
 * 기안 화면은 주소로 바로 연다: `/gw/app/approval/document/new/{기안부서ID}/{양식ID}`. 여는 순간
 * 문서 ID 가 발급되지만 저장하지 않으면 문서로 남지 않는다.
 *
 * 입력 칸은 번호(`editorForm_N`)가 아니라 본문 표의 라벨로 찾는다. 관리자가 양식을 고치면 번호가
 * 통째로 밀려, 종료일에 넣을 값이 시작일에 들어간 적이 있다. 날짜 입력은 readonly 에 datepicker
 * 가 붙어 있지만 value 대입이 먹는다. `input` 과 `change` 를 모두 일으켜야 양식이 알아챈다.
 *
 * 결재선과 참조자는 한 편집기 세션 안에서 넣는다(`setTripApprovalInfo()`). 양식 기본 결재선을 비우고
 * 설정의 결재자를 순서대로 넣은 뒤 `타입 이름` 순서가 정확히 같은지 본다. 참조자도 이름이 정확히
 * 같아야 한다. 미리보기에 없던 사람이 결재선이나 참조에 남으면 상신하지 않는다. 편집기의 확인은 화면
 * 안에서만 유지되므로 편집과 상신을 같은 페이지에서 끝낸다.
 */

const fail = (message) => new PluginError('UPSTREAM', message);
const FIELD_LABELS = { title: '제목', start: '시작일', end: '종료일', place: '출장지', transport: '교통편', goal: '출장목적', note: '비고' };
const BODY_LABELS = { start: '시작일', end: '종료일', place: '출장지', transport: '교통편', goal: '출장목적', note: '비고' };
const norm = (v) => String(v ?? '').replace(/\s+/g, ' ').trim();

/** `결재 김대표 → 합의 이합의` 꼴. 기안 행은 뺀다. */
const lineOf = (rows) => rows.filter((r) => r.kind !== '기안').map((r) => `${r.kind} ${r.name}`).join(' → ');
const wantLine = (approvers) => approvers.map((a) => `${a.type ?? '결재'} ${a.name}`).join(' → ');

/** 참조자 표 글(`홍길동(부서), 김철수(부서)`)의 이름들. 비어 있으면 빈 배열. */
export function refererNames(text) {
  const t = String(text ?? '').trim();
  if (!t || t.startsWith('(')) return [];
  return t.split(', ').map((x) => x.replace(/\([^)]*\)\s*$/, '').trim()).filter(Boolean);
}

/** 결재 정보 편집기의 단계들(approval.mjs). 시험에서 갈아 끼운다. */
export const EDITOR = Object.freeze({
  openApprovalLineEditor, saveApprovalLineEditor, approvalLineRows, removeFromApprovalLine, dropIntoApprovalLine,
  setApprovalLineType, readApprovalLine, addRecipients, readRecipients, switchEditorTab,
});

/** 이름 목록이 순서와 상관없이 정확히 같은가. */
const sameNames = (a, b) => [...a].sort().join('\n') === [...b].sort().join('\n');

/**
 * 결재선과 참조자를 한 편집기 세션 안에서 넣는다. 기본 결재선의 기안 아닌 사람을 모두 빼고, 결재자를
 * 순서대로 넣고, 참조자 탭에서 참조자를 넣는다. 결재선은 `타입 이름` 순서가, 참조자는 이름 묶음이
 * 정확히 같아야 한다(`김철` 이 `김철수` 에 맞는 식의 부분 일치는 쓰지 않는다). 양식 기본 참조자처럼
 * 설정에 없는 사람이 남으면 멈춘다. `{ line, rows, referers }`
 */
export async function setTripApprovalInfo(page, { approvers, referers }, ed = EDITOR) {
  await ed.openApprovalLineEditor(page);
  for (let round = 0; round < 10; round++) {
    const other = (await ed.approvalLineRows(page)).find((r) => r.kind !== '기안');
    if (!other) break;
    if (!(await ed.removeFromApprovalLine(page, other.name))) throw fail(`기본 결재선에서 "${other.name}" 을 빼지 못했습니다.`);
  }
  if (lineOf(await ed.approvalLineRows(page))) throw fail(`기본 결재선을 비우지 못했습니다: ${lineOf(await ed.approvalLineRows(page))}`);
  for (const { name, dept, type = '결재' } of approvers) {
    await ed.dropIntoApprovalLine(page, name, { dept });
    if (!(await ed.approvalLineRows(page)).some((r) => r.kind !== '기안' && r.name === name)) throw fail(`결재선에 "${name}" 이 들어가지 않았습니다.`);
    await ed.setApprovalLineType(page, name, type);
  }
  const rows = await ed.approvalLineRows(page);
  if (lineOf(rows) !== wantLine(approvers)) throw fail(`결재선이 기대와 다릅니다: ${lineOf(rows) || '(비어 있음)'} (기대 ${wantLine(approvers)})`);
  // 수신자, 참조자 표도 table.tb_approval_line 이라 결재선은 탭을 바꾸기 전에 읽는다.
  const line = await ed.readApprovalLine(page);

  const refText = referers.length ? await ed.addRecipients(page, referers, 'referer')
    : (await ed.switchEditorTab(page, 'referer'), await ed.readRecipients(page, 'referer'));
  const names = refererNames(refText);
  const want = referers.map((r) => r.name);
  if (!sameNames(names, want)) {
    const extra = names.filter((n) => !want.includes(n));
    throw fail([
      `참조자가 설정과 다릅니다: ${names.join(', ') || '(없음)'} (설정 ${want.join(', ') || '(없음)'}).`,
      extra.length ? `${extra.join(', ')} 은 양식 기본 참조자로 보입니다. 넣어야 하는 사람이면 회사 설정 trip.referers 에 넣어 미리보기에 보이게 해 주세요.` : '',
    ].join(' ').trim());
  }
  if (names.some((n) => rows.some((r) => r.name === n))) throw fail(`참조자가 결재선에도 들어 있습니다: ${line}`);
  await ed.saveApprovalLineEditor(page);
  return { line, rows, referers: names };
}

/** 편집기를 다시 열어 저장된 결재선과 참조자가 그대로인지 본다. 상신 바로 전에 부른다. */
export async function checkTripApprovalInfo(page, { approvers, referers }, ed = EDITOR) {
  await ed.openApprovalLineEditor(page);
  const rows = await ed.approvalLineRows(page);
  await ed.switchEditorTab(page, 'referer');
  const names = refererNames(await ed.readRecipients(page, 'referer'));
  await ed.saveApprovalLineEditor(page);
  const problems = [];
  if (lineOf(rows) !== wantLine(approvers)) problems.push(`결재선 ${lineOf(rows) || '(비어 있음)'} (기대 ${wantLine(approvers)})`);
  if (!sameNames(names, referers.map((r) => r.name))) problems.push(`참조자 ${names.join(', ') || '(없음)'} (기대 ${referers.map((r) => r.name).join(', ') || '(없음)'})`);
  if (problems.length) throw fail(`상신 전에 다시 본 결재 정보가 기대와 다릅니다: ${problems.join('; ')}`);
}

/** 기안 창을 연다. */
export async function openTripDraft(page, baseUrl, { deptId, formId }) {
  await page.goto(`${baseUrl}/gw/app/approval/document/new/${encodeURIComponent(deptId)}/${encodeURIComponent(formId)}`, {
    waitUntil: 'domcontentloaded', timeout: 45_000,
  });
  await page.waitForLoadState('networkidle', { timeout: 25_000 }).catch(() => {});
  await page.waitForTimeout(5000);
  await dismissDialogs(page);
}

/**
 * 제목과 본문 칸을 라벨로 찾아 채우고, 들어간 값을 읽어 돌려준다.
 * 찾지 못한 칸은 `없음:<라벨>`, 선택지에 없는 교통편은 `옵션없음:…` 으로 돌아온다.
 */
export async function fillTripForm(page, values) {
  return page.evaluate((t) => {
    const inputsFor = (label) => {
      const td = [...document.querySelectorAll('td')].find(
        (c) => c.innerText.replace(/\s/g, '') === label && c.nextElementSibling,
      );
      if (!td) return null;
      return [...td.nextElementSibling.querySelectorAll('input, select, textarea')];
    };
    const fire = (el) => {
      el.dispatchEvent(new Event('input', { bubbles: true }));
      el.dispatchEvent(new Event('change', { bubbles: true }));
    };
    const setText = (label, value, nth = 0) => {
      const els = inputsFor(label);
      if (!els || !els[nth]) return `없음:${label}[${nth}]`;
      els[nth].value = value;
      fire(els[nth]);
      return els[nth].value;
    };
    const setSelect = (label, text) => {
      const el = inputsFor(label)?.find((e) => e.tagName === 'SELECT');
      if (!el) return `없음:${label}`;
      const option = [...el.options].find((o) => o.text === text);
      if (!option) return `옵션없음:${text} (가능: ${[...el.options].map((o) => o.text).join('/')})`;
      el.value = option.value;
      fire(el);
      return el.options[el.selectedIndex].text;
    };
    const subject = document.getElementById('subject');
    if (subject) { subject.value = t.title; fire(subject); }
    const out = {
      title: subject ? subject.value : '없음:#subject',
      start: setText('출장기간', t.start, 0),
      end: setText('출장기간', t.end, 1),
      place: setText('출장지', t.place),
      transport: setSelect('교통편', t.transport),
      goal: setText('출장목적', t.goal),
    };
    if (t.note) out.note = setText('비고', t.note);
    return out;
  }, values);
}

/** 채운 값과 기대한 값을 칸별로 대조한다. 어긋난 칸의 설명 목록을 돌려준다. */
export function compareTripFields(expected, got, labels = FIELD_LABELS) {
  const out = [];
  for (const [key, label] of Object.entries(labels)) {
    if (expected[key] === undefined || expected[key] === '') continue;
    const want = norm(expected[key]);
    const have = norm(got?.[key]);
    if (want !== have) out.push(`${label}: "${have || '(비어 있음)'}" (기대 "${want}")`);
  }
  return out;
}


/**
 * 상신한 출장 문서를 서버에서 다시 읽어 대조한다. 첨부가 없어도 상신 직후에는 상태가 빌 수
 * 있어 `waitMs` 까지 기다린다. `{ status, title, mismatches, referers, line }`
 */
/** 결재선, 참조자 응답의 이름. `userName`/`name` 이 있으면 그것, 없으면 표시 이름의 첫 말이다. */
const personName = (plain, display) => String(plain ?? '').trim() || String(display ?? '').trim().split(/[\s(]/)[0];

export async function verifyTrip(api, docId, expected, { approvers = [], referers = [], drafter = null, waitMs = 90_000, sleep = defaultSleep } = {}) {
  const until = Date.now() + waitMs;
  let r = await api(`/gw/api/approval/document/${docId}`);
  while (!r.data?.data?.document?.docStatusName && Date.now() < until) {
    await sleep(5000);
    r = await api(`/gw/api/approval/document/${docId}`);
  }
  const data = r.data?.data ?? {};
  const doc = data.document ?? {};
  const mismatches = [];
  const status = doc.docStatusName ?? '';
  if (status !== '진행중') mismatches.push(`상태: "${status || '(비어 있음)'}" (기대 "진행중")`);
  if (norm(doc.title) !== norm(expected.title)) mismatches.push(`제목: "${doc.title ?? ''}" (기대 "${expected.title}")`);

  const body = parseTripBody(doc.docBodyContent ?? '');
  if (!body.start && !body.place && !body.goal) mismatches.push('본문 칸을 읽지 못해 대조하지 못했습니다');
  else mismatches.push(...compareTripFields(expected, body, BODY_LABELS));

  // 이름은 정확히 맞춘다. 기안자는 빼고 결재자 순서가 같아야 하고, 참조자는 이름 묶음이 같아야 한다.
  const line = (data.apprFlow?.activityGroups ?? []).flatMap((g) => (g.activities ?? []).map((a) => personName(a.userName, a.displayName)));
  const approving = drafter ? line.filter((n, i) => !(i === 0 && n === drafter)) : line;
  const wantNames = approvers.map((a) => a.name);
  if (approving.join(' → ') !== wantNames.join(' → ')) mismatches.push(`결재선: ${approving.join(' → ') || '(비어 있음)'} (기대 ${wantNames.join(' → ')})`);
  const refs = (data.docInfo?.docReferenceReaders ?? []).map((x) => personName(x.reader?.name, x.reader?.fullName));
  if (!sameNames(refs, referers.map((r) => r.name))) mismatches.push(`참조자: ${refs.join(', ') || '(없음)'} (기대 ${referers.map((r) => r.name).join(', ') || '(없음)'})`);

  return { status, title: doc.title ?? '', mismatches, line, referers: refs, body };
}

const defaultSleep = (ms) => new Promise((res) => setTimeout(res, ms));

/**
 * 출장 한 건을 기안해 상신한다. **승인된 실행에서만 부른다.** 결재선, 칸, 제목을 채운 뒤 읽어
 * 대조하고, 하나라도 어긋나면 상신하지 않고 멈춘다. `{ docId, line, referers, filled }`
 */
export async function submitTrip(page, api, baseUrl, { trip, title, deptId, formId, approvers, referers }) {
  // 상신 뒤 주소에서 문서 ID 를 못 읽으면 새로 생긴 문서로 찾는다. 그 전에 있던 문서를 적어 둔다.
  const before = new Set((await listDrafts(api, { offset: 100 })).map((d) => d.id));
  await openTripDraft(page, baseUrl, { deptId, formId });

  const info = await setTripApprovalInfo(page, { approvers, referers });

  const expected = { ...trip, title };
  const filled = await fillTripForm(page, expected);
  const missing = Object.entries(filled).filter(([, v]) => /^(없음|옵션없음):/.test(String(v)));
  if (missing.length) {
    throw fail(`출장 양식 칸을 채우지 못했습니다: ${missing.map(([, v]) => v).join(', ')}. 관리자가 양식을 바꿨을 수 있습니다.`);
  }
  const off = compareTripFields(expected, filled);
  if (off.length) throw fail(`채운 값이 기대와 다릅니다: ${off.join('; ')}`);
  await checkTripApprovalInfo(page, { approvers, referers });

  // submitDocument() 가 확인 버튼 직전에 markSent() 를 부른다. 그 뒤의 실패는 런타임이 결과를 모르는
  // 것(afterWrite)으로 알린다.
  await submitDocument(page);

  let docId = page.url().match(/document\/(\d{6,25})/)?.[1] ?? null;
  if (!docId) {
    // 주소가 바뀌지 않으면 상신 전에 없던 같은 제목의 문서를 찾는다. 하나가 아니면 고르지 않는다.
    const fresh = (await listDrafts(api, { offset: 100 })).filter((d) => d.title === title && !before.has(d.id));
    if (fresh.length === 1) docId = fresh[0].id;
  }
  if (!docId) {
    throw new PluginError('UPSTREAM', '결재요청은 눌렀지만 새 문서의 ID 를 찾지 못했습니다. 다시 올리기 전에 다우오피스 내 기안 문서에서 확인해 주세요.', { data: { afterWrite: true } });
  }
  return { docId, line: info.line, referers: info.referers, filled };
}
