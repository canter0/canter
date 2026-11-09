import test from 'node:test';
import assert from 'node:assert/strict';
import {desktopKey,desktopInputQueue} from '../src/lib/wren-desktop-input.ts';
const key=(key,extra={})=>desktopKey({key,ctrlKey:false,metaKey:false,altKey:false,shiftKey:false,...extra});
test('native desktop typing preserves text and maps host shortcuts to the Linux guest',()=>{
 assert.deepEqual(key('é'),{kind:'type',text:'é'});
 assert.deepEqual(key('A',{shiftKey:true}),{kind:'type',text:'A'});
 assert.deepEqual(key('a',{metaKey:true}),{kind:'key',key:'ctrl+a'});
 assert.deepEqual(key('ArrowLeft',{ctrlKey:true,shiftKey:true}),{kind:'key',key:'ctrl+shift+Left'});
 assert.equal(key('Process',{isComposing:true}),null);
 assert.equal(key('Control'),null);
});
test('typing behind a click stays ordered and coalesces without losing characters',async()=>{
 const calls=[];let complete;
 const queue=desktopInputQueue(async input=>{calls.push({...input});if(calls.length===1)await new Promise(r=>complete=r);return true;});
 queue.push({kind:'click',x:10,y:20});queue.push({kind:'type',text:'hello'});queue.push({kind:'type',text:' world'});queue.push({kind:'key',key:'Return'});
 complete();await new Promise(r=>setImmediate(r));
 assert.deepEqual(calls,[{kind:'click',x:10,y:20},{kind:'type',text:'hello world'},{kind:'key',key:'Return'}]);queue.dispose();
});
test('a failed mutation cancels pending input instead of replaying into unknown focus',async()=>{
 const calls=[];let fail;
 const queue=desktopInputQueue(async input=>{calls.push(input);await new Promise(r=>fail=r);return false;});
 queue.push({kind:'click',x:10,y:20});queue.push({kind:'type',text:'must not send'});fail();await new Promise(r=>setImmediate(r));assert.equal(calls.length,1);queue.dispose();
});
test('loss of control cancels queued input and dispose rejects new input',async()=>{
 const calls=[];let finish;
 const queue=desktopInputQueue(async input=>{calls.push(input);await new Promise(r=>finish=r);return true;});
 queue.push({kind:'click',x:1,y:1});queue.push({kind:'type',text:'cancel'});queue.clear();finish();await new Promise(r=>setImmediate(r));assert.equal(calls.length,1);queue.dispose();assert.equal(queue.push({kind:'key',key:'Return'}),false);
});
