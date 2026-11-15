import test from 'node:test';
import assert from 'node:assert/strict';
import {projectConversationTarget,projectSourceURLs} from '../src/lib/wren-projects.ts';
test('project destination is the exact private conversation, never every chat using a shared persona',()=>{
 const wrens=[{id:'same-persona',conversationId:'my-dm',name:'Reading',shared:true}];
 assert.deepEqual(projectConversationTarget('my-dm','me',wrens,[]),{id:'my-dm',label:'Your private chat with Reading',canManage:true,shared:false});
 assert.equal(projectConversationTarget('other-person-dm','me',wrens,[]),null);
 assert.equal(projectConversationTarget('same-persona','me',wrens,[]),null);
});
test('shared room destination identifies its actual audience and only current owner manages attachment',()=>{
 const room={conversationId:'room-conv',name:'# Launch',kind:'channel',ownerId:'owner'};
 assert.deepEqual(projectConversationTarget('room-conv','member',[],[room]),{id:'room-conv',label:'Channel Launch',canManage:false,shared:true});
 assert.equal(projectConversationTarget('room-conv','owner',[],[room]).canManage,true);
 assert.equal(projectConversationTarget('room-conv','owner',[],[{...room,kind:'group',name:'Writers'}]).label,'Group chat Writers');
});
test('project sources require real web references without embedded credentials or executable URLs',()=>{
 assert.deepEqual(projectSourceURLs(' https://example.test/a\n\nhttp://example.test/b\nhttps://example.test/a '),['https://example.test/a','http://example.test/b']);
 for(const value of ['javascript:alert(1)','file:///private/notes','https://user:secret@example.test/a','not a URL'])assert.throws(()=>projectSourceURLs(value));
 assert.throws(()=>projectSourceURLs(Array.from({length:13},(_,i)=>`https://example.test/${i}`).join('\n')));
 assert.deepEqual(projectSourceURLs(''),[]);
});
