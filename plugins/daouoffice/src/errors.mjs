/**
 * Errors the bot can react to without parsing prose.
 *
 * The runtime turns a `PluginError` into a tool result with `isError: true` and
 * `structuredContent: { code, message }`. The Go registry maps the code to its own
 * typed errors (`NOT_CONNECTED` → connect prompt, `DUPLICATE_SESSION` → kick prompt,
 * `AUTH_FAILED` → reconnect prompt). Messages are Korean: the model relays them.
 */

export const CODES = Object.freeze({
  NOT_CONNECTED: 'NOT_CONNECTED',
  AUTH_FAILED: 'AUTH_FAILED',
  DUPLICATE_SESSION: 'DUPLICATE_SESSION',
  RATE_LIMITED: 'RATE_LIMITED',
  VALIDATION: 'VALIDATION',
  NOT_FOUND: 'NOT_FOUND',
  CONFLICT: 'CONFLICT',
  UPSTREAM: 'UPSTREAM',
  INTERNAL: 'INTERNAL',
});

export class PluginError extends Error {
  /**
   * @param {keyof CODES} code
   * @param {string} message Korean, safe to show the person. Never put a password,
   *   a DaouOffice response body or another person's data in it.
   * @param {{ cause?: unknown, data?: Record<string, unknown> }} [options]
   */
  constructor(code, message, { cause, data } = {}) {
    super(message, cause === undefined ? undefined : { cause });
    if (!CODES[code]) throw new TypeError(`unknown plugin error code: ${code}`);
    this.name = 'PluginError';
    this.code = code;
    this.data = data;
  }
}

export const isPluginError = (e) => e instanceof PluginError;

/** Shorthands for the codes used most. */
export const validation = (message, data) => new PluginError('VALIDATION', message, { data });
export const upstream = (message, cause) => new PluginError('UPSTREAM', message, { cause });
export const notFound = (message) => new PluginError('NOT_FOUND', message);
export const conflict = (message, data) => new PluginError('CONFLICT', message, { data });
