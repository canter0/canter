import test from 'node:test';
import assert from 'node:assert/strict';
import { readAttachmentBatch, transferredFiles } from '../src/lib/operator-attachments.ts';

// Emulate the browser file reader while exercising real validation and batching.
globalThis.FileReader = class {
  readAsDataURL(file) {
    file.arrayBuffer().then(bytes => { this.result = `data:${file.type};base64,${Buffer.from(bytes).toString('base64')}`; this.onload(); });
  }
};
const file = name => new File(['hello'], name, { type: 'text/plain' });
test('an invalid file does not discard good files before or after it', async () => {
  const result = await readAttachmentBatch([file('one.txt'), file('unsupported.exe'), file('two.md')], []);
  assert.deepEqual(result.items.map(item => item.name), ['one.txt', 'two.md']);
  assert.equal(result.errors.length, 1);
  assert.match(result.errors[0], /unsupported.exe/);
});
test('retains allowed files when a batch exceeds remaining slots', async () => {
  const existing = (await readAttachmentBatch([file('a.txt'), file('b.txt'), file('c.txt')], [])).items;
  const result = await readAttachmentBatch([file('d.txt'), file('e.txt')], existing);
  assert.equal(result.items.length, 1);
  assert.match(result.errors[0], /up to 4/);
});
test('clipboard file items are a fallback, not duplicate attachments', () => {
  const image = new File(['image'], 'paste.png', { type: 'image/png' });
  const items = [{ kind: 'file', getAsFile: () => image }, { kind: 'string', getAsFile: () => null }];
  assert.deepEqual(transferredFiles({ files: [image], items }), [image]);
  assert.deepEqual(transferredFiles({ files: [], items }), [image]);
});
test('oversized image reports its name and preserves valid neighbors', async () => {
  const large = new File([new Uint8Array(2 * 1048576 + 1)], 'screenshot.png', { type: 'image/png' });
  const result = await readAttachmentBatch([large, file('note.txt')], []);
  assert.equal(result.items[0].name, 'note.txt');
  assert.match(result.errors[0], /screenshot.png.*2 MB/);
});
