import test from 'node:test';
import assert from 'node:assert/strict';
import {wrenProfile} from '../src/lib/wren-sharing.ts';
test('configuration review/copy allowlist cannot include private state or authority',()=>{
 const input={id:'canonical',conversationId:'publisher-private',name:'Scout',purpose:'Research',shape:'circle',palette:2,history:'secret',credentials:{key:'secret'},memory:'private',model:'selected',extensions:['private-grant']};
 assert.deepEqual(wrenProfile(input),{name:'Scout',purpose:'Research',role:'',section:'',shape:'circle',palette:2});
 assert.equal(JSON.stringify(wrenProfile(input)).includes('secret'),false);
});
