import test from 'node:test';
import assert from 'node:assert/strict';
import {appendDictationDraft,dictationWav,monoAudioChunk,maxDictationBytes} from '../src/lib/wren-dictation.ts';

test('dictation emits an exact mono PCM16 WAV with clipped signed samples',()=>{
 const audio=dictationWav([new Float32Array([-2,-1,0,1,2,NaN])],16000);
 const view=new DataView(audio.buffer),text=(start,length)=>String.fromCharCode(...audio.slice(start,start+length));
 assert.equal(text(0,4),'RIFF');assert.equal(view.getUint32(4,true)+8,audio.length);assert.equal(text(8,4),'WAVE');
 assert.equal(view.getUint32(16,true),16);assert.equal(view.getUint16(20,true),1);assert.equal(view.getUint16(22,true),1);
 assert.equal(view.getUint32(24,true),16000);assert.equal(view.getUint32(28,true),32000);assert.equal(view.getUint16(32,true),2);assert.equal(view.getUint16(34,true),16);
 assert.equal(text(36,4),'data');assert.equal(view.getUint32(40,true),12);
 assert.deepEqual(Array.from({length:6},(_,i)=>view.getInt16(44+i*2,true)),[-32768,-32768,0,32767,32767,0]);
});
test('device-rate conversion preserves sample duration and maximum recording bounds',()=>{
 const audio=dictationWav([new Float32Array(48000),new Float32Array(48000)],48000);
 assert.equal((audio.length-44)/32000,2);
 const maximum=dictationWav([new Float32Array(16000*60)],16000);
 assert.equal((maximum.length-44)/32000,60);assert.ok(maximum.length<=maxDictationBytes);
 assert.throws(()=>dictationWav([new Float32Array(16000*61)],16000),/60 seconds/);
 assert.throws(()=>dictationWav([],16000),/No audio/);
 assert.throws(()=>dictationWav([new Float32Array(1)],NaN),/unsupported/);
});
test('channel mixing produces mono without mutating microphone buffers',()=>{
 const left=new Float32Array([1,-1,NaN]),right=new Float32Array([-1,1,1]);
 assert.deepEqual([...monoAudioChunk([left,right])],[0,0,.5]);
 assert.deepEqual([...monoAudioChunk([left,right],1)],[0]);assert.equal(left[0],1);assert.equal(right[0],-1);
});
test('transcript appends to the current editable draft and never silently replaces or truncates it',()=>{
 assert.equal(appendDictationDraft('Already typed','  spoken words.  '),'Already typed spoken words.');
 assert.equal(appendDictationDraft('New paragraph\n','Spoken words'),'New paragraph\nSpoken words');
 assert.equal(appendDictationDraft('','spoken'),'spoken');assert.equal(appendDictationDraft('Keep this','  '),'Keep this');
 assert.equal(appendDictationDraft('12345','too much',10),null);
});
