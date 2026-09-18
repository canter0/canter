import test from 'node:test';
import assert from 'node:assert/strict';
import { createRepositoryInspectionCache } from '../src/lib/repository-inspection-cache.ts';

const commit = 'a'.repeat(40);
const result = { repository: 'owner/repo', commit, files: ['README.md'], truncated: false };

test('source tabs deduplicate pending reads and share only pinned successful trees', async () => {
  let release, calls = 0;
  const inspect = createRepositoryInspectionCache(() => { calls++; return new Promise(resolve => { release = resolve; }); });
  const first = inspect('workspace', 'owner/repo', '');
  const second = inspect('workspace', 'owner/repo', '');
  assert.equal(first, second);
  await Promise.resolve();
  assert.equal(calls, 1);
  release(result);
  assert.equal(await first, result);
  assert.equal(await inspect('workspace', 'owner/repo', commit), result);
  assert.equal(calls, 1);
  const refreshedBranch = inspect('workspace', 'owner/repo', '');
  await Promise.resolve();
  assert.equal(calls, 2);
  release(result);
  await refreshedBranch;
});

test('failures are retryable and cache entries stay isolated by workspace, repo, and commit', async () => {
  let calls = 0;
  const inspect = createRepositoryInspectionCache(async path => {
    calls++;
    if (calls === 1) throw new Error('GitHub rate limit');
    return { ...result, commit: new URL(path, 'http://canter.test').searchParams.get('commit') };
  });
  await assert.rejects(inspect('one', 'owner/repo', commit), /rate limit/);
  await inspect('one', 'owner/repo', commit);
  await inspect('one', 'owner/repo', commit);
  assert.equal(calls, 2);
  await inspect('two', 'owner/repo', commit);
  await inspect('one', 'owner/other', commit);
  await inspect('one', 'owner/repo', 'b'.repeat(40));
  assert.equal(calls, 5);
  await inspect('one', 'owner/repo', commit, true);
  assert.equal(calls, 6);
});

test('successful snapshots expire and the cache retains at most sixteen trees', async () => {
  let calls = 0, time = 0;
  const inspect = createRepositoryInspectionCache(async () => { calls++; return result; }, () => time);
  await inspect('workspace', 'owner/repo', commit);
  time = 300_001;
  await inspect('workspace', 'owner/repo', commit);
  assert.equal(calls, 2);
  for (let i = 0; i < 16; i++) await inspect('workspace', `owner/repo-${i}`, commit);
  await inspect('workspace', 'owner/repo', commit);
  assert.equal(calls, 19);
});
