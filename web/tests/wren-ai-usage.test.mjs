import test from 'node:test';
import assert from 'node:assert/strict';
import {aiUsageMode,formatAIMicroUSD} from '../src/lib/wren-ai-usage.ts';
const snapshot=(changes={})=>({configured:true,enforced:true,currency:'USD',unit:'micro_usd',limitMicroUsd:250000,heldMicroUsd:1000,spentMicroUsd:112,remainingMicroUsd:248888,estimatedHoldsMicroUsd:1000,reservedEstimateMicroUsd:400,dispatchedEstimateMicroUsd:400,uncertainEstimateMicroUsd:200,closed:false,computeIncluded:false,...changes});
test('micro USD amounts retain actual subcent spending and never use compute cents',()=>{
 for(const [value,formatted] of [[0,'$0.00'],[1,'$0.000001'],[112,'$0.000112'],[5000,'$0.005'],[9999,'$0.009999'],[10000,'$0.01'],[250000,'$0.25'],[1234567,'$1.234567'],[1000000000,'$1,000.00']])assert.equal(formatAIMicroUSD(value),formatted);
 for(const invalid of [-1,NaN,Infinity,1.5,Number.MAX_SAFE_INTEGER+1])assert.equal(formatAIMicroUSD(invalid),'Unavailable');
});
test('only genuinely enforced available allowance is active; closed and exhausted requests are paused',()=>{
 assert.equal(aiUsageMode(snapshot()),'active');
 assert.equal(aiUsageMode(snapshot({closed:true,remainingMicroUsd:0})),'paused');
 assert.equal(aiUsageMode(snapshot({remainingMicroUsd:0})),'paused');
 // Availability comes from the authoritative server snapshot, not recomputed
 // from a guessed billing period or a future reset.
 assert.equal(aiUsageMode(snapshot({remainingMicroUsd:7})),'active');
});
test('unsupported unconfigured tracking stays hidden; configured unenforced usage is tracking only',()=>{
 assert.equal(aiUsageMode(snapshot({configured:false,enforced:false})),'hidden');
 assert.equal(aiUsageMode(snapshot({configured:false,enforced:true})),'unavailable');
 assert.equal(aiUsageMode(snapshot({enforced:false})),'tracking');
 assert.equal(aiUsageMode(snapshot({enforced:false,closed:true,remainingMicroUsd:0})),'tracking');
});
test('wrong units, combined compute and invalid monetary values cannot render as an AI allowance',()=>{
 for(const change of [{currency:'EUR'},{unit:'cents'},{computeIncluded:true},{spentMicroUsd:-1},{heldMicroUsd:Infinity},{remainingMicroUsd:1.5},{uncertainEstimateMicroUsd:NaN}])assert.equal(aiUsageMode(snapshot(change)),'unavailable');
});
