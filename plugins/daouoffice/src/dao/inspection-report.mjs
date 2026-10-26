import fs from 'node:fs';
import path from 'node:path';
import { PluginError } from '../errors.mjs';
import { requireExecute } from '../guard.mjs';
import { dismissDialogs } from './dialogs.mjs';
import { setApprovalLine, submitDocument, linkDocuments, withdrawDocument } from './approval.mjs';
import { inspectionLine, describeLine } from './approvers.mjs';
import { cardHolder, last4Of } from './cards.mjs';
import { listAllDrafts } from './documents.mjs';
import { reportRows, splitVat, headOf, intangible, photosOfKeys, photosFor } from './inspection.mjs';

/**
 * 검수조서 한 장을 계획하고 올린다. 원본 저장소 `inspection-report.mjs` 의 `plan()`, `fillForm()`,
 * 첨부, 결재선, 상신, 읽어 와 대조하는 흐름을 그대로 옮겼다. 바뀐 것은 데이터의 출처(DB),
 * 사람마다 다른 부서와 이름, 결재선(회사 설정), 첨부를 메모리에서 바로 넘기는 것이다.
 *
 * 양식: 프로젝트명, 결제방식(법인카드), 결제 카드번호, 거래처명, 사업자등록번호와 계좌번호(카드
 * 결제는 해당없음), 총 금액(카드 승인 금액), 입고일자(사진 찍은 날, 구독은 결제일), 비고(구매 품의와
 * 지출결의 문서번호). 세부내역은 품명, 거래처명, 수량, 공급가액, 부가세액금, 합계이고 20줄까지다.
 * 첨부는 영수증과 제품 사진이다(양식 안내 "1. 견적서 또는 영수증 2. 제품 사진").
 *
 * 제목: `YYYY년 M월 (<카드 뒤 네 자리>/<부서 코드>/<카드 명의자>) 검수조서 - <품의 제목에서 구매의 건을 뺀 것>`.
 * 달은 결제한 달이다.
 */

export const FORM_NAME = '검수조서';
const won = (n) => Number(n).toLocaleString('ko-KR');

/**
 * 입고일자는 사진 찍은 날이다. 한 결제의 사진이 여럿이면 가장 늦은 날. 날짜는 사진을 받을 때
 * 메타데이터를 지우기 전에 읽어 둔다. 없으면 지어내지 않고 묻는다.
 */
export function takenDateOf(photos) {
  const missing = photos.filter((p) => !p.takenDate);
  if (missing.length) {
    throw new PluginError('VALIDATION',
      `촬영 날짜를 모르는 사진이 있습니다 (사진 ${missing.map((p) => p.id).join(', ')}). 언제 찍었는지 물어 save_photos 의 takenDate 로 적어 주세요.`,
      { data: { photoIds: missing.map((p) => p.id) } });
  }
  return photos.map((p) => p.takenDate).sort().at(-1);
}

/**
 * 결제 한 건(`inspectionUnits()` 의 한 줄)의 검수조서 계획. 올리기 전에 막아야 할 것은 모두 여기서
 * `VALIDATION` 으로 막는다. 미리보기와 실행이 같은 계획을 쓴다.
 *
 * @param {object} o
 * @param {object} o.unit        { a, ps, key, perOrder, cardAmount }
 * @param {object} o.card        listCards() 의 카드
 * @param {object[]} o.photos    그 사람의 사진 행 전체
 * @param {(r: object) => string|null} o.receiptPath  영수증 행 → 저장된 파일 경로(없으면 null)
 * @param {(r: object) => string|null} o.photoPath    사진 행 → 저장된 파일 경로(없으면 null)
 * @param {object} o.marks       inspectionMarks()
 * @param {Map<string,string>} o.docNums  문서 ID → 문서번호
 * @param {object} o.profile     accounts 행 (name, deptCode)
 * @param {object} o.config      회사 설정
 */
