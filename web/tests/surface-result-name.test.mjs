import test from 'node:test';
import assert from 'node:assert/strict';
import { surfaceResultName } from '../src/lib/surface-result-name.ts';
const artifact=(artifactId,name)=>({kind:'artifact',data:{artifactId,name}});
test('published file labels resolve conversation events outside the model run and latest revision wins',()=>{
 const events=[artifact('a','draft.md'),artifact('b','unrelated.pdf'),artifact('a','review.md')];
 assert.equal(surfaceResultName({kind:'wren-artifact',id:'a'},events),'review.md');
 assert.equal(surfaceResultName({kind:'wren-artifact',id:'unknown'},events),undefined);
 assert.equal(surfaceResultName({kind:'computer',id:'a'},events),undefined);
});
test('a saved memory result follows its own current title, including a forgotten entry',()=>{
 const events=[{kind:'memory',data:{memoryId:'m',title:'Launch decision'}},{kind:'memory',data:{memoryId:'other',title:'Other room note'}}];
 assert.equal(surfaceResultName({kind:'wren-memory',id:'m'},events),'Launch decision');
 assert.equal(surfaceResultName({kind:'wren-memory',id:'missing'},events),undefined);
 events.push({kind:'memory',data:{memoryId:'m',title:'Forgotten memory',deleted:true}});
 assert.equal(surfaceResultName({kind:'wren-memory',id:'m'},events),'Forgotten memory');
 assert.equal(surfaceResultName({kind:'wren-artifact',id:'m'},events),undefined);
});
