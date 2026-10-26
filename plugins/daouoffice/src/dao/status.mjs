import { overrideRules, isSubscription } from './card-match.mjs';
import { intangible, photosOfKeys, rowKey, purchaseView, serialNeeds } from './inspection.mjs';

/**
 * 결제별 처리 현황. 원본 저장소 `status-report.mjs` 의 표를 데이터로 만든다. 조회 전용이다.
 *
 *   영수증    짝지은 영수증 수, 필요 없으면 '필요 없음'
 *   구매 품의  품의 문서 ID(올리지 않았으면 '임시'), 필요 없으면 '필요 없음'
 *   검수 사진  사진 수, 모자라면 표시, 처리된 결제는 그 사유
 *   검수조서   이 플러그인이 올린 기록(filings)의 문서 ID. 기록에 없는 것은 모른다고 둔다
 *   지출결의   카드 행에 붙은 결재 문서 ID, 아니면 '아직'
 *
 * NIPA 칸은 이번 범위가 아니라 뺐다. 지어내지 않는다: 이 플러그인 밖에서 올린 검수조서는
 * 기록이 없어 '기록 없음' 으로 보인다. 남이 처리하는 결제, 환불, 취소 쌍은 짝짓기에서 이미 빠진다.
 */

const won = (n) => Number(n).toLocaleString('ko-KR');

/**
 * @param {object} o
 * @param {object[]} o.audited      matchRows().audited
 * @param {Map} o.purchasesByKey    품의 키 → purchaseView
 * @param {object[]} o.photos       사진 행
 * @param {object[]} o.overrides    overrides 행
 * @param {object[]} o.filings      filings 행 (kind 'inspection')
 * @param {object} o.marks          inspectionMarks()
 * @param {object} o.config         회사 설정
 */
export function paymentStatus({ audited, purchasesByKey, photos, overrides, filings, marks, config = {} }) {
  const { claimAsIs } = overrideRules(overrides);
  const subs = config.cardMatch?.subscriptionMerchants ?? [];
  const requisitionFrom = config.cardMatch?.requisitionFrom ?? '';
  const serialFromKrw = config.inspection?.serialFromKrw;
  const reportOf = (key) => filings.filter((f) => f.kind === 'inspection' && (f.ref === key || String(f.ref ?? '').startsWith(`${key}#`)));

  const rows = [];
  const todo = { receipt: [], purchase: [], photo: [], serial: [], report: [], claim: [] };
  for (const a of audited) {
    const r = a.row;
    const date = String(r.useDate).slice(0, 10);
    const key = rowKey(r);
    const tag = `${date.slice(5)} ${r.merchant} ${won(r.amount)}원`;
    const base = { key, date, merchant: r.merchant, amount: r.amount };
    const receipts = a.receipts?.length ?? (a.receipt ? 1 : 0);
    const ps = (a.purchases ?? []).map((p) => purchasesByKey.get(p.key) ?? purchaseView(p));
    const goods = ps.length > 0 && !ps.every(intangible);
    const asIs = Boolean(a.claimAsIs ?? claimAsIs(r));
    const noReq = a.kind !== '구매' || asIs || (requisitionFrom && date < requisitionFrom);
    const got = goods ? photosOfKeys(photos, ps.map((p) => p.key)) : [];
    const incomplete = marks.incompleteOf(ps);
    const done = marks.doneOf(ps);
    const reports = reportOf(key);
    const serials = goods ? ps.flatMap((p) => serialNeeds(p, { serialFromKrw, itemRows: marks.itemRows })) : [];
    const serialMissing = serials.filter((s) => !s.numbers.length);
    const needsReceipt = a.kind === '구매' || isSubscription(a, subs);
    const settled = a.claimed || asIs;

    const cell = {
      receipt: receipts ? `${receipts}` : needsReceipt ? '없음' : '필요 없음',
      purchase: noReq ? '필요 없음' : ps.length ? ps.map((p) => p.docId ?? '임시').join(', ') : '없음',
      photo: !goods ? '' : done ? done : got.length ? (incomplete ? `${got.length}장, 모자람` : `${got.length}장`) : '없음',
      report: !ps.length ? '' : done ? done : reports.length ? reports.map((f) => f.docId ?? f.status).join(', ') : '기록 없음',
      claim: a.claimed ? (r.docId ?? '올림') : '아직',
    };
    if (needsReceipt && !receipts && !settled) todo.receipt.push(tag);
    if (!noReq && !ps.length && !settled) todo.purchase.push(tag);
    if (goods && !done && (!got.length || incomplete)) todo.photo.push(`${tag}${incomplete ? ` (${incomplete})` : ''}`);
    if (goods && !done && serialMissing.length) todo.serial.push(`${tag}: ${serialMissing.map((s) => s.item).join(', ')}`);
    if (ps.length && !done && (!goods || (got.length && !incomplete)) && !reports.length) todo.report.push(tag);
    if (!a.claimed && !asIs) todo.claim.push(tag);
    rows.push({ ...base, ...cell, purchases: ps.map((p) => p.key) });
  }
  return { rows, todo };
}

/** 표와 남은 일을 채팅에 맞는 글로. `limit` 줄까지만 표를 싣는다. */
export function describeStatus({ rows, todo }, { title, limit = 60 }) {
  const labels = { receipt: '영수증', purchase: '구매 품의', photo: '검수 사진', serial: '시리얼 번호', report: '검수조서', claim: '지출결의' };
  const L = [title, '', '남은 일'];
  for (const [k, label] of Object.entries(labels)) {
    L.push(`- ${label} ${todo[k].length}건${todo[k].length ? `: ${todo[k].slice(0, 8).join('; ')}${todo[k].length > 8 ? ' 외' : ''}` : ''}`);
  }
  L.push('', '결제별 (날짜 가맹점 금액 | 영수증 | 품의 | 사진 | 검수조서 | 지출결의)');
  for (const r of rows.slice(0, limit)) {
    L.push(`- ${r.date.slice(5)} ${r.merchant} ${won(r.amount)}원 | ${r.receipt} | ${r.purchase} | ${r.photo || '-'} | ${r.report || '-'} | ${r.claim}`);
  }
  if (rows.length > limit) L.push(`(앞의 ${limit}건만 보입니다. 기간을 좁혀 주세요.)`);
  L.push('', '검수조서 칸은 이 봇이 올린 기록만 봅니다. 다른 곳에서 올린 것은 기록 없음으로 보입니다.');
  return L.join('\n');
}
