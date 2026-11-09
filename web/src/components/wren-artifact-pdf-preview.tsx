"use client";
import {useEffect,useState} from 'react';
import {artifactPDFContentPath,artifactPDFPreviewKey,validateArtifactPDFBytes} from '@/lib/artifact-pdf-preview';
import {fetchWrenBlob,workError,type WrenArtifactRecord} from '@/lib/wren-work';
import {useWorkspace} from './workspace-context';
import {PDFBytesPreview} from './operator-pdf-preview';
import styles from './operator-pdf-preview.module.css';
export function WrenArtifactPDFPreview({workspaceId,artifact}:{workspaceId:string;artifact:WrenArtifactRecord}){
 const {data}=useWorkspace(),key=artifactPDFPreviewKey(data?.account.id??'',workspaceId,artifact);
 const [state,setState]=useState<{key:string;bytes?:Uint8Array;error?:string}>({key:''});
 const path=artifactPDFContentPath(workspaceId,artifact),size=artifact.size,sha256=artifact.sha256;
 useEffect(()=>{
  if(!key)return;
  const controller=new AbortController();
  async function load(){try{const blob=await fetchWrenBlob(path,controller.signal);if(controller.signal.aborted)return;const bytes=await validateArtifactPDFBytes({size,sha256},await blob.arrayBuffer(),blob.type);if(!controller.signal.aborted)setState({key:key!,bytes});}catch(cause){if(!controller.signal.aborted)setState({key:key!,error:workError(cause,'Couldn’t load this saved PDF.')});}}
  void load();return()=>controller.abort();
 },[key,path,size,sha256]);
 if(!key)return <p className={styles.message}>This saved PDF revision is unavailable.</p>;
 if(state.key!==key||!state.bytes&&!state.error)return <p className={styles.loading} role="status">Loading saved PDF…</p>;
 if(state.error)return <p className={styles.message} role="alert">{state.error}</p>;
 return state.bytes?<PDFBytesPreview key={key} name={artifact.name} bytes={state.bytes}/>:null;
}
