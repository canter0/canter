import test from 'node:test';
import assert from 'node:assert/strict';
import { conversationState, conversationPreview, conversationHref } from '../src/lib/wren-presentation.ts';

test('queue, execution and unavailable identity remain distinct across directory and roster', () => {
  assert.equal(conversationState({status:'queued'}).label, 'Queued');
  assert.equal(conversationState({status:'running'}).label, 'Working');
  assert.equal(conversationState({status:'running',sharedAvailable:false}).label, 'Unavailable');
  assert.equal(conversationState({status:'completed',lastMessage:'Response'}).label, 'Last conversation');
  assert.equal(conversationState({status:'failed'}).tone, 'attention');
});
test('quiet previews carry actual message or purpose without manufacturing a result', () => {
  assert.equal(conversationPreview({status:'idle',purpose:'Compare **sources**\ncarefully.'}), 'Compare sources carefully.');
  assert.equal(conversationPreview({status:'completed',lastMessage:'Saved `brief.md`',purpose:'Research'}), 'Saved brief.md');
  assert.equal(conversationPreview({status:'idle',purpose:''}), 'No messages yet');
  assert.equal(conversationPreview({status:'queued',lastMessage:'Old result'}), 'Queued');
});
test('overview links preserve individual and shared conversation routes', () => {
  assert.equal(conversationHref({id:'wren_one'}), '/app/wrens/wren_one');
  assert.equal(conversationHref({id:'room_one',kind:'channel'}), '/app/wrens/rooms/room_one');
  assert.equal(conversationHref({id:'wren/one'}), '/app/wrens/wren%2Fone');
});

test('queued identities do not use the executing avatar animation state', async () => {
  const { wrenState } = await import('../src/lib/wrens.ts');
  assert.equal(wrenState('queued'), 'queued');
  assert.equal(wrenState('running'), 'working');
  assert.equal(wrenState('failed'), 'blocked');
});

test('message previews keep readable link labels including truncated server previews',()=>{
  assert.equal(conversationPreview({status:'completed',lastMessage:'See [the source](https://example.com/notes).'}),'See the source.');
  assert.equal(conversationPreview({status:'completed',lastMessage:'Result. [Source: SDK](https://deepwiki.com/modelc'}),'Result. Source: SDK');
  assert.equal(conversationPreview({status:'completed',lastMessage:'## Findings\n- **First**\n- `Second`'}),'Findings First Second');
});
