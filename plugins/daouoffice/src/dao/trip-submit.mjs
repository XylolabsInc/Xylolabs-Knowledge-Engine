import { PluginError } from '../errors.mjs';
import { dismissDialogs } from './dialogs.mjs';
import { editApprovalInfo, submitDocument } from './approval.mjs';
import { listAllDrafts } from './documents.mjs';
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
 * 결재선과 참조자는 한 편집기 세션 안에서 넣는다(`editApprovalInfo()`). 편집기의 확인은 화면
 * 안에서만 유지되므로 편집과 상신을 같은 페이지에서 끝낸다.
 */

const fail = (message) => new PluginError('UPSTREAM', message);
const FIELD_LABELS = { title: '제목', start: '시작일', end: '종료일', place: '출장지', transport: '교통편', goal: '출장목적', note: '비고' };
const BODY_LABELS = { start: '시작일', end: '종료일', place: '출장지', transport: '교통편', goal: '출장목적', note: '비고' };
const norm = (v) => String(v ?? '').replace(/\s+/g, ' ').trim();

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
export async function verifyTrip(api, docId, expected, { approvers = [], referers = [], waitMs = 90_000, sleep = defaultSleep } = {}) {
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

  const line = (data.apprFlow?.activityGroups ?? []).flatMap((g) => (g.activities ?? []).map((a) => a.displayName ?? a.userName ?? ''));
  for (const a of approvers) if (!line.some((l) => String(l).includes(a.name))) mismatches.push(`결재선에 ${a.name} 이 없습니다`);
  const refs = (data.docInfo?.docReferenceReaders ?? []).map((x) => x.reader?.fullName ?? x.reader?.name ?? '');
  for (const p of referers) if (!refs.some((x) => String(x).includes(p.name))) mismatches.push(`참조자에 ${p.name} 이 없습니다`);

  return { status, title: doc.title ?? '', mismatches, line, referers: refs, body };
}

const defaultSleep = (ms) => new Promise((res) => setTimeout(res, ms));

/**
 * 출장 한 건을 기안해 상신한다. **승인된 실행에서만 부른다.** 결재선, 칸, 제목을 채운 뒤 읽어
 * 대조하고, 하나라도 어긋나면 상신하지 않고 멈춘다. `{ docId, line, referers, filled }`
 */
export async function submitTrip(page, api, baseUrl, { trip, title, deptId, formId, approvers, referers }) {
  await openTripDraft(page, baseUrl, { deptId, formId });

  const info = await editApprovalInfo(page, { approvers, referers });
  for (const { name } of approvers) {
    if (!info.line.includes(name)) throw fail(`결재선에 ${name} 이 들어가지 않았습니다: ${info.line}`);
  }
  for (const { name } of referers) {
    if (!info.referers.includes(name)) throw fail(`참조자에 ${name} 이 들어가지 않았습니다: ${info.referers}`);
    if (info.line.includes(name)) throw fail(`${name} 이 결재선에 남아 있습니다: ${info.line}`);
  }

  const expected = { ...trip, title };
  const filled = await fillTripForm(page, expected);
  const missing = Object.entries(filled).filter(([, v]) => /^(없음|옵션없음):/.test(String(v)));
  if (missing.length) {
    throw fail(`출장 양식 칸을 채우지 못했습니다: ${missing.map(([, v]) => v).join(', ')}. 관리자가 양식을 바꿨을 수 있습니다.`);
  }
  const off = compareTripFields(expected, filled);
  if (off.length) throw fail(`채운 값이 기대와 다릅니다: ${off.join('; ')}`);

  await submitDocument(page);

  let docId = page.url().match(/document\/(\d{6,25})/)?.[1] ?? null;
  if (!docId) {
    // 주소가 바뀌지 않으면 내 기안 문서에서 같은 제목의 가장 최근 문서를 찾는다.
    const mine = await listAllDrafts(api, { maxPages: 1 });
    docId = mine.find((d) => d.title === title)?.documentId ?? null;
    if (docId) docId = String(docId);
  }
  if (!docId) throw fail('상신 뒤 문서 ID 를 찾지 못했습니다. 다우오피스 내 기안 문서에서 확인해 주세요.');
  return { docId, line: info.line, referers: info.referers, filled };
}
