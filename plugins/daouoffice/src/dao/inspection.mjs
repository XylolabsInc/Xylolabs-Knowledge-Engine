import { PluginError } from '../errors.mjs';
import { OVERRIDE_KINDS } from './card-match.mjs';

/**
 * 검수조서 공용 규칙. 원본 저장소 `inspection.mjs`, `inspection-list.mjs`, `inspection-report.mjs`,
 * `serials.mjs` 에서 데이터를 빼고 규칙만 옮겼다. 사진 짝, 처리된 건, 품목 금액, 시리얼은 이제
 * 플러그인 DB 에 있다.
 *
 *   사진 짝           photos 행 (purchaseKey, alsoFor, shows, otherCard, data.warn, data.serials)
 *   처리된 건          overrides `inspectionDone`        { match: { key }, data: { why } }
 *   사진 모자람         overrides `inspectionIncomplete`  { match: { key }, data: { missing } }
 *   품목 금액          overrides `itemOverride`          { match: { key }, data: { rows: [{ name, qty, unitWord, amount }] } }
 *   시리얼             purchases.data.serials            { '<품목>': ['…'] } 또는 'none'
 *
 * 검수조서는 결제 한 건에 한 장이고 구매 건마다 제품 사진이 필수다. 구독, 사용량 청구, 갱신,
 * 크레딧, 토큰은 사진 대신 인보이스를 붙인다.
 */

/** DB 의 품의 행을 원본 `PURCHASES` 항목 모양으로 본다. `data` 에 원본 필드가 그대로 있다. */
export function purchaseView(row) {
  return {
    ...(row.data ?? {}),
    key: row.key,
    title: row.title ?? row.data?.title ?? '',
    docId: row.docId ?? null,
    status: row.status,
    usd: row.usd ?? null,
    paidKrw: row.paidKrw ?? null,
    paidAt: row.paidAt ?? null,
    amountKrw: row.amountKrw ?? null,
  };
}

/** 결제 한 건을 가리키는 짧은 키. `2026-09-28/68600`. */
export const rowKey = (r) => `${String(r.useDate).slice(0, 10)}/${r.amount}`;

/** 실체가 없는 구매. 구독, 사용량 청구, 갱신, 크레딧, 토큰, 이용권. */
export const intangible = (p) => /(구독|지출|갱신)의 건/.test(p.title ?? '') || /크레딧|토큰|이용권|이용료/.test(p.item ?? '');

/** 괄호 안의 쉼표는 건너뛰고 쉼표로 쪼갠다. `(EU 플러그, 흰색 2m 케이블)` 을 가르지 않는다. */
export function splitTop(text) {
  const parts = []; let depth = 0; let cur = '';
  for (const ch of String(text ?? '')) {
    if (ch === '(') depth++;
    if (ch === ')') depth = Math.max(0, depth - 1);
    if (ch === ',' && depth === 0) { parts.push(cur.trim()); cur = ''; continue; }
    cur += ch;
  }
  if (cur.trim()) parts.push(cur.trim());
  return parts;
}

