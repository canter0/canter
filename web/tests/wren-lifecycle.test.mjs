import test from 'node:test';
import assert from 'node:assert/strict';
import {lifecycleVisible} from '../src/lib/wren-lifecycle.ts';
test('personal hiding and archive retain separate recoverable conversation views',()=>{
 const active={hidden:false,archived:false},hidden={hidden:true,archived:false},archived={hidden:false,archived:true};
 assert.equal(lifecycleVisible(active,'active'),true);assert.equal(lifecycleVisible(hidden,'active'),false);assert.equal(lifecycleVisible(archived,'active'),false);
 assert.equal(lifecycleVisible(hidden,'hidden'),true);assert.equal(lifecycleVisible(archived,'archived'),true);
 for(const state of [active,hidden,archived])assert.equal(lifecycleVisible(state,'all'),true);
 assert.equal(lifecycleVisible(active,'hidden'),false);
});
