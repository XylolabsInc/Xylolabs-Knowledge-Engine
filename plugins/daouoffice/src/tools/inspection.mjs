import fs from 'node:fs';
import path from 'node:path';
import { createHash } from 'node:crypto';
import * as z from 'zod';
import { PluginError } from '../errors.mjs';
import { kstDate, kstIso, addDays, monthRange } from '../dao/kst.mjs';
import { ingestPhoto, stripImage, thumbnail } from '../photos.mjs';
import { stripPdf } from '../pdf.mjs';
import { digestOf } from '../runtime.mjs';
import { listCards, chooseCard, last4Of } from '../dao/cards.mjs';
import { matchCard, OVERRIDE_KINDS, overrideMatches } from '../dao/card-match.mjs';
import { findForm } from '../dao/forms.mjs';
import { postComment } from '../dao/approval.mjs';
import { checkWriting } from '../dao/writing.mjs';
import {
  purchaseView, inspectionMarks, inspectionUnits, photosOfKeys, photosFor, itemLines, intangible, serialNeeds,
} from '../dao/inspection.mjs';
import {
  planReport, describePlan, filedByTitle, oldReports, fileReport, inside, FORM_NAME,
} from '../dao/inspection-report.mjs';
import { paymentStatus, describeStatus } from '../dao/status.mjs';

/**
 * 검수조서와 결제 현황 도구.
 *
 *   save_photos             local  받은 사진의 메타데이터를 지우고 품의에 짝지어 저장한다
 *   save_receipt            local  받은 영수증(이미지, PDF)을 이름 규칙대로 저장한다
 *   mark_inspection         local  처리된 건, 사진 모자람, 품목별 금액을 적는다
 *   mark_payment            local  남이 처리하는 결제, 환불, 그대로 청구, 영수증 짝을 적는다
 *   inspection_candidates   read   검수조서가 필요한 결제와 사진, 시리얼 현황
 *   payment_status          read   결제별 영수증, 품의, 사진, 검수조서, 지출결의
 *   file_inspection_report  write  검수조서 한 장 (미리보기 → 승인 → 상신, 대조)
 *   attach_photos_to_claim  write  검수 사진을 그 결제가 들어간 지출결의에 댓글로 붙인다
 */

const won = (n) => Number(n).toLocaleString('ko-KR');
const dateArg = z.string().regex(/^\d{4}-\d{2}-\d{2}$/, 'YYYY-MM-DD');
const monthArg = z.string().regex(/^\d{4}-\d{2}$/, 'YYYY-MM');
const keyArg = z.string().min(1).max(120);
const cardArg = z.string().optional().describe('카드 뒤 네 자리나 카드 이름 일부. 안 주면 내 이름의 카드가 한 장일 때 그 카드');
const periodShape = {
  card: cardArg,
  from: dateArg.optional().describe('시작일 YYYY-MM-DD'),
  to: dateArg.optional().describe('종료일 YYYY-MM-DD'),
  month: monthArg.optional().describe('한 달 YYYY-MM. from, to 대신 쓴다'),
};

const IMAGE = /\.(jpe?g|png|heic|heif|webp|tiff?)$/i;
const isImage = (a) => /^image\//.test(a.mimeType) || IMAGE.test(a.name);
const isPdf = (a) => a.mimeType === 'application/pdf' || /\.pdf$/i.test(a.name);

/** 기간. `month` 가 먼저고, 없으면 from/to, 둘 다 없으면 `fallback` 이다. 오늘 뒤로는 자른다. */
function periodOf({ from, to, month }, fallback) {
  const today = kstDate();
  let range = month ? monthRange(month) : [from ?? fallback[0], to ?? fallback[1]];
  if (range[1] > today) range = [range[0], today];
  if (range[0] > range[1]) throw new PluginError('VALIDATION', `기간이 거꾸로입니다: ${range[0]}부터 ${range[1]}까지`);
  return range;
}

/** 첨부를 번호(1부터)로 고른다. 이름을 주면 맞는지도 본다. */
function attachmentAt(call, index, name) {
  const a = call.attachments[index - 1];
  if (!a) {
    throw new PluginError('VALIDATION', call.attachments.length
      ? `첨부 ${index} 번이 없습니다. 이 메시지의 첨부는 ${call.attachments.length}개입니다.`
      : '이 메시지에 첨부된 파일이 없습니다. 사진이나 영수증을 같은 메시지에 올려 달라고 해 주세요.');
  }
  if (name && name !== a.name) throw new PluginError('VALIDATION', `첨부 ${index} 번은 ${a.name} 입니다 (${name} 이 아닙니다).`);
  return a;
}

/** 품의 키가 그 사람의 품의인지 본다. */
function requirePurchase(rt, owner, key) {
  const row = rt.db.purchase(owner, key);
  if (!row) throw new PluginError('NOT_FOUND', `품의 키 "${key}" 가 없습니다. inspection_candidates 나 품의 목록에서 키를 확인해 주세요.`);
  return purchaseView(row);
}

/** 사진 행의 저장 경로. 저장소 밖을 가리키면 null. */
const pathIn = (dir) => (row) => inside(dir, row.file);

/** 카드, 짝짓기, 품의, 사진, 예외를 한 번에 읽는다. */
async function loadContext(rt, call, api, { card: needle, from, to }) {
  const account = rt.db.account(call.owner);
  const card = chooseCard(await listCards(api), { needle, holderName: account?.name });
  const m = await matchCard({
    api, db: rt.db, owner: call.owner, card, from, to, vendorAliases: rt.config.cardMatch?.vendorAliases ?? {},
  });
  const purchasesByKey = new Map(rt.db.purchases(call.owner).map((row) => [row.key, purchaseView(row)]));
  const overrides = rt.db.overrides(call.owner);
  return {
    account, card, m, purchasesByKey, overrides,
    marks: inspectionMarks(overrides),
    photos: rt.db.photos(call.owner),
    units: inspectionUnits(m.audited, purchasesByKey, rt.config.inspection ?? {}),
  };
}

/** 문서 ID → 문서번호. 비고 칸에 쓴다. 못 읽으면 문서 ID 를 그대로 쓴다. */
async function docNumbers(api, ids) {
  const out = new Map();
  for (const id of new Set(ids.filter(Boolean).map(String))) {
    if (!/^\d{6,25}$/.test(id)) continue;
    const d = (await api(`/gw/api/approval/document/${id}`)).data?.data?.document;
    if (d?.docNum) out.set(id, String(d.docNum));
  }
  return out;
}

/** 미리보기에 작게 싣는 사진 수. 넘는 것은 글로 센다. */
const THUMBS = 6;

/** 사진 몇 장을 작게 줄여 미리보기에 싣는다. */
async function thumbnails(paths, max = THUMBS) {
  const out = [];
  for (const p of paths.slice(0, max)) out.push({ data: await thumbnail(p), mimeType: 'image/jpeg' });
  return out;
}

