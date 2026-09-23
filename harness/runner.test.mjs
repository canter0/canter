import test from 'node:test';
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { mkdtemp, symlink, rm } from 'node:fs/promises';
import { join } from 'node:path';
import { execute } from './runner.mjs';

test('real shell pipelines process supplied evidence and persist only scratch', async () => {
  const result = await execute({ command: "jq -r '.apps[].name' /workspace/apps.json | sort > /scratch/apps.txt; cat /scratch/apps.txt", files: { '/workspace/apps.json': '{"apps":[{"name":"flight"},{"name":"bot"}]}' } });
  assert.equal(result.exitCode, 0);
  assert.equal(result.stdout, 'bot\nflight\n');
  const resumed = await execute({ command: 'cat /scratch/apps.txt', scratch: result.scratch });
  assert.equal(resumed.stdout, result.stdout);
  const other = await execute({ command: 'cat /scratch/apps.txt' });
  assert.notEqual(other.exitCode, 0);
});

test('host files, secrets, networking, binaries, and optional runtimes are unavailable', async () => {
  const result = await execute({ command: 'cat /etc/passwd; printenv; curl https://example.com; node -e "process.exit()"; python3 -c "print(1)"; sqlite3 :memory: "SELECT 1"; js-exec -c "1"' });
  assert.notEqual(result.exitCode, 0);
  assert.doesNotMatch(result.stdout, /root:|OPENROUTER|DATABASE_URL/);
  assert.match(result.stderr, /command not found/);
});

test('saved scripts execute in the same restricted command environment', async () => {
  const result = await execute({ command: "source /scratch/script.sh", scratch: { '/scratch/script.sh': 'echo café\ncat /etc/passwd\ncurl https://example.com\n' } });
  assert.equal(result.stdout, 'café\n');
  assert.match(result.stderr, /command not found/);
  assert.notEqual(result.exitCode, 0);
});

test('loops and scratch growth are bounded and invalid snapshots cannot escape', async () => {
  const result = await execute({ command: 'while true; do echo a; done' });
  assert.notEqual(result.exitCode, 0);
  assert.ok(result.stdout.length <= 65536);
  await assert.rejects(execute({ command: 'true', scratch: { '/scratch/../workspace/injected': 'x' } }));
  const overflow = await execute({ command: "cp /scratch/keep /scratch/huge", scratch: { '/scratch/keep': 'x'.repeat(150000) } });
  assert.match(overflow.persistenceError, /not saved/);
  assert.deepEqual({ ...overflow.scratch }, { '/scratch/keep': 'x'.repeat(150000) });
});

test('restricted subprocess protocol works with a clean environment', async () => {
  const root = fileURLToPath(new URL('.', import.meta.url));
  // Production enters through a managed current symlink. Node resolves
  // import.meta.url to the real release while preserving the argv path.
  const directory = await mkdtemp(join(root, '.runtime-test-'));
  await symlink(root, join(directory, 'current'), 'dir');
  const result = await new Promise((resolve, reject) => {
    const child = spawn(process.execPath, ['--permission', `--allow-fs-read=${root}`, `--allow-fs-read=${directory}`, '--max-old-space-size=96', join(directory, 'current/runner.mjs')], { env: { LANG: 'C.UTF-8' }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', error = '';
    child.stdout.on('data', chunk => { output += chunk; });
    child.stderr.on('data', chunk => { error += chunk; });
    child.on('error', reject);
    child.on('close', code => code === 0 ? resolve(JSON.parse(output)) : reject(new Error(error)));
    child.stdin.end(JSON.stringify({ command: 'echo ready; echo café > /scratch/unicode' }));
  }).finally(() => rm(directory, { recursive: true, force: true }));
  assert.equal(result.stdout, 'ready\n');
  assert.equal(result.scratch['/scratch/unicode'], 'café\n');
  assert.ok(result.metrics.peakRssKiB < 192 * 1024, `RSS exceeds production limit: ${result.metrics.peakRssKiB}`);
});
