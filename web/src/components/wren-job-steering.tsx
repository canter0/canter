"use client";
import {useState} from "react";
import {canterFetch,CanterAPIError} from "@/lib/canter-api";
import {workBase,workError,type WrenJobRecord} from "@/lib/wren-work";
import {useWorkspace} from "./workspace-context";
import styles from "./wren-job-steering.module.css";

export function WrenJobSteering({workspaceId,job,refresh}:{workspaceId:string;job:WrenJobRecord;refresh:()=>void}) {
  const {data:workspace}=useWorkspace();
  const [draft,setDraft]=useState(""),[sending,setSending]=useState(false),[error,setError]=useState("");
  const [request,setRequest]=useState<{content:string;key:string}|null>(null);
  const canWrite=workspace?.workspace.role!=="viewer"&&!!workspace;
  async function send() {
    if(sending||!job.canSteer||!draft.trim())return;
    const input=request?.content===draft.trim()?request:{content:draft.trim(),key:crypto.randomUUID()};
    setRequest(input);setSending(true);setError("");
    try {await canterFetch(`${workBase(workspaceId)}/wren-jobs/${encodeURIComponent(job.id)}/steer`,{method:"POST",body:JSON.stringify({operationKey:input.key,content:input.content})});setDraft("");setRequest(null);refresh();}
    catch(cause){setError(cause instanceof CanterAPIError && cause.status===409 ? "This worker has finished or stopped. Your correction was not applied; it is kept here for a follow-up." : workError(cause,"Couldn’t forward this correction."));}
    finally{setSending(false);}
  }
  if(!job.canSteer&&!job.steering?.length&&!draft&&!error)return null;
  return <details className={styles.root}><summary>Guide work{job.steering?.some(input=>!input.appliedAt)?" · pending":""}</summary>
    {job.steering?.map(input=><div key={input.id} className={styles.receipt}><p>{input.content}</p><small>{input.appliedAt?"Received at a model checkpoint":"Waiting for the next model checkpoint"}</small></div>)}
    {canWrite&&(job.canSteer||draft)?<form onSubmit={event=>{event.preventDefault();void send();}}><textarea aria-label="Correction for this worker" placeholder="What should change?" value={draft} readOnly={sending} maxLength={2000} onChange={event=>setDraft(event.target.value)} rows={2}/><p className={styles.note}>{job.canSteer?"Applies at the next model checkpoint. Work already submitted keeps its own state.":"This worker has finished. This correction has not been applied."}</p>{job.canSteer?<button type="submit" disabled={sending||!draft.trim()}>{sending?"Forwarding…":"Forward correction"}</button>:null}</form>:null}
    {error?<p role="alert">{error}</p>:null}
  </details>;
}
