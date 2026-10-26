import { PluginError } from '../errors.mjs';
import { checkWriting, checkNoteStyle } from './writing.mjs';
import { toKrw, fxNote, paidNote } from './fx.mjs';
import { purchaseLine } from './approvers.mjs';

/**
 * 구매 품의서(`일반 / 구매 품의서`) 규칙. 원본 저장소 `purchases.mjs` 의 `formValues()` 와
 * `validatePurchase()` 를 옮겼다. 목록(`PURCHASES`)은 플러그인 DB `purchases` 로 갔고, 회사마다
 * 다른 값(예산, 과제와 세부 과제, 최소 집행 금액)은 회사 설정에서 읽는다.
 *
 * 회사 관례(동료가 올려 승인된 문서에서 읽었다).
 *
 *   제목        물건은 `{품목} 구매의 건`       비워 두면 양식 이름만 들어간다
 *               구독은 `{서비스} 구독의 건`
 *               도메인, 라이선스 연장은 `{대상} 갱신의 건`
 *               사용량 청구는 `{대상} 사용료 지출의 건`
 *               품목 가짓수는 `종` 으로 센다 (`외 3종 구매의 건`)
 *   구매품명    `{품명} / {수량}`               수량을 꼭 붙인다
 *   업체명      판매자 이름, 플랫폼은 괄호      `테스트상점 (알리익스프레스)`. 네이버페이 같은 결제수단은 쓰지 않는다
 *   구입일자    결제일(카드 승인일)             요일은 자동으로 붙는다. 결제 전 품의만 예정일
 *   계좌, 예금주 법인카드면 `해당없음`          자동으로 채운다
 *   기타        상품 링크, 부연                 외화면 환산식이 자동으로 붙는다. 문장이 아니라 항목으로 쓴다
 *
 * ## 구입목적: 대상 + 용도
 *
 * **품명을 되풀이하면 반려된다.** 통과하는 꼴은 `시험 로봇 원격조종`(조종기),
 * `로봇 원격제어 시 실시간 화면 표시용`(액션캠)이다. 용도를 모르면 지어내지 말고 묻는다.
 *
 * ## 금액
 *
 *   원화 결제        `amount`                    결제(예정)액 그대로
 *   외화, 결제 전    `usd`                       초안에 고정한 매매기준율(`fx`)로 환산
 *   외화, 결제 완료  `usd` + `paidKrw` `paidAt`  카드 승인액을 그대로 쓴다
 *
 * ## 첨부: 견적서 대용 상품 페이지
 *
 * 구매 전 문서라 **영수증과 주문내역은 붙이지 않는다.** 상품 페이지 캡처(`quotes`)가 없으면
 * 올리지 않는다.
 */

/** 양식 선택지와 이름. 회사 설정의 `purchase` 절이 있으면 그 값을 쓴다. */
export const DEFAULTS = Object.freeze({
  formName: '구매 품의서',
  budgets: ['운영자금', '프로젝트 사업비'],
  projectBudget: '프로젝트 사업비',
  payWhen: ['당월', '익월'],
  payMethods: ['세금계산서', '법인카드', '현금지급', '기타/사업소득 지급'],
  cardMethod: '법인카드',
  cardIssuers: ['연구비카드', 'KB', '신한', 'BNK'],
  minPurchaseKrw: 100_000,
  /** 카드 적용환율(원/USD)의 정상 범위. 벗어나면 금액 짝이 틀렸을 가능성이 크다. */
  rateRange: [1_000, 1_800],
});

const list = (v, fallback) => (Array.isArray(v) && v.length ? [...v] : [...fallback]);

