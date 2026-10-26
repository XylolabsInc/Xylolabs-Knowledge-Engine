import { createHash } from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';
import * as z from 'zod';
import { PluginError } from '../errors.mjs';
import { digestOf } from '../runtime.mjs';
import { kstDate, addDays } from '../dao/kst.mjs';
import { listCards, listPurposes, listUsage, pickCard, last4Of, cardHolder } from '../dao/cards.mjs';
import { matchCard } from '../dao/card-match.mjs';
import { claimLine, describeLine } from '../dao/approvers.mjs';
import { listDrafts } from '../dao/documents.mjs';
import { parsePurposeMap, effectivePurposeName, resolvePurpose } from '../dao/expense-map.mjs';
import { tripsForExpenses, TRIP_EXPENSE_KIND } from '../dao/trip-expenses.mjs';
import {
  claimMonth, assertMonthOver, claimTitle, claimRef, sortClaimRows, planClaim, groupSummary,
  claimStatement, statementLine, linkCandidates, decideLinks, opinionText, savedNotes, isLiveStatus,
  savePurposes, createClaimDocument, attachAndSubmit, verifyClaim, claimLedger, claimMismatches, CLAIM_NOTE_KIND, EDGE_DAYS,
} from '../dao/expense-claim.mjs';

/**
 * `file_expense_claim` (write): 법인카드 한 장, 한 달, 사용용도 하나의 지출결의.
 *
 * preview  라이브 카드 내역을 영수증, 구매 품의, 출장, 메모와 짝짓고, 사용용도별로 나누고, 제목,
 *          명세서 대조, 결재선, 연결할 문서를 보여 준다. 다우오피스를 바꾸지 않는다(사용용도 저장도
 *          서버를 바꾸므로 실행에서만 한다).
 * execute  같은 검사를 다시 하고, 사용용도를 저장하고, 문서를 만들고, 영수증을 붙이고, 문서를
 *          연결하고, 결재선을 넣어 상신한 뒤 서버 문서를 다시 읽어 대조한다.
 */

const won = (n) => `${Number(n).toLocaleString('ko-KR')}원`;
const dayOf = (row) => String(row.useDate).slice(0, 10);
const tagOf = (row) => `${dayOf(row)} ${String(row.merchant ?? '').trim()} ${won(row.amount)}`;
const MAX_LINES = 40;
const VERIFY_MS = 90_000;

const noteArg = z.object({
  date: z.string().regex(/^\d{4}-\d{2}-\d{2}$/, 'YYYY-MM-DD').describe('결제일 YYYY-MM-DD (카드 내역의 날짜)'),
  amount: z.number().int().describe('결제 금액(원)'),
  merchant: z.string().trim().min(1).max(80).optional().describe('가맹점 이름 일부. 같은 날 같은 금액 결제가 둘 이상일 때 넣는다'),
  detail: z.string().trim().min(1).max(80).optional().describe('상세 사용용도. 사람이 알려 준 용도를 명사구로 (예: "테스트시 출장 주차료")'),
  purpose: z.string().trim().min(1).max(40).optional().describe('사용용도 이름 (예: 소모품비, 주차료)'),
  exclude: z.boolean().optional().describe('이 결제를 지출결의에서 뺀다 (개인 결제, 다른 사람이 청구하는 결제 등)'),
  reason: z.string().trim().min(1).max(80).optional().describe('뺀 이유'),
});

const receiptPath = (rt, owner, file) => (path.isAbsolute(String(file)) ? String(file) : path.join(rt.filesDir(owner, 'receipts'), String(file)));

/** 결재 문서 하나의 상태와 문서번호. 없으면 status 가 빈 문자열이다. */
async function docState(api, docId) {
  const r = await api(`/gw/api/approval/document/${encodeURIComponent(docId)}`);
  const doc = r.ok ? (r.data?.data?.document ?? {}) : {};
  return { status: doc.docStatusName ?? '', docNum: doc.docNum ?? doc.documentNumber ?? '', title: doc.title ?? '' };
}

const list = (lines, max = MAX_LINES) => (lines.length > max ? [...lines.slice(0, max), `(외 ${lines.length - max}건)`] : lines);

