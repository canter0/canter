"use client";

import {useState} from "react";
import {usePathname,useSearchParams} from "next/navigation";
import {canterFetch,CanterAPIError} from "@/lib/canter-api";
import {useWrenResource,workBase} from "@/lib/wren-work";
import type {KnowledgeTarget} from "@/lib/wren-knowledge";
import type {ExtensionInstallation,WrenAppAccount} from "@/lib/wren-extensions";
import {WrenMCPConnection,type MCPDiscovery} from "./wren-mcp-connection";
import {ProviderIcon} from "./provider-icon";
import styles from "./wren-apps.module.css";

const keepAccountsCurrent=()=>true;
export function WrenAppAccounts({workspaceId,target,onInstalled,bare}:{bare?:boolean;workspaceId:string;target:KnowledgeTarget|null;onInstalled:(value:ExtensionInstallation)=>void}){
 const pathname=usePathname(),query=useSearchParams(),base=`${workBase(workspaceId)}/app-accounts`;
 const nextQuery=new URLSearchParams(),conversation=query.get("conversation");
 if(conversation)nextQuery.set("conversation",conversation);
 nextQuery.set("app","linear");
 const next=`${pathname}?${nextQuery}`;
 const resource=useWrenResource<{accounts:WrenAppAccount[];enabled:boolean}>(base,3000,keepAccountsCurrent);
 const [busy,setBusy]=useState(""),[error,setError]=useState(""),[reauth,setReauth]=useState(false),[confirm,setConfirm]=useState("");
 const [review,setReview]=useState<{account:WrenAppAccount;discovery:MCPDiscovery}|null>(null);
 const [reconnecting,setReconnecting]=useState("");
 const [renaming,setRenaming]=useState(""),[label,setLabel]=useState("");
 const result=query.get("appConnection"),accounts=resource.data?.accounts??[];
 async function connect(account?:WrenAppAccount){if(busy)return;setBusy(account?.id??"connect");setError("");setReauth(false);try{const value=await canterFetch<{authorizationUrl:string}>(`${base}/connect`,{method:"POST",body:JSON.stringify({provider:"linear",next,...account?{reconnectId:account.id,expectedRevision:account.revision}:{}})});window.location.assign(value.authorizationUrl);}catch(cause){setError(cause instanceof Error?cause.message:"Couldn’t start the connection.");setReauth(cause instanceof CanterAPIError&&cause.status===401);setBusy("");}}
 async function choose(account:WrenAppAccount){setBusy(account.id);setError("");try{const discovery=await canterFetch<MCPDiscovery>(`${base}/${encodeURIComponent(account.id)}/discover`,{method:"POST"});setReview({account,discovery});}catch(cause){setError(cause instanceof Error?cause.message:"Couldn’t read the app’s tools.");resource.refresh();}finally{setBusy("");}}
 async function change(account:WrenAppAccount,remove:boolean){setBusy(account.id);setError("");try{await canterFetch(`${base}/${encodeURIComponent(account.id)}`,{method:remove?"DELETE":"PATCH",body:JSON.stringify({expectedRevision:account.revision,label})});setConfirm("");setRenaming("");resource.refresh();}catch(cause){setError(cause instanceof Error?cause.message:"Couldn’t update this connection.");resource.refresh();}finally{setBusy("");}}
 return <section className={bare?styles.bareAccount:styles.account} aria-label="Your Linear accounts">
  {bare?null:<header className={styles.accountHeading}><span className={styles.appIcon}><ProviderIcon provider="linear"/></span><div><h3>Linear</h3><p>Bring issues and project context into your conversation.</p></div><span className={styles.accountLabel}>Your account</span></header>}
  <p>Read-only access. Sign in once, then choose the tools available in each chat or room. Your teammates use their own accounts.</p>
  {result==="reconnected"?<p role="status">Account reconnected. Its existing chat connections can use it again.</p>:result==="changed"?<p role="status">This connection changed during sign-in. Review its current state before trying again.</p>:result==="connected"?<p role="status">Account connected. Choose its tools to use it here.</p>:result?<p role="status">{result==="cancelled"?"Sign-in cancelled. No account was connected.":"The account wasn’t connected. Try again from this conversation."}</p>:null}
  {resource.loading&&!resource.data?<p role="status">Checking your accounts…</p>:null}
  {accounts.map(account=><div className={styles.savedAccount} key={account.id}>
   <strong>{account.label}</strong><small>{account.reconnect?"Reconnect required":"Your saved connection"}</small>
   <div className={styles.actions}><button disabled={!!busy||account.reconnect} className={account.reconnect?undefined:styles.primary} onClick={()=>void choose(account)}>{busy===account.id?"Loading…":"Choose tools"}</button><button disabled={!!busy} className={account.reconnect?styles.primary:undefined} onClick={()=>{setReconnecting(account.id);setRenaming("");setConfirm("");setError("");}}>Reconnect</button><button disabled={!!busy} onClick={()=>{setRenaming(account.id);setReconnecting("");setConfirm("");setLabel(account.label);}}>Rename</button><button disabled={!!busy} onClick={()=>{setConfirm(account.id);setReconnecting("");setRenaming("");}}>Disconnect</button></div>
   {reconnecting===account.id?<div className={styles.disconnect}><p>Reconnect {account.label} using the Linear account you choose. This restores its existing access in your chats and rooms. Other people’s accounts stay separate.</p><div className={styles.actions}><button className={styles.primary} disabled={!!busy} onClick={()=>void connect(account)}>{busy===account.id?"Opening Linear…":"Continue to Linear"}</button><button disabled={!!busy} onClick={()=>setReconnecting("")}>Cancel</button></div></div>:null}
   {renaming===account.id?<form onSubmit={event=>{event.preventDefault();void change(account,false);}}><label>Account label<input autoFocus required maxLength={100} value={label} onChange={event=>setLabel(event.target.value)}/></label><div className={styles.actions}><button disabled={!!busy||!label.trim()}>Save label</button><button type="button" onClick={()=>setRenaming("")}>Cancel</button></div></form>:null}
   {confirm===account.id?<div className={styles.disconnect}><p>Disconnect {account.label} from this workspace? Your Wrens will lose this account’s access in every chat. Messages and other people’s connections stay available. You can reconnect later.</p><button disabled={!!busy} onClick={()=>void change(account,true)}>Disconnect this account</button><button disabled={!!busy} onClick={()=>setConfirm("")}>Keep connected</button></div>:null}
  </div>)}
  {resource.data?.enabled?<button disabled={!!busy} className={accounts.length?undefined:styles.primary} onClick={()=>void connect()}>{busy==="connect"?"Opening Linear…":accounts.length?"Connect another Linear account":"Connect Linear"}</button>:resource.data?<p>Account connections aren’t configured on this server.</p>:null}
  {reauth?<a href={`/sign-in?reauth=1&next=${encodeURIComponent(next)}`}>Sign in again and return here</a>:null}
  {resource.error||error?<p role="alert">{error||resource.error}</p>:null}
  {review?<div className={styles.accountReview}><button onClick={()=>setReview(null)}>Back to your accounts</button><WrenMCPConnection key={review.account.id} workspaceId={workspaceId} initialDiscovery={review.discovery} appAccount={review.account} target={target} onInstalled={onInstalled} onSaved={resource.refresh}/></div>:null}
 </section>;
}