/** 회사 설정에서 품의 규칙을 뽑는다. 설정이 비어 있어도 양식 선택지는 기본값으로 동작한다. */
export function purchaseRules(config = {}) {
  const p = config.purchase ?? {};
  const projects = config.projects ?? {};
  return {
    formName: p.formName ?? DEFAULTS.formName,
    budgets: list(config.budgets, DEFAULTS.budgets),
    projectBudget: p.projectBudget ?? DEFAULTS.projectBudget,
    projects: Object.keys(projects),
    subProjectsOf: (project) => [...(projects[project]?.subProjects ?? [])],
    payWhen: list(p.payWhen, DEFAULTS.payWhen),
    payMethods: list(p.payMethods, DEFAULTS.payMethods),
    cardMethod: p.cardMethod ?? DEFAULTS.cardMethod,
    cardIssuers: list(p.cardIssuers, DEFAULTS.cardIssuers),
    minPurchaseKrw: Number(config.minPurchaseKrw ?? DEFAULTS.minPurchaseKrw),
    rateRange: list(p.rateRange, DEFAULTS.rateRange),
  };
}

const WEEKDAY = ['일', '월', '화', '수', '목', '금', '토'];
const DATE = /^\d{4}-\d{2}-\d{2}$/;

/** `2026-09-18` → `2026-09-18(금)`. 날짜가 아니면 그대로 둔다. */
export function withWeekday(date) {
  const m = String(date ?? '').match(/^(\d{4})-(\d{2})-(\d{2})$/);
  if (!m) return date ?? '';
  return `${date}(${WEEKDAY[new Date(Date.UTC(+m[1], +m[2] - 1, +m[3])).getUTCDay()]})`;
}

/**
 * 구입수량 칸. 품명의 `/ N개` 를 더한다. `총 N개` 가 있으면 그것을 쓴다. `qty` 로 덮어쓸 수 있다.
 * 단위가 섞이면 `개` 로 센다.
 */
export function totalQuantity(p) {
  if (p.qty != null) return String(p.qty);
  const total = String(p.item ?? '').match(/총\s*(\d+)\s*(개|대|세트|건|쌍)/);
  if (total) return `${total[1]}${total[2]}`;
  const parts = [...String(p.item ?? '').matchAll(/\/\s*(\d+)\s*(개|대|세트|건|쌍)/g)];
  if (!parts.length) return '';
  const units = new Set(parts.map((x) => x[2]));
  const n = parts.reduce((s, x) => s + Number(x[1]), 0);
  return `${n}${units.size === 1 ? [...units][0] : '개'}`;
}

/**
 * 총금액을 공급가액과 부가세로 나눈다. 국내 결제는 부가세 포함가라 10/110 을 세액으로 본다
 * (거래명세표의 산출 기준과 같다). 해외 결제는 부가세가 없다. `vat` 로 덮어쓸 수 있다.
 */
export function splitVat(p, amount) {
  const vat = p.vat != null ? Number(p.vat) : (p.usd != null ? 0 : Math.round((amount * 10) / 110));
  return { supply: amount - vat, vat };
}

const won = (n) => (Number.isFinite(n) ? n.toLocaleString('ko-KR') : '');

/** 품의 한 건의 원화 총액. 결제 전 외화에 환율이 없으면 NaN 이다. */
export function amountKrw(p, fx = p.fx) {
  if (p.paidKrw != null) return Number(p.paidKrw);
  if (p.usd != null) return fx?.rate ? toKrw(p.usd, fx.rate) : NaN;
  return p.amount == null ? NaN : Number(p.amount);
}

/**
 * 양식에 넣을 값을 한 곳에서 만든다. **채울 때와 검증할 때 이 값을 같이 쓴다.**
 * 둘이 따로 계산하면 채운 것과 검증한 것이 어긋나도 모른다.
 *
 * `fx` 를 주지 않으면 초안에 고정한 `p.fx` 를 쓴다.
 */
