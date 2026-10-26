import fs from 'node:fs';
import path from 'node:path';
import { createHash } from 'node:crypto';
import { PluginError } from '../errors.mjs';
import { kstIso } from './kst.mjs';
import { RECEIPT_LIKE } from './purchases.mjs';

/**
 * 구매 품의서에 붙일 **상품 페이지 스크린샷**(견적서 대용). 원본 저장소 `quote-shots.mjs` 와
 * `quote-verify.mjs` 를 옮겼다.
 *
 * 품의서는 구매 **전** 문서라 영수증과 주문내역은 붙이지 않는다. 상품명, 가격, 판매자가 보이는
 * 상품 페이지를 붙인다.
 *
 * 바뀐 것:
 *
 * - 원본은 macOS 의 평범한 크롬과 원시 CDP 로 쿠팡, 네이버를 찍었다. 서버에는 화면도 크롬도
 *   없으므로 공유 Chromium 의 **빈 컨텍스트**(다우오피스 쿠키도 로그인도 없다)에서 헤드리스로만
 *   찍는다. 쿠팡과 네이버는 헤드리스를 자주 막는다. 막히면 그렇다고 알리고, 제조사 상품 페이지나
 *   다나와 검색 페이지를 대신 찍거나 사람이 찍은 화면을 받는다(원본의 "참고 캡처" 규칙).
 * - 원본의 두 번째 검사(사람이 사진을 열어 보고 `--ok`)는 `submit_purchase` 미리보기에 캡처를
 *   보여 주고 사람이 승인하는 것으로 갈음한다. 미리보기 뒤에 파일이 바뀌면 상신하지 않는다.
 * - 파일 이름 끝에 내용 해시를 붙인다. 다시 찍으면 이름이 바뀌어, 상신취소한 문서에 남은 옛
 *   캡처가 새 캡처로 오인되지 않는다(원본은 `--reattach` 로 풀었다).
 *
 * 기계 검사(저장 전에 한다): 주소가 로그인이나 오류 화면으로 넘어가지 않았는지, 캡차나 차단
 * 화면이 아닌지, 상품이 없는 페이지가 아닌지, `expect` 문구가 있는지, 고른 옵션이 선택됐는지,
 * 가리는 팝업이 없는지.
 */

/** 견적으로 붙이면 안 되는 페이지(주문, 영수증). 주소 경로와 제목으로 본다. */
const ORDER_URL = /\/(orders?|order-?detail|orderdetail|myorders?|trade\/order|invoices?|receipts?|purchase-?history)(\/|\.|\?|$)|[?&](orderId|order_id|invoiceId)=/i;
const ORDER_TITLE = /주문\s*상세|주문\s*내역|주문\s*조회|구매\s*내역|영수증|거래명세|Order details|Order history|Your orders|Invoice|Receipt/i;

const LOGIN_URL = /\/(login|signin|sign-in|auth\/login)(\/|\?|$)|accounts\.|nid\.naver\.com|login\.coupang\.com/i;
const LOGIN_TEXT = /로그인이 필요|로그인 후 (이용|확인)|Please (log|sign) in to continue|Sign in to continue/i;
const CAPTCHA_URL = /captcha|nid\.naver\.com\/user2\/help|\/challenge|cdn-cgi\/challenge/i;
// 바닥글의 "protected by reCAPTCHA" 같은 문구에 걸리지 않게 `captcha` 낱말 하나로는 보지 않는다.
const CAPTCHA_TEXT = /complete the security verification|보안 확인|자동입력 방지|Verify you are human|사람인지 확인|Checking your browser|Just a moment\.\.\.|slide to verify|밀어서 인증|unusual traffic/i;
const BLOCKED_TEXT = /Access Denied|접속이 일시적으로 제한|시스템오류|Request blocked|You don't have permission to access|403 Forbidden|비정상적인 접근/i;
const MISSING_TEXT = /currently unavailable in your location|Sorry, this item|찾을 수 없습니다|페이지를 찾을 수 없|판매가 종료|This item is no longer available/i;

