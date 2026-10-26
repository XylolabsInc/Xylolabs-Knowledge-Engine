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
 * `assertExecute()` also records that a write has started (`state.wrote`). The runtime uses it
 * so that a login error raised after that point is not reported as "nothing happened": the bot
 * puts an action back to pending on AUTH_FAILED or DUPLICATE_SESSION, which would invite a second
 * submission.
 */
const store = new AsyncLocalStorage();

/**
 * Runs `fn` with the given mode in effect for everything it awaits. Pass `state` to read
 * `state.wrote` afterwards.
 */
export function runWithMode(mode, fn, state = {}) {
  state.mode = mode;
  state.wrote ??= false;
  return store.run(state, fn);
}

/** `"execute"`, `"preview"` or `undefined` outside a handler. */
export function currentMode() {
  return store.getStore()?.mode;
}

/** Throws unless the current call is an approved execute call; then marks that a write started. */
export function assertExecute(action) {
  const state = store.getStore();
  if (state?.mode !== 'execute') {
    throw new PluginError('INTERNAL', `승인 전에는 실행할 수 없는 동작입니다: ${action}`);
  }
  state.wrote = true;
}