export function formValues(p, { config = {}, fx } = {}) {
  const r = purchaseRules(config);
  const rate = fx ?? p.fx ?? null;
  const card = p.payMethod === r.cardMethod;
  const amount = amountKrw(p, rate);
  const { supply, vat } = splitVat(p, amount);
  const note = [...(p.note ?? [])];
  if (p.paidKrw != null && p.usd != null) note.push(paidNote({ usd: p.usd, krw: p.paidKrw, at: p.paidAt }));
  else if (p.paidKrw == null && p.usd != null && rate?.rate) note.push(fxNote({ usd: p.usd, fx: rate }));
  const projectBudget = p.budget === r.projectBudget;
  return {
    title: p.title ?? '',
    budget: p.budget ?? '',
    project: projectBudget ? (p.project ?? '') : '',
    subProject: projectBudget && r.subProjectsOf(p.project).length ? (p.subProject ?? '') : '',
    item: p.item ?? '',
    vendor: p.vendor ?? '',
    quantity: totalQuantity(p),
    // 구입일자는 결제일이다(회계 지시). 결제 전 품의만 예정일을 쓴다.
    purchaseDate: withWeekday(p.paidAt ?? p.purchaseDate ?? ''),
    deliveryDate: withWeekday(p.deliveryDate ?? ''),
    purpose: p.purpose ?? '',
    amount: won(amount),
    supply: won(supply),
    vat: won(vat),
    amountNumber: amount,
    payWhen: p.payWhen ?? '',
    payMethod: p.payMethod ?? '',
    cardIssuer: card ? (p.cardIssuer ?? '') : '',
    cardLast4: card ? String(p.cardLast4 ?? '') : '',
    account: card ? '해당없음' : (p.account ?? ''),
    accountHolder: card ? '해당없음' : (p.accountHolder ?? ''),
    note: note.join('\n'),
    attachments: (p.quotes ?? []).map((q) => q.file),
  };
}

/** 목적으로 안 쳐 주는 말들. 이것만 남으면 품명을 되풀이한 것이다. */
const FILLER = ['구매', '구입', '주문', '신청', '필요', '건', '및', '외', '용'];

/** 영수증, 주문내역으로 보이는 파일명. 구매 품의서에는 붙이지 않는다. */
export const RECEIPT_LIKE = /영수증|인보이스|주문내역|거래명세|receipt|invoice|order/i;

/** 칸 이름. 오류 메시지에 쓴다. */
export const FIELD_LABELS = {
  key: '품의 키', title: '제목', budget: '예산', item: '구매품명', vendor: '업체명', purpose: '구입목적',
};

/**
 * 구매 한 건이 상신할 만한지 본다. `{ errors, warnings }` 를 돌려준다.
 *
 * `filed` 는 이미 올라간 문서(상신취소한 임시저장 포함)를 고치는 경우다. 옛 양식에는 세부 과제와
 * 구입일자 칸이 없어 그 둘을 요구하지 않는다. 기본은 `docId` 가 있는지로 정한다.
 * 견적 캡처는 `p.quotes` (`[{ file, url, manual }]`)로 받는다. 파일이 실제로 있는지는 부르는 쪽이 본다.
 */
