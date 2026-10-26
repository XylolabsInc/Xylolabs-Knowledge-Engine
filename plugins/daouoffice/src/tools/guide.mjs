import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import * as z from 'zod';
import { PluginError } from '../errors.mjs';

/**
 * `guide(topic)` returns one of `guide/*.md`: the rules from the private repo's skills, without
 * names. Topics are the file names present at startup, so a module that adds `guide/<topic>.md`
 * shows up here without code changes.
 */

export const GUIDE_DIR = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../../guide');

export function guideTopics(dir = GUIDE_DIR) {
  try {
    return fs.readdirSync(dir).filter((f) => /^[a-z0-9-]+\.md$/.test(f)).map((f) => f.slice(0, -3)).sort();
  } catch {
    return [];
  }
}

export function readGuide(topic, dir = GUIDE_DIR) {
  if (!guideTopics(dir).includes(topic)) throw new PluginError('NOT_FOUND', `안내 주제가 없습니다: ${topic}`);
  return fs.readFileSync(path.join(dir, `${topic}.md`), 'utf8');
}

export function register(server, rt) {
  const topics = guideTopics();
  const others = topics.filter((t) => t !== 'index');
  rt.tool(server, 'guide', {
    kind: 'read',
    account: false,
    description: `다우오피스 업무 규칙 안내를 읽는다. 결재 문서를 만들거나 고치기 전, 규칙이 헷갈릴 때 먼저 읽는다. 주제: ${others.join(', ')}. 주제를 안 주면 목차(index)다.`,
    input: z.object({
      topic: (topics.length ? z.enum(topics) : z.string()).optional().describe('안내 주제'),
    }),
  }, async ({ topic = 'index' }) => {
    const text = readGuide(topic);
    return { text, data: { topic, topics } };
  });
}
