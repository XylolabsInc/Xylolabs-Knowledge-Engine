import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  projectApprovers, purchaseLine, claimLine, inspectionLine, tripLine, describeLine, resolveEntry,
} from '../src/dao/approvers.mjs';
import { parseConfig } from '../src/config.mjs';

const cfg = parseConfig({
  people: { ceo: '김대표', cfo: '이재무', cto: '박기술' },
  cfoFrom: 1_000_000,
  projects: { 스마트농장: { reviewer: '최담당' }, 해양로봇: { reviewer: '정담당' } },
  lines: { purchase: 'project', claim: [{ role: 'ceo', type: '결재' }], inspection: 'ctoFinal' },
  trip: { approvers: [{ role: 'ceo' }], referers: [{ name: '한참조', dept: '경영지원팀' }] },
});

test('project line: reviewer, CFO from the threshold, CEO last', () => {
  const cases = [
    { amount: 999_999, want: '합의 최담당 → 결재 김대표' },
    { amount: 1_000_000, want: '합의 최담당 → 합의 이재무 → 결재 김대표' },
    { amount: '2500000', want: '합의 최담당 → 합의 이재무 → 결재 김대표' },
  ];
  for (const { amount, want } of cases) assert.equal(describeLine(projectApprovers(cfg, '스마트농장', amount)), want);
});

test('unknown project or amount stops instead of guessing', () => {
  assert.throws(() => projectApprovers(cfg, '없는과제', 1000), (e) => e.code === 'VALIDATION' && /사람에게/.test(e.message));
  assert.throws(() => projectApprovers(cfg, '스마트농장', 'abc'), (e) => e.code === 'VALIDATION');
});

test('purchase line follows lines.purchase', () => {
  assert.equal(describeLine(purchaseLine(cfg, { project: '해양로봇', amount: 50_000 })), '합의 정담당 → 결재 김대표');
});

test('claim line from config, CEO by default', () => {
  assert.deepEqual(claimLine(cfg), [{ name: '김대표', type: '결재' }]);
  assert.deepEqual(claimLine(parseConfig({ people: { ceo: '김대표' } })), [{ name: '김대표', type: '결재' }]);
});

test('inspection line: CTO final, drafter alone when the drafter is the CTO', () => {
  assert.deepEqual(inspectionLine(cfg, '박기술'), []);
  assert.deepEqual(inspectionLine(cfg, '홍길동'), [{ name: '박기술', type: '결재' }]);
  assert.equal(describeLine(inspectionLine(cfg, '박기술')), '기안자 단독 (결재자 없음)');
});

test('inspection line set to project puts everyone in as 결재', () => {
  const c = parseConfig({ ...cfg, lines: { inspection: 'project' } });
  assert.equal(describeLine(inspectionLine(c, '홍길동', { project: '스마트농장', amount: 1_200_000 })),
    '결재 최담당 → 결재 이재무 → 결재 김대표');
});

test('trip line resolves roles and keeps the referer department', () => {
  assert.deepEqual(tripLine(cfg), {
    approvers: [{ name: '김대표', type: '결재' }],
    referers: [{ name: '한참조', dept: '경영지원팀' }],
  });
});

test('missing people in config are reported, not invented', () => {
  const c = parseConfig({});
  assert.throws(() => claimLine(c), (e) => e.code === 'VALIDATION' && /ceo/.test(e.message));
  assert.throws(() => resolveEntry(c, { type: '결재' }), (e) => e.code === 'VALIDATION');
  assert.throws(() => resolveEntry(cfg, { name: '누구', type: '참조' }), (e) => e.code === 'VALIDATION');
});