export function planReport({ unit, card, photos, receiptPath, photoPath, marks, docNums, profile, config }) {
  const { a, ps } = unit;
  const r = a.row;
  const day = String(r.useDate).slice(0, 10);
  const last4 = last4Of(card);
  const warnings = [];

  const done = marks.doneOf(ps);
  if (done) throw new PluginError('CONFLICT', `검수조서가 따로 처리된 결제입니다: ${done}`);
  const service = ps.every(intangible);
  const mine = service ? [] : photosOfKeys(photos, ps.map((p) => p.key));
  if (!service && !mine.length) {
    throw new PluginError('VALIDATION', [
      `이 결제의 제품 사진이 없습니다 (${day} ${r.merchant} ${won(r.amount)}원). 검수조서는 사진이 있어야 올립니다.`,
      '찍을 사진:', ...photosFor(ps).map((s) => `- ${s}`),
    ].join('\n'), { data: { missing: 'photos', key: unit.key } });
  }
  const incomplete = marks.incompleteOf(ps);
  if (incomplete) {
    throw new PluginError('VALIDATION', `사진에 빠진 품목이 있습니다: ${incomplete}. 사진을 더 받은 뒤 mark_inspection(clear)으로 풀어 주세요.`,
      { data: { missing: 'items', key: unit.key } });
  }
  const unfiled = ps.filter((p) => !p.docId);
  if (unfiled.length) {
    throw new PluginError('VALIDATION', `구매 품의가 아직 올라가지 않았습니다: ${unfiled.map((p) => p.title).join(', ')}. 품의를 먼저 올립니다.`);
  }
  if (!profile?.deptCode) {
    throw new PluginError('VALIDATION', '부서 코드를 모릅니다. 조직도에서 읽지 못해 제목을 만들 수 없습니다. 관리자에게 회사 설정의 departments 를 요청해 주세요.');
  }

  // 주문마다 나눈 결제는 그 품의의 영수증만 붙인다(외화 금액이 같은 영수증). 못 고르면 결제의 영수증 전부.
  let receipts = a.receipts ?? (a.receipt ? [a.receipt] : []);
  if (unit.perOrder && receipts.length > 1) {
    const own = receipts.filter((x) => x.currency === 'USD' && ps[0].usd != null && Math.abs(x.amount - ps[0].usd) < 0.005);
    if (own.length === 1) receipts = own;
  }
  if (service && !receipts.length) {
    throw new PluginError('VALIDATION', '구독과 이용료 결제는 사진 대신 인보이스를 붙입니다. 인보이스(영수증)를 save_receipt 로 먼저 저장해 주세요.');
  }
  if (!service && !receipts.length) warnings.push('영수증이 없습니다. 양식은 견적서 또는 영수증을 첨부하라고 합니다.');

  const files = [];
  for (const x of receipts) {
    const p = receiptPath(x);
    if (!p) throw new PluginError('VALIDATION', `영수증 파일이 저장돼 있지 않습니다: ${x.file}. 영수증을 다시 보내 save_receipt 로 저장해 주세요.`);
    files.push({ kind: 'receipt', name: x.file, path: p, mimeType: /\.pdf$/i.test(x.file) ? 'application/pdf' : 'image/jpeg' });
  }
  mine.forEach((ph, i) => {
    const p = photoPath(ph);
    if (!p) throw new PluginError('VALIDATION', `사진 파일이 저장돼 있지 않습니다 (사진 ${ph.id}).`);
    files.push({ kind: 'photo', name: `제품사진_${i + 1}.jpg`, path: p, mimeType: 'image/jpeg', photoId: ph.id });
  });

  const lines = splitVat(reportRows(r, ps, marks.itemRows), ps.some((p) => p.usd != null));
  const [y, m] = day.split('-');
  const holder = cardHolder(card);
  if (profile.name && holder !== profile.name) warnings.push(`카드 명의자(${holder})와 기안자(${profile.name})가 다릅니다.`);
  const sub = ps.map((p) => p.subProject).find(Boolean);
  const project = ps[0].project ?? config.cards?.[last4]?.project ?? '';
  const issuer = ps[0].cardIssuer ?? config.cards?.[last4]?.issuer ?? card.issuer ?? '';
  const vendors = [...new Set(ps.map((p) => p.vendor).filter(Boolean))];
  const docNumOf = (id) => docNums.get(String(id)) ?? String(id);
  const amount = Number(r.amount);

  return {
    key: unit.key,
    payment: { date: day, merchant: r.merchant, amount, cardAmount: unit.cardAmount, perOrder: unit.perOrder, claimDocId: r.docId ?? null, rowId: r.id ?? null },
    purchases: ps.map((p) => ({ key: p.key, title: p.title, docId: p.docId })),
    service,
    title: `${y}년 ${Number(m)}월 (${last4}/${profile.deptCode}/${holder}) ${FORM_NAME} - ${headOf(ps, lines)}`,
    header: {
      프로젝트명: sub ? `${project} (${sub})` : project,
      결제방식: '법인카드',
      '결제 카드번호': `${issuer} ${card.cardNo || `****-${last4}`}`.trim(),
      거래처명: vendors.join(', '),
      // 카드 결제는 사업자등록번호와 계좌번호를 `해당없음` 으로 쓴다.
      사업자등록번호: '해당없음',
      '(은행)계좌번호': '해당없음',
      '총 금액(VAT포함)': won(amount),
      입고일자: service ? day : takenDateOf(mine),
      비고: [`구매 품의 ${ps.map((p) => docNumOf(p.docId)).join(', ')}`,
        r.docId ? `지출결의 ${docNumOf(r.docId)}` : null].filter(Boolean).join(', '),
    },
    approvers: inspectionLine(config, profile.name, { project, amount }),
    lines,
    total: {
      수량: '',
      공급가액: won(lines.reduce((t, l) => t + l.supply, 0)),
      부가세액금: won(lines.reduce((t, l) => t + l.vat, 0)),
      합계: won(amount),
    },
    files,
    photoIds: mine.map((ph) => ph.id),
    warnings,
  };
}