/**
 * 내 기안 문서를 최신순으로 `since`(YYYY-MM-DD)보다 앞에 기안한 문서가 나올 때까지 읽는다(100건씩,
 * 최대 `maxPages` 쪽). 한 달의 지출결의는 그 달 1일 이후에 기안하므로 그 앞은 볼 필요가 없다.
 */
async function draftsSince(api, since, maxPages = 20) {
  const out = [];
  for (let page = 0; page < maxPages; page++) {
    const rows = await listDrafts(api, { page, offset: 100 });
    out.push(...rows);
    const oldest = rows.map((d) => String(d.draftedAt).slice(0, 10)).filter(Boolean).sort()[0];
    if (rows.length < 100 || (oldest && oldest < since)) break;
  }
  return out;
}

/** 끝나지 않은 시도의 기록 상태. 그 문서가 임시저장에 남아 있으면 카드 행이 거기 묶였을 수 있다. */
const UNFINISHED = new Set(['작성중', '상신 실패', '확인 못 함', '생성 중 멈춤']);

/**
 * 달 앞뒤 `EDGE_DAYS` 일의 카드 대장. 달을 넘긴 취소 짝을 찾는 데만 쓴다. 짝짓기(영수증, 품의)에는
 * 넣지 않아 이 달 행의 영수증을 빼앗지 않는다. 뒤쪽은 오늘까지만 본다.
 */
async function edgeRows(api, cardId, m, today) {
  const late = addDays(m.to, EDGE_DAYS);
  const windows = [[addDays(m.from, -EDGE_DAYS), addDays(m.from, -1)], [addDays(m.to, 1), late < today ? late : today]];
  const out = [];
  for (const [from, to] of windows) if (from <= to) out.push(...(await listUsage(api, { from, to, cardId })).rows);
  return out;
}

/**
 * 미리보기와 실행이 함께 쓰는 계획. 막히면 VALIDATION 이나 CONFLICT 를 던진다. 다우오피스는
 * 읽기만 한다(카드, 사용내역, 사용용도 목록, 문서 상태, 내 기안 문서).
 */
