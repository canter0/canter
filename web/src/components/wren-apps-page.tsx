"use client";

import Link from "next/link";
import {useRouter,useSearchParams} from "next/navigation";
import {operatorDraftKey} from "@/lib/operator-draft-storage";
import {extensionTarget} from "@/lib/wren-knowledge";
import type {Conversation} from "@/lib/operator-api";
import {useWrenResource,workBase} from "@/lib/wren-work";
import type {Wren,WrenRoom} from "@/lib/wrens";
import {AppShell} from "./app-shell";
import {useWorkspace} from "./workspace-context";
import {WorkspaceIcon} from "./workspace-icon";
import {WrenExtensionsPanel} from "./wren-extensions-panel";
import {GlobalPluginMarketplace} from "./global-plugin-marketplace";
import styles from "./wren-apps.module.css";
import shell from "./workspace.module.css";
import {CustomizationEditor} from "./customization-editor";
const sections=[["plugins","Plugins"],["skills","Skills"],["hooks","Hooks"],["rules","Rules"]] as const;
function CustomizationIcon({kind}:{kind:string}) {
 const paths:Record<string,React.ReactNode>={
  plugins:<path d="M8 3h3a2 2 0 1 1 4 0h3v6a2 2 0 1 1 0 4v6H8v-4a2 2 0 1 1-4 0v-4H2V3h6Z" transform="translate(1 1) scale(.9)"/>,
  skills:<path d="m13 2-8 11h6l-1 9 9-12h-6l1-8Z"/>,
  hooks:<><path d="m7 6-5 6 5 6m10-12 5 6-5 6M14 4l-4 16"/></>,
  rules:<><path d="M6 3h8l4 4v14H6ZM14 3v5h4M9 12h6m-6 4h6"/></>,
 };
 return <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">{paths[kind]}</svg>;
}

export function WrenAppsPage({conversationId,installationId,catalogSlug,app,githubResult}:{conversationId?:string;installationId?:string;catalogSlug?:string;app?:string;githubResult?:string}){
 const {data}=useWorkspace(),router=useRouter(),query=useSearchParams(),workspaceId=data?.workspace.id??"";
 const wrensEnabled=!!data?.products?.some(product=>product.id==="wrens"&&product.enabled);
 const wrens=useWrenResource<{wrens:Wren[]}>(workspaceId&&wrensEnabled?`${workBase(workspaceId)}/wrens`:""),rooms=useWrenResource<{rooms:WrenRoom[]}>(workspaceId&&wrensEnabled?`${workBase(workspaceId)}/rooms`:""),conversations=useWrenResource<{conversations:Conversation[]}>(workspaceId?`${workBase(workspaceId)}/conversations`:"");
 const target=extensionTarget(conversationId,wrens.data?.wrens??[],rooms.data?.rooms??[],conversations.data?.conversations??[],data?.account.id);
 const back=target?.room?{name:target.room.name.replace(/^\s*#\s*/,""),href:`/app/wrens/rooms/${encodeURIComponent(target.room.id)}`}:target?.wren?{name:target.wren.name,href:`/app/wrens/${encodeURIComponent(target.wren.id)}`}:target?.scope.scope==="operator"&&conversationId?{name:target.label.replace(/^Chat ·\s*/,"")||"conversation",href:`/app/conversations/${encodeURIComponent(conversationId)}`}:null;
 function useInChat(prompt:string){
  if(!back||!conversationId)return;
  const key=operatorDraftKey(data?.account.id,workspaceId,conversationId);
  if(key)try{const existing=sessionStorage.getItem(key)??"";sessionStorage.setItem(key,existing.trim()?`${existing.trim()}\n\n${prompt}`:prompt);window.dispatchEvent(new Event("canter-draft"));}catch{/* Nonessential storage. */}
  router.push(back.href);
 }
 // Deep links (a chat, an installation, a source, an account) open the existing
 // management panel; the plain global view is the standalone Plugins browser.
 const management=!!conversationId||!!installationId||!!catalogSlug||!!app||!!githubResult;
 const requested=query.get("view"),section=management?"plugins":sections.find(([key])=>key===requested)?.[0]??"plugins";
 const navigation=<nav className={shell.navigation} aria-label="Customizations">
  <div className={shell.sidebarLabel}>Customizations</div>
  {sections.map(([key,label])=><Link key={key} className={shell.navLink} href={key==="plugins"?"/app/customize":`/app/customize?view=${key}`} aria-label={label} aria-current={section===key?"page":undefined}><CustomizationIcon kind={key}/><span>{label}</span></Link>)}
 </nav>;
 return <AppShell active="Customize" pageTitle="Customize" sidebarContent={navigation}>
  {management?<div className={styles.appsPage}>
   {back?<Link className={styles.pageBack} href={back.href}><WorkspaceIcon name="right" style={{transform:"rotate(180deg)"}} width="14" height="14"/>Back to {back.name}</Link>:null}
   {workspaceId?<WrenExtensionsPanel workspaceId={workspaceId} conversationId={conversationId} installationId={installationId} catalogSlug={catalogSlug} app={app} onUse={back?useInChat:undefined} githubResult={githubResult}/>:null}
  </div>:section==="plugins"?<GlobalPluginMarketplace custom={requested==="custom"}/>:workspaceId?<CustomizationEditor key={`${workspaceId}:${section}`} workspaceId={workspaceId} kind={section==="rules"?"rule":section==="hooks"?"hook":"skill"}/>:null}
 </AppShell>;
}
