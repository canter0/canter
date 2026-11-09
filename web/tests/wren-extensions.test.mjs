import test from 'node:test';
import assert from 'node:assert/strict';
import {parseExtensionArguments,parseExtensionManifest,extensionInstallationLabel,extensionCallLabel} from '../src/lib/wren-extensions.ts';
test('test input must be bounded object data; HTML and formulas stay literal data',()=>{
 assert.deepEqual(parseExtensionArguments('{"name":"<script>test</script>","cell":"=SUM(1,2)"}'),{name:'<script>test</script>',cell:'=SUM(1,2)'});
 for(const text of ['null','[]','1','"string"'])assert.throws(()=>parseExtensionArguments(text),/object/);
 assert.throws(()=>parseExtensionArguments(JSON.stringify({value:'x'.repeat(65536)})),/64 KiB/);
});
test('manifest import preserves actual schema fields and excludes undeclared authority properties',()=>{
 const manifest={name:'Example',description:'A real service configured by the developer',origin:'https://example.test',auth:'bearer',tools:[{name:'lookup',description:'Look up a record',endpoint:'https://example.test/lookup',impact:'read',inputSchema:{type:'object'},outputSchema:false}],credential:'must-not-import',hostExecution:true};
 const parsed=parseExtensionManifest(JSON.stringify(manifest));assert.deepEqual(parsed.events,[]);assert.equal(parsed.tools[0].outputSchema,false);assert.equal('credential' in parsed,false);assert.equal('hostExecution' in parsed,false);
 assert.throws(()=>parseExtensionManifest(JSON.stringify({...manifest,tools:[{...manifest.tools[0],impact:'automatic'}]})),/read\/write/);
});
test('stored setup and remote outcomes never imply unverified connection success',()=>{
 assert.equal(extensionInstallationLabel({enabled:true,extension:{disabled:false}}),'Enabled here');
 assert.equal(extensionInstallationLabel({enabled:true,extension:{disabled:true}}),'Disabled by publisher');
 assert.equal(extensionInstallationLabel({revokedAt:'now',enabled:true,extension:{disabled:false}}),'Revoked');
 assert.equal(extensionCallLabel({status:'unknown'}),'Outcome unknown');assert.equal(extensionCallLabel({status:'running'}),'Awaiting outcome');assert.equal(extensionCallLabel({status:'completed'}),'Completed');
});

test('GET transport survives export/import and never silently converts to POST or write access',()=>{
 const manifest={name:'Public search',description:'Find public records',origin:'https://api.example.test',auth:'none',tools:[{name:'search',description:'Search',endpoint:'https://api.example.test/search',method:'GET',impact:'read',inputSchema:{type:'object',properties:{q:{type:'string'}}}}]};
 assert.equal(parseExtensionManifest(JSON.stringify(manifest)).tools[0].method,'GET');
 for(const patch of [{method:'POST'},{impact:'write'}])assert.throws(()=>parseExtensionManifest(JSON.stringify({...manifest,tools:[{...manifest.tools[0],...patch}]})));
});

test("MCP imports retain remote tool identity and output schema",()=>{
 const tool={name:"tool_01",remoteName:"Lookup.Record",description:"Lookup",method:"MCP",endpoint:"https://example.com/mcp",impact:"read",inputSchema:{type:"object"},outputSchema:{type:"object",required:["result"]}};
 const parsed=parseExtensionManifest(JSON.stringify({name:"MCP",description:"Remote",origin:"https://example.com",auth:"bearer",tools:[tool],events:[]}));
 assert.deepEqual(parsed.tools[0],tool);
 assert.throws(()=>parseExtensionManifest(JSON.stringify({...parsed,tools:[{...tool,method:"GET"}]})));
});

test('version review identifies changed authority, endpoints, schemas, tools and events',async()=>{
 const {extensionVersionChanges}=await import('../src/lib/wren-extensions.ts');
 const tool={name:'lookup',description:'Find records',endpoint:'https://example.test/read',impact:'read',method:'GET',inputSchema:{type:'object',properties:{q:{type:'string'}}}};
 const before={name:'Lookup',description:'Records',origin:'https://example.test',auth:'none',tools:[tool,{...tool,name:'old_tool'}],events:['old_event']};
 const after={...before,auth:'bearer',tools:[{...tool,impact:'write',method:undefined,endpoint:'https://example.test/write',inputSchema:{type:'object',properties:{id:{type:'integer'}}},outputSchema:{type:'object'},description:'Change records'},{...tool,name:'new_tool'}],events:['new_event']};
 const diff=extensionVersionChanges(before,after);
 assert.equal(diff.authChanged,true);
 assert.deepEqual(diff.added.map(t=>t.name),['new_tool']); assert.deepEqual(diff.removed.map(t=>t.name),['old_tool']);
 assert.deepEqual(diff.changed[0].changes,['Read access → write access','Destination: https://example.test/read → https://example.test/write','Transport: GET → POST','Accepted inputs changed','Returned data schema changed','Tool instructions changed']);
 assert.deepEqual(diff.addedEvents,['new_event']);assert.deepEqual(diff.removedEvents,['old_event']);
});
test('version review ignores object key ordering but detects remote tool identity and descriptive-only changes',async()=>{
 const {extensionVersionChanges}=await import('../src/lib/wren-extensions.ts');
 const tool={name:'lookup',remoteName:'Lookup.Record',description:'Read',endpoint:'https://example.test/mcp',method:'MCP',impact:'read',inputSchema:{type:'object',properties:{a:{type:'string'},b:{type:'number'}}}};
 const before={name:'Lookup',description:'Records',origin:'https://example.test',auth:'none',tools:[tool],events:[]};
 const reordered={...before,description:'Updated description',tools:[{...tool,inputSchema:{properties:{b:{type:'number'},a:{type:'string'}},type:'object'}}]};
 const diff=extensionVersionChanges(before,reordered);assert.deepEqual(diff.changed,[]);assert.equal(diff.descriptionChanged,true);
 assert.deepEqual(extensionVersionChanges(before,{...before,tools:[{...tool,remoteName:'Write.Record'}]}).changed[0].changes,['Remote tool identity changed']);
});
