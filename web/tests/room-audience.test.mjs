import test from 'node:test';
import assert from 'node:assert/strict';
import { roomAudience, roomMentions, restoredRoomRecipient } from '../src/lib/room-audience.ts';
const wrens = [{id:'one',name:'Fieldwork Peer'}, {id:'two',name:'Scout+'}];
test('ordinary room conversation does not automatically wake a Wren', () => {
  assert.deepEqual(roomAudience('people', 'Hello everyone', wrens), {postOnly:true,recipientIds:[]});
  assert.equal(roomAudience('people', '@Unknown hello', wrens).postOnly, true);
});
test('explicit mentions override the selector with exact bounded names', () => {
  assert.deepEqual(roomAudience('people', '@Fieldwork Peer, check this', wrens).recipientIds, ['one']);
  assert.deepEqual(roomAudience('one', '@Scout+!', wrens).recipientIds, ['two']);
  assert.deepEqual(roomAudience('people', 'mail@Fieldwork Peer and @Scout+extra', wrens).recipientIds, []);
  assert.deepEqual(roomAudience('people', '@TEAM check', wrens).recipientIds, ['one','two']);
});
test('removed recipients and people-only rooms stay people-only', () => {
  assert.equal(roomAudience('missing', '', wrens).postOnly, true);
  assert.equal(roomAudience('team', '@team', []).postOnly, true);
});

test('staged mentions preserve first appearance order without duplicate replies', () => {
  const text='@Scout+ draft first.\n@Fieldwork Peer review it. @Scout+ owns the draft.';
  assert.deepEqual(roomAudience('one',text,wrens).recipientIds,['two','one']);
  assert.deepEqual(roomMentions(text,wrens).map(item=>item.name),['Scout+','Fieldwork Peer']);
  assert.deepEqual(roomAudience('two','@team work together',wrens).recipientIds,['one','two']);
  assert.deepEqual(roomMentions('mail@Scout+ and @Scout+extra',wrens),[]);
});

test('restored recipient keeps an explicit choice but cannot target a removed Wren', () => {
  assert.equal(restoredRoomRecipient('one',wrens),'one');
  assert.equal(restoredRoomRecipient('team',wrens),'team');
  assert.equal(restoredRoomRecipient('removed',wrens),'people');
  assert.equal(restoredRoomRecipient(null,wrens),'people');
  assert.equal(restoredRoomRecipient('one',[]),'people');
  assert.deepEqual(roomAudience(restoredRoomRecipient('removed',wrens),'Keep this draft',wrens),{postOnly:true,recipientIds:[]});
});