export function register(server, rt) {
  const photosDir = (owner) => rt.filesDir(owner, 'photos');
  const receiptsDir = (owner) => rt.filesDir(owner, 'receipts');

  // ------------------------------------------------------------------ save_photos

  rt.tool(server, 'save_photos', {
    kind: 'local',
    description: [
      '검수조서용 제품 사진을 저장한다. 사진의 위치, 기기, 촬영 시각 메타데이터는 지우고 촬영 날짜만 남긴다.',
      '사진마다 어느 구매 품의(purchaseKey)의 물건인지와 사진에 보이는 것(shows)을 적는다. 상자나 라벨의 모델명, 수량으로 짝을 짓는다.',
      '어느 품의인지 확실하지 않으면 지어내지 말고 사람에게 묻는다. 다른 카드로 산 물건이면 otherCard 에 그 카드 뒤 네 자리를 적는다.',
      '같은 물건을 두 번 주문해 한 사진이 두 품의에 해당하면 alsoFor 에 다른 품의 키를 적는다. 라벨에서 읽은 시리얼 번호는 serials 에 적는다(읽히는 것만).',
      '이미 저장한 사진은 photoId 로 고친다(짝, 설명, 촬영 날짜). 품의 키는 inspection_candidates 결과에 있다.',
    ].join(' '),
    input: z.object({
      photos: z.array(z.object({
        index: z.number().int().min(1).optional().describe('이 메시지 첨부의 순서(1부터). 새 사진일 때'),
        name: z.string().optional().describe('첨부 파일 이름. 순서를 확인하려고 쓴다'),
        photoId: z.number().int().min(1).optional().describe('이미 저장한 사진을 고칠 때 그 사진 번호'),
        purchaseKey: keyArg.optional().describe('이 사진의 구매 품의 키'),
        alsoFor: z.array(keyArg).optional().describe('같은 사진이 해당하는 다른 품의 키'),
        shows: z.string().max(200).optional().describe('사진이 보여 주는 것. 예: 상자와 모델명 라벨, 다섯 개 모두'),
        otherCard: z.string().regex(/^\d{4}$/).optional().describe('다른 카드로 산 물건이면 그 카드 뒤 네 자리'),
        serials: z.array(z.object({
          item: z.string().min(1).describe('품목 이름'),
          numbers: z.array(z.string().min(1)).min(1).describe('라벨에서 읽은 번호'),
        })).optional(),
        warn: z.string().max(200).optional().describe('짝이 애매하면 그 이유'),
        takenDate: dateArg.optional().describe('사진에 촬영 날짜가 없을 때 사람이 알려 준 촬영일'),
      })).min(1),
    }),
  }, async ({ photos }, call) => {
    const out = [];
    const lines = [];
    const notes = [];
    for (const [i, e] of photos.entries()) {
      const label = e.photoId ? `사진 ${e.photoId}` : `첨부 ${e.index}`;
      if (Boolean(e.index) === Boolean(e.photoId)) {
        throw new PluginError('VALIDATION', `${i + 1}번째 항목: 새 사진은 index, 저장한 사진은 photoId 중 하나만 적습니다.`);
      }
      const views = [e.purchaseKey, ...(e.alsoFor ?? [])].filter(Boolean).map((k) => requirePurchase(rt, call.owner, k));
      const serials = e.serials ?? [];
      if (serials.length && !e.purchaseKey) throw new PluginError('VALIDATION', `${label}: 시리얼 번호는 품의(purchaseKey)와 함께 적습니다.`);

      let row;
      if (e.index) {
        if (!e.purchaseKey && !e.otherCard) {
          throw new PluginError('VALIDATION', `${label}: 어느 품의의 물건인지 모릅니다. 사람에게 물어 purchaseKey 를 적거나, 다른 카드로 샀으면 otherCard 를 적어 주세요.`);
        }
        if (!e.shows) throw new PluginError('VALIDATION', `${label}: 사진에 보이는 것(shows)을 적어 주세요.`);
        const att = attachmentAt(call, e.index, e.name);
        if (!isImage(att)) throw new PluginError('VALIDATION', `${label} (${att.name}) 은 사진이 아닙니다.`);
        const saved = await ingestPhoto(att.path, photosDir(call.owner));
        const fields = {
          purchaseKey: e.purchaseKey ?? null, alsoFor: e.alsoFor ?? [], shows: e.shows,
          otherCard: e.otherCard ?? null, takenDate: e.takenDate ?? saved.takenDate,
        };
        const added = rt.db.addPhoto({
          owner: call.owner, file: saved.file, sha256: saved.sha256, ...fields,
          data: { warn: e.warn ?? null, serials, sourceName: att.name, width: saved.width, height: saved.height, bytes: saved.bytes },
        });
        row = added;
        if (!added.created) {
          // The same picture again: change only what this call gave; keep the rest (date, links, serials).
          notes.push(`${label}: 이미 저장한 사진(사진 ${added.id})이라 이번에 적은 값만 고쳤습니다.`);
          const patch = {};
          for (const k of ['purchaseKey', 'alsoFor', 'shows', 'otherCard']) if (e[k] !== undefined) patch[k] = e[k];
          const taken = e.takenDate ?? added.takenDate ?? saved.takenDate;
          if (taken !== added.takenDate) patch.takenDate = taken;
          patch.data = { ...added.data, ...(e.warn !== undefined ? { warn: e.warn } : {}), ...(serials.length ? { serials } : {}) };
          row = rt.db.updatePhoto(call.owner, added.id, patch);
        }
      } else {
        const ph = rt.db.photo(call.owner, e.photoId);
        if (!ph) throw new PluginError('NOT_FOUND', `사진 ${e.photoId} 이 없습니다.`);
        const patch = {};
        for (const k of ['purchaseKey', 'alsoFor', 'shows', 'otherCard', 'takenDate']) if (e[k] !== undefined) patch[k] = e[k];
        patch.data = { ...ph.data, ...(e.warn !== undefined ? { warn: e.warn } : {}), ...(serials.length ? { serials } : {}) };
        row = rt.db.updatePhoto(call.owner, ph.id, patch);
      }

      // 라벨에서 읽은 시리얼 번호는 품의에도 적는다. 검수조서 품명 끝에 붙는다.
      if (serials.length) {
        const p = views[0];
        const current = p.serials && typeof p.serials === 'object' ? p.serials : {};
        const next = { ...current };
        for (const s of serials) next[s.item] = [...new Set([...(next[s.item] ?? []), ...s.numbers])];
        rt.db.patchPurchaseData(call.owner, p.key, { serials: next });
      }
      const title = views[0]?.title ?? (row.otherCard ? `다른 카드 ${row.otherCard}` : '');
      out.push({ id: row.id, purchaseKey: row.purchaseKey, otherCard: row.otherCard, takenDate: row.takenDate });
      const line = `- 사진 ${row.id}: ${title} · ${row.shows ?? ''}${row.takenDate ? ` · 촬영 ${row.takenDate}` : ' · 촬영 날짜 없음'}`;
      lines.push(line);
      if (!row.takenDate && !row.otherCard) notes.push(`사진 ${row.id} 은 촬영 날짜가 없습니다. 검수조서 입고일자가 사진 찍은 날이라, 언제 찍었는지 물어 takenDate 로 적어 주세요.`);
    }
    const unused = call.attachments.length - photos.filter((e) => e.index).length;
    if (unused > 0) notes.push(`첨부 ${call.attachments.length}개 가운데 ${unused}개는 저장하지 않았습니다.`);
    return {
      text: [`사진 ${out.length}장을 저장했습니다. 위치와 기기 정보는 지웠습니다.`, ...lines, ...notes].join('\n'),
      data: { photos: out },
    };
  });

  // ------------------------------------------------------------------ save_receipt

  rt.tool(server, 'save_receipt', {
    kind: 'local',
    description: [
      '영수증, 인보이스, 주문내역, 거래명세서(이미지나 PDF)를 저장한다. 카드 결제와 검수조서, 지출결의 짝짓기에 쓴다.',
      '날짜, 비목, 품명, 구매처, 금액, 통화, 문서 종류는 영수증에서 읽어 적고, 확실하지 않은 값은 사람에게 확인한다.',
      '날짜는 카드 승인일(한국 시간)이다. 해외 인보이스는 미국 날짜라 카드 날짜가 하루 늦을 수 있다. 알리익스프레스 주문은 주문번호(16자리)를 적는다.',
    ].join(' '),
    input: z.object({
      receipts: z.array(z.object({
        index: z.number().int().min(1).describe('이 메시지 첨부의 순서(1부터)'),
        name: z.string().optional().describe('첨부 파일 이름. 순서를 확인하려고 쓴다'),
        date: dateArg.describe('카드 승인일 YYYY-MM-DD'),
        category: z.string().min(1).max(30).describe('비목. 예: 재료비, 소모품비, 클라우드구독료, 소프트웨어'),
        item: z.string().min(1).max(120).describe('품명. 여러 개면 첫 품목 외 N건'),
        vendor: z.string().min(1).max(60).describe('구매처(카드를 결제한 판매자)'),
        amount: z.number().positive().describe('영수증 총액'),
        currency: z.enum(['원', 'USD', 'EUR', 'JPY', 'GBP', 'SGD', 'CNY']).describe('통화'),
        kind: z.enum(['영수증', '인보이스', '주문내역', '거래명세서']).describe('문서 종류'),
        orderId: z.string().regex(/^[0-9A-Za-z-]{4,40}$/).optional().describe('주문번호'),
        cardLast4: z.string().regex(/^\d{4}$/).optional().describe('이 결제를 한 카드 뒤 네 자리. 알 때만'),
      })).min(1),
    }),
  }, async ({ receipts }, call) => {
    const dir = receiptsDir(call.owner);
    const known = rt.db.receipts(call.owner);
    const lines = [];
    const saved = [];
    for (const r of receipts) {
      const att = attachmentAt(call, r.index, r.name);
      let data;
      let ext;
      if (isPdf(att)) {
        const raw = fs.readFileSync(att.path);
        if (raw.subarray(0, 5).toString('latin1') !== '%PDF-') throw new PluginError('VALIDATION', `첨부 ${r.index} (${att.name}) 은 PDF 가 아닙니다.`);
        // Author, creator, XMP and the like go before the file is kept or attached anywhere.
        data = await stripPdf(raw);
        ext = 'pdf';
      } else if (isImage(att)) {
        ({ data } = await stripImage(fs.readFileSync(att.path)));
        ext = 'jpg';
      } else {
        throw new PluginError('VALIDATION', `첨부 ${r.index} (${att.name}) 은 이미지나 PDF 가 아닙니다.`);
      }
      const sha256 = createHash('sha256').update(data).digest('hex');
      const same = known.find((x) => x.sha256 === sha256);
      if (same) {
        lines.push(`- 첨부 ${r.index}: 이미 저장한 영수증입니다 (${same.file}).`);
        saved.push({ id: same.id, file: same.file, created: false });
        continue;
      }
      let file = receiptFileName(r, ext);
      for (let n = 2; known.some((x) => x.file === file) || fs.existsSync(path.join(dir, file)); n++) {
        file = receiptFileName(r, ext, n);
      }
      fs.writeFileSync(path.join(dir, file), data, { flag: 'wx' });
      const row = rt.db.addReceipt({
        owner: call.owner, file, date: r.date, category: r.category, item: r.item, vendor: r.vendor,
        amount: r.amount, currency: r.currency, kind: r.kind, orderId: r.orderId ?? null, sha256,
        data: { sourceName: att.name, ...(r.cardLast4 ? { cardLast4: r.cardLast4 } : {}) },
      });
      known.push(row);
      saved.push({ id: row.id, file, created: true });
      lines.push(`- ${r.date} ${r.vendor} ${r.currency === '원' ? `${won(r.amount)}원` : `${r.currency} ${r.amount}`} · ${r.item} (${r.kind})`);
    }
    return { text: [`영수증 ${saved.filter((s) => s.created).length}건을 저장했습니다.`, ...lines].join('\n'), data: { receipts: saved } };
  });

  // ------------------------------------------------------------------ mark_inspection

  rt.tool(server, 'mark_inspection', {
    kind: 'local',
    description: [
      '구매 품의 하나의 검수조서 처리 상태를 적는다. 사람이 말한 것만 적는다.',
      'done: 다른 분이 검수조서를 올렸거나 사진이 필요 없다고 했다(reason).',
      'incomplete: 사진에 빠진 품목이 있다(missing). 검수조서를 미룬다.',
      'items: 품목별 금액을 품의에서 읽을 수 없을 때 품목마다 이름, 수량, 금액을 적는다. 금액 합은 품의 금액과 같아야 한다. 영수증이 합계만 주면 한 줄로 쓰고 나누지 않는다.',
      'clear: 이 품의에 적은 상태를 모두 지운다.',
    ].join(' '),
    input: z.object({
      purchaseKey: keyArg,
      action: z.enum(['done', 'incomplete', 'items', 'clear']),
      reason: z.string().max(200).optional().describe('done 의 이유'),
      missing: z.string().max(200).optional().describe('incomplete 일 때 사진에 없는 품목'),
      items: z.array(z.object({
        name: z.string().min(1).max(120),
        qty: z.number().int().min(1),
        unitWord: z.enum(['개', '대', '세트', '건', '쌍', '장', '식']).optional(),
        amount: z.number().int(),
      })).optional(),
    }),
  }, async ({ purchaseKey, action, reason, missing, items }, call) => {
    const p = requirePurchase(rt, call.owner, purchaseKey);
    const kinds = { done: OVERRIDE_KINDS.inspectionDone, incomplete: OVERRIDE_KINDS.inspectionIncomplete, items: OVERRIDE_KINDS.itemOverride };
    const drop = (kind) => rt.db.overrides(call.owner, kind)
      .filter((o) => o.match?.key === purchaseKey).forEach((o) => rt.db.removeOverride(call.owner, o.id));
    if (action === 'clear') {
      rt.db.transaction(() => Object.values(kinds).forEach(drop));
      return { text: `품의 "${p.title}" 에 적은 검수 상태를 지웠습니다.`, data: { purchaseKey, action } };
    }
    let data;
    if (action === 'done') {
      if (!reason) throw new PluginError('VALIDATION', 'done 에는 이유(reason)가 필요합니다.');
      data = { why: reason };
    } else if (action === 'incomplete') {
      if (!missing) throw new PluginError('VALIDATION', 'incomplete 에는 사진에 없는 품목(missing)이 필요합니다.');
      data = { missing };
    } else {
      if (!items?.length) throw new PluginError('VALIDATION', 'items 에는 품목 줄이 필요합니다.');
      const sum = items.reduce((t, x) => t + x.amount, 0);
      if (Number.isFinite(Number(p.amountKrw)) && sum !== Number(p.amountKrw)) {
        throw new PluginError('VALIDATION', `품목 금액 합 ${won(sum)}원이 품의 금액 ${won(p.amountKrw)}원과 다릅니다.`);
      }
      const bad = items.flatMap((x) => checkWriting(x.name, '품명').filter((w) => /가운뎃점|대시|물결표|둥근 따옴표|내부 참조/.test(w)));
      if (bad.length) throw new PluginError('VALIDATION', `품명 문구를 고쳐 주세요: ${bad.join('; ')}`);
      data = { rows: items.map((x) => ({ name: x.name, qty: x.qty, unitWord: x.unitWord ?? '개', amount: x.amount })) };
    }
    rt.db.transaction(() => {
      drop(kinds[action]);
      rt.db.addOverride({ owner: call.owner, kind: kinds[action], match: { key: purchaseKey }, data });
    });
    const what = { done: `처리됨 (${reason})`, incomplete: `사진 모자람 (${missing})`, items: `품목 ${items?.length}줄` }[action];
    return { text: `품의 "${p.title}" 에 적었습니다: ${what}`, data: { purchaseKey, action } };
  });

  // ------------------------------------------------------------------ mark_payment

  rt.tool(server, 'mark_payment', {
    kind: 'local',
    description: [
      '카드 결제 한 건에 사람이 알려 준 사정을 적는다. 결제는 날짜, 가맹점(카드 내역에 찍힌 이름의 일부), 금액으로 가리킨다.',
      'elsewhere: 이 카드로 결제했지만 다른 사람이 품의와 청구를 맡는다. refunded: 취소 행 없이 환불, 반품됐다.',
      'claimAsIs: 영수증도 품의도 없이 지출결의에 그대로 넣는다(purposeName 사용용도, detail 상세). pin: 금액으로 맞지 않는 결제에 영수증 파일 이름 일부를 짝지어 준다.',
      'clear: 이 결제에 적은 것을 지운다. 사람이 말하지 않은 것은 적지 않는다.',
    ].join(' '),
    input: z.object({
      date: dateArg.describe('카드 승인일'),
      merchant: z.string().min(2).max(60).describe('카드 내역의 가맹점 이름 일부'),
      amount: z.number().int().describe('카드 승인 금액(원)'),
      action: z.enum(['elsewhere', 'refunded', 'claimAsIs', 'pin', 'clear']),
      reason: z.string().max(200).optional().describe('사람이 말한 이유'),
      purposeName: z.string().max(30).optional().describe('claimAsIs 의 사용용도 이름'),
      detail: z.string().max(100).optional().describe('claimAsIs 의 상세 사용용도'),
      receipts: z.array(z.string().min(2)).optional().describe('pin 일 때 영수증 파일 이름 일부'),
    }),
  }, async ({ date, merchant, amount, action, reason, purposeName, detail, receipts }, call) => {
    const match = { date, merchant, amount };
    const kinds = { elsewhere: OVERRIDE_KINDS.elsewhere, refunded: OVERRIDE_KINDS.refunded, claimAsIs: OVERRIDE_KINDS.claimAsIs, pin: OVERRIDE_KINDS.pinned };
    const probe = { useDate: date, merchant, amount };
    const same = (o) => o.match?.date === date && o.match?.amount === amount && overrideMatches(o.match, probe);
    const drop = (kind) => rt.db.overrides(call.owner, kind).filter(same).forEach((o) => rt.db.removeOverride(call.owner, o.id));
    if (action === 'clear') {
      rt.db.transaction(() => Object.values(kinds).forEach(drop));
      return { text: `${date} ${merchant} ${won(amount)}원에 적은 것을 지웠습니다.`, data: { match, action } };
    }
    if (!reason) throw new PluginError('VALIDATION', '사람이 말한 이유(reason)를 적어 주세요.');
    let data = { why: reason };
    if (action === 'claimAsIs') data = { ...data, purposeName: purposeName ?? '', detail: detail ?? '' };
    if (action === 'pin') {
      if (!receipts?.length) throw new PluginError('VALIDATION', 'pin 에는 영수증 파일 이름 일부(receipts)가 필요합니다.');
      const files = rt.db.receipts(call.owner).map((r) => r.file.normalize('NFC'));
      const unknown = receipts.filter((part) => !files.some((f) => f.includes(part.normalize('NFC'))));
      if (unknown.length) throw new PluginError('NOT_FOUND', `저장된 영수증에서 찾지 못했습니다: ${unknown.join(', ')}`);
      data = { ...data, receipts };
    }
    rt.db.transaction(() => {
      drop(kinds[action]);
      rt.db.addOverride({ owner: call.owner, kind: kinds[action], match, data });
    });
    const what = { elsewhere: '다른 사람이 처리', refunded: '환불', claimAsIs: '그대로 청구', pin: `영수증 짝 ${receipts?.length}개` }[action];
    return { text: `${date} ${merchant} ${won(amount)}원: ${what}로 적었습니다.`, data: { match, action } };
  });

  // ------------------------------------------------------------------ inspection_candidates

  rt.tool(server, 'inspection_candidates', {
    kind: 'read',
    description: [
      '검수조서가 필요한 카드 결제를 품목, 받은 사진, 찍을 사진, 시리얼 번호, 올릴 수 있는지와 함께 보여 준다(조회만 한다).',
      '사진을 받기 전이나 검수조서를 올리기 전에 쓴다. 해외 배송은 몇 주 걸리니 도착 전 결제의 사진이 없는 것은 정상이다.',
      '기간을 안 주면 이번 달이다. 품의 키(purchaseKey)와 결제 키(payment)가 결과에 있다.',
    ].join(' '),
    input: z.object(periodShape),
  }, async (args, call) => rt.session(call, async ({ api }) => {
    const today = kstDate();
    const [from, to] = periodOf(args, [`${today.slice(0, 7)}-01`, today]);
    const ctx = await loadContext(rt, call, api, { card: args.card, from, to });
    const filed = await filedByTitle(api);
    const filings = rt.db.filings(call.owner, { kind: 'inspection' });
    const serialFromKrw = rt.config.inspection?.serialFromKrw;
    const units = [];
    for (const u of ctx.units) {
      const ps = u.ps;
      const service = ps.every(intangible);
      const got = service ? [] : photosOfKeys(ctx.photos, ps.map((p) => p.key));
      const serials = service ? [] : ps.flatMap((p) => serialNeeds(p, { serialFromKrw, itemRows: ctx.marks.itemRows }));
      let state;
      let reason = null;
      let title = null;
      const record = filings.find((f) => f.ref === u.key && f.docId);
      try {
        const plan = planReport({
          unit: u, card: ctx.card, photos: ctx.photos, marks: ctx.marks, docNums: new Map(), profile: ctx.account,
          config: rt.config, receiptPath: pathIn(receiptsDir(call.owner)), photoPath: pathIn(photosDir(call.owner)),
        });
        title = plan.title;
        const docs = filed.get(plan.title) ?? [];
        state = record || docs.length ? '올림' : '올릴 수 있음';
        if (state === '올림') reason = `문서 ${record?.docId ?? docs.map((d) => d.documentId).join(', ')}`;
      } catch (e) {
        if (!(e instanceof PluginError)) throw e;
        state = e.code === 'CONFLICT' ? '따로 처리됨' : '아직';
        reason = e.message.split('\n')[0];
      }
      units.push({
        key: u.key, date: String(u.a.row.useDate).slice(0, 10), merchant: u.a.row.merchant, amount: u.a.row.amount,
        perOrder: u.perOrder, service, state, reason, title,
        purchases: ps.map((p) => ({ key: p.key, title: p.title, docId: p.docId })),
        items: ps.flatMap((p) => itemLines(p)),
        photos: got.map((ph) => ({ id: ph.id, shows: ph.shows, warn: ph.data?.warn ?? null, takenDate: ph.takenDate })),
        shots: service || got.length ? [] : photosFor(ps),
        serialMissing: serials.filter((s) => !s.numbers.length).map((s) => s.item),
      });
    }
    const unknown = ctx.m.audited.filter((a) => !a.purchase && !a.receipt && a.kind === '구매' && !a.claimed && !a.claimAsIs)
      .map((a) => ({ date: String(a.row.useDate).slice(0, 10), merchant: a.row.merchant, amount: a.row.amount }));
    const covered = new Set(units.flatMap((u) => u.purchases.map((p) => p.key)));
    // 짝이 맞지 않는 사진: 없는 품의 키를 단 사진, 또는 이 기간에 결제한 품의인데 결제와 이어지지 않은 사진.
    const paidIn = (key) => {
      const paid = ctx.purchasesByKey.get(key)?.paidAt ?? '';
      return paid >= from && paid <= to;
    };
    const strays = ctx.photos.filter((ph) => !ph.otherCard && ph.purchaseKey && !covered.has(ph.purchaseKey)
      && (!ctx.purchasesByKey.has(ph.purchaseKey) || paidIn(ph.purchaseKey)));

    const L = [`검수조서 대상 · ${ctx.card.name} (${last4Of(ctx.card)}) · ${from}부터 ${to}까지 · ${units.length}건`];
    units.slice(0, 40).forEach((u, i) => {
      L.push('', `${i + 1}. ${u.date.slice(5)} ${u.merchant} ${won(u.amount)}원 · ${u.state}${u.reason ? ` (${u.reason})` : ''}`);
      L.push(`   결제 키 ${u.key}${u.perOrder ? ' (주문마다 한 장)' : ''}`);
      for (const p of u.purchases) L.push(`   품의 ${p.title} [${p.key}]${p.docId ? '' : ' (아직 올리지 않음)'}`);
      if (u.service) L.push('   구독·이용료: 사진 대신 인보이스를 붙인다');
      else L.push(`   사진 ${u.photos.length}장${u.photos.length ? `: ${u.photos.map((ph) => `${ph.id} ${ph.shows}${ph.warn ? ` (확인 필요: ${ph.warn})` : ''}`).join('; ')}` : ''}`);
      if (u.shots.length) L.push(`   찍을 사진: ${u.shots.join('; ')}`);
      if (u.serialMissing.length) L.push(`   시리얼 번호가 보이게 찍을 것: ${u.serialMissing.join(', ')}`);
    });
    if (units.length > 40) L.push('', `(앞의 40건만 보입니다. 기간을 좁혀 주세요.)`);
    if (unknown.length) {
      L.push('', '품의도 영수증도 없는 결제 (무엇을 샀는지 물어봐야 한다):');
      for (const x of unknown) L.push(`- ${x.date} ${x.merchant} ${won(x.amount)}원`);
    }
    if (strays.length) {
      L.push('', '이 기간 결제와 짝이 맞지 않는 사진 (품의 키를 확인한다):');
      for (const ph of strays) L.push(`- 사진 ${ph.id} → ${ph.purchaseKey}: ${ph.shows ?? ''}`);
    }
    const other = ctx.photos.filter((ph) => ph.otherCard);
    if (other.length) L.push('', `다른 카드로 산 물건 사진 ${other.length}장은 이 목록에 넣지 않았습니다.`);
    return {
      text: L.join('\n'),
      data: { card: { name: ctx.card.name, last4: last4Of(ctx.card) }, from, to, units, unknown, strays: strays.map((ph) => ({ id: ph.id, purchaseKey: ph.purchaseKey })) },
    };
  }));

  // ------------------------------------------------------------------ payment_status

  rt.tool(server, 'payment_status', {
    kind: 'read',
    description: '카드 한 장의 결제마다 영수증, 구매 품의, 검수 사진, 시리얼 번호, 검수조서, 지출결의가 갖춰졌는지 표와 남은 일로 보여 준다(조회만 한다). 기간을 안 주면 이번 달이다.',
    input: z.object(periodShape),
  }, async (args, call) => rt.session(call, async ({ api }) => {
    const today = kstDate();
    const [from, to] = periodOf(args, [`${today.slice(0, 7)}-01`, today]);
    const ctx = await loadContext(rt, call, api, { card: args.card, from, to });
    const status = paymentStatus({
      audited: ctx.m.audited, purchasesByKey: ctx.purchasesByKey, photos: ctx.photos, overrides: ctx.overrides,
      filings: rt.db.filings(call.owner, { kind: 'inspection' }), marks: ctx.marks, config: rt.config,
    });
    return {
      text: describeStatus(status, { title: `결제별 처리 현황 · ${ctx.card.name} (${last4Of(ctx.card)}) · ${from}부터 ${to}까지` }),
      data: { card: { name: ctx.card.name, last4: last4Of(ctx.card) }, from, to, ...status },
    };
  }));

  // ------------------------------------------------------------------ file_inspection_report

  /** sha256 of a stored file, for the preview digest: a re-sent photo or receipt changes it. */
  const fileHash = (file) => createHash('sha256').update(fs.readFileSync(file)).digest('hex');

  /** How long an unconfirmed submission blocks the payment before the draft box is trusted. */
  const PENDING_MS = 15 * 60_000;

  /**
   * Filings left at `submitting` (the run stopped after it reached DaouOffice) are settled first:
   * found in the draft box → recorded with that document; otherwise they block the payment for
   * PENDING_MS, then they are marked `lost`.
   */
  function settlePending(call, records, filed) {
    for (const f of records.filter((r) => r.status === 'submitting')) {
      const skip = new Set((f.detail?.oldIds ?? []).map(String));
      const docs = (filed.get(f.title) ?? []).filter((d) => !skip.has(String(d.documentId)));
      if (docs.length) {
        rt.db.updateFiling(call.owner, f.id, {
          docId: String(docs[0].documentId), status: docs[0].docStatusName, detail: { ...f.detail, settledAt: kstIso() },
        });
        continue;
      }
      const withdrawn = f.detail?.withdrawn ?? [];
      if (Date.now() - Date.parse(f.filedAt) < PENDING_MS) {
        throw new PluginError('CONFLICT', [
          `이 결제의 직전 상신(${f.title}) 결과를 아직 확인하지 못했습니다. 내 기안함(my_drafts)이나 read_document 로 올라갔는지 확인해 주세요.`,
          withdrawn.length ? `그때 옛 문서 ${withdrawn.join(', ')} 는 상신취소돼 임시저장에 있습니다.` : null,
          '몇 분 뒤 다시 요청하면 내 기안함을 다시 확인합니다.',
        ].filter(Boolean).join(' '), { data: { pendingFiling: f.id } });
      }
      rt.db.updateFiling(call.owner, f.id, { status: 'lost', detail: { ...f.detail, settledAt: kstIso() } });
    }
  }

  const SUFFIX = / \(\d+월 \d+일 결제\)$/;

  /**
   * The old reports a fix or refile acts on. They come from this payment's filings first (by
   * document id), so a report of another payment with the same title is never touched. Only when no
   * filing names one does the title decide, and then only if exactly one report matches (or
   * `oldDocId` picks it). Each must be a 검수조서 drafted by this person.
   */
  async function resolveOldReports(call, api, plan, records, filed, oldDocId, me) {
    const recorded = records.filter((f) => f.docId && !['submitting', 'lost'].includes(f.status)).map((f) => f.docId);
    let olds;
    if (oldDocId) {
      olds = await oldReports(api, [oldDocId]);
      if (!olds.length) throw new PluginError('NOT_FOUND', `문서 ${oldDocId} 를 찾지 못했습니다.`);
      const base = plan.title.replace(SUFFIX, '');
      if (!recorded.includes(String(oldDocId)) && !olds[0].title.startsWith(base)) {
        throw new PluginError('VALIDATION', `문서 ${oldDocId} 는 이 결제의 검수조서가 아닙니다 (${olds[0].title}).`);
      }
    } else if (recorded.length) {
      olds = (await oldReports(api, recorded)).filter((o) => o.status !== '임시저장');
    } else {
      const base = plan.title;
      const matches = [...filed.entries()].filter(([title]) => title === base || (title.startsWith(`${base} (`) && SUFFIX.test(title)))
        .flatMap(([, docs]) => docs);
      if (matches.length > 1) {
        throw new PluginError('VALIDATION', `같은 제목의 검수조서가 ${matches.length}건 있습니다: ${matches.map((d) => `${d.documentId} (${d.title})`).join(', ')}. 어느 문서인지 물어 oldDocId 로 알려 주세요.`);
      }
      olds = await oldReports(api, matches.map((d) => d.documentId));
    }
    olds = olds.filter((o) => o.status !== '임시저장');
    if (!olds.length) {
      throw new PluginError('VALIDATION', `고치거나 대신할 옛 검수조서를 찾지 못했습니다: ${plan.title}. 이미 상신취소해 임시저장으로 돌아갔다면 action new 로 올립니다.`);
    }
    for (const o of olds) {
      if (o.formName && o.formName !== FORM_NAME) throw new PluginError('CONFLICT', `문서 ${o.id} 는 검수조서가 아닙니다 (${o.formName}).`);
      if (o.drafter && me && o.drafter !== me) throw new PluginError('CONFLICT', `문서 ${o.id} 는 다른 사람(${o.drafter})이 올린 문서라 건드리지 않습니다.`);
    }
    return olds;
  }

  /** 미리보기와 실행이 같은 계산을 한다. 실행은 이것을 처음부터 다시 돌고, 미리보기와 다르면 멈춘다. */
  async function prepareReport(call, { api }, { payment, purchaseKey, card, action = 'new', oldDocId }) {
    const [day, amountText] = payment.split('/');
    const [mStart, mEnd] = monthRange(day.slice(0, 7));
    const today = kstDate();
    const to = addDays(mEnd, 7) > today ? today : addDays(mEnd, 7);
    const ctx = await loadContext(rt, call, api, { card, from: addDays(mStart, -7), to });
    const candidates = ctx.units.filter((u) => u.key.split('#')[0] === `${day}/${amountText}`);
    if (!candidates.length) {
      throw new PluginError('NOT_FOUND', `결제 ${payment} 를 찾지 못했습니다. inspection_candidates 의 결제 키를 확인해 주세요 (품의가 짝지어지지 않은 결제는 대상이 아닙니다).`);
    }
    let unit = candidates[0];
    if (candidates.length > 1 || candidates[0].perOrder) {
      unit = candidates.find((u) => u.ps[0].key === purchaseKey);
      if (!unit) {
        throw new PluginError('VALIDATION', `이 결제는 품의마다 검수조서를 한 장씩 씁니다. purchaseKey 를 골라 주세요: ${candidates.map((u) => `${u.ps[0].key} (${u.ps[0].title})`).join(', ')}`);
      }
    }
    const docNums = await docNumbers(api, [...unit.ps.map((p) => p.docId), unit.a.row.docId]);
    const plan = planReport({
      unit, card: ctx.card, photos: ctx.photos, marks: ctx.marks, docNums, profile: ctx.account, config: rt.config,
      receiptPath: pathIn(receiptsDir(call.owner)), photoPath: pathIn(photosDir(call.owner)),
    });

    const filed = await filedByTitle(api);
    settlePending(call, rt.db.filings(call.owner, { kind: 'inspection', ref: plan.key }), filed);
    const records = rt.db.filings(call.owner, { kind: 'inspection', ref: plan.key });
    const opts = { withdraw: [], links: [], oldIds: [] };
    const notes = [];
    if (action === 'new') {
      // A filing counts while its document is still up (a manually withdrawn one sits in 임시저장).
      const liveIds = new Set([...filed.values()].flat().map((d) => String(d.documentId)));
      const mine = records.filter((f) => f.docId && liveIds.has(String(f.docId)));
      if (mine.length) {
        throw new PluginError('CONFLICT', `이 결제의 검수조서가 이미 올라가 있습니다 (문서 ${mine.map((f) => f.docId).join(', ')}). 고치려면 action fix, 끝난 문서를 대신하려면 refile 입니다.`);
      }
      if (filed.has(plan.title)) {
        // 같은 구독을 같은 달에 두 번 결제하면 제목이 겹친다. 다른 결제로 올린 기록이 있으면 결제일을 붙여 가른다.
        const sameTitle = rt.db.filings(call.owner, { kind: 'inspection' }).filter((f) => f.title === plan.title);
        const otherKey = sameTitle.some((f) => f.ref !== plan.key);
        if (!otherKey) {
          throw new PluginError('CONFLICT', `같은 제목의 검수조서가 이미 올라가 있습니다 (문서 ${filed.get(plan.title).map((d) => d.documentId).join(', ')}). 고치려면 action fix, 끝난 문서를 대신하려면 refile 입니다.`);
        }
        const [, mm, dd] = day.split('-');
        plan.title = `${plan.title} (${Number(mm)}월 ${Number(dd)}일 결제)`;
        if (filed.has(plan.title)) throw new PluginError('CONFLICT', `같은 제목의 검수조서가 이미 올라가 있습니다: ${plan.title}`);
      }
    } else {
      const olds = await resolveOldReports(call, api, plan, records, filed, oldDocId, ctx.account?.name);
      // 고친 문서는 옛 문서가 쓰던 결제일 꼬리표를 그대로 단다(같은 달 같은 구독을 가르는 표시).
      const suffix = olds.map((o) => SUFFIX.exec(o.title)?.[0]).find(Boolean);
      if (suffix && !SUFFIX.test(plan.title)) plan.title = `${plan.title}${suffix}`;
      if (action === 'refile') {
        const live = olds.filter((o) => o.status !== '완료');
        if (live.length) throw new PluginError('CONFLICT', `진행 중인 옛 문서가 있습니다 (${live.map((o) => o.id).join(', ')}). 결재 전이면 action fix 로 상신취소하고 고쳐 올립니다.`);
        opts.links = olds.filter((o) => o.docNum).map((o) => ({ id: o.id, docNum: o.docNum }));
        opts.oldIds = olds.map((o) => o.id);
        notes.push(`끝난 옛 문서 ${olds.map((o) => o.docNum ?? o.id).join(', ')} 를 관련문서로 연결하고 새로 올립니다.`);
      } else {
        const done = olds.filter((o) => o.status === '완료');
        if (done.length) throw new PluginError('CONFLICT', `결재가 끝난 문서는 고칠 수 없습니다 (${done.map((o) => o.id).join(', ')}). 댓글로 정정하거나 action refile 을 씁니다.`);
        const locked = olds.filter((o) => !o.canCancel);
        if (locked.length) throw new PluginError('CONFLICT', `결재가 진행돼 상신취소할 수 없습니다 (${locked.map((o) => o.id).join(', ')}). 결재자나 회계에게 반려를 부탁해야 합니다.`);
        opts.withdraw = olds.map((o) => o.id);
        opts.oldIds = olds.map((o) => o.id);
        notes.push(`새 문서를 다 채운 뒤 옛 문서 ${olds.map((o) => `${o.id} (${o.title})`).join(', ')} 를 상신취소하고 바로 올립니다.`);
      }
    }
    const digest = digestOf({
      action, key: plan.key, title: plan.title, header: plan.header, total: plan.total,
      lines: plan.lines.map((l) => [l.name, l.vendor, l.qty, l.supply, l.vat, l.total]),
      approvers: plan.approvers,
      files: plan.files.map((f) => [f.kind, f.name, fileHash(f.path)]),
      photoIds: plan.photoIds, withdraw: opts.withdraw, links: opts.links.map((l) => l.id), oldIds: opts.oldIds,
    });
    return { ctx, plan, opts, notes, digest };
  }

  rt.tool(server, 'file_inspection_report', {
    kind: 'write',
    previewDigest: true,
    description: [
      '카드 결제 한 건의 검수조서를 올린다. 이 도구는 미리보기(제목, 칸, 세부내역, 첨부, 결재선, 사진)를 만들 뿐이고, 사람이 승인 버튼을 눌러야 상신된다.',
      '품목이 모두 찍힌 사진이 있어야 한다. 구독과 이용료는 사진 대신 인보이스를 붙인다. 먼저 inspection_candidates 로 "올릴 수 있음" 인지 확인하고, 사람에게 어느 결제를 올릴지 확인받는다.',
      '주문마다 품의를 따로 쓰는 결제는 purchaseKey 로 품의를 고른다. action: new(새로), fix(결재 전 문서를 상신취소하고 고쳐 올림), refile(결재가 끝난 문서를 대신할 새 문서를 올리고 연결).',
      '같은 제목의 옛 문서가 여럿이면 어느 것인지 물어 oldDocId 로 준다.',
    ].join(' '),
    input: z.object({
      payment: z.string().regex(/^\d{4}-\d{2}-\d{2}\/-?\d+$/, 'YYYY-MM-DD/금액').describe('결제 키. 예: 2026-03-02/45000'),
      purchaseKey: keyArg.optional().describe('주문마다 한 장씩 쓰는 결제에서 고를 품의'),
      card: cardArg,
      action: z.enum(['new', 'fix', 'refile']).optional().describe('기본 new'),
      oldDocId: z.string().regex(/^\d{6,25}$/).optional().describe('fix, refile 에서 고칠 옛 문서 ID. 같은 제목이 여럿일 때만'),
    }),
  }, {
    async preview(args, call) {
      return rt.session(call, async (s) => {
        const { plan, notes, digest } = await prepareReport(call, s, args);
        const photos = plan.files.filter((f) => f.kind === 'photo').map((f) => f.path);
        return {
          text: [
            describePlan(plan),
            ...notes.map((n) => `동작: ${n}`),
            photos.length > THUMBS ? `사진 ${photos.length}장 가운데 앞의 ${THUMBS}장만 아래에 작게 보입니다. 나머지도 모두 첨부됩니다.` : null,
            '',
            '승인하면 이대로 상신합니다. 그사이 사진, 영수증, 품의가 바뀌면 상신하지 않고 미리보기를 다시 만듭니다. 올린 뒤 서버의 문서를 칸마다 다시 읽어 대조합니다.',
          ].filter((l) => l !== null).join('\n'),
          images: await thumbnails(photos),
          data: { payment: plan.key, title: plan.title, action: args.action ?? 'new', files: plan.files.map((f) => f.name), photos: photos.length, approvers: plan.approvers },
          digest,
        };
      });
    },
    async execute(args, call) {
      return rt.session(call, async (s) => {
        const { ctx, plan, opts, digest } = await prepareReport(call, s, args);
        call.assertPreviewed(digest);
        const form = await findForm(s.api, FORM_NAME);
        const detail = {
          action: args.action ?? 'new', oldIds: opts.oldIds, files: plan.files.map((f) => f.name), photoIds: plan.photoIds,
          purchaseKeys: plan.purchases.map((p) => p.key), purchaseId: plan.payment.rowId, withdrawn: [],
        };
        let filing = null;
        // Recorded right before the first irreversible step, so a run that stops after it leaves a trace.
        const onStage = (stage, info) => {
          filing ??= rt.db.addFiling({ owner: call.owner, kind: 'inspection', ref: plan.key, title: plan.title, status: 'submitting', detail });
          if (stage === 'withdrawn') {
            detail.withdrawn.push(info.id);
            rt.db.updateFiling(call.owner, filing.id, { detail });
          }
        };
        let res;
        try {
          res = await fileReport(s.page, s.api, s.baseUrl, plan, { formId: form.id, deptId: ctx.account?.deptId, ...opts, onStage });
        } catch (e) {
          if (!detail.withdrawn.length || !(e instanceof PluginError)) throw e;
          throw new PluginError(e.code,
            `${e.message} 옛 문서 ${detail.withdrawn.join(', ')} 는 상신취소돼 임시저장에 있습니다. 내 기안함에서 확인한 뒤 action new 로 다시 올립니다.`,
            { cause: e, data: { ...(e.data ?? {}), withdrawn: detail.withdrawn } });
        }
        const problems = [
          ...(res.titleOk ? [] : ['제목']),
          ...(res.attachCount === plan.files.length ? [] : [`첨부 ${res.attachCount}/${plan.files.length}`]),
          ...res.diffs.map((d) => `칸 ${d.index + 1}: "${d.got ?? ''}" (기대 "${d.want}")`),
        ];
        rt.db.updateFiling(call.owner, filing.id, { docId: res.docId, status: problems.length ? 'mismatch' : res.status, detail: { ...detail, problems } });
        if (problems.length) {
          throw new PluginError('UPSTREAM',
            `검수조서는 올라갔지만(문서 ${res.docId}) 서버 내용이 계획과 다릅니다: ${problems.slice(0, 6).join('; ')}. 다우오피스에서 확인하고, 결재 전이면 action fix 로 고쳐 올려 주세요.`,
            { data: { docId: res.docId, problems } });
        }
        return {
          text: `검수조서를 올렸습니다: ${plan.title} (문서 ${res.docId}, ${res.status}). 서버 문서의 제목, 칸 ${plan.lines.length}줄, 첨부 ${plan.files.length}개가 계획과 같습니다.`,
          data: { docId: res.docId, status: res.status, title: plan.title, payment: plan.key, verified: true, withdrawn: res.withdrawn },
        };
      });
    },
  });

  // ------------------------------------------------------------------ attach_photos_to_claim

  /** 아직 지출결의에 붙이지 않은 사진을 지출결의 문서별 댓글로 묶는다. */
  async function planComments(call, { api }, { card, from, to, photoIds }) {
    const today = kstDate();
    const [f, t] = periodOf({ from, to }, [addDays(today, -120), today]);
    const ctx = await loadContext(rt, call, api, { card, from: f, to: t });
    const me = ctx.account?.name;
    if (!me) throw new PluginError('VALIDATION', '내 이름을 아직 모릅니다. 계정 정보를 먼저 읽어야 합니다(me 도구).');
    const posted = (ph) => (ph.comments ?? []).some((c) => c.target === 'claim');
    const pick = ctx.photos.filter((ph) => !ph.otherCard && ph.purchaseKey && (photoIds?.length ? photoIds.includes(ph.id) : !posted(ph)));
    const claimOf = new Map();
    for (const a of ctx.m.audited) {
      if (!a.row.docId) continue;
      for (const p of a.purchases ?? []) claimOf.set(p.key, String(a.row.docId));
    }
    const docs = new Map();
    const docInfo = async (id) => {
      if (!docs.has(id)) {
        const d = (await api(`/gw/api/approval/document/${id}`)).data?.data?.document;
        docs.set(id, d ? { title: d.title ?? '', drafter: d.drafterName ?? '' } : null);
      }
      return docs.get(id);
    };
    const groups = new Map();
    const skipped = [];
    for (const ph of pick) {
      if (photoIds?.length && posted(ph)) { skipped.push(`사진 ${ph.id}: 이미 지출결의에 붙였습니다`); continue; }
      const keys = [ph.purchaseKey, ...(ph.alsoFor ?? [])];
      const docId = keys.map((k) => claimOf.get(k)).find(Boolean);
      if (!docId) { skipped.push(`사진 ${ph.id}: 그 결제가 아직 지출결의에 들어가지 않았습니다`); continue; }
      const info = await docInfo(docId);
      if (!info) { skipped.push(`사진 ${ph.id}: 지출결의 ${docId} 를 읽지 못했습니다`); continue; }
      if (info.drafter !== me) { skipped.push(`사진 ${ph.id}: 다른 사람(${info.drafter || '모름'})이 올린 지출결의입니다`); continue; }
      const p = ctx.purchasesByKey.get(ph.purchaseKey);
      const label = String(p?.title ?? ph.purchaseKey).replace(/의 건$/, '');
      if (!groups.has(docId)) groups.set(docId, { docId, title: info.title, items: new Map() });
      const g = groups.get(docId);
      if (!g.items.has(label)) g.items.set(label, []);
      g.items.get(label).push(ph);
    }
    // 한 댓글에 사진 10장, 30MB 까지. 큰 업로드가 덜 끝난 채 등록되면 댓글이 조용히 사라졌다.
    const dir = photosDir(call.owner);
    const comments = [];
    for (const g of groups.values()) {
      const parts = [];
      let cur = new Map(); let size = 0; let count = 0;
      for (const [label, list] of g.items) {
        const bytes = list.reduce((sum, ph) => {
          const f = inside(dir, ph.file);
          return sum + (f ? fs.statSync(f).size : 0);
        }, 0);
        if (cur.size && (size + bytes > 30 * 1024 * 1024 || count + list.length > 10)) { parts.push(cur); cur = new Map(); size = 0; count = 0; }
        cur.set(label, list); size += bytes; count += list.length;
      }
      if (cur.size) parts.push(cur);
      parts.forEach((items, i) => {
        const head = parts.length > 1 ? `검수 사진을 첨부합니다 (${i + 1}/${parts.length}).` : '검수 사진을 첨부합니다.';
        const text = [head, ...[...items].map(([label, list]) => `- ${label}: ${list.map((ph) => ph.shows).filter(Boolean).join(', ')}`)].join('\n');
        const photos = [...items.values()].flat();
        comments.push({ docId: g.docId, docTitle: g.title, text, photos });
      });
    }
    const digest = digestOf(comments.map((c) => ({
      docId: c.docId, text: c.text, photos: c.photos.map((ph) => [ph.id, ph.sha256]),
    })));
    return { comments, skipped, from: f, to: t, me, digest };
  }

  rt.tool(server, 'attach_photos_to_claim', {
    kind: 'write',
    previewDigest: true,
    description: [
      '검수 사진을 그 물건 결제가 들어간 지출결의(법인카드 지출품의서)에 댓글로 붙인다. 아직 붙이지 않은 사진만 붙이고, 붙인 사진은 기록해 다시 올리지 않는다.',
      '댓글은 결재선 사람에게 알림이 가므로 사람이 붙이라고 했을 때만 쓴다. 이 도구는 미리보기를 만들 뿐이고 사람이 승인 버튼을 눌러야 올라간다.',
      '지출결의에 아직 들어가지 않은 결제와 다른 사람이 올린 지출결의는 건너뛴다.',
    ].join(' '),
    input: z.object({
      card: cardArg,
      from: dateArg.optional().describe('결제를 찾을 기간 시작. 기본 120일 전'),
      to: dateArg.optional(),
      photoIds: z.array(z.number().int().min(1)).optional().describe('이 사진들만 붙인다'),
    }),
  }, {
    async preview(args, call) {
      return rt.session(call, async (s) => {
        const { comments, skipped, digest } = await planComments(call, s, args);
        if (!comments.length) {
          throw new PluginError('VALIDATION', ['붙일 사진이 없습니다.', ...skipped].join('\n'));
        }
        const L = [];
        for (const c of comments) L.push(`지출결의 ${c.docId} (${c.docTitle}) · 사진 ${c.photos.length}장`, c.text, '');
        if (skipped.length) L.push('건너뜀:', ...skipped.map((x) => `- ${x}`), '');
        const total = comments.reduce((t, c) => t + c.photos.length, 0);
        if (total > THUMBS) L.push(`사진 ${total}장 가운데 앞의 ${THUMBS}장만 아래에 작게 보입니다.`);
        L.push('승인하면 이 댓글을 답니다. 결재선 사람에게 알림이 갑니다. 그사이 사진이나 짝이 바뀌면 달지 않고 미리보기를 다시 만듭니다.');
        const dir = photosDir(call.owner);
        return {
          text: L.join('\n'),
          images: await thumbnails(comments.flatMap((c) => c.photos).map((ph) => inside(dir, ph.file)).filter(Boolean)),
          data: { comments: comments.map((c) => ({ docId: c.docId, photoIds: c.photos.map((ph) => ph.id) })), skipped },
          digest,
        };
      });
    },
    async execute(args, call) {
      return rt.session(call, async (s) => {
        const { comments, skipped, me, digest } = await planComments(call, s, args);
        call.assertPreviewed(digest);
        if (!comments.length) throw new PluginError('CONFLICT', ['붙일 사진이 없습니다.', ...skipped].join('\n'));
        const dir = photosDir(call.owner);
        const done = [];
        for (const c of comments) {
          const files = c.photos.map((ph) => inside(dir, ph.file));
          if (files.some((f) => !f)) throw new PluginError('VALIDATION', `사진 파일이 저장돼 있지 않습니다 (지출결의 ${c.docId}).`);
          // postComment skips a comment that is already there with the same text (a run that stopped after posting).
          const made = await postComment(s.page, s.api, s.baseUrl, c.docId, c.text, files, { authorName: me });
          const at = kstIso();
          // 하나 올릴 때마다 적는다. 중간에 멈춰도 올린 사진은 다시 올리지 않는다.
          for (const ph of c.photos) {
            rt.db.updatePhoto(call.owner, ph.id, { comments: [...(ph.comments ?? []), { target: 'claim', docId: c.docId, commentId: made?.id ?? null, at }] });
          }
          done.push({ docId: c.docId, commentId: made?.id ?? null, photos: c.photos.length, existing: Boolean(made?.existing) });
        }
        return {
          text: [`지출결의 댓글 ${done.length}개에 사진 ${done.reduce((t, d) => t + d.photos, 0)}장을 붙였습니다. 서버에서 첨부 수를 확인했습니다.`,
            ...done.map((d) => `- 지출결의 ${d.docId}: 사진 ${d.photos}장${d.existing ? ' (같은 댓글이 이미 있어 다시 달지 않았습니다)' : ''}`),
            ...(skipped.length ? ['건너뜀:', ...skipped.map((x) => `- ${x}`)] : [])].join('\n'),
          data: { comments: done, skipped },
        };
      });
    },
  });
}

