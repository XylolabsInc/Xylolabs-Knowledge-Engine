import { PluginError } from '../errors.mjs';
import { checkDocId } from './documents.mjs';

/**
 * 구매 품의서 문서를 서버에서 읽어 칸별로 풀고, 기대값과 대조한다. 원본 저장소
 * `purchase-doc.mjs` 를 옮겼다.
 *
 * **올린 뒤에는 반드시 이걸로 확인한다.** 상태가 `진행중` 이라고 내용이 맞는 것은 아니다.
 * 제목이 전부 빈 채로 올라간 적이 있고, 금액이 틀린 적도 있다. 상태와 첨부 수만 보고 성공이라고
 * 보고했었다.
 *
 * 본문(`document.docBodyContent`)은 입력 요소가 아니라 `span[data-type]` 으로 저장된다.
 *
 *   text, textarea, calendar  data-value 에 값
 *   radio                     고른 쪽에만 data-select-option="true"
 *   cSel (선택)               data-selectval 에 고른 값
 *
 * 제목은 본문이 아니라 `document.title` 이다. 비워 두면 양식 이름이 들어간다.
 * 본문은 페이지의 DOMParser 로 푼다(스크립트도 이미지도 실행되지 않는다).
 */

/**
 * 칸 이름. 양식이 바뀌어 배송일자와 금액(vat포함) 대신 구입수량, 구입일자, 총금액(vat포함,
 * 공급가액, 부가세액)이 생겼다. 옛 문서를 읽을 수 있게 둘 다 둔다.
 */
export const LABELS = [
  '예산', '구매품명', '업체명', '구입수량', '구입일자', '배송일자', '구입목적',
  '총금액(vat포함)', '금액(vat포함)',
  '지급요청일', '지급방법', '지급계좌(은행)', '예금주', '기타(링크)',
];

/** 금액 칸 값. 새 양식은 총금액(vat포함) 의 첫 칸, 옛 양식은 금액(vat포함). */
export const amountOf = (fields) => (fields['총금액(vat포함)'] ?? fields['금액(vat포함)'] ?? []).filter((p) => p.type === 'text')[0]?.value ?? '';

/**
 * 페이지 안에서 돌리는 본문 파서. `{ 라벨: [{ type, value }] | null }`.
 * 시험이 같은 함수를 실제 Chromium 에 넣어 돌린다.
 */
export function parsePurchaseBody({ html, labels }) {
  const root = new DOMParser().parseFromString(String(html ?? ''), 'text/html');
  const norm = (t) => String(t ?? '').replace(/[\s ]/g, '');
  const cellOf = (label) => [...root.querySelectorAll('td, th')]
    .find((c) => norm(c.textContent) === norm(label))?.nextElementSibling;
  const out = {};
  for (const label of labels) {
    const cell = cellOf(label);
    if (!cell) { out[label] = null; continue; }
    const parts = [];
    for (const s of cell.querySelectorAll('[data-type]')) {
      const t = s.getAttribute('data-type');
      if (t === 'radio') {
        if (s.getAttribute('data-select-option') === 'true') parts.push({ type: 'radio', value: s.getAttribute('data-value') });
      } else if (t === 'cSel') {
        parts.push({ type: 'select', value: s.getAttribute('data-selectval') ?? '' });
      } else {
        parts.push({ type: 'text', value: s.getAttribute('data-value') ?? '' });
      }
    }
    out[label] = parts;
  }
  return out;
}

/** 문서 하나를 읽는다. 본문 파싱은 페이지의 DOMParser 를 빌린다. */
export async function readPurchaseDoc(page, api, docId) {
  const id = checkDocId(docId);
  const r = await api(`/gw/api/approval/document/${id}`);
  if (r.status === 404) throw new PluginError('NOT_FOUND', `문서 ${id} 를 찾지 못했습니다.`);
  if (r.status !== 200) throw new PluginError('UPSTREAM', `문서 ${id} 를 읽지 못했습니다 (HTTP ${r.status}).`);
  const data = r.data?.data ?? {};
  const doc = data.document ?? {};

  const fields = await page.evaluate(parsePurchaseBody, { html: String(doc.docBodyContent ?? ''), labels: LABELS });

  const groups = data.apprFlow?.activityGroups ?? [];
  return {
    docId: id,
    formName: doc.formName ?? '',
    title: doc.title ?? '',
    status: doc.docStatusName ?? '',
    // 결재가 끝난 문서를 새 문서에 연결할 때 쓴다(문서번호로 검색한다).
    docNum: doc.docNum ?? '',
    fields,
    attachments: (doc.attaches ?? []).map((a) => a.name),
    line: groups.flatMap((g) => (g.activities ?? []).map((a) => ({
      group: g.name, name: a.userName, display: a.displayName, status: a.statusName,
    }))),
    canCancel: Boolean(data.actionCheck?.isAllowCancelDraftUser),
  };
}

