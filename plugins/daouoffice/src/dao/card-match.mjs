import { listUsage } from './cards.mjs';

/**
 * 법인카드 한 줄마다 영수증과 구매 품의서를 짝짓는다. 조회 전용이다. 원본 저장소
 * `card-match.mjs` 의 알고리즘과 허용 오차를 그대로 옮겼다. 바뀐 것은 데이터의 출처다.
 *
 *   카드 대장   다우오피스 경비 앱의 라이브 내역(`listUsage`). 사용용도가 차 있으면 청구가 끝난 것이다
 *   영수증      플러그인 DB `receipts` (원본은 `receipts/` 파일명 규칙을 읽었다)
 *   구매 품의   플러그인 DB `purchases` 의 `amountKrw`·`usd`·`paidKrw`·`paidAt`
 *   예외        플러그인 DB `overrides` (사람이 알려 준 환불, 남이 처리하는 결제, 짝 고정, 그대로 청구)
 *
 * 검수조서와 경비청구가 같이 쓴다. 두 곳이 따로 짝을 지으면 한쪽에서만 빠진다.
 * 취소 쌍(같은 가맹점, 같은 금액의 +/−)은 상계해 뺀다.
 *
 * 원본과 다른 점 하나: 금액이 같은 품의가 둘 이상이면 원본은 마지막 하나만 남겼다(`byAmount`
 * 덮어쓰기). 여기서는 후보를 모두 두고, 결제일(`paidAt`)이 카드 승인일에 가까운 쌍부터 한 품의를
 * 한 결제에만 붙인다. 결제일이 없는 품의는 가장 나중에 고른다.
 */

/** `overrides.kind` 값. 원본의 표 이름을 옆에 적었다. */
export const OVERRIDE_KINDS = Object.freeze({
  refunded: 'refunded',               // REFUNDED: 취소 행 없이 사람이 환불이라고 확인한 결제
  elsewhere: 'elsewhere',             // HANDLED_ELSEWHERE: 이 카드로 결제했지만 다른 사람이 처리한다
  pinned: 'pinned',                   // PINNED: 사람이 영수증 짝을 알려 준 결제. data.receipts 에 파일명 조각
  claimAsIs: 'claimAsIs',             // CLAIM_AS_IS: 영수증도 품의도 없이 지출결의에 그대로 넣는다
  inspectionDone: 'inspectionDone',   // INSPECTION_DONE
  inspectionIncomplete: 'inspectionIncomplete', // INSPECTION_INCOMPLETE
  itemOverride: 'itemOverride',       // ITEM_OVERRIDES
});

/**
 * 영수증도 구매 품의도 세지 않는 가맹점. 주유, 철도, 식대, 택시, 주차다. 표에는 `생략` 으로
 * 남겨 무엇을 뺐는지 보이게 한다. 택시는 주차가 아니다. `카카오\s*T` 만 보면 택시를 주차로 잡는다.
 */
export const SKIP_KINDS = [
  ['주유', /주유소|에너지|오일뱅크|SK엔무브|GS칼텍스|현대오일/i],
  ['철도', /철도공사|코레일|KORAIL|SRT/i],
  ['식대', /식당|김밥|국밥|분식|카페|커피|베이커리|치킨|피자|메가\s*MGC|스타벅스|이디야|투썸|빽다방/i],
  ['택시', /택시|Grab\*|UBER|우버/i],
  ['주차', /주차|파킹|카카오\s*T\s*주차/i],
];

/** 출장·현장 경비로 보이는 가맹점. 구매 품의가 아니라 경비청구 대상이다. */
export const EXPENSE_LIKE = /AGODA|아고다|이마트|호텔|숙박|야놀자|여기어때/i;

/** 구독으로 보는 영수증 비목. 구독 가맹점 목록은 회사 설정(`cardMatch.subscriptionMerchants`)에 둔다. */
const SUBSCRIPTION_CATEGORIES = new Set(['클라우드구독료', '소프트웨어']);

