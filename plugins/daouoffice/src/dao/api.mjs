import { assertExecute } from '../guard.mjs';
import { PluginError } from '../errors.mjs';

/**
 * 로그인된 페이지 컨텍스트 안에서 내부 JSON API 를 부른다.
 * 세션 쿠키가 자동으로 실리므로 토큰을 따로 다루지 않는다.
 *
 *   const r = await api('/gw/api/approval/todo/count');
 *   r.status, r.data      // data 는 JSON 을 푼 값, 아니면 null
 *
 * 응답 형태는 두 가지가 섞여 있다. 파서를 하나로 통일하지 않는다.
 *
 *   /gw/api/…                       { code, message, data: {…} }
 *   /api/attend, /api/leave, /eacc  { data: {…} }   (ESS 는 data.data 가 한 겹 더)
 *
 * **응답 원문(`raw`)은 돌려주지 않는다.** 메일 제목, 접속 IP, 다른 사람 이름이 섞여 있어
 * 오류 메시지나 로그로 새면 안 된다. 원본 저장소는 `raw` 앞부분을 오류에 붙였다.
 *
 * GET 이 아닌 호출은 읽기 전용으로 확인된 경로(`READ_ONLY_POSTS`)가 아니면 승인된
 * 실행(`mode=execute`)에서만 나간다. 미리보기가 실수로 서버를 바꾸지 못하게 한다.
 */
export const READ_ONLY_POSTS = [
  '/eacc/api/ess/custom/code/getList',
  '/eacc/api/ess/myExp/getCompanyCardList',
  '/eacc/api/ess/certIsu/getIssueCertList',
  '/eacc/api/ess/myhrinfo/getBasicInfo',
  '/eacc/api/ess/myhrinfo/getBasicDetailInfo',
  '/eacc/api/ess/user/getSession',
  '/api/mail/message/count',
];

const isReadOnly = (method, path) => method === 'GET'
  || READ_ONLY_POSTS.some((p) => String(path).split('?')[0] === p);

/**
 * `onUnauthorized` 는 401 을 받으면 불린다. 세션 관리자가 다음 호출에서 다시 로그인하게
 * 표시하는 데 쓴다. 세션은 로그인 기준 30분이면 끊기는 절대 만료라 작업 도중에도 끊긴다.
 */
export function makeApi(page, { onUnauthorized } = {}) {
  return async function call(path, { method = 'GET', body } = {}) {
    const m = String(method).toUpperCase();
    if (!isReadOnly(m, path)) assertExecute(`${m} ${String(path).split('?')[0]}`);
    let result;
    try {
      result = await page.evaluate(
        async ({ path, method, body }) => {
          const res = await fetch(path, {
            method,
            headers: { Accept: 'application/json', ...(body ? { 'Content-Type': 'application/json' } : {}) },
            body: body ? JSON.stringify(body) : undefined,
          });
          return { status: res.status, text: await res.text() };
        },
        { path, method: m, body },
      );
    } catch (cause) {
      // `Failed to fetch` 는 네트워크가 잠깐 끊긴 것이다. 아무것도 저장되지 않았다.
      throw new PluginError('UPSTREAM', '다우오피스에 연결하지 못했습니다. 잠시 뒤 다시 시도해 주세요.', { cause });
    }
    if (result.status === 401) onUnauthorized?.();
    let data = null;
    try { data = JSON.parse(result.text); } catch { /* HTML 이나 빈 응답 */ }
    return { status: result.status, ok: result.status >= 200 && result.status < 300, data };
  };
}

/**
 * 200 이 아니면 `UPSTREAM` 으로 멈춘다. `what` 은 무엇을 읽다 실패했는지 사람에게 보일 말이다.
 * 401 은 세션 만료다. "다른 곳에서 로그인했다" 로 단정하지 않는다(구분할 방법이 없다).
 */
export function ensureOk(res, what) {
  if (res.ok) return res;
  if (res.status === 401) {
    throw new PluginError('UPSTREAM', `${what}: 다우오피스 세션이 만료됐습니다. 다시 요청하면 다시 로그인합니다.`);
  }
  throw new PluginError('UPSTREAM', `${what}: 다우오피스가 HTTP ${res.status} 로 응답했습니다.`);
}

/**
 * 세션이 살아 있는지 본다. 응답 코드를 그대로 돌려준다.
 * 401·403 이면 만료됐거나 다른 곳에서 로그인한 것이다(어느 쪽인지 모른다).
 */
export async function sessionStatus(page) {
  return page.evaluate(async () => {
    const res = await fetch('/api/portal/common/notification/badge', { headers: { Accept: 'application/json' } });
    return res.status;
  });
}
