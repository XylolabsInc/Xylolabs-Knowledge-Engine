import { PluginError } from '../errors.mjs';

/**
 * 다우오피스 로그인. 원본 저장소 `session.mjs` 의 `loginOnPage()` 에서 왔다.
 * 바뀐 것은 자격증명을 인자로 받는 것, 고정 대기 대신 조건 대기, 실패를 코드로 나누는 것이다.
 *
 * 로그인 화면은 `/login?nextUrl=%2F`, 요청은 `POST /api/portal/public/auth/login`, 성공하면
 * `/home` 으로 간다. 캡차와 2FA 는 없다.
 *
 * **계정당 세션 하나.** 같은 계정이 다른 곳에 접속해 있으면 비밀번호가 맞은 뒤에 "동시 접근
 * 제한" 모달이 뜨고 `기존 접속 종료` 버튼 하나만 준다. 누르면 다른 곳의 세션이 끊긴다.
 * 사람이 허락하지 않았으면 누르지 않는다.
 *
 *   onDuplicate: 'fail'    DUPLICATE_SESSION 으로 멈춘다 (기본)
 *                'kick'    `기존 접속 종료` 를 누른다 (사람이 허락했을 때만)
 *                'report'  누르지 않고 { status: 'duplicate' } 를 돌려준다. 모달은 비밀번호가
 *                          맞아야 뜨므로 계정 연결 확인에는 이것으로 충분하다
 *
 * 비밀번호가 틀리면 AUTH_FAILED 로 멈추고 다시 시도하지 않는다. 계정이 잠길 수 있다.
 * 호출한 쪽이 실패한 컨텍스트를 닫는다.
 */

export const LOGIN_PATH = '/login?nextUrl=%2F';
export const LOGIN_API = '/api/portal/public/auth/login';
export const KICK_BUTTON = '기존 접속 종료';

const SELECTORS = {
  username: 'input.input_txt[type="text"]',
  password: 'input.input_txt.password',
};

const onLoginPage = (url) => {
  try { return new URL(url).pathname.startsWith('/login'); } catch { return true; }
};

/** 보이는 `기존 접속 종료` 버튼이 있는가. */
async function duplicateShown(page) {
  const btn = page.getByRole('button', { name: KICK_BUTTON });
  return (await btn.count()) > 0 && (await btn.first().isVisible().catch(() => false));
}

/**
 * @returns {Promise<{ status: 'ok' | 'already' | 'duplicate', kicked: boolean }>}
 */
export async function login(page, baseUrl, { username, password }, { onDuplicate = 'fail', timeoutMs = 30_000 } = {}) {
  if (!username || !password) throw new PluginError('AUTH_FAILED', '아이디와 비밀번호가 모두 필요합니다.');

  try {
    await page.goto(`${baseUrl}${LOGIN_PATH}`, { waitUntil: 'domcontentloaded', timeout: 60_000 });
  } catch (cause) {
    throw new PluginError('UPSTREAM', '다우오피스 로그인 화면을 열지 못했습니다. 네트워크를 확인해 주세요.', { cause });
  }

  // 폼이 그려지거나 이미 로그인돼 다른 화면으로 넘어갈 때까지 기다린다(원본은 2초 고정 대기).
  await page.waitForFunction(
    (sel) => !location.pathname.startsWith('/login') || Boolean(document.querySelector(sel)),
    SELECTORS.password, { timeout: 20_000, polling: 250 },
  ).catch(() => {});
  if (!onLoginPage(page.url())) return { status: 'already', kicked: false };
  if (!(await page.locator(SELECTORS.password).count())) {
    throw new PluginError('UPSTREAM', '다우오피스 로그인 화면이 열리지 않았습니다.');
  }

  // 오류 메시지에 입력값이 섞이지 않게 원래 예외는 원인으로만 남긴다.
  try {
    await page.fill(SELECTORS.username, username);
    await page.fill(SELECTORS.password, password);
  } catch (cause) {
    throw new PluginError('UPSTREAM', '다우오피스 로그인 폼에 입력하지 못했습니다.', { cause });
  }

  const answered = page.waitForResponse(
    (r) => r.request().method() === 'POST' && new URL(r.url()).pathname === LOGIN_API,
    { timeout: timeoutMs },
  ).catch(() => null);
  try {
    await page.getByRole('button', { name: '로그인', exact: true }).click({ timeout: 10_000 });
  } catch (cause) {
    throw new PluginError('UPSTREAM', '다우오피스 로그인 버튼을 누르지 못했습니다.', { cause });
  }
  const response = await answered;

  // 결과 셋 중 하나를 기다린다: 화면이 넘어감, 동시 접근 제한 모달, 아무 변화 없음(실패).
  // 서버가 4xx 로 거절했으면 오래 기다릴 것이 없다.
  const status = response?.status() ?? 0;
  const settleMs = !response ? 5_000 : status >= 400 && status < 500 ? 3_000 : 15_000;
  await page.waitForFunction(
    (label) => !location.pathname.startsWith('/login')
      || [...document.querySelectorAll('button')].some((b) => (b.innerText || '').trim() === label && b.offsetParent !== null),
    KICK_BUTTON, { timeout: settleMs, polling: 250 },
  ).catch(() => {});

  if (await duplicateShown(page)) {
    if (onDuplicate === 'report') return { status: 'duplicate', kicked: false };
    if (onDuplicate !== 'kick') {
      throw new PluginError('DUPLICATE_SESSION', '이 계정이 다른 곳에서 접속 중입니다. 봇이 로그인하면 그 접속이 끊깁니다.');
    }
    await page.getByRole('button', { name: KICK_BUTTON }).first().click({ timeout: 10_000 });
    const left = await page.waitForURL((u) => !u.pathname.startsWith('/login'), { timeout: 30_000 })
      .then(() => true, () => false);
    if (!left) throw new PluginError('UPSTREAM', '기존 접속을 끊은 뒤 다우오피스 화면으로 넘어가지 않았습니다.');
    await page.waitForLoadState('networkidle', { timeout: 15_000 }).catch(() => {});
    return { status: 'ok', kicked: true };
  }

  if (onLoginPage(page.url())) {
    // 로그인 요청에 서버가 답했는데도 로그인 화면에 머물러 있으면 자격증명이 틀린 것으로 본다.
    // 답이 없거나 5xx 면 네트워크나 서버 문제이므로 계정을 무효로 만들지 않는다.
    // UNVERIFIED: shape not observed — the wrong-password response (status, body, on-screen message) has not been captured.
    if (!response) throw new PluginError('UPSTREAM', '다우오피스가 로그인 요청에 응답하지 않았습니다.');
    if (status >= 500) {
      throw new PluginError('UPSTREAM', `다우오피스 로그인 서버 오류입니다 (HTTP ${status}).`);
    }
    throw new PluginError('AUTH_FAILED', '다우오피스 아이디나 비밀번호가 맞지 않습니다. 계정을 다시 연결해 주세요.');
  }

  await page.waitForLoadState('networkidle', { timeout: 15_000 }).catch(() => {});
  return { status: 'ok', kicked: false };
}
