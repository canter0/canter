import test from 'node:test';
import assert from 'node:assert/strict';
import {coalescedResourceRefresh,workUpdateMatchesPath} from '../src/lib/wren-resource-refresh.ts';
const turn=()=>new Promise(resolve=>setImmediate(resolve));
test('work invalidation is restricted to the exact decoded workspace, including pinned revision URLs',()=>{
 assert.equal(workUpdateMatchesPath('/workspaces/team%20one/wren-artifacts/a?revision=1',{workspaceId:'team one'}),true);
 assert.equal(workUpdateMatchesPath('/workspaces/team-one/wren-artifacts/a',{workspaceId:'team'}),false);
 assert.equal(workUpdateMatchesPath('/workspaces/team/wren-artifacts/a',{workspaceId:'other'}),false);
 for(const detail of [null,{}, {workspaceId:1}])assert.equal(workUpdateMatchesPath('/workspaces/team/wren-artifacts/a',detail),false);
 assert.equal(workUpdateMatchesPath('/workspaces/%invalid/wren-artifacts/a',{workspaceId:'team'}),false);
});
test('in-flight invalidations suppress stale metadata and coalesce into one current read',async()=>{
 const releases=[],committed=[];let calls=0;
 const loader=coalescedResourceRefresh(async current=>{
  const revision=++calls;await new Promise(resolve=>releases.push(resolve));if(current())committed.push(revision);
 });
 loader.request();loader.request();loader.request();
 assert.equal(calls,1);
 releases.shift()();await turn();assert.equal(calls,2);assert.deepEqual(committed,[]);
 releases.shift()();await turn();assert.deepEqual(committed,[2]);
 loader.dispose();
});
test('updates arriving during a follow-up are retained and a disposed resource never commits',async()=>{
 const releases=[],committed=[];let calls=0;
 const loader=coalescedResourceRefresh(async current=>{
  const revision=++calls;await new Promise(resolve=>releases.push(resolve));if(current())committed.push(revision);
 });
 loader.request();loader.request();releases.shift()();await turn();
 loader.request();releases.shift()();await turn();assert.equal(calls,3);assert.deepEqual(committed,[]);
 loader.dispose();loader.request();releases.shift()();await turn();
 assert.equal(calls,3);assert.deepEqual(committed,[]);
});
test('completed reads permit later independent refreshes',async()=>{
 let calls=0;const loader=coalescedResourceRefresh(async current=>{if(current())calls++;});
 loader.request();await turn();loader.request();await turn();assert.equal(calls,2);loader.dispose();
});
