import * as z from 'zod';
import { PluginError } from '../errors.mjs';
import { digestOf } from '../runtime.mjs';
import { findForm } from '../dao/forms.mjs';
import { addDays } from '../dao/kst.mjs';
import { tripLine, describeLine } from '../dao/approvers.mjs';
import {
  TRIP_STATUS, tripKey, tripTitle, validateTrip, tripFormName, tripTransports,
} from '../dao/trips.mjs';
import { listSubmittedTrips, findDuplicates, describeTripDoc } from '../dao/trip-docs.mjs';
import { submitTrip, verifyTrip } from '../dao/trip-submit.mjs';

/**
 * 출장 신청.
 *
 * file_trip (write)  preview = 목적 검사, 제목, 내 기안 문서함과 부서 문서함 중복 확인, 결재선과
 *                    참조자. execute = 같은 검사를 다시 하고, 기안 부서의 기안 창에서 결재선과
 *                    참조자를 넣고, 라벨로 칸을 채워 상신하고, 서버 문서를 칸별로 대조한다.
 * list_trips (read)  플러그인에 기록된 출장, 원하면 다우오피스에 올라간 출장 문서까지.
 */

const dateArg = z.string().regex(/^\d{4}-\d{2}-\d{2}$/, 'YYYY-MM-DD');
const LIVE = new Set(['진행중', '완료', '대기']);
const VERIFY_MS = 90_000;

const STATUS_LABEL = { draft: '올리기 전', submitted: '올림', elsewhere: '다른 사람이 올림', cancelled: '취소' };

const period = (t) => (t.start === t.end ? t.start : `${t.start}부터 ${t.end}까지`);

function tripFrom(args) {
  const trip = {
    start: args.start, end: args.end, place: args.place.trim(), transport: args.transport, goal: args.goal.trim(),
  };
  if (args.note?.trim()) trip.note = args.note.trim();
  if (args.titlePlace?.trim()) trip.titlePlace = args.titlePlace.trim();
  return trip;
}

/** 미리보기와 실행이 함께 쓰는 검사. 막히면 VALIDATION 이나 CONFLICT. 다우오피스는 읽기만 한다. */
async function planTrip(rt, call, { api }, args) {
  const trip = tripFrom(args);
  const { errors } = validateTrip(trip, { transports: tripTransports(rt.config) });
  if (errors.length) {
    throw new PluginError('VALIDATION', ['출장 신청을 올리기 전에 고칠 것이 있습니다.', ...errors.map((e) => `- ${e}`)].join('\n'), { errors });
  }
  const account = rt.db.account(call.owner);
  if (!account?.deptId) {
    throw new PluginError('VALIDATION', '내 기안 부서를 몰라 출장 신청 창을 열 수 없습니다. me 로 부서를 확인하고, 조직도에서 읽지 못했으면 관리자에게 회사 설정 departments 를 요청해 주세요.');
  }
  const title = tripTitle(trip, { dept: account.deptCode, name: account.name });
  const formName = tripFormName(rt.config);
  const form = await findForm(api, formName);
  const { approvers, referers } = tripLine(rt.config);
  if (!approvers.length) throw new PluginError('VALIDATION', '출장 결재자가 회사 설정(trip.approvers)에 없습니다. 관리자에게 요청해 주세요.');

  const key = tripKey(trip);
  const saved = rt.db.trip(call.owner, key);
  // 출장 반년 앞부터 기안한 문서를 본다. 그보다 먼저 올린 출장 신청은 이 출장과 겹칠 일이 없다.
  const submitted = await listSubmittedTrips(api, { deptId: account.deptId, formName, since: addDays(trip.start, -183) });
  if (saved?.status === TRIP_STATUS.submitted && saved.docId && !submitted.some((d) => d.documentId === String(saved.docId))) {
    const r = await api(`/gw/api/approval/document/${encodeURIComponent(saved.docId)}`);
    const status = r.ok ? r.data?.data?.document?.docStatusName ?? '' : '';
    if (LIVE.has(status)) submitted.push({ documentId: String(saved.docId), box: '플러그인 기록', drafter: account.name, status, ...saved.data });
  }
  const { strong, weak } = findDuplicates(submitted, trip);
  if (strong.length && !args.allowOverlap) {
    throw new PluginError('CONFLICT', [
      '이미 올라간 출장과 겹칩니다. 같은 출장이면 다시 올리지 않습니다.',
      ...strong.map((d) => `- ${describeTripDoc(d)}`),
      '다른 출장이 맞다고 사람이 확인하면 allowOverlap 을 넣어 다시 만듭니다.',
    ].join('\n'), { duplicates: strong.map(({ documentId, start, end, place, status, drafter }) => ({ documentId, start, end, place, status, drafter })) });
  }
  return { trip, title, key, form, account, approvers, referers, strong, weak };
}