/** 미리보기 글. 칸마다 값을 보여 준다. */
export function describePlan(plan) {
  const L = [`검수조서: ${plan.title}`, ''];
  for (const [k, v] of Object.entries(plan.header)) L.push(`- ${k}: ${v}`);
  L.push('', '세부내역 (품명 | 거래처 | 수량 | 공급가액 | 부가세 | 합계)');
  plan.lines.forEach((l, i) => L.push(`${i + 1}. ${l.name} | ${l.vendor} | ${l.qty || '-'} | ${won(l.supply)} | ${won(l.vat)} | ${won(l.total)}`));
  L.push(`합계: 공급가액 ${plan.total.공급가액}, 부가세 ${plan.total.부가세액금}, 합계 ${plan.total.합계}`);
  L.push('', `첨부 ${plan.files.length}개: ${plan.files.map((f) => f.name).join(', ')}`);
  L.push(`결재선: ${plan.approvers.length ? describeLine(plan.approvers) : '기안자 단독 (상신하면 바로 완료)'}`);
  for (const w of plan.warnings) L.push(`주의: ${w}`);
  return L.join('\n');
}

/** 내 기안함의 검수조서를 제목별로. 임시저장은 뺀다(상신취소한 문서). */
export async function filedByTitle(api) {
  const out = new Map();
  for (const d of await listAllDrafts(api)) {
    if (d.formName !== FORM_NAME || d.docStatusName === '임시저장') continue;
    out.set(d.title, [...(out.get(d.title) ?? []), d]);
  }
  return out;
}

