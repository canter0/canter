import test from 'node:test';
import assert from 'node:assert/strict';
import {readScreenFrames} from '../src/lib/wren-screen-stream.ts';
const packet=bytes=>{const b=new Uint8Array(bytes.length+4);new DataView(b.buffer).setUint32(0,bytes.length);b.set(bytes,4);return b;};
const stream=chunks=>new ReadableStream({start(c){for(const bytes of chunks)c.enqueue(bytes);c.close();}});
test('screen stream preserves frame boundaries across split headers and combined chunks',async()=>{
 const raw=new Uint8Array([...packet([1,2,3]),...packet([4,5]),...packet([])]),seen=[];
 await readScreenFrames(stream([...raw].map(n=>Uint8Array.of(n))),async b=>seen.push([...b]));assert.deepEqual(seen,[[1,2,3],[4,5]]);
 const combined=[];await readScreenFrames(stream([raw]),async b=>combined.push([...b]));assert.deepEqual(combined,seen);
});
test('truncated and oversized feeds fail rather than leaving a falsely live screen',async()=>{
 await assert.rejects(readScreenFrames(stream([packet([1,2]).subarray(0,5)]),async()=>{}),/interrupted/);
 const huge=new Uint8Array(4);new DataView(huge.buffer).setUint32(0,0xffffffff);
 await assert.rejects(readScreenFrames(stream([huge]),async()=>{}),/size/);
});
test('a failed viewer releases the reader without consuming more frames',async()=>{
 let cancelled=false;const body=new ReadableStream({start(c){c.enqueue(packet([1]));},cancel(){cancelled=true;}});
 await assert.rejects(readScreenFrames(body,async()=>{throw Error('gone');}),/gone/);assert.equal(cancelled,true);assert.equal(body.locked,false);
});
