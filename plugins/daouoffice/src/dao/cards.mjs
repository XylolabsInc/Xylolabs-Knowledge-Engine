import { PluginError } from '../errors.mjs';

/**
 * 법인카드 사용내역 조회. **읽기 전용이다.** 원본 저장소 `cards.mjs` 와 `list-cards.mjs` 의
 * `pickCard()` 에서 왔다.
 *
 * 경비 앱(`/ess/app/#expenses` → 회사 법인 카드)이 쓰는 API 를 그대로 부른다.
 * 경비청구·전자결재 상신은 여기서 하지 않는다. ESS 응답은 두 겹이라 알맹이가 `r.data.data` 에 있다.
 *
 * 함정 넷.
 *
 *   1. 상태 필터는 `statuses` 만 먹는다. `statuses: [""]` 는 0건, 빈 배열이 전체다
 *   2. 카드별 조회를 다 합쳐도 전체보다 적다. 카드 콤보에 안 잡히는 카드가 있다
 *   3. 행에 카드 식별자가 없다. 카드별로 나눠 부르는 것이 유일한 귀속 수단이다
 *   4. 1001 건은 상한이 아닐 수 있다. 그 크기가 나오면 기간을 반으로 쪼개 다시 센다
 *
 * 카드 대장은 영업일 2-3일 늦게 올라온다. 날짜는 KST 문자열 그대로 자른다.
 */

/** 전자결재 상태 코드. 화면 콤보에서 그대로 읽어 왔다. */
export const STATUS = {
  9: '작성대기',
  0: '작성중',
  1: '결재중',
  2: '결재완료',
};

/** 이름 → 코드. 상태 이름으로 거르기 위한 역방향 표다. */
export const STATUS_CODE = Object.fromEntries(Object.entries(STATUS).map(([k, v]) => [v, k]));

/**
 * 지금까지 한 응답에서 본 가장 많은 행수.
 *
 * **상한인지 아닌지 모른다.** 넓은 기간을 물으면 자꾸 1001 이 나와서 처음엔
 * 상한이라고 봤는데, 2025 년 이전이 0건이라 1001 이 그냥 전체 건수였다.
 * `limit`·`page` 는 확실히 안 먹는다 — 붙여도 응답 크기가 그대로다.
 *
 * 그래서 이 크기가 나오면 기간을 반으로 쪼개 **다시 세어 본다.** 쪼갠 합이
 * 같으면 잘린 것이 아니고, 더 많으면 잘렸던 것이다. 어느 쪽이든 맞는 답이
 * 나온다. 데이터가 늘어 진짜 상한에 닿는 날에도 그대로 동작한다.
 */
const ROW_PROBE = 1001;

/**
 * 법인카드 목록.
 *
 * `valueField` 가 `creditcardId` 다. `displayField` 는
 * `R&D(홍길동)   [홍길동]` 처럼 카드 별칭과 담당자가 붙어 있다. 카드번호는 응답 단계에서 이미
 * 마스킹돼 있다.
 */
export async function listCards(api) {
  const r = await api('/eacc/api/ess/custom/code/getList', {
    method: 'POST',
    body: { comboType: 'CREDIT_CARD' },
  });
  const list = r.data?.data?.list;
  if (!Array.isArray(list)) throw new PluginError('UPSTREAM', `법인카드 목록을 읽지 못했습니다 (HTTP ${r.status}).`);

  return list.map((c) => {
    const display = String(c.displayField ?? '').replace(/\s+/g, ' ').trim();
    const holder = display.match(/\[([^\]]+)\]\s*$/)?.[1] ?? '';
    return {
      id: c.valueField,
      name: display.replace(/\s*\[[^\]]+\]\s*$/, '').trim(),
      holder,
      cardNo: c.creditcardNo ?? '',
      issuer: c.description2 ?? '',
      adminId: c.description3 ?? '',
      dtype: c.dtype ?? '',
      display,
    };
  });
}

/** 사용용도 코드표. `valueField` 가 코드, `displayField` 가 이름이다. */
export async function listPurposes(api) {
  const r = await api('/eacc/api/ess/custom/code/getList', {
    method: 'POST',
    body: { comboType: 'USE_PURPOSE', page: 1, start: 0, limit: 200 },
  });
  const list = r.data?.data?.list ?? [];
  return list.map((p) => ({ code: p.valueField, name: p.displayField }));
}

/**
 * 기간별 사용내역.
 *
 *   listUsage(api, { from: '2026-08-01', to: '2026-09-12' })
 *   listUsage(api, { from, to, cardId, statuses: ['9'] })
 *
 * `cardId` 를 비우면 **볼 수 있는 모든 카드**가 나온다. 카드를 지정하면
 * 그 카드만 나오는데, 카드별 결과를 다 합쳐도 전체보다 적다 — 카드 콤보에
 * 안 잡히는 카드가 있다. docs/09-expense-cards.md 참조.
 *
 * `{ rows, totals, truncated }` 를 돌려준다. `truncated` 에 날짜가 들어 있으면
 * 그 하루가 혼자서 관측 최대치를 채워 더 쪼갤 수 없었다는 뜻이다 — 잘렸을
 * 수도 있고 정말 그만큼 쓴 것일 수도 있다.
 */
