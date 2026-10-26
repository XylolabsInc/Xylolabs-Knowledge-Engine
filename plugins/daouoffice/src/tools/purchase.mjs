import fs from 'node:fs';
import path from 'node:path';
import * as z from 'zod';
import { PluginError } from '../errors.mjs';
import { kstDate, kstIso } from '../dao/kst.mjs';
import { describeLine } from '../dao/approvers.mjs';
import { ingestPhoto } from '../photos.mjs';
import {
  purchaseRules, formValues, validatePurchase, purchaseApprovers, fieldTable, purchaseEntry, purchaseRow,
  RECEIPT_LIKE,
} from '../dao/purchases.mjs';
import { todayRate } from '../dao/fx.mjs';
import { captureQuote, captureContextOptions, storeQuote, checkQuoteFiles, detectBlock } from '../dao/quotes.mjs';
import { planSubmission, submitPurchase } from '../dao/purchase-submit.mjs';
import { describeDiff } from '../dao/purchase-doc.mjs';

/**
 * 구매 품의서(구매 품의). 초안은 플러그인 DB 에만 저장하고(`draft_purchase`, `capture_quote`),
 * 다우오피스에는 사람이 미리보기를 보고 승인한 `submit_purchase` 만 올린다.
 *
 * 초안에는 판(`revision`)이 있다. 초안이나 견적이 바뀔 때마다 하나씩 올라간다. `submit_purchase`
 * 는 판을 받아, 미리보기에서 보여 준 판과 지금 판이 같을 때만 올린다. 미리보기 뒤에 누가 초안을
 * 고쳤으면 승인 버튼은 아무것도 올리지 않는다.
 */

const KEY = /^[0-9A-Za-z가-힣][0-9A-Za-z가-힣._-]{0,63}$/;
const keyArg = z.string().regex(KEY, '영문, 숫자, 한글, 점, 밑줄, 하이픈 64자 이내');
const dateArg = z.string().regex(/^\d{4}-\d{2}-\d{2}$/, 'YYYY-MM-DD');
const won = (n) => (Number.isFinite(Number(n)) && n !== null ? `${Number(n).toLocaleString('ko-KR')}원` : '금액 없음');
const MIME = { '.png': 'image/png', '.jpg': 'image/jpeg', '.jpeg': 'image/jpeg' };

/** 지워진 표시가 없는 견적만. 견적 행은 지우지 않고 `data.dropped` 로 뺀다. */
const activeQuotes = (db, owner, purchaseId) => db.quotes(owner, purchaseId).filter((q) => !q.data?.dropped);

function loadEntry(rt, owner, key) {
  const row = rt.db.purchase(owner, key);
  if (!row) throw new PluginError('NOT_FOUND', `품의 초안 "${key}" 가 없습니다. 먼저 draft_purchase 로 만들거나 list_purchases 로 키를 확인해 주세요.`);
  const quotes = activeQuotes(rt.db, owner, row.id);
  return { row, quotes, entry: purchaseEntry(row, quotes) };
}

/** 엔트리를 저장한다. 판은 부르는 쪽이 정한다. */
function saveEntry(rt, owner, entry) {
  return rt.db.upsertPurchase(purchaseRow(owner, entry));
}

/**
 * 지금 DB 에 있는 행에 `patch` 를 얹어 저장한다. 오래 걸리는 작업(브라우저) 뒤에 옛 사본으로
 * 덮어써 그사이 고친 내용을 잃지 않게 한다. `revision` 을 주면 그 판일 때만 저장한다.
 */
function patchEntry(rt, owner, key, patch, { revision } = {}) {
  const { entry } = loadEntry(rt, owner, key);
  if (revision !== undefined && (Number(entry.revision) || 0) !== revision) {
    throw new PluginError('CONFLICT', '작업하는 사이에 초안이 바뀌었습니다. draft_purchase 나 list_purchases 로 내용을 다시 확인해 주세요.');
  }
  const { quotes: _q, ...rest } = entry;
  return saveEntry(rt, owner, { ...rest, ...patch });
}

function nextKey(db, owner, date) {
  const base = `p-${String(date).replace(/-/g, '')}`;
  for (let i = 1; i < 1000; i++) {
    const key = `${base}-${i}`;
    if (!db.purchase(owner, key)) return key;
  }
  throw new PluginError('INTERNAL', '품의 키를 만들지 못했습니다.');
}

/** 결재선 설명 한 줄. 규칙이 없으면 그 이유. */
function lineText(rt, entry, v) {
  try {
    return { approvers: purchaseApprovers(rt.config, entry, v), text: null };
  } catch (e) {
    if (e instanceof PluginError) return { approvers: null, text: e.message };
    throw e;
  }
}

