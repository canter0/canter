import test from 'node:test';
import assert from 'node:assert/strict';
import { createConversationCache } from '../src/lib/conversation-cache.ts';
import { requiredWorkspaceResources } from '../src/lib/workspace-data.ts';

const detail = (id = 'one', workspaceId = 'workspace', content = 'Hello') => ({ conversation: { id, workspaceId, title: id }, messages: [{ id: 'message', content }], run: null });

test('reopening retains messages and events and deduplicates concurrent refreshes', async () => {
  let calls = 0, release;
  const cache = createConversationCache('workspace', () => { calls++; return new Promise(resolve => { release = resolve; }); });
  const first = cache.load('one');
  assert.equal(cache.load('one'), first);
  await Promise.resolve();
  release(detail());
  await first;
  const events = [{ sequence: 12, kind: 'finished' }];
  cache.save('one', cache.peek('one').detail, events);
  assert.equal((await cache.load('one')).messages[0].content, 'Hello');
  assert.equal(calls, 1);
  const refresh = cache.load('one', true);
  assert.equal(cache.load('one', true), refresh);
  assert.equal(cache.peek('one').events.at(-1).sequence, 12);
  await Promise.resolve();
  release(detail('one', 'workspace', 'Updated'));
  await refresh;
  assert.equal(cache.peek('one').events.at(-1).sequence, 12);
  assert.equal(cache.peek('one').detail.messages[0].content, 'Updated');
});

test('stale data renders immediately but refreshes; writes of the same detail do not renew freshness', async () => {
  let time = 0, calls = 0;
  const cache = createConversationCache('workspace', async () => { calls++; return detail(); }, () => time);
  await cache.load('one');
  time = 16_000;
  const previous = cache.peek('one');
  cache.save('one', previous.detail, [{ sequence: 1 }]);
  assert.equal(cache.peek('one').fetchedAt, 0);
  assert.ok(cache.peek('one'));
  await cache.load('one');
  assert.equal(calls, 2);
  time += 300_001;
  assert.equal(cache.peek('one'), undefined);
});

test('an invalidated in-flight read cannot resurrect a deleted conversation', async () => {
  let release;
  const cache = createConversationCache('workspace', () => new Promise(resolve => { release = resolve; }));
  const request = cache.load('one');
  await Promise.resolve();
  cache.invalidate('one');
  release(detail());
  await request;
  assert.equal(cache.peek('one'), undefined);
});

test('failures are retryable and failed revalidation preserves usable messages', async () => {
  let fail = true;
  const cache = createConversationCache('workspace', async () => { if (fail) throw Error('offline'); return detail(); });
  await assert.rejects(cache.load('one'), /offline/);
  assert.equal(cache.peek('one'), undefined);
  fail = false;
  await cache.load('one');
  fail = true;
  await assert.rejects(cache.load('one', true), /offline/);
  assert.equal(cache.peek('one').detail.messages[0].content, 'Hello');
});

test('provider caches are isolated and reject data for another conversation or workspace', async () => {
  const first = createConversationCache('workspace', async () => detail());
  const otherAccount = createConversationCache('workspace', async () => detail('one', 'workspace', 'Private'));
  await first.load('one');
  assert.equal(otherAccount.peek('one'), undefined);
  await otherAccount.load('one');
  assert.equal(first.peek('one').detail.messages[0].content, 'Hello');
  const wrongWorkspace = createConversationCache('other', async () => detail());
  await assert.rejects(wrongWorkspace.load('one'), /does not belong/);
  first.save('two', detail());
  assert.equal(first.peek('two'), undefined);
});

test('entry count and attachment bytes are bounded', () => {
  const cache = createConversationCache('workspace', async id => detail(id));
  for (let i = 0; i < 9; i++) cache.save(String(i), detail(String(i)));
  assert.equal(cache.peek('0'), undefined);
  assert.ok(cache.peek('1'));
  cache.save('huge', detail('huge', 'workspace', 'x'.repeat(9 * 1024 * 1024)));
  assert.equal(cache.peek('huge'), undefined);
  cache.save('large-a', detail('large-a', 'workspace', 'x'.repeat(5 * 1024 * 1024)));
  cache.save('large-b', detail('large-b', 'workspace', 'x'.repeat(5 * 1024 * 1024)));
  assert.equal(cache.peek('large-a'), undefined);
  assert.ok(cache.peek('large-b'));
});

test('conversations never wait for app, agent, deployment, or activity lists', () => {
  assert.deepEqual(requiredWorkspaceResources('/app'), ['conversations']);
  assert.deepEqual(requiredWorkspaceResources('/app/conversations/one'), ['conversations']);
  assert.deepEqual(requiredWorkspaceResources('/app/system'), ['systems']);
  assert.deepEqual(requiredWorkspaceResources('/app/agents/one'), ['installations']);
  assert.deepEqual(requiredWorkspaceResources('/app/account'), []);
  assert.deepEqual(requiredWorkspaceResources('/app/billing'), []);
});
