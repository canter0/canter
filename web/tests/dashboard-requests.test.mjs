import test from 'node:test';
import assert from 'node:assert/strict';
import {
  appendDashboardRequestText,
  commitDashboardRequestToDraft,
  consumeDashboardRequest,
  createDashboardRequest,
  dashboardRequestKey,
  dashboardRequestMaxPrompt,
  dashboardRequestMatches,
  dashboardRequestTTLMs,
  parseDashboardRequest,
  readDashboardRequest,
  serializeDashboardRequest,
  storeDashboardRequest,
} from '../src/lib/dashboard-requests.ts';

function memoryStore() {
  const map = new Map();
  return {
    getItem: (k) => (map.has(k) ? map.get(k) : null),
    setItem: (k, v) => map.set(k, v),
    removeItem: (k) => map.delete(k),
    size: () => map.size,
  };
}

const base = { accountId: 'acct_1', workspaceId: 'wsp_1', conversationId: 'conv_1', prompt: 'Help me create a sandbox.' };

test('create + roundtrip keeps an exact account/workspace/destination request', () => {
  const request = createDashboardRequest({ ...base, surface: { kind: 'sandbox', id: 'sbx_1' } }, 1000, 'nonce-1');
  assert.ok(request);
  const raw = serializeDashboardRequest(request);
  assert.ok(raw);
  const store = memoryStore();
  storeDashboardRequest(request, store);
  assert.equal(store.getItem(dashboardRequestKey('nonce-1')), raw);
  const looked = readDashboardRequest('nonce-1', store, 1000);
  assert.deepEqual(looked.request, request);
  assert.ok(dashboardRequestMatches(request, { accountId: 'acct_1', workspaceId: 'wsp_1', conversationId: 'conv_1' }));
});

test('new general conversation destination uses null conversationId', () => {
  const request = createDashboardRequest({ ...base, conversationId: null }, 1000);
  assert.ok(request);
  assert.equal(request.conversationId, null);
  assert.ok(dashboardRequestMatches(request, { accountId: 'acct_1', workspaceId: 'wsp_1', conversationId: null }));
  assert.ok(!dashboardRequestMatches(request, { accountId: 'acct_1', workspaceId: 'wsp_1', conversationId: 'conv_1' }));
});

test('rejects malformed, wrong-scope and unsafe payloads', () => {
  assert.equal(createDashboardRequest({ ...base, accountId: '' }), null);
  assert.equal(createDashboardRequest({ ...base, prompt: '   ' }), null);
  assert.equal(createDashboardRequest({ ...base, prompt: 'x'.repeat(dashboardRequestMaxPrompt + 1) }), null);
  assert.equal(createDashboardRequest({ ...base, conversationId: 'c'.repeat(200) }), null);
  assert.equal(parseDashboardRequest(null), null);
  assert.equal(parseDashboardRequest('str'), null);
  assert.equal(parseDashboardRequest({ nonce: 5, ...base, createdAt: 1 }), null);
  assert.equal(parseDashboardRequest({ ...base, nonce: 'n', createdAt: -4 }), null);
  // Malformed surfaces are rejected rather than smuggled through.
  assert.equal(parseDashboardRequest({ ...base, nonce: 'n', createdAt: 1000, surface: { kind: '', id: 'x' } }, 1000), null);
  assert.equal(parseDashboardRequest({ ...base, nonce: 'n', createdAt: 1000, surface: { kind: 'sandbox', id: 'sbx_' + 'x'.repeat(200) } }, 1000), null);
  // A serialized request whose nonce does not match the lookup key fails.
  const request = createDashboardRequest(base, 1000, 'nonce-real');
  const store = memoryStore();
  storeDashboardRequest(request, store);
  assert.equal(readDashboardRequest('nonce-other', store, 1000).error, 'missing');
});

test('expired and future-dated requests are rejected', () => {
  const request = createDashboardRequest(base, 1000, 'n1');
  const store = memoryStore();
  storeDashboardRequest(request, store);
  assert.equal(readDashboardRequest('n1', store, 1000 + dashboardRequestTTLMs + 1).error, 'expired');
  assert.equal(parseDashboardRequest({ ...base, nonce: 'n', createdAt: 1000 + 10 * 60 * 1000 }, 1000), null);
});

test('consumption removes the envelope so a nonce cannot replay', () => {
  const request = createDashboardRequest(base, 1000, 'n2');
  const store = memoryStore();
  storeDashboardRequest(request, store);
  consumeDashboardRequest('n2', store);
  assert.equal(readDashboardRequest('n2', store, 1000).error, 'missing');
});

test('storage denial surfaces to the caller instead of dropping the request', () => {
  const request = createDashboardRequest(base, 1000, 'n3');
  const denied = { getItem: () => null, setItem: () => { throw new Error('denied'); }, removeItem: () => {} };
  assert.throws(() => storeDashboardRequest(request, denied));
  const lookupDenied = { getItem: () => { throw new Error('denied'); }, setItem: () => {}, removeItem: () => {} };
  assert.equal(readDashboardRequest('n3', lookupDenied, 1000).error, 'missing');
});

