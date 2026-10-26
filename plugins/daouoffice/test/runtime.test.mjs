import { test, describe, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { Writable } from 'node:stream';
import * as z from 'zod';
import { McpServer, InMemoryTransport } from '@modelcontextprotocol/server';
import { Client } from '@modelcontextprotocol/client';
import { createRuntime, createLogger, buildCall, toToolResult, ownerKey } from '../src/runtime.mjs';
import { PluginError } from '../src/errors.mjs';
import { assertExecute, currentMode } from '../src/guard.mjs';
import { makeApi } from '../src/dao/api.mjs';

const CREDS = { 'xylolabs/credentials': { username: 'Hong', password: 'pw-secret-1234' } };
const USER = { 'xylolabs/user': { key: 'slack:U123', name: '홍길동', platform: 'slack' } };

function capture() {
  const lines = [];
  const stream = new Writable({ write(chunk, _enc, cb) { lines.push(String(chunk)); cb(); } });
  return { stream, lines };
}

describe('runtime over an in-memory MCP transport', () => {
  const log = capture();
  const calls = [];
  let client;

  before(async () => {
    const server = new McpServer({ name: 'test', version: '0.0.0' }, { capabilities: { tools: {} } });
    const rt = createRuntime({ config: {}, db: null, sessions: null, dataDir: '/tmp', logger: createLogger({ stream: log.stream }) });

    rt.tool(server, 'echo', { kind: 'read', description: 'echo', input: z.object({ text: z.string() }) },
      async ({ text }, call) => { calls.push({ tool: 'echo', call }); return { text: `echo ${text}`, data: { owner: call.owner } }; });
    rt.tool(server, 'free', { kind: 'read', account: false, description: 'no account', input: z.object({}) },
      async () => ({ text: 'free' }));
    rt.tool(server, 'submit', { kind: 'write', description: 'gated', input: z.object({ n: z.number() }) }, {
      async preview({ n }, call) {
        calls.push({ tool: 'submit', phase: 'preview', mode: call.mode, guard: currentMode() });
        return { text: `미리보기 ${n}`, data: { n } };
      },
      async execute({ n }, call) {
        calls.push({ tool: 'submit', phase: 'execute', mode: call.mode, guard: currentMode() });
        assertExecute('test submit');
        return { text: `상신 ${n}`, data: { docId: '1000001' } };
      },
    });
    rt.tool(server, 'sneaky', { kind: 'write', description: 'preview that tries to submit', input: z.object({}) }, {
      async preview() { assertExecute('sneaky submit'); return { text: 'never' }; },
      async execute() { return { text: 'ok' }; },
    });
    rt.tool(server, 'reader_writes', { kind: 'read', description: 'read tool that tries to write', input: z.object({}) },
      async () => { assertExecute('read tool write'); return { text: 'never' }; });
    rt.tool(server, 'late_login', { kind: 'write', description: 'login error after the write began', input: z.object({ after: z.boolean() }) }, {
      async preview() { return { text: 'preview' }; },
      async execute({ after }) {
        if (after) assertExecute('submit');
        throw new PluginError('DUPLICATE_SESSION', 'in use elsewhere');
      },
    });
    rt.tool(server, 'fails', { kind: 'read', description: 'throws', input: z.object({ how: z.string() }) }, async ({ how }) => {
      if (how === 'plugin') throw new PluginError('CONFLICT', '이미 올라간 문서입니다.');
      if (how === 'timeout') { const e = new Error('Timeout 30000ms exceeded'); e.name = 'TimeoutError'; throw e; }
      throw new Error('boom with pw-secret-1234 inside');
    });
    rt.tool(server, 'files', { kind: 'read', description: 'returns blobs', input: z.object({}) }, async () => ({
      text: '',
      images: [{ data: Buffer.from([1, 2, 3]), mimeType: 'image/png' }],
      files: [{ name: '재직증명서.pdf', data: Buffer.from('%PDF-1.4'), mimeType: 'application/pdf', private: true }],
      data: ['a', 'b'],
    }));
    rt.tool(server, 'connect', {
      kind: 'read', role: 'connect', description: 'connect',
      input: z.object({ username: z.string(), password: z.string().meta({ format: 'password' }), allowKick: z.boolean().optional() }),
    }, async (args, call) => ({ text: 'ok', data: { owner: call.owner, allowKick: call.allowKick } }));

    const [a, b] = InMemoryTransport.createLinkedPair();
    await server.connect(a);
    client = new Client({ name: 'test-client', version: '0.0.0' });
    await client.connect(b);
  });

  after(async () => { await client?.close(); });

  test('listing carries annotations and role meta', async () => {
    const { tools } = await client.listTools();
    const by = Object.fromEntries(tools.map((t) => [t.name, t]));
    assert.deepEqual(by.echo.annotations, { readOnlyHint: true, destructiveHint: false });
    assert.deepEqual(by.submit.annotations, { readOnlyHint: false, destructiveHint: true });
    assert.equal(by.submit._meta['xylolabs/kind'], 'write');
    assert.equal(by.connect._meta['xylolabs/role'], 'connect');
    assert.equal(by.connect.inputSchema.properties.password.format, 'password');
  });

  test('read tool with credentials gets the call object', async () => {
    const r = await client.callTool({ name: 'echo', arguments: { text: 'hi' }, _meta: { ...CREDS, ...USER } });
    assert.ok(!r.isError);
    assert.equal(r.content[0].text, 'echo hi');
    assert.deepEqual(r.structuredContent, { owner: 'hong' });
    const { call } = calls.findLast((c) => c.tool === 'echo');
    assert.equal(call.user.key, 'slack:U123');
    assert.equal(call.mode, 'execute');
    assert.equal(call.allowKick, false);
    assert.deepEqual(call.attachments, []);
  });

  test('missing credentials → NOT_CONNECTED', async () => {
    const r = await client.callTool({ name: 'echo', arguments: { text: 'x' }, _meta: USER });
    assert.equal(r.isError, true);
    assert.equal(r.structuredContent.code, 'NOT_CONNECTED');
    assert.equal(r.content[0].text, r.structuredContent.message);
  });

  test('account: false tools run without credentials', async () => {
    const r = await client.callTool({ name: 'free', arguments: {} });
    assert.ok(!r.isError);
  });

  for (const [label, meta] of [['no mode', {}], ['mode preview', { 'xylolabs/mode': 'preview' }], ['unknown mode', { 'xylolabs/mode': 'EXECUTE' }]]) {
    test(`write tool with ${label} runs preview only`, async () => {
      calls.length = 0;
      const r = await client.callTool({ name: 'submit', arguments: { n: 1 }, _meta: { ...CREDS, ...meta } });
      assert.ok(!r.isError);
      assert.equal(r.content[0].text, '미리보기 1');
      assert.equal(r.structuredContent.preview, true);
      assert.deepEqual(calls.map((c) => c.phase), ['preview']);
      assert.equal(calls[0].mode, 'preview');
      assert.equal(calls[0].guard, 'preview');
    });
  }

  test('write tool with mode execute runs execute', async () => {
    calls.length = 0;
    const r = await client.callTool({ name: 'submit', arguments: { n: 2 }, _meta: { ...CREDS, 'xylolabs/mode': 'execute' } });
    assert.ok(!r.isError, r.content[0].text);
    assert.equal(r.content[0].text, '상신 2');
    assert.equal(r.structuredContent.preview, undefined);
    assert.deepEqual(calls.map((c) => c.phase), ['execute']);
    assert.equal(calls[0].guard, 'execute');
  });

  test('a preview that reaches a submitting function is refused by the guard', async () => {
    const r = await client.callTool({ name: 'sneaky', arguments: {}, _meta: CREDS });
    assert.equal(r.isError, true);
    assert.equal(r.structuredContent.code, 'INTERNAL');
  });

  test('read tools cannot reach a DaouOffice write either', async () => {
    const r = await client.callTool({ name: 'reader_writes', arguments: {}, _meta: CREDS });
    assert.equal(r.structuredContent.code, 'INTERNAL');
  });

  test('a login error before the write stays as is; after the write it becomes an unknown outcome', async () => {
    const before = await client.callTool({ name: 'late_login', arguments: { after: false }, _meta: { ...CREDS, 'xylolabs/mode': 'execute' } });
    assert.equal(before.structuredContent.code, 'DUPLICATE_SESSION');
    const after = await client.callTool({ name: 'late_login', arguments: { after: true }, _meta: { ...CREDS, 'xylolabs/mode': 'execute' } });
    assert.equal(after.structuredContent.code, 'UPSTREAM');
    assert.deepEqual(after.structuredContent.data, { afterWrite: true, original: 'DUPLICATE_SESSION' });
  });

  test('PluginError maps to its code; unknown errors become INTERNAL without detail', async () => {
    const conflict = await client.callTool({ name: 'fails', arguments: { how: 'plugin' }, _meta: CREDS });
    assert.deepEqual(conflict.structuredContent, { code: 'CONFLICT', message: '이미 올라간 문서입니다.' });
    const timeout = await client.callTool({ name: 'fails', arguments: { how: 'timeout' }, _meta: CREDS });
    assert.equal(timeout.structuredContent.code, 'UPSTREAM');
    const boom = await client.callTool({ name: 'fails', arguments: { how: 'boom' }, _meta: CREDS });
    assert.equal(boom.structuredContent.code, 'INTERNAL');
    assert.doesNotMatch(boom.content[0].text, /boom|pw-secret/);
  });

  test('logs never contain the password or arguments', async () => {
    const text = log.lines.join('');
    assert.ok(text.length > 0);
    assert.doesNotMatch(text, /pw-secret-1234/);
    assert.match(text, /boom with \*\*\* inside/);
    assert.doesNotMatch(text, /"text":"hi"/);
    for (const l of log.lines) JSON.parse(l);
  });

  test('images and files become image and resource blocks', async () => {
    const r = await client.callTool({ name: 'files', arguments: {}, _meta: CREDS });
    assert.equal(r.content[0].type, 'text');
    assert.ok(r.content[0].text.length > 0, 'text block is never empty');
    assert.deepEqual(r.content[1], { type: 'image', data: 'AQID', mimeType: 'image/png' });
    const res = r.content[2];
    assert.equal(res.type, 'resource');
    assert.equal(res.resource.mimeType, 'application/pdf');
    assert.equal(Buffer.from(res.resource.blob, 'base64').toString(), '%PDF-1.4');
    assert.equal(res._meta['xylolabs/private'], true);
    assert.equal(res.resource._meta['xylolabs/name'], '재직증명서.pdf');
    assert.deepEqual(r.structuredContent, { result: ['a', 'b'] });
  });

  test('connect takes credentials and allowKick from arguments', async () => {
    const r = await client.callTool({ name: 'connect', arguments: { username: ' Kim ', password: 'x', allowKick: true } });
    assert.ok(!r.isError);
    assert.deepEqual(r.structuredContent, { owner: 'kim', allowKick: true });
  });

  test('schema violations come back as isError text (no code)', async () => {
    const r = await client.callTool({ name: 'echo', arguments: { text: 5 }, _meta: CREDS });
    assert.equal(r.isError, true);
    assert.equal(r.structuredContent, undefined);
  });
});

describe('buildCall', () => {
  test('attachments are validated', () => {
    const c = buildCall({ ...CREDS, 'xylolabs/attachments': [{ name: '../IMG_1.jpg', mimeType: 'image/jpeg', path: '/abs/IMG_1.jpg' }] },
      { kind: 'local' }, {});
    assert.deepEqual(c.attachments, [{ name: 'IMG_1.jpg', mimeType: 'image/jpeg', path: '/abs/IMG_1.jpg' }]);
    assert.throws(() => buildCall({ ...CREDS, 'xylolabs/attachments': [{ path: 'relative.jpg' }] }, { kind: 'local' }, {}),
      (e) => e.code === 'VALIDATION');
  });

  test('allowKick must be literally true', () => {
    assert.equal(buildCall({ ...CREDS, 'xylolabs/allowKick': 'true' }, { kind: 'read' }).allowKick, false);
    assert.equal(buildCall({ ...CREDS, 'xylolabs/allowKick': true }, { kind: 'read' }).allowKick, true);
  });

  test('owner key is the trimmed, lower-cased login id', () => {
    assert.equal(ownerKey('  Hong.GilDong '), 'hong.gildong');
    assert.equal(ownerKey('a b'), null);
    assert.equal(ownerKey(''), null);
  });
});

test('toToolResult never sends an empty text block', () => {
  assert.equal(toToolResult({}).content[0].text, '완료했습니다.');
  assert.equal(toToolResult('  ').content[0].text, '완료했습니다.');
});

test('api refuses non-read calls outside execute mode', async () => {
  const page = { evaluate: async () => ({ status: 200, text: '{"data":{}}' }) };
  const api = makeApi(page);
  await assert.doesNotReject(api('/gw/api/approval/todo/count'));
  await assert.doesNotReject(api('/eacc/api/ess/myExp/getCompanyCardList', { method: 'POST', body: {} }));
  await assert.rejects(api('/eacc/api/ess/myExp/saveCompanyCard', { method: 'POST', body: {} }), (e) => e.code === 'INTERNAL');
  const { runWithMode } = await import('../src/guard.mjs');
  await runWithMode('execute', () => assert.doesNotReject(api('/eacc/api/ess/myExp/saveCompanyCard', { method: 'POST', body: {} })));
});

test('api marks 401 and hides the raw body', async () => {
  let unauthorized = 0;
  const page = { evaluate: async () => ({ status: 401, text: '<html>secret mail subject</html>' }) };
  const r = await makeApi(page, { onUnauthorized: () => { unauthorized++; } })('/api/portal/common/notification/badge');
  assert.equal(r.status, 401);
  assert.equal(r.ok, false);
  assert.equal(r.data, null);
  assert.equal(r.raw, undefined);
  assert.equal(unauthorized, 1);
});
