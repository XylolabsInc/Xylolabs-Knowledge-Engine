import { AsyncLocalStorage } from 'node:async_hooks';
import { PluginError } from './errors.mjs';

/**
 * Write guard. Only a call that the Go registry sent with `_meta["xylolabs/mode"] =
 * "execute"` (after a person pressed 상신) may change anything in DaouOffice.
 *
 * The runtime runs every handler inside `runWithMode()`. Functions that change
 * server state (submit, withdraw, delete, comment, non-read API calls) call
 * `assertExecute()` first, so a preview that reaches them by mistake fails loudly
 * instead of submitting. Outside any handler (tests, one-off scripts) the mode is
 * unknown and writes are refused too.
 *
 * Two more marks feed the runtime's error reporting:
 *
 *   state.wrote   set by `assertExecute()`: a write is about to start. From then on every error is
 *                 reported as an unknown outcome (`data.afterWrite`), never as a failure to retry.
 *   state.sent    set by `markSent()` right before the irreversible request leaves (the click on the
 *                 final confirm button, the POST). Every error after it carries `afterWrite`, so the
 *                 bot reports an unknown outcome instead of a failure that invites a second try.
 *
 * Tools that compare execute against their preview (`previewDigest`) also set
 * `state.requirePreviewed`; then `assertExecute()` refuses until the handler has called
 * `call.assertPreviewed()`.
 */
const store = new AsyncLocalStorage();

/**
 * Runs `fn` with the given mode in effect for everything it awaits. Pass `state` to read
 * `state.wrote` and `state.sent` afterwards.
 */
export function runWithMode(mode, fn, state = {}) {
  state.mode = mode;
  state.wrote ??= false;
  state.sent ??= false;
  return store.run(state, fn);
}

/** `"execute"`, `"preview"`, `"read"`, `"local"` or `undefined` outside a handler. */
export function currentMode() {
  return store.getStore()?.mode;
}

/**
 * Throws unless the current call is an approved execute call (and, for `previewDigest` tools, its
 * preview was matched). Marks nothing: use it to refuse early in a flow whose first steps only read.
 */
export function requireExecute(action) {
  const state = store.getStore();
  if (state?.mode !== 'execute') {
    throw new PluginError('INTERNAL', `승인 전에는 실행할 수 없는 동작입니다: ${action}`);
  }
  if (state.requirePreviewed && !state.previewed) {
    throw new PluginError('INTERNAL', `미리보기와 맞춰 보기 전에는 실행할 수 없는 동작입니다: ${action}`);
  }
  return state;
}

/** Throws unless the current call is an approved execute call; then marks that a write started. */
export function assertExecute(action) {
  requireExecute(action).wrote = true;
}

/** Marks that the irreversible request is being sent now. Call it right before the final click or POST. */
export function markSent(action) {
  assertExecute(action);
  store.getStore().sent = true;
}
