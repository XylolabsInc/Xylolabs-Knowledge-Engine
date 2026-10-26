import * as z from 'zod';
import { PluginError } from '../errors.mjs';
import { kstDate, addDays } from '../dao/kst.mjs';
import { listForms } from '../dao/forms.mjs';
import { listCards, listUsage, pickCard, last4Of, statusCodeOf, STATUS } from '../dao/cards.mjs';
import { withdrawDocument } from '../dao/approval.mjs';
import {
  todoCount, listTodo, listDrafts, readDocument, attendanceToday, leaveBalance, checkDocId,
} from '../dao/documents.mjs';

/**
 * Read tools, plus `withdraw_document` (gated) because it works on one document like
 * `read_document`. Result text is Korean and short enough for a chat reply.
 */

const won = (n) => `${Number(n).toLocaleString('ko-KR')}원`;
const dateArg = z.string().regex(/^\d{4}-\d{2}-\d{2}$/, 'YYYY-MM-DD');
const docIdArg = z.string().regex(/^\d{6,25}$/, '숫자로 된 문서 ID').describe('다우오피스 문서 ID (숫자). 내 기안 문서나 결재할 문서 목록에서 얻는다.');
const MAX_ROWS = 60;

const docLine = (d) => `- ${d.title || d.formName || '(제목 없음)'} · ${d.status || '상태 모름'} · ${String(d.draftedAt).slice(0, 10)}${d.drafter ? ` · ${d.drafter}` : ''} · 문서 ID ${d.id}`;

