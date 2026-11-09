import type {WrenArtifactRecord} from './wren-work';
export function artifactPDFPreviewKey(accountId:string,workspaceId:string,artifact:WrenArtifactRecord){
 if(!accountId||!workspaceId||artifact.workspaceId!==workspaceId||!artifact.conversationId||artifact.mediaType!=='application/pdf'||!Number.isSafeInteger(artifact.revision)||artifact.revision<1||!Number.isSafeInteger(artifact.size)||artifact.size<1||artifact.size>8*1024*1024||!/^\w[\w-]*$/.test(artifact.id)||!/^[a-fA-F0-9]{64}$/.test(artifact.sha256))return null;
 return JSON.stringify([accountId,workspaceId,artifact.conversationId,artifact.id,artifact.revision,artifact.sha256,artifact.size]);
}
export function artifactPDFContentPath(workspaceId:string,artifact:Pick<WrenArtifactRecord,'id'|'revision'>){return `/workspaces/${encodeURIComponent(workspaceId)}/wren-artifacts/${encodeURIComponent(artifact.id)}/content?revision=${artifact.revision}`;}
export async function validateArtifactPDFBytes(artifact:Pick<WrenArtifactRecord,'size'|'sha256'>,bytes:ArrayBuffer,mediaType:string){
 if(mediaType.split(';',1)[0].trim()!=='application/pdf'||bytes.byteLength!==artifact.size)throw new Error('The saved PDF bytes do not match this file revision.');
 const digest=Array.from(new Uint8Array(await crypto.subtle.digest('SHA-256',bytes)),value=>value.toString(16).padStart(2,'0')).join('');
 if(digest!==artifact.sha256.toLowerCase())throw new Error('The saved PDF does not match this file revision. Reload the file before previewing.');
 return new Uint8Array(bytes);
}
