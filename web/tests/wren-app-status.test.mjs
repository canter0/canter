import test from 'node:test';
import assert from 'node:assert/strict';
import {appStatusSnapshot} from '../src/lib/wren-app-status.ts';

const event=(result,name='canter_show_connections',status='completed')=>({kind:'tool',data:{name,status,result}});
test('connection card uses the latest actual access check, never catalog availability',()=>{
  assert.deepEqual(appStatusSnapshot([event({publicSources:[{name:'DeepWiki'}]})]),[]);
  assert.deepEqual(appStatusSnapshot([
    event({conversationConnections:[{id:'i',name:'DeepWiki',state:'available'}]}),
    event({conversationConnections:[{id:'i',name:'DeepWiki',state:'paused',credential:'do not render',canManage:true}]}),
  ]),[{id:'i',name:'DeepWiki',label:'Paused'}]);
});
test('unrelated tools and malformed statuses cannot become an app status card',()=>{
  const result={conversationConnections:[{id:'i',name:'Bad',state:'__proto__'},null,{id:'i',name:'App',state:'made_up'}]};
  assert.deepEqual(appStatusSnapshot([event(result)]),[]);
  assert.deepEqual(appStatusSnapshot([event({conversationConnections:[{id:'i',name:'App',state:'available'}]},'another_tool')]),[]);
});

test('verified empty discovery retains actual catalog choices without inventing an installation',async()=>{
 const {appDiscoverySnapshot}=await import('../src/lib/wren-app-status.ts');
 const snapshot=appDiscoverySnapshot([event({conversationConnections:[],publicSources:[{slug:'crossref',name:'Crossref',description:'Publication metadata',credential:'never-render'}]})]);
 assert.deepEqual(snapshot,{state:'checked',connections:[],sources:[{slug:'crossref',name:'Crossref',description:'Publication metadata'}]});
 assert.equal(appDiscoverySnapshot([]),null);
 assert.equal(appDiscoverySnapshot([event({},'canter_show_connections','running')]),null);
});
test('latest failure does not replay an earlier available connection',async()=>{
 const {appDiscoverySnapshot}=await import('../src/lib/wren-app-status.ts');
 const prior=event({conversationConnections:[{id:'i',name:'App',state:'available'}]});
 assert.deepEqual(appDiscoverySnapshot([prior,event({},'canter_show_connections','failed')]),{state:'unavailable',connections:[],sources:[]});
 assert.equal(appDiscoverySnapshot([event({conversationConnections:[null]})]).state,'unavailable');
});
test('historical catalog without slugs can be browsed but cannot fabricate a direct target',async()=>{
 const {appDiscoverySnapshot}=await import('../src/lib/wren-app-status.ts');
 const result=appDiscoverySnapshot([event({conversationConnections:[],publicSources:[{name:'Crossref',description:'Metadata',slug:'../../secrets'},{name:'Crossref',description:'duplicate'}]})]);
 assert.deepEqual(result.sources,[{name:'Crossref',description:'Metadata'}]);
});