/** 영수증 구매처 ↔ 카드 가맹점 별칭. 회사마다 다른 것은 설정(`cardMatch.vendorAliases`)으로 더한다. */
export const VENDOR_ALIAS = { 알리익스프레스: 'ALIEXPRESS' };

const isKrw = (currency) => currency === '원' || currency === 'KRW';
const day = (useDate) => String(useDate).slice(0, 10);
const DAY_MS = 86_400_000;
const near = (a, b, days) => Math.abs(new Date(a) - new Date(b)) <= days * DAY_MS;

/** `구매` · `경비` · 생략 종류(`주유` 등) 가운데 하나를 돌려준다. */
export function classify(merchant) {
  for (const [kind, re] of SKIP_KINDS) if (re.test(merchant)) return kind;
  return EXPENSE_LIKE.test(merchant) ? '경비' : '구매';
}

export function sameVendor(receiptVendor, merchant, aliases = VENDOR_ALIAS) {
  const key = String(aliases[receiptVendor] ?? receiptVendor ?? '').toUpperCase().replace(/[^A-Z]/g, '');
  const m = String(merchant ?? '').toUpperCase().replace(/[^A-Z]/g, '');
  return key.length >= 3 && m.includes(key.slice(0, 6));
}

/** 같은 가맹점·같은 금액의 +/− 쌍을 찾아 상계 표시한다. */
export function markCancelled(rows) {
  const cancelled = new Set();
  for (const neg of rows.filter((r) => r.amount < 0)) {
    const mate = rows.find((r) => !cancelled.has(r.id) && r.amount === -neg.amount
      && r.merchant === neg.merchant && r.id !== neg.id);
    if (mate) { cancelled.add(neg.id); cancelled.add(mate.id); }
  }
  return cancelled;
}

/** 같은 결제의 문서인가. 인보이스와 영수증이 함께 있는 결제가 있다. */
export const sameReceipt = (a, b) => a.date === b.date && a.vendor === b.vendor
  && a.amount === b.amount && a.currency === b.currency;

/** 사람이 알려 준 결제 하나(`overrides.match`)가 카드 행에 맞는가. 날짜는 있으면 맞춘다. */
export function overrideMatches(match, row) {
  if (!match) return false;
  if (match.date && !day(row.useDate).startsWith(match.date)) return false;
  if (match.merchant && !String(row.merchant).toUpperCase().includes(String(match.merchant).toUpperCase())) return false;
  return match.amount === undefined || Number(match.amount) === row.amount;
}

/**
 * 사람이 알려 준 예외를 카드 행에 맞춰 보는 함수들. 맞는 `overrides` 행을, 없으면 undefined.
 *
 *   isRefund     환불·반품 확인 (경비에서 뺀다)
 *   isElsewhere  다른 사람이 품의와 청구를 맡는다 (검수와 경비청구에서 뺀다)
 *   pinnedFor    영수증 짝 고정
 *   claimAsIs    영수증도 품의도 없이 그대로 청구
 */
export function overrideRules(overrides = []) {
  const finder = (kind) => {
    const rules = overrides.filter((o) => o.kind === kind);
    return (row) => rules.find((o) => overrideMatches(o.match, row));
  };
  return {
    isRefund: finder(OVERRIDE_KINDS.refunded),
    isElsewhere: finder(OVERRIDE_KINDS.elsewhere),
    pinnedFor: finder(OVERRIDE_KINDS.pinned),
    claimAsIs: finder(OVERRIDE_KINDS.claimAsIs),
  };
}

/** 구독 결제인가. 짝지은 결과(`audited` 한 줄)를 받는다. */
export function isSubscription(a, subscriptionMerchants = []) {
  const merchants = subscriptionMerchants.map((s) => new RegExp(s, 'i'));
  return merchants.some((re) => re.test(a.row.merchant))
    || (a.receipts ?? (a.receipt ? [a.receipt] : [])).some((x) => SUBSCRIPTION_CATEGORIES.has(x.category));
}

