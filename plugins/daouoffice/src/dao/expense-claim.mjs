import { PluginError } from '../errors.mjs';
import { assertExecute } from '../guard.mjs';
import { dismissDialogs } from './dialogs.mjs';
import { setApprovalLine, approvalLineRows, linkDocuments } from './approval.mjs';
import { sameReceipt } from './card-match.mjs';
import { purposeNameFor, detailFor, effectivePurposeName } from './expense-map.mjs';
import { tripExpense } from './trip-expenses.mjs';
import { checkWriting } from './writing.mjs';
import { kstDate, monthRange } from './kst.mjs';

/**
 * 법인카드 지출결의(경비청구). 원본 저장소 `expense-claim.mjs` 와 `claim-expenses.mjs` 에서 왔다.
 * 위쪽은 다우오피스를 부르지 않는 규칙(달, 제목, 행 분류, 사용용도별 묶음, 명세서 대조, 문서
 * 연결)이고, 아래쪽은 경비 앱과 기안 창을 다루는 DOM 흐름이다.
 *
 * 회계가 정한 규칙
 *
 *   기간      KST 기준 한 달, 1일부터 말일까지. 카드 명세서와 문서 금액을 맞춘다
 *   올리는 때 그 달이 끝나고 증빙이 갖춰진 뒤 다음 달 초에 한 번. 같은 달 두 번째 문서는 만들지 않는다
 *   나누기    카드 한 장, 한 달을 사용용도(비목)별 문서로 나눈다. 한 번에 사용용도 하나씩 올린다
 *   제목      `YYYY년 M월 (<카드 뒤 4자리>/<부서 코드>/<명의자>) 법인카드 지출의 건 (<사용용도>)`
 *   금액      이 달 카드 내역 = 이미 청구 + 이번 문서 + 나머지. 남는 결제는 사람에게 묻는다
 *   연결      행마다 구매 품의서와 검수조서를 관련문서로 연결한다. 결재가 끝난 문서만 고를 수 있다
 *
 * 상신은 여섯 단계다. 1 은 API, 2-5 는 화면 조작이다.
 *
 *   1. POST myExp/saveCompanyCard              사용용도와 상세 저장 (행이 작성중)
 *   2. 경비 화면 `전자결재` → `저장 후 결재요청`  문서 생성 + 기안 창
 *   3. 기안 창: 제목, 영수증 첨부, 문서 연결
 *   4. 결재선을 비우고 순서대로 넣은 뒤 대조
 *   5. `결재요청` → 기안의견 창의 `결재요청`     PUT document/{id}/draft
 *   6. API 로 상태, 제목, 첨부 수, 연결 확인
 */

const fail = (message) => new PluginError('UPSTREAM', message);
const won = (n) => `${Number(n).toLocaleString('ko-KR')}원`;
const dayOf = (row) => String(row.useDate).slice(0, 10);
const tagOf = (row) => `${dayOf(row)} ${String(row.merchant ?? '').trim()} ${won(row.amount)}`;

/** 사람이 남긴 지출결의 메모(`overrides.kind`). 실행이 끝나면 다음 달 문서에도 쓰이게 저장한다. */
export const CLAIM_NOTE_KIND = 'claimNote';

/** 다시 올릴 수 있는 문서 상태. 반려됐거나 상신취소로 임시저장에 돌아간 청구는 카드 행과 이어지지 않는다. */
const DEAD = new Set(['반려', '임시저장']);
export const isLiveStatus = (status) => Boolean(status) && !DEAD.has(status);

// ------------------------------------------------------------------ 달과 제목

/**
 * `YYYY-MM` 을 푼다. `{ year, month, ym, from, to, over }`. `over` 는 말일이 오늘(KST)보다 앞이다.
 * 날짜는 KST 문자열 그대로 자른다. UTC 로 바꾸면 자정부터 오전 9시 사이 결제가 앞 달에 섞인다.
 */
export function claimMonth(ym, today = kstDate()) {
  const m = /^(\d{4})-(\d{2})$/.exec(String(ym ?? ''));
  if (!m || Number(m[2]) < 1 || Number(m[2]) > 12) {
    throw new PluginError('VALIDATION', `청구하는 달은 YYYY-MM 입니다: ${ym}`);
  }
  const [from, to] = monthRange(ym);
  return { year: Number(m[1]), month: Number(m[2]), ym, from, to, over: to < today };
}