function issuesText({ errors, warnings }) {
  const lines = [];
  if (errors.length) lines.push('', `상신 전에 고칠 것 ${errors.length}건`, ...errors.map((e) => `- ${e}`));
  if (warnings.length) lines.push('', '주의', ...warnings.map((w) => `- ${w}`));
  return lines;
}

function readImage(dir, file) {
  const data = fs.readFileSync(path.join(dir, file));
  return { data, mimeType: MIME[path.extname(file).toLowerCase()] ?? 'image/png' };
}

// ------------------------------------------------------------------ schemas

const draftInput = z.object({
  key: keyArg.optional().describe('고칠 초안의 키. 새 초안이면 비운다(자동으로 만든다)'),
  title: z.string().max(200).optional().describe('제목. 물건 "… 구매의 건", 구독 "… 구독의 건", 도메인과 라이선스 연장 "… 갱신의 건", 사용료 "… 지출의 건". 여러 가지면 "… 외 N종 구매의 건"'),
  budget: z.string().optional().describe('예산 이름(회사 설정의 budgets 가운데 하나). 카드 뒷자리를 주면 카드 설정에서 채운다'),
  project: z.string().optional().describe('프로젝트(과제) 이름. 사업비 예산일 때 쓴다. 카드 설정에서 채울 수 있다'),
  subProject: z.string().nullable().optional().describe('세부 과제. 지어내지 말고 사람에게 묻는다'),
  item: z.string().max(300).optional().describe('구매품명과 수량. "{품명} / 1개", 여러 가지면 "{품명} 외 2종 / 총 5개". 구독은 "/ 1개월", 계정은 "/ 1계정", 크레딧은 "/ USD 50.00"'),
  qty: z.string().max(20).nullable().optional().describe('구입수량 칸을 따로 정할 때만(예: 8개). 비우면 품명에서 센다'),
  vendor: z.string().max(200).optional().describe('업체명: 판매자 이름, 플랫폼은 괄호(예: 테스트상점 (알리익스프레스)). 네이버페이, 카카오페이 같은 결제수단은 쓰지 않는다'),
  purpose: z.string().max(300).optional().describe('구입목적: 어디에 무엇을 하려고 쓰는지(대상 + 용도) 한 줄. 품명을 되풀이하지 않는다. 사람이 말하지 않았으면 지어내지 말고 묻는다'),
  paidAt: dateArg.nullable().optional().describe('결제일(카드 승인일). 결제했으면 이것이 구입일자다'),
  purchaseDate: dateArg.nullable().optional().describe('결제 전 품의의 구입 예정일'),
  deliveryDate: z.string().nullable().optional().describe('배송일자. 옛 양식 문서를 고칠 때만 필요하다. YYYY-MM-DD 또는 해당없음'),
  amount: z.number().int().positive().nullable().optional().describe('원화 결제액(쿠폰 적용 뒤 실제 결제액). 외화면 비우고 usd 를 쓴다'),
  usd: z.number().positive().nullable().optional().describe('외화(USD) 금액. 결제 전이면 오늘 하나은행 매매기준율로 환산한다'),
  paidKrw: z.number().int().positive().nullable().optional().describe('외화 건을 카드로 결제한 뒤의 카드 승인 원화액. 카드 결제는 이 금액을 쓴다'),
  vat: z.number().int().nonnegative().nullable().optional().describe('영수증에 부가세가 따로 적혀 있을 때만. 비우면 국내 10/110, 해외 0'),
  payWhen: z.string().optional().describe('지급요청일: 당월 또는 익월. 카드면 비워도 당월'),
  payMethod: z.string().optional().describe('지급방법. 카드 뒷자리를 주면 법인카드'),
  cardIssuer: z.string().optional().describe('카드사(양식 선택지). 카드 설정에서 채울 수 있다'),
  cardLast4: z.string().regex(/^\d{4}$/, '네 자리 숫자').optional().describe('법인카드 뒤 네 자리'),
  account: z.string().nullable().optional().describe('지급계좌(은행). 카드가 아닐 때만'),
  accountHolder: z.string().nullable().optional().describe('예금주. 카드가 아닐 때만'),
  note: z.array(z.string().max(300)).max(30).optional().describe('기타 칸 줄(전부 바꾼다). 첫 줄에 상품 링크, 나머지는 구성과 단가, 할인, 배송비, 주문번호를 문장이 아닌 항목으로. 외화 환산식은 자동으로 붙으니 쓰지 않는다'),
  serials: z.array(z.object({ item: z.string(), numbers: z.array(z.string()) })).optional()
    .describe('사람이 알려 준 시리얼 번호. 지어내지 않는다'),
  noSerials: z.boolean().optional().describe('사람이 시리얼 관리 대상이 아니라고 했을 때 true'),
  refreshFx: z.boolean().optional().describe('결제 전 외화 건의 고정 환율을 오늘 값으로 다시 받을 때 true'),
});

