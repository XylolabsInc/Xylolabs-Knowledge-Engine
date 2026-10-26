import { ensureOk } from './api.mjs';
import { PluginError } from '../errors.mjs';

/**
 * 로그인한 사람과 그 사람의 부서. 원본 저장소에 박혀 있던 기안자 이름, 기안 부서 ID,
 * 부서 코드(`DRAFT_DEPT_ID`, `DRAFT_DEPT_CODE`)를 사람마다 여기서 읽는다.
 *
 *   이름        GET /gw/api/user/session → data.name            (관측됨)
 *   부서        GET /api/portal/common/organization/tree        (사람·부서 노드, nodeType 으로 구분)
 *   부서 코드   조직도 부서 노드의 `code`. 제목에 그대로 쓴다. 대문자로 바꾸지 않는다
 *
 * 세션 응답에서 확인된 것은 `data.name` 뿐이다. 나머지 필드와 조직도의 모양은 관측하지
 * 못해 여러 이름을 차례로 본다. 못 찾으면 지어내지 않고 null 로 둔다. 부서가 필요한
 * 도구(제목, 기안 화면 주소)가 그때 사람에게 묻거나 멈춘다.
 */

const first = (...values) => values.find((v) => v !== undefined && v !== null && v !== '') ?? null;
const str = (v) => (v === null || v === undefined ? null : String(v));

/** `GET /gw/api/user/session`. 이름이 없으면 UPSTREAM. */
export async function sessionUser(api) {
  const r = ensureOk(await api('/gw/api/user/session'), '로그인한 사람 정보');
  const d = r.data?.data ?? r.data ?? {};
  const name = first(d.name, d.userName);
  if (!name) throw new PluginError('UPSTREAM', '다우오피스 세션에서 이름을 읽지 못했습니다.');
  // UNVERIFIED: shape not observed — only data.name is confirmed; id and department fields are guesses.
  const dept = Array.isArray(d.deptMembers) ? d.deptMembers[0] : (d.dept ?? d.department ?? null);
  return {
    name: String(name),
    userId: str(first(d.id, d.userId)),
    loginId: str(first(d.loginId, d.employeeId)),
    deptId: str(first(d.deptId, d.departmentId, dept?.deptId, dept?.id)),
    deptName: str(first(d.deptName, d.departmentName, dept?.deptName, dept?.name)),
  };
}

// UNVERIFIED: shape not observed — node field names (name/title, id, code, children or parent id) and the
// nodeType values for people and departments; only "nodeType tells people from departments" is documented.
const nameOf = (n) => first(n.name, n.title, n.displayName, n.data?.title, n.metadata?.name, n.text);
const idOf = (n) => str(first(n.id, n.nodeId, n.deptId, n.userId, n.metadata?.id, n.attr?.nodeId));
/** `홍길동`, `홍길동 책임`, `홍길동(연구원)` are the same person for `홍길동`. */
const samePerson = (nodeName, name) => Boolean(nodeName) && (nodeName === name || String(nodeName).split(/[\s(]/)[0] === name);
const isPerson = (n) => /user|member|person|employee|emp/i.test(String(n.nodeType ?? ''));
const isDept = (n) => /dept|depart|org|group|team|company/i.test(String(n.nodeType ?? '')) && !isPerson(n);

/** 조직도 응답 어디에 있든 `nodeType` 이 있는 객체를 부모 경로와 함께 훑는다. */
export function walkOrgNodes(value, visit, parents = []) {
  if (Array.isArray(value)) {
    for (const v of value) walkOrgNodes(v, visit, parents);
    return;
  }
  if (!value || typeof value !== 'object') return;
  const isNode = 'nodeType' in value;
  if (isNode) visit(value, parents);
  const next = isNode ? [...parents, value] : parents;
  for (const v of Object.values(value)) if (v && typeof v === 'object') walkOrgNodes(v, visit, next);
}

/**
 * 조직도에서 그 사람의 부서를 찾는다. `{ deptId, deptCode, deptName }`, 못 찾으면 값이 null.
 *
 * 동명이인이 있고 세션의 사람 ID 로도 가려지지 않으면 고르지 않는다.
 */
export function findDepartment(tree, { name, userId, deptId }) {
  const people = [];
  const depts = new Map();
  walkOrgNodes(tree, (node, parents) => {
    if (isDept(node)) depts.set(idOf(node), node);
    if (isPerson(node) && (samePerson(nameOf(node), name) || (userId && idOf(node) === userId))) {
      people.push({ node, parent: [...parents].reverse().find(isDept) ?? null });
    }
  });

  let dept = deptId ? depts.get(String(deptId)) ?? null : null;
  if (!dept) {
    const byId = userId ? people.filter((p) => idOf(p.node) === userId) : [];
    const pick = byId.length === 1 ? byId[0] : people.length === 1 ? people[0] : null;
    if (pick) {
      const ref = str(first(pick.node.deptId, pick.node.departmentId, pick.node.parentId));
      dept = pick.parent ?? (ref ? depts.get(ref) ?? null : null);
    }
  }
  if (!dept) return { deptId: deptId ? String(deptId) : null, deptCode: null, deptName: null };
  return {
    deptId: idOf(dept),
    deptCode: str(first(dept.code, dept.deptCode, dept.metadata?.code)),
    deptName: str(nameOf(dept)),
  };
}

/** 조직도 전체. 응답 모양을 모르므로 푼 값을 그대로 돌려준다. */
export async function orgTree(api) {
  // UNVERIFIED: shape not observed — whether one call returns the whole tree or only the top level.
  const r = ensureOk(await api('/api/portal/common/organization/tree'), '조직도');
  return r.data?.data ?? r.data;
}

/**
 * 이름, 사람 ID, 부서 ID·코드·이름. 조직도가 실패해도 이름만으로 돌려준다.
 * `fallbacks` 는 회사 설정의 `departments` ({ 부서이름: { code, id } }) 로, 조직도에서
 * 코드를 못 읽었을 때만 쓴다.
 */
export async function readProfile(api, { fallbacks = {} } = {}) {
  const user = await sessionUser(api);
  let dept = { deptId: user.deptId, deptCode: null, deptName: user.deptName };
  try {
    const found = findDepartment(await orgTree(api), user);
    dept = {
      deptId: found.deptId ?? dept.deptId,
      deptCode: found.deptCode,
      deptName: found.deptName ?? dept.deptName,
    };
  } catch (e) {
    if (!(e instanceof PluginError)) throw e;
  }
  const fb = dept.deptName ? fallbacks[dept.deptName] : null;
  return {
    name: user.name,
    userId: user.userId,
    deptId: dept.deptId ?? str(fb?.id),
    deptCode: dept.deptCode ?? str(fb?.code),
    deptName: dept.deptName,
  };
}