/** 달이 끝나지 않았으면 VALIDATION. 지출결의는 다음 달 초에 한 번 올린다. */
export function assertMonthOver(m, today = kstDate()) {
  if (m.to < today) return;
  throw new PluginError('VALIDATION', [
    `${m.year}년 ${m.month}월은 아직 끝나지 않았습니다 (말일 ${m.to}, 오늘 ${today}, 한국 시간).`,
    '지출결의는 카드별로 그 달이 끝나고 영수증과 품의가 갖춰진 뒤 다음 달 초에 한 번 올립니다.',
    '먼저 올리면 그 뒤 결제를 같은 문서에 넣을 수 없고, 같은 달 두 번째 문서는 회계가 받지 않습니다.',
  ].join(' '));
}

/**
 * 회계가 정한 제목. 달은 청구하는 달이고 앞자리 0 을 붙이지 않는다. 카드 번호는 뒤 네 자리,
 * 이름은 카드 명의자, 부서 코드는 조직도 코드 그대로(대소문자를 바꾸지 않는다). 제목을 비우면
 * 양식 이름이 들어가 회계가 되돌린다.
 */
export function claimTitle({ year, month, cardLast4, deptCode, holder, purpose }) {
  if (!year || !month || !cardLast4 || !holder) throw new PluginError('VALIDATION', '지출결의 제목을 만들 값(달, 카드, 명의자)이 모자랍니다.');
  if (!deptCode) {
    throw new PluginError('VALIDATION', '내 부서 코드를 몰라 지출결의 제목을 만들 수 없습니다. me 로 부서를 확인하고, 조직도에서 읽지 못했으면 관리자에게 회사 설정 departments 를 요청해 주세요.');
  }
  const base = `${year}년 ${Number(month)}월 (${cardLast4}/${deptCode}/${holder}) 법인카드 지출의 건`;
  return purpose ? `${base} (${purpose})` : base;
}

/** filings.ref. 카드, 달, 사용용도 하나에 문서 하나다. */
export const claimRef = (cardLast4, ym, purpose) => `${cardLast4}/${ym}/${purpose}`;

// ------------------------------------------------------------------ 메모

/** 도구 인자 `notes` 한 줄이 카드 행에 맞는가. 날짜와 금액은 반드시, 가맹점은 있으면 맞춘다. */
export function noteMatches(note, row) {
  if (!note || dayOf(row) !== note.date || Number(note.amount) !== Number(row.amount)) return false;
  return !note.merchant || String(row.merchant ?? '').toUpperCase().includes(String(note.merchant).toUpperCase());
}

/** 저장된 메모(`overrides` claimNote)를 인자 모양으로 바꾼다. */
export const savedNotes = (overrides = []) => overrides
  .filter((o) => o.kind === CLAIM_NOTE_KIND && o.match?.date && o.match?.amount !== undefined)
  .map((o) => ({ ...o.data, date: o.match.date, amount: Number(o.match.amount), merchant: o.match.merchant, saved: true, id: o.id }));

// ------------------------------------------------------------------ 행 분류

const cardOf = (receipt) => {
  const c = receipt?.data?.card ?? receipt?.data?.cardLast4;
  return c === undefined || c === null || c === '' ? null : String(c);
};
const itemOf = (p) => String(p?.item ?? p?.data?.item ?? '');
const vendorOf = (p) => String(p?.vendor ?? p?.data?.vendor ?? '');

/**
 * 짝지은 카드 행(`matchRows().audited`)을 이 달 청구에 맞게 나눈다. 다우오피스를 부르지 않는다.
 *
 *   claimed      이미 청구한 행 (문서가 붙었거나 결재가 시작됐다)
 *   items        이번에 올릴 수 있는 행. 영수증, 출장 경비, 그대로 청구, 구매 품의, 메모 가운데 하나로 설명된다
 *   excluded     사람이 빼라고 한 행
 *   unexplained  어느 것으로도 설명되지 않는 행. 지어내지 않고 사람에게 묻는다
 *
 * 영수증은 같은 결제의 둘째 문서(인보이스와 영수증)도 함께 붙인다. 영수증에 다른 카드가 적혀
 * 있으면(`data.cardLast4`, save_receipt 가 적는다) 이 카드 청구에 넣지 않는다. 적혀 있지 않으면 모르는
 * 것이지 다른 카드가 아니다.
 */
