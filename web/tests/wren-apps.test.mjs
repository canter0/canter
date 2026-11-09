import test from 'node:test';
import assert from 'node:assert/strict';
import {appsPageHref} from '../src/lib/wren-apps.ts';

test('apps page links carry only the context they need',()=>{
 assert.equal(appsPageHref(),'/app/customize');
 assert.equal(appsPageHref('conv_1'),'/app/customize?conversation=conv_1');
 assert.equal(appsPageHref('conv_1',{installation:'extinstall_9'}),'/app/customize?conversation=conv_1&installation=extinstall_9');
 assert.equal(appsPageHref('conv_1',{source:'deepwiki'}),'/app/customize?conversation=conv_1&source=deepwiki');
 assert.equal(appsPageHref('conv_1',{app:'github'}),'/app/customize?conversation=conv_1&app=github');
 assert.equal(appsPageHref('conv_1',{app:'linear'}),'/app/customize?conversation=conv_1&app=linear');
 assert.equal(appsPageHref(undefined,{app:'linear'}),'/app/customize?app=linear');
 assert.equal(appsPageHref('a b&c',{source:'x/y'}),`/app/customize?${new URLSearchParams({conversation:'a b&c',source:'x/y'})}`);
});