/** 같은 제목의 옛 검수조서들. 다시 올리거나 고칠 때 본다. */
export async function oldReports(api, ids) {
  const out = [];
  for (const id of [...new Set(ids.map(String))]) {
    if (!/^\d{6,25}$/.test(id)) continue;
    const r = await api(`/gw/api/approval/document/${id}`);
    const d = r.data?.data ?? {};
    if (!r.ok || !d.document) continue;
    out.push({
      id,
      title: d.document.title ?? '',
      formName: d.document.formName ?? '',
      drafter: d.document.drafterName ?? '',
      docNum: d.document.docNum ?? null,
      status: d.document.docStatusName ?? '',
      canCancel: Boolean(d.actionCheck?.isAllowCancelDraftUser),
    });
  }
  return out;
}

/** 라벨 칸 옆 입력에 값을 넣는다. 세부내역은 줄 번호로, 칸은 머리글 위치로 찾는다. */
export async function fillForm(page, want) {
  await page.fill('#subject', want.title);
  const res = await page.evaluate(({ header, lines, total }) => {
    const norm = (t) => String(t ?? '').replace(/\s+/g, '');
    const set = (el, v) => {
      if (el.tagName === 'SELECT') {
        const o = [...el.options].find((x) => x.text.trim() === v);
        if (!o) return false;
        el.value = o.value;
      } else el.value = v;
      el.dispatchEvent(new Event('input', { bubbles: true }));
      el.dispatchEvent(new Event('change', { bubbles: true }));
      return true;
    };
    const miss = [];
    const cells = [...document.querySelectorAll('td, th')];
    for (const [label, v] of Object.entries(header)) {
      const cell = cells.find((c) => norm(c.innerText) === norm(label));
      const el = cell?.nextElementSibling?.querySelector('input, select, textarea');
      if (!el || !set(el, v)) miss.push(label);
    }
    // 세부내역 머리글의 x 위치로 칸을 고른다.
    const heads = cells.filter((c) => ['품명', '거래처명', '수량', '공급가액', '부가세액금', '합계'].includes(c.innerText.trim())
      && c.closest('table')?.innerText.includes('부가세액금') && c.closest('tr')?.innerText.includes('공급가액'));
    const colX = Object.fromEntries(heads.map((h) => { const b = h.getBoundingClientRect(); return [h.innerText.trim(), b.x + b.width / 2]; }));
    const inputsOfRow = (label) => {
      const tr = [...document.querySelectorAll('tr')].find((t) => norm(t.firstElementChild?.innerText) === label && t.querySelector('input'));
      return [...(tr?.querySelectorAll('input') ?? [])];
    };
    const pick = (inputs, col) => inputs.find((i) => { const b = i.getBoundingClientRect(); return Math.abs(b.x + b.width / 2 - colX[col]) < 40; });
    const keys = { name: '품명', vendor: '거래처명', qty: '수량', supply: '공급가액', vat: '부가세액금', total: '합계' };
    lines.forEach((l, i) => {
      const inputs = inputsOfRow(String(i + 1));
      for (const [k, col] of Object.entries(keys)) {
        const el = pick(inputs, col);
        if (!el) { miss.push(`${i + 1}행 ${col}`); continue; }
        set(el, l[k] === '' || l[k] == null ? '' : String(l[k]));   // 수량 칸은 숫자만 받는다
      }
    });
    const tin = inputsOfRow('합계');
    for (const [col, v] of Object.entries(total)) {
      const el = pick(tin, col);
      if (el) set(el, v); else if (v) miss.push(`합계 ${col}`);
    }
    return miss;
  }, {
    header: want.header,
    lines: want.lines.map((l) => ({ ...l, supply: won(l.supply), vat: won(l.vat), total: won(l.total) })),
    total: want.total,
  });
  if (res.length) throw new PluginError('UPSTREAM', `검수조서 양식의 칸을 채우지 못했습니다: ${res.join(', ')}. 관리자가 양식을 바꿨을 수 있습니다.`);
}