/** 막힌 사이트에 대해 사람에게 줄 대안. 원본 문서에서 실제로 통한 것들이다. */
export function alternativeHint(url) {
  let host = '';
  try { host = new URL(url).hostname; } catch { /* 주소가 아니면 일반 안내 */ }
  if (/coupang\.com$/.test(host)) {
    return '쿠팡은 헤드리스 캡처를 막습니다. 다나와 검색 페이지(https://search.danawa.com/dsearch.php?query=상품명)나 제조사 상품 페이지를 대신 찍거나, 사람이 찍은 화면을 보내 주세요.';
  }
  if (/naver\.com$/.test(host)) {
    return '네이버 스마트스토어는 헤드리스 캡처를 막고 로그인을 요구합니다. 제조사나 공식 판매처의 상품 소개 페이지를 대신 찍거나, 사람이 찍은 화면을 보내 주세요. 실제 판매처 주소와 옵션, 금액은 기타 칸에 적습니다.';
  }
  if (/aliexpress\./.test(host)) {
    return '알리익스프레스 봇 검사가 떴습니다. 잠시 뒤 다시 찍거나, 같은 상품의 다른 공개 페이지를 찍거나, 사람이 찍은 화면을 보내 주세요.';
  }
  return '제조사 상품 페이지나 다나와 검색 페이지를 대신 찍거나, 사람이 찍은 화면을 보내 주세요.';
}

/**
 * 견적으로 쓸 수 없는 화면인지 본다. 문제가 있으면 `{ kind, reason }`, 없으면 null.
 * 브라우저 없이 시험할 수 있게 글자만 받는다. `status` 는 HTTP 응답 코드(모르면 생략).
 *
 * kind: `receipt` | `login` | `captcha` | `blocked` | `missing`
 */
export function detectBlock({ url = '', landedUrl = '', title = '', text = '', status } = {}) {
  const where = landedUrl || url;
  const t = String(text ?? '').replace(/\s+/g, ' ');
  const hint = alternativeHint(url || where);
  if (ORDER_URL.test(url) || ORDER_URL.test(where) || ORDER_TITLE.test(title)) {
    return { kind: 'receipt', reason: '주문내역이나 영수증 페이지로 보입니다. 구매 품의서에는 영수증과 주문내역을 붙이지 않습니다. 상품 페이지를 찍어 주세요.' };
  }
  if (CAPTCHA_URL.test(where) || CAPTCHA_TEXT.test(`${title} ${t.slice(0, 4000)}`)) {
    return { kind: 'captcha', reason: `보안 확인(캡차) 화면이 떴습니다. 자동으로 풀지 않습니다. ${hint}` };
  }
  // 상품 페이지에도 "로그인 후 확인 가능한 쿠폰" 같은 문구가 있다. 글자가 적은 화면(로그인 벽)만 본문으로 가린다.
  if (LOGIN_URL.test(where) || /^(로그인|Sign in|Log in)(\s|$)/i.test(String(title).trim()) || (t.length < 2500 && LOGIN_TEXT.test(t))) {
    return { kind: 'login', reason: `로그인 화면으로 넘어갔습니다. 견적 캡처는 로그인하지 않습니다. ${hint}` };
  }
  if ((status && status >= 400 && status !== 404 && status !== 410) || BLOCKED_TEXT.test(`${title} ${t.slice(0, 4000)}`)) {
    return { kind: 'blocked', reason: `사이트가 자동 접속을 막았습니다${status >= 400 ? ` (HTTP ${status})` : ''}. ${hint}` };
  }
  if (status === 404 || status === 410 || MISSING_TEXT.test(t.slice(0, 6000))) {
    return { kind: 'missing', reason: '상품이 없는 페이지입니다(지역 제한, 판매 종료, 404). 주소를 확인해 주세요.' };
  }
  return null;
}

/** `expect` 가운데 페이지에 없는 첫 문구. 모두 있으면 null. 대소문자는 가리지 않는다. */
export function missingExpect(text, expect = []) {
  const t = String(text ?? '').toLowerCase();
  return expect.find((want) => !t.includes(String(want).toLowerCase())) ?? null;
}

