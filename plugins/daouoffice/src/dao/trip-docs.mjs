import { ensureOk } from './api.mjs';
import { TRIP_FORM_NAME } from './trips.mjs';

/**
 * 이미 올라간 출장 문서를 읽어 중복을 잡는다. 원본 저장소 `trip-docs.mjs` 에서 왔다. 조회만 한다.
 *
 * 옛 출장 문서는 제목이 전부 양식 이름이라 목록만 봐서는 어느 출장인지 알 수 없다. 본문
 * (`docBodyContent`)을 풀어 기간과 출장지를 꺼낸다.
 *
 * 내 기안 문서함과 부서 문서함을 함께 본다. 부서 문서함은 같은 부서 사람이 올린 건을 잡으려고
 * 본다. 다른 부서 사람이 올린 건은 여기에 안 나온다. 사람이 마지막으로 확인해야 한다.
 */

const PAGE = 100;
const listQuery = (page) => `page=${page}&offset=${PAGE}&property=draftedAt&direction=desc&searchtype=&keyword=&fromDate=&toDate=&duration=all`;

/** 본문 표의 라벨들. 라벨 다음 칸이 값이고, 다음 칸이 또 라벨이면 빈 값이다. */
const LABELS = [
  '출장내용', '출장기간', '출장지', '교통편', '출장목적', '비고',
  '동행자 출장자 정보', '프로젝트명', '성명', '직급', '소속',
  '기안자', '기안일', '문서번호',
];

/** 본문 HTML 에서 출장 칸을 읽는다. `{ start, end, place, transport, goal, note }` */
export function parseTripBody(html) {
  const cells = String(html ?? '')
    .replace(/<br\s*\/?>/gi, ' ')
    .split(/<\/t[dh]>/i)
    .map((c) => c.replace(/<[^>]+>/g, '').replace(/&nbsp;/g, ' ').replace(/&amp;/g, '&').replace(/\s+/g, ' ').trim())
    .filter(Boolean);

  const after = (label) => {
    const i = cells.indexOf(label);
    if (i < 0) return '';
    const next = cells[i + 1] ?? '';
    return LABELS.includes(next) ? '' : next;
  };

  // "2026-08-14(금) ~ 2026-08-14(금)" · "2026-08-26 ~ 2026-09-04"
  const [rawStart = '', rawEnd = ''] = after('출장기간').split('~').map((s) => s.trim());
  const day = (s) => s.slice(0, 10);

  return {
    start: day(rawStart),
    end: day(rawEnd) || day(rawStart),
    place: after('출장지'),
    transport: after('교통편'),
    goal: after('출장목적'),
    note: after('비고'),
  };
}

/**
 * 내 기안 문서함과 부서 문서함의 출장 신청을 읽어 온다. 문서마다 본문을 읽는다.
 * `[{ documentId, box, drafter, status, draftedAt, start, end, place, transport, goal, note }]`
 *
 * 문서함은 기안일 최신순으로 100건씩 읽는다. `since`(YYYY-MM-DD)를 주면 그보다 앞에 기안한 문서가
 * 나올 때까지 `maxPages` 쪽까지 넘기고, 그 앞 문서는 보지 않는다. 주지 않으면 첫 쪽만 본다. 부서
 * 문서함이 쪽 번호를 무시하면 새 문서가 없는 쪽에서 멈춘다.
 */
export async function listSubmittedTrips(api, { deptId, formName = TRIP_FORM_NAME, since = null, maxPages = 10 } = {}) {
  const boxes = [
    ['내 기안', (page) => `/gw/api/approval/doclist/draft/all?${listQuery(page)}`],
    ...(deptId ? [['부서', (page) => `/gw/api/approval/deptfolder/draft/${encodeURIComponent(deptId)}?${listQuery(page)}`]] : []),
  ];
  const drafted = (doc) => String(doc.draftedAt ?? '').slice(0, 10);

  const seen = new Map();
  for (const [box, path] of boxes) {
    const ids = new Set();
    for (let page = 0; page < (since ? maxPages : 1); page++) {
      const res = ensureOk(await api(path(page)), `${box} 문서함`);
      const rows = Array.isArray(res.data?.data) ? res.data.data : [];
      const fresh = rows.filter((d) => !ids.has(String(d.documentId)));
      for (const doc of fresh) {
        ids.add(String(doc.documentId));
        if (doc.formName !== formName || (since && drafted(doc) && drafted(doc) < since)) continue;
        if (!seen.has(String(doc.documentId))) seen.set(String(doc.documentId), { box, doc });
      }
      const oldest = rows.map(drafted).filter(Boolean).sort()[0];
      if (rows.length < PAGE || !fresh.length || (since && oldest && oldest < since)) break;
    }
  }

  const trips = [];
  for (const { box, doc } of seen.values()) {
    const detail = ensureOk(await api(`/gw/api/approval/document/${encodeURIComponent(doc.documentId)}`), '출장 문서');
    trips.push({
      documentId: String(doc.documentId),
      box,
      drafter: doc.drafterName ?? '',
      status: doc.docStatusName ?? '',
      draftedAt: String(doc.draftedAt ?? '').slice(0, 10),
      title: doc.title ?? '',
      ...parseTripBody(detail.data?.data?.document?.docBodyContent ?? ''),
    });
  }
  return trips;
}

/** 장소 문자열에서 비교에 쓸 말을 뽑는다. `테스트공장(테스트시), ABC(테스트군)` → [테스트공장, 테스트시, abc, 테스트군] */
export function placeTokens(place) {
  return String(place ?? '')
    .toLowerCase()
    .split(/[^0-9a-z가-힣]+/)
    .filter((t) => t.length >= 2);
}

/** 기간이 하루라도 겹치는가. */
const overlaps = (a, b) => a.start <= b.end && b.start <= a.end;

/** 반려된 문서는 다시 올릴 수 있다. 겹쳐도 참고로만 알린다. */
const DEAD = new Set(['반려']);

/**
 * 이미 올라간 문서 가운데 이 출장과 겹치는 것을 찾는다. `{ strong, weak }`
 *
 * 기간이 겹치고 장소 말이 하나라도 같으면 중복 후보(strong)다. 기간만 겹치면 같은 날 다른
 * 출장일 수 있어 참고(weak)로만 둔다. 반려된 문서는 겹쳐도 참고다.
 */
export function findDuplicates(submitted, trip) {
  const want = placeTokens(trip.place);
  const strong = [];
  const weak = [];
  for (const doc of submitted) {
    if (!doc.start || !overlaps(doc, trip)) continue;
    const shared = placeTokens(doc.place).filter((t) => want.includes(t));
    (shared.length && !DEAD.has(doc.status) ? strong : weak).push({ ...doc, shared });
  }
  return { strong, weak };
}

/** 겹치는 문서 한 줄. 미리보기와 오류 메시지에 쓴다. */
export const describeTripDoc = (d) => `${d.start}${d.end && d.end !== d.start ? `부터 ${d.end}까지` : ''} ${d.place || '(출장지 없음)'} · ${d.drafter || '기안자 모름'} · ${d.status || '상태 모름'} · ${d.box ?? ''}문서함 · 문서 ID ${d.documentId}`;