/**
 * 짝짓기 본체. 다우오피스를 부르지 않으므로 그대로 시험할 수 있다.
 *
 * @param {object} input
 * @param {object[]} input.rows       `listUsage()` 의 정리된 행
 * @param {object[]} input.receipts   DB `receipts` 행 ({ file, date, item, vendor, amount, currency, category, orderId })
 * @param {object[]} input.purchases  DB `purchases` 행 ({ key, amountKrw, usd, paidKrw, paidAt, ... })
 * @param {object[]} input.overrides  DB `overrides` 행 ({ kind, match, data })
 * @param {number}  [input.fxRate]    외화 짝짓기 기준 환율이 근처 실측에서 안 나올 때 쓴다
 * @param {object}  [input.vendorAliases]
 */
export function matchRows({ rows, receipts, purchases, overrides = [], fxRate = null, vendorAliases = {} }) {
  const aliases = { ...VENDOR_ALIAS, ...vendorAliases };
  const { isRefund, isElsewhere, pinnedFor, claimAsIs } = overrideRules(overrides);

  const cancelled = markCancelled(rows);
  const live = rows.filter((r) => !cancelled.has(r.id) && r.amount > 0 && !isRefund(r) && !isElsewhere(r));
  const refunded = rows.filter(isRefund);

  const hit = new Map();          // 카드 행 id → 영수증
  const group = new Map();        // 카드 행 id → 영수증 여럿 (한 결제에 주문 둘)
  const used = new Set();

  // 1차: 원화 영수증은 금액이 정확히 같아야 한다. 먼저 붙여 두지 않으면
  // 느슨한 외화 규칙이 원화 건의 영수증을 먼저 채 간다.
  for (const r of live) {
    const x = receipts.find((y) => isKrw(y.currency) && !used.has(y.file)
      && y.amount === r.amount && near(y.date, day(r.useDate), 1));
    if (x) { hit.set(r.id, x); used.add(x.file); }
  }

  // 사람이 알려 준 짝을 먼저 붙인다.
  for (const r of live) {
    const pin = pinnedFor(r);
    if (!pin) continue;
    const parts = pin.data?.receipts ?? [];
    const list = parts.map((part) => receipts.find((y) => !used.has(y.file)
      && y.file.normalize('NFC').includes(String(part).normalize('NFC'))));
    if (!list.length || list.some((y) => !y)) continue;
    hit.set(r.id, list[0]);
    if (list.length > 1) group.set(r.id, list);
    list.forEach((y) => used.add(y.file));
  }

  // 2차: 외화 영수증. 한 장이 한 줄일 수도 있고, 알리처럼 판매자 둘에게 한 번에 주문하면
  // 영수증 두 장이 카드 한 줄로 승인되기도 한다.
  //
  // 적용환율이 기준에 가장 가까운 조합을 고른다. 넓은 범위(1,250-1,600원/USD) 안이면 먼저
  // 찾은 것을 붙이던 때는 한 장짜리 주문이 엉뚱한 줄에 붙었다. 실제 카드 적용환율은 고시
  // 매매기준율의 0-3% 위에 모인다.
  //
  // 기준 환율은 그 무렵의 실제 적용환율이다. 같은 날 영수증 한 장과 카드 한 줄이 맞는 쌍들의
  // 환율을 모아 결제일 앞뒤 7일의 중앙값을 쓴다. 없으면 `fxRate`(원본은 고정 환율 상수였다),
  // 그것도 없으면 기간 전체의 중앙값. 셋 다 없으면 그 줄은 외화 짝짓기를 하지 않는다.
  const TOLERANCE = 0.04;
  const samples = [];
  for (const r of live) {
    const d = day(r.useDate);
    for (const y of receipts) {
      if (y.currency !== 'USD' || y.date !== d || !sameVendor(y.vendor, r.merchant, aliases) || !(y.amount > 0)) continue;
      const rate = r.amount / y.amount;
      if (rate > 1250 && rate < 1600) samples.push({ day: d, rate });
    }
  }
  const median = (xs) => { const s = [...xs].sort((a, b) => a - b); return s.length ? s[Math.floor(s.length / 2)] : null; };
  const refFor = (d) => {
    const t = Date.parse(d);
    return median(samples.filter((x) => Math.abs(Date.parse(x.day) - t) <= 7 * DAY_MS).map((x) => x.rate))
      ?? (Number(fxRate) > 0 ? Number(fxRate) : null)
      ?? median(samples.map((x) => x.rate));
  };

  // 알리 영수증 여럿은 한 번에 결제한 주문끼리만 묶는다. 한 번에 결제하면 판매자별 주문번호가
  // 20,000씩 떨어져 나온다. 따로 결제한 주문이 다른 결제의 주문들과 묶이면 정작 그 결제는
  // 영수증이 없다고 나온다. 주문번호는 원본에서 품명으로 되찾았고(이름이 겹치면 흔들렸다),
  // 여기서는 영수증 행의 `orderId` 를 그대로 쓴다.
  const orderIdOf = (y) => (/^\d{16}$/.test(String(y.orderId ?? '')) ? BigInt(y.orderId) : null);
  const sameCheckout = (list) => {
    const ids = list.map(orderIdOf).filter((x) => x != null);
    if (ids.length < 2) return true;
    const sorted = ids.sort((a, b) => (a < b ? -1 : 1));
    return sorted[sorted.length - 1] - sorted[0] <= BigInt(20_000 * 8);
  };
  const combos = [];
  for (const r of live) {
    if (hit.has(r.id)) continue;
    const d = day(r.useDate);
    const REF = refFor(d);
    if (!REF) continue;
    const cand = receipts.filter((y) => y.currency === 'USD' && !used.has(y.file)
      && near(y.date, d, 3) && sameVendor(y.vendor, r.merchant, aliases));
    // 주문일이 결제일과 다르면 벌점을 준다. 환율만 보면 우연히 더 가까운 다른 날 주문이 이긴다.
    const days = (y) => Math.round(Math.abs(new Date(y.date) - new Date(d)) / DAY_MS);
    const push = (list) => {
      const usd = list.reduce((t, y) => t + y.amount, 0);
      const off = Math.abs(r.amount / usd / REF - 1);
      if (off <= TOLERANCE) combos.push({ r, list, off: off + 0.02 * Math.max(...list.map(days)) });
    };
    // 알리는 한 번 결제한 주문을 판매자별로 나눠 영수증이 여러 장 나온다. 여섯 장이 카드 한 줄인
    // 적이 있어 여섯까지 조합한다. 후보는 12개로 잘라 조합 수를 막는다.
    const MAX_PER_LINE = 6;
    const pool = cand.slice(0, 12);
    const walk = (start, pick) => {
      if (pick.length && sameCheckout(pick)) push(pick);
      if (pick.length === MAX_PER_LINE) return;
      for (let i = start; i < pool.length; i++) walk(i + 1, [...pick, pool[i]]);
    };
    walk(0, []);
  }
  // 가장 잘 맞는 조합부터 확정한다. 한 영수증은 한 줄에만 붙는다.
  // 겹치는 결제끼리는 배정을 함께 고른다. 한 줄씩 가장 가까운 조합을 먼저 집으면 작은 영수증이
  // 환율 오차 안에서 옆 결제로 넘어간다. 영수증을 공유하는 결제들을 한 무리로 묶고, 무리 안에서
  // (1) 쓰이지 않는 후보 영수증이 가장 적고 (2) 짝을 못 찾는 결제가 가장 적고 (3) 오차 합이
  // 가장 작은 배정을 고른다. 결제마다 상위 8개 조합만 본다.
  const TOP = 8;
  const byRow = new Map();
  for (const c of combos.sort((x, y) => x.off - y.off)) {
    const l = byRow.get(c.r.id) ?? [];
    if (l.length < TOP) l.push(c);
    byRow.set(c.r.id, l);
  }
  const rowIds = [...byRow.keys()];
  const parent = new Map(rowIds.map((id) => [id, id]));
  const find = (x) => (parent.get(x) === x ? x : (parent.set(x, find(parent.get(x))), parent.get(x)));
  const owner = new Map();
  for (const id of rowIds) {
    for (const c of byRow.get(id)) {
      for (const y of c.list) {
        if (owner.has(y.file)) parent.set(find(id), find(owner.get(y.file)));
        else owner.set(y.file, id);
      }
    }
  }
  const clusters = new Map();
  for (const id of rowIds) clusters.set(find(id), [...(clusters.get(find(id)) ?? []), id]);
  for (const ids of clusters.values()) {
    const pool = new Set(ids.flatMap((id) => byRow.get(id).flatMap((c) => c.list.map((y) => y.file))));
    let best = null;
    const walk = (i, taken, chosen, off, misses) => {
      if (i === ids.length) {
        const unused = [...pool].filter((f) => !taken.has(f)).length;
        const key = [unused, misses, off];
        if (!best || key[0] < best.key[0] || (key[0] === best.key[0] && (key[1] < best.key[1] || (key[1] === best.key[1] && key[2] < best.key[2])))) {
          best = { key, chosen: [...chosen] };
        }
        return;
      }
      for (const c of byRow.get(ids[i])) {
        if (c.list.some((y) => taken.has(y.file))) continue;
        const next = new Set(taken); c.list.forEach((y) => next.add(y.file));
        walk(i + 1, next, [...chosen, c], off + c.off, misses);
      }
      walk(i + 1, taken, chosen, off, misses + 1);   // 이 결제는 짝 없음
    };
    walk(0, new Set(), [], 0, 0);
    for (const { r, list } of best?.chosen ?? []) {
      hit.set(r.id, list[0]);
      if (list.length > 1) group.set(r.id, list);
      list.forEach((y) => used.add(y.file));
    }
  }

  // 구매 품의: 원화 금액(`amountKrw`)이 카드 승인액과 같다. 후보를 모두 두고 결제일이 가까운
  // 쌍부터 한 품의를 한 결제에 붙인다.
  const exactFor = assignByAmount(live, purchases);

  // 구매 품의가 금액으로 안 잡히는 외화 건은 영수증의 외화 금액으로 잇는다.
  // 결제 전에 올린 품의는 환산 추정액이라 카드 승인액과 다르다.
  // 결제일이 적힌 품의는 그 날짜 앞뒤 사흘 안의 결제에만 잇는다. 외화 금액만 보면 같은 USD 금액의
  // 다른 서비스 품의가 붙는다.
  const byUsd = (usd, d) => purchases.filter((p) => p.usd != null && Math.abs(p.usd - usd) < 0.005
    && (!p.paidAt || Math.abs(Date.parse(p.paidAt) - Date.parse(d)) <= 3 * DAY_MS));
  const purchasesFor = (r) => {
    const exact = exactFor.get(r.id);
    if (exact) return [exact];
    const list = group.get(r.id) ?? (hit.get(r.id) ? [hit.get(r.id)] : []);
    // 금액이 큰 주문을 앞에 둔다. 상세 문구가 첫 품의 제목을 쓴다.
    const found = [...list].sort((x, y) => y.amount - x.amount)
      .flatMap((y) => (y.currency === 'USD' ? byUsd(y.amount, day(r.useDate)).slice(0, 1) : []));
    // 같은 물건을 두 번 주문하면 USD 가 같은 품의가 둘이다. 원화 승인액 합도 맞아야 이 결제의 품의다.
    const krwOk = (ps) => ps.some((p) => p.paidKrw == null)
      || ps.reduce((t, p) => t + p.paidKrw, 0) === r.amount;
    if (found.length === list.length && new Set(found).size === found.length && krwOk(found)) return found;
    // 한 결제를 용도별로 품의 여럿에 나눠 올린 경우. 결제일이 같은 외화 품의 가운데 USD 합이
    // 영수증 합과 같은 묶음을 찾는다.
    const usd = list.reduce((t, y) => t + (y.currency === 'USD' ? y.amount : NaN), 0);
    if (!list.length || !(usd > 0)) return [];
    const d = day(r.useDate);
    const pool = purchases.filter((p) => p.usd != null && p.paidAt === d).slice(0, 12);
    let best = null;
    const walk = (i, pick, sum) => {
      if (pick.length && Math.abs(sum - usd) < 0.01 && krwOk(pick)) { best = pick; return; }
      for (let k = i; k < pool.length && !best; k++) walk(k + 1, [...pick, pool[k]], sum + pool[k].usd);
    };
    walk(0, [], 0);
    return best ? [...best].sort((x, y) => y.usd - x.usd) : [];
  };

  const audited = live.map((r) => {
    const ps = purchasesFor(r);
    return {
      row: r,
      receipt: hit.get(r.id),
      receipts: group.get(r.id),
      purchase: ps[0],
      purchases: ps,
      kind: classify(r.merchant),
      claimAsIs: claimAsIs(r) ?? null,
      // 사용용도만 저장한 행(작성중)은 아직 청구 전이다. 문서가 붙었거나 결재가 시작된 행만 끝난 것으로 본다.
      claimed: Boolean(r.docId) || !['작성대기', '작성중'].includes(r.status),
    };
  });

  return { rows, live, refunded, cancelled, audited, receipts, used };
}