/** 품의의 품목 줄. 기타 칸에 품목별 줄이 있으면 그것을, 없으면 구매품명을 쪼갠다. */
export function itemLines(p) {
  const detail = (p.note ?? []).filter((l) => !/^https?:/.test(l)
    && /\d+\s*(개|세트)[\s,]|\d+\s*개입|USD \d/.test(l)
    && !/주문번호|배송비|전파법|카드 승인|정가|합계|=|인보이스|결제대행|공급가액|견적서/.test(l));
  if (detail.length >= 2) return detail;
  // `A / 1개, B / 1개` 에서 B 가 제품명 없이 옵션만 있으면 A 의 제품명을 붙인다.
  const parts = splitTop(p.item).filter((x) => /\//.test(x) || !/^\d/.test(x));
  const head = parts[0]?.split('/')[0].trim().split(' ').slice(0, 3).join(' ') ?? '';
  return parts.map((x, i) => (i > 0 && /^[\dA-Za-z.]+(MHz|GHz)/.test(x) ? `${head.replace(/ \S*MHz.*$/, '')} ${x}` : x));
}

/** 낱개 수. `6개입 / 1세트` 는 6개다. */
export function unitCount(p) {
  const m = String(p.item ?? '').match(/총\s*(\d+)\s*개/);
  if (m) return Number(m[1]);
  return splitTop(p.item).reduce((n, part) => {
    const q = part.match(/\/\s*(\d+)\s*(개|대|세트)/);
    if (!q) return n;
    // 케이블 타이, 볼트 같은 잔부품은 한 봉지가 한 개다. 충전기, 케이블 묶음만 낱개로 센다.
    const pack = !/케이블 타이|클립|볼트|나사/.test(part) && part.match(/(\d+)\s*개입/);
    return n + Number(q[1]) * (pack ? Number(pack[1]) : 1);
  }, 0);
}

/**
 * 고유 번호(시리얼, IMEI)가 있을 만한 장비. 회계 안내: 고가 제품과 장비는 시리얼 번호가 보이게 찍는다.
 * 원본은 사람이 정리한 물품 목록이었다. 여기서는 종류 낱말로 고르고, 설정의
 * `inspection.serialFromKrw` 이상 금액의 품목을 더한다.
 */
export const DEVICE = /라우터|공유기|스위치|허브|KVM|조종기|컨트롤러|Controller|수신기|Receiver|송신기|트랜시버|카메라|액션캠|Camera|마이크|스피커|사운드바|라즈베리파이|싱글보드|개발 키트|파워서플라이|PSU|모니터|노트북|태블릿|휴대전화|스마트폰|갤럭시|아이폰|멀티미터|오실로스코프|드론|프리앰프|Router/i;
const PHONE = /갤럭시|아이폰|휴대전화|스마트폰|SM-[A-Z]\d/i;
const CONSUMABLE = /락카|바니쉬|퍼티|필라멘트|시트지|케이블 타이|볼트/;

/** 품목 글자로 찍을 사진을 고른다. 겹치는 것은 한 번만 적는다. */
export function photosFor(ps) {
  const text = ps.map((p) => `${p.title} ${p.item} ${(p.note ?? []).join(' ')}`).join(' ');
  const total = ps.reduce((n, p) => n + unitCount(p), 0);
  const consumable = CONSUMABLE.test(text);
  const shots = [];
  shots.push(total > 1 ? `전체 사진 한 장. 품목을 모두 모아 ${total}개를 셀 수 있게 찍는다` : '전체 사진 한 장');
  if (PHONE.test(text)) shots.push('본체 앞뒤, 설정의 휴대전화 정보 화면(모델명, IMEI)');
  else if (DEVICE.test(text)) shots.push('본체나 포장의 모델명 라벨. 시리얼 번호가 읽히게 가까이 찍는다');
  if (/\((송신기|[^)]*,[^)]*)\)/.test(text) && /마이크|Mic|콤보|세트/.test(text)) shots.push('구성품을 모두 꺼내 늘어놓은 사진');
  if (/전파법/.test(text)) shots.push('전파 인증 표기(FCC, CE 등)가 있는 면. 통관 면제 사유서와 맞춰 볼 수 있다');
  if (/충전기|컨버터|파워서플라이|PSU/.test(text)) shots.push('정격 표기(입력, 출력)');
  if (/배터리/.test(text)) shots.push('배터리 라벨. 전압, 용량, 시리얼이 읽히게');
  if (/랜케이블|CAT\.|동축 케이블|충전 케이블|C to C|USB to C/.test(text)) shots.push('케이블에 인쇄된 규격(등급, 길이)이나 포장 라벨');
  if (/개입/.test(text) && total > 1 && !consumable) shots.push('묶음 포장을 뜯어 낱개를 모두 늘어놓은 사진');
  if (consumable) shots.push('남은 용기나 포장의 라벨. 이미 썼으면 빈 용기라도 함께 찍고, 작업하거나 출력한 부품을 찍는다');
  if (ps.some((p) => /로봇/.test(p.purpose ?? '')) && !consumable) shots.push('로봇에 달았으면 장착된 모습');
  return shots;
}

