import { PluginError } from '../errors.mjs';

/**
 * 결재 양식 트리 조회.
 *
 * `/gw/api/approval/apprform/tree` 는 jsTree 포맷을 돌려준다. 노드 정보가
 * `data.title`, `metadata.id`, `attr.rel` 에 흩어져 있어 그대로 쓰기 불편하므로
 * 평평한 목록으로 정리해 준다. 폴더를 펼치려면 `?folderId={id}`.
 *
 * `GET /gw/api/approval/form`, `/forms`, `/form/list` 는 모두 404 다.
 */

const nodesOf = (res) => (Array.isArray(res.data) ? res.data : (res.data?.data ?? []));

const nodeInfo = (node) => ({
  id: node.metadata?.id ?? node.attr?.nodeId,
  title: node.data?.title ?? node.metadata?.name,
  rel: node.attr?.rel,
});

/** 모든 폴더와 양식을 평평한 배열로 돌려준다. `{ id, title, rel, path }` */
export async function listForms(api) {
  const found = [];

  const walk = async (nodes, path) => {
    for (const node of nodes) {
      const { id, title, rel } = nodeInfo(node);
      const here = [...path, title];
      found.push({ id, title, rel, path: here.join(' / ') });
      if (rel === 'FOLDER' && id) {
        await walk(nodesOf(await api(`/gw/api/approval/apprform/tree?folderId=${encodeURIComponent(id)}`)), here);
      }
    }
  };

  const root = await api('/gw/api/approval/apprform/tree');
  if (!root.ok) throw new PluginError('UPSTREAM', `결재 양식 목록을 읽지 못했습니다 (HTTP ${root.status}).`);
  await walk(nodesOf(root), []);
  return found;
}

/**
 * 이름으로 양식 하나를 찾는다. 양식 ID 는 인스턴스 설정값이라 관리자가
 * 양식을 손대면 바뀌므로, ID 를 하드코딩하는 대신 이 함수를 쓴다.
 */
export async function findForm(api, title) {
  const forms = (await listForms(api)).filter((f) => f.rel === 'FORM');
  const exact = forms.filter((f) => f.title === title);
  if (exact.length === 1) return exact[0];
  if (exact.length > 1) throw new PluginError('UPSTREAM', `양식 이름이 중복됩니다: ${title} (${exact.length}건)`);

  const partial = forms.filter((f) => f.title?.includes(title));
  if (partial.length === 1) return partial[0];
  if (partial.length > 1) {
    throw new PluginError('VALIDATION', `양식 이름이 모호합니다: ${title} → ${partial.map((f) => f.title).join(', ')}`);
  }
  throw new PluginError('NOT_FOUND', `양식을 찾지 못했습니다: ${title}. 관리자가 양식 이름을 바꿨을 수 있습니다.`);
}
