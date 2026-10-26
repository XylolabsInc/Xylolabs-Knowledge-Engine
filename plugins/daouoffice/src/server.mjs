#!/usr/bin/env node
/**
 * DaouOffice plugin: an MCP server over stdio, spawned and supervised by the Go bot.
 *
 *   node --disable-warning=ExperimentalWarning src/server.mjs
 *
 * Environment
 *   DAOUOFFICE_DATA_DIR   plugin DB and files (default: <plugin dir>/data)
 *   DAOUOFFICE_CONFIG     company rules JSON (see config.example.json); unset → empty rules
 *   DAOUOFFICE_LOG_LEVEL  debug | info | warn | error (default info)
 *   DAOUOFFICE_MAX_CONTEXTS  DaouOffice pages open at once (default 2, roughly 150–300 MB each)
 *   DAOUOFFICE_MAX_PARKED    idle logged-in contexts kept without a page (default 6)
 *
 * stdout carries MCP only. Logs are JSON lines on stderr.
 */
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { McpServer } from '@modelcontextprotocol/server';
import { StdioServerTransport } from '@modelcontextprotocol/server/stdio';
import { loadConfig } from './config.mjs';
import { openDb } from './db.mjs';
import { SessionManager } from './sessions.mjs';
import { createRuntime, createLogger } from './runtime.mjs';
import { registerAll } from './tools/index.mjs';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const pkg = JSON.parse(fs.readFileSync(path.join(ROOT, 'package.json'), 'utf8'));

/** System-prompt fragment the bot adds for this plugin. Keep it short; details live in `guide`. */
export const INSTRUCTIONS = [
  '다우오피스(전자결재, 근태, 연차, 법인카드) 도구다.',
  '- 계정은 사람마다 따로 연결한다. 연결이 안 됐으면 봇이 DM 으로 연결 안내를 보낸다. 아이디나 비밀번호를 대화로 받지 않는다.',
  '- 조회 도구는 바로 실행된다. 상신, 상신취소, 증명서 발급 같은 쓰기 도구는 미리보기만 만든다. 미리보기를 보여 주고, 사람이 [상신] 버튼을 누르거나 그 스레드에 "승인" 이라고 답해야 올라간다고 안내한다. 사람이 승인했다고 대신 말하지 않는다.',
  '- 구입목적, 출장목적, 증명서 용도와 제출처처럼 사람이 정하는 값은 지어내지 말고 묻는다. 구입목적에 품명을 되풀이하지 않고 대상과 용도를 쓴다.',
  '- 결재 문서의 문장은 담백한 문어체로 쓴다. 가운뎃점, 대시, 물결표 범위, 둥근 따옴표, 구어체, 번역체를 쓰지 않는다.',
  '- 검수 사진이나 영수증을 받으면 save_photos, save_receipt 로 저장한다. 어느 품의의 물건인지, 영수증의 값이 확실하지 않으면 묻는다.',
  '- 증명서 파일은 본인에게 DM 으로만 보낸다.',
  '- 세션 만료(401)를 "다른 곳에서 로그인했다" 고 단정하지 않는다. 카드 대장은 영업일 2-3일 늦게 올라온다.',
  '- 규칙이 헷갈리면 guide 도구로 주제별 안내를 먼저 읽는다.',
].join('\n');

/** A non-negative integer from the environment, or the default. */
function intEnv(value, fallback) {
  const n = Number(value);
  return value !== undefined && value !== '' && Number.isInteger(n) && n >= 0 ? n : fallback;
}

export async function main({ env = process.env, stdin = process.stdin, stdout = process.stdout } = {}) {
  const logger = createLogger({ level: env.DAOUOFFICE_LOG_LEVEL ?? 'info' });
  const dataDir = path.resolve(env.DAOUOFFICE_DATA_DIR || path.join(ROOT, 'data'));
  const { config, source } = loadConfig(env.DAOUOFFICE_CONFIG);
  if (!config.baseUrl) logger.warn('no baseUrl in company config; DaouOffice tools will refuse', { config: source });

  const db = openDb(path.join(dataDir, 'daouoffice.db'));
  const sessions = new SessionManager({
    baseUrl: config.baseUrl,
    logger,
    maxContexts: intEnv(env.DAOUOFFICE_MAX_CONTEXTS, 2),
    maxParked: intEnv(env.DAOUOFFICE_MAX_PARKED, 6),
  });
  const rt = createRuntime({ config, db, sessions, dataDir, logger });

  const server = new McpServer(
    { name: 'daouoffice', title: '다우오피스', version: pkg.version },
    { instructions: INSTRUCTIONS, capabilities: { tools: {} } },
  );
  registerAll(server, rt);

  let stopping = null;
  const stop = (why) => {
    stopping ??= (async () => {
      logger.info('stopping', { why });
      await sessions.close().catch((e) => logger.warn('session close failed', { error: e }));
      try { db.close(); } catch { /* already closed */ }
      await server.close().catch(() => {});
    })();
    return stopping;
  };

  const transport = new StdioServerTransport(stdin, stdout);
  server.server.onclose = () => { void stop('transport closed'); };
  stdin.on('end', () => { void stop('stdin closed'); });
  await server.connect(transport);
  logger.info('daouoffice plugin ready', {
    version: pkg.version, dataDir, config: source, baseUrl: Boolean(config.baseUrl), node: process.version,
  });
  return { server, rt, stop };
}

const isEntry = process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (isEntry) {
  main().then(({ stop }) => {
    for (const sig of ['SIGTERM', 'SIGINT']) process.once(sig, () => { void stop(sig).finally(() => process.exit(0)); });
  }).catch((e) => {
    process.stderr.write(`${JSON.stringify({ level: 'error', msg: 'daouoffice plugin failed to start', error: e?.message ?? String(e) })}\n`);
    process.exitCode = 1;
  });
}