export async function listUsage(api, { from, to, cardId = '', statuses = [], approvalUserId = '' } = {}) {
  if (!from || !to) throw new PluginError('VALIDATION', '조회 기간의 시작일과 종료일이 필요합니다 (YYYY-MM-DD).');
  if (from > to) throw new PluginError('VALIDATION', `기간이 거꾸로입니다: ${from}부터 ${to}까지`);

  const seen = new Map();
  const truncated = [];

  const fetchRange = async (start, end) => {
    const r = await api('/eacc/api/ess/myExp/getCompanyCardList', {
      method: 'POST',
      body: {
        fromDate: compact(start),
        toDate: compact(end),
        // `statuses` 만 실제로 걸린다. `status`·`approvalStatus` 는 화면이
        // 같이 보내지만 서버가 무시한다. 빈 문자열을 넣으면 0건이 나온다.
        statuses,
        status: [''],
        approvalStatus: [''],
        creditcardId: cardId,
        approvalUserId,
      },
    });
    const body = r.data?.data;
    const list = body?.list;
    if (!Array.isArray(list)) {
      throw new PluginError('UPSTREAM', `법인카드 사용내역을 읽지 못했습니다 (${start}부터 ${end}까지, HTTP ${r.status}).`);
    }

    if (list.length >= ROW_PROBE) {
      if (start === end) {
        truncated.push(start);
      } else {
        const mid = middle(start, end);
        await fetchRange(start, mid);
        await fetchRange(nextDay(mid), end);
        return;
      }
    }
    for (const row of list) seen.set(row.purchaseId, row);
  };

  await fetchRange(from, to);

  // 합계는 기간을 쪼개면 구간별로 따로 나온다. 합쳐 쓰지 말고 행에서 다시 센다.
  const rows = [...seen.values()].sort((a, b) => String(b.useDate).localeCompare(String(a.useDate)));
  return { rows: rows.map(normalize), totals: totalsOf(rows), truncated };
}

/** 행 하나를 쓰기 좋은 모양으로 고친다. 원본 필드도 남긴다. */
function normalize(row) {
  return {
    id: row.purchaseId,
    statusCode: row.status,
    status: STATUS[row.status] ?? `알수없음(${row.status})`,
    useDate: row.useDate,
    merchant: row.bpName ?? '',
    amount: Number(row.grossAmount ?? 0),
    purpose: row.usePurposeName ?? '',
    purposeCode: row.usePurpose ?? '',
    detail: row.usePurposeDetail ?? '',
    project: row.projectName ?? '',
    claimant: row.adminName ?? '',
    docId: row.docId ?? null,
    raw: row,
  };
}

/**
 * 상태별 건수와 금액. 서버가 주는 `progressAmount` 등은 기간을 쪼개면
 * 구간별로 나뉘므로 쓰지 않고 행에서 직접 센다.
 *
 * 취소 거래는 음수 금액으로 들어온다. 합계에 그대로 반영한다.
 */
function totalsOf(rows) {
  const out = { count: rows.length, amount: 0, byStatus: {} };
  for (const r of rows) {
    const amount = Number(r.grossAmount ?? 0);
    const name = STATUS[r.status] ?? `알수없음(${r.status})`;
    out.amount += amount;
    out.byStatus[name] ??= { count: 0, amount: 0 };
    out.byStatus[name].count++;
    out.byStatus[name].amount += amount;
  }
  return out;
}

/** `2026-08-01` → `20260801`. 서버는 구분자 없는 형태만 받는다. */
function compact(date) {
  const d = String(date).replace(/-/g, '');
  if (!/^\d{8}$/.test(d)) throw new PluginError('VALIDATION', `날짜 형식이 이상합니다: ${date} (YYYY-MM-DD)`);
  return d;
}

/** 두 날짜의 가운데 날. 기간을 반으로 쪼갤 때 쓴다. */
function middle(start, end) {
  const a = Date.parse(`${start}T00:00:00Z`);
  const b = Date.parse(`${end}T00:00:00Z`);
  return new Date(a + Math.floor((b - a) / 2)).toISOString().slice(0, 10);
}

function nextDay(date) {
  return new Date(Date.parse(`${date}T00:00:00Z`) + 86_400_000).toISOString().slice(0, 10);
}

/**
 * 이름 일부, 카드 뒤 네 자리 또는 id 로 카드를 고른다. 여럿이 걸리면 고르지 않고 묻는다.
 */
export function pickCard(cards, needle) {
  const want = String(needle ?? '').trim();
  const exact = cards.find((c) => c.id === want);
  if (exact) return exact;
  if (/^\d{4}$/.test(want)) {
    const byLast4 = cards.filter((c) => String(c.cardNo).replace(/\D/g, '').endsWith(want));
    if (byLast4.length === 1) return byLast4[0];
  }
  const hits = cards.filter((c) => c.display.toLowerCase().includes(want.toLowerCase()));
  if (hits.length === 1) return hits[0];
  if (hits.length === 0) throw new PluginError('NOT_FOUND', `"${want}" 에 맞는 법인카드가 없습니다. 카드 목록을 먼저 확인해 주세요.`);
  throw new PluginError('VALIDATION', `"${want}" 에 여러 카드가 걸립니다: ${hits.map((c) => c.name).join(', ')}`);
}

/** 카드번호(마스킹)의 뒤 네 자리. 없으면 빈 문자열. */
export const last4Of = (card) => String(card?.cardNo ?? '').replace(/\D/g, '').slice(-4);

/** 상태 이름 → 코드. 모르는 이름이면 VALIDATION. */
export function statusCodeOf(name) {
  const code = STATUS_CODE[name];
  if (code === undefined) {
    throw new PluginError('VALIDATION', `모르는 상태입니다: ${name} (${Object.values(STATUS).join(', ')})`);
  }
  return code;
}