/** 견적 파일 이름. `<이름>-<해시 앞 8자리>.<확장자>`. 영수증처럼 보이는 이름은 거부한다. */
export function quoteFileName({ label, url, digest, ext = 'png' }) {
  let base = String(label ?? '').trim();
  if (!base) {
    try { base = new URL(url).hostname.replace(/^www\./, ''); } catch { base = 'quote'; }
  }
  base = base.normalize('NFC').replace(/[^0-9A-Za-z가-힣._-]+/g, '_').replace(/^[._-]+|[._-]+$/g, '').slice(0, 60) || 'quote';
  if (RECEIPT_LIKE.test(base)) {
    throw new PluginError('VALIDATION', `영수증이나 주문내역으로 보이는 이름은 견적으로 쓰지 않습니다: ${base}`);
  }
  return `${base}-${String(digest).slice(0, 8)}.${ext}`;
}

export const sha256 = (buf) => createHash('sha256').update(buf).digest('hex');

const safeHost = (url) => { try { return new URL(url).hostname.replace(/^\[|\]$/g, ''); } catch { return ''; } };

/**
 * 견적 파일을 저장하고 `quotes` 에 남긴다. 같은 내용이면 같은 이름이라 한 번만 저장된다.
 * 돌려주는 값은 DB 행이다.
 */
export function storeQuote({ db, owner, dir, purchaseId, buffer, label, url, ext, data = {} }) {
  const digest = sha256(buffer);
  const file = quoteFileName({ label, url, digest, ext });
  fs.mkdirSync(dir, { recursive: true });
  const full = path.join(dir, file);
  if (!fs.existsSync(full)) fs.writeFileSync(full, buffer);
  return db.addQuote({ owner, purchaseId, file, url: url ?? null, capturedAt: kstIso(), digest, seenAt: null, data });
}

/**
 * 붙일 견적 파일이 그대로 있는지 본다. 없거나 내용이 기록과 다르면 문제 목록에 넣는다.
 * `previewed` 를 주면 미리보기에서 보여 준 해시와도 맞춘다.
 */
export function checkQuoteFiles(dir, quotes, { previewed } = {}) {
  const problems = [];
  for (const q of quotes) {
    const full = path.join(dir, q.file);
    let buf;
    try { buf = fs.readFileSync(full); } catch {
      problems.push(`견적 파일이 없습니다: ${q.file}`);
      continue;
    }
    if (q.digest && sha256(buf) !== q.digest) problems.push(`견적 파일이 기록과 다릅니다: ${q.file}`);
    if (previewed && previewed[q.file] !== q.digest) problems.push(`미리보기 뒤에 견적이 바뀌었습니다: ${q.file}`);
  }
  return problems;
}

// ------------------------------------------------------------------ capture

/** 알리는 한국어, 원화가 기본이라 주문서(영어, 달러)와 표기가 어긋난다. 탭을 열기 전에 쿠키로 맞춘다. */
export const ALI_COOKIES = {
  USD: [{ name: 'aep_usuc_f', value: 'site=glo&c_tp=USD&region=KR&b_locale=en_US', domain: '.aliexpress.com', path: '/' }],
  KRW: [{ name: 'aep_usuc_f', value: 'site=kor&c_tp=KRW&region=KR&b_locale=ko_KR', domain: '.aliexpress.com', path: '/' }],
};

/**
 * 캡처용 컨텍스트 설정. 헤드리스 표시(`HeadlessChrome`)가 든 UA 를 막는 상점이 많아 실제 버전의
 * 일반 크롬 UA 로 바꾼다. `withGuardedContext` 의 `contextOptions` 로 넘긴다(프록시는 그쪽이 붙인다).
 */
export function captureContextOptions(browser) {
  const version = browser?.version?.() ?? '';
  const major = String(version).split('.')[0];
  const ua = major
    ? `Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/${major}.0.0.0 Safari/537.36`
    : undefined;
  return {
    viewport: { width: 1280, height: 1100 },
    locale: 'ko-KR',
    timezoneId: 'Asia/Seoul',
    serviceWorkers: 'block',
    ...(ua ? { userAgent: ua } : {}),
  };
}

/**
 * 한 페이지를 찍는다. `{ ok, reason, kind?, landedUrl, title, shot? }`.
 *
 * quote = { url, expect: [문자열...], options: [버튼 글자...], waitMs, noClose, hideText, decline, aliRegion }
 * guard = `withGuardedContext` 가 넘겨 주는 검사 프록시. 없으면 찍지 않는다.
 *
 * `expect` 없이 찍으면 엉뚱한 페이지를 저장하고도 성공으로 보고한다. 그래서 필수다.
 */
