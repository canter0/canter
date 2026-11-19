import test from 'node:test';
import assert from 'node:assert/strict';
import { knowledgeTarget, knowledgeQuery, memoryForgetNotices, parseSkillCapabilities, roomMemoryAudience, skillInstallationLabel, visibleMemory } from '../src/lib/wren-knowledge.ts';

test('private knowledge follows the owned Wren chat, while a room has an explicitly shared scope', () => {
  const wren = { id: 'wren_a', conversationId: 'conv_a', name: 'A' };
  const room = { id: 'room_a', conversationId: 'conv_room', name: 'Project', kind: 'group' };
  assert.deepEqual(knowledgeTarget('conv_a', [wren], [room]).scope, { conversationId: 'conv_a', scope: 'private', wrenId: 'wren_a' });
  assert.deepEqual(knowledgeTarget('conv_room', [wren], [room]).scope, { conversationId: 'conv_room', scope: 'room' });
  assert.equal(knowledgeTarget('conv_foreign', [wren], [room]), null);
  assert.equal(knowledgeQuery({ conversationId: 'conv_a', scope: 'private', wrenId: 'wren_a' }), 'conversationId=conv_a&scope=private&wrenId=wren_a');
});
test('forget immediately suppresses cached old revision text and sources without rewriting an independently shared copy', () => {
  const old = { id: 'memory_private', title: 'Sensitive title', content: 'Private remembered text', sources: { urls: ['https://example.com/private'] }, revision: 1, deleted: false };
  const redacted = visibleMemory(old, new Set(['memory_private']));
  assert.equal(redacted.content, ''); assert.equal(redacted.title, 'Forgotten memory'); assert.deepEqual(redacted.sources, {}); assert.equal(redacted.deleted, true);
  assert.equal(old.content, 'Private remembered text');
  const copy = { ...old, id: 'memory_room', promotedFromId: old.id };
  assert.equal(visibleMemory(copy, new Set([old.id])).content, copy.content);
  assert.deepEqual(memoryForgetNotices([{ sequence: 1, kind: 'memory', data: { memoryId: old.id, deleted: true } }, { sequence: 2, kind: 'memory', data: { memoryId: 'other', deleted: false } }]), [{ sequence: 1, id: old.id }]);
});
test('skill dependency declarations are bounded and never equated with permission or execution readiness', () => {
  assert.deepEqual(parseSkillCapabilities('canter_search_web\ncanter_computer_input, canter_search_web'), ['canter_search_web', 'canter_computer_input']);
  assert.throws(() => parseSkillCapabilities('unsafe/tool'));
  assert.equal(skillInstallationLabel({ enabled: true, skill: { disabled: true } }), 'Disabled by author');
  assert.equal(skillInstallationLabel({ enabled: false, skill: { disabled: false } }), 'Disabled here');
  assert.equal(skillInstallationLabel({ enabled: true, skill: { disabled: false } }), 'Enabled here');
  assert.match(roomMemoryAudience({ kind: 'channel', name: 'Project' }, 'Canter'), /Everyone in Canter/);
  assert.match(roomMemoryAudience({ kind: 'group', name: 'Private group' }, 'Canter'), /invited people and Wrens/);
});

test('shared-copy navigation resolves the exact confirmed record and current room, never a private or unknown target', async () => {
  const { sharedMemoryDestination } = await import('../src/lib/wren-knowledge.ts');
  const rooms = [{ id: 'room_alpha', conversationId: 'conv_alpha', name: 'Research' }];
  const copy = { id: 'memory_copy', conversationId: 'conv_alpha', scope: 'room', deleted: false };
  assert.deepEqual(sharedMemoryDestination(copy, rooms), { name: 'Research', href: '/app/wrens/rooms/room_alpha?work=wren-memory&workId=memory_copy' });
  assert.equal(sharedMemoryDestination({...copy, scope: 'private'}, rooms), null);
  assert.equal(sharedMemoryDestination({...copy, deleted: true}, rooms), null);
  assert.equal(sharedMemoryDestination({...copy, conversationId: 'conv_elsewhere'}, rooms), null);
  assert.equal(sharedMemoryDestination(copy, []), null);
});

test('extension scope adds ordinary owner chats without broadening memory or skill scopes (synthetic)', async () => {
  const { extensionTarget, extensionQuery } = await import('../src/lib/wren-knowledge.ts');
  const wren = { id: 'wren_a', conversationId: 'conv_wren', name: 'A' };
  const room = { id: 'room_a', conversationId: 'conv_room', name: 'Project', kind: 'group' };
  const conversations = [
    { id: 'conv_plain', title: 'Budget chat', ownerAccountId: 'acct_me' },
    { id: 'conv_shared', title: 'Shared chat', ownerAccountId: 'acct_me', shared: true },
    { id: 'conv_peer', title: 'Peer chat', ownerAccountId: 'acct_peer', shared: true },
    { id: 'conv_wren', title: 'Wren chat' },
    { id: 'conv_room', title: 'Room chat' },
  ];
  // Ordinary chats resolve the extension-only operator scope with a truthful label.
  assert.deepEqual(extensionTarget('conv_plain', [wren], [room], conversations, 'acct_me').scope, { conversationId: 'conv_plain', scope: 'operator' });
  assert.match(extensionTarget('conv_plain', [wren], [room], conversations, 'acct_me').label, /Budget chat/);
  // A chat once shared, a peer's shared chat, and a missing account never get a target.
  assert.equal(extensionTarget('conv_shared', [wren], [room], conversations, 'acct_me'), null);
  assert.equal(extensionTarget('conv_peer', [wren], [room], conversations, 'acct_me'), null);
  assert.equal(extensionTarget('conv_plain', [wren], [room], conversations), null);
  // Rooms and Wren chats keep their existing scopes; unknown or foreign ids never guess.
  assert.deepEqual(extensionTarget('conv_wren', [wren], [room], conversations, 'acct_me').scope, { conversationId: 'conv_wren', scope: 'private', wrenId: 'wren_a' });
  assert.deepEqual(extensionTarget('conv_room', [wren], [room], conversations, 'acct_me').scope, { conversationId: 'conv_room', scope: 'room' });
  assert.equal(extensionTarget('conv_foreign', [wren], [room], conversations, 'acct_me'), null);
  assert.equal(extensionTarget(undefined, [wren], [room], conversations, 'acct_me'), null);
  assert.equal(extensionQuery({ conversationId: 'conv_plain', scope: 'operator' }), 'conversationId=conv_plain&scope=operator');
  // Memory/skill targets still reject the operator scope entirely.
  assert.equal(knowledgeTarget('conv_plain', [wren], [room]), null);
});
