"use client";
import {useRef,useState,type FormEvent} from "react";
import {canterFetch,type Workspace} from "@/lib/canter-api";
import {selectWorkspace,openSelectedWorkspace} from "@/lib/workspace-selection";
import {WorkspaceIcon} from "./workspace-icon";
import {useDialog} from "./use-dialog";
import styles from "./agent-connection-dialog.module.css";
export function CreateOrganizationDialog({onClose}:{onClose:()=>void}){
 const dialog=useRef<HTMLDialogElement>(null);
 const [name,setName]=useState(""),[pending,setPending]=useState(false),[error,setError]=useState("");
 const requestID=useRef("");
 const dismiss=useDialog(dialog,"input");
 function close(){if(!pending)dismiss(onClose);}
 async function create(event:FormEvent){event.preventDefault();if(pending)return;setPending(true);setError("");
  if(!requestID.current)requestID.current=`wrk_${crypto.randomUUID()}`;
  try{const workspace=await canterFetch<Workspace>("/workspaces",{method:"POST",body:JSON.stringify({id:requestID.current,name})});await selectWorkspace(workspace.id);openSelectedWorkspace();}
  catch(cause){setError(cause instanceof Error?cause.message:"Couldn’t create this organization.");setPending(false);}}
 return <dialog ref={dialog} className={styles.dialog} aria-labelledby="create-organization-title" onCancel={event=>{event.preventDefault();close();}}>
  <button className={styles.close} aria-label="Close" disabled={pending} onClick={close}><WorkspaceIcon name="close" width="17" height="17"/></button>
  <div className={styles.icon}><WorkspaceIcon name="plus" width="23" height="23"/></div>
  <h2 id="create-organization-title">Create an organization</h2>
  <form onSubmit={create}>
   <p>Organizations keep their own people, agents, and work.</p>
   <label className={styles.field}>Name<input type="text" required maxLength={64} value={name} onChange={event=>setName(event.target.value)} placeholder="Acme team"/></label>
   <button className={styles.primary} disabled={pending||!name.trim()}>{pending?"Creating…":"Create organization"}</button>
  </form>
  {error?<p className={styles.error} role="alert">{error}</p>:null}
 </dialog>;
}