async function buildClaim(rt, call, { api }, args, today = kstDate()) {
  const m = claimMonth(args.month, today);
  assertMonthOver(m, today);
  const owner = call.owner;
  const map = parsePurposeMap(rt.config.purposeMap);

  const card = pickCard(await listCards(api), args.card);
  const last4 = last4Of(card) || args.card;
  const holder = cardHolder(card);
  const account = rt.db.account(owner);
  const purposes = await listPurposes(api);
  if (!purposes.length) throw new PluginError('UPSTREAM', '다우오피스 사용용도 목록을 읽지 못했습니다.');

  const overrides = rt.db.overrides(owner);
  const matched = await matchCard({
    api, db: rt.db, owner, card, from: m.from, to: m.to, vendorAliases: rt.config.cardMatch?.vendorAliases ?? {},
  });
  const edge = await edgeRows(api, card.id, m, today);

  const notes = [...(args.notes ?? []), ...savedNotes(overrides)];
  const sorted = sortClaimRows({
    audited: matched.audited, ledger: matched.rows, cancelled: matched.cancelled, overrides, edge,
    from: m.from, to: m.to, receipts: rt.db.receipts(owner),
    trips: tripsForExpenses(rt.db.trips(owner)), confirmed: overrides.filter((o) => o.kind === TRIP_EXPENSE_KIND),
    notes, cardLast4: last4,
  });

  if (sorted.unexplained.length) {
    throw new PluginError('VALIDATION', [
      `${m.year}년 ${m.month}월 ${card.name} 카드에 설명되지 않은 결제가 ${sorted.unexplained.length}건 있습니다. 명세서와 금액을 맞춰야 해서 그대로 올리지 않습니다.`,
      ...list(sorted.unexplained.map((u) => `- ${tagOf(u.row)}: ${u.reason}`)),
      '각 결제가 무엇이었는지 사람에게 묻고 notes 로 알려 주세요: 넣을 결제는 detail(상세 사용용도)과 purpose(사용용도), 뺄 결제는 exclude. 지어내지 않습니다.',
    ].join('\n'), { unexplained: sorted.unexplained.map((u) => ({ date: dayOf(u.row), merchant: u.row.merchant, amount: u.row.amount, reason: u.reason })) });
  }

  const planned = planClaim({ items: sorted.items, purposes, map });
  const groups = groupSummary(planned.groups);
  const groupText = groups.map((g) => `${g.purpose} ${g.count}건 ${won(g.amount)}`).join(', ') || '(없음)';
  if (!args.purpose) {
    throw new PluginError('VALIDATION', `어느 사용용도를 올릴지 정해 주세요. 지출결의는 사용용도마다 문서 하나입니다. ${m.year}년 ${m.month}월에 아직 안 올린 것: ${groupText}`, { groups });
  }
  const purpose = planned.groups.has(args.purpose) ? args.purpose : effectivePurposeName(purposes, args.purpose, map);

  const blocking = planned.problems.filter((p) => !p.purpose || p.purpose === purpose);
  if (blocking.length) {
    throw new PluginError('VALIDATION', ['올리기 전에 고칠 것이 있습니다.', ...list(blocking.map((p) => `- ${p.text}`))].join('\n'));
  }

  const ref = claimRef(last4, m.ym, purpose);
  const deptCode = account?.deptCode ?? null;
  const title = claimTitle({ ...m, cardLast4: last4, deptCode, holder, purpose });
  const baseTitle = claimTitle({ ...m, cardLast4: last4, deptCode, holder });

  // 같은 카드, 같은 달, 같은 사용용도에 살아 있는 문서가 있으면 두 번째 문서다. 만들지 않는다.
  const existing = [];
  const seen = new Set();
  for (const d of await draftsSince(api, m.from)) {
    if ((d.title === title || d.title === baseTitle) && isLiveStatus(d.status)) {
      existing.push({ docId: d.id, title: d.title, status: d.status });
      seen.add(d.id);
    }
  }
  const claimedHere = sorted.claimed.filter((a) => a.row.docId && a.row.purpose === purpose).map((a) => String(a.row.docId));
  const filings = rt.db.filings(owner, { kind: 'claim', ref }).filter((f) => f.docId);
  const unfinished = new Set(filings.filter((f) => UNFINISHED.has(f.status)).map((f) => String(f.docId)));
  const stuck = [];
  for (const docId of new Set([...claimedHere, ...filings.map((f) => String(f.docId))])) {
    if (seen.has(docId)) continue;
    const st = await docState(api, docId);
    if (isLiveStatus(st.status)) existing.push({ docId, title: st.title, status: st.status });
    // 앞선 시도가 문서를 만들고 멈추면 카드 행이 임시저장 문서에 묶여 남는다. 그대로 두면 "올릴 결제가
    // 없다" 고 보이므로 그 문서를 알려 주고 멈춘다.
    else if (st.status === '임시저장' && (claimedHere.includes(docId) || unfinished.has(docId))) stuck.push({ docId, title: st.title });
  }
  if (stuck.length) {
    throw new PluginError('CONFLICT', [
      `앞선 ${purpose} 지출결의 시도가 임시저장 문서에서 멈췄습니다: ${stuck.map((d) => `${d.title || '(제목 없음)'} (문서 ID ${d.docId})`).join(', ')}.`,
      '카드 행이 그 문서에 묶여 있을 수 있습니다. 다우오피스에서 그 문서를 열어 확인하고, 지우거나 상신한 뒤 다시 요청해 주세요.',
    ].join(' '), { stuck });
  }

  const rows = [...(planned.groups.get(purpose) ?? [])].sort((a, b) => String(a.row.useDate).localeCompare(String(b.row.useDate)));
  if (!rows.length) {
    if (existing.length) {
      throw new PluginError('CONFLICT', `${m.year}년 ${m.month}월 ${purpose} 지출결의는 이미 올라가 있습니다: ${existing.map((e) => `${e.title || '(제목 없음)'} (${e.status}, 문서 ID ${e.docId})`).join(', ')}`, { existing });
    }
    throw new PluginError('VALIDATION', `${m.year}년 ${m.month}월 ${card.name} 카드에 ${purpose}로 올릴 결제가 없습니다. 아직 안 올린 것: ${groupText}`, { groups });
  }
  if (existing.length) {
    throw new PluginError('CONFLICT', [
      `${m.year}년 ${m.month}월 ${purpose} 지출결의가 이미 있습니다: ${existing.map((e) => `${e.title || '(제목 없음)'} (${e.status}, 문서 ID ${e.docId})`).join(', ')}.`,
      `남은 ${rows.length}건(${won(rows.reduce((t, r) => t + r.row.amount, 0))})을 두 번째 문서로 올리지 않습니다. 같은 달 두 번째 문서는 명세서 대조를 깨뜨립니다. 회계에 처리 방법을 물어 주세요.`,
    ].join(' '), { existing });
  }
  const purposeCode = resolvePurpose(purposes, purpose, map);

  const files = [...new Set(rows.flatMap((r) => r.receipts.map((x) => receiptPath(rt, owner, x.file))))];
  const lost = files.filter((f) => !fs.existsSync(f));
  if (lost.length) {
    throw new PluginError('VALIDATION', `영수증 파일이 서버에 없습니다: ${lost.map((f) => path.basename(f)).join(', ')}. 영수증을 다시 올려 주세요.`);
  }
  // 미리보기와 실행이 같은 파일을 붙이는지 내용으로 맞춘다. 이름이 같아도 바뀐 파일은 다른 것이다.
  const fileHashes = files.map((f) => ({ name: path.basename(f), sha256: createHash('sha256').update(fs.readFileSync(f)).digest('hex') }));

  const cands = linkCandidates(rows, { inspections: rt.db.filings(owner, { kind: 'inspection' }) });
  for (const c of cands) {
    const st = await docState(api, c.docId);
    Object.assign(c, { status: st.status, docNum: st.docNum, done: st.status === '완료' && Boolean(st.docNum) });
  }
  const linkPlan = decideLinks(cands, { linkDone: args.linkDone, withoutLinks: args.withoutLinks });
  if (linkPlan.blocked.length) {
    throw new PluginError('VALIDATION', [
      `결재가 끝나지 않은 문서 ${linkPlan.blocked.length}건은 관련문서로 연결할 수 없습니다(결재가 끝난 문서만 고를 수 있습니다).`,
      ...list(linkPlan.blocked.map((c) => `- ${c.kind} ${c.title || '(제목 없음)'} (${c.status || '상태 모름'}, 문서 ID ${c.docId})`)),
      '결재가 끝난 뒤 올리는 것이 원칙입니다. 사람이 정하면 linkDone(끝난 것만 연결하고 나머지는 기안의견에 적는다) 또는 withoutLinks(회계와 합의했을 때만, 모두 기안의견에 적는다)를 씁니다.',
    ].join('\n'), { blocked: linkPlan.blocked.map(({ kind, docId, title, status }) => ({ kind, docId, title, status })) });
  }

  const amount = rows.reduce((t, r) => t + r.row.amount, 0);
  const line = claimLine(rt.config, { project: rt.config.cards?.[last4]?.project, amount });
  const st = claimStatement({ sorted, planned, purpose });
  const from = rt.config.cardMatch?.requisitionFrom;
  const noRequisition = from ? rows.filter((r) => r.kind === '구매' && !r.purchase && dayOf(r.row) >= from) : [];

  return {
    m, card, last4, holder, purpose, purposeCode, title, ref, rows, files, fileHashes, amount, line, st, groups,
    links: linkPlan.links, opinion: opinionText(linkPlan.opinion), opinionDocs: linkPlan.opinion,
    sorted, noRequisition,
  };
}

