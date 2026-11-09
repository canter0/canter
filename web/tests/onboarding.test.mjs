import test from 'node:test';
import assert from 'node:assert/strict';
import { showOnboarding } from '../src/lib/onboarding.ts';

test('unused accounts reach onboarding through the ordinary dashboard route', () => {
  assert.equal(showOnboarding(false), true);
  assert.equal(showOnboarding(true), false);
  assert.equal(showOnboarding(undefined), false);
});

test('draft transitions bypass onboarding and explicit welcome links can revisit it', () => {
  assert.equal(showOnboarding(false, undefined, '1'), false);
  assert.equal(showOnboarding(true, '1'), true);
  assert.equal(showOnboarding(false, '0'), true);
  assert.equal(showOnboarding(true, '0'), false);
});

test('Create a Wren waits for durable onboarding completion before opening the existing creation route', async () => {
  const {openWrenOnboarding}=await import('../src/lib/onboarding.ts');
  const calls=[],destinations=[];
  let finish;
  const pending=new Promise(resolve=>{finish=resolve;});
  const action=openWrenOnboarding(async(path,init)=>{calls.push({path,init});await pending;},destination=>destinations.push(destination));
  assert.deepEqual(calls,[{path:'/me/onboarding',init:{method:'POST',keepalive:true}}]);
  assert.deepEqual(destinations,[]);
  finish();await action;
  assert.deepEqual(destinations,['/app/wrens/new']);
});
test('failed welcome completion retains onboarding instead of pretending the choice was saved', async () => {
  const {openWrenOnboarding}=await import('../src/lib/onboarding.ts');
  const destinations=[];
  await assert.rejects(openWrenOnboarding(async()=>{throw new Error('service unavailable');},destination=>destinations.push(destination)),/service unavailable/);
  assert.deepEqual(destinations,[]);
});
