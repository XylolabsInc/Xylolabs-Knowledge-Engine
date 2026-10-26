import * as z from 'zod';
import { PluginError } from '../errors.mjs';
import { digestOf } from '../runtime.mjs';
import { kstDate } from '../dao/kst.mjs';
import { sessionUser } from '../dao/org.mjs';
import {
  CERT_KINDS, CERT_PURPOSES, CERT_STATUS, STEP_LABEL, listCertRequests, normalizeCert, pickRequest, certSteps, certLineNames, onlyMe,
  selfApprovalAllowed, checkRequestDialog, saveRequest, submitRequest, approveRequest, downloadCertificate,
} from '../dao/certificate.mjs';

/**
 * 증명서.
 *
 * issue_certificate (write)     preview = 무엇을 신청하는지, 남은 단계, 결재 경로(셀프승인이면 바로
 *                               완료된다는 것). 새 신청이면 신청 창을 채워 보고 취소해 용도 선택지를
 *                               확인한다(아무것도 저장하지 않는다). execute = 저장 → 상신 → (셀프승인)
 *                               승인 → 출력. PDF 는 본인에게만 가는 파일(private)로 돌려준다.
 * certificate_history (read)    발급 신청 이력. 사번, 신청 건 UUID 는 빼고 보여 준다.
 *
 * PDF 에는 주민번호 뒷자리(기본 마스킹), 주소, 생년월일이 들어 있다. 디스크에 남기지 않고 로그에
 * 쓰지 않는다.
 */

const WAIT_MS = 90_000;
const defaultSleep = (ms) => new Promise((res) => setTimeout(res, ms));

/** 신청 건이 원하는 상태가 될 때까지 이력을 다시 읽는다. 마지막으로 읽은 행을 돌려준다. */
async function waitForRow(api, id, ok, { waitMs = WAIT_MS, sleep = defaultSleep } = {}) {
  const until = Date.now() + waitMs;
  let row = (await listCertRequests(api)).find((r) => r.issuecertId === id) ?? null;
  while (!(row && ok(row)) && Date.now() < until) {
    await sleep(4000);
    row = (await listCertRequests(api)).find((r) => r.issuecertId === id) ?? null;
  }
  return row;
}

const requestOf = (args) => ({ kind: args.kind, purpose: args.purpose.trim(), target: args.target.trim(), note: args.note?.trim() ?? '' });

function routeText(self, me) {
  return self
    ? `결재선을 본인 단독(기안 ${me} → 결재 ${me})으로 바꿔 상신하고 본인이 바로 승인합니다. 다른 사람에게 결재 알림이 가지 않고, 승인하는 즉시 결재가 완료돼 PDF 가 나옵니다. 결재 기록과 결재승인번호는 남습니다.`
    : '양식의 기본 결재선으로 상신합니다. 결재선 사람에게 알림이 가고, 그 사람들이 승인해야 PDF 가 나옵니다. 승인된 뒤 같은 요청을 다시 하면 PDF 를 받습니다.';
}

function decideSelf(rt, args) {
  const allowed = selfApprovalAllowed(rt.config);
  if (args.selfApprove && !allowed) {
    throw new PluginError('VALIDATION', '회사 설정이 증명서 셀프승인을 허락하지 않습니다. 기본 결재선으로 올리려면 selfApprove 를 빼 주세요.');
  }
  return args.selfApprove ?? allowed;
}

/** 이미 결재 중인 신청을 본인 승인으로 끝내도 되는가. 결재선에 본인만 있어야 한다. */
async function assertSelfLine(api, row, me) {
  const names = await certLineNames(api, row.docId);
  if (!onlyMe(names, me)) {
    throw new PluginError('CONFLICT', `이미 결재 중인 증명서 신청의 결재선에 다른 사람이 있어 본인 승인으로 끝낼 수 없습니다: ${names.join(' → ') || '(읽지 못함)'}. 결재가 끝나면 다시 요청해 주세요.`);
  }
}

/** 미리보기가 보여 준 것. 그사이 신청이 결재중에서 결재완료로 바뀌는 식으로 단계가 달라지면 다르다. */
const certDigest = ({ req, self, steps, row }) => digestOf({
  request: req, self, steps, docId: row?.docId ? String(row.docId) : null, status: row ? String(row.docStatusCode ?? '') : null,
});

const describeRow = (r) => {
  const c = normalizeCert(r);
  return `${c.requestDate} ${c.kind} / ${c.purpose} / ${c.target} · ${c.status}${c.expireDate ? ` (유효 ${c.expireDate}까지)` : ''}`;
};

