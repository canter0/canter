import test from 'node:test';
import assert from 'node:assert/strict';
import {replyMessageContent,eligibleEmailResponsibilities,inboxStateLabel,messageWebhookPath,normalizedMessageContent,messageAddresses,messageMayEdit,messageMaySend,messageMayReject,messageStateLabel,validateMessageRecipients} from '../src/lib/wren-messaging.ts';
const connection={ownerId:'owner',enabled:true},draft={creatorId:'author',state:'draft'};
test('only connection owner sends; creator may edit/reject but a room writer cannot send someone else’s identity',()=>{
 assert.equal(messageMayEdit(draft,connection,'author',true),true);
 assert.equal(messageMaySend(draft,connection,'author',true),false);
 assert.equal(messageMaySend(draft,connection,'owner',true),true);
 assert.equal(messageMayEdit(draft,connection,'other-writer',true),false);
 assert.equal(messageMaySend(draft,connection,'owner',false),false);
 for(const state of ['dispatching','accepted','delivered','unknown','failed','rejected']){
  assert.equal(messageMaySend({...draft,state},connection,'owner',true),false);
  assert.equal(messageMayEdit({...draft,state},connection,'author',true),false);
 }
});
test('disabled or revoked credentials cannot send, but rejecting an unsent draft remains available',()=>{
 for(const value of [{...connection,enabled:false},{...connection,revokedAt:'2026-10-26'}]){
  assert.equal(messageMaySend(draft,value,'owner',true),false);
  assert.equal(messageMayReject(draft,value,'author',true),true);
 }
 assert.equal(messageMayReject(draft,connection,'viewer',false),false);
 assert.equal(messageMayReject(draft,connection,'other',true),false);
});
test('recipient parsing preserves explicit To/CC/BCC semantics and rejects duplicate or injected recipients',()=>{
 assert.deepEqual(messageAddresses('a@example.test, b@example.test\nc@example.test'),['a@example.test','b@example.test','c@example.test']);
 assert.throws(()=>messageAddresses('Alice <alice@example.test>'));
 assert.throws(()=>messageAddresses('a@example.test\rInjected: value'));
 assert.throws(()=>validateMessageRecipients({to:['a@example.test'],cc:['A@example.test'],bcc:[]}));
 assert.throws(()=>validateMessageRecipients({to:[],cc:['a@example.test'],bcc:[]}));
 assert.throws(()=>validateMessageRecipients({to:Array.from({length:21},(_,i)=>`${i}@example.test`),cc:[],bcc:[]}));
});
test('provider acceptance is not labelled delivery and ambiguous outcomes remain unconfirmed',()=>{
 assert.equal(messageStateLabel('accepted'),'Accepted by provider');
 assert.equal(messageStateLabel('delivered'),'Delivered');
 assert.equal(messageStateLabel('unknown'),'Outcome unconfirmed');
 assert.equal(messageStateLabel('new-unrecognized-state'),'Status unavailable');
});

test('receiving wakes require explicit owned active email event responsibility in this exact conversation',()=>{
 const valid={id:'valid',ownerId:'me',conversationId:'here',state:'active',schedule:{kind:'event',eventName:'email_received'}};
 const values=[valid,{...valid,id:'other-person',ownerId:'other'},{...valid,id:'other-chat',conversationId:'elsewhere'},{...valid,id:'paused',state:'paused'},{...valid,id:'timer',schedule:{kind:'interval'}},{...valid,id:'other-event',schedule:{kind:'event',eventName:'other'}}];
 assert.deepEqual(eligibleEmailResponsibilities(values,'me','here').map(item=>item.id),['valid']);
 assert.equal(inboxStateLabel('pending'),'Waiting for message body');
 assert.equal(inboxStateLabel('blocked'),'Connection unavailable');
 assert.equal(messageWebhookPath('mailconn_a/b'),'/api/canter/wren-message-webhooks/mailconn_a%2Fb');
});
test('reply uses source identity and sender only; does not inherit recipients, attachments or caller-supplied thread headers',()=>{
 const source={id:'mailin_source',from:'Alice Example <alice@example.test>',subject:'A question',to:['team@example.test'],cc:['private@example.test']};
 const reply=replyMessageContent(source);
 assert.deepEqual(reply,{to:['alice@example.test'],cc:[],bcc:[],subject:'Re: A question',text:'',attachments:[],inReplyToId:'mailin_source'});
 assert.equal(replyMessageContent({...source,subject:'Re: Already replied'}).subject,'Re: Already replied');
 assert.deepEqual(replyMessageContent({...source,from:'malformed sender'}).to,[]);
 assert.deepEqual(replyMessageContent({...source,from:'a@example.test,b@example.test'}).to,[]);
});
test('optional empty provider arrays normalize without inventing message content or altering pinned attachments',()=>{
 const input={to:['a@example.test'],cc:null,bcc:null,attachments:null,subject:'Actual subject',text:'Actual body',inReplyToId:'mailin_real'};
 assert.deepEqual(normalizedMessageContent(input),{...input,cc:[],bcc:[],attachments:[]});
 const attachment={id:'artifact_real',revision:2,sha256:'real'};
 assert.equal(normalizedMessageContent({...input,attachments:[attachment]}).attachments[0],attachment);
});