/** 품목 한 줄을 이름, 수량, 금액으로 읽는다. 값이 없으면 null. */
export function parseLine(line) {
  let name = String(line).trim();
  let usd = null; let krw = null; let unit = null; let qty = null; let unitWord = '개';
  const u = name.match(/\s*USD\s*([\d.]+)\s*$/);
  if (u) { usd = Number(u[1]); name = name.slice(0, u.index).trim(); }
  const d = name.match(/,?\s*단가\s*([\d,]+)원\s*$/);
  if (d) { unit = Number(d[1].replace(/,/g, '')); name = name.slice(0, d.index).trim(); }
  const k = name.match(/\s+([\d,]+)원\s*$/);
  if (k) { krw = Number(k[1].replace(/,/g, '')); name = name.slice(0, k.index).trim(); }
  const q = name.match(/\s*\/\s*(\d+)\s*(개|대|세트|건|쌍|장)\s*$/) ?? name.match(/\s+(\d+)\s*(개|대|세트|쌍|장)\s*$/);
  if (q) { qty = Number(q[1]); unitWord = q[2]; name = name.slice(0, q.index).trim(); }
  return { name, usd, krw, unit, qty: qty ?? 1, unitWord };
}

const words = (t) => new Set(String(t).toLowerCase().split(/[^0-9a-z가-힣.+]+/).filter((w) => w.length > 1));

/**
 * 품의의 `serials` 를 품명 끝에 붙인다. 라벨이 겉에 없는 물품이 많아 사진만으로는 번호를 확인할 수
 * 없다. 시리얼을 적은 품목 이름과 낱말이 가장 많이 겹치는 줄에 붙인다.
 */
export function withSerials(p, parsed) {
  if (!p.serials || p.serials === 'none') return parsed;
  const out = parsed.map((x) => ({ ...x }));
  for (const [item, nums] of Object.entries(p.serials)) {
    if (!nums?.length || !out.length) continue;
    const want = words(item);
    let best = 0; let score = -1;
    out.forEach((x, i) => {
      const n = [...words(x.name)].filter((w) => want.has(w)).length;
      if (n > score) { score = n; best = i; }
    });
    // `200364500000 (USB 일련번호)` → `200364500000, USB 일련번호`. `SoC ID …` 처럼 이름이 붙은 값은 그대로 쓴다.
    const text = nums.map((n) => String(n).replace(/\s*\((.+)\)$/, ', $1'));
    const label = /^[0-9A-Za-z]/.test(text[0]) && !/^SoC/i.test(text[0]) ? 'S/N ' : '';
    out[best].name = `${out[best].name} (${label}${text.join(' / ')})`;
  }
  return out;
}

/**
 * 결제 한 건의 세부내역 줄. 금액 합이 카드 금액과 같게 맞춘다.
 * `itemRows(key)` 는 사람이 적어 둔 품목 금액(`itemOverride`)이다.
 */