test('oversized prompts stay under the serialized bound', () => {
  const prompt = 'run '.repeat(1900);
  const request = createDashboardRequest({ ...base, prompt }, 1000, 'n4');
  assert.ok(request);
  assert.ok(serializeDashboardRequest(request));
  const huge = 'x'.repeat(dashboardRequestMaxPrompt);
  const big = createDashboardRequest({ ...base, prompt: huge, surface: { kind: 'sandbox', id: 'sbx_' + 'x'.repeat(120) } }, 1000, 'n5');
  assert.ok(big);
  assert.ok(serializeDashboardRequest(big));
});

test('append preserves the existing draft verbatim with a blank line', () => {
  assert.equal(appendDashboardRequestText('', 'ask'), 'ask');
  assert.equal(appendDashboardRequestText('   ', 'ask'), 'ask');
  const existing = 'line one\n trailing space ';
  assert.equal(appendDashboardRequestText(existing, 'ask'), `${existing}\n\nask`);
});

test('only handoff-produced surface kinds are accepted', () => {
  const base = { accountId: 'acct-1', workspaceId: 'ws-1', conversationId: null, prompt: 'p' };
  for (const kind of ['apps', 'sandboxes']) {
    const request = createDashboardRequest({ ...base, surface: { kind } }, 1000, 'n');
    assert.ok(request && request.surface?.kind === kind);
    // These kinds never carry an id through a handoff.
    assert.equal(createDashboardRequest({ ...base, surface: { kind, id: 'sbx_1' } }, 1000, 'n'), null);
  }
  // Unknown kinds are rejected rather than smuggled through by a cast.
  assert.equal(createDashboardRequest({ ...base, surface: { kind: 'evil' } }, 1000, 'n'), null);
  assert.equal(createDashboardRequest({ ...base, surface: { kind: 'repository' } }, 1000, 'n'), null);
  // Sandbox surfaces require a real sbx_ id.
  for (const id of [undefined, '', 'sbx_', 'sbx', 'vm_1', 'sbx_ has space', 'sbx_\tbad', 'sbx_' + 'x'.repeat(200)]) {
    assert.equal(createDashboardRequest({ ...base, surface: { kind: 'sandbox', id } }, 1000, 'n'), null, `accepted ${JSON.stringify(id)}`);
  }
  assert.ok(createDashboardRequest({ ...base, surface: { kind: 'sandbox', id: 'sbx_abc-123' } }, 1000, 'n'));
  // Parsed envelopes enforce the same rules.
  const wire = { nonce: 'n', accountId: 'a', workspaceId: 'w', conversationId: null, prompt: 'p', createdAt: 1000 };
  assert.equal(parseDashboardRequest({ ...wire, surface: { kind: 'evil' } }, 1000), null);
  assert.equal(parseDashboardRequest({ ...wire, surface: { kind: 'sandbox' } }, 1000), null);
  assert.equal(parseDashboardRequest({ ...wire, surface: { kind: 'sandbox', id: 'sbx_' } }, 1000), null);
  assert.equal(parseDashboardRequest({ ...wire, surface: { kind: 'sandboxes', id: 'x' } }, 1000), null);
});

test('oversized stored payloads are rejected before parsing', () => {
  const store = memoryStore();
  store.setItem('canter:dashboard-request:big', JSON.stringify({ nonce: 'big', prompt: 'x'.repeat(20 * 1024) }));
  assert.equal(readDashboardRequest('big', store, Date.now()).error, 'invalid');
});

test('future timestamps and malformed createdAt are rejected', () => {
  const wire = { nonce: 'n', accountId: 'a', workspaceId: 'w', conversationId: null, prompt: 'p' };
  assert.equal(parseDashboardRequest({ ...wire, createdAt: 1001 }, 1000), null);
  assert.equal(parseDashboardRequest({ ...wire, createdAt: 'soon' }, 1000), null);
  assert.equal(parseDashboardRequest({ ...wire, createdAt: 1000.5 }, 1000), null);
  assert.equal(parseDashboardRequest({ ...wire, createdAt: -5 }, 1000), null);
  assert.equal(createDashboardRequest({ accountId: 'a', workspaceId: 'w', conversationId: null, prompt: 'p' }, Number.NaN, 'n'), null);
  assert.equal(createDashboardRequest({ accountId: 'a', workspaceId: 'w', conversationId: null, prompt: 'p' }, 1000, ''), null);
});