/**
 * 미리보기가 보여 준 것. 실행은 계획을 다시 세워 이 값이 같을 때만 올린다. 카드 대장이 늦게 올라와 행이
 * 늘거나, 품의가 그사이 결재돼 연결이 늘거나, 영수증이 바뀌면 다르다.
 */
const claimDigest = (p) => digestOf({
  title: p.title, purpose: p.purpose, purposeCode: p.purposeCode, card: p.last4, amount: p.amount,
  rows: p.rows.map((r) => ({ id: String(r.row.id), amount: r.row.amount, detail: r.detail })),
  links: p.links.map((c) => c.docId), opinion: p.opinionDocs.map((c) => c.docId),
  line: p.line.map((a) => ({ name: a.name, type: a.type })),
  files: p.fileHashes,
});

/** 메모를 저장해 다음 문서(다른 사용용도, 다시 올리기)에도 쓰이게 한다. 같은 결제의 옛 메모는 바꾼다. */
function saveNotes(rt, owner, used) {
  const old = rt.db.overrides(owner, CLAIM_NOTE_KIND);
  for (const n of used.filter((x) => !x.saved)) {
    for (const o of old) {
      if (o.match?.date === n.date && Number(o.match?.amount) === n.amount && (o.match?.merchant ?? null) === (n.merchant ?? null)) {
        rt.db.removeOverride(owner, o.id);
      }
    }
    const data = Object.fromEntries(Object.entries({ detail: n.detail, purpose: n.purpose, exclude: n.exclude, reason: n.reason }).filter(([, v]) => v !== undefined));
    rt.db.addOverride({ owner, kind: CLAIM_NOTE_KIND, match: { date: n.date, amount: n.amount, ...(n.merchant ? { merchant: n.merchant } : {}) }, data });
  }
}

