import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { Client } from '@modelcontextprotocol/client';
import { StdioClientTransport } from '@modelcontextprotocol/client/stdio';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');

test('server starts over stdio, lists tools and answers guide', async () => {
  const dataDir = fs.mkdtempSync(path.join(os.tmpdir(), 'dao-smoke-'));
  const stderr = [];
  const transport = new StdioClientTransport({
    command: process.execPath,
    args: ['--disable-warning=ExperimentalWarning', path.join(ROOT, 'src/server.mjs')],
    cwd: ROOT,
    env: {
      PATH: process.env.PATH ?? '',
      DAOUOFFICE_DATA_DIR: dataDir,
      DAOUOFFICE_CONFIG: path.join(ROOT, 'config.example.json'),
    },
    stderr: 'pipe',
  });
  transport.stderr?.on('data', (b) => stderr.push(String(b)));
  const client = new Client({ name: 'smoke', version: '0.0.0' });
  try {
    await client.connect(transport);
    assert.equal(client.getServerVersion()?.name, 'daouoffice');
    assert.match(client.getInstructions() ?? '', /미리보기/);

    const { tools } = await client.listTools();
    const names = tools.map((t) => t.name).sort();
    for (const n of ['connect', 'login', 'me', 'attendance_today', 'leave_balance', 'approval_inbox', 'my_drafts',
      'read_document', 'list_forms', 'list_cards', 'card_usage', 'withdraw_document', 'guide']) {
      assert.ok(names.includes(n), `missing tool ${n}`);
    }
    const connect = tools.find((t) => t.name === 'connect');
    assert.equal(connect._meta['xylolabs/role'], 'connect');
    assert.equal(connect.inputSchema.properties.password.format, 'password');
    const withdraw = tools.find((t) => t.name === 'withdraw_document');
    assert.equal(withdraw.annotations.destructiveHint, true);
    assert.equal(withdraw.annotations.readOnlyHint, false);

    const guide = await client.callTool({ name: 'guide', arguments: { topic: 'session' } });
    assert.ok(!guide.isError);
    assert.match(guide.content[0].text, /동시 접근 제한/);

    const me = await client.callTool({ name: 'me', arguments: {} });
    assert.equal(me.isError, true);
    assert.equal(me.structuredContent.code, 'NOT_CONNECTED');

    // Nothing but JSON lines on stderr, and no ExperimentalWarning.
    const lines = stderr.join('').split('\n').filter(Boolean);
    assert.ok(lines.length > 0);
    for (const l of lines) assert.doesNotThrow(() => JSON.parse(l), `stderr line is not JSON: ${l}`);
  } finally {
    await client.close().catch(() => {});
    fs.rmSync(dataDir, { recursive: true, force: true });
  }
});