/**
 * 원화 금액이 같은 (결제, 품의) 쌍을 결제일 차이가 작은 것부터 하나씩 확정한다.
 * 한 품의는 한 결제에만, 한 결제는 한 품의에만 붙는다.
 */
export function assignByAmount(rows, purchases) {
  const byAmount = new Map();
  for (const p of purchases) {
    const amount = Number(p.amountKrw);
    if (!Number.isFinite(amount)) continue;
    byAmount.set(amount, [...(byAmount.get(amount) ?? []), p]);
  }
  const pairs = [];
  for (const r of rows) {
    for (const p of byAmount.get(r.amount) ?? []) {
      const gap = p.paidAt ? Math.abs(Date.parse(p.paidAt) - Date.parse(day(r.useDate))) / DAY_MS : Number.MAX_SAFE_INTEGER;
      pairs.push({ r, p, gap });
    }
  }
  pairs.sort((a, b) => a.gap - b.gap || String(a.r.useDate).localeCompare(String(b.r.useDate)));
  const out = new Map();
  const taken = new Set();
  for (const { r, p } of pairs) {
    if (out.has(r.id) || taken.has(p)) continue;
    out.set(r.id, p);
    taken.add(p);
  }
  return out;
}

/**
 * 카드 한 장의 기간 내역을 DB 의 영수증, 구매 품의, 예외와 짝짓는다.
 *
 *   const m = await matchCard({ api, db, owner, card, from: '2026-09-01', to: '2026-09-30' });
 *
 * `audited[]` 한 줄은 `{ row, receipt, receipts, purchase, purchases, kind, claimAsIs, claimed }` 다.
 * `receipts` 는 한 결제에 영수증이 둘 이상일 때만 있다.
 */
export async function matchCard({ api, db, owner, card, from, to, fxRate, vendorAliases, usage = listUsage }) {
  const { rows } = await usage(api, { from, to, cardId: card.id });
  return matchRows({
    rows,
    receipts: db.receipts(owner),
    purchases: db.purchases(owner),
    overrides: db.overrides(owner),
    fxRate: fxRate ?? db.latestFxRate()?.rate ?? null,
    vendorAliases,
  });
}