/**
 * 영수증 파일 이름: `날짜_비목_품명_구매처_금액통화_종류.확장자`. 카드 짝짓기의 예외(pin)가 이 이름을 본다.
 * 파일 시스템의 이름 한도(255바이트)를 넘지 않게 UTF-8 바이트로 자른다. 한글은 글자당 3바이트다.
 */
export const RECEIPT_NAME_MAX_BYTES = 240;
const cutBytes = (text, max) => {
  let out = '';
  for (const ch of text) {
    if (Buffer.byteLength(out + ch) > max) break;
    out += ch;
  }
  return out;
};
export function receiptFileName(r, ext, n = 1) {
  const clean = (s, max) => String(s).normalize('NFC').replace(/[\\/:*?"<>|\u0000-\u001f]/g, ' ')
    .replace(/_/g, '-').replace(/\s+/g, ' ').trim().slice(0, max);
  let item = clean(r.item, 60).replace(/ /g, '-');
  let vendor = clean(r.vendor, 40);
  let category = clean(r.category, 20);
  const amount = r.currency === '원' ? String(Math.round(r.amount)) : String(r.amount);
  const tail = `_${amount}${r.currency}_${r.kind}${n > 1 ? `-${n}` : ''}.${ext}`;
  const build = () => `${r.date}_${category}_${item}_${vendor}${tail}`;
  // Shorten the free-text parts, longest first, until the name fits.
  for (const part of ['item', 'vendor', 'category']) {
    const over = Buffer.byteLength(build()) - RECEIPT_NAME_MAX_BYTES;
    if (over <= 0) break;
    const current = { item, vendor, category }[part];
    const keep = cutBytes(current, Math.max(1, Buffer.byteLength(current) - over));
    if (part === 'item') item = keep; else if (part === 'vendor') vendor = keep; else category = keep;
  }
  return build();
}