/** 인사 앱과 결재 창을 다루는 단계. 시험에서 갈아 끼운다. */
const DOM = { checkRequestDialog, saveRequest, submitRequest, approveRequest, downloadCertificate };

export function register(server, rt, { dom = DOM } = {}) {
  rt.tool(server, 'issue_certificate', {
    kind: 'write',
    previewDigest: true,
    description: [
      '재직증명서나 경력증명서를 발급받는다(신청 저장, 전자결재 상신, 승인, PDF 출력).',
      '용도와 제출처는 사람이 정하는 값이라 모르면 묻는다. PDF 는 본인에게 DM 으로만 간다.',
      '이 도구는 미리보기를 만들 뿐이고 사람이 승인해야 신청하고 상신한다. 먼저 guide 의 certificate 주제를 읽는다.',
    ].join(' '),
    input: z.object({
      kind: z.enum(CERT_KINDS).describe('증명서 종류'),
      purpose: z.string().trim().min(1).max(30).describe(`용도. 신청 창의 선택지 가운데 하나 (${CERT_PURPOSES.join(', ')}). 모르면 묻는다`),
      target: z.string().trim().min(1).max(60).describe('제출처 (예: 테스트은행). 모르면 묻는다'),
      note: z.string().trim().max(100).optional().describe('비고'),
      selfApprove: z.boolean().optional().describe('본인 단독 결재선으로 올려 본인이 승인한다. 회사 설정이 허락할 때만. 빼면 회사 설정을 따른다'),
    }),
  }, {
    async preview(args, call) {
      const req = requestOf(args);
      const self = decideSelf(rt, args);
      return rt.session(call, async ({ page, api, baseUrl }) => {
        const today = kstDate();
        const row = pickRequest(await listCertRequests(api), req, today);
        const steps = certSteps(row, { self });
        if (steps[0] === 'wait') {
          throw new PluginError('CONFLICT', `같은 증명서 신청이 이미 결재 중입니다 (${describeRow(row)}). 결재가 끝나면 다시 요청하면 PDF 를 받습니다.`);
        }
        if (steps.includes('save')) await dom.checkRequestDialog(page, baseUrl, req);
        const me = rt.db.account(call.owner)?.name ?? (await sessionUser(api)).name;
        if (steps[0] === 'approve') await assertSelfLine(api, row, me);
        const lines = [
          `증명서 발급 미리보기: ${req.kind}`,
          `- 용도: ${req.purpose}`,
          `- 제출처: ${req.target}`,
          req.note ? `- 비고: ${req.note}` : null,
          row ? `- 이어서 처리할 신청: ${describeRow(row)}` : '- 새로 신청합니다.',
          `- 남은 단계: ${steps.map((s) => STEP_LABEL[s]).join(' → ')}`,
          steps.includes('submit') || steps.includes('approve') ? `- 결재: ${routeText(self, me)}` : null,
          steps.includes('print') ? '- PDF 는 본인에게 DM 으로만 보냅니다. 주민번호 뒷자리는 가려서 나옵니다.' : null,
          '',
          '승인하면 위 단계를 실행합니다.',
        ];
        return {
          text: lines.filter((l) => l !== null).join('\n'),
          data: { request: req, steps, selfApproval: self, existing: row ? normalizeCert(row) : null },
          digest: certDigest({ req, self, steps, row }),
        };
      });
    },

    async execute(args, call) {
      const req = requestOf(args);
      const self = decideSelf(rt, args);
      return rt.session(call, async ({ page, api, baseUrl }) => {
        const today = kstDate();
        let row = pickRequest(await listCertRequests(api), req, today);
        const steps = certSteps(row, { self });
        if (steps[0] === 'wait') {
          throw new PluginError('CONFLICT', `같은 증명서 신청이 이미 결재 중입니다 (${describeRow(row)}).`);
        }
        call.assertPreviewed(certDigest({ req, self, steps, row }));
        const account = rt.db.account(call.owner);
        const me = account?.name ?? (await sessionUser(api)).name;
        const ref = `${req.kind}/${req.purpose}/${req.target}`;

        if (steps.includes('save')) {
          await dom.saveRequest(page, baseUrl, req);
          row = pickRequest(await listCertRequests(api), req, today);
          if (!row || String(row.docStatusCode ?? '') !== '') {
            throw new PluginError('UPSTREAM', '증명서 신청을 저장했지만 목록에서 찾지 못했습니다. 다우오피스 인사 > 증명서 발급 신청에서 확인해 주세요.');
          }
        }
        let filing = null;
        if (steps.includes('submit')) {
          const line = await dom.submitRequest(page, baseUrl, row.issuecertId, { self, me, myDept: account?.deptName });
          row = await waitForRow(api, row.issuecertId, (r) => r.docId && ['01', '02'].includes(String(r.docStatusCode ?? '')));
          if (!row?.docId) throw new PluginError('UPSTREAM', '증명서 신청을 상신했지만 결재 문서가 목록에 나타나지 않습니다. 다우오피스에서 확인해 주세요.');
          filing = rt.db.addFiling({ owner: call.owner, kind: 'certificate', ref, docId: String(row.docId), title: req.kind, status: CERT_STATUS[row.docStatusCode] ?? '', detail: { self, line } });
        }
        if (!self && String(row.docStatusCode ?? '') !== '02') {
          return {
            text: `${req.kind} 신청을 상신했습니다 (문서 ID ${row.docId}). 결재선 사람이 승인하면 같은 요청을 다시 해 PDF 를 받습니다.`,
            data: { ...normalizeCert(row), selfApproval: false },
          };
        }
        if (String(row.docStatusCode ?? '') === '01') {
          // 방금 올린 신청이든 전에 올린 신청이든, 결재선에 본인만 있을 때만 승인한다.
          await assertSelfLine(api, row, me);
          await dom.approveRequest(page, baseUrl, row.docId);
          row = await waitForRow(api, row.issuecertId, (r) => String(r.docStatusCode ?? '') === '02');
          if (String(row?.docStatusCode ?? '') !== '02') {
            throw new PluginError('UPSTREAM', '증명서 결재를 승인했지만 결재완료로 바뀌지 않았습니다. 다우오피스에서 확인해 주세요.');
          }
        }
        // 같은 문서의 기록이 있으면 고친다. 다시 출력할 때마다 기록을 늘리지 않는다.
        filing ??= row.docId ? rt.db.filings(call.owner, { kind: 'certificate', ref }).findLast((f) => f.docId === String(row.docId)) ?? null : null;
        if (filing) rt.db.updateFiling(call.owner, filing.id, { status: CERT_STATUS['02'], detail: { ...filing.detail, steps, printedAt: kstDate() } });
        else rt.db.addFiling({ owner: call.owner, kind: 'certificate', ref, docId: row.docId ? String(row.docId) : null, title: req.kind, status: CERT_STATUS['02'], detail: { self, steps, printedAt: kstDate() } });

        const pdf = await dom.downloadCertificate(page, baseUrl, row.issuecertId);
        const c = normalizeCert(row);
        return {
          text: `${req.kind}를 발급했습니다 (${c.purpose}, 제출처 ${c.target}${c.expireDate ? `, 유효 ${c.expireDate}까지` : ''}). PDF 는 본인에게만 보냅니다.`,
          files: [{ name: `${req.kind}_${c.requestDate || today}.pdf`, data: pdf, mimeType: 'application/pdf', private: true }],
          data: { ...c, selfApproval: self },
        };
      });
    },
  });

  rt.tool(server, 'certificate_history', {
    kind: 'read',
    description: '내 증명서 발급 신청 이력(종류, 용도, 제출처, 상태, 유효일자)을 본다. 증명서가 승인됐는지, 이미 신청했는지 볼 때 쓴다.',
    input: z.object({ limit: z.number().int().min(1).max(100).optional().describe('최대 건수, 기본 20') }),
  }, async ({ limit = 20 }, call) => rt.session(call, async ({ api }) => {
    const rows = (await listCertRequests(api)).map(normalizeCert).sort((a, b) => b.requestDate.localeCompare(a.requestDate));
    if (!rows.length) return { text: '증명서 발급 신청 이력이 없습니다.', data: { requests: [] } };
    const shown = rows.slice(0, limit);
    const lines = [`증명서 발급 신청 ${rows.length}건 (최근순)`, ...shown.map((c) => `- ${c.requestDate} ${c.kind} / ${c.purpose} / ${c.target} · ${c.status}${c.approvalCode ? ` · 승인번호 ${c.approvalCode}` : ''}${c.expireDate ? ` · 유효 ${c.expireDate}까지` : ''}`)];
    if (rows.length > shown.length) lines.push(`(최근 ${shown.length}건만 보입니다.)`);
    return { text: lines.join('\n'), data: { requests: shown } };
  }));
}