export function sortClaimRows({ audited, from, to, receipts = [], trips = [], confirmed = [], notes = [], cardLast4 }) {
  const inMonth = audited.filter((a) => dayOf(a.row) >= from && dayOf(a.row) <= to);
  const claimed = [];
  const items = [];
  const excluded = [];
  const unexplained = [];
  const usedNotes = new Set();

  for (const a of inMonth) {
    const r = a.row;
    if (a.claimed || r.docId) { claimed.push(a); continue; }
    // 이번 호출의 메모가 저장된 메모보다 먼저다.
    const note = notes.find((n) => !n.saved && noteMatches(n, r)) ?? notes.find((n) => n.saved && noteMatches(n, r));
    if (note) usedNotes.add(note);
    if (note?.exclude) { excluded.push({ row: r, reason: note.reason || '사람이 뺐다', note }); continue; }

    const base = { row: r, kind: a.kind, purchase: a.purchase ?? null, purchases: a.purchases ?? [], receipts: [] };
    const primary = a.receipts ?? (a.receipt ? [a.receipt] : []);
    let item = null;
    let why = '영수증, 출장, 구매 품의, 메모 어느 것으로도 설명되지 않습니다';
    if (primary.length) {
      const other = primary.find((x) => cardOf(x) && cardOf(x) !== String(cardLast4));
      if (other) {
        why = `영수증에 다른 카드(${cardOf(other)})가 적혀 있습니다`;
      } else {
        const files = receipts.filter((x) => primary.some((p) => sameReceipt(p, x)) && (!cardOf(x) || cardOf(x) === String(cardLast4)));
        item = {
          ...base, source: 'receipt',
          category: primary[0].category, item: primary.map((x) => x.item).filter(Boolean).join(' '), vendor: primary[0].vendor,
          receipts: [...new Map([...primary, ...files].map((x) => [x.file, x])).values()],
        };
      }
    } else {
      const te = tripExpense(r, a.kind, { trips, confirmed });
      if (te) {
        item = { ...base, source: 'trip', tripKind: te.kind, purposeName: te.purposeName, detail: te.detail, trip: te.trip?.key ?? null };
      } else if (a.claimAsIs) {
        if (a.claimAsIs.data?.purposeName) {
          item = { ...base, source: 'asIs', purposeName: a.claimAsIs.data.purposeName, detail: a.claimAsIs.data.detail };
        } else {
          why = '그대로 넣기로 한 결제인데 사용용도가 정해지지 않았습니다';
        }
      } else if (a.purchase) {
        const usage = /(구독|지출|갱신)의 건/.test(a.purchase.title ?? '');
        item = { ...base, source: 'purchase', category: usage ? '클라우드구독료' : '재료비', item: itemOf(a.purchase), vendor: vendorOf(a.purchase) };
      }
    }

    if (note && (note.detail || note.purpose)) {
      if (item) {
        item = { ...item, ...(note.purpose ? { purposeName: note.purpose } : {}), ...(note.detail ? { detail: note.detail } : {}), note };
      } else if (note.detail && note.purpose) {
        item = { ...base, source: 'note', purposeName: note.purpose, detail: note.detail, note };
      } else {
        why = '메모로 넣으려면 상세 사용용도(detail)와 사용용도(purpose)가 모두 있어야 합니다';
      }
    }
    if (!item) { unexplained.push({ row: r, reason: why }); continue; }
    items.push(item);
  }

  const unusedNotes = notes.filter((n) => !n.saved && !usedNotes.has(n));
  return { inMonth, claimed, items, excluded, unexplained, usedNotes: [...usedNotes], unusedNotes };
}

/**
 * 행마다 사용용도와 상세를 정하고 사용용도별로 묶는다. 사용용도 이름은 라이브 목록 기준
 * (`effectivePurposeName()`)이다. 상세는 결재자가 읽는 문장이라 `checkWriting()` 을 건다.
 * `{ rows, groups: Map<사용용도, rows>, problems: [{ purpose, text }] }`. 사용용도를 정하지 못한
 * 행의 문제는 `purpose` 가 null 이라 모든 사용용도의 문서를 막는다.
 */
