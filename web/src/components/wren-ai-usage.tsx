"use client";
import { useEffect, useState } from "react";
import { canterFetch, CanterAPIError } from "@/lib/canter-api";
import { aiUsageMode, formatAIMicroUSD, type WrenAIUsageSnapshot } from "@/lib/wren-ai-usage";
import { useWorkspace } from "./workspace-context";
import styles from "./wren-ai-usage.module.css";
type Snapshot = {scope:string;status:"loading"|"ready"|"hidden"|"error";data?:WrenAIUsageSnapshot};
export function WrenAIUsageSection({workspaceId:scopedWorkspaceId}:{workspaceId?:string}={}){
 const {data:workspace}=useWorkspace();
 const accountId=workspace?.account.id,workspaceId=scopedWorkspaceId??workspace?.workspace.id;
 const scope=accountId&&workspaceId?`${accountId}:${workspaceId}`:"";
 const [snapshot,setSnapshot]=useState<Snapshot>({scope:"",status:"loading"}),[revision,setRevision]=useState(0);
 useEffect(()=>{
  if(!scope||!workspaceId)return;
  const controller=new AbortController();let pending=false;
  async function load(){
   if(pending||controller.signal.aborted)return;pending=true;
   try{const value=await canterFetch<WrenAIUsageSnapshot>(`/workspaces/${encodeURIComponent(workspaceId!)}/ai-usage`,{signal:controller.signal});if(!controller.signal.aborted)setSnapshot({scope,status:"ready",data:value});}
   catch(cause){if(!controller.signal.aborted)setSnapshot({scope,status:cause instanceof CanterAPIError&&cause.status===404?"hidden":"error"});}
   finally{pending=false;}
  }
  const visible=()=>{if(document.visibilityState==="visible")void load();};
  void load();window.addEventListener("focus",visible);document.addEventListener("visibilitychange",visible);
  return()=>{controller.abort();window.removeEventListener("focus",visible);document.removeEventListener("visibilitychange",visible);};
 },[scope,workspaceId,revision]);
 if(!scope)return null;
 const current=snapshot.scope===scope?snapshot:undefined;
 if(current?.status==="hidden")return null;
 const usage=current?.data,mode=usage?aiUsageMode(usage):"unavailable";
 if(mode==="hidden")return null;
 const retry=()=>setRevision(value=>value+1);
 const status=mode==="tracking"?"Tracking only":mode==="paused"?"Paused":mode==="active"?"Shared allowance":"Unavailable";
 return <section className={styles.section} aria-label="AI usage">
  <header><h2>AI usage</h2>{current?.status==="ready"?<span>{status}</span>:null}</header>
  {!current||current.status==="loading"?<p className={styles.note} role="status">Loading AI usage…</p>:current.status==="error"?<p className={styles.note} role="status">Couldn’t load AI usage. <button type="button" onClick={retry}>Retry</button></p>:mode==="unavailable"?<p className={styles.note}>AI allowance information is unavailable.{usage?.enforced&&!usage.configured?" This workspace does not have a configured AI allowance.":""} Compute is billed separately.</p>:usage?<>
   <div className={styles.card}><dl className={styles.metrics}>
    <div><dt>Settled spending</dt><dd>{formatAIMicroUSD(usage.spentMicroUsd)}</dd></div>
    <div><dt>Estimated holds</dt><dd>{formatAIMicroUSD(usage.estimatedHoldsMicroUsd)}</dd></div>
    {mode!=="tracking"?<div><dt>Available</dt><dd>{formatAIMicroUSD(usage.remainingMicroUsd)}</dd></div>:null}
   </dl></div>
   <p className={styles.note}>USD · AI usage is separate from compute charges. {mode==="tracking"?"Usage is tracked; an AI allowance is not enforced.":`Configured AI allowance: ${formatAIMicroUSD(usage.limitMicroUsd)}.`}</p>
   {mode==="paused"?<p className={styles.note}>{usage.closed?"AI requests are paused for this workspace.":"AI requests are paused because the allowance is exhausted or fully held by unsettled requests."}</p>:null}
   {usage.estimatedHoldsMicroUsd>0?<p className={styles.note}>Holds are estimates for requests that have not settled. They are separate from settled spending.</p>:null}
   {usage.uncertainEstimateMicroUsd>0?<p className={styles.note}>{formatAIMicroUSD(usage.uncertainEstimateMicroUsd)} of the estimated holds remains reserved while the provider outcome is unconfirmed. It is not settled spending.</p>:null}
   <button type="button" className={styles.refresh} onClick={retry}>Refresh AI usage</button>
  </>:null}
 </section>;
}
