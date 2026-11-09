import test from 'node:test';
import assert from 'node:assert/strict';
import {workspacePreference,workspaceFromPreference,workspaceDisplayName} from '../src/lib/workspace-selection.ts';
const me={account:{id:'person',email:'person@example.test'},workspaces:[{id:'personal',name:'Personal',role:'owner'},{id:'team',name:'Team',role:'viewer'}]};
test('a selected second workspace remains selected using its actual current membership role',()=>{
 const selected=workspaceFromPreference(me,workspacePreference('person','team'));
 assert.equal(selected.id,'team');assert.equal(selected.role,'viewer');
 assert.equal(workspaceFromPreference(me,encodeURIComponent(workspacePreference('person','team'))).id,'team');
});
test('stale, malformed, foreign-account and forged workspace preferences never grant membership',()=>{
 for(const preference of [undefined,'bad-json','%invalid',workspacePreference('other-person','team'),workspacePreference('person','foreign-workspace'),JSON.stringify(['person','team','extra'])])assert.equal(workspaceFromPreference(me,preference).id,'personal');
 assert.equal(workspaceFromPreference({...me,workspaces:[]},workspacePreference('person','team')),undefined);
 assert.equal(workspaceFromPreference({...me,workspaces:[me.workspaces[0]]},workspacePreference('person','team')).id,'personal');
});

test('a workspace named default displays as Personal while other names pass through',()=>{
 assert.equal(workspaceDisplayName('default'),'Personal');
 assert.equal(workspaceDisplayName('Team Org'),'Team Org');
 assert.equal(workspaceDisplayName('Defaults'),'Defaults');
 assert.equal(workspaceDisplayName(undefined),'');
});
