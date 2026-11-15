import test from 'node:test';
import assert from 'node:assert/strict';
import { visibleSubscription } from '../src/lib/visible-subscription.ts';

class Page extends EventTarget {
  visibilityState = 'visible';
  show(visible) {
    this.visibilityState = visible ? 'visible' : 'hidden';
    this.dispatchEvent(new Event('visibilitychange'));
  }
}
const turn = () => new Promise(resolve => setImmediate(resolve));
function pending() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

test('hidden tabs hold no subscription; return starts once and hiding aborts immediately', async () => {
  const page = new Page(); page.show(false);
  const sessions = [], errors = [];
  const dispose = visibleSubscription(page, signal => {
    const task = pending(); sessions.push({ signal, ...task }); return task.promise;
  }, error => errors.push(error));
  await turn(); assert.equal(sessions.length, 0);
  page.show(true); page.show(true); await turn(); assert.equal(sessions.length, 1);
  page.show(false); assert.equal(sessions[0].signal.aborted, true);
  sessions[0].reject(new Error('request aborted')); await turn();
  assert.deepEqual(errors, []);
  page.show(true); await turn(); assert.equal(sessions.length, 2);
  dispose(); assert.equal(sessions[1].signal.aborted, true);
  page.show(true); sessions[1].resolve(); await turn(); assert.equal(sessions.length, 2);
});

test('a late prior response cannot stop the resumed subscription or publish an error', async () => {
  const page = new Page(), sessions = [], errors = [];
  const dispose = visibleSubscription(page, signal => {
    const task = pending(); sessions.push({ signal, ...task }); return task.promise;
  }, error => errors.push(error));
  await turn(); page.show(false); page.show(true); await turn();
  sessions[0].reject(new Error('late failure')); await turn();
  page.show(true); await turn();
  assert.equal(sessions.length, 2); assert.equal(sessions[1].signal.aborted, false);
  assert.deepEqual(errors, []); dispose(); sessions[1].resolve(); await turn();
});

test('visible failures retry, but hiding cancels retry and returning reconciles immediately', async t => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const page = new Page(), errors = []; let calls = 0;
  const dispose = visibleSubscription(page, async () => { calls++; throw new Error('offline'); }, error => errors.push(error.message));
  await turn(); assert.equal(calls, 1); assert.deepEqual(errors, ['offline']);
  t.mock.timers.tick(3000); await turn(); assert.equal(calls, 2);
  page.show(false); t.mock.timers.tick(30000); await turn(); assert.equal(calls, 2);
  page.show(true); await turn(); assert.equal(calls, 3);
  dispose(); t.mock.timers.tick(30000); await turn(); assert.equal(calls, 3);
});

test('navigation before startup prevents a request from being issued', async () => {
  const page = new Page(); let calls = 0;
  const dispose = visibleSubscription(page, async () => { calls++; }, () => {});
  dispose(); await turn(); assert.equal(calls, 0);
});
