import fs from 'node:fs';
import path from 'node:path';
import * as z from 'zod';
import { PluginError } from './errors.mjs';

/**
 * Company rules. The file lives on the server only (`DAOUOFFICE_CONFIG`); the repository ships
 * `config.example.json` with placeholders. Names of approvers, card ↔ project mappings and
 * department codes are business settings of one company, so they never go into the code.
 *
 * Unknown keys are kept so later modules can add their own sections without touching this file.
 */

const lineEntry = z.object({
  name: z.string().min(1).optional(),
  role: z.enum(['ceo', 'cfo', 'cto']).optional(),
  type: z.enum(['결재', '합의', '확인']).default('결재'),
  dept: z.string().min(1).optional(),
}).refine((e) => e.name || e.role, { message: 'name 또는 role 이 필요하다' });

const lineSpec = z.union([z.literal('project'), z.literal('ctoFinal'), z.array(lineEntry)]);

export const ConfigSchema = z.looseObject({
  /** DaouOffice is reached over https only: credentials go through this origin. */
  baseUrl: z.url({ protocol: /^https$/ }).optional(),
  people: z.looseObject({
    ceo: z.string().min(1).optional(),
    cfo: z.string().min(1).optional(),
    cto: z.string().min(1).optional(),
  }).default({}),
  cfoFrom: z.number().int().nonnegative().default(1_000_000),
  projects: z.record(z.string(), z.looseObject({
    reviewer: z.string().min(1).optional(),
    subProjects: z.array(z.string()).default([]),
  })).default({}),
  budgets: z.array(z.string()).default(['운영자금', '프로젝트 사업비']),
  cards: z.record(z.string().regex(/^\d{4}$/), z.looseObject({
    project: z.string().optional(),
    budget: z.string().optional(),
    issuer: z.string().optional(),
  })).default({}),
  lines: z.looseObject({
    purchase: lineSpec.optional(),
    claim: lineSpec.optional(),
    inspection: lineSpec.optional(),
  }).default({}),
  trip: z.looseObject({
    approvers: z.array(lineEntry).optional(),
    referers: z.array(lineEntry).optional(),
  }).default({}),
  purposeMap: z.array(z.any()).default([]),
  minPurchaseKrw: z.number().int().nonnegative().default(100_000),
  /** Fallback when the org tree does not give a department code: { "<dept name>": { code, id } }. */
  departments: z.record(z.string(), z.looseObject({
    code: z.string().optional(),
    id: z.string().optional(),
  })).default({}),
  inspection: z.looseObject({
    /** Card merchants whose charges get one 검수조서 per requisition (one per order receipt). */
    perOrderMerchants: z.array(z.string()).default(['ALIEXPRESS']),
    /** Payments from this date on follow the per-order rule; unset = always. */
    perOrderFrom: z.string().regex(/^\d{4}-\d{2}-\d{2}$/).optional(),
    /** Items at or above this amount (KRW) need a readable serial photo, besides the device keywords. */
    serialFromKrw: z.number().int().positive().optional(),
  }).default({ perOrderMerchants: ['ALIEXPRESS'] }),
  cardMatch: z.looseObject({
    vendorAliases: z.record(z.string(), z.string()).default({}),
    subscriptionMerchants: z.array(z.string()).default([]),
    requisitionFrom: z.string().regex(/^\d{4}-\d{2}-\d{2}$/).optional(),
  }).default({ vendorAliases: {}, subscriptionMerchants: [] }),
});

/** Validates an object. Throws `PluginError('INTERNAL')` listing every problem. */
export function parseConfig(raw, source = 'config') {
  const r = ConfigSchema.safeParse(raw ?? {});
  if (!r.success) {
    const issues = r.error.issues.map((i) => `${i.path.join('.') || '(root)'}: ${i.message}`).join('; ');
    throw new PluginError('INTERNAL', `회사 설정이 잘못됐습니다 (${source}): ${issues}`);
  }
  for (const re of [...r.data.cardMatch.subscriptionMerchants, ...r.data.inspection.perOrderMerchants]) {
    try { new RegExp(re, 'i'); } catch {
      throw new PluginError('INTERNAL', `회사 설정이 잘못됐습니다 (${source}): 정규식 "${re}" (cardMatch.subscriptionMerchants, inspection.perOrderMerchants)`);
    }
  }
  return deepFreeze(r.data);
}

/**
 * Reads the config file. No path → empty config (the server still starts and lists its tools;
 * tools that need DaouOffice report that `baseUrl` is missing). A path that does not exist or
 * does not parse is an error: a wrong path should be loud, not silently empty.
 */
export function loadConfig(file = process.env.DAOUOFFICE_CONFIG) {
  if (!file) return { config: parseConfig({}), source: null };
  const full = path.resolve(file);
  let text;
  try {
    text = fs.readFileSync(full, 'utf8');
  } catch (cause) {
    throw new PluginError('INTERNAL', `회사 설정 파일을 읽지 못했습니다: ${full}`, { cause });
  }
  let raw;
  try {
    raw = JSON.parse(text);
  } catch (cause) {
    throw new PluginError('INTERNAL', `회사 설정 파일이 JSON 이 아닙니다: ${full}`, { cause });
  }
  return { config: parseConfig(raw, full), source: full };
}

function deepFreeze(o) {
  if (o && typeof o === 'object' && !Object.isFrozen(o)) {
    Object.freeze(o);
    for (const v of Object.values(o)) deepFreeze(v);
  }
  return o;
}
