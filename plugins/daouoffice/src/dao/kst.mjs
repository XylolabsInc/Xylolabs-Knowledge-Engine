/**
 * 한국 시간(+09:00). 다우오피스 화면, 결재 문서, 카드 대장의 날짜와 시각은 전부 KST 다.
 *
 * `new Date().toISOString()` 은 UTC 라서 로그 시각이 9시간 어긋나고, 자정부터 오전 9시
 * 사이에는 날짜까지 하루 틀린다. 그 시간대 결제가 앞 달 지출결의에 섞여 회계가 되돌린
 * 적이 있다. 시각과 날짜는 이 모듈로 만든다.
 */
export const KST_OFFSET_MS = 9 * 3600_000;

/** `2026-09-29T14:57:32.123+09:00` */
export function kstIso(d = new Date()) {
  return new Date(d.getTime() + KST_OFFSET_MS).toISOString().replace('Z', '+09:00');
}

/** `14:57:32` */
export const kstTime = (d = new Date()) => kstIso(d).slice(11, 19);

/** `2026-09-29` */
export const kstDate = (d = new Date()) => kstIso(d).slice(0, 10);

/** 그 달 1일과 말일. `2026-09` → `['2026-09-01', '2026-09-30']` */
export function monthRange(ym) {
  const m = String(ym).match(/^(\d{4})-(\d{2})$/);
  if (!m) throw new TypeError(`YYYY-MM 이 아니다: ${ym}`);
  const last = new Date(Date.UTC(+m[1], +m[2], 0)).getUTCDate();
  return [`${ym}-01`, `${ym}-${String(last).padStart(2, '0')}`];
}

/** `date` 에서 `days` 일 뒤(음수면 앞). 날짜 문자열끼리만 계산한다. */
export function addDays(date, days) {
  return new Date(Date.parse(`${date}T00:00:00Z`) + days * 86_400_000).toISOString().slice(0, 10);
}
