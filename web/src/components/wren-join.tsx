"use client";
import {selectWorkspace,openSelectedWorkspace} from "@/lib/workspace-selection";
import Link from "next/link";
import {useEffect,useState} from "react";
import {canterFetch,CanterAPIError,type Me} from "@/lib/canter-api";
import {clearAllOperatorAttachmentDrafts} from "@/lib/operator-attachment-draft";
import {clearOperatorTextDrafts} from "@/lib/operator-draft-storage";
import type {WrenInvite} from "@/lib/wren-multiplayer";
import styles from "./wren-members-panel.module.css";
export function WrenJoin({token}:{token:string}){
 const [me,setMe]=useState<Me|null>(null),[loaded,setLoaded]=useState(false),[busy,setBusy]=useState(false),[error,setError]=useState(""),[result,setResult]=useState<WrenInvite|null>(null);
 useEffect(()=>{const controller=new AbortController();void canterFetch<Me>("/me",{signal:controller.signal}).then(value=>{if(!controller.signal.aborted)setMe(value);}).catch(cause=>{if(!controller.signal.aborted&&!(cause instanceof CanterAPIError&&cause.status===401))setError("Couldn’t check your sign-in. Refresh to try again.");}).finally(()=>{if(!controller.signal.aborted)setLoaded(true);});return()=>controller.abort();},[]);
 async function changeAccount(){
  if(busy)return;setBusy(true);setError("");
  try{await canterFetch("/auth/signout",{method:"POST"});
   try{clearOperatorTextDrafts(sessionStorage);}catch{/* Storage can be unavailable. */}
   await clearAllOperatorAttachmentDrafts().catch(()=>{});
   window.location.replace(`/sign-in?next=${encodeURIComponent(next)}`);
  }catch{setError("Couldn’t sign out. You’re still on this invitation; try again.");setBusy(false);}
 }
 async function continueToWorkspace(workspaceId:string){if(busy)return;setBusy(true);setError("");try{await selectWorkspace(workspaceId);openSelectedWorkspace();}catch(cause){setError(cause instanceof Error?cause.message:"You joined, but the organization could not be selected. Try Continue again.");}finally{setBusy(false);}}
 async function decide(action:"accept"|"decline"){if(busy)return;setBusy(true);setError("");try{const invite=await canterFetch<WrenInvite>("/workspace-invites",{method:"POST",body:JSON.stringify({token,action})});setResult(invite);history.replaceState(null,"","/join");if(invite.state==="accepted")await selectWorkspace(invite.workspaceId);}catch(cause){setError(cause instanceof Error?cause.message:"Couldn’t respond to this invitation.");}finally{setBusy(false);}}
 const next=`/join?invite=${encodeURIComponent(token)}`;
 return <main className={`${styles.panel} ${styles.join}`}><Link className="wordmark" href="/">canter</Link><h1>Join an organization</h1>{result?<><p>{result.state==="accepted"?"You’ve joined the organization.":"Invitation declined."}</p>{result.state==="accepted"?<button disabled={busy} onClick={()=>void continueToWorkspace(result.workspaceId)}>{busy?"Opening organization…":"Continue to Wrens"}</button>:<Link href="/app/wrens">Continue to Canter</Link>}</>:!token?<p>This invitation link is incomplete.</p>:!loaded?<p role="status">Checking your sign-in…</p>:me?<><p>Signed in as {me.account.email}. This invitation must match your verified email.</p><p>Joining grants organization access to shared channels. Private groups require a separate invitation from someone in the group.</p><button disabled={busy} onClick={()=>void decide("accept")}>Accept invitation</button>{" "}<button disabled={busy} onClick={()=>void decide("decline")}>Decline</button><p><button type="button" className={styles.changeAccount} disabled={busy} onClick={()=>void changeAccount()}>Sign in with a different account</button></p></>:<><p>Sign in with the email address this invitation was sent to.</p><Link href={`/sign-in?next=${encodeURIComponent(next)}`}>Sign in to continue</Link></>}{error?<p role="alert">{error}</p>:null}</main>;
}
