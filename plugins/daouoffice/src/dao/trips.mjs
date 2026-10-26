import { PluginError } from '../errors.mjs';
import { checkWriting } from './writing.mjs';

/**
 * 출장 신청(국내출장신청) 규칙. 원본 저장소 `trips.mjs` 에서 왔다. 출장 목록(`TRIPS`)은
 * 코드에서 빠져 플러그인 DB `trips` 로 갔다.
 *
 * `trips` 행
 *
 *   key     tripKey(trip) = `<start>/<end>/<place>`
 *   status  draft      알고 있지만 올리지 않은 출장
 *           submitted  올린 출장 (docId 가 있으면 그 문서)
 *           elsewhere  같은 출장을 다른 사람이 올렸다
 *           cancelled  취소. 어디에도 쓰지 않는다
 *   data    { start, end, place, transport, goal, note?, titlePlace?, claimName?, claimUntil?, title? }
 *
 * `claimName` 은 지출결의 상세 사용용도에 들어가는 출장지 이름이고, `claimUntil` 은 신청서
 * 기간보다 늦게 끝난 출장의 실제 마지막 날이다. 취소가 아닌 출장은 모두 영수증 없는 출장
 * 경비를 짝짓는 데 쓴다(`trip-expenses.mjs`).
 *
 * 출장목적은 사유다. 출장지를 되풀이하지 않고 무엇을 하러 가는지 쓴다.
 *
 *   나쁨  ○○군청 방문 · 해외 출장 · △△공장 방문
 *   좋음  실증 부지 인허가 협의 · 시제품 성능 시험 입회 · 지원 사업 중간 발표
 *
 * 동행자와 상세 일정은 비고에 적는다. 목적을 모르면 지어내지 않고 사람에게 묻는다.
 */

export const TRIP_STATUS = Object.freeze({
  draft: 'draft',
  submitted: 'submitted',
  elsewhere: 'elsewhere',
  cancelled: 'cancelled',
});

/** 국내출장신청 양식 이름. 양식 ID 는 관리자가 양식을 손대면 바뀌어 이름으로 찾는다. */
export const TRIP_FORM_NAME = '국내출장신청';

/** 양식의 교통편 선택지. 자차 항목이 없어 자차 이동은 `기타` 로 넣는다. */
export const TRANSPORTS = Object.freeze(['고속철도(KTX/SRT)', '시외버스', '항공편', '렌트카', '기타']);

/** 회사 설정 `trip.form` (없으면 국내출장신청). */
export const tripFormName = (cfg) => cfg?.trip?.form || TRIP_FORM_NAME;

/** 회사 설정 `trip.transports` (없으면 양식의 다섯 선택지). */
export const tripTransports = (cfg) => (Array.isArray(cfg?.trip?.transports) && cfg.trip.transports.length
  ? cfg.trip.transports.map(String) : [...TRANSPORTS]);

/** `trips.key`. 같은 날 다른 곳으로 가는 출장이 있어 출장지까지 넣는다. */
export function tripKey({ start, end, place }) {
  return `${start}/${end ?? start}/${String(place ?? '').trim()}`;
}

/** 괄호 속 지역을 뺀 출장지. `테스트공장 (테스트시)` → `테스트공장` */
export const placeName = (place) => String(place ?? '').replace(/\s*\([^)]*\)\s*/g, ' ').replace(/\s+/g, ' ').trim();

/**
 * 출장 신청서 제목. 양식 본문이 안내하는 형식이다.
 *
 *   제목 양식 (출장일자/출장 지/부서/성함) 예시: ('26.01.01/<출장지>/<부서 코드>/<성함>)
 *
 * 제목을 비우면 양식 이름이 들어간다. 부서는 조직도 코드를 그대로 쓰고 대소문자를 바꾸지 않는다.
 * 출장지는 괄호 속 지역을 뺀 이름이고, `titlePlace` 가 있으면 그것을 쓴다.
 * 여러 날이면 `'26.09.16-09.17` 로 쓴다.
 */