const rowLine = (r) => {
  const how = r.source === 'receipt' ? `영수증 ${r.receipts.length}` : { trip: '출장 경비', asIs: '그대로 청구', purchase: '구매 품의', note: '메모' }[r.source];
  return `- ${tagOf(r.row)} · ${r.detail} · ${how}${r.note ? ' (메모 반영)' : ''}`;
};

function previewText(p) {
  const lines = [
    `지출결의 미리보기: ${p.title}`,
    `- 카드: ${p.card.name} (${p.last4}) · 기간 ${p.m.from}부터 ${p.m.to}까지 (한국 시간)`,
    `- 이번 문서: ${p.purpose} ${p.rows.length}건 ${won(p.amount)} · 영수증 ${p.files.length}개`,
    ...list(p.rows.map(rowLine)).map((l) => `  ${l}`),
    `- 사용용도별 (이 달 아직 안 올린 것): ${p.groups.map((g) => `${g.purpose} ${g.count}건 ${won(g.amount)}`).join(', ')}`,
    `- 명세서 대조: ${statementLine(p.st)}`,
  ];
  const why = (xs) => xs.map((e) => `${tagOf(e.row)} (${e.reason})`).join(', ');
  if (p.sorted.elsewhere.length) lines.push(`- 다른 사람 몫: ${why(p.sorted.elsewhere)}`);
  if (p.sorted.excluded.length) lines.push(`- 뺀 결제: ${why(p.sorted.excluded)}`);
  if (p.sorted.cancelled.length) lines.push(`- 취소: ${why(p.sorted.cancelled)}`);
  if (p.sorted.refunded.length) lines.push(`- 환불: ${why(p.sorted.refunded)}`);
  for (const w of p.sorted.warnings) lines.push(`- 주의: ${w}`);
  lines.push(`- 결재선: 기안 나 → ${describeLine(p.line)}`);
  lines.push(p.links.length
    ? `- 문서 연결 ${p.links.length}건: ${p.links.map((c) => `${c.kind} ${c.title || c.docId}${c.docNum ? ` (${c.docNum})` : ''}`).join(', ')}`
    : '- 문서 연결: 없음');
  if (p.opinionDocs.length) lines.push(`- 기안의견에 적을 문서 ${p.opinionDocs.length}건 (결재가 끝나지 않아 연결 대신): ${p.opinionDocs.map((c) => c.title || c.docId).join(', ')}`);
  if (p.noRequisition.length) lines.push(`- 참고: 구매 품의가 없는 구매 ${p.noRequisition.length}건: ${p.noRequisition.map((r) => tagOf(r.row)).join(', ')}`);
  if (p.unusedNotes?.length) lines.push(`- 참고: 이 달 카드 내역에 맞는 결제가 없는 메모 ${p.unusedNotes.length}건: ${p.unusedNotes.map((n) => `${n.date} ${won(n.amount)}`).join(', ')}`);
  lines.push('', '승인하면 카드 행에 사용용도와 상세를 저장하고, 지출결의서를 만들어 영수증을 붙이고, 문서를 연결하고, 결재선을 넣어 상신합니다. 상신하면 결재선에 알림이 갑니다.');
  return lines.join('\n');
}

