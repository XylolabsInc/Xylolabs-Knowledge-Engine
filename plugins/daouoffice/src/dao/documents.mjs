import { ensureOk } from './api.mjs';
import { PluginError } from '../errors.mjs';

/**
 * 결재 문서와 근태·연차 조회. 전부 읽기다.
 *
 *   GET /gw/api/approval/todo/count                     결재할 문서 건수 (data.docCount)
 *   GET /gw/api/approval/doclist/draft/all?…            내 기안 문서 전체, 페이지네이션 동작
 *   GET /gw/api/approval/document/{docId}               문서 상세
 *   GET /api/attend/normal/gadget/today                 오늘 근무 상태 (data.workStatusName)
 *   GET /api/leave/normal/gadget                        연차 (data.formattedRemain 등)
 *
 * 홈 가젯(`/gw/api/approval/home/*`)은 5건 고정이라 전체를 못 본다. 문서함 API 를 쓴다.
 * 문서함 목록 행의 필드는 `documentId`, `formName`, `title`, `drafterName`, `draftedAt`,
 * `docStatusName` 이다.
 */

export const DOC_ID = /^\d{6,25}$/;

export function checkDocId(docId) {
  if (!DOC_ID.test(String(docId ?? ''))) {
    throw new PluginError('VALIDATION', `문서 ID 는 숫자입니다: ${docId}`);
  }
  return String(docId);
}

const listQuery = ({ page = 0, offset = 20 } = {}) =>
  `page=${page}&offset=${offset}&property=draftedAt&direction=desc&searchtype=&keyword=&fromDate=&toDate=&duration=all`;

/** 문서함 목록 한 줄을 쓰기 좋게 고친다. */
export const normalizeListDoc = (d) => ({
  id: String(d.documentId ?? d.id ?? ''),
  formName: d.formName ?? '',
  title: d.title ?? '',
  drafter: d.drafterName ?? '',
  draftedAt: d.draftedAt ?? '',
  status: d.docStatusName ?? '',
});

const listOf = (r) => (Array.isArray(r.data?.data) ? r.data.data : []);

/** 결재할 문서 건수. */
export async function todoCount(api) {
  const r = ensureOk(await api('/gw/api/approval/todo/count'), '결재할 문서 건수');
  const n = Number(r.data?.data?.docCount);
  if (!Number.isFinite(n)) throw new PluginError('UPSTREAM', '결재할 문서 건수를 읽지 못했습니다.');
  return n;
}

/**
 * 결재할 문서 목록.
 * UNVERIFIED: shape not observed — `/gw/api/approval/todo` appears in the endpoint list but its query
 * string and rows were never captured. Rows are assumed to match the doclist rows; when nothing comes
 * back the five-item home gadget is used instead.
 */
export async function listTodo(api, { offset = 20 } = {}) {
  const r = await api(`/gw/api/approval/todo?${listQuery({ offset })}`);
  if (r.ok && listOf(r).length) return { docs: listOf(r).map(normalizeListDoc), partial: false };
  const home = await api('/gw/api/approval/home/todo');
  if (home.ok) return { docs: listOf(home).map(normalizeListDoc), partial: true };
  ensureOk(r, '결재할 문서 목록');
  return { docs: [], partial: false };
}

/**
 * 내 기안 문서 전체. 100건씩 `maxPages` 쪽까지 읽는다. 같은 제목이 이미 올라갔는지 볼 때 쓴다.
 * 행은 목록 원본 그대로다(`documentId`, `formName`, `title`, `docStatusName`, `draftedAt`).
 */
export async function listAllDrafts(api, { maxPages = 20 } = {}) {
  const out = [];
  for (let page = 0; page < maxPages; page++) {
    const r = ensureOk(await api(`/gw/api/approval/doclist/draft/all?${listQuery({ page, offset: 100 })}`), '내 기안 문서');
    const rows = listOf(r);
    out.push(...rows);
    if (rows.length < 100) break;
  }
  return out;
}

/** 내 기안 문서. 최신순. */
export async function listDrafts(api, { page = 0, offset = 20 } = {}) {
  const r = ensureOk(await api(`/gw/api/approval/doclist/draft/all?${listQuery({ page, offset })}`), '내 기안 문서');
  return listOf(r).map(normalizeListDoc);
}

/**
 * 문서 하나를 읽는다. 본문은 입력 요소가 아니라 `span[data-type]` 으로 저장된다.
 *
 *   text·textarea·calendar  data-value 에 값
 *   radio                   고른 쪽에만 data-select-option="true"
 *   cSel (선택)              data-selectval 에 고른 값 (value 속성은 첫 선택지라 쓰지 않는다)
 *
 * 제목은 본문이 아니라 `document.title` 이다. 결재선은 `apprFlow.activityGroups`, 수신자는
 * `docInfo.docReceptionReaders[].reader.fullName`, 참조자는 `docInfo.docReferenceReaders`.
 * `document.references` 는 참조자가 아니라 연결 문서다.
 *
 * 본문 표는 페이지의 DOMParser 로 푼다(스크립트도 이미지도 실행되지 않는다).
 */
