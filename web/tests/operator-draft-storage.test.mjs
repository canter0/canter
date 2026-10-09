import test from 'node:test';
import assert from 'node:assert/strict';
import { clearOperatorTextDrafts } from '../src/lib/operator-draft-storage.ts';

test('sign-out cleanup removes conversation text drafts and preserves preferences', () => {
  const values = new Map([
    ['canter:conversation-draft:workspace:new', 'unsent prompt'],
    ['canter:conversation-draft:workspace:conv_1', 'another prompt'],
    ['canter:conversation-model:account:workspace:conv_1', 'model-id'],
  ]);
  const storage = {
    get length() { return values.size; },
    key(index) { return [...values.keys()][index] ?? null; },
    removeItem(key) { values.delete(key); },
  };
  clearOperatorTextDrafts(storage);
  assert.deepEqual([...values], [['canter:conversation-model:account:workspace:conv_1', 'model-id']]);
});