/** 서버 문서 본문의 값 목록(`data-value`). 채운 순서와 같다. */
export async function serverValues(page, api, docId) {
  const doc = (await api(`/gw/api/approval/document/${docId}`)).data?.data?.document ?? {};
  const values = await page.evaluate((html) => {
    const root = new DOMParser().parseFromString(html, 'text/html');
    return [...root.querySelectorAll('[data-type]')].map((s) => s.getAttribute('data-selectval') ?? s.getAttribute('data-value') ?? '');
  }, String(doc.docBodyContent ?? ''));
  return { doc, values };
}

/** 올린 문서 본문에 있어야 할 값. 본문 앞의 다섯 값(문서번호, 작성일자, 부서, 신청자, 제목)은 뺀 순서다. */
export function expectedValues(want) {
  const h = want.header;
  const out = [h.프로젝트명, h.결제방식, h['결제 카드번호'], h.거래처명, h.사업자등록번호, h['(은행)계좌번호'], h['총 금액(VAT포함)'], h.입고일자, h.비고];
  // 수량 칸은 숫자만 남긴다(`3개` → `3`).
  for (let i = 0; i < 20; i++) {
    const l = want.lines[i];
    out.push(...(l ? [l.name, l.vendor, String(l.qty).replace(/\D/g, ''), won(l.supply), won(l.vat), won(l.total)] : ['', '', '', '', '', '']));
  }
  out.push('', want.total.공급가액, want.total.부가세액금, want.total.합계);
  return out;
}

/** 칸별로 대조해 다른 것만 돌려준다. */
export function diffValues(want, values) {
  return expectedValues(want)
    .map((v, i) => (String(values[i] ?? '').trim() === String(v).trim() ? null : { index: i, got: values[i] ?? null, want: v }))
    .filter(Boolean);
}

/**
 * 계획대로 검수조서를 올리고 서버에서 다시 읽어 대조한다. 승인된 실행에서만 돈다.
 *
 * `withdraw` 는 상신취소할 옛 문서, `links` 는 관련문서로 연결할 끝난 옛 문서(`{ id, docNum }`),
 * `oldIds` 는 새 문서를 찾을 때 건너뛸 옛 문서 ID 다.
 *
 * 순서: 새 기안 창을 열어 칸을 채우고 첨부, 연결, 결재선까지 마친 뒤에 옛 문서를 상신취소하고
 * 바로 상신한다. 채우다 실패하면 옛 문서는 그대로다. 상신취소는 같은 컨텍스트의 다른 탭에서 해
 * 채운 기안 창을 잃지 않는다.
 * UNVERIFIED: withdrawing from a second tab while a filled draft is open in the first has not been
 * tried on the live site.
 *
 * `onStage(stage, info)` 는 되돌릴 수 없는 단계 직전에 불린다: `'withdrawing'`, `'withdrawn'`
 * (`{ id }`), `'submitting'`. 부르는 쪽이 기록을 남긴다.
 *
 * @returns {Promise<{ docId, status, attachCount, titleOk, diffs, line, withdrawn }>}
 */