export async function readDocument(page, api, docId) {
  const id = checkDocId(docId);
  const r = await api(`/gw/api/approval/document/${id}`);
  if (r.status === 404) throw new PluginError('NOT_FOUND', `문서 ${id} 를 찾지 못했습니다.`);
  if (r.status === 403) throw new PluginError('NOT_FOUND', `문서 ${id} 를 볼 권한이 없습니다.`);
  ensureOk(r, `문서 ${id}`);
  const data = r.data?.data ?? {};
  const doc = data.document ?? {};
  if (!doc || (!doc.title && !doc.docStatusName && !doc.formName)) {
    throw new PluginError('NOT_FOUND', `문서 ${id} 를 찾지 못했습니다.`);
  }

  const fields = await page.evaluate((html) => {
    const root = new DOMParser().parseFromString(html, 'text/html');
    const clean = (t) => String(t ?? '').replace(/\s+/g, ' ').trim();
    const out = [];
    for (const cell of root.querySelectorAll('td, th')) {
      const spans = [...cell.querySelectorAll('[data-type]')];
      if (!spans.length) continue;
      const values = [];
      for (const s of spans) {
        const t = s.getAttribute('data-type');
        if (t === 'radio') {
          if (s.getAttribute('data-select-option') === 'true') values.push(s.getAttribute('data-value') ?? '');
        } else if (t === 'cSel') {
          values.push(s.getAttribute('data-selectval') ?? '');
        } else {
          values.push(s.getAttribute('data-value') ?? '');
        }
      }
      const label = clean(cell.previousElementSibling?.textContent);
      if (values.some((v) => v !== '')) out.push({ label, values });
    }
    return out;
  }, String(doc.docBodyContent ?? ''));

  const groups = data.apprFlow?.activityGroups ?? [];
  const readers = (list) => (list ?? []).map((x) => x.reader?.fullName ?? x.reader?.name ?? '').filter(Boolean);
  return {
    id,
    title: doc.title ?? '',
    formName: doc.formName ?? '',
    status: doc.docStatusName ?? '',
    // docNum and drafterName are read this way by the source's inspection report and photo comments.
    // UNVERIFIED: shape not observed — draftedAt on the detail response.
    docNum: doc.docNum ?? doc.documentNumber ?? '',
    drafter: doc.drafterName ?? doc.drafter?.name ?? '',
    draftedAt: doc.draftedAt ?? '',
    attachments: (doc.attaches ?? []).map((a) => a.name).filter(Boolean),
    line: groups.flatMap((g) => (g.activities ?? []).map((a) => ({
      group: g.name ?? '', name: a.userName ?? '', display: a.displayName ?? '', status: a.statusName ?? '',
    }))),
    recipients: readers(data.docInfo?.docReceptionReaders),
    referers: readers(data.docInfo?.docReferenceReaders),
    fields,
    canCancel: Boolean(data.actionCheck?.isAllowCancelDraftUser),
  };
}

/** 오늘 근무 상태. */
export async function attendanceToday(api) {
  const r = ensureOk(await api('/api/attend/normal/gadget/today'), '오늘 근태');
  const d = r.data?.data ?? {};
  if (!d.workStatusName && d.workStatusName !== '') throw new PluginError('UPSTREAM', '오늘 근태를 읽지 못했습니다.');
  // UNVERIFIED: shape not observed — only workStatusName and isShowAttendanceButton are confirmed;
  // the clock-in/out and accumulated-time field names below are guesses and are shown only when present.
  return {
    status: d.workStatusName,
    clockIn: d.clockInTime ?? d.startTime ?? d.workStartTime ?? null,
    clockOut: d.clockOutTime ?? d.endTime ?? d.workEndTime ?? null,
    worked: d.formattedWorkTime ?? d.accumulatedWorkTime ?? d.workTime ?? null,
  };
}

/** 연차 총계, 사용, 잔여. */
export async function leaveBalance(api) {
  const r = ensureOk(await api('/api/leave/normal/gadget'), '연차');
  const d = r.data?.data ?? {};
  if (d.formattedRemain === undefined) throw new PluginError('UPSTREAM', '연차를 읽지 못했습니다.');
  // UNVERIFIED: shape not observed — only formattedRemain is confirmed; total/used names are guesses.
  return {
    remain: d.formattedRemain,
    total: d.formattedTotal ?? d.formattedTotalCount ?? null,
    used: d.formattedUsed ?? d.formattedUseCount ?? null,
  };
}
