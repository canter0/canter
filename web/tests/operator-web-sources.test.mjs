import test from 'node:test';
import assert from 'node:assert/strict';
import { operatorWebSources } from '../src/lib/operator-web-sources.ts';
const source = {sourceId:'web_1',url:'https://exa.ai/docs',title:'Docs',retrievedAt:'2026-09-23T00:00:00Z',kind:'document'};
const event = (sources, overrides={}) => ({kind:'tool',data:{name:'canter_open_web',status:'completed',result:{sources},...overrides}});
test('source links require successful document evidence and deduplicate repeated reads',()=>{
 const events=[event([source]),event([source],{name:'canter_read_web'}),event([{...source,url:'https://example.com',kind:'preview'}]),event([source],{status:'failed'})];
 assert.equal(operatorWebSources(events).length,1);
 assert.equal(operatorWebSources(events)[0].url,source.url);
});
test('source links reject unsafe URLs and malformed historical payloads',()=>{
 const urls=['javascript:alert(1)','data:text/html,hello','https://user:secret@example.com','invalid'];
 assert.deepEqual(operatorWebSources([event(urls.map(url=>({...source,url}))),event([null,{}]),event([],{result:null}),event([source],{name:'canter_search_web'})]),[]);
});
