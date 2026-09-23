import { Bash } from 'just-bash';
import { posix } from 'node:path';
import { fileURLToPath } from 'node:url';
import { realpathSync } from 'node:fs';

export const limits = Object.freeze({ input: 3 * 1024 * 1024, files: 2 * 1024 * 1024, scratch: 256 * 1024, entries: 64, output: 64 * 1024, command: 16000 });
// No host commands, network, interpreters, archive expansion, or WASM runtimes.
const commands = ['cat', 'cp', 'ls', 'mkdir', 'mv', 'rm', 'rmdir', 'stat', 'touch', 'tree', 'awk', 'base64', 'column', 'comm', 'cut', 'diff', 'grep', 'head', 'jq', 'nl', 'printf', 'rg', 'sed', 'sort', 'tail', 'tr', 'uniq', 'wc', 'xargs', 'basename', 'dirname', 'echo', 'env', 'find', 'pwd', 'tee', 'date', 'expr', 'false', 'help', 'seq', 'true', 'which'];

function fileMap(value, scratch = false) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('Invalid file map');
  const entries = Object.entries(value);
  if (entries.length > (scratch ? limits.entries : 128)) throw new Error('Too many files');
  let bytes = 0;
  const result = Object.create(null);
  for (const [name, content] of entries) {
    if (typeof content !== 'string' || name.length > 256 || name.includes('\0') || posix.normalize(name) !== name || !(scratch ? name.startsWith('/scratch/') : name.startsWith('/workspace/') || name.startsWith('/results/'))) throw new Error('Invalid virtual file');
    bytes += Buffer.byteLength(name) + Buffer.byteLength(content);
    if (bytes > (scratch ? limits.scratch : limits.files)) throw new Error('File budget exceeded');
    result[name] = content;
  }
  return result;
}

function textLimit(text, size) {
  const bytes = Buffer.from(text);
  if (bytes.length <= size) return text;
  // Decode complete UTF-8 rather than cutting a multibyte character in half.
  let end = size;
  while (end > 0 && (bytes[end] & 0xc0) === 0x80) end--;
  return bytes.subarray(0, end).toString('utf8');
}

export async function execute(input) {
  if (typeof input.command !== 'string' || !input.command.trim() || Buffer.byteLength(input.command) > limits.command) throw new Error('Command must contain 1 to 16000 bytes');
  const files = fileMap(input.files ?? {});
  const scratch = fileMap(input.scratch ?? {}, true);
  const bash = new Bash({
    files: { ...files, ...scratch, '/workspace/README.txt': 'Private virtual workspace. /results contains saved tool evidence. Only regular text files under /scratch persist. Context and results are snapshots; editing them does not change Canter. Use Canter tools for live state, infrastructure, and connected-agent work. No network, host filesystem, native binaries, package installation, or credentials are available. Run saved scripts with source /scratch/script.sh. Shell variables and cwd reset each call.\n' },
    cwd: '/workspace', env: { HOME: '/scratch', PATH: '/usr/bin:/bin', LANG: 'C.UTF-8' },
    commands, python: false, javascript: false,
    executionLimitProfile: 'hardened',
    executionLimits: { maxSourceBytes: limits.command, maxFileSystemBytes: 4 * 1024 * 1024, maxOutputSize: limits.output, maxCommandCount: 1000, maxLoopIterations: 2000, maxCallDepth: 20, maxExecutionTimeMs: 4000 },
  });
  await bash.fs.mkdir('/scratch', { recursive: true });
  const result = await bash.exec(input.command, { signal: AbortSignal.timeout(4500), rawScript: true });
  const next = Object.create(null);
  let entries = 0;
  async function collect(directory, depth = 0) {
    if (depth > 12) throw new Error('Scratch directory nesting limit reached');
    for (const name of await bash.fs.readdir(directory)) {
      if (++entries > 128) throw new Error('Scratch entry limit reached');
      const path = posix.join(directory, name);
      const stat = await bash.fs.lstat(path);
      if (stat.isSymbolicLink) throw new Error('Scratch links cannot be persisted');
      if (stat.isDirectory) await collect(path, depth + 1);
      else if (stat.isFile) next[path] = new TextDecoder('utf-8', { fatal: true }).decode(await bash.fs.readFileBuffer(path));
    }
  }
  // Validate the entire new snapshot before the host commits anything.
  // A failed export retains the previous snapshot, including deleted files.
  let saved = scratch;
  let persistenceError;
  try { await collect('/scratch'); saved = fileMap(next, true); }
  catch { persistenceError = 'Scratch changes were not saved: keep at most 64 text files, 256 KiB total, and 12 directory levels.'; }
  return {
    stdout: textLimit(result.stdout, limits.output), stderr: textLimit(result.stderr, limits.output),
    exitCode: result.exitCode, scratch: saved,
    ...(persistenceError ? { persistenceError } : {}),
    // Linux retains pre-exec high-water usage, including a Go race-instrumented
    // launcher's resident pages. Report current worker RSS separately.
    metrics: { peakRssKiB: process.resourceUsage().maxRSS, rssKiB: Math.ceil(process.memoryUsage.rss() / 1024) },
  };
}

if (process.argv[1] && realpathSync(process.argv[1]) === fileURLToPath(import.meta.url)) {
  let request = '';
  try {
    // Synchronous bounded reads work both with a subprocess pipe and a systemd
    // socket on stdin. Never inherit or forward the host's environment.
    const { readSync } = await import('node:fs');
    const chunk = Buffer.alloc(16384);
    let count;
    let bytes = 0;
    while ((count = readSync(0, chunk, 0, chunk.length, null)) > 0) {
      bytes += count;
      if (bytes > limits.input) throw new Error('Input limit');
      request += chunk.subarray(0, count).toString('latin1');
    }
    const result = await execute(JSON.parse(Buffer.from(request, 'latin1').toString('utf8')));
    process.stdout.write(JSON.stringify(result));
  } catch {
    // Parser/runtime exception messages may contain host paths or internals.
    process.stdout.write(JSON.stringify({ exitCode: 125, stdout: '', stderr: 'The command exceeded its limits or could not be executed. Use a smaller command.', failed: true }));
  }
}