export async function fileReport(page, api, baseUrl, plan, { formId, deptId, withdraw = [], links = [], oldIds = [], onStage = () => {} }) {
  requireExecute('검수조서 상신');   // filling the form changes nothing; the withdraw and submit mark the write
  if (!deptId) throw new PluginError('VALIDATION', '기안 부서 ID 를 모릅니다. 계정 정보를 다시 읽어야 합니다.');
  await page.goto(`${baseUrl}/gw/app/approval/document/new/${encodeURIComponent(deptId)}/${encodeURIComponent(formId)}`, { waitUntil: 'domcontentloaded', timeout: 60_000 });
  await page.waitForLoadState('networkidle', { timeout: 30_000 }).catch(() => {});
  await page.waitForSelector('#subject', { timeout: 30_000 });
  await page.waitForTimeout(3000);
  await dismissDialogs(page);
  await fillForm(page, plan);

  for (const f of plan.files) {
    await page.locator('input[type=file][name=file]').first().setInputFiles({ name: f.name, mimeType: f.mimeType, buffer: fs.readFileSync(f.path) });
    await page.waitForFunction((n) => {
      const th = [...document.querySelectorAll('th')].find((t) => t.innerText.trim() === '파일첨부');
      return (th?.nextElementSibling?.innerText ?? '').includes(n);
    }, f.name, { timeout: 120_000, polling: 1000 }).catch(() => {
      throw new PluginError('UPSTREAM', `첨부가 올라가지 않았습니다: ${f.name}`);
    });
  }
  if (links.length) {
    const linked = await linkDocuments(page, links);
    const missing = links.filter((d) => !linked.includes(d.id));
    if (missing.length) throw new PluginError('UPSTREAM', `옛 문서 연결이 빠졌습니다: ${missing.map((d) => d.docNum).join(', ')}`);
  }
  const line = await setApprovalLine(page, plan.approvers);

  const withdrawn = [];
  if (withdraw.length) {
    await onStage('withdrawing', { ids: withdraw });
    const tab = await page.context().newPage();
    try {
      for (const id of withdraw) {
        const st = await withdrawDocument(tab, api, baseUrl, id);
        if (st !== '임시저장') {
          throw new PluginError('UPSTREAM', `옛 검수조서 ${id} 를 상신취소하지 못했습니다 (${st || '상태 모름'}).${withdrawn.length ? ` 이미 상신취소한 문서: ${withdrawn.join(', ')}` : ''}`);
        }
        withdrawn.push(id);
        await onStage('withdrawn', { id });
      }
    } finally {
      await tab.close().catch(() => {});
    }
    await page.bringToFront().catch(() => {});
  }
  // 같은 제목의 문서가 이미 있을 수 있다(다른 결제, 옛 문서). 상신 전에 있던 문서는 새 문서로 보지 않는다.
  const recent = async () => (await api('/gw/api/approval/doclist/draft/all?page=0&offset=20&property=draftedAt&direction=desc&searchtype=&keyword=&duration=all')).data?.data ?? [];
  const seen = new Set([...oldIds.map(String), ...(await recent()).filter((d) => d.title === plan.title).map((d) => String(d.documentId))]);
  await onStage('submitting', { withdrawn });
  await submitDocument(page);

  // 검증: 제목으로 새 문서를 찾고 칸, 첨부, 제목을 읽는다. URL 변화만 믿지 않는다.
  let docId = null;
  for (let i = 0; i < 10 && !docId; i++) {
    docId = (await recent()).find((d) => d.formName === FORM_NAME && d.title === plan.title && d.docStatusName !== '임시저장'
      && !seen.has(String(d.documentId)))?.documentId ?? null;
    if (!docId) await page.waitForTimeout(3000);
  }
  if (!docId) throw new PluginError('UPSTREAM', '상신했지만 내 기안함에서 새 검수조서를 찾지 못했습니다. 다우오피스에서 확인해 주세요.');
  docId = String(docId);
  let { doc, values } = await serverValues(page, api, docId);
  // 읽기가 한 번 비어 돌아온 적이 있다. 한 번 더 읽는다.
  if (!values.length || values.every((v) => v === '' || v == null)) {
    await page.waitForTimeout(3000);
    ({ doc, values } = await serverValues(page, api, docId));
  }
  const diffs = diffValues(plan, values.slice(5));
  return {
    docId,
    status: doc.docStatusName ?? '',
    attachCount: Number(doc.attachCount ?? NaN),
    titleOk: doc.title === plan.title,
    diffs,
    line,
    withdrawn,
  };
}

/** 첨부 파일 경로가 저장소 안에 있는지 본다. 경로를 받은 대로 믿지 않는다. */
export function inside(root, file) {
  const full = path.resolve(root, file);
  return full.startsWith(path.resolve(root) + path.sep) && fs.existsSync(full) ? full : null;
}