export async function captureQuote(page, quote, { guard } = {}) {
  if (!guard?.proxy) throw new PluginError('INTERNAL', '검사 프록시 없이 공개 페이지를 열지 않습니다.');
  if (!(quote.expect ?? []).length) {
    return { ok: false, kind: 'input', reason: 'expect 가 없습니다. 주문한 판매자나 상품명처럼 페이지에 반드시 있어야 할 글자를 넣어 주세요.' };
  }
  const pre = detectBlock({ url: quote.url });
  if (pre) return { ok: false, ...pre, landedUrl: quote.url, title: '' };
  // 막는 것은 프록시다. 여기서는 사람에게 이유를 먼저 알린다.
  const why = await guard.reason(quote.url);
  if (why) return { ok: false, kind: 'private', reason: `공개된 상품 페이지만 찍습니다: ${why}.`, landedUrl: quote.url, title: '' };
  const privateFail = () => (guard.blocked.length
    ? { ok: false, kind: 'private', reason: `공개된 상품 페이지만 찍습니다: 내부망으로 가는 요청을 막았습니다 (${guard.blocked[0].reason}).`, landedUrl: page.url(), title: '' }
    : null);

  if (/aliexpress\./i.test(quote.url)) {
    await page.context().addCookies(quote.aliRegion === 'KR' ? ALI_COOKIES.KRW : ALI_COOKIES.USD).catch(() => {});
  }
  await page.context().addInitScript(() => Object.defineProperty(navigator, 'webdriver', { get: () => undefined })).catch(() => {});

  let status;
  try {
    const res = await page.goto(quote.url, { waitUntil: 'domcontentloaded', timeout: 60_000 });
    status = res?.status();
  } catch (e) {
    return privateFail()
      ?? { ok: false, kind: 'blocked', reason: `페이지를 열지 못했습니다: ${String(e?.message ?? e).split('\n')[0].slice(0, 120)}`, landedUrl: quote.url, title: '' };
  }
  // 첫 문서가 리다이렉트로 내부망을 가리켰으면 프록시가 막았다.
  if (status === 403 && guard.blocked.some((b) => b.host === safeHost(page.url()))) return privateFail();
  await page.waitForTimeout(quote.waitMs ?? 5000);

  const look = async () => ({
    landedUrl: page.url(),
    title: await page.title().catch(() => ''),
    text: await page.evaluate(() => document.body?.innerText ?? '').catch(() => ''),
  });

  // 막힌 화면에서 팝업을 닫거나 옵션을 누르지 않는다. 먼저 본다.
  const first = await look();
  const blocked = detectBlock({ url: quote.url, ...first, status });
  if (blocked) return { ok: false, ...blocked, landedUrl: first.landedUrl, title: first.title };

  // `noClose`: 닫기 처리가 오히려 무언가를 연다(숨은 검색창의 Close 버튼을 누르면 검색창이 열리는 사이트).
  if (!quote.noClose) await closePopups(page);
  const bad = await pickOptions(page, quote);
  if (bad) return { ok: false, kind: 'option', reason: bad, landedUrl: page.url(), title: first.title };
  if (quote.decline) {
    await page.evaluate((label) => [...document.querySelectorAll('button, a')]
      .find((e) => (e.innerText || '').trim() === label && e.getBoundingClientRect().width > 0)?.click(), quote.decline).catch(() => {});
    await page.waitForTimeout(1500);
  }
  await page.evaluate(() => window.scrollTo(0, 0)).catch(() => {});
  if (!quote.noClose) await closePopups(page);

  const seen = await look();
  const late = detectBlock({ url: quote.url, ...seen, status });
  if (late) return { ok: false, ...late, landedUrl: seen.landedUrl, title: seen.title };
  const missing = missingExpect(seen.text, quote.expect);
  if (missing) {
    return { ok: false, kind: 'expect', reason: `페이지에 "${missing}" 가 없습니다. 주문한 판매자나 상품이 아닐 수 있습니다.`, landedUrl: seen.landedUrl, title: seen.title };
  }

  // `hideText`: 닫기 버튼으로 안 닫히는 떠 있는 안내 상자를 글자로 찾아 숨긴다. 고정 위치 상자만 건드린다.
  if (quote.hideText?.length) {
    await page.evaluate((texts) => {
      for (const el of document.querySelectorAll('body *')) {
        const cs = getComputedStyle(el);
        if (cs.position !== 'fixed' && cs.position !== 'sticky') continue;
        if (texts.some((t) => (el.innerText || '').includes(t))) el.style.visibility = 'hidden';
      }
    }, quote.hideText);
  }
  await page.waitForTimeout(800);
  const overlay = await coveringOverlay(page);
  if (overlay) return { ok: false, kind: 'overlay', reason: `팝업이 화면을 가립니다: ${overlay}`, landedUrl: seen.landedUrl, title: seen.title };

  const shot = await page.screenshot({ type: 'png' });
  return {
    ok: true,
    reason: '저장했습니다.',
    landedUrl: seen.landedUrl,
    title: seen.title,
    priceSeen: /US \$|USD|원|₩|£|€/.test(seen.text),
    textLength: seen.text.length,
    shot,
  };
}