export function planClaim({ items, purposes, map }) {
  const rows = [];
  const problems = [];
  for (const it of items) {
    let purposeName;
    try {
      // 출장 경비 종류(주유, 철도 등)의 사용용도는 설정의 `{ category: <종류>, purpose }` 가 덮는다.
      const tripPurpose = it.source === 'trip' && !it.note?.purpose ? map.categories[it.tripKind] : undefined;
      purposeName = tripPurpose ?? it.purposeName ?? purposeNameFor({ category: it.category, item: it.item }, map);
    } catch (e) {
      if (!(e instanceof PluginError)) throw e;
      problems.push({ purpose: null, text: `${tagOf(it.row)}: ${e.message}` });
      continue;
    }
    purposeName = effectivePurposeName(purposes, purposeName, map);
    const detail = String(it.detail ?? detailFor(it, map)).trim();
    const bad = checkWriting(detail, '상세 사용용도');
    if (!detail) problems.push({ purpose: purposeName, text: `${tagOf(it.row)}: 상세 사용용도가 비어 있습니다` });
    else if (bad.length) problems.push({ purpose: purposeName, text: `${tagOf(it.row)}: ${bad.join(' / ')}` });
    else rows.push({ ...it, purposeName, detail });
  }
  const groups = new Map();
  for (const r of rows) groups.set(r.purposeName, [...(groups.get(r.purposeName) ?? []), r]);
  return { rows, groups, problems };
}

/** 사용용도별 묶음 요약. `[{ purpose, count, amount }]` */
export const groupSummary = (groups) => [...groups].map(([purpose, rows]) => ({
  purpose, count: rows.length, amount: rows.reduce((t, r) => t + r.row.amount, 0),
}));

/**
 * 명세서 대조. 이 달 카드 내역 = 이미 청구 + 이번 문서 + 다른 사용용도 + 다른 사람 몫 + 뺀 결제
 * + 설명 안 된 결제. 취소 쌍과 환불은 카드 내역에서 이미 상계돼 셈하지 않는다.
 */
export function claimStatement({ sorted, planned, purpose, elsewhere = [] }) {
  const sum = (xs) => xs.reduce((t, x) => t + Number(x.row?.amount ?? x.amount ?? 0), 0);
  const thisRows = planned.groups.get(purpose) ?? [];
  const otherRows = planned.rows.filter((r) => r.purposeName !== purpose);
  const st = {
    claimed: sum(sorted.claimed),
    thisDoc: sum(thisRows),
    otherPurposes: sum(otherRows),
    elsewhere: sum(elsewhere),
    excluded: sum(sorted.excluded),
    unexplained: sum(sorted.unexplained) + (sum(sorted.items) - sum(planned.rows)),
  };
  st.total = sum(sorted.inMonth) + st.elsewhere;
  return st;
}

/** `이 달 카드 내역 … = 이미 청구 … + 이번 문서 … + …` */
export function statementLine(st) {
  const parts = [`이미 청구 ${won(st.claimed)}`, `이번 문서 ${won(st.thisDoc)}`];
  if (st.otherPurposes) parts.push(`다른 사용용도 ${won(st.otherPurposes)}`);
  if (st.elsewhere) parts.push(`다른 사람 몫 ${won(st.elsewhere)}`);
  if (st.excluded) parts.push(`뺀 결제 ${won(st.excluded)}`);
  if (st.unexplained) parts.push(`설명 안 된 결제 ${won(st.unexplained)}`);
  return `이 달 카드 내역 ${won(st.total)} = ${parts.join(' + ')}`;
}

// ------------------------------------------------------------------ 문서 연결

/**
 * 연결할 문서 후보. 행의 구매 품의서(문서가 있는 것)와, 그 카드 행이나 품의에 묶인 검수조서 기록
 * (`filings` kind inspection)이다. `[{ kind, docId, title, ref }]`
 *
 * 검수조서 기록의 `ref` 는 카드 행 `<날짜>/<금액>` 이고, 품의마다 나눈 결제는 `<날짜>/<금액>#<품의 키>`
 * 다(`inspection.mjs` 의 `rowKey`). 같은 ref 에 기록이 여럿이면(고쳐 다시 올린 경우) 마지막 것을 쓴다.
 * 품의 키를 ref 로 쓴 기록과 `detail.purchaseKeys`, `detail.purchaseId` 도 받는다.
 */
