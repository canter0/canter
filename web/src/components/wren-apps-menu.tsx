"use client";

import Link from "next/link";
import {useRef,useState,type KeyboardEvent} from "react";
import type {GitHubConnection} from "@/lib/github-connection";
import {knowledgeQuery,knowledgeTarget} from "@/lib/wren-knowledge";
import {appMonogram,appProvider,appsPageHref,installedSource} from "@/lib/wren-apps";
import {addCatalogConnector,setInstallationEnabled} from "@/lib/wren-apps-api";
import {extensionInstallationLabel,type ConnectorCatalogEntry,type ExtensionInstallation,type WrenAppAccount} from "@/lib/wren-extensions";
import {notifyWorkUpdate,useWrenResource,workBase} from "@/lib/wren-work";
import type {Wren,WrenRoom} from "@/lib/wrens";
import {useWorkspace} from "./workspace-context";
import {ProviderIcon} from "./provider-icon";
import {WorkspaceIcon} from "./workspace-icon";
import shared from "./workspace.module.css";
import styles from "./wren-apps-menu.module.css";

export function useChatApps(workspaceId?:string,conversationId?:string){
 const base=workspaceId?workBase(workspaceId):"";
 const wrens=useWrenResource<{wrens:Wren[]}>(base?`${base}/wrens`:"");
 const rooms=useWrenResource<{rooms:WrenRoom[]}>(base?`${base}/rooms`:"");
 const target=knowledgeTarget(conversationId,wrens.data?.wrens??[],rooms.data?.rooms??[]);
 const installations=useWrenResource<{installations:ExtensionInstallation[]}>(base&&target?`${base}/wren-extensions/installations?${knowledgeQuery(target.scope)}`:"");
 const refresh=()=>{wrens.refresh();rooms.refresh();installations.refresh();};
 return {target,installations:installations.data?.installations,error:wrens.error||rooms.error||installations.error,loading:wrens.loading||rooms.loading||installations.loading,refresh};
}

const menuItems="a[href], button:not(:disabled)";
function moveFocus(event:KeyboardEvent<HTMLElement>){
 if(!["ArrowDown","ArrowUp","Home","End"].includes(event.key))return;
 const items=Array.from(event.currentTarget.querySelectorAll<HTMLElement>(menuItems)).filter(item=>item.getClientRects().length);
 if(!items.length)return;
 event.preventDefault();
 const current=items.indexOf(document.activeElement as HTMLElement);
 const next=event.key==="Home"?0:event.key==="End"?items.length-1:(current+(event.key==="ArrowDown"?1:-1)+items.length)%items.length;
 items[next]?.focus();
}

export function AppTile({name,origin,tools}:{name:string;origin?:string;tools?:{endpoint:string}[]}){
 const provider=appProvider({name,origin,tools});
 return provider?<ProviderIcon provider={provider}/>:<>{appMonogram(name)}</>;
}

