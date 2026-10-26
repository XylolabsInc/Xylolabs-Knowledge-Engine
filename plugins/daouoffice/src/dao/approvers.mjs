import { PluginError } from '../errors.mjs';

/**
 * 결재선(기안자 뒤). 이름은 저장소에 두지 않고 회사 설정(`DAOUOFFICE_CONFIG`)에서 읽는다.
 *
 *   project    합의 과제 담당자 → (cfoFrom 원 이상이면 합의 CFO) → 결재 CEO
 *              사업비 카드의 지출결의서, 검수조서, 구매 품의서가 쓰던 결재선이다
 *   ctoFinal   CTO 전결. 기안자가 CTO 면 결재자 없이(기안자 단독) 올라가 바로 완료된다
 *   [ ... ]    설정에 적은 그대로. `{ name }` 은 이름, `{ role: "ceo" }` 는 `people` 의 사람
 *
 * 과제의 합의 담당자가 설정에 없으면 지어내지 않고 멈춘다. 사람에게 묻는다.
 * 결재 순서는 배열 순서다. `setApprovalLine()` 이 이름, 타입, 순서를 그대로 대조한다.
 */

const TYPES = ['결재', '합의', '확인'];

function person(cfg, role) {
  const name = cfg.people?.[role];
  if (!name) throw new PluginError('VALIDATION', `회사 설정에 ${role} 가 없습니다. 관리자에게 설정을 요청해 주세요.`);
  return name;
}

/** `{ name }` 또는 `{ role }` 한 줄을 `{ name, type, dept? }` 로 푼다. */
export function resolveEntry(cfg, entry) {
  const name = entry.name ?? (entry.role ? person(cfg, entry.role) : null);
  if (!name) throw new PluginError('VALIDATION', '결재선 설정에 이름이 없는 줄이 있습니다.');
  const type = entry.type ?? '결재';
  if (!TYPES.includes(type)) throw new PluginError('VALIDATION', `결재 타입은 ${TYPES.join(', ')} 중 하나입니다: ${type}`);
  return { name, type, ...(entry.dept ? { dept: entry.dept } : {}) };
}

/** 과제와 금액으로 결재선을 만든다. */
export function projectApprovers(cfg, project, amount) {
  const reviewer = cfg.projects?.[project]?.reviewer;
  if (!reviewer) {
    throw new PluginError('VALIDATION', `과제 "${project}" 의 합의 담당자를 모릅니다. 사람에게 물어 회사 설정에 넣어야 합니다.`);
  }
  if (!Number.isFinite(Number(amount))) throw new PluginError('VALIDATION', `금액이 없습니다: ${amount}`);
  const cfoFrom = Number(cfg.cfoFrom ?? Infinity);
  return [
    { name: reviewer, type: '합의' },
    ...(Number(amount) >= cfoFrom ? [{ name: person(cfg, 'cfo'), type: '합의' }] : []),
    { name: person(cfg, 'ceo'), type: '결재' },
  ];
}

/** 설정값 하나(`"project"`, `"ctoFinal"`, 배열)를 결재선으로 푼다. */
function lineFrom(cfg, spec, { project, amount, drafterName, allApprove = false } = {}) {
  if (Array.isArray(spec)) return spec.map((e) => resolveEntry(cfg, e));
  if (spec === 'project') {
    const line = projectApprovers(cfg, project, amount);
    return allApprove ? line.map((a) => ({ ...a, type: '결재' })) : line;
  }
  if (spec === 'ctoFinal') {
    const cto = person(cfg, 'cto');
    return drafterName === cto ? [] : [{ name: cto, type: '결재' }];
  }
  throw new PluginError('VALIDATION', `모르는 결재선 설정입니다: ${JSON.stringify(spec)}`);
}

/** 구매 품의서. 기본은 `project`. */
export function purchaseLine(cfg, { project, amount }) {
  return lineFrom(cfg, cfg.lines?.purchase ?? 'project', { project, amount });
}

/** 법인카드 지출결의서. 기본은 대표이사 결재. */
export function claimLine(cfg, { project, amount } = {}) {
  return lineFrom(cfg, cfg.lines?.claim ?? [{ role: 'ceo', type: '결재' }], { project, amount });
}

/**
 * 검수조서. 기본은 CTO 전결. `project` 로 설정하면 검수조서 양식에는 타입이 `결재` 하나뿐이라
 * 같은 순서로 모두 결재로 넣는다.
 */
export function inspectionLine(cfg, drafterName, { project, amount } = {}) {
  return lineFrom(cfg, cfg.lines?.inspection ?? 'ctoFinal', { project, amount, drafterName, allApprove: true });
}

/** 출장 결재선과 참조자. `{ approvers, referers }` */
export function tripLine(cfg) {
  return {
    approvers: (cfg.trip?.approvers ?? [{ role: 'ceo', type: '결재' }]).map((e) => resolveEntry(cfg, e)),
    referers: (cfg.trip?.referers ?? []).map((e) => {
      const r = resolveEntry(cfg, e);
      return { name: r.name, ...(r.dept ? { dept: r.dept } : {}) };
    }),
  };
}

/** `합의 홍길동 → 결재 김철수` 꼴. 로그와 대조, 미리보기에 쓴다. 비어 있으면 기안자 단독이다. */
export const describeLine = (approvers) => (approvers.length
  ? approvers.map((a) => `${a.type} ${a.name}`).join(' → ')
  : '기안자 단독 (결재자 없음)');