const NULLABLE = ['subProject', 'qty', 'paidAt', 'purchaseDate', 'deliveryDate', 'amount', 'usd', 'paidKrw', 'vat', 'account', 'accountHolder'];
const PLAIN = ['title', 'budget', 'project', 'item', 'vendor', 'purpose', 'payWhen', 'payMethod', 'cardIssuer', 'cardLast4', ...NULLABLE];

/** 받은 칸만 덮어쓴다. `null` 은 그 칸을 지운다. 글자는 앞뒤 공백을 지운다. */
export function mergeDraft(existing, args) {
  const next = { ...existing };
  for (const k of PLAIN) {
    if (args[k] === undefined) continue;
    if (args[k] === null) { delete next[k]; continue; }
    next[k] = typeof args[k] === 'string' ? args[k].trim() : args[k];
  }
  if (args.note !== undefined) next.note = args.note.map((l) => String(l).trim()).filter(Boolean);
  if (args.noSerials) next.serials = 'none';
  else if (args.serials) {
    next.serials = Object.fromEntries(args.serials.map((s) => [s.item.trim(), s.numbers.map((x) => String(x).trim()).filter(Boolean)]));
  }
  return next;
}

/** 카드 뒷자리로 회사 설정의 카드(과제, 예산, 카드사)를 채운다. 사람이 준 값이 앞선다. */
export function applyCardDefaults(entry, config) {
  const r = purchaseRules(config);
  const out = { ...entry };
  const card = out.cardLast4 ? config.cards?.[out.cardLast4] : null;
  if (out.cardLast4) out.payMethod ??= r.cardMethod;
  if (card) {
    out.budget ??= card.budget;
    out.project ??= card.project;
    out.cardIssuer ??= card.issuer;
  }
  if (out.payMethod === r.cardMethod) out.payWhen ??= r.payWhen[0];
  return out;
}

// ------------------------------------------------------------------ register