/**
 * 주문한 옵션을 고른다. 두 모양이다. Shopify 는 글자 버튼, 알리는 `[data-sku-col]` 썸네일이고 이름이
 * img alt 나 title 에 있다. 고른 뒤 선택 표시가 붙었는지 본다. 문제가 있으면 이유를, 없으면 null.
 * 옵션을 안 고르면 기본 옵션이 찍혀 주문과 다른 사진이 된다.
 */
async function pickOptions(page, quote) {
  for (const label of quote.options ?? []) {
    const picked = await page.evaluate((label) => {
      const shown = (el) => el.getBoundingClientRect().width > 0;
      const sku = [...document.querySelectorAll('[data-sku-col]')].filter(shown);
      const name = (el) => el.querySelector('img')?.getAttribute('alt') || el.getAttribute('title') || (el.innerText || '').trim();
      if (sku.length) {
        const hit = sku.find((el) => name(el) === label) ?? sku.find((el) => name(el).includes(label));
        if (!hit) return { ok: false, available: sku.map(name) };
        if (!/selected/.test(hit.className)) hit.click();
        return { ok: true, sku: true };
      }
      const btn = [...document.querySelectorAll('label, button, [role="radio"]')].filter(shown)
        .find((el) => (el.innerText || '').trim() === label)
        ?? [...document.querySelectorAll('label, button, [role="radio"]')].filter(shown)
          .find((el) => (el.innerText || '').trim().includes(label));
      if (!btn) return { ok: false, available: [] };
      btn.click();
      return { ok: true };
    }, label);
    if (!picked.ok) {
      return `옵션 "${label}" 을 찾지 못했습니다${picked.available.length ? ` (있는 것: ${picked.available.slice(0, 12).join(', ')})` : ''}`;
    }
    await page.waitForTimeout(1500);
    if (picked.sku) {
      const selected = await page.evaluate((label) => [...document.querySelectorAll('[data-sku-col]')]
        .some((el) => /selected/.test(el.className)
          && ((el.querySelector('img')?.getAttribute('alt') || el.getAttribute('title') || (el.innerText || '').trim()) || '').includes(label)), label);
      if (!selected) return `옵션 "${label}" 을 눌렀는데 선택되지 않았습니다`;
    }
  }
  return null;
}

