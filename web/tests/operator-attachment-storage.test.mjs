import test from 'node:test';
import assert from 'node:assert/strict';

test('clears attachment drafts through a readwrite IndexedDB transaction', async () => {
  let cleared = 0;
  const db = {
    createObjectStore() {},
    transaction(name, mode) {
      assert.equal(name, 'attachments');
      assert.equal(mode, 'readwrite');
      const transaction = {
        objectStore() { return { clear() { cleared++; queueMicrotask(() => transaction.oncomplete?.()); } }; },
      };
      return transaction;
    },
  };
  globalThis.indexedDB = {
    open(name, version) {
      assert.equal(name, 'canter-drafts');
      assert.equal(version, 1);
      const request = { result: db };
      queueMicrotask(() => { request.onupgradeneeded?.(); request.onsuccess?.(); });
      return request;
    },
  };
  const { clearOperatorAttachmentStorage } = await import('../src/lib/operator-attachment-storage.ts?success');
  await clearOperatorAttachmentStorage();
  assert.equal(cleared, 1);
  delete globalThis.indexedDB;
});

test('does not wait indefinitely for a blocked attachment transaction', async () => {
  const db = {
    createObjectStore() {},
    transaction() { return { objectStore() { return { clear() {} }; } }; },
  };
  globalThis.indexedDB = {
    open() {
      const request = { result: db };
      queueMicrotask(() => { request.onupgradeneeded?.(); request.onsuccess?.(); });
      return request;
    },
  };
  const { clearOperatorAttachmentStorage } = await import('../src/lib/operator-attachment-storage.ts?blocked');
  const started = Date.now();
  await clearOperatorAttachmentStorage();
  assert.ok(Date.now() - started < 750, 'cleanup should honor its 500 ms bound');
  delete globalThis.indexedDB;
});
