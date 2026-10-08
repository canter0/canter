import test from 'node:test';
import assert from 'node:assert/strict';
import { createReleaseChecker, releaseCommit } from '../src/lib/release-update.ts';

const loaded = 'a'.repeat(40), newer = 'b'.repeat(40);

test('only complete release commits are accepted', () => {
  for (const value of [null, {}, 'html', { commit: 'main' }, { commit: 'abc123' }, { commit: newer + 'extra' }]) assert.equal(releaseCommit(value), null);
  assert.equal(releaseCommit({ commit: newer }), newer);
});

test('the loaded bundle detects an already deployed release on its first check', async () => {
  const notices = [];
  const checker = createReleaseChecker(loaded, { read: async () => ({ commit: newer }), onChange: value => notices.push(value) });
  await checker.check();
  assert.deepEqual(notices, [newer]);
  checker.dispose();
});

test('offline and malformed responses stay quiet and recovering or rolling back clears the notice', async () => {
  let time = 0, result = { commit: loaded };
  const notices = [];
  const checker = createReleaseChecker(loaded, { now: () => time, read: async () => { if (result instanceof Error) throw result; return result; }, onChange: value => notices.push(value) });
  for (const value of [{ commit: loaded }, new Error('offline'), {}, { commit: newer }, { commit: loaded }]) {
    result = value; await checker.check(); time += 30_000;
  }
  assert.deepEqual(notices, [null, newer, null]);
  checker.dispose();
});

test('wake events are throttled, pending requests are shared, and cleanup aborts late results', async () => {
  let time = 0, calls = 0, resolve, signal;
  const notices = [];
  const checker = createReleaseChecker(loaded, { now: () => time, read: next => { calls++; signal = next; return new Promise(done => { resolve = done; }); }, onChange: value => notices.push(value) });
  const pending = checker.check();
  time = 60_000;
  await checker.check();
  assert.equal(calls, 1);
  checker.dispose();
  assert.equal(signal.aborted, true);
  resolve({ commit: newer }); await pending;
  assert.deepEqual(notices, []);
  await checker.check(); assert.equal(calls, 1);

  calls = 0;
  const completed = createReleaseChecker(loaded, { now: () => time, read: async () => { calls++; return { commit: loaded }; }, onChange: () => {} });
  await completed.check(); time += 1_000; await completed.check(); assert.equal(calls, 1);
  time += 30_000; await completed.check(); assert.equal(calls, 2);
  completed.dispose();
});

test('development builds without a release identity never poll or announce an update', async () => {
  const checker = createReleaseChecker('', { read: async () => { throw new Error('must not fetch'); }, onChange: () => assert.fail('must not announce') });
  await checker.check(); checker.dispose();
});