/** 쿠폰, 쿠키 동의, 알림 등록, 챗봇 같은 팝업을 닫는다. 상품 정보는 건드리지 않는다. */
async function closePopups(page) {
  for (let i = 0; i < 3; i++) {
    await page.keyboard.press('Escape').catch(() => {});
    await page.waitForTimeout(400);
    await page.evaluate(() => {
      const shown = (el) => { const r = el.getBoundingClientRect(); return r.width > 0 && r.height > 0; };
      const closers = [...document.querySelectorAll(
        '[aria-label*="close" i], [aria-label*="닫기"], button[class*="close" i], a[class*="close" i], .needsclick button,'
        // OneTrust 쿠키 동의 배너. 기업 사이트가 널리 쓴다.
        + ' #onetrust-reject-all-handler, .ot-pc-refuse-all-handler, #onetrust-accept-btn-handler,'
        + ' #onetrust-close-btn-container button,'
        // 구독 유도 팝업. 닫기 아이콘에 라벨이 없어 컨테이너로 잡는다.
        + ' .wp-optin-dialog-container [class*="close"], .wp-optin-dialog-container svg',
      )].filter(shown);
      // 닫기 아이콘 대신 거절 버튼만 있는 팝업이 있다(알림 등록, 쿠키 안내).
      const decline = [...document.querySelectorAll('button, a, span, div')].filter(shown)
        .filter((el) => el.children.length === 0 && /^(설정 안 함|나중에|닫기|No thanks|Not now|Maybe later|Don['’]t allow|Allow Cookies|모두 허용)$/i.test((el.innerText || '').trim()));
      for (const c of [...closers, ...decline]) c.click();
      // 쿠키 안내가 shadow DOM 안에 있어 위 검색에 안 잡히는 사이트가 있다(다나와).
      for (const host of document.querySelectorAll('*')) {
        const root = host.shadowRoot;
        if (!root) continue;
        for (const b of root.querySelectorAll('button, a')) {
          if (/^(Allow Cookies|모두 허용|동의|Close|닫기)$/i.test((b.innerText || b.textContent || '').trim())) b.click();
        }
      }
      // 챗봇 말풍선은 iframe 안에 있어 닫기 버튼을 못 누른다. 떠 있는 iframe 위젯만 숨긴다.
      for (const el of document.querySelectorAll('body *')) {
        const cs = getComputedStyle(el);
        if (cs.position !== 'fixed' || !el.querySelector('iframe')) continue;
        const r = el.getBoundingClientRect();
        if (r.width > 0 && r.width < innerWidth * 0.9) el.style.visibility = 'hidden';
      }
    }).catch(() => {});
    await page.waitForTimeout(600);
  }
}

/**
 * 화면을 가리는 팝업이 있는지. 있으면 그 설명을 돌려준다.
 *
 * 두 경우를 본다. 화면 절반 이상을 덮는 레이어(쿠폰 팝업, 어두운 배경)와, 작더라도 글자가 든
 * **떠 있는 상자**(알림 등록 안내). 절반 기준만 봤을 때 뒤쪽이 상품명을 가린 채 통과했다.
 * 전체 폭 헤더, 하단 고정 바처럼 원래 페이지에 붙어 있는 것은 제외한다.
 */
export async function coveringOverlay(page) {
  return page.evaluate(() => {
    const vw = innerWidth; const vh = innerHeight;
    for (const el of document.querySelectorAll('body *')) {
      const cs = getComputedStyle(el);
      if (cs.position !== 'fixed' || cs.visibility === 'hidden' || cs.display === 'none' || Number(cs.opacity) === 0) continue;
      // 화면 낭독기용 숨은 글자는 보이지 않는다. 잘라 낸 상자는 건너뛴다.
      if (/\b(blind|sr-only|screen_out|visually-hidden|hidden-text)\b/.test(String(el.className))) continue;
      if (/^rect\(0/.test(cs.clip) || /inset\(50%/.test(cs.clipPath)) continue;
      const r = el.getBoundingClientRect();
      const w = Math.max(0, Math.min(r.right, vw) - Math.max(r.left, 0));
      const h = Math.max(0, Math.min(r.bottom, vh) - Math.max(r.top, 0));
      const area = (w * h) / (vw * vh);
      const text = (el.innerText || '').replace(/\s+/g, ' ').trim();
      const desc = `${el.tagName}.${String(el.className).slice(0, 40)} "${text.slice(0, 60)}"`;
      if (area >= 0.5 && (text || cs.backgroundColor !== 'rgba(0, 0, 0, 0)')) return desc;
      // 글자가 든 떠 있는 상자, 또는 iframe 을 품은 떠 있는 상자(챗봇 위젯)
      const floating = w < vw * 0.9 && area >= 0.03 && (text.length >= 8 || !!el.querySelector('iframe'));
      if (floating) return desc;
    }
    return null;
  });
}