function planData(p) {
  return {
    card: { name: p.card.name, last4: p.last4 },
    month: p.m.ym,
    purpose: p.purpose,
    title: p.title,
    amount: p.amount,
    rows: p.rows.map((r) => ({ date: dayOf(r.row), merchant: r.row.merchant, amount: r.row.amount, detail: r.detail, source: r.source, receipts: r.receipts.length })),
    groups: p.groups,
    statement: p.st,
    line: p.line,
    links: p.links.map(({ kind, docId, title, docNum }) => ({ kind, docId, title, docNum })),
    opinion: p.opinionDocs.map(({ kind, docId, title }) => ({ kind, docId, title })),
  };
}

/** 기안 창을 다루는 단계. 시험에서 갈아 끼운다. */
const DOM = { createClaimDocument, attachAndSubmit };

export function register(server, rt, { dom = DOM } = {}) {
  rt.tool(server, 'file_expense_claim', {
    kind: 'write',
    previewDigest: true,
    description: [
      '법인카드 지출결의(경비청구)를 올린다. 카드 한 장, 한 달(KST 1일부터 말일까지), 사용용도 하나가 문서 하나다.',
      '그 달이 끝난 뒤 다음 달 초에 올린다. 사용용도를 모르면 purpose 없이 부르면 사용용도별 묶음을 알려 준다.',
      '설명되지 않는 결제가 있으면 멈추고 목록을 준다. 사람에게 묻고 notes 로 알려 준다(지어내지 않는다).',
      '이 도구는 미리보기를 만들 뿐이고 사람이 승인해야 실제로 저장하고 상신한다. 먼저 guide 의 claim 주제를 읽는다.',
    ].join(' '),
    input: z.object({
      card: z.string().regex(/^\d{4}$/, '카드 뒤 네 자리').describe('법인카드 뒤 네 자리'),
      month: z.string().regex(/^\d{4}-(0[1-9]|1[0-2])$/, 'YYYY-MM').describe('청구하는 달 YYYY-MM (한국 시간 기준, 이미 끝난 달)'),
      purpose: z.string().trim().min(1).max(40).optional().describe('올릴 사용용도 이름 (예: 재료비, 차량유지비). 없으면 사용용도별 묶음을 알려 준다'),
      notes: z.array(noteArg).max(100).optional().describe('영수증, 출장, 품의로 설명되지 않는 결제에 대해 사람이 알려 준 것. 실행이 끝나면 저장돼 같은 결제에 다시 쓰인다'),
      linkDone: z.boolean().optional().describe('결재가 끝난 품의와 검수조서만 연결하고, 진행중인 것은 기안의견에 적는다. 사람이 정했을 때만'),
      withoutLinks: z.boolean().optional().describe('문서를 연결하지 않고 모두 기안의견에 적는다. 회계와 합의했을 때만'),
    }),
  }, {
    async preview(args, call) {
      assertMonthOver(claimMonth(args.month));
      return rt.session(call, async (s) => {
        const p = await buildClaim(rt, call, s, args);
        p.unusedNotes = p.sorted.unusedNotes;
        return { text: previewText(p), data: planData(p), digest: claimDigest(p) };
      });
    },

    async execute(args, call) {
      assertMonthOver(claimMonth(args.month));
      return rt.session(call, async (s) => {
        const { page, api, baseUrl } = s;
        const p = await buildClaim(rt, call, s, args);
        call.assertPreviewed(claimDigest(p));
        const links = p.links.map((c) => ({ id: c.docId, docNum: c.docNum }));

        const progress = {};
        await savePurposes(api, p.rows.map((r) => ({ raw: r.row.raw, purchaseId: r.row.id, purposeCode: p.purposeCode, detail: r.detail })));
        let made;
        try {
          made = await dom.createClaimDocument(page, baseUrl, { cardId: p.card.id, purchaseIds: p.rows.map((r) => r.row.id), progress });
        } catch (e) {
          if (!progress.created) throw e;
          rt.db.addFiling({ owner: call.owner, kind: 'claim', ref: p.ref, docId: null, title: p.title, status: '생성 중 멈춤', detail: { card: p.last4, month: p.m.ym, purpose: p.purpose } });
          const why = e instanceof PluginError ? e.message : '경비 화면 조작이 실패했습니다.';
          throw new PluginError('UPSTREAM', `지출결의 문서를 만드는 도중 멈췄습니다: ${why} 카드 행이 임시저장 문서에 묶였을 수 있습니다. 다시 올리기 전에 다우오피스 임시저장 문서를 확인해 주세요.`, { cause: e, data: { afterWrite: true } });
        }
        const { docId, docPage } = made;
        const detail = {
          card: p.last4, month: p.m.ym, purpose: p.purpose, amount: p.amount,
          rows: p.rows.map((r) => ({ id: r.row.id, date: dayOf(r.row), amount: r.row.amount })),
          links: links.map((l) => l.id), attachments: p.files.length,
        };
        const filing = rt.db.addFiling({ owner: call.owner, kind: 'claim', ref: p.ref, docId, title: p.title, status: '작성중', detail });

        let lineText;
        try {
          lineText = await dom.attachAndSubmit(docPage, { files: p.files, approvers: p.line, title: p.title, links, opinion: p.opinion, progress });
        } catch (e) {
          const why = e instanceof PluginError ? e.message : '기안 창 조작이 실패했습니다.';
          if (progress.sent) {
            // 결재요청을 누른 뒤의 실패는 올라갔는지 모르는 것이다. 다시 올리라고 하지 않는다.
            rt.db.updateFiling(call.owner, filing.id, { status: '확인 못 함' });
            throw new PluginError('UPSTREAM', `결재요청을 누른 뒤 결과를 확인하지 못했습니다 (문서 ID ${docId}): ${why} 다시 올리기 전에 다우오피스에서 그 문서의 상태를 확인해 주세요.`, { cause: e, data: { docId, afterWrite: true } });
          }
          rt.db.updateFiling(call.owner, filing.id, { status: '상신 실패' });
          throw new PluginError('UPSTREAM', `지출결의 문서(문서 ID ${docId})를 만들었지만 상신하지 못했습니다: ${why} 다우오피스 임시저장 문서를 확인해 주세요.`, { cause: e, data: { docId, afterWrite: true } });
        } finally {
          await docPage.close().catch(() => {});
        }
        saveNotes(rt, call.owner, p.sorted.usedNotes);

        const v = await verifyClaim(api, docId, { waitMs: VERIFY_MS });
        const ledger = await claimLedger(api, { cardId: p.card.id, from: p.m.from, to: p.m.to, docId });
        const mismatches = claimMismatches(v, { title: p.title, files: p.files, links, ledger, rows: p.rows.map((r) => r.row) });
        rt.db.updateFiling(call.owner, filing.id, { status: v.status || '확인 못 함', detail: { ...detail, line: lineText, verified: !mismatches.length, mismatches } });
        if (mismatches.length) {
          throw new PluginError('UPSTREAM', `지출결의를 올렸지만 서버 문서가 기대와 다릅니다 (문서 ID ${docId}): ${mismatches.join('; ')}. 다우오피스에서 확인하고, 잘못됐으면 결재자가 처리하기 전에 상신취소해 주세요.`, { data: { docId, mismatches, afterWrite: true } });
        }
        return {
          text: [
            `지출결의를 올렸습니다: ${p.title}`,
            `- 문서 ID ${docId} · ${v.status} · ${p.rows.length}건 ${won(p.amount)} · 첨부 ${v.attachCount}개${links.length ? ` · 문서 연결 ${links.length}건` : ''}`,
            `- 결재선: ${v.line.join(' → ') || describeLine(p.line)}`,
            p.opinion ? `- 기안의견에 연결하지 못한 문서 ${p.opinionDocs.length}건을 적었습니다.` : null,
          ].filter(Boolean).join('\n'),
          data: { docId, title: p.title, status: v.status, attachCount: v.attachCount, links: links.map((l) => l.id), verified: true },
        };
      });
    },
  });
}
