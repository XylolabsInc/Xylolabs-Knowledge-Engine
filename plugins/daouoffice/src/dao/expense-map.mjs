import * as z from 'zod';
import { PluginError } from '../errors.mjs';

/**
 * 영수증 비목을 다우오피스 사용용도로 옮기는 표와 상세 사용용도 문구 규칙. 원본 저장소
 * `expense-map.mjs` 에서 왔다. 바뀐 것은 회사마다 다른 예외(품목 예외, 구독 상세 문구, 대체
 * 사용용도)를 코드가 아니라 회사 설정 `purposeMap` 에서 읽는 것이다.
 *
 * 둘은 다른 체계다. 비목(`재료비`, `클라우드구독료`)은 영수증을 정리할 때 쓰는 이름이고,
 * 사용용도는 경비 앱의 고정 코드 목록이다. 코드는 하드코딩하지 않는다. 이름으로 적어 두고
 * `resolvePurpose()` 가 라이브 목록(`listPurposes()`)에서 코드를 찾는다. 관리자가 코드를 손대도
 * 견딘다. 목록에 없는 사용용도는 조용히 다른 것으로 바꾸지 않고 멈춘다. 사람이 대체 사용용도를
 * 정했으면 설정에 `fallback` 으로 적는다.
 *
 * `purposeMap` 한 줄은 다섯 모양 가운데 하나다.
 *
 *   { "category": "비품", "purpose": "소모품비" }                       비목 → 사용용도 (기본표를 덮는다)
 *   { "match": "태블릿", "unless": "케이스|커버", "purpose": "장비자산" } 품목에 match 가 들면 (unless 정규식이 맞지 않을 때)
 *   { "vendor": "^테스트", "detail": "테스트 API 사용료" }               품의 없는 구독의 상세 문구 (판매처 정규식)
 *   { "purpose": "재료비", "fallback": "소모품비" }                       목록에 재료비가 없을 때만 대신 쓴다
 *   { "purpose": "수수료비용", "prefer": "지급수수료" }                   목록에 지급수수료가 있으면 그것을 쓴다
 */

/** 비목 → 사용용도 기본표. 설정의 `{ category, purpose }` 가 덮는다. */
export const DEFAULT_CATEGORIES = Object.freeze({
  재료비: '재료비',
  클라우드구독료: '수수료비용',
  소프트웨어: '수수료비용',
  비품: '소모품비',
  사무용품: '사무용품',
});

/** 이용료로 끝내는 비목. 나머지는 구매로 끝낸다. */
const SUBSCRIPTION = new Set(['클라우드구독료', '소프트웨어']);

const isRegex = (s) => { try { new RegExp(s, 'i'); return true; } catch { return false; } };
const name = z.string().trim().min(1).max(40);
const regex = z.string().min(1).max(200).refine(isRegex, '정규식이 아닙니다');

const Entry = z.union([
  z.strictObject({ category: name, purpose: name }),
  z.strictObject({ match: z.string().trim().min(1).max(80), unless: regex.optional(), purpose: name }),
  z.strictObject({ vendor: regex, detail: z.string().trim().min(1).max(80) }),
  z.strictObject({ purpose: name, fallback: name }),
  z.strictObject({ purpose: name, prefer: name }),
]);

const cache = new WeakMap();

/**
 * 설정의 `purposeMap` 을 풀어 쓰기 좋은 모양으로 만든다. 잘못된 줄이 있으면 INTERNAL 로 멈춘다.
 * 설정은 얼려 있으므로 같은 배열이면 한 번만 푼다.
 */
export function parsePurposeMap(list = []) {
  if (list && typeof list === 'object' && cache.has(list)) return cache.get(list);
  const r = z.array(Entry).safeParse(list ?? []);
  if (!r.success) {
    const where = r.error.issues.map((i) => `purposeMap.${i.path.join('.')}`).join(', ');
    throw new PluginError('INTERNAL', `회사 설정의 purposeMap 이 잘못됐습니다: ${where}`);
  }
  const map = { categories: { ...DEFAULT_CATEGORIES }, overrides: [], details: [], fallback: {}, prefer: {} };
  for (const e of r.data) {
    if ('category' in e) map.categories[e.category] = e.purpose;
    else if ('match' in e) map.overrides.push({ match: e.match.toLowerCase(), unless: e.unless ? new RegExp(e.unless, 'i') : null, purpose: e.purpose });
    else if ('vendor' in e) map.details.push({ vendor: new RegExp(e.vendor, 'i'), detail: e.detail });
    else if ('fallback' in e) map.fallback[e.purpose] = e.fallback;
    else map.prefer[e.purpose] = e.prefer;
  }
  if (list && typeof list === 'object') cache.set(list, map);
  return map;
}

/**
 * 비목과 품목으로 사용용도 이름을 정한다. 품목 예외가 먼저고, 없으면 비목 표를 쓴다.
 * 모르는 비목이면 지어내지 않고 VALIDATION 으로 멈춘다.
 */
