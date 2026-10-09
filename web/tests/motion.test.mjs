import test from 'node:test';
import assert from 'node:assert/strict';
import { playMotion } from '../src/lib/motion.ts';

function environment(t, reduced = false) {
  const original = globalThis.window;
  const preference = new EventTarget();
  preference.matches = reduced;
  let listeners = 0;
  const add = preference.addEventListener.bind(preference);
  const remove = preference.removeEventListener.bind(preference);
  preference.addEventListener = (...args) => { listeners++; add(...args); };
  preference.removeEventListener = (...args) => { listeners--; remove(...args); };
  globalThis.window = { matchMedia: () => preference };
  t.after(() => { if (original) globalThis.window = original; else delete globalThis.window; });
  const calls = [];
  const element = { animate(frames, options) {
    const animation = new EventTarget();
    animation.cancelled = false;
    animation.cancel = () => { animation.cancelled = true; animation.dispatchEvent(new Event('cancel')); };
    calls.push({ frames, options, animation });
    return animation;
  } };
  return { preference, calls, element, listeners: () => listeners };
}

test('reduced motion renders the final UI without starting an animation', t => {
  const env = environment(t, true);
  assert.equal(playMotion(env.element, [{ opacity: 0 }, { opacity: 1 }]), null);
  assert.equal(env.calls.length, 0);
  assert.equal(env.listeners(), 0);
});

test('switching to reduced motion cancels the active animation and releases its listener', t => {
  const env = environment(t);
  const animation = playMotion(env.element, [{ opacity: 0 }, { opacity: 1 }]);
  assert.equal(env.listeners(), 1);
  env.preference.matches = true;
  env.preference.dispatchEvent(new Event('change'));
  assert.equal(animation.cancelled, true);
  assert.equal(env.listeners(), 0);
});

test('finished and interrupted controls do not accumulate preference listeners', t => {
  const env = environment(t);
  for (let i = 0; i < 20; i++) {
    const animation = playMotion(env.element, [{ opacity: 0 }, { opacity: 1 }]);
    if (i % 2) animation.cancel();
    else animation.dispatchEvent(new Event('finish'));
    assert.equal(env.listeners(), 0);
  }
});