/** 양식이 구입일자 칸을 가진 새 양식인가. 새 양식인지 옛 양식인지는 구입일자 칸으로 가린다. */
export const isNewForm = (fields) => fields['구입일자'] != null;

/**
 * 읽은 문서와 기대값(`formValues()`)을 칸별로 대조한다. 다른 것만 모아 돌려준다.
 * 빈 배열이면 전부 맞다.
 */
export function diffPurchaseDoc(actual, expected, { projectBudget = '프로젝트 사업비', cardMethod = '법인카드' } = {}) {
  const diffs = [];
  const eq = (label, got, want) => {
    if (String(got ?? '') !== String(want ?? '')) diffs.push({ label, got, want });
  };
  const vals = (label, type) => (actual.fields[label] ?? []).filter((p) => !type || p.type === type).map((p) => p.value);

  eq('제목', actual.title, expected.title);
  eq('예산', vals('예산', 'radio')[0], expected.budget);
  if (expected.budget === projectBudget) eq('예산 프로젝트', vals('예산', 'select')[0], expected.project);
  const newForm = isNewForm(actual.fields);
  if (newForm && expected.subProject) eq('예산 세부 과제', vals('예산', 'radio')[1], expected.subProject);
  eq('구매품명', vals('구매품명')[0], expected.item);
  eq('업체명', vals('업체명')[0], expected.vendor);
  if (newForm) {
    eq('구입수량', vals('구입수량')[0], expected.quantity);
    eq('구입일자', vals('구입일자')[0], expected.purchaseDate);
  } else {
    eq('배송일자', vals('배송일자')[0], expected.deliveryDate);
  }
  eq('구입목적', vals('구입목적')[0], expected.purpose);
  if (newForm) {
    eq('총금액(vat포함)', vals('총금액(vat포함)')[0], expected.amount);
    eq('총금액 공급가액', vals('총금액(vat포함)')[1], expected.supply);
    eq('총금액 부가세액', vals('총금액(vat포함)')[2], expected.vat);
  } else {
    eq('금액(vat포함)', vals('금액(vat포함)')[0], expected.amount);
  }
  eq('지급요청일', vals('지급요청일', 'radio')[0], expected.payWhen);
  eq('지급방법', vals('지급방법', 'select')[0], expected.payMethod);
  if (expected.payMethod === cardMethod) {
    eq('지급방법 카드번호', vals('지급방법', 'text')[0], expected.cardLast4);
    eq('지급방법 카드사', vals('지급방법', 'select')[1], expected.cardIssuer);
  }
  eq('지급계좌(은행)', vals('지급계좌(은행)')[0], expected.account);
  eq('예금주', vals('예금주')[0], expected.accountHolder);
  eq('기타(링크)', vals('기타(링크)')[0], expected.note);

  const wantFiles = [...expected.attachments].sort();
  const gotFiles = [...actual.attachments].sort();
  eq('첨부', gotFiles.join(' | '), wantFiles.join(' | '));

  return diffs;
}

/**
 * 서버 문서의 결재선(기안자 뒤)이 기대한 사람 순서와 같은가. 다르면 diff 한 줄, 같으면 null.
 * API 는 구획 이름을 `신청`, `승인` 으로 준다. 첫 줄은 기안자다.
 */
export function diffLine(doc, approvers) {
  const want = approvers.map((a) => a.name).join(',');
  const got = doc.line.slice(1).map((a) => a.name).join(',');
  return want === got ? null : { label: '결재선', got: doc.line.map((a) => a.name).join(','), want };
}

/** `"제목": "a" ≠ "b"` 꼴의 한 줄. 오류 메시지에 쓴다. */
export const describeDiff = (d) => `${d.label}: "${d.got ?? ''}" ≠ "${d.want ?? ''}"`;
