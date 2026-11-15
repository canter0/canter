import test from 'node:test';
import assert from 'node:assert/strict';
import {createHash} from 'node:crypto';
import {artifactPDFPreviewKey,artifactPDFContentPath,validateArtifactPDFBytes} from '../src/lib/artifact-pdf-preview.ts';
const bytes=new TextEncoder().encode('%PDF-1.7\nactual QA bytes');
const artifact={id:'artifact_real',workspaceId:'ws',conversationId:'private',mediaType:'application/pdf',revision:2,size:bytes.length,sha256:createHash('sha256').update(bytes).digest('hex')};
test('saved PDF preview scope includes account/workspace/conversation/revision/digest and never substitutes latest for pinned content',()=>{
 const key=artifactPDFPreviewKey('owner','ws',artifact);
 assert.ok(key);
 assert.notEqual(key,artifactPDFPreviewKey('other','ws',artifact));
 assert.notEqual(key,artifactPDFPreviewKey('owner','ws',{...artifact,conversationId:'room'}));
 assert.notEqual(key,artifactPDFPreviewKey('owner','ws',{...artifact,revision:1}));
 assert.notEqual(key,artifactPDFPreviewKey('owner','ws',{...artifact,sha256:'f'.repeat(64)}));
 assert.equal(artifactPDFPreviewKey('owner','foreign',artifact),null);
 assert.equal(artifactPDFPreviewKey('','ws',artifact),null);
 assert.equal(artifactPDFPreviewKey('owner','ws',{...artifact,revision:0}),null);
 assert.equal(artifactPDFContentPath('ws',artifact),'/workspaces/ws/wren-artifacts/artifact_real/content?revision=2');
});
test('authorized content must match actual revision bytes and media type before being rendered',async()=>{
 const result=await validateArtifactPDFBytes(artifact,bytes.buffer,'application/pdf');
 assert.deepEqual(result,bytes);
 await assert.rejects(validateArtifactPDFBytes(artifact,bytes.buffer,'text/html'));
 await assert.rejects(validateArtifactPDFBytes({...artifact,size:1},bytes.buffer,'application/pdf'));
 await assert.rejects(validateArtifactPDFBytes({...artifact,sha256:'0'.repeat(64)},bytes.buffer,'application/pdf'));
});
