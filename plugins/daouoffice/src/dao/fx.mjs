import { PluginError } from '../errors.mjs';
import { kstDate, kstIso } from './kst.mjs';

/**
 * 원/달러 환율. 하나은행 고시 **매매기준율**을 네이버 금융에서 읽는다. 원본 저장소 `fx.mjs` 를
 * 옮겼다. 바뀐 것은 브라우저를 따로 띄우지 않고 공유 Chromium 의 빈 컨텍스트(다우오피스 쿠키가
 * 없다)를 쓰는 것, 받은 값을 하루 단위로 `fx_rates` 에 남기는 것이다.
 *
 * 구매 품의서의 외화 금액은 **결제 전일 때만** 이 값으로 원화 환산한다. 국제 시세가 아니라 국내
 * 은행 고시 기준을 쓴다. 품의서를 보는 사람이 확인할 수 있는 숫자다. 하루에도 여러 번 바뀌므로
 * 초안을 만들 때 받은 값을 초안에 고정하고, 미리보기와 상신이 같은 값을 쓴다.
 *
 * **이미 결제된 건은 환산하지 않는다.** 카드사 적용환율은 고시 기준율과 달라서 환산하면 카드
 * 대장과 몇백 원씩 어긋난다. 카드 승인액(`paidKrw`)을 그대로 쓰고 적용환율을 거꾸로 구해
 * `기타` 칸에 남긴다(`paidNote`).
 *
 * 네이버 금융은 서버 쪽 HTTP 요청을 막는다. 그래서 브라우저로 연다.
 */

export const FX_URL = 'https://finance.naver.com/marketindex/exchangeDetail.naver?marketindexCd=FX_USDKRW';

/**
 * 페이지 글자에서 환율을 읽는다. 못 읽으면 `null`.
 *
 *   "하나은행 고시회차 691회 … 매매기준율 기준 미국 USD 1 달러 대한민국 KRW 1,385.00 원 … 2026. 09. 17. 20:01 기준"
 */
export function parseFxText(text) {
  const t = String(text ?? '').replace(/\s+/g, ' ');
  const bank = t.match(/(하나은행|신한은행)\s*고시회차\s*(\d+)회/);
  const at = t.match(/(\d{4})\.\s*(\d{2})\.\s*(\d{2})\.\s*(\d{2}):(\d{2})\s*기준/);
  const rate = t.match(/매매기준율 기준 미국 USD 1 달러 대한민국 KRW ([\d,]+(?:\.\d+)?) 원/);
  if (!rate || !at) return null;
  const value = Number(rate[1].replace(/,/g, ''));
  if (!(value > 0)) return null;
  return {
    rate: value,
    at: `${at[1]}-${at[2]}-${at[3]} ${at[4]}:${at[5]}`,
    source: `${bank?.[1] ?? '하나은행'} 매매기준율${bank ? ` (고시 ${bank[2]}회차)` : ''}`,
  };
}

/** 열린 페이지(다우오피스와 무관한 컨텍스트)로 네이버 금융을 읽는다. */
export async function fetchUsdKrw(page, { url = FX_URL, timeoutMs = 30_000 } = {}) {
  try {
    await page.goto(url, { waitUntil: 'domcontentloaded', timeout: timeoutMs });
    // 원본은 1.5초를 기다렸다. 고시 문구가 뜰 때까지 본다.
    await page.waitForFunction(() => /매매기준율 기준 미국 USD/.test(document.body?.innerText ?? ''), null, { timeout: 15_000, polling: 500 })
      .catch(() => {});
    const text = await page.evaluate(() => document.body.innerText);
    const fx = parseFxText(text);
    if (!fx) throw new PluginError('UPSTREAM', '네이버 금융에서 하나은행 매매기준율을 읽지 못했습니다. 잠시 뒤 다시 시도해 주세요.');
    return fx;
  } catch (e) {
    if (e instanceof PluginError) throw e;
    throw new PluginError('UPSTREAM', '네이버 금융 환율 페이지를 열지 못했습니다. 잠시 뒤 다시 시도해 주세요.', { cause: e });
  }
}

/**
 * 오늘(KST) 환율. `fx_rates` 에 오늘 값이 있으면 그것을, 없으면 네이버 금융에서 받아 남긴다.
 * `fetch` 는 시험용이다. 기본은 공유 브라우저의 빈 컨텍스트에서 `fetchUsdKrw` 를 부른다.
 *
 * 돌려주는 값 `{ rate, at, source, day }`
 */
export async function todayRate({ db, sessions, fetch, now = new Date() }) {
  const day = kstDate(now);
  const cached = db.fxRate(day);
  if (cached) return { rate: cached.rate, at: cached.at, source: cached.source, day };
  const get = fetch ?? (() => sessions.withScratchContext(({ page }) => fetchUsdKrw(page)));
  const fx = await get();
  db.addFxRate({ day, rate: fx.rate, at: fx.at ?? kstIso(now).slice(0, 16).replace('T', ' '), source: fx.source });
  return { ...fx, day };
}

/**
 * 외화를 원화로 바꾼다. 원 단위 반올림이다.
 * 부동소수 오차를 피하려고 센트 단위 정수로 곱한다.
 */
export function toKrw(usd, rate) {
  const cents = Math.round(Number(usd) * 100);
  const rateCenti = Math.round(Number(rate) * 100);
  return Math.round((cents * rateCenti) / 10_000);
}

/**
 * **이미 카드로 결제된** 외화 건의 `기타` 칸 한 줄.
 *
 * 카드사가 실제로 적용한 환율은 고시 매매기준율과 다르다(수수료, 고시 시각 차이).
 * 결제된 건은 추정할 이유가 없으므로 카드 승인액을 그대로 쓰고, 적용환율은
 * 결제액 ÷ 외화액으로 거꾸로 구해 적는다.
 */
export function paidNote({ usd, krw, at }) {
  const rate = Number(krw) / Number(usd);
  return `USD ${Number(usd).toFixed(2)} × ${rate.toLocaleString('ko-KR', { minimumFractionDigits: 1, maximumFractionDigits: 1 })}원 `
    + `(${at} 카드 승인 적용환율) = ${Number(krw).toLocaleString('ko-KR')}원`;
}

/** 아직 결제되지 않은 외화 건의 `기타` 칸 한 줄. 고시 매매기준율로 환산한다. */
export function fxNote({ usd, fx }) {
  const krw = toKrw(usd, fx.rate);
  return `USD ${Number(usd).toFixed(2)} × ${Number(fx.rate).toLocaleString('ko-KR', { minimumFractionDigits: 2 })}원 `
    + `(${fx.source}, ${fx.at}) = ${krw.toLocaleString('ko-KR')}원`;
}
