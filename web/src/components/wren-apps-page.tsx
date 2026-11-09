"use client";

import Link from "next/link";
import {useRouter} from "next/navigation";
import {operatorDraftKey} from "@/lib/operator-draft-storage";
import {knowledgeTarget} from "@/lib/wren-knowledge";
import {useWrenResource,workBase} from "@/lib/wren-work";
import type {Wren,WrenRoom} from "@/lib/wrens";
import {AppShell} from "./app-shell";
import {useWorkspace} from "./workspace-context";
import {WorkspaceIcon} from "./workspace-icon";
import {WrenExtensionsPanel} from "./wren-extensions-panel";
import styles from "./wren-apps.module.css";

export function WrenAppsPage({conversationId,installationId,catalogSlug,app,githubResult}:{conversationId?:string;installationId?:string;catalogSlug?:string;app?:string;githubResult?:string}){
 const {data}=useWorkspace(),router=useRouter(),workspaceId=data?.workspace.id??"";
 const wrens=useWrenResource<{wrens:Wren[]}>(workspaceId?`${workBase(workspaceId)}/wrens`:""),rooms=useWrenResource<{rooms:WrenRoom[]}>(workspaceId?`${workBase(workspaceId)}/rooms`:"");
 const target=knowledgeTarget(conversationId,wrens.data?.wrens??[],rooms.data?.rooms??[]);
 const back=target?.room?{name:target.room.name.replace(/^\s*#\s*/,""),href:`/app/wrens/rooms/${encodeURIComponent(target.room.id)}`}:target?.wren?{name:target.wren.name,href:`/app/wrens/${encodeURIComponent(target.wren.id)}`}:null;
 function useInChat(prompt:string){
  if(!back||!conversationId)return;
  const key=operatorDraftKey(data?.account.id,workspaceId,conversationId);
  if(key)try{const existing=sessionStorage.getItem(key)??"";sessionStorage.setItem(key,existing.trim()?`${existing.trim()}\n\n${prompt}`:prompt);window.dispatchEvent(new Event("canter-draft"));}catch{/* Nonessential storage. */}
  router.push(back.href);
 }
 return <AppShell active="Customize" pageTitle="Customize">
  <div className={styles.appsPage}>
   {back?<Link className={styles.pageBack} href={back.href}><WorkspaceIcon name="right" style={{transform:"rotate(180deg)"}} width="14" height="14"/>Back to {back.name}</Link>:null}
   {workspaceId?<WrenExtensionsPanel workspaceId={workspaceId} conversationId={conversationId} installationId={installationId} catalogSlug={catalogSlug} app={app} onUse={back?useInChat:undefined} githubResult={githubResult}/>:null}
  </div>
 </AppShell>;
}