export function tripTitle(trip, { dept, name }) {
  if (!dept) throw new PluginError('VALIDATION', '내 부서 코드를 몰라 출장 제목을 만들 수 없습니다. me 로 부서를 확인하고, 조직도에서 읽지 못했으면 관리자에게 회사 설정 departments 를 요청해 주세요.');
  if (!name) throw new PluginError('VALIDATION', '내 이름을 몰라 출장 제목을 만들 수 없습니다. 다우오피스에 다시 로그인한 뒤 시도해 주세요.');
  const d = (iso) => iso.slice(2).replace(/-/g, '.');
  const date = trip.start === trip.end ? `'${d(trip.start)}` : `'${d(trip.start)}-${trip.end.slice(5).replace('-', '.')}`;
  const place = String(trip.titlePlace ?? '').trim() || placeName(trip.place);
  return `${date}/${place}/${dept}/${name}`;
}

/** 사유로 안 쳐 주는 말들. 이것만 남으면 장소를 되풀이한 것이다. */
const FILLER = ['방문', '출장', '미팅', '회의', '참석', '업무', '관련', '건', '및', '외'];

/** 출장목적에 있어야 하는 '하는 일'. 행사 이름이나 장소만으로는 목적이 아니다. */
const ACTIVITY = /참석|참가|운영|전시|시연|설치|시험|점검|보고|발표|회의|미팅|협의|논의|평가|교육|조사|수거|납품|견학|방문|지원|촬영|측정|실증|계약|면담|자문|입회/;

const DATE = /^\d{4}-\d{2}-\d{2}$/;
/** 받침이 있으면 `이`, 없으면 `가`. */
const iGa = (word) => {
  const c = String(word).charCodeAt(String(word).length - 1) - 0xac00;
  return c >= 0 && c < 11172 && c % 28 ? '이' : '가';
};
const validDate = (s) => DATE.test(s) && new Date(`${s}T00:00:00Z`).toISOString().slice(0, 10) === s;

/**
 * 출장 한 건이 올릴 만한지 본다. `{ errors, warnings }` 를 돌려준다.
 *
 * 핵심은 출장목적이 출장지를 되풀이하기만 하는지 보는 것이다. 장소에서 온 말과 상투어를
 * 걷어낸 뒤 남는 것이 없으면 사유가 아니다. 행사 이름만 있고 하는 일이 없어도 거부한다
 * (경고로 두면 그대로 올라간다). 목적을 고치려면 사람에게 묻는다.
 */
export function validateTrip(trip, { transports = TRANSPORTS } = {}) {
  const errors = [];
  const warnings = [];
  const labels = { start: '시작일', end: '종료일', place: '출장지', transport: '교통편', goal: '출장목적' };
  for (const [field, label] of Object.entries(labels)) {
    if (!String(trip[field] ?? '').trim()) errors.push(`${label}${iGa(label)} 비어 있습니다.`);
  }
  if (errors.length) return { errors, warnings };

  for (const field of ['start', 'end']) {
    if (!validDate(trip[field])) errors.push(`${labels[field]}${iGa(labels[field])} 날짜(YYYY-MM-DD)가 아닙니다: ${trip[field]}`);
  }
  if (!errors.length && trip.start > trip.end) errors.push(`기간이 거꾸로입니다: ${trip.start}부터 ${trip.end}까지`);
  if (!transports.includes(trip.transport)) {
    errors.push(`교통편은 양식 선택지 가운데 하나입니다: ${transports.join(', ')}. 자차는 기타로 넣습니다.`);
  }

  const goal = String(trip.goal).trim();
  let residue = goal.toLowerCase();
  for (const token of String(trip.place).toLowerCase().split(/[^0-9a-z가-힣]+/).filter((t) => t.length >= 2)) {
    residue = residue.split(token).join(' ');
  }
  for (const word of FILLER) residue = residue.split(word).join(' ');
  residue = residue.replace(/[^0-9a-z가-힣]/g, '');

  if (residue.length < 4) {
    errors.push(`출장목적이 출장지를 되풀이하기만 합니다: "${goal}". 무엇을 하러 가는지 사람에게 물어 채워 주세요 (예: "실증 부지 인허가 협의").`);
  } else if (!ACTIVITY.test(goal)) {
    errors.push(`출장목적에 하는 일이 없습니다: "${goal}". 무엇을 하러 가는지 사람에게 물어 채워 주세요 (예: "행사 부스 운영", "시험 결과 보고").`);
  }

  // 출장목적과 비고도 결재자가 읽는 문장이다. 구매 품의와 같은 검사를 건다.
  for (const w of checkWriting(goal, '출장목적')) errors.push(w);
  if (trip.note) for (const w of checkWriting(trip.note, '비고')) errors.push(w);

  return { errors, warnings };
}
