import { test, describe, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { Writable } from 'node:stream';
import * as z from 'zod';
import { McpServer, InMemoryTransport } from '@modelcontextprotocol/server';
import { Client } from '@modelcontextprotocol/client';
import { createRuntime, createLogger, buildCall, toToolResult, ownerKey, digestOf, canonicalJson } from '../src/runtime.mjs';
import { openDb } from '../src/db.mjs';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { PluginError } from '../src/errors.mjs';
import { assertExecute, currentMode, markSent } from '../src/guard.mjs';
import { makeApi } from '../src/dao/api.mjs';

const CREDS = { 'xylolabs/credentials': { username: 'Hong', password: 'pw-secret-1234' } };
const USER = { 'xylolabs/user': { key: 'slack:U123', name: '홍길동', platform: 'slack' } };

function capture() {
  const lines = [];
  const stream = new Writable({ write(chunk, _enc, cb) { lines.push(String(chunk)); cb(); } });
  return { stream, lines };
}

let planVersion = 1;

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
    rt.tool(server, 'gated', { kind: 'write', previewDigest: true, description: 'compares with its preview', input: z.object({ n: z.number() }) }, {
      async preview({ n }) { return { text: `plan ${n}`, digest: digestOf({ plan: n, version: planVersion }) }; },
      async execute({ n }, call) {
        if (n === 99) { assertExecute('skips the comparison'); return { text: 'never' }; }
        call.assertPreviewed(digestOf({ plan: n, version: planVersion }));
        assertExecute('gated write');
        return { text: `done ${n}` };
      },
    });
    rt.tool(server, 'wrote_then', { kind: 'write', description: 'fails in various ways after a write began', input: z.object({ how: z.string() }) }, {
      async preview() { return { text: 'p' }; },
      async execute({ how }) {
        assertExecute('first server change');
        if (how === 'retry') throw new PluginError('NOT_FOUND', '문서를 찾지 못했습니다. 잠시 뒤 다시 시도해 주세요.', { data: { docId: '1000000000000001' } });
        if (how === 'timeout') { const e = new Error('Timeout 30000ms exceeded'); e.name = 'TimeoutError'; throw e; }
        throw new Error('unexpected');
      },
    });
    rt.tool(server, 'object_digest', { kind: 'write', previewDigest: true, description: 'digest as a plain object', input: z.object({}) }, {
      async preview() { return { text: 'p', digest: { b: 2, a: [1, { y: 1, x: 2 }] } }; },
      async execute(args, call) { call.assertPreviewed({ a: [1, { x: 2, y: 1 }], b: 2 }); assertExecute('w'); return { text: 'done' }; },
    });
    rt.tool(server, 'sent_then_fails', { kind: 'write', description: 'fails after sending', input: z.object({}) }, {
      async preview() { return { text: 'p' }; },
      async execute() { markSent('final click'); throw new PluginError('VALIDATION', '읽어 온 값이 다릅니다.'); },
    });
    rt.tool(server, 'fails', { kind: 'read', description: 'throws', input: z.object({ how: z.string() }) }, async ({ how }) => {
      if (how === 'plugin') throw new PluginError('CONFLICT', '이미 올라간 문서입니다.');
      if (how === 'timeout') { const e = new Error('Timeout 30000ms exceeded'); e.name = 'TimeoutError'; throw e; }
      if (how === 'escaped') throw new Error(`json ${JSON.stringify({ p: 'pw"se\\cret' })} url ${encodeURIComponent('pw"se\\cret')}`);
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
    assert.equal(call.mode, 'read', 'read tools carry their own kind as mode');
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

  test('previewDigest: execute runs only what the preview showed, once', async () => {
    const EXEC = { ...CREDS, 'xylolabs/mode': 'execute' };
    const none = await client.callTool({ name: 'gated', arguments: { n: 1 }, _meta: EXEC });
    assert.equal(none.structuredContent.code, 'CONFLICT', 'no preview yet');
    const p = await client.callTool({ name: 'gated', arguments: { n: 1 }, _meta: CREDS });
    assert.equal(p.content[0].text, 'plan 1');
    assert.equal(p.structuredContent.digest, undefined, 'the digest stays inside the plugin');
    planVersion = 2;   // the data changed between preview and approval
    const changed = await client.callTool({ name: 'gated', arguments: { n: 1 }, _meta: EXEC });
    assert.equal(changed.structuredContent.code, 'CONFLICT');
    assert.match(changed.content[0].text, /미리보기 뒤에 내용이 바뀌었습니다/);
    await client.callTool({ name: 'gated', arguments: { n: 1 }, _meta: CREDS });
    const ok = await client.callTool({ name: 'gated', arguments: { n: 1 }, _meta: EXEC });
    assert.equal(ok.content[0].text, 'done 1');
    const twice = await client.callTool({ name: 'gated', arguments: { n: 1 }, _meta: EXEC });
    assert.equal(twice.structuredContent.code, 'CONFLICT', 'one preview, one execute');
    await client.callTool({ name: 'gated', arguments: { n: 99 }, _meta: CREDS });
    const skipped = await client.callTool({ name: 'gated', arguments: { n: 99 }, _meta: EXEC });
    assert.equal(skipped.structuredContent.code, 'INTERNAL', 'a write without the comparison is refused by the guard');
  });

  test('every error after a write began is an unknown outcome that does not suggest a retry', async () => {
    const EXEC = { ...CREDS, 'xylolabs/mode': 'execute' };
    const retry = await client.callTool({ name: 'wrote_then', arguments: { how: 'retry' }, _meta: EXEC });
    assert.equal(retry.structuredContent.code, 'NOT_FOUND');
    assert.deepEqual(retry.structuredContent.data, { docId: '1000000000000001', afterWrite: true, original: 'NOT_FOUND' });
    assert.doesNotMatch(retry.content[0].text, /다시 시도/);
    assert.match(retry.content[0].text, /다우오피스에서 문서를 확인해 주세요/);
    const timeout = await client.callTool({ name: 'wrote_then', arguments: { how: 'timeout' }, _meta: EXEC });
    assert.equal(timeout.structuredContent.code, 'UPSTREAM');
    assert.equal(timeout.structuredContent.data.afterWrite, true);
    assert.match(timeout.content[0].text, /결과를 확인하지 못했습니다/);
    assert.doesNotMatch(timeout.content[0].text, /다시 시도/);
    const boom = await client.callTool({ name: 'wrote_then', arguments: { how: 'boom' }, _meta: EXEC });
    assert.equal(boom.structuredContent.code, 'INTERNAL');
    assert.equal(boom.structuredContent.data.afterWrite, true);
    assert.doesNotMatch(boom.content[0].text, /unexpected/);
  });

  test('a preview digest may be the plan object itself; key order does not matter', async () => {
    const EXEC = { ...CREDS, 'xylolabs/mode': 'execute' };
    assert.ok(!(await client.callTool({ name: 'object_digest', arguments: {}, _meta: CREDS })).isError);
    const r = await client.callTool({ name: 'object_digest', arguments: {}, _meta: EXEC });
    assert.equal(r.content[0].text, 'done');
  });

  test('tools that work without an account say so in their metadata', async () => {
    const { tools } = await client.listTools();
    const by = Object.fromEntries(tools.map((t) => [t.name, t]));
    assert.equal(by.free._meta['xylolabs/account'], false);
    assert.equal('xylolabs/account' in by.echo._meta, false);
  });

  test('an error after the request was sent is an unknown outcome', async () => {
    const r = await client.callTool({ name: 'sent_then_fails', arguments: {}, _meta: { ...CREDS, 'xylolabs/mode': 'execute' } });
    assert.equal(r.structuredContent.code, 'VALIDATION');
    assert.equal(r.structuredContent.data.afterWrite, true);
    assert.equal(r.structuredContent.data.original, 'VALIDATION');
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

  test('a password is masked in its JSON-escaped and URL-encoded forms too', async () => {
    const before = log.lines.length;
    const odd = { 'xylolabs/credentials': { username: 'hong', password: 'pw"se\\cret' } };
    await client.callTool({ name: 'fails', arguments: { how: 'escaped' }, _meta: odd });
    const text = log.lines.slice(before).join('');
    assert.match(text, /json/);
    assert.doesNotMatch(text, /se\\\\cret|pw%22se/);
    assert.doesNotMatch(text, /cret/);
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

test('canonical JSON ignores key order; digests are stable', () => {
  assert.equal(canonicalJson({ b: 1, a: { d: [1, { y: 1, x: 2 }], c: 3 } }), '{"a":{"c":3,"d":[1,{"x":2,"y":1}]},"b":1}');
  assert.equal(digestOf({ a: 1, b: 2 }), digestOf({ b: 2, a: 1 }));
  assert.notEqual(digestOf({ a: 1 }), digestOf({ a: 2 }));
});

test('preview digests live in the plugin DB and survive a restart', async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'dao-prev-'));
  const file = path.join(dir, 'daouoffice.db');
  const sink = new Writable({ write(_c, _e, cb) { cb(); } });
  const spec = { kind: 'write', previewDigest: true, description: 'x', input: z.object({ n: z.number() }) };
  const handler = {
    async preview({ n }) { return { text: 'p', digest: digestOf({ n }) }; },
    async execute({ n }, call) { call.assertPreviewed(digestOf({ n })); return { text: 'ok' }; },
  };
  const first = openDb(file);
  const rt1 = createRuntime({ config: {}, db: first, sessions: null, dataDir: dir, logger: createLogger({ stream: sink }) });
  await rt1.invoke('t', spec, handler, { n: 1 }, CREDS);
  first.close();
  const second = openDb(file);
  const rt2 = createRuntime({ config: {}, db: second, sessions: null, dataDir: dir, logger: createLogger({ stream: sink }) });
  const r = await rt2.invoke('t', spec, handler, { n: 1 }, { ...CREDS, 'xylolabs/mode': 'execute' });
  assert.ok(!r.isError, r.content[0].text);
  second.close();
  fs.rmSync(dir, { recursive: true, force: true });
});