/** 미리보기가 보여 준 것. 실행은 같은 값일 때만 올린다(결재선, 참조자 설정이 바뀌면 다르다). */
const tripDigest = (p) => digestOf({
  title: p.title, trip: p.trip, formId: p.form.id, deptId: p.account.deptId,
  approvers: p.approvers.map((a) => ({ name: a.name, type: a.type, dept: a.dept ?? null })),
  referers: p.referers.map((r) => ({ name: r.name, dept: r.dept ?? null })),
  overlaps: p.strong.map((d) => d.documentId),
});

function previewText(p) {
  const t = p.trip;
  const lines = [
    `출장 신청 미리보기: ${p.title}`,
    `- 출장기간: ${period(t)}`,
    `- 출장지: ${t.place}`,
    `- 교통편: ${t.transport}`,
    `- 출장목적: ${t.goal}`,
    t.note ? `- 비고: ${t.note}` : null,
    `- 양식: ${p.form.title} · 기안 부서 ${p.account.deptName ?? p.account.deptId}`,
    `- 결재선: 기안 ${p.account.name} → ${describeLine(p.approvers)}`,
    `- 참조: ${p.referers.length ? p.referers.map((r) => r.name).join(', ') : '없음'}`,
  ];
  if (p.strong.length) lines.push(`- 겹치는 문서 (사람이 다른 출장이라고 확인함): ${p.strong.map(describeTripDoc).join(' / ')}`);
  if (p.weak.length) lines.push(`- 참고, 같은 기간의 다른 출장: ${p.weak.map(describeTripDoc).join(' / ')}`);
  lines.push('- 중복 확인은 내 기안 문서함과 부서 문서함만 봅니다. 다른 부서 사람이 올린 같은 출장은 잡히지 않으니 사람에게 확인해 주세요.');
  lines.push('', '승인하면 출장 신청서를 채워 상신합니다. 상신하면 결재선과 참조자에게 알림이 갑니다.');
  return lines.filter((l) => l !== null).join('\n');
}

const tripLineText = (t) => `- ${period(t.data)} ${t.data.place} · ${t.data.goal ?? ''} · ${t.data.transport ?? ''} · ${STATUS_LABEL[t.status] ?? t.status}${t.docId ? ` · 문서 ID ${t.docId}` : ''}`;

/** 기안 창을 다루는 단계. 시험에서 갈아 끼운다. */
const DOM = { submitTrip };