test('consumption is verified before the request counts as used', () => {
  const wire = createDashboardRequest({ accountId: 'a', workspaceId: 'w', conversationId: null, prompt: 'p' }, 1000, 'n');
  // A store that silently keeps the value reports failure.
  const stuck = { getItem: () => 'x', setItem: () => {}, removeItem: () => {} };
  assert.equal(consumeDashboardRequest(wire.nonce, stuck), false);
  // A throwing removeItem reports failure instead of pretending success.
  const blocked = { getItem: () => 'x', setItem: () => {}, removeItem: () => { throw new Error('denied'); } };
  assert.equal(consumeDashboardRequest(wire.nonce, blocked), false);
  // Normal storage consumes exactly once.
  const store = memoryStore();
  storeDashboardRequest(wire, store);
  assert.equal(consumeDashboardRequest(wire.nonce, store), true);
  assert.equal(readDashboardRequest(wire.nonce, store, 1000).error, 'missing');
});

test('draft commit leaves draft and envelope intact when the draft write is denied', () => {
  const wire = createDashboardRequest({ accountId: 'a', workspaceId: 'w', conversationId: null, prompt: 'p' }, 1000, 'n');
  const map = new Map([['canter:dashboard-request:n', serializeDashboardRequest(wire)], ['draft-key', 'old draft']]);
  const denied = {
    getItem: (k) => (map.has(k) ? map.get(k) : null),
    setItem: (k) => { if (k === 'draft-key') throw new Error('denied'); map.set(k, 'x'); },
    removeItem: (k) => map.delete(k),
  };
  assert.equal(commitDashboardRequestToDraft('n', denied, 'draft-key', 'old draft\n\nnew request'), false);
  // The envelope stays usable and the existing draft is untouched.
  assert.equal(readDashboardRequest('n', denied, 1000).request?.prompt, 'p');
  assert.equal(denied.getItem('draft-key'), 'old draft');
});

test('draft commit restores the previous draft when consumption fails', () => {
  const wire = createDashboardRequest({ accountId: 'a', workspaceId: 'w', conversationId: null, prompt: 'p' }, 1000, 'n');
  const map = new Map([['canter:dashboard-request:n', serializeDashboardRequest(wire)], ['draft-key', 'old draft']]);
  const stuck = {
    getItem: (k) => (map.has(k) ? map.get(k) : null),
    setItem: (k, v) => map.set(k, v),
    removeItem: () => {}, // never removes
  };
  assert.equal(commitDashboardRequestToDraft('n', stuck, 'draft-key', 'old draft\n\nnew request'), false);
  assert.equal(stuck.getItem('draft-key'), 'old draft');
  assert.notEqual(readDashboardRequest('n', stuck, 1000).request, undefined);
  // With no prior draft the restore writes an empty draft via setItem rather
  // than removeItem, so a store refusing removals still rolls back fully and
  // the unconsumed envelope lets the user retry cleanly.
  const removed = [];
  const map2 = new Map([['canter:dashboard-request:n', serializeDashboardRequest(wire)]]);
  const stuck2 = {
    getItem: (k) => (map2.has(k) ? map2.get(k) : null),
    setItem: (k, v) => map2.set(k, v),
    removeItem: (k) => { removed.push(k); },
  };
  assert.equal(commitDashboardRequestToDraft('n', stuck2, 'draft-key', 'new request'), false);
  assert.equal(stuck2.getItem('draft-key'), '');
  assert.deepEqual(removed, ['canter:dashboard-request:n']);
  assert.notEqual(readDashboardRequest('n', stuck2, 1000).request, undefined);
});

test('normal draft commit appends once and retires the envelope', () => {
  const wire = createDashboardRequest({ accountId: 'a', workspaceId: 'w', conversationId: null, prompt: 'p' }, 1000, 'n');
  const store = memoryStore();
  storeDashboardRequest(wire, store);
  store.setItem('draft-key', 'old draft');
  assert.equal(commitDashboardRequestToDraft('n', store, 'draft-key', 'old draft\n\nnew request'), true);
  assert.equal(store.getItem('draft-key'), 'old draft\n\nnew request');
  assert.equal(readDashboardRequest('n', store, 1000).error, 'missing');
  // Storage exceptions on every surface stay contained and report failure.
  const hostile = { getItem: () => { throw new Error('denied'); }, setItem: () => { throw new Error('denied'); }, removeItem: () => { throw new Error('denied'); } };
  assert.equal(commitDashboardRequestToDraft('n', hostile, 'draft-key', 'x'), false);
});

test('matching requires exact account, workspace and destination', () => {
  const request = createDashboardRequest(base, 1000, 'n6');
  assert.ok(request);
  assert.ok(!dashboardRequestMatches(request, { accountId: 'acct_2', workspaceId: 'wsp_1', conversationId: 'conv_1' }));
  assert.ok(!dashboardRequestMatches(request, { accountId: 'acct_1', workspaceId: 'wsp_2', conversationId: 'conv_1' }));
  assert.ok(!dashboardRequestMatches(request, { accountId: 'acct_1', workspaceId: 'wsp_1', conversationId: null }));
});
