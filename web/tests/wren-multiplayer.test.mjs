import test from 'node:test';
import assert from 'node:assert/strict';
import {canMarkWrenRead,wrenSearchDestination,wrenSearchBytes} from '../src/lib/wren-multiplayer.ts';
test('read cursors advance only in a focused visible conversation at its live edge',()=>{
 const value={visible:true,focused:true,covered:false,gap:0,sequence:123};
 assert.equal(canMarkWrenRead(value),true);
 for(const change of [{visible:false},{focused:false},{covered:true},{gap:80},{sequence:0},{sequence:NaN}])assert.equal(canMarkWrenRead({...value,...change}),false);
});
test('search opens only authorized known conversations and preserves exact result identity',()=>{
 const wrens=[{id:'wren_one',conversationId:'private'}],rooms=[{id:'room_one',conversationId:'shared'}];
 assert.equal(wrenSearchDestination({kind:'message',id:'result',messageId:'message/a',conversationId:'shared'},wrens,rooms),'/app/wrens/rooms/room_one?message=message%2Fa');
 assert.equal(wrenSearchDestination({kind:'artifact',id:'artifact_one',conversationId:'private'},wrens,rooms),'/app/wrens/wren_one?work=wren-artifact&workId=artifact_one');
 assert.equal(wrenSearchDestination({kind:'job',id:'private_child',conversationId:'unknown'},wrens,rooms),null);
});
test('search length follows UTF8 bytes rather than JavaScript code units',()=>{
 assert.equal(wrenSearchBytes(' ab '),2);assert.equal(wrenSearchBytes('猫'),3);
});