export function register(server, rt, { dom = DOM } = {}) {
  const transports = tripTransports(rt.config);

  rt.tool(server, 'file_trip', {
    kind: 'write',
    previewDigest: true,
    description: [
      '국내 출장 신청서를 올린다. 출장목적은 무엇을 하러 가는지 쓴다(출장지를 되풀이하지 않는다). 목적을 모르면 지어내지 말고 사람에게 묻는다.',
      '이미 올라간 출장과 겹치면 멈춘다. 이 도구는 미리보기를 만들 뿐이고 사람이 승인해야 상신한다. 먼저 guide 의 trip 주제를 읽는다.',
    ].join(' '),
    input: z.object({
      start: dateArg.describe('출장 시작일 YYYY-MM-DD'),
      end: dateArg.describe('출장 종료일 YYYY-MM-DD. 당일 출장이면 시작일과 같다'),
      place: z.string().trim().min(1).max(100).describe('출장지. 기관이나 장소 이름, 지역은 괄호에 (예: 테스트시청 (테스트시))'),
      transport: z.enum(transports).describe(`교통편: ${transports.join(', ')}. 자차는 기타`),
      goal: z.string().trim().min(1).max(100).describe('출장목적. 무엇을 하러 가는지 (예: 실증 부지 인허가 협의). 모르면 묻는다'),
      note: z.string().trim().max(200).optional().describe('비고. 동행자, 상세 일정'),
      titlePlace: z.string().trim().max(60).optional().describe('제목에 쓸 출장지. 출장지가 길 때만'),
      allowOverlap: z.boolean().optional().describe('겹치는 출장 문서가 다른 출장이라고 사람이 확인했을 때만 true'),
    }),
  }, {
    async preview(args, call) {
      return rt.session(call, async (s) => {
        const p = await planTrip(rt, call, s, args);
        return {
          text: previewText(p),
          data: {
            title: p.title, key: p.key, trip: p.trip, approvers: p.approvers, referers: p.referers,
            overlaps: p.strong.map((d) => d.documentId), sameDays: p.weak.map((d) => d.documentId),
          },
          digest: tripDigest(p),
        };
      });
    },

    async execute(args, call) {
      return rt.session(call, async (s) => {
        const { page, api, baseUrl } = s;
        const p = await planTrip(rt, call, s, args);
        call.assertPreviewed(tripDigest(p));
        // 결재요청 뒤의 실패는 런타임이 결과를 모르는 것(afterWrite)으로 알린다.
        const { docId, line, referers } = await dom.submitTrip(page, api, baseUrl, {
          trip: p.trip, title: p.title, deptId: p.account.deptId, formId: p.form.id, approvers: p.approvers, referers: p.referers,
        });
        // 저장된 출장의 다른 값(claimName, claimUntil, 옮겨 온 기록)은 남기고 이번 값을 덮는다. 올리기 전
        // 상태(draft, 취소)만 submitted 로 바꾸고, 다른 사람이 올린 출장(elsewhere) 같은 기록은 그대로 둔다.
        const saved = rt.db.trip(call.owner, p.key);
        const status = saved && ![TRIP_STATUS.draft, TRIP_STATUS.cancelled].includes(saved.status) ? saved.status : TRIP_STATUS.submitted;
        rt.db.upsertTrip({ owner: call.owner, key: p.key, data: { ...saved?.data, ...p.trip, title: p.title }, docId, status });
        const filing = rt.db.addFiling({ owner: call.owner, kind: 'trip', ref: p.key, docId, title: p.title, status: '상신', detail: { line, referers } });

        const v = await verifyTrip(api, docId, { ...p.trip, title: p.title }, { approvers: p.approvers, referers: p.referers, drafter: p.account.name, waitMs: VERIFY_MS });
        rt.db.updateFiling(call.owner, filing.id, { status: v.status || '확인 못 함', detail: { line, referers, verified: !v.mismatches.length, mismatches: v.mismatches } });
        if (v.mismatches.length) {
          throw new PluginError('UPSTREAM', `출장 신청을 올렸지만 서버 문서가 기대와 다릅니다 (문서 ID ${docId}): ${v.mismatches.join('; ')}. 다우오피스에서 확인하고, 잘못됐으면 결재자가 처리하기 전에 상신취소해 주세요.`, { data: { docId, mismatches: v.mismatches, afterWrite: true } });
        }
        return {
          text: [
            `출장 신청을 올렸습니다: ${p.title}`,
            `- 문서 ID ${docId} · ${v.status}`,
            `- 결재선: ${v.line.join(' → ') || line}`,
            v.referers.length ? `- 참조: ${v.referers.join(', ')}` : null,
          ].filter(Boolean).join('\n'),
          data: { docId, title: p.title, status: v.status, verified: true },
        };
      });
    },
  });

  rt.tool(server, 'list_trips', {
    kind: 'read',
    description: '내 출장 목록을 본다. 플러그인에 기록된 출장(올린 것, 다른 사람이 올린 것)과, includeSubmitted 를 주면 다우오피스 내 기안 문서함과 부서 문서함의 출장 문서까지 본다. 지출결의의 출장 경비가 어느 출장인지 볼 때도 쓴다.',
    input: z.object({
      from: dateArg.optional().describe('이 날 이후에 끝나는 출장만'),
      status: z.enum(Object.values(TRIP_STATUS)).optional().describe('기록 상태'),
      includeSubmitted: z.boolean().optional().describe('다우오피스에 올라간 출장 문서도 읽는다 (조금 느리다)'),
    }),
  }, async ({ from, status, includeSubmitted }, call) => {
    let saved = rt.db.trips(call.owner, status ? { status } : {});
    if (from) saved = saved.filter((t) => (t.data?.end ?? t.data?.start ?? '') >= from);
    saved.sort((a, b) => String(b.data?.start ?? '').localeCompare(String(a.data?.start ?? '')));
    const lines = saved.length ? [`기록된 출장 ${saved.length}건 (최근순)`, ...saved.slice(0, 60).map(tripLineText)] : ['기록된 출장이 없습니다.'];
    const data = { trips: saved.map((t) => ({ key: t.key, status: t.status, docId: t.docId, ...t.data })) };
    if (includeSubmitted) {
      const docs = await rt.session(call, async ({ api }) => {
        const account = rt.db.account(call.owner);
        return listSubmittedTrips(api, { deptId: account?.deptId, formName: tripFormName(rt.config) });
      });
      const shown = docs.filter((d) => !from || (d.end || d.start) >= from).sort((a, b) => String(b.start).localeCompare(String(a.start)));
      lines.push('', shown.length ? `다우오피스 출장 문서 ${shown.length}건` : '다우오피스에서 찾은 출장 문서가 없습니다.');
      for (const d of shown.slice(0, 60)) lines.push(`- ${describeTripDoc(d)}${d.goal ? ` · ${d.goal}` : ''}`);
      data.documents = shown.map(({ documentId, box, drafter, status: st, start, end, place, transport, goal }) => ({ documentId, box, drafter, status: st, start, end, place, transport, goal }));
    }
    return { text: lines.join('\n'), data };
  });
}
