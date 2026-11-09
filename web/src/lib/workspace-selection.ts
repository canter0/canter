import type {Me,Workspace} from "./canter-api";
export const workspacePreferenceCookie="canter_workspace";
export function workspacePreference(accountId:string,workspaceId:string){return JSON.stringify([accountId,workspaceId]);}
export function workspaceFromPreference(me:Me,preference?:string):Workspace|undefined{
 if(preference){try{const value:unknown=JSON.parse(decodeURIComponent(preference));if(Array.isArray(value)&&value.length===2&&value[0]===me.account.id&&typeof value[1]==="string"){const selected=me.workspaces.find(workspace=>workspace.id===value[1]);if(selected)return selected;}}catch{/* Preferences never grant membership. Invalid/stale values use a current membership. */}}
 return me.workspaces[0];
}
export type WorkspaceSelection=Me&{workspace:Workspace};
export function workspaceDisplayName(name:string|undefined){return name==="default"?"Personal":name??"";}
export async function getSelectedWorkspace(signal?:AbortSignal):Promise<WorkspaceSelection>{
 const response=await fetch("/api/workspace-selection",{credentials:"include",cache:"no-store",signal});if(!response.ok){const {CanterAPIError}=await import("./canter-api");throw new CanterAPIError(response.status,response.status===401?"Sign in to continue.":"Your selected organization could not be loaded.");}return response.json();
}
export async function selectWorkspace(workspaceId:string){
 const response=await fetch("/api/workspace-selection",{method:"POST",headers:{"Content-Type":"application/json"},credentials:"include",body:JSON.stringify({workspaceId})});if(!response.ok){let message="Couldn’t select this organization.";try{const value=await response.json();if(typeof value.error==="string")message=value.error;}catch{/* Preserve the HTTP failure. */}throw new Error(message);}
 const selected:unknown=await response.json();
 if(selected&&typeof selected==="object"&&"accountId" in selected&&typeof selected.accountId==="string"){try{localStorage.setItem("canter-workspace-change",JSON.stringify({accountId:selected.accountId,workspaceId,at:Date.now()}));}catch{/* The preference cookie still works when local storage is unavailable. */}}
}

export function openSelectedWorkspace(){
 // A fresh document drops the prior workspace's in-memory conversations and
 // resource caches. Drafts remain under their existing workspace-scoped keys.
 window.location.assign(new URL("/app/wrens",window.location.origin).href);
}
