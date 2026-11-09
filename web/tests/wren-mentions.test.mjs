import test from 'node:test';
import assert from 'node:assert/strict';
import { wrenMention, wrenMentionChoices } from '../src/lib/wren-mentions.ts';
const wrens = [{id:'peer',name:'Fieldwork Peer',role:'Research'}, {id:'qa',name:'Fieldwork QA',role:'Review'}];
test('multiword Wren names remain selectable without treating email or following prose as mentions',()=>{
  assert.equal(wrenMention('Ask @Fieldwork P',16,wrens)?.query,'Fieldwork P');
  assert.equal(wrenMention('test@Fieldwork',14,wrens),null);
  assert.equal(wrenMention('@Fieldwork Peer do the review',28,wrens),null);
  assert.equal(wrenMention('@owner/repo',11,wrens),null);
  assert.equal(wrenMention('@Fieldwork Peer',15,wrens)?.query,'Fieldwork Peer');
});
test('rooms use the actual roster, apps and repositories, never private conversations or hosting controls',()=>{
  const choices=wrenMentionChoices(wrens,true,'');
  assert.deepEqual(choices.map(c=>c.kind),['wren','wren','team','apps','github']);
  assert.deepEqual(wrenMentionChoices(wrens,true,'fieldwork p').map(c=>c.wren.id),['peer']);
  assert.equal(wrenMentionChoices([],true,'team').length,0);
  assert.equal(wrenMentionChoices(wrens.slice(0,1),true,'team').length,0);
  assert.equal(wrenMentionChoices([],true,'conversations').length,0);
});
test('private chat context remains available without exposing room-only team routing',()=>{
  const choices=wrenMentionChoices(wrens,false,'');
  assert.ok(choices.some(c=>c.kind==='conversations'));
  assert.ok(!choices.some(c=>c.kind==='team'));
  assert.deepEqual(wrenMention('@github canter',14,wrens),{start:0,end:14,category:'github',query:'canter'});
  assert.equal(wrenMention('@conversations ',15,wrens)?.category,'conversations');
});