export function linkCandidates(rows, { inspections = [] } = {}) {
  const out = new Map();
  const keys = new Set();
  const rowIds = new Set(rows.map((r) => String(r.row.id)));
  const rowKeys = new Set(rows.map((r) => `${dayOf(r.row)}/${r.row.amount}`));
  for (const r of rows) {
    const ps = r.purchases?.length ? r.purchases : (r.purchase ? [r.purchase] : []);
    for (const p of ps) {
      if (p.key) keys.add(String(p.key));
      if (p.docId && !out.has(String(p.docId))) {
        out.set(String(p.docId), { kind: '구매 품의서', docId: String(p.docId), title: p.title ?? '', ref: p.key ?? null });
      }
    }
  }
  const latest = new Map();
  for (const f of [...inspections].sort((a, b) => String(a.filedAt ?? '').localeCompare(String(b.filedAt ?? '')) || (a.id ?? 0) - (b.id ?? 0))) {
    if (f.docId) latest.set(String(f.ref ?? `doc:${f.docId}`), f);
  }
  for (const f of latest.values()) {
    if (out.has(String(f.docId))) continue;
    const ref = String(f.ref ?? '');
    const [rowKey, purchaseKey] = ref.split('#');
    const refs = [ref, purchaseKey, ...(f.detail?.purchaseKeys ?? [])].filter(Boolean).map(String);
    const byRowKey = rowKeys.has(rowKey);
    const byKey = refs.some((k) => keys.has(k));
    const byRow = f.detail?.purchaseId != null && rowIds.has(String(f.detail.purchaseId));
    if (byRowKey || byKey || byRow) out.set(String(f.docId), { kind: '검수조서', docId: String(f.docId), title: f.title ?? '', ref: f.ref ?? null });
  }
  return [...out.values()];
}

/**
 * 후보마다 `done`(결재 완료, 문서번호 있음)이 붙은 목록으로 무엇을 연결할지 정한다.
 *
 *   withoutLinks  연결하지 않고 모두 기안의견에 적는다 (회계와 합의했을 때만)
 *   linkDone      끝난 문서만 연결하고, 진행중인 것은 기안의견에 적는다
 *   (기본)        하나라도 진행중이면 멈춘다(blocked). 결재가 끝난 뒤 올린다
 */
export function decideLinks(cands, { linkDone = false, withoutLinks = false } = {}) {
  if (withoutLinks) return { links: [], opinion: cands, blocked: [] };
  const notDone = cands.filter((c) => !c.done);
  if (notDone.length && !linkDone) return { links: [], opinion: [], blocked: notDone };
  return { links: cands.filter((c) => c.done), opinion: notDone, blocked: [] };
}

/** 기안의견. 연결하지 못한 문서의 ID 와 제목을 적는다. */
export const opinionText = (opinion) => (opinion.length
  ? ['결재가 끝나지 않아 문서 연결 대신 적는다.', ...opinion.map((c) => `${c.docId} ${c.title}`.trim())].join('\n')
  : '');

// ------------------------------------------------------------------ 다우오피스 (쓰기)

/**
 * 사용용도와 상세를 카드 행에 저장한다. 문서는 만들지 않는다. **서버를 바꾼다**: `api()` 가
 * 승인된 실행에서만 보낸다. `rows` 는 `{ raw, purchaseId, purposeCode, detail }`.
 */
export async function savePurposes(api, rows) {
  const paramList = rows.map((r) => ({
    ...r.raw,
    usePurpose: r.purposeCode,
    usePurposeDetail: r.detail,
    referenceId: r.purchaseId,
    referenceObject: '10',
  }));
  const res = await api('/eacc/api/ess/myExp/saveCompanyCard', { method: 'POST', body: { paramList } });
  if (res.status !== 200) throw fail(`사용용도를 저장하지 못했습니다 (HTTP ${res.status}).`);
  return paramList.length;
}

/** 경비 > 회사 법인 카드 화면을 열고 ExtJS 가 뜰 때까지 기다린다. */
export async function openCardScreen(page, baseUrl) {
  await page.goto(`${baseUrl}/ess/app/#expenses`, { waitUntil: 'domcontentloaded', timeout: 60_000 });
  await page.waitForLoadState('networkidle', { timeout: 60_000 }).catch(() => {});
  await page.waitForFunction(
    () => typeof Ext !== 'undefined' && Ext.ComponentQuery.query('field').some((f) => f.getFieldLabel?.() === '카드'),
    null,
    { timeout: 90_000, polling: 1000 },
  );
  await dismissDialogs(page);
}

