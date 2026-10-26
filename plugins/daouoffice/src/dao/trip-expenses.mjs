import { overrideMatches } from './card-match.mjs';
import { placeName, TRIP_STATUS } from './trips.mjs';

/**
 * 영수증 없이 청구하는 출장 경비를 출장과 짝짓는다. 원본 저장소 `trip-expenses.mjs` 에서 왔다.
 * 출장 목록은 플러그인 DB `trips`, 사람이 확인한 결제는 DB `overrides` (kind `tripExpense`)다.
 *
 * 주유, 철도, 식대, 주차, 택시, 숙박은 영수증이 없어도 된다. 대신 어느 출장의 비용인지 상세
 * 사용용도에 적는다. 결제일이 출장 기간(`start` 부터 `claimUntil` 또는 `end` 까지)에 들면 그
 * 출장이다. 기간 밖이면 짝짓지 않는다. 가까운 출장에 끼워 넣지 않고 사람에게 묻는다.
 *
 * 숙박과 기차표는 출장 전에 사서 결제일이 기간 밖에 있다. 사람이 확인한 결제만 잇는다.
 *
 *   overrides { kind: 'tripExpense',
 *               match: { date: 'YYYY-MM-DD', merchant: '<가맹점 일부>', amount? },
 *               data:  { kind: '숙박', trip: '<출장 시작일 또는 tripKey>' }   그 출장의 비용
 *                      { kind: '주차', detail: '<사람이 알려 준 용도>' } }   출장이 아닌 업무 경비
 *
 * 기간 밖 결제는 누구의 어느 출장인지 확인하고 넣는다. 다른 사람 출장의 기차표를 내 출장으로
 * 넣었다가 바로잡은 적이 있다.
 */

export const TRIP_EXPENSE_KIND = 'tripExpense';

/** 비용 종류 → 사용용도와 상세 문구. 결재가 끝난 옛 청구의 사용용도를 따랐다. */
export const TRIP_KINDS = Object.freeze({
  주유: { purpose: '차량유지비', word: '유류비' },
  철도: { purpose: '출장교통비', word: '철도 교통비' },
  식대: { purpose: '식대, 음료', word: '식대' },
  주차: { purpose: '주차료', word: '주차료' },
  택시: { purpose: '출장교통비', word: '택시비' },
  숙박: { purpose: '여비교통비', word: '숙박비' },
});

const LODGING_MERCHANT = /AGODA|아고다|호텔|숙박|야놀자|여기어때/i;

const dayOf = (row) => String(row.useDate).slice(0, 10);

/**
 * DB 의 출장 행을 짝짓기에 쓰는 모양으로 푼다. 취소된 출장은 뺀다.
 * `[{ key, start, end, place, claimName?, claimUntil?, titlePlace? }]`
 */
export function tripsForExpenses(tripRows = []) {
  return tripRows
    .filter((t) => t.status !== TRIP_STATUS.cancelled && t.data?.start)
    .map((t) => ({ key: t.key, ...t.data, end: t.data.end ?? t.data.start }));
}

/** 카드 행의 비용 종류. 출장 경비가 아니면 null. `auditKind` 는 card-match 의 `classify()` 결과다. */
export function tripKind(row, auditKind) {
  if (LODGING_MERCHANT.test(row.merchant)) return '숙박';
  return TRIP_KINDS[auditKind] ? auditKind : null;
}

/** 사람이 확인한 결제 가운데 이 행에 맞는 것. */
const confirmedFor = (row, kind, confirmed) => confirmed
  .filter((o) => (o.kind ?? TRIP_EXPENSE_KIND) === TRIP_EXPENSE_KIND)
  .find((o) => o.data?.kind === kind && overrideMatches(o.match, row));

/** 결제일로 출장을 찾는다. 기간 밖의 결제는 확인된 것으로만 잇는다. 숙박은 늘 확인이 필요하다. */
export function tripFor(row, kind, { trips = [], confirmed = [] } = {}) {
  const told = confirmedFor(row, kind, confirmed);
  if (told?.data?.trip) {
    const ref = String(told.data.trip);
    return trips.find((t) => t.key === ref) ?? trips.find((t) => t.start === ref) ?? null;
  }
  if (kind === '숙박') return null;
  const day = dayOf(row);
  const hits = trips.filter((t) => day >= t.start && day <= (t.claimUntil ?? t.end));
  // 같은 날 출장이 둘이면 어느 쪽 비용인지 모른다. 지어내지 않는다.
  return hits.length === 1 ? hits[0] : null;
}

/** 상세 사용용도에 쓰는 출장지 이름. */
export const tripClaimName = (trip) => String(trip.claimName ?? '').trim()
  || placeName(trip.titlePlace || trip.place);

/** `{ kind, trip, purposeName, detail }`, 출장 경비로 설명할 수 없으면 null. */
export function tripExpense(row, auditKind, { trips = [], confirmed = [] } = {}) {
  const kind = tripKind(row, auditKind);
  if (!kind) return null;
  const told = confirmedFor(row, kind, confirmed);
  if (told?.data?.detail) {
    return { kind, trip: null, purposeName: TRIP_KINDS[kind].purpose, detail: String(told.data.detail) };
  }
  const trip = tripFor(row, kind, { trips, confirmed });
  if (!trip) return null;
  return { kind, trip, purposeName: TRIP_KINDS[kind].purpose, detail: `${tripClaimName(trip)} 출장 ${TRIP_KINDS[kind].word}` };
}
