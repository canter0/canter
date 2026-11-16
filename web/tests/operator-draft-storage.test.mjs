import test from 'node:test';
import assert from 'node:assert/strict';
import { clearOperatorTextDrafts, operatorDraftKey, operatorReplyDraftKey, serializeOperatorReplyAnchor, parseOperatorReplyAnchor, isReplyTargetForAnchor } from '../src/lib/operator-draft-storage.ts';

test('draft keys isolate accounts, workspaces and conversations for text and attachments', () => {
  const owner = operatorDraftKey('owner', 'workspace', 'conv_1');
  const viewer = operatorDraftKey('viewer', 'workspace', 'conv_1');
  const otherWorkspace = operatorDraftKey('owner', 'other', 'conv_1');
  const otherConversation = operatorDraftKey('owner', 'workspace', 'conv_2');
  const fresh = operatorDraftKey('owner', 'workspace');
  assert.equal(new Set([owner, viewer, otherWorkspace, otherConversation, fresh]).size, 5);
  assert.equal(fresh, operatorDraftKey('owner', 'workspace', null));
  const text = new Map([[owner, 'owner-only prompt']]);
  const attachments = new Map([[owner, [{ name: 'private.pdf' }]]]);
  assert.equal(text.get(viewer), undefined);
  assert.equal(attachments.get(viewer), undefined);
  assert.equal(text.get(otherWorkspace), undefined);
  assert.equal(attachments.get(otherConversation), undefined);
});

test('missing identity never selects an ownerless legacy record', () => {
  for (const missing of [null, undefined, '', '  ']) {
    assert.equal(operatorDraftKey(missing, 'workspace', 'conv_1'), null);
    assert.equal(operatorDraftKey('account', missing, 'conv_1'), null);
  }
  assert.equal(operatorDraftKey('account', 'workspace', ''), null);
  const legacy = new Map([['canter:conversation-draft:workspace:conv_1', 'legacy prompt']]);
  assert.equal(legacy.get(operatorDraftKey('account', 'workspace', 'conv_1')), undefined);
  assert.equal(legacy.size, 1); // No automatic legacy migration or deletion.
});

test('opaque identity segments cannot collide through separators', () => {
  assert.notEqual(operatorDraftKey('a:b', 'c', 'd'), operatorDraftKey('a', 'b:c', 'd'));
  assert.notEqual(operatorDraftKey('a', 'b:c', 'd'), operatorDraftKey('a', 'b', 'c:d'));
});

test('sign-out cleanup removes every account-scoped and legacy text draft while preserving preferences', () => {
  const values = new Map([
    ['canter:conversation-draft:workspace:new', 'legacy unsent prompt'],
    ['canter:conversation-draft:workspace:conv_1', 'legacy saved-conversation prompt'],
    [operatorDraftKey('owner', 'workspace'), 'new prompt'],
    [operatorDraftKey('owner', 'workspace', 'conv_1'), 'saved prompt'],
    [operatorDraftKey('owner', 'workspace', 'conv_1')+':audience', 'wren_1'],
    [operatorDraftKey('viewer', 'workspace', 'conv_1'), 'other account prompt'],
    [operatorReplyDraftKey('owner', 'workspace', 'conv_1'), serializeOperatorReplyAnchor({conversationId:'conv_1',messageId:'msg_1'})],
    ['canter:conversation-model:account:workspace:conv_1', 'model-id'],
    ['canter:conversation-drafts:preference', 'unrelated preference'],
  ]);
  const storage = {
    get length() { return values.size; },
    key(index) { return [...values.keys()][index] ?? null; },
    removeItem(key) { values.delete(key); },
  };
  clearOperatorTextDrafts(storage);
  clearOperatorTextDrafts(storage); // Repeated cleanup is safe.
  assert.deepEqual([...values], [
    ['canter:conversation-model:account:workspace:conv_1', 'model-id'],
    ['canter:conversation-drafts:preference', 'unrelated preference'],
  ]);
});


test('reply anchors persist only scoped IDs and never trust a stored quote or routing grant', () => {
  const key = operatorReplyDraftKey('owner', 'workspace', 'conv_1');
  assert.notEqual(key, operatorReplyDraftKey('viewer', 'workspace', 'conv_1'));
  assert.notEqual(key, operatorReplyDraftKey('owner', 'other', 'conv_1'));
  assert.equal(operatorReplyDraftKey(null, 'workspace', 'conv_1'), null);
  assert.equal(operatorReplyDraftKey('owner', 'workspace', undefined), null);
  const raw = serializeOperatorReplyAnchor({conversationId:'conv_1',messageId:'msg_1',senderName:'Stale Reading',excerpt:'private quote',canWake:true});
  assert.deepEqual(JSON.parse(raw), {version:1,conversationId:'conv_1',messageId:'msg_1'});
  assert.deepEqual(parseOperatorReplyAnchor(raw,'conv_1'), {version:1,conversationId:'conv_1',messageId:'msg_1'});
  assert.equal(parseOperatorReplyAnchor(raw,'conv_2'), null);
  assert.equal(parseOperatorReplyAnchor('{bad json','conv_1'), null);
  assert.equal(parseOperatorReplyAnchor(JSON.stringify({version:1,conversationId:'conv_1',messageId:''}),'conv_1'), null);
  assert.equal(parseOperatorReplyAnchor(JSON.stringify({version:2,conversationId:'conv_1',messageId:'msg_1'}),'conv_1'), null);
});

test('reply restoration accepts only the authoritative response for the exact saved message and conversation', () => {
  const anchor = {version:1,conversationId:'conv_1',messageId:'msg_1'};
  const resolved = {conversationId:'conv_1',messageId:'msg_1',role:'assistant',senderName:'Reading',excerpt:'Current server quote',wrenId:'wren_1',canWake:false};
  assert.equal(isReplyTargetForAnchor(resolved,anchor),true);
  assert.equal(isReplyTargetForAnchor({...resolved,messageId:'another'},anchor),false);
  assert.equal(isReplyTargetForAnchor({...resolved,conversationId:'private_other'},anchor),false);
  assert.equal(isReplyTargetForAnchor({...resolved,canWake:'true'},anchor),false);
  assert.equal(isReplyTargetForAnchor({...resolved,role:'system'},anchor),false);
  assert.equal(isReplyTargetForAnchor({messageId:'msg_1',conversationId:'conv_1'},anchor),false);
  assert.equal(isReplyTargetForAnchor(null,anchor),false);
});