export function register(server, rt) {
  rt.tool(server, 'draft_purchase', {
    kind: 'local',
    description: [
      '구매 품의서(구매 품의) 초안을 만들거나 고친다. 다우오피스에는 아무것도 올리지 않고 플러그인 DB 에만 저장한 뒤, 검증 결과와 양식에 들어갈 칸별 값을 돌려준다.',
      '구입목적과 세부 과제는 절대 지어내지 말고 사람에게 묻는다. 구입목적은 품명을 되풀이하지 않고 대상과 용도를 쓴다(예: "로봇 원격제어 시 실시간 화면 표시용").',
      '카드로 결제한 건은 카드 승인액을 쓴다(외화면 usd 와 paidKrw). 결제 전 외화만 오늘 하나은행 매매기준율로 환산한다.',
      '회사 기준 최소 금액보다 작은 결제는 경고가 나온다. 사람에게 알린다.',
      '고칠 때는 key 와 바꿀 칸만 보낸다. 돌려준 오류는 전부 사람에게 보여 주고 고친다. 다음 단계는 capture_quote(상품 페이지 캡처) 와 submit_purchase(key, revision) 다.',
    ].join(' '),
    input: draftInput,
  }, async (args, call) => {
    const owner = call.owner;
    const existingRow = args.key ? rt.db.purchase(owner, args.key) : null;
    const base = existingRow ? purchaseEntry(existingRow) : {};
    delete base.quotes;
    let next = applyCardDefaults(mergeDraft(base, args), rt.config);
    next.key = existingRow?.key ?? args.key ?? nextKey(rt.db, owner, next.paidAt ?? next.purchaseDate ?? kstDate());
    next.status = existingRow?.status ?? 'draft';

    // 결제 전 외화만 환율을 쓴다. 한 번 받은 값은 초안에 고정한다(미리보기와 상신이 같은 값을 쓴다).
    const notes = [];
    if (next.usd != null && next.paidKrw == null) {
      if (!next.fx || args.refreshFx) {
        try {
          const fx = await todayRate({ db: rt.db, sessions: rt.sessions });
          next.fx = { rate: fx.rate, at: fx.at, source: fx.source };
        } catch (e) {
          notes.push(`환율을 받지 못했습니다: ${e instanceof PluginError ? e.message : '알 수 없는 오류'}`);
        }
      }
    } else {
      delete next.fx;
    }
    // 환율을 받는 사이에 검수조서 쪽(save_photos)이 시리얼을 적었을 수 있다. 사람이 시리얼을 주지
    // 않았으면 지금 DB 의 값을 쓰고, 판도 지금 DB 의 판에서 올린다.
    const current = rt.db.purchase(owner, next.key);
    if (current && args.serials === undefined && !args.noSerials) {
      if (current.data?.serials !== undefined) next.serials = current.data.serials;
      else delete next.serials;
    }
    next.revision = (Number(current?.data?.revision) || 0) + 1;

    const row = saveEntry(rt, owner, next);
    const quotes = activeQuotes(rt.db, owner, row.id);
    const entry = purchaseEntry(row, quotes);
    const v = formValues(entry, { config: rt.config });
    const result = validatePurchase(entry, { config: rt.config });
    const line = lineText(rt, entry, v);

    const head = `구매 품의 초안 ${entry.key} (판 ${entry.revision})${entry.status === 'submitted' ? ` · 이미 올린 문서 ${entry.docId ?? '(ID 모름)'}, 고친 내용은 submit_purchase 의 fix 로 다시 올린다` : ''}`;
    const lines = [head, ...fieldTable(v)];
    lines.push(`- 결재선: ${line.approvers ? describeLine(line.approvers) : line.text}`);
    if (entry.fx) lines.push(`- 환율: ${Number(entry.fx.rate).toLocaleString('ko-KR')}원 (${entry.fx.source}, ${entry.fx.at})`);
    lines.push(...notes.map((n) => `- ${n}`));
    lines.push(...issuesText(result));
    lines.push('', quotes.length
      ? `견적 캡처 ${quotes.length}장. 상신하려면 submit_purchase(key "${entry.key}", revision ${entry.revision}) 를 부른다.`
      : `다음: capture_quote 로 상품 페이지를 찍는다(key "${entry.key}"). 캡처 없이는 올리지 않는다.`);
    return {
      text: lines.join('\n'),
      data: {
        key: entry.key, revision: entry.revision, status: entry.status, docId: entry.docId ?? null,
        errors: result.errors, warnings: result.warnings, fields: v,
        approvers: line.approvers, quotes: quotes.map((q) => q.file),
      },
    };
  });

  rt.tool(server, 'list_purchases', {
    kind: 'read',
    description: '저장된 구매 품의 초안과 올린 품의(키, 상태, 날짜, 제목, 금액, 문서 ID, 견적 수, 판)를 본다. 사람이 품의 목록이나 상태를 묻거나, submit_purchase 에 넘길 키와 판을 찾을 때 쓴다. 다우오피스의 실제 결재 상태는 read_document 로 본다.',
    input: z.object({
      status: z.enum(['draft', 'submitted']).optional().describe('draft 초안, submitted 올린 것'),
      from: dateArg.optional().describe('구입일자가 이날부터'),
      to: dateArg.optional().describe('구입일자가 이날까지'),
      key: keyArg.optional().describe('이 키 하나만 자세히'),
      query: z.string().optional().describe('제목, 품명, 업체명에 이 말이 든 것만'),
      limit: z.number().int().min(1).max(100).optional().describe('최대 건수, 기본 30'),
    }),
  }, async ({ status, from, to, key, query, limit = 30 }, call) => {
    if (key) {
      const { entry, quotes } = loadEntry(rt, call.owner, key);
      const v = formValues(entry, { config: rt.config });
      const result = validatePurchase(entry, { config: rt.config });
      const lines = [
        `${entry.key} · ${entry.status === 'submitted' ? `올림 (문서 ${entry.docId ?? 'ID 모름'})` : '초안'} · 판 ${entry.revision ?? 0}`,
        ...fieldTable(v),
        ...(entry.refiledFrom ? [`- 다시 올리기 전 옛 문서: ${entry.refiledFrom}`] : []),
        ...(entry.hold ? [`- 보류: ${entry.hold}`] : []),
        ...issuesText(result),
      ];
      return { text: lines.join('\n'), data: { entry: { ...entry, quotes: quotes.map((q) => q.file) }, fields: v, ...result } };
    }
    let rows = rt.db.purchases(call.owner, status ? { status } : {}).map((r) => purchaseEntry(r));
    const dayOf = (e) => e.paidAt ?? e.purchaseDate ?? '';
    if (from) rows = rows.filter((e) => dayOf(e) >= from);
    if (to) rows = rows.filter((e) => dayOf(e) && dayOf(e) <= to);
    if (query) rows = rows.filter((e) => `${e.title} ${e.item ?? ''} ${e.vendor ?? ''}`.includes(query));
    rows.reverse();
    const shown = rows.slice(0, limit);
    if (!shown.length) return { text: '조건에 맞는 구매 품의가 없습니다.', data: { purchases: [] } };
    const lines = [`구매 품의 ${rows.length}건${rows.length > shown.length ? ` (최근 ${shown.length}건)` : ''}`];
    const out = [];
    for (const e of shown) {
      const row = rt.db.purchase(call.owner, e.key);
      const n = activeQuotes(rt.db, call.owner, row.id).length;
      const amount = formValues(e, { config: rt.config }).amountNumber;
      lines.push(`- ${e.key} · ${e.status === 'submitted' ? '올림' : '초안'} · ${dayOf(e) || '날짜 없음'} · ${e.title || '(제목 없음)'} · ${won(amount)}`
        + `${e.docId ? ` · 문서 ${e.docId}` : ''} · 견적 ${n}장 · 판 ${e.revision ?? 0}${e.hold ? ` · 보류: ${e.hold}` : ''}`);
      out.push({ key: e.key, status: e.status, date: dayOf(e) || null, title: e.title, amount: Number.isFinite(amount) ? amount : null, docId: e.docId ?? null, quotes: n, revision: e.revision ?? 0 });
    }
    return { text: lines.join('\n'), data: { purchases: out } };
  });

  rt.tool(server, 'capture_quote', {
    kind: 'local',
    description: [
      '구매 품의서에 붙일 상품 페이지 스크린샷(견적서 대용)을 찍어 초안에 붙이고, 찍은 사진을 돌려준다. 다우오피스에는 올리지 않는다.',
      'url 은 주문한 판매자의 상품 페이지다. expect 에는 페이지에 반드시 보여야 할 판매자 이름이나 상품명을 넣고, options 에는 주문한 옵션 버튼 글자를 넣는다.',
      '영수증, 주문내역 페이지는 붙이지 않는다. 쿠팡, 네이버는 자동 캡처를 자주 막는다. 막히면 제조사 상품 페이지나 다나와 검색 페이지를 찍거나, 사람이 찍은 화면을 첨부로 받아 url 없이 부른다.',
      '찍은 사진은 사람에게 보여 준다. 판매자, 상품, 옵션, 가격이 주문과 다르면 다시 찍는다. drop 으로 잘못 찍은 견적을 뺀다.',
    ].join(' '),
    input: z.object({
      key: keyArg.describe('품의 초안 키'),
      url: z.url({ protocol: /^https?$/ }).optional().describe('상품 페이지 주소'),
      expect: z.array(z.string().min(1)).max(10).optional().describe('페이지에 있어야 할 글자(판매자, 상품명). url 이 있으면 필수'),
      options: z.array(z.string().min(1)).max(10).optional().describe('주문한 옵션 버튼 글자. 기본 옵션이 찍히지 않게 고른다'),
      name: z.string().max(60).optional().describe('파일 이름 앞부분(예: 테스트상점_카메라). 영수증, 주문 같은 말은 쓰지 않는다'),
      waitMs: z.number().int().min(1000).max(30_000).optional().describe('페이지가 늦게 뜨는 사이트에서 기다릴 시간(ms). 기본 5000, 알리는 20000 안팎'),
      noClose: z.boolean().optional().describe('팝업 닫기가 오히려 무언가를 여는 사이트에서 true'),
      hideText: z.array(z.string()).max(5).optional().describe('닫히지 않는 떠 있는 안내 상자를 그 안의 짧은 글자로 숨긴다'),
      decline: z.string().optional().describe('찍기 전에 누를 거절 버튼 글자'),
      aliRegion: z.enum(['KR', 'GLOBAL']).optional().describe('알리익스프레스 표시. 기본 GLOBAL(영어, 달러, 한국 배송)'),
      drop: z.array(z.string()).max(10).optional().describe('초안에서 뺄 견적 파일 이름'),
    }),
  }, async (args, call) => {
    const owner = call.owner;
    const { row, entry, quotes } = loadEntry(rt, owner, args.key);
    const dir = rt.filesDir(owner, 'quotes');
    const lines = [];
    const images = [];
    let changed = false;

    for (const file of args.drop ?? []) {
      const q = quotes.find((x) => x.file === file);
      if (!q) throw new PluginError('NOT_FOUND', `이 초안에 그런 견적이 없습니다: ${file}`);
      rt.db.addQuote({ ...q, owner, data: { ...q.data, dropped: true, droppedAt: kstIso() } });
      lines.push(`견적을 뺐습니다: ${file}`);
      changed = true;
    }

    const added = [];
    if (args.url) {
      if (!(args.expect ?? []).length) {
        throw new PluginError('VALIDATION', 'expect 가 없습니다. 주문한 판매자나 상품명처럼 페이지에 반드시 있어야 할 글자를 넣어 주세요.');
      }
      // 주문, 영수증 페이지는 열지도 않는다.
      const pre = detectBlock({ url: args.url });
      if (pre) throw new PluginError('VALIDATION', `캡처하지 않았습니다: ${pre.reason}`, { data: { kind: pre.kind } });
      const quote = {
        url: args.url, expect: args.expect, options: args.options ?? [], waitMs: args.waitMs,
        noClose: args.noClose, hideText: args.hideText, decline: args.decline, aliRegion: args.aliRegion === 'KR' ? 'KR' : undefined,
      };
      // 공유 브라우저의 빈 컨텍스트에서 찍는다. 다우오피스 쿠키도 로그인도 없다.
      const res = await rt.sessions.withScratchContext(({ page }) => captureQuote(page, quote), { contextOptions: captureContextOptions });
      if (!res.ok) {
        throw new PluginError('VALIDATION', `캡처하지 못했습니다: ${res.reason}`, { data: { kind: res.kind, landedUrl: res.landedUrl ?? null } });
      }
      const q = storeQuote({
        db: rt.db, owner, dir, purchaseId: row.id, buffer: res.shot, label: args.name, url: args.url, ext: 'png',
        data: { landedUrl: res.landedUrl, title: res.title, expect: args.expect, options: args.options ?? [], how: 'headless', priceSeen: res.priceSeen },
      });
      added.push(q);
    } else {
      const imgs = call.attachments.filter((a) => /^image\//.test(a.mimeType) || /\.(png|jpe?g|heic|heif|webp)$/i.test(a.name));
      if (!imgs.length && !args.drop?.length) {
        throw new PluginError('VALIDATION', 'url 이나 사람이 찍은 상품 페이지 화면(이미지 첨부)이 필요합니다.');
      }
      for (const a of imgs) {
        if (RECEIPT_LIKE.test(a.name)) {
          throw new PluginError('VALIDATION', `영수증이나 주문내역으로 보이는 파일은 견적으로 받지 않습니다: ${a.name}`);
        }
        // 메타데이터를 지우고 JPEG 로 바꾼다. 휴대폰 화면 캡처에도 위치 정보가 남을 수 있다.
        const tmp = path.join(dir, '.incoming');
        const ing = await ingestPhoto(a.path, tmp);
        const buffer = fs.readFileSync(ing.path);
        fs.rmSync(ing.path, { force: true });
        const label = args.name ?? path.parse(a.name).name;
        added.push(storeQuote({
          db: rt.db, owner, dir, purchaseId: row.id, buffer, label, url: null, ext: 'jpg',
          data: { manual: true, how: '사람이 찍은 화면' },
        }));
      }
    }
    for (const q of added) {
      lines.push(`견적을 붙였습니다: ${q.file}${q.url ? ` (${q.url})` : ' (사람이 찍은 화면)'}`);
      images.push(readImage(dir, q.file));
      changed = true;
    }

    let revision = Number(entry.revision) || 0;
    if (changed) {
      // 견적이 바뀌면 붙일 파일이 바뀐 것이다. 판을 올려 이전 미리보기의 승인이 통하지 않게 한다.
      const current = Number(loadEntry(rt, owner, entry.key).entry.revision) || 0;
      revision = current + 1;
      patchEntry(rt, owner, entry.key, { revision });
    }
    const now = activeQuotes(rt.db, owner, row.id);
    lines.push(`초안 ${entry.key} 견적 ${now.length}장 · 판 ${revision}`);
    if (added.length) lines.push('사진을 열어 판매자, 상품명, 고른 옵션, 가격, 통화, 가리는 팝업을 확인해 주세요. 주문과 다르면 다시 찍습니다.');
    return {
      text: lines.join('\n'),
      images,
      data: { key: entry.key, revision, quotes: now.map((q) => ({ file: q.file, url: q.url, digest: q.digest, manual: Boolean(q.data?.manual) })) },
    };
  });

  /** 미리보기와 실행이 똑같이 하는 검사. 다우오피스는 부르지 않는다. */
  function prepare(args, call) {
    if (args.fix && args.refile) throw new PluginError('VALIDATION', 'fix 와 refile 은 함께 쓰지 않습니다.');
    const option = args.fix ? 'fix' : args.refile ? 'refile' : null;
    const { row, entry, quotes } = loadEntry(rt, call.owner, args.key);
    const revision = Number(entry.revision) || 0;
    if (revision !== args.revision) {
      throw new PluginError('CONFLICT', `초안 판이 다릅니다: 지금 판 ${revision}, 요청한 판 ${args.revision}. draft_purchase 나 list_purchases 로 내용을 다시 확인해 주세요.`);
    }
    if (!option && entry.status === 'submitted') {
      throw new PluginError('CONFLICT', `이미 올린 품의입니다 (문서 ${entry.docId ?? 'ID 모름'}). 고치려면 fix, 최종 결재자가 빠진 옛 문서를 새 결재선으로 다시 올리려면 refile 을 씁니다.`);
    }
    const rules = purchaseRules(rt.config);
    const checked = option === 'refile' ? { ...entry, docId: undefined } : entry;
    const result = validatePurchase(checked, { config: rt.config });
    if (result.errors.length) {
      throw new PluginError('VALIDATION', [`상신할 수 없습니다. 고칠 것 ${result.errors.length}건`, ...result.errors.map((e) => `- ${e}`)].join('\n'),
        { data: { errors: result.errors, warnings: result.warnings } });
    }
    const dir = rt.filesDir(call.owner, 'quotes');
    const problems = checkQuoteFiles(dir, quotes);
    if (problems.length) throw new PluginError('VALIDATION', `${problems.join('\n')}\ncapture_quote 로 다시 찍어 주세요.`);
    const want = formValues(entry, { config: rt.config });
    const approvers = purchaseApprovers(rt.config, entry, want);
    if (!approvers.length) throw new PluginError('VALIDATION', '구매 품의서 결재선이 비어 있습니다. 회사 설정 lines.purchase 를 확인해 주세요.');
    return { row, entry, quotes, option, rules, want, approvers, revision, dir, warnings: result.warnings };
  }

  const submitInput = z.object({
    key: keyArg.describe('품의 초안 키'),
    revision: z.number().int().min(0).describe('초안 판. draft_purchase, capture_quote, list_purchases 가 알려 준다'),
    fix: z.boolean().optional().describe('이미 올린 품의를 고칠 때 true. 진행중이면 상신취소하고 같은 문서로 다시 올린다'),
    refile: z.boolean().optional().describe('최종 결재자가 빠진 옛 품의를 새 결재선으로 다시 올릴 때 true'),
  });

  rt.tool(server, 'submit_purchase', {
    kind: 'write',
    description: [
      '저장된 구매 품의 초안을 다우오피스 구매 품의서로 상신한다. 이 도구는 미리보기(칸별 값, 결재선, 견적 캡처)만 만들고, 사람이 승인 버튼을 누르거나 "승인" 이라고 답해야 실제로 올라간다. 사람이 승인했다고 대신 말하지 않는다.',
      '견적 캡처가 없거나 검증 오류가 있으면 올리지 않는다. revision 은 draft_purchase 나 list_purchases 가 알려 준 판이다.',
      '이미 올린 품의를 고치면 fix, 최종 결재자가 빠진 옛 문서를 새 결재선으로 다시 올리면 refile 을 true 로 준다.',
    ].join(' '),
    input: submitInput,
  }, {
    async preview(args, call) {
      const p = prepare(args, call);
      const needDoc = Boolean(p.option || p.entry.docId);
      let account = rt.db.account(call.owner);
      let plan = null;
      if (needDoc || !account?.deptId) {
        plan = await rt.session(call, async ({ page, api }) => (needDoc
          ? planSubmission({ page, api, entry: p.entry, option: p.option, formName: p.rules.formName, approvers: p.approvers })
          : null));
        account = rt.db.account(call.owner);
      }
      if (!plan?.editId && !account?.deptId) {
        throw new PluginError('VALIDATION', '기안 부서를 모릅니다. 조직도에서 부서를 읽지 못했으면 관리자가 회사 설정 departments 에 넣어야 합니다.');
      }
      if (plan && !plan.newForm && !p.want.deliveryDate) {
        throw new PluginError('VALIDATION', '옛 양식 문서라 배송일자가 필요합니다. draft_purchase 로 deliveryDate 를 넣어 주세요.');
      }

      // 미리보기에서 보여 준 판과 견적을 남긴다. 실행은 이것과 같을 때만 올린다.
      patchEntry(rt, call.owner, p.entry.key, {
        previewedRevision: p.revision,
        previewDigests: Object.fromEntries(p.quotes.map((q) => [q.file, q.digest])),
        previewedAt: kstIso(),
      }, { revision: p.revision });

      const head = p.option === 'fix' ? '올린 구매 품의서를 고쳐 다시 올립니다.'
        : p.option === 'refile' ? '구매 품의서를 새 결재선으로 다시 올립니다.'
          : '다음 구매 품의서를 상신합니다.';
      const drafter = rt.accountName(account, call.owner);
      const lines = [
        `${head} (초안 ${p.entry.key}, 판 ${p.revision})`,
        ...fieldTable(p.want),
        `- 결재선: 기안 ${drafter} → ${describeLine(p.approvers)}`,
        ...(plan?.steps ?? ['새 문서로 기안합니다.']).map((s) => `- ${s}`),
        ...issuesText({ errors: [], warnings: p.warnings }),
        '',
        `견적 캡처 ${p.quotes.length}장을 붙였습니다. 상품, 판매자, 옵션, 가격이 주문과 맞는지 보고 승인해 주세요. 승인하면 결재선 전원에게 알림이 갑니다.`,
      ];
      return {
        text: lines.join('\n'),
        images: p.quotes.map((q) => readImage(p.dir, q.file)),
        data: {
          key: p.entry.key, revision: p.revision, option: p.option,
          fields: p.want, approvers: p.approvers,
          plan: plan ? { editId: plan.editId, withdraw: plan.withdraw, links: plan.links, refileFrom: plan.refileFrom, newForm: plan.newForm } : { editId: null },
          warnings: p.warnings,
        },
      };
    },

    async execute(args, call) {
      const p = prepare(args, call);
      if (p.entry.previewedRevision !== p.revision) {
        throw new PluginError('CONFLICT', '이 판의 미리보기를 보여 준 기록이 없습니다. 미리보기를 다시 만들어 확인받아 주세요.');
      }
      const changed = checkQuoteFiles(p.dir, p.quotes, { previewed: p.entry.previewDigests ?? {} });
      if (changed.length || Object.keys(p.entry.previewDigests ?? {}).length !== p.quotes.length) {
        throw new PluginError('CONFLICT', `미리보기 뒤에 견적이 바뀌었습니다. 미리보기를 다시 만들어 확인받아 주세요.${changed.length ? `\n${changed.join('\n')}` : ''}`);
      }
      const owner = call.owner;
      let submittedAt = null;
      const mark = (patch) => patchEntry(rt, owner, p.entry.key, patch);

      const out = await rt.session(call, async ({ page, api, baseUrl }) => {
        const account = rt.db.account(owner);
        const plan = await planSubmission({ page, api, entry: p.entry, option: p.option, formName: p.rules.formName, approvers: p.approvers });
        const r = await submitPurchase({
          page, api, baseUrl, deptId: account?.deptId, want: p.want, approvers: p.approvers, plan,
          quoteDir: p.dir, formName: p.rules.formName, projectBudget: p.rules.projectBudget, cardMethod: p.rules.cardMethod,
          logger: rt.logger,
          // 결재요청을 누른 순간부터는 올린 것으로 적는다. 뒤에서 실패해도 같은 품의를 다시 올리지 않게 한다.
          onSubmitted: ({ docId }) => {
            submittedAt ??= kstIso();
            mark({
              status: 'submitted', ...(docId ? { docId } : {}), submittedAt,
              ...(p.option === 'refile' ? { refiledFrom: plan.refileFrom } : {}),
            });
          },
        });
        return { ...r, plan };
      });

      const now = kstIso();
      mark({ status: 'submitted', docId: out.docId, filedAt: now, filedRevision: p.revision, hold: undefined });
      for (const q of p.quotes) rt.db.addQuote({ ...q, owner, seenAt: q.seenAt ?? now });
      const mismatches = out.diffs.map(describeDiff);
      rt.db.addFiling({
        owner, kind: 'purchase', ref: p.entry.key, docId: out.docId, title: p.want.title, status: out.status,
        detail: {
          option: p.option, revision: p.revision, withdrew: out.withdrew, mismatches,
          ...(p.option === 'refile' ? { refiledFrom: out.plan.refileFrom } : {}),
        },
      });

      if (mismatches.length) {
        throw new PluginError('UPSTREAM',
          [`상신은 됐지만 서버 문서가 기대와 다릅니다 (문서 ID ${out.docId}, ${out.status || '상태 모름'}). 다우오피스에서 확인해 주세요.`, ...mismatches.map((m) => `- ${m}`)].join('\n'),
          { data: { docId: out.docId, status: out.status, verified: false, mismatches } });
      }
      const lines = [
        `상신했습니다: ${p.want.title} (문서 ID ${out.docId}, ${out.status}).`,
        `- 결재선: ${out.line}`,
        ...(out.plan.refileFrom ? [`- 옛 문서 ${out.plan.refileFrom}${out.plan.links.length ? ' 를 관련문서로 연결했습니다' : ' 에서 다시 올렸습니다'}`] : []),
        ...(out.withdrew ? ['- 상신취소한 뒤 같은 문서로 다시 올렸습니다.'] : []),
        '서버 문서를 칸별로 대조했고 모두 맞습니다.',
      ];
      return {
        text: lines.join('\n'),
        images: out.review ? [{ data: out.review, mimeType: 'image/jpeg' }] : [],
        data: { key: p.entry.key, docId: out.docId, status: out.status, verified: true, refiledFrom: out.plan.refileFrom ?? null, withdrew: out.withdrew },
      };
    },
  });
}