/** 카드를 고르고 조회한다. 그리드가 다시 찰 때까지 기다린다. */
export async function searchCard(page, cardId) {
  await page.evaluate((id) => {
    Ext.ComponentQuery.query('field').find((f) => f.getFieldLabel?.() === '카드').setValue(id);
    const b = Ext.ComponentQuery.query('button').find((x) => (x.getText?.() ?? '') === '조회');
    b.fireHandler ? b.fireHandler() : b.fireEvent('click', b);
  }, cardId);
  await page.waitForTimeout(3000);
  await page.waitForFunction(
    () => {
      const g = Ext.ComponentQuery.query('grid')[0];
      return g && !g.getStore().isLoading() && g.getStore().getCount() > 0;
    },
    null,
    { timeout: 60_000, polling: 1000 },
  );
}

/** ExtJS 확인 대화상자의 버튼을 누른다. `예`, `아니오`, `확인`. */
async function answerExtDialog(page, label) {
  await page.waitForFunction(
    (t) => Ext.ComponentQuery.query('messagebox').some(
      (w) => w.isVisible?.() && Ext.ComponentQuery.query('button', w).some((b) => (b.getText?.() ?? '') === t && b.isVisible?.()),
    ),
    label, { timeout: 30_000, polling: 400 },
  );
  await page.evaluate((t) => {
    const box = Ext.ComponentQuery.query('messagebox').find((w) => w.isVisible?.());
    const b = Ext.ComponentQuery.query('button', box).find((x) => (x.getText?.() ?? '') === t && x.isVisible?.());
    b.fireHandler ? b.fireHandler() : b.fireEvent('click', b);
  }, label);
  await page.waitForTimeout(2500);
}

/**
 * 경비 화면에서 대상 행을 골라 지출품의서를 만든다. **서버에 문서가 생긴다.**
 * 문서 ID 와 기안 창(새 페이지)을 돌려준다. 아직 상신 전이다.
 */