export function WrenAppsMenu({workspaceId,conversationId,onClose}:{workspaceId:string;conversationId?:string;onClose:()=>void}){
 const {data}=useWorkspace();
 const base=`${workBase(workspaceId)}/wren-extensions`;
 const {target,installations,error,loading,refresh}=useChatApps(workspaceId,conversationId);
 const sources=useWrenResource<{connectors:ConnectorCatalogEntry[]}>(`${base}/catalog`);
 const github=useWrenResource<GitHubConnection>(`${workBase(workspaceId)}/github`);
 const accounts=useWrenResource<{accounts:WrenAppAccount[];enabled:boolean}>(`${workBase(workspaceId)}/app-accounts`);
 const [busy,setBusy]=useState(""),[problem,setProblem]=useState("");
 const attempt=useRef<{slug:string;id:string}|null>(null);
 const writer=!!data&&data.workspace.role!=="viewer",owner=data?.workspace.role==="owner";
 const room=target?.scope.scope==="room";
 const added=(installations??[]).filter(item=>!item.revokedAt);
 const available=(sources.data?.connectors??[]).filter(entry=>!installedSource(added,entry));
 const linearAccounts=accounts.data?.accounts.filter(account=>!account.reconnect)??[];
 const manage=(item:ExtensionInstallation)=>writer&&(item.ownerId===data?.account.id||owner);
 async function toggle(item:ExtensionInstallation){
  if(!target||busy)return;
  setBusy(item.id);setProblem("");
  try{await setInstallationEnabled(base,item,target.scope,!item.enabled);refresh();notifyWorkUpdate(workspaceId,conversationId);}
  catch(cause){setProblem(cause instanceof Error?cause.message:"Couldn’t update this app.");refresh();}
  finally{setBusy("");}
 }
 async function add(entry:ConnectorCatalogEntry){
  if(!target||busy||!writer)return;
  setBusy(entry.slug);setProblem("");
  try{
   if(attempt.current?.slug!==entry.slug)attempt.current={slug:entry.slug,id:`extinstall_${crypto.randomUUID()}`};
   await addCatalogConnector(base,target,entry,attempt.current.id);refresh();notifyWorkUpdate(workspaceId,conversationId);
  }catch(cause){setProblem(cause instanceof Error?cause.message:"Couldn’t add this app.");}finally{setBusy("");}
 }
 return <div className={`${shared.contextMenu} ${styles.menu}`} role="dialog" aria-label={room?"Apps for this room":"Apps for this chat"} onKeyDown={moveFocus}>
  <p className={styles.heading}>{room?"In this room":"In this chat"}</p>
  {loading&&!installations?<p className={styles.note} role="status">Loading apps…</p>:null}
  {error?<p className={styles.note} role="alert">{error} <button type="button" onClick={refresh}>Retry</button></p>:null}
  {installations&&!added.length?<p className={styles.note}>No apps yet.</p>:null}
  {added.map(item=>{
   const signIn=item.extension.manifest.auth==="bearer"&&!item.credentialConfigured;
   return <div className={styles.row} key={item.id}>
    <span className={styles.tile} aria-hidden="true"><AppTile name={item.extension.manifest.name} origin={item.extension.manifest.origin} tools={item.extension.manifest.tools}/></span>
    <span className={styles.meta}><strong>{item.extension.manifest.name}</strong><small>{extensionInstallationLabel(item)}</small></span>
    {manage(item)&&!item.extension.disabled?signIn?<Link className={styles.rowLink} href={appsPageHref(conversationId,{installation:item.id})} onClick={onClose}>Sign in</Link>:<button type="button" role="switch" aria-checked={item.enabled} aria-label={`${item.enabled?"Pause":"Resume"} ${item.extension.manifest.name} in this ${room?"room":"chat"}`} className={styles.switch} disabled={busy===item.id} onClick={()=>void toggle(item)}><span aria-hidden="true"/></button>:<small className={styles.status}>{extensionInstallationLabel(item)}</small>}
   </div>;
  })}
  {target?<><p className={styles.heading}>Add to this {room?"room":"chat"}</p>
  {writer?available.map(entry=><div className={styles.row} key={entry.slug}>
    <span className={styles.tile} aria-hidden="true"><AppTile name={entry.manifest.name} origin={entry.manifest.origin} tools={entry.manifest.tools}/></span>
    <span className={styles.meta}><strong>{entry.manifest.name}</strong><small>{entry.category} · No sign-in</small></span>
    <button type="button" className={styles.addButton} aria-label={`Add ${entry.manifest.name}`} title={`Add ${entry.manifest.name}`} disabled={busy===entry.slug} onClick={()=>void add(entry)}><WorkspaceIcon name="plus" width="15" height="15"/></button>
   </div>):null}
  {github.data?<div className={styles.row}>
   <span className={styles.tile} aria-hidden="true"><ProviderIcon provider="github"/></span>
   <span className={styles.meta}><strong>GitHub</strong><small>{github.data.connected?`Connected as ${github.data.login}`:github.data.reconnect?"Reconnect needed":"Your account"}</small></span>
   <Link className={styles.rowLink} href={appsPageHref(conversationId,{app:"github"})} onClick={onClose}>{github.data.connected?"Manage":"Connect"}</Link>
  </div>:null}
  {accounts.data&&(accounts.data.enabled||accounts.data.accounts.length)?<div className={styles.row}>
   <span className={styles.tile} aria-hidden="true"><ProviderIcon provider="linear"/></span>
   <span className={styles.meta}><strong>Linear</strong><small>{linearAccounts.length?`${linearAccounts.length} saved ${linearAccounts.length===1?"account":"accounts"}`:accounts.data.accounts.length?"Reconnect needed":"Your account"}</small></span>
   <Link className={styles.rowLink} href={appsPageHref(conversationId,{app:"linear"})} onClick={onClose}>{linearAccounts.length?"Choose tools":"Connect"}</Link>
  </div>:null}</>:null}
  {problem?<p className={styles.note} role="alert">{problem}</p>:null}
  <Link className={styles.manage} href={appsPageHref(conversationId)} onClick={onClose}><span>Manage apps</span><WorkspaceIcon name="right" width="14" height="14"/></Link>
 </div>;
}
