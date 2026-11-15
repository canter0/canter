"use client";

import {useState} from "react";
import {canterFetch} from "@/lib/canter-api";
import {useWrenResource,workBase,notifyWorkUpdate} from "@/lib/wren-work";
import type {ExtensionCall} from "@/lib/wren-extensions";
import {useWorkspace} from "./workspace-context";
import {ConnectorResult} from "./wren-connector-query";
import styles from "./wren-apps.module.css";

type Review={id:string;conversationId:string;app:string;account:string;endpoint:string;tool:string;description:string;arguments:unknown;status:"pending"|"accepted"|"declined"|"expired"|"invalidated"|"private";expiresAt:string;receipt?:ExtensionCall};
const pending=(value:Review)=>value.status==="pending"||value.receipt?.status==="running";

/** Shared event, personal authorization: no arguments are embedded in room events. */
export function WrenActionReview({id}:{id:string}){
 const {data}=useWorkspace();
 const path=data?`${workBase(data.workspace.id)}/wren-extensions/reviews/${encodeURIComponent(id)}`:"";
 const resource=useWrenResource<Review>(path,3000,pending);
 const [busy,setBusy]=useState(false),[error,setError]=useState("");
 const value=resource.data;
 async function decide(decision:"approve"|"decline"){
  if(busy||!data)return;setBusy(true);setError("");
  try{await canterFetch(path,{method:"POST",body:JSON.stringify({decision})});resource.refresh();notifyWorkUpdate(data.workspace.id,value?.conversationId);}
  catch(cause){setError(cause instanceof Error?cause.message:"Couldn’t record your decision. Check the action before retrying.");resource.refresh();}
  finally{setBusy(false);}
 }
 return <section className={`${styles.chatCard} ${styles.actionReview}`} aria-label="App action review">
  <header><strong>{value?.app?`${value.app} · Action review`:"App action review"}</strong></header>
  {value?.status==="private"?<p>This action belongs to another participant. Only its requester can view the details or approve it.</p>:!value?resource.error?<><p>Couldn’t load this review. Only its requester can open the details, while they still have access to this conversation.</p><button type="button" onClick={resource.refresh}>Retry</button></>:<p role="status">Loading action…</p>:<>
   <p><strong>{value.tool.replaceAll("_"," ")}</strong> — {value.description}</p>
   {value.account?<p>Account: {value.account}</p>:null}
   <p>Destination: <span>{value.endpoint}</span></p>
   <details open={value.status==="pending"}><summary>Exact action details</summary><dl>{Object.entries(value.arguments as Record<string,unknown>).map(([key,entry])=><div key={key}><dt>{key.replaceAll("_"," ")}</dt><dd>{typeof entry==="string"?entry:JSON.stringify(entry,null,2)}</dd></div>)}</dl></details>
   {value.status==="pending"?<><p>This action can change data in the connected app. Nothing has run yet. Approve these details once, or ask your Wren to change them.</p><div className={styles.actions}><button type="button" className={styles.primary} disabled={busy||data?.workspace.role==="viewer"} onClick={()=>void decide("approve")}>{busy?"Saving decision…":"Approve and run"}</button><button type="button" disabled={busy||data?.workspace.role==="viewer"} onClick={()=>void decide("decline")}>Decline</button></div><small>Review expires {new Date(value.expiresAt).toLocaleString()}.</small></>:null}
   {value.status==="declined"?<p role="status">Declined. This action was not sent.</p>:null}
   {value.status==="invalidated"?<p role="status">This request or connection changed. Ask your Wren to prepare a fresh action.</p>:null}
   {value.status==="expired"?<p role="status">This review expired. Ask your Wren to prepare a fresh action.</p>:null}
   {value.receipt?<><p role="status">{value.receipt.status==="completed"?"The app confirmed this action completed.":value.receipt.status==="failed"?"The app reported a failure.":value.receipt.status==="unknown"?"The outcome is uncertain. Check the app before requesting another action.":"The action was accepted. Waiting for the app’s response…"}</p>{value.receipt.failure?<p>{value.receipt.failure}</p>:null}{value.receipt.result!==undefined?<details><summary>App response</summary><ConnectorResult result={value.receipt.result}/></details>:null}</>:null}
  </>}
  {error?<p role="alert">{error}</p>:null}
 </section>;
}