export function register(server, rt) {
  rt.tool(server, 'attendance_today', {
    kind: 'read',
    description: '오늘 내 근무 상태(출근, 근무 중, 퇴근 등)를 본다. 사람이 오늘 근태나 출퇴근 기록을 물을 때 쓴다.',
    input: z.object({}),
  }, async (args, call) => rt.session(call, async ({ api }) => {
    const a = await attendanceToday(api);
    const lines = [`오늘 근무 상태: ${a.status || '(없음)'}`];
    if (a.clockIn) lines.push(`- 출근: ${a.clockIn}`);
    if (a.clockOut) lines.push(`- 퇴근: ${a.clockOut}`);
    if (a.worked) lines.push(`- 누적 근무: ${a.worked}`);
    return { text: lines.join('\n'), data: a };
  }));

  rt.tool(server, 'leave_balance', {
    kind: 'read',
    description: '내 연차 잔여(그리고 알 수 있으면 총계와 사용)를 본다. 사람이 연차, 휴가 일수를 물을 때 쓴다.',
    input: z.object({}),
  }, async (args, call) => rt.session(call, async ({ api }) => {
    const l = await leaveBalance(api);
    const lines = [`남은 연차: ${l.remain}`];
    if (l.total) lines.push(`- 총 연차: ${l.total}`);
    if (l.used) lines.push(`- 사용: ${l.used}`);
    return { text: lines.join('\n'), data: l };
  }));

  rt.tool(server, 'approval_inbox', {
    kind: 'read',
    description: '내가 결재할 문서의 건수와 목록을 본다. 사람이 "결재할 것", "결재 대기", "결재함" 을 물을 때 쓴다. 이 도구는 결재(승인)를 하지 않는다.',
    input: z.object({ limit: z.number().int().min(1).max(50).optional().describe('목록 최대 건수, 기본 20') }),
  }, async ({ limit = 20 }, call) => rt.session(call, async ({ api }) => {
    const count = await todoCount(api);
    if (!count) return { text: '결재할 문서가 없습니다.', data: { count: 0, docs: [] } };
    const { docs, partial } = await listTodo(api, { offset: limit });
    const shown = docs.slice(0, limit);
    const lines = [`결재할 문서 ${count}건`, ...shown.map(docLine)];
    if (partial && count > shown.length) lines.push(`(목록은 최근 ${shown.length}건만 보입니다. 나머지는 다우오피스 결재함에서 확인해 주세요.)`);
    return { text: lines.join('\n'), data: { count, docs: shown } };
  }));

  rt.tool(server, 'my_drafts', {
    kind: 'read',
    description: '내가 올린 결재 문서(기안)와 상태(진행중, 완료, 반려, 임시저장 등)를 최신순으로 본다. 사람이 "내가 올린 문서", "상신한 것", "품의 상태" 를 물을 때 쓴다. 문서 하나의 내용은 read_document 로 본다.',
    input: z.object({
      limit: z.number().int().min(1).max(100).optional().describe('최대 건수, 기본 20'),
      status: z.string().optional().describe('이 상태만 본다. 예: 진행중, 완료, 반려, 임시저장'),
      query: z.string().optional().describe('제목이나 양식 이름에 이 말이 든 것만 본다'),
    }),
  }, async ({ limit = 20, status, query }, call) => rt.session(call, async ({ api }) => {
    const offset = status || query ? 100 : limit;
    let docs = await listDrafts(api, { offset });
    if (status) docs = docs.filter((d) => d.status.includes(status));
    if (query) docs = docs.filter((d) => `${d.title} ${d.formName}`.includes(query));
    docs = docs.slice(0, limit);
    if (!docs.length) return { text: '조건에 맞는 기안 문서가 없습니다.', data: { docs: [] } };
    return { text: [`내 기안 문서 ${docs.length}건 (최신순)`, ...docs.map(docLine)].join('\n'), data: { docs } };
  }));

  rt.tool(server, 'read_document', {
    kind: 'read',
    description: '결재 문서 하나의 제목, 상태, 결재선, 수신·참조자, 첨부, 본문 칸 값을 읽는다. 문서 ID 가 필요하다. 모르면 먼저 my_drafts 나 approval_inbox 로 찾는다.',
    input: z.object({ docId: docIdArg }),
  }, async ({ docId }, call) => rt.session(call, async ({ page, api }) => {
    const d = await readDocument(page, api, docId);
    const lines = [
      `${d.title || d.formName} (${d.status})`,
      `- 양식: ${d.formName}${d.docNum ? ` · 문서번호 ${d.docNum}` : ''}`,
      d.drafter ? `- 기안: ${d.drafter}${d.draftedAt ? ` · ${String(d.draftedAt).slice(0, 10)}` : ''}` : null,
      d.line.length ? `- 결재선: ${d.line.map((a) => `${a.display || a.name}${a.status ? `(${a.status})` : ''}`).join(' → ')}` : null,
      d.recipients.length ? `- 수신: ${d.recipients.join(', ')}` : null,
      d.referers.length ? `- 참조: ${d.referers.join(', ')}` : null,
      d.attachments.length ? `- 첨부 ${d.attachments.length}개: ${d.attachments.join(', ')}` : null,
      ...(d.fields.length ? ['', '본문', ...d.fields.map((f) => `- ${f.label || '(이름 없는 칸)'}: ${f.values.filter(Boolean).join(' / ')}`)] : []),
    ].filter((l) => l !== null);
    return { text: lines.join('\n'), data: d };
  }));

  rt.tool(server, 'list_forms', {
    kind: 'read',
    description: '전자결재 양식 목록을 본다. 사람이 어떤 양식이 있는지 묻거나, 양식 이름을 정확히 알아야 할 때 쓴다.',
    input: z.object({ query: z.string().optional().describe('경로나 이름에 이 말이 든 양식만') }),
  }, async ({ query }, call) => rt.session(call, async ({ api }) => {
    let forms = (await listForms(api)).filter((f) => f.rel === 'FORM');
    if (query) forms = forms.filter((f) => f.path.includes(query));
    if (!forms.length) return { text: '맞는 결재 양식이 없습니다.', data: { forms: [] } };
    const shown = forms.slice(0, 80);
    const lines = [`결재 양식 ${forms.length}개`, ...shown.map((f) => `- ${f.path}`)];
    if (forms.length > shown.length) lines.push(`(앞의 ${shown.length}개만 보입니다. 검색어로 좁혀 주세요.)`);
    return { text: lines.join('\n'), data: { forms: shown.map(({ title, path }) => ({ title, path })) } };
  }));

  rt.tool(server, 'list_cards', {
    kind: 'read',
    description: '내가 볼 수 있는 법인카드 목록(카드 이름, 담당자, 카드사, 마스킹된 번호)을 본다. 사용내역을 카드별로 보기 전에 카드를 고를 때 쓴다.',
    input: z.object({}),
  }, async (args, call) => rt.session(call, async ({ api }) => {
    const cards = await listCards(api);
    if (!cards.length) return { text: '볼 수 있는 법인카드가 없습니다.', data: { cards: [] } };
    const lines = [`법인카드 ${cards.length}장`, ...cards.map((c) => `- ${c.name} · ${c.holder || '담당자 모름'} · ${c.issuer} · ${c.cardNo}`)];
    return {
      text: lines.join('\n'),
      data: { cards: cards.map((c) => ({ name: c.name, holder: c.holder, issuer: c.issuer, cardNo: c.cardNo, last4: last4Of(c) })) },
    };
  }));

  rt.tool(server, 'card_usage', {
    kind: 'read',
    description: '법인카드 사용내역과 상태별 합계를 본다(조회만 한다). 기간을 안 주면 이번 달 1일부터 오늘까지다. 카드를 안 주면 볼 수 있는 모든 카드다. 카드 대장은 영업일 2-3일 늦게 올라오므로 최근 결제가 없으면 아직 안 올라온 것이라고 말한다.',
    input: z.object({
      from: dateArg.optional().describe('시작일 YYYY-MM-DD'),
      to: dateArg.optional().describe('종료일 YYYY-MM-DD'),
      days: z.number().int().min(1).max(366).optional().describe('오늘까지 최근 N일. from 대신 쓴다'),
      card: z.string().optional().describe('카드 뒤 네 자리, 카드 이름 일부'),
      status: z.enum(Object.values(STATUS)).optional().describe('전자결재 상태'),
    }),
  }, async ({ from, to, days, card, status }, call) => rt.session(call, async ({ api }) => {
    const today = kstDate();
    const end = to ?? today;
    const start = days ? addDays(today, -(days - 1)) : (from ?? `${today.slice(0, 7)}-01`);
    if (start > end) throw new PluginError('VALIDATION', `기간이 거꾸로입니다: ${start}부터 ${end}까지`);
    const picked = card ? pickCard(await listCards(api), card) : null;
    const statuses = status ? [statusCodeOf(status)] : [];
    const { rows, totals, truncated } = await listUsage(api, { from: start, to: end, cardId: picked?.id ?? '', statuses });
    const head = picked ? `${picked.name} (${last4Of(picked) || picked.cardNo})` : '볼 수 있는 모든 카드';
    const lines = [`법인카드 사용내역 · ${head} · ${start}부터 ${end}까지${status ? ` · ${status}` : ''}`];
    for (const r of rows.slice(0, MAX_ROWS)) {
      lines.push(`- ${String(r.useDate).slice(0, 10)} ${r.merchant} ${won(r.amount)} · ${r.status}${r.purpose ? ` · ${r.purpose}` : ''}${r.claimant ? ` · ${r.claimant}` : ''}`);
    }
    if (rows.length > MAX_ROWS) lines.push(`(앞의 ${MAX_ROWS}건만 보입니다. 기간이나 카드로 좁혀 주세요.)`);
    lines.push('', ...Object.entries(totals.byStatus).map(([name, s]) => `${name} ${s.count}건 ${won(s.amount)}`));
    lines.push(`합계 ${totals.count}건 ${won(totals.amount)}`);
    if (truncated.length) lines.push(`주의: ${truncated.join(', ')} 은 하루치가 관측 최대치를 채워 잘렸는지 확인하지 못했습니다.`);
    const newest = rows[0]?.useDate ? String(rows[0].useDate).slice(0, 10) : null;
    if (newest && newest < end) lines.push(`가장 최근 결제가 ${newest} 입니다. 카드사 반영이 영업일 2-3일 늦어 그 뒤 결제는 아직 안 올라왔을 수 있습니다.`);
    if (!picked) lines.push('카드를 지정하지 않아 모든 카드를 합쳤습니다. 행에는 어느 카드인지 표시되지 않습니다.');
    return {
      text: lines.join('\n'),
      data: {
        from: start, to: end, card: picked ? { name: picked.name, last4: last4Of(picked) } : null,
        totals,
        rows: rows.map(({ raw, ...r }) => r),
      },
    };
  }));

  rt.tool(server, 'withdraw_document', {
    kind: 'write',
    description: '내가 상신한 결재 문서를 상신취소한다(결재자가 처리하기 전에만 된다). 문서는 임시저장으로 돌아가고 같은 ID 로 고쳐 다시 올릴 수 있다. 사람이 어느 문서를 취소할지 분명히 말했을 때만 쓰고, 문서 ID 를 모르면 my_drafts 로 찾아 확인받는다. 이 도구는 미리보기를 만들 뿐이고 사람이 승인 버튼을 눌러야 실제로 취소된다.',
    input: z.object({ docId: docIdArg }),
  }, {
    async preview({ docId }, call) {
      return rt.session(call, async ({ page, api }) => {
        const d = await withdrawable(page, api, docId);
        return {
          text: [
            '다음 문서를 상신취소합니다.',
            `- 제목: ${d.title || d.formName}`,
            `- 상태: ${d.status}`,
            d.line.length ? `- 결재선: ${d.line.map((a) => a.display || a.name).join(' → ')}` : null,
            '승인하면 문서가 임시저장으로 돌아가고, 같은 문서 ID 로 고쳐 다시 올릴 수 있습니다.',
          ].filter(Boolean).join('\n'),
          data: { docId: d.id, title: d.title, status: d.status },
        };
      });
    },
    async execute({ docId }, call) {
      return rt.session(call, async ({ page, api, baseUrl }) => {
        const d = await withdrawable(page, api, docId);
        const status = await withdrawDocument(page, api, baseUrl, d.id);
        if (status !== '임시저장') {
          throw new PluginError('UPSTREAM', `상신취소 뒤 상태가 임시저장이 아닙니다 (${status || '모름'}). 다우오피스에서 문서를 확인해 주세요.`);
        }
        rt.db.addFiling({ owner: call.owner, kind: 'withdraw', ref: d.id, docId: d.id, title: d.title, status });
        return {
          text: `상신취소했습니다: ${d.title || d.formName} (문서 ID ${d.id}). 문서는 임시저장으로 돌아갔습니다.`,
          data: { docId: d.id, title: d.title, status, verified: true },
        };
      });
    },
  });
}

/** 상신취소할 수 있는 문서인지 본다. 미리보기와 실행이 같은 검사를 한다. */
async function withdrawable(page, api, docId) {
  const d = await readDocument(page, api, checkDocId(docId));
  if (d.status === '임시저장') throw new PluginError('CONFLICT', `이미 임시저장 상태인 문서입니다: ${d.title || d.formName}`);
  if (!d.canCancel) {
    throw new PluginError('CONFLICT', `상신취소할 수 없는 문서입니다: ${d.title || d.formName} (${d.status}). 결재가 이미 진행됐거나 내가 올린 문서가 아닙니다.`);
  }
  return d;
}
