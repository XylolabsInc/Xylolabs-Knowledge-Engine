// A tiny MCP server for client_test.go. The test copies this file next to a
// node_modules symlink (plugins/daouoffice/node_modules), because ESM resolves
// packages from the importing file's directory and ignores NODE_PATH.
import { appendFileSync, statSync } from 'node:fs';
import { McpServer } from '@modelcontextprotocol/server';
import { StdioServerTransport } from '@modelcontextprotocol/server/stdio';
import * as z from 'zod';

const server = new McpServer(
  { name: 'test-plugin', version: '0.0.1' },
  { instructions: 'Test plugin instructions.' },
);

const meta = (ctx) => ctx?.mcpReq?._meta ?? {};

// Records a call in COUNT_FILE so the test can count attempts across restarts.
const count = (tool) => {
  if (process.env.COUNT_FILE) appendFileSync(process.env.COUNT_FILE, `${tool}\n`);
};

// 1x1 transparent PNG.
const PNG = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=';

server.registerTool(
  'echo',
  {
    description: 'Echo the text and report what arrived in _meta.',
    inputSchema: z.object({
      text: z.string().describe('Text to echo'),
      n: z.number().int().optional(),
    }),
    annotations: { readOnlyHint: true },
  },
  async ({ text, n }, ctx) => {
    const m = meta(ctx);
    const creds = m['xylolabs/credentials'] ?? {};
    const attachments = (m['xylolabs/attachments'] ?? []).map((a) => ({
      name: a.name,
      mimeType: a.mimeType,
      size: statSync(a.path).size,
    }));
    return {
      content: [{ type: 'text', text: `echo: ${text}` }],
      structuredContent: {
        text,
        n: n ?? null,
        user: m['xylolabs/user'] ?? null,
        mode: m['xylolabs/mode'] ?? null,
        allowKick: m['xylolabs/allowKick'] ?? null,
        username: creds.username ?? null,
        hasPassword: typeof creds.password === 'string',
        attachments,
        secretEnvVisible: 'PLUGIN_SECRET_KEY' in process.env,
        extraEnv: process.env.EXTRA_VAR ?? null,
      },
    };
  },
);

server.registerTool(
  'submit',
  {
    description: 'A gated write tool.',
    inputSchema: z.object({ key: z.string() }),
    annotations: { readOnlyHint: false, destructiveHint: true },
  },
  async ({ key }, ctx) => ({
    content: [{ type: 'text', text: `${meta(ctx)['xylolabs/mode'] ?? 'none'} ${key}` }],
  }),
);

server.registerTool(
  'save_note',
  {
    description: 'A local write that needs no approval.',
    inputSchema: z.object({ note: z.string() }),
    annotations: { readOnlyHint: false, destructiveHint: false },
  },
  async ({ note }) => ({ content: [{ type: 'text', text: `saved ${note}` }] }),
);

server.registerTool(
  'plain',
  {
    description: 'No annotations, so MCP defaults make it a destructive write.',
    inputSchema: z.object({}),
  },
  async () => ({ content: [{ type: 'text', text: 'plain' }] }),
);

server.registerTool(
  'connect',
  {
    description: 'Sign in and report the profile.',
    inputSchema: z.object({
      username: z.string().meta({ title: '아이디' }),
      password: z.string().meta({ title: '비밀번호', format: 'password' }),
      allowKick: z.boolean().optional().meta({ title: '기존 접속을 끊어도 됩니다' }),
    }),
    annotations: { readOnlyHint: false, destructiveHint: false },
    _meta: { 'xylolabs/role': 'connect' },
  },
  async ({ username }) => ({
    content: [{ type: 'text', text: 'connected' }],
    structuredContent: { accountName: '홍길동 (개발본부)', accountId: username },
  }),
);

server.registerTool(
  'busy',
  {
    description: 'Always reports another live session.',
    inputSchema: z.object({}),
    annotations: { readOnlyHint: true },
  },
  async () => ({
    isError: true,
    content: [{ type: 'text', text: '다른 곳에서 접속 중입니다' }],
    structuredContent: { code: 'DUPLICATE_SESSION', message: '다른 곳에서 접속 중입니다' },
  }),
);

server.registerTool(
  'files',
  {
    description: 'Returns an image and a private PDF.',
    inputSchema: z.object({}),
    annotations: { readOnlyHint: true },
  },
  async () => ({
    content: [
      { type: 'text', text: 'two files' },
      { type: 'image', data: PNG, mimeType: 'image/png' },
      {
        type: 'resource',
        resource: { uri: 'file:///out/cert.pdf', mimeType: 'application/pdf', blob: Buffer.from('%PDF-1.4 test').toString('base64') },
        _meta: { 'xylolabs/private': true },
      },
      {
        type: 'resource',
        resource: {
          uri: `file:///${encodeURIComponent('재직증명서 1.pdf')}`,
          mimeType: 'application/pdf',
          blob: Buffer.from('%PDF-1.4 second').toString('base64'),
          _meta: { 'xylolabs/name': '재직증명서.pdf', 'xylolabs/private': true },
        },
        _meta: { 'xylolabs/private': true },
      },
      {
        type: 'resource',
        resource: { uri: `file:///${encodeURIComponent('견적서 A.pdf')}`, mimeType: 'application/pdf', blob: Buffer.from('%PDF q').toString('base64') },
      },
    ],
  }),
);

server.registerTool(
  'big',
  {
    description: 'Returns an image of the requested size.',
    inputSchema: z.object({ bytes: z.number().int() }),
    annotations: { readOnlyHint: true },
  },
  async ({ bytes }) => ({
    content: [{ type: 'image', data: Buffer.alloc(bytes, 7).toString('base64'), mimeType: 'image/jpeg' }],
  }),
);

server.registerTool(
  'kind_write',
  {
    description: 'Annotated as read-only but tagged as a write by _meta.',
    inputSchema: z.object({}),
    annotations: { readOnlyHint: true },
    _meta: { 'xylolabs/kind': 'write' },
  },
  async () => ({ content: [{ type: 'text', text: 'kind' }] }),
);

server.registerTool(
  'exit_after',
  {
    description: 'Answers, then exits the process.',
    inputSchema: z.object({}),
    annotations: { readOnlyHint: true },
  },
  async () => {
    setTimeout(() => process.exit(0), 20);
    return { content: [{ type: 'text', text: 'bye' }] };
  },
);

server.registerTool(
  'crash_read',
  {
    description: 'Exits mid-call (read tool).',
    inputSchema: z.object({}),
    annotations: { readOnlyHint: true },
  },
  async () => {
    count('crash_read');
    process.exit(1);
  },
);

server.registerTool(
  'crash_submit',
  {
    description: 'Exits mid-call (write tool).',
    inputSchema: z.object({}),
    annotations: { readOnlyHint: false, destructiveHint: true },
  },
  async () => {
    count('crash_submit');
    process.exit(1);
  },
);

await server.connect(new StdioServerTransport());