export function validatePurchase(p, { config = {}, fx, filed = Boolean(p.docId) } = {}) {
  const r = purchaseRules(config);
  const errors = [];
  const warnings = [];
  const need = (field) => {
    if (!String(p[field] ?? '').trim()) errors.push(`${FIELD_LABELS[field] ?? field} 칸이 비어 있다 (${field})`);
  };

  for (const f of ['key', 'title', 'budget', 'item', 'vendor', 'purpose']) need(f);
  if (errors.length) return { errors, warnings };
  const rate = fx ?? p.fx ?? null;
  const projectBudget = p.budget === r.projectBudget;

  // 물건은 `구매의 건`, 구독 서비스는 `구독의 건`, 도메인, 라이선스 연장은 `갱신의 건`, 사용량
  // 청구는 `지출의 건` 이다. 구독을 "구매" 라고 쓰면 말이 안 된다. 뒤에 괄호 보충이 붙는 것까지 인정한다.
  if (!/(구매|구독|갱신|지출)의 건(\s*\([^)]+\))?$/.test(p.title)) {
    errors.push(`제목은 "… 구매의 건, 구독의 건, 갱신의 건, 지출의 건" 으로 끝난다: "${p.title}"`);
  }
  if (!/\/\s*(총\s*)?[\d.,]+\s*(개|대|종|식|건|벌|석|세트|개입|개월|계정|EA)|\/\s*USD\s*[\d.]+/i.test(p.item)) {
    errors.push(`구매품명에 수량이 없다: "${p.item}" (예: "… / 1개")`);
  }

  if (!r.budgets.includes(p.budget)) errors.push(`예산은 ${r.budgets.join(', ')} 중 하나다: "${p.budget}"`);
  if (projectBudget) {
    if (!r.projects.length) {
      errors.push('회사 설정에 과제 목록(projects)이 없다. 관리자에게 설정을 요청한다');
    } else if (!r.projects.includes(p.project)) {
      errors.push(`프로젝트가 선택지에 없다: "${p.project ?? ''}" (${r.projects.join(', ')})`);
    } else {
      // 세부 과제가 있는 과제는 고른다. 이미 올라간 옛 양식 문서에는 그 칸이 없다.
      const subs = r.subProjectsOf(p.project);
      if (!filed && subs.length && !subs.includes(p.subProject)) {
        errors.push(`${p.project} 는 세부 과제(${subs.join(', ')})를 고른다: "${p.subProject ?? ''}". 모르면 사람에게 묻는다`);
      }
    }
  }
  if (!filed && !(p.paidAt ?? p.purchaseDate)) {
    errors.push('구입일자가 없다. 결제했으면 paidAt(카드 결제일), 결제 전이면 purchaseDate 를 적는다');
  }
  for (const f of ['paidAt', 'purchaseDate']) {
    if (p[f] != null && !DATE.test(String(p[f]))) errors.push(`${f} 는 YYYY-MM-DD 다: "${p[f]}"`);
  }

  const hasKrw = p.amount != null;
  const hasUsd = p.usd != null;
  if (hasKrw === hasUsd) errors.push('amount(원화)와 usd(외화) 중 정확히 하나만 적는다');
  if (hasKrw && !(Number(p.amount) > 0)) errors.push(`금액이 이상하다: ${p.amount}`);
  if (hasUsd && !(Number(p.usd) > 0)) errors.push(`외화 금액이 이상하다: ${p.usd}`);

  // 결제가 끝난 외화 건은 카드 승인액을 그대로 쓴다. 환산하면 대장과 어긋난다.
  if (p.paidKrw != null) {
    if (!hasUsd) errors.push('paidKrw 는 usd 와 함께 쓴다. 원화 결제면 amount 에 적는다');
    if (!(Number(p.paidKrw) > 0)) errors.push(`카드 승인액이 이상하다: ${p.paidKrw}`);
    if (!DATE.test(String(p.paidAt ?? ''))) errors.push('paidKrw 에는 카드 승인일 paidAt (YYYY-MM-DD) 이 필요하다');
    const implied = Number(p.paidKrw) / Number(p.usd);
    if (hasUsd && !(implied > r.rateRange[0] && implied < r.rateRange[1])) {
      errors.push(`적용환율이 정상 범위를 벗어난다: ${implied.toFixed(1)}원/USD. 금액 짝이 틀렸을 수 있다`);
    }
  } else if (hasUsd) {
    if (!rate?.rate) errors.push('결제 전 외화 품의에 환율이 없다. 초안을 다시 저장하면 오늘 매매기준율을 받는다');
    if (p.paidAt && p.payMethod === r.cardMethod) {
      warnings.push('이미 카드로 결제한 외화 건이다. 카드 승인액이 대장에 올라오면 paidKrw 로 넣어야 품의 금액이 카드와 맞는다');
    }
  }

  // 최소 집행 금액(회계 안내). 이미 결제한 건은 막지 않고 경고만 한다.
  const krw = amountKrw(p, rate);
  if (Number.isFinite(krw) && krw > 0 && krw < r.minPurchaseKrw) {
    warnings.push(`${won(krw)}원: ${won(r.minPurchaseKrw)}원 미만 결제는 집행하지 않는 것이 회계 안내다. 장바구니에 모아 한 번에 결제한다`);
  }

  if (p.deliveryDate != null && p.deliveryDate !== '해당없음' && !DATE.test(String(p.deliveryDate))) {
    errors.push(`배송일자는 YYYY-MM-DD 또는 해당없음이다: "${p.deliveryDate}"`);
  }
  if (!r.payWhen.includes(p.payWhen)) errors.push(`지급요청일은 ${r.payWhen.join(', ')} 중 하나다: "${p.payWhen ?? ''}"`);
  if (!r.payMethods.includes(p.payMethod)) errors.push(`지급방법이 선택지에 없다: "${p.payMethod ?? ''}" (${r.payMethods.join(', ')})`);
  if (p.payMethod === r.cardMethod) {
    if (!r.cardIssuers.includes(p.cardIssuer)) errors.push(`카드사가 선택지에 없다: "${p.cardIssuer ?? ''}" (${r.cardIssuers.join(', ')})`);
    if (!/^\d{4}$/.test(String(p.cardLast4 ?? ''))) errors.push('카드번호 뒷자리가 네 자리가 아니다');
  } else if (r.payMethods.includes(p.payMethod) && (!p.account || !p.accountHolder)) {
    errors.push(`${p.payMethod} 는 지급계좌와 예금주가 필요하다`);
  }

  // 본문 문구 검사. 목적과 기타는 사람이 쓰는 문장이라 말투가 섞인다.
  // 상품명과 업체명은 고유명사가 그대로 들어가므로 검사하지 않는다.
  for (const w of checkWriting(p.purpose, '구입목적')) errors.push(w);
  for (const w of checkWriting((p.note ?? []).join('\n'), '기타')) errors.push(w);
  for (const w of checkNoteStyle(p.note)) errors.push(w);
  for (const w of checkWriting(p.title, '제목')) errors.push(w);

  const purpose = String(p.purpose).trim();
  let residue = purpose.toLowerCase();
  for (const token of String(p.item).toLowerCase().split(/[^0-9a-z가-힣]+/).filter((t) => t.length >= 2)) {
    residue = residue.split(token).join(' ');
  }
  for (const word of FILLER) residue = residue.split(word).join(' ');
  if (residue.replace(/[^0-9a-z가-힣]/g, '').length < 4) {
    errors.push(`구입목적이 품명을 되풀이한다: "${purpose}". 대상과 용도를 쓴다 (예: "시험 로봇 원격조종"). 모르면 사람에게 묻는다`);
  }

  // 판매 페이지 캡처는 **필수다**. 견적서 자리에 들어가는 근거라 없으면 올릴 수 없다.
  // 페이지 가격이 결제액과 다른 것(묶음, 쿠폰, 가격 변동)은 캡처를 빼는 이유가 못 된다.
  // 차이를 기타 칸에 적어 둔다.
  if (!p.quotes?.length) errors.push('판매 페이지 캡처가 없다. capture_quote 로 찍기 전에는 올리지 않는다');
  for (const q of p.quotes ?? []) {
    if (RECEIPT_LIKE.test(q.file)) errors.push(`영수증과 주문내역은 첨부하지 않는다: ${q.file}`);
    if (!q.manual && !q.url) errors.push(`캡처한 주소가 없다: ${q.file}`);
  }
  if (!(p.note ?? []).some((l) => /https?:\/\//.test(l))) warnings.push('기타 칸에 상품 링크가 없다');

  return { errors, warnings };
}

/**
 * 품의의 결재선. 기본(`lines.purchase` = `project`)은 사업비 결재선이라 예산이 사업비가 아니면
 * 규칙이 없다. 그때는 지어내지 않고 멈춘다.
 */
export function purchaseApprovers(config, p, v = formValues(p, { config })) {
  const r = purchaseRules(config);
  const spec = config.lines?.purchase ?? 'project';
  if (spec === 'project' && v.budget !== r.projectBudget) {
    throw new PluginError('VALIDATION',
      `${v.budget || '(예산 없음)'} 품의의 결재선 규칙이 없습니다. 결재선을 사람에게 묻고 회사 설정 lines.purchase 에 넣어야 합니다.`);
  }
  return purchaseLine(config, { project: v.project, amount: v.amountNumber });
}

/** 검토표. 상신 전에 사람에게 보여 주는 칸별 값이다. */
export function fieldTable(v) {
  const pay = [v.payWhen, v.payMethod, v.cardIssuer, v.cardLast4].filter(Boolean).join(', ');
  const rows = [
    ['제목', v.title],
    ['예산', [v.budget, v.project, v.subProject].filter(Boolean).join(' / ')],
    ['구매품명', v.item],
    ['업체명', v.vendor],
    ['구입수량', v.quantity],
    ['구입일자', v.purchaseDate],
    ...(v.deliveryDate ? [['배송일자', v.deliveryDate]] : []),
    ['구입목적', v.purpose],
    ['총금액', v.amount ? `${v.amount}원 (공급가액 ${v.supply}원, 부가세액 ${v.vat}원)` : ''],
    ['지급', pay],
    ['지급계좌, 예금주', `${v.account || '(없음)'}, ${v.accountHolder || '(없음)'}`],
  ];
  const lines = rows.map(([k, val]) => `- ${k}: ${val || '(비어 있음)'}`);
  const note = String(v.note ?? '').split('\n').filter(Boolean);
  lines.push(`- 기타: ${note.length ? '' : '(없음)'}`.trimEnd());
  for (const l of note) lines.push(`    ${l}`);
  lines.push(`- 첨부: ${v.attachments.length ? v.attachments.join(', ') : '(없음)'}`);
  return lines;
}

// ------------------------------------------------------------------ DB rows

/** 엔트리에서 DB 행으로 가지 않는 키. 행의 칼럼이나 다른 표가 맡는다. */
const ROW_OWNED = new Set(['key', 'status', 'docId', 'quotes', 'id', 'owner', 'createdAt', 'updatedAt']);

/**
 * DB 행 → 원본 `PURCHASES` 한 건과 같은 모양. 칼럼이 `data` 보다 앞선다.
 * `quotes` 는 `db.quotes(row.id)` 의 행을 넘긴다.
 */
export function purchaseEntry(row, quotes = []) {
  if (!row) return null;
  return {
    ...(row.data ?? {}),
    key: row.key,
    status: row.status,
    title: row.title || row.data?.title || '',
    paidAt: row.paidAt ?? row.data?.paidAt ?? undefined,
    usd: row.usd ?? row.data?.usd ?? undefined,
    paidKrw: row.paidKrw ?? row.data?.paidKrw ?? undefined,
    docId: row.docId ?? undefined,
    quotes: quotes.map((q) => ({ file: q.file, url: q.url ?? undefined, manual: Boolean(q.data?.manual), digest: q.digest })),
  };
}

/** 엔트리 → `db.upsertPurchase()` 인자. `amountKrw` 는 품의 총액이다(카드 짝짓기가 쓴다). */
export function purchaseRow(owner, entry, { status = entry.status ?? 'draft' } = {}) {
  const data = Object.fromEntries(Object.entries(entry)
    .filter(([k, v]) => !ROW_OWNED.has(k) && v !== undefined));
  const total = amountKrw(entry);
  return {
    owner,
    key: entry.key,
    status,
    title: entry.title ?? '',
    paidAt: entry.paidAt ?? null,
    amountKrw: Number.isFinite(total) ? total : null,
    usd: entry.usd ?? null,
    paidKrw: entry.paidKrw ?? null,
    docId: entry.docId ?? null,
    data,
  };
}