export function reportRows(row, ps, itemRows = () => null) {
  const day = String(row.useDate).slice(0, 10);
  const lines = [];
  for (const p of ps) {
    const share = Number(p.amountKrw);
    if (!Number.isFinite(share)) throw new PluginError('VALIDATION', `품의 ${p.key} 의 원화 금액이 없습니다.`);
    // 판매자가 여럿인 알리 묶음은 줄마다 `알리익스프레스` 로 쓴다. 판매자 목록은 거래처명 칸에 있다.
    const vendor = /^알리익스프레스 \(/.test(p.vendor ?? '') ? '알리익스프레스' : (p.vendor ?? '');
    const parsed = withSerials(p, (itemRows(p.key) ?? itemLines(p)).map((l) => (typeof l === 'string' ? parseLine(l) : { qty: 1, unitWord: '개', ...l })));
    if (!parsed.length) throw new PluginError('VALIDATION', `품의 ${p.key} 에서 품목을 읽지 못했습니다.`);
    const usdSum = parsed.reduce((t, x) => t + (x.usd ?? 0), 0);
    let amounts;
    if (parsed.length === 1) amounts = [share];
    else if (parsed.every((x) => x.usd != null)) {
      amounts = parsed.map((x) => Math.round((x.usd / usdSum) * share));
      amounts[amounts.length - 1] += share - amounts.reduce((t, a) => t + a, 0);
    } else if (parsed.every((x) => x.krw != null || x.unit != null || x.amount != null)) {
      amounts = parsed.map((x) => x.amount ?? x.krw ?? x.unit * x.qty);
    } else {
      throw new PluginError('VALIDATION',
        `품의 "${p.title}" 의 품목별 금액을 알 수 없습니다. 품목마다 금액을 물어 mark_inspection(items)으로 적어 주세요: ${parsed.map((x) => x.name).join(', ')}`,
        { data: { purchaseKey: p.key, items: parsed.map((x) => x.name) } });
    }
    parsed.forEach((x, i) => {
      const amount = amounts[i];
      lines.push({
        name: x.name, vendor, qty: `${x.qty}${x.unitWord}`,
        unit: x.qty ? Math.round(amount / x.qty) : amount, amount, note: day,
      });
    });
  }
  const sum = lines.reduce((t, l) => t + l.amount, 0);
  if (sum !== row.amount) {
    // 원화 품목가가 공급가액이면 차이가 부가세다.
    const diff = row.amount - sum;
    const foreign = ps.some((p) => p.usd != null);
    const name = foreign ? '환율 차이'
      : Math.abs(diff - Math.round(sum * 0.1)) <= 10 ? '부가세'
        : diff > 0 ? '배송비' : '할인 및 적립금';
    lines.push({ name, vendor: lines[0].vendor, qty: '', unit: '', amount: diff, note: day });
  }
  if (lines.length > 20) {
    throw new PluginError('VALIDATION', `세부내역이 20줄을 넘습니다 (${lines.length}줄). 별첨 파일이 필요해 이 도구로는 올리지 않습니다.`);
  }
  return lines;
}

/**
 * 세부내역을 공급가액, 부가세액, 합계로 나눈다. 해외 결제는 공급가액이 전부이고 부가세 0. 국내
 * 결제에 `부가세` 조정 줄이 있으면 품목가가 공급가액이라 그 줄을 품목마다 10% 로 나눠 넣는다.
 * 없으면 품목가가 부가세 포함 값이다.
 */
export function splitVat(lines, foreign) {
  const vatLine = lines.find((l) => l.name === '부가세' && l.qty === '');
  const rows = lines.filter((l) => l !== vatLine).map((l) => {
    if (foreign) return { ...l, supply: l.amount, vat: 0 };
    if (vatLine) return { ...l, supply: l.amount, vat: Math.round(l.amount * 0.1) };
    const supply = Math.round(l.amount / 1.1);
    return { ...l, supply, vat: l.amount - supply };
  });
  if (vatLine && rows.length) rows.at(-1).vat += vatLine.amount - rows.reduce((t, l) => t + l.vat, 0);
  return rows.map((l) => ({ ...l, total: l.supply + l.vat }));
}

/** 첫 품목 이름 외 N종. 품의 제목에서 `… 구매의 건` 을 걷어 낸다. */
export function headOf(ps, lines) {
  const strip = (t) => String(t).replace(/ (구매|구독|갱신|지출)의 건.*$/, '');
  // 품의 하나면 품의 제목을 그대로 쓴다(이미 `외 N종` 이 세어져 있다).
  if (ps.length === 1) return strip(ps[0].title);
  const items = lines.filter((l) => l.qty !== '');
  return `${strip(ps[0].title).replace(/ 외 \d+종/, '')} 외 ${items.length - 1}종`;
}

/** 사람이 알려 준 검수 처리 상태를 품의 키로 찾는 함수들. */
export function inspectionMarks(overrides = []) {
  const byKind = (kind) => {
    const m = new Map();
    for (const o of overrides.filter((x) => x.kind === kind && x.match?.key)) m.set(o.match.key, o.data ?? {});
    return m;
  };
  const done = byKind(OVERRIDE_KINDS.inspectionDone);
  const incomplete = byKind(OVERRIDE_KINDS.inspectionIncomplete);
  const items = byKind(OVERRIDE_KINDS.itemOverride);
  return {
    /** 처리됐거나 사진이 필요 없다고 한 이유, 없으면 undefined. */
    doneOf: (ps) => ps.map((p) => done.get(p.key)?.why).find(Boolean),
    /** 빠진 품목 설명, 없으면 undefined. */
    incompleteOf: (ps) => ps.map((p) => incomplete.get(p.key)?.missing).find(Boolean),
    /** 사람이 적어 둔 품목 금액 줄, 없으면 null. */
    itemRows: (key) => (Array.isArray(items.get(key)?.rows) ? items.get(key).rows : null),
  };
}

/** 품의 키들의 사진. 다른 카드로 산 물건(`otherCard`)은 넣지 않는다. 한 사진이 `alsoFor` 로 두 품의에 쓰인다. */
export function photosOfKeys(photos, keys) {
  const want = new Set(keys);
  return photos.filter((ph) => !ph.otherCard
    && (want.has(ph.purchaseKey) || (ph.alsoFor ?? []).some((k) => want.has(k))));
}

/**
 * 시리얼 사진이 필요한 품목과 받은 번호. `{ item, needed, numbers }` 배열.
 * 품의의 `serials: 'none'` 은 사람이 대상 아니라고 한 것이라 빼고, 번호는 지어내지 않는다.
 */
export function serialNeeds(p, { serialFromKrw, itemRows } = {}) {
  if (p.serials === 'none') return [];
  const parsed = (itemRows?.(p.key) ?? itemLines(p)).map((l) => (typeof l === 'string' ? parseLine(l) : l));
  const given = p.serials && typeof p.serials === 'object' ? p.serials : {};
  const single = parsed.length === 1;
  return parsed
    .filter((x) => DEVICE.test(x.name) || (serialFromKrw && Number(x.amount ?? x.krw ?? (single ? p.amountKrw : 0)) >= serialFromKrw))
    .map((x) => {
      const want = words(x.name);
      const hit = Object.entries(given).find(([k]) => single || [...words(k)].some((w) => want.has(w)));
      return { item: x.name, numbers: hit?.[1] ?? [] };
    });
}

/**
 * 카드 결제를 검수조서 단위로 편다. 보통 결제 한 건에 한 장이지만, 주문 영수증마다 품의를 따로
 * 쓰는 가맹점(설정 `inspection.perOrderMerchants`, 기본 알리익스프레스)은 `perOrderFrom` 이후
 * 결제부터 품의마다 한 장이고, 금액은 그 품의의 몫이다.
 *
 * `{ a, ps, key, perOrder, cardAmount }` 배열. `key` 는 `날짜/금액`, 나눈 결제는 `날짜/금액#품의키`.
 */
export function inspectionUnits(audited, purchasesByKey, cfg = {}) {
  const merchants = (cfg.perOrderMerchants ?? ['ALIEXPRESS']).map((s) => new RegExp(s, 'i'));
  const from = cfg.perOrderFrom ?? '';
  return audited
    .map((a) => ({ a, ps: (a.purchases?.length ? a.purchases : (a.purchase ? [a.purchase] : [])).map((p) => purchasesByKey.get(p.key) ?? purchaseView(p)) }))
    .filter(({ ps }) => ps.length)
    .flatMap(({ a, ps }) => {
      const day = String(a.row.useDate).slice(0, 10);
      const split = ps.length > 1 && merchants.some((re) => re.test(a.row.merchant)) && day >= from;
      if (!split) return [{ a, ps, key: rowKey(a.row), perOrder: false, cardAmount: a.row.amount }];
      return ps.map((p) => ({
        a: { ...a, row: { ...a.row, amount: Number(p.amountKrw) } },
        ps: [p],
        key: `${rowKey(a.row)}#${p.key}`,
        perOrder: true,
        cardAmount: a.row.amount,
      }));
    })
    .sort((x, y) => String(x.a.row.useDate).localeCompare(String(y.a.row.useDate)));
}