export async function createClaimDocument(page, baseUrl, { cardId, purchaseIds }) {
  assertExecute('지출결의 문서 생성');
  await openCardScreen(page, baseUrl);
  await searchCard(page, cardId);

  const picked = await page.evaluate((ids) => {
    const grid = Ext.ComponentQuery.query('grid')[0];
    const store = grid.getStore();
    const recs = [];
    for (let i = 0; i < store.getCount(); i++) {
      const r = store.getAt(i);
      if (ids.includes(r.get('purchaseId'))) recs.push(r);
    }
    grid.getSelectionModel().select(recs);
    return recs.length;
  }, purchaseIds);
  if (picked !== purchaseIds.length) {
    throw fail(`경비 화면에서 행을 고르지 못했습니다: ${purchaseIds.length}건 가운데 ${picked}건만 잡혔습니다. 결재가 끝난 행은 화면에 나오지 않습니다.`);
  }
  await page.waitForTimeout(1500);

  await page.evaluate(() => {
    const b = Ext.ComponentQuery.query('button').find((x) => (x.getText?.() ?? '') === '전자결재');
    b.fireHandler ? b.fireHandler() : b.fireEvent('click', b);
  });
  await page.waitForFunction(
    () => Ext.ComponentQuery.query('window').some((w) => w.isVisible?.() && /전자결재 요청/.test(w.title ?? '')),
    null,
    { timeout: 45_000, polling: 500 },
  );
  await page.waitForTimeout(2000);

  // 창이 열리기를 먼저 기다린다. 버튼 단계가 실패해도 이 약속이 혼자 거부되지 않게 받아 둔다.
  const opened = page.context().waitForEvent('page', { timeout: 60_000 }).catch((e) => e);
  await page.evaluate(() => {
    const win = Ext.ComponentQuery.query('window').find((w) => /전자결재 요청/.test(w.title ?? ''));
    const b = Ext.ComponentQuery.query('button', win).find((x) => (x.getText?.() ?? '') === '저장 후 결재요청');
    b.fireHandler ? b.fireHandler() : b.fireEvent('click', b);
  });
  await answerExtDialog(page, '예');

  const docPage = await opened;
  if (docPage instanceof Error) throw fail('지출결의 기안 창이 열리지 않았습니다. 다우오피스 임시저장 문서를 확인해 주세요.');
  await docPage.waitForLoadState('domcontentloaded', { timeout: 45_000 }).catch(() => {});
  const docId = docPage.url().match(/document\/(\d+)\//)?.[1];
  if (!docId) {
    await docPage.close().catch(() => {});
    throw fail('기안 창 주소에서 문서 ID 를 읽지 못했습니다. 다우오피스 임시저장 문서를 확인해 주세요.');
  }
  return { docId, docPage };
}

/** 결재 정보 편집기를 연다(저장하지 않고 읽기만 할 때). */
async function openApprovalLineEditorQuiet(page) {
  await page.locator('#act_edit_apprflow').first().click({ timeout: 20_000 });
  await page.waitForTimeout(4000);
}

/** 바뀐 것이 없는 편집기를 확인으로 닫는다. */
async function closeApprovalLineEditorQuiet(page) {
  await page.evaluate(() => {
    const btn = [...document.querySelectorAll('.go_popup a, .go_popup button, [role=dialog] button')]
      .find((e) => (e.innerText || '').trim() === '확인' && e.offsetParent !== null);
    btn?.click();
  });
  await page.waitForTimeout(2500);
}

/**
 * 기안 창에서 제목을 넣고 영수증을 붙이고 문서를 연결하고 결재선을 넣어 상신한다.
 *
 * 첨부는 파일을 넣기만 해서는 서버에 남지 않는다. 상신이나 임시저장을 해야 붙는다. 결재선 편집도
 * 같은 창 안에서 끝낸다. 결재선은 비우고 순서대로 넣은 뒤, 편집기를 다시 열어 이름, 타입, 순서가
 * 기대와 같은지 본다. 이 양식의 기본 결재선에 대표가 이미 들어 있어 추가만 하면 중복된다.
 *
 * `결재요청` 뒤에는 결재자가 있으면 기안의견 창(.layer_approval), 없으면 바로 완료 경고 창이 뜬다.
 * 둘을 헷갈려 아무 주 버튼이나 누르면 사고가 난다. 창을 특정해서 잡는다.
 */
export async function attachAndSubmit(docPage, { files, approvers, title, links = [], opinion = '' }) {
  await docPage.waitForLoadState('networkidle', { timeout: 30_000 }).catch(() => {});
  await docPage.waitForTimeout(5000);
  await dismissDialogs(docPage);

  await docPage.waitForSelector('#subject', { timeout: 30_000 });
  const got = await docPage.evaluate((t) => {
    const el = document.getElementById('subject');
    el.value = t;
    el.dispatchEvent(new Event('input', { bubbles: true }));
    el.dispatchEvent(new Event('change', { bubbles: true }));
    return el.value;
  }, title);
  if (got !== title) throw fail(`제목이 들어가지 않았습니다: ${got}`);

  const baseName = (f) => String(f).split('/').pop().normalize('NFC');
  const attached = await docPage.evaluate(() =>
    [...document.querySelectorAll('#imgWrap li[data-name], #fileWrap li[data-name]')].map((li) => li.getAttribute('data-name').normalize('NFC')));
  for (const file of files) {
    if (attached.includes(baseName(file))) continue;
    await docPage.locator('input[type=file][name=file]').first().setInputFiles(file);
    await docPage.waitForFunction(
      (name) => {
        const th = [...document.querySelectorAll('th')].find((t) => t.innerText.trim() === '파일첨부');
        return (th?.nextElementSibling?.innerText ?? '').includes(name);
      },
      baseName(file),
      { timeout: 45_000, polling: 1000 },
    ).catch(() => { throw fail(`첨부가 화면에 나타나지 않습니다: ${baseName(file)}`); });
  }

  if (links.length) {
    const linked = await linkDocuments(docPage, links);
    const missing = links.filter((d) => !linked.includes(String(d.id)));
    if (missing.length) throw fail(`문서 연결이 빠졌습니다: ${missing.map((d) => d.docNum).join(', ')}`);
  }

  const line = await setApprovalLine(docPage, approvers);
  await openApprovalLineEditorQuiet(docPage);
  const rows = (await approvalLineRows(docPage)).filter((r) => r.kind !== '기안');
  await closeApprovalLineEditorQuiet(docPage);
  const have = rows.map((r) => `${r.kind} ${r.name}`).join(' → ');
  const want = approvers.map((a) => `${a.type} ${a.name}`).join(' → ');
  if (have !== want) throw fail(`결재선이 기대와 다릅니다: ${rows.map((r) => `${r.group} ${r.kind} ${r.name}`).join(' / ')} (기대 ${want})`);
  await docPage.waitForTimeout(3000);

  assertExecute('지출결의 결재요청');
  const clicked = await docPage.evaluate(() => {
    const b = [...document.querySelectorAll('a')].find((e) => e.id === 'act_draft' && e.offsetParent !== null);
    if (!b) return false;
    b.click();
    return true;
  });
  if (!clicked) throw fail('결재요청 버튼이 없습니다.');

  // 옵션은 세 번째 인자다. 보이는지는 크기로 본다(`offsetParent` 는 position:fixed 에서 늘 null).
  const kind = await docPage.waitForFunction(
    () => {
      const shown = (el) => {
        if (!el) return false;
        const r = el.getBoundingClientRect();
        return r.width > 0 && r.height > 0;
      };
      if (shown(document.querySelector('.go_popup.layer_approval'))) return 'ap';
      if (shown(document.querySelector('#gpopupLayer p.q'))) return 'warn';
      return false;
    },
    null,
    { timeout: 60_000, polling: 500 },
  ).then((h) => h.jsonValue());
  if (kind === 'warn') throw fail('결재자가 붙지 않아 상신하지 않았습니다. 그대로 올리면 바로 완료 처리됩니다.');

  if (opinion) {
    const box = docPage.locator('.go_popup.layer_approval textarea').first();
    if (!(await box.count())) throw fail('기안의견 칸을 찾지 못했습니다.');
    await box.fill(opinion);
    if ((await box.inputValue()) !== opinion) throw fail('기안의견이 제대로 들어가지 않았습니다.');
  }
  await docPage.click('.go_popup.layer_approval a.btn_major_s');
  await docPage.waitForTimeout(10_000);
  return line;
}

const defaultSleep = (ms) => new Promise((res) => setTimeout(res, ms));

/**
 * 상신 결과를 API 로 확인한다. 첨부가 많으면 상신 직후에는 상태가 비어 있다(첨부 40개짜리가
 * 20초 뒤에야 진행중이 됐다). `waitMs` 까지 기다리며 다시 읽는다. 서버의 `referenceCount` 는 연결이
 * 있어도 0 으로 와서 `references` 목록을 본다.
 */
export async function verifyClaim(api, docId, { waitMs = 90_000, sleep = defaultSleep } = {}) {
  const until = Date.now() + waitMs;
  let r = await api(`/gw/api/approval/document/${docId}`);
  while (!r.data?.data?.document?.docStatusName && Date.now() < until) {
    await sleep(5000);
    r = await api(`/gw/api/approval/document/${docId}`);
  }
  const data = r.data?.data ?? {};
  const doc = data.document ?? {};
  return {
    status: doc.docStatusName ?? '',
    title: doc.title ?? '',
    references: (doc.references ?? []).map((x) => String(x.id ?? x.documentId ?? '')),
    attachCount: Number(doc.attachCount ?? (doc.attaches ?? []).length ?? 0),
    line: (data.apprFlow?.activityGroups ?? []).flatMap((g) => (g.activities ?? []).map((a) => `${g.name ?? ''} ${a.displayName ?? a.userName ?? ''}`.trim())),
  };
}

/** 기대와 서버 문서를 대조한다. 어긋난 것의 설명 목록. */
export function claimMismatches(v, { title, files, links }) {
  const out = [];
  if (v.status !== '진행중') out.push(`상태가 진행중이 아닙니다: ${v.status || '(비어 있음)'}`);
  if (v.title !== title) out.push(`제목이 다릅니다: ${v.title || '(비어 있음)'}`);
  if (v.attachCount !== files.length) out.push(`첨부 수가 다릅니다: ${v.attachCount}/${files.length}`);
  const linked = new Set(v.references);
  const missing = links.filter((l) => !linked.has(String(l.id)));
  if (missing.length) out.push(`연결되지 않은 문서: ${missing.map((l) => l.docNum || l.id).join(', ')}`);
  return out;
}