export function purposeNameFor({ category, item = '' }, map = parsePurposeMap()) {
  const needle = String(item ?? '').toLowerCase();
  for (const o of map.overrides) {
    if (needle.includes(o.match) && !o.unless?.test(needle)) return o.purpose;
  }
  const purpose = map.categories[category];
  if (!purpose) {
    throw new PluginError('VALIDATION', `모르는 비목입니다: "${category ?? '(없음)'}". 어느 사용용도로 올릴지 사람에게 묻고, 회사 설정 purposeMap 에 넣어야 합니다.`);
  }
  return purpose;
}

/** 파일명 조각을 사람이 읽는 말로 되돌린다. 하이픈은 공백이다. */
function clean(text, max = 40) {
  const s = String(text ?? '').replace(/-/g, ' ').replace(/\s+/g, ' ').trim();
  return s.length > max ? `${s.slice(0, max - 1).trimEnd()}…` : s;
}

const itemOf = (p) => String(p?.item ?? p?.data?.item ?? '');

/**
 * 상세 사용용도 문구를 만든다.
 *
 * 구매 품의가 있으면 그 제목을 쓴다. 결재자가 이미 읽고 승인한 문구라 품의서와 지출결의가
 * 같은 말을 한다. `… 구매의 건` → `… 구매`, `… 구독의 건` → `… 구독`. 한 결제에 품의가
 * 여럿이면 품목 수로 세어 `<첫 품목> 외 N종 구매` 로 쓴다(`외` 가 겹치면 읽기 어렵다).
 *
 * 품의가 없으면 영수증에서 만든다. 구독은 `<판매처> 이용료` (설정에 문구가 있으면 그것),
 * 물건은 `<품목> 구매`. 지어낸 말이 섞이지 않는다.
 */
export function detailFor({ category, item = '', vendor = '', purchase = null, purchases = [] }, map = parsePurposeMap()) {
  const list = purchases?.length ? purchases : (purchase ? [purchase] : []);
  if (list[0]?.title) {
    const head = list[0].title.replace(/의 건(\s*\([^)]+\))?$/, '').replace(/\s*·\s*/g, ', ').trim();
    if (list.length === 1) return head;
    const kinds = list.reduce((t, p) => t + Math.max(1, (itemOf(p).match(/ \/ /g) ?? []).length), 0);
    const base = head.replace(/ 외 \d+종/, '').replace(/ (구매|구독|갱신|지출)$/, '');
    const verb = /(구독|갱신|지출)$/.exec(head)?.[1] ?? '구매';
    return kinds > 1 ? `${base} 외 ${kinds - 1}종 ${verb}` : `${base} ${verb}`;
  }
  if (SUBSCRIPTION.has(category)) {
    const who = vendor || String(item ?? '').split('-')[0];
    const fixed = map.details.find((d) => d.vendor.test(who));
    if (fixed) return fixed.detail;
    return `${clean(who)} 이용료`;
  }
  return `${clean(item)} 구매`;
}

/**
 * 실제로 저장될 사용용도 이름. 설정의 `prefer` 가 목록에 있으면 그것, 이름이 목록에 없고
 * `fallback` 이 목록에 있으면 그것. 문서 제목과 묶음은 이 이름을 쓴다. 이름과 저장 코드가
 * 어긋나면 제목은 (재료비) 인데 행은 다른 사용용도로 저장된다.
 */
export function effectivePurposeName(purposes, purpose, map = parsePurposeMap()) {
  const has = (n) => purposes.some((p) => p.name === n);
  const preferred = map.prefer[purpose];
  if (preferred && has(preferred)) return preferred;
  const fallback = map.fallback[purpose];
  if (!has(purpose) && fallback && has(fallback)) return fallback;
  return purpose;
}

/** 사용용도 이름을 코드로 바꾼다. 목록에 없으면 이유를 말하고 멈춘다. */
export function resolvePurpose(purposes, purpose, map = parsePurposeMap()) {
  const effective = effectivePurposeName(purposes, purpose, map);
  const hit = purposes.find((p) => p.name === effective);
  if (hit) return hit.code;
  const fallback = map.fallback[purpose];
  throw new PluginError('VALIDATION', [
    `사용용도 "${effective}" 이 다우오피스 사용용도 목록에 없습니다.`,
    fallback ? `대체 사용용도 "${fallback}" 도 목록에 없습니다.` : null,
    '관리자(회계)에게 사용용도 추가를 요청하거나, 사람이 대체 사용용도를 정하면 회사 설정 purposeMap 에',
    `{ "purpose": "${purpose}", "fallback": "<대체 사용용도>" } 로 넣습니다. 다른 사용용도로 바꿔 올리지 않았습니다.`,
  ].filter(Boolean).join(' '));
}
