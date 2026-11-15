"use client";
import {useEffect,type RefObject} from "react";
import {canterFetch} from "@/lib/canter-api";
import {canMarkWrenRead} from "@/lib/wren-multiplayer";
export function useWrenRead({workspaceId,conversationId,sequence,covered,transcript,onRead}:{workspaceId?:string;conversationId?:string;sequence:number;covered:boolean;transcript:RefObject<HTMLDivElement|null>;onRead?:()=>void}) {
 useEffect(()=>{
  if(!workspaceId||!conversationId||!sequence)return;
  const controller=new AbortController();let busy=false,done=false;
  const mark=async()=>{const element=transcript.current;if(done||busy||!element||!canMarkWrenRead({visible:document.visibilityState==="visible",focused:document.hasFocus(),covered,gap:element.scrollHeight-element.scrollTop-element.clientHeight,sequence}))return;busy=true;
   try{await canterFetch(`/workspaces/${encodeURIComponent(workspaceId)}/wren-read`,{method:"POST",body:JSON.stringify({conversationId,sequence}),signal:controller.signal});if(!controller.signal.aborted){done=true;onRead?.();}}
   catch{/* Do not change the persisted unread badge unless the cursor was accepted. */}finally{busy=false;}
  };
  const element=transcript.current;const timer=window.setTimeout(()=>void mark(),700);
  const changed=()=>void mark();element?.addEventListener("scroll",changed);window.addEventListener("focus",changed);document.addEventListener("visibilitychange",changed);
  return()=>{controller.abort();clearTimeout(timer);element?.removeEventListener("scroll",changed);window.removeEventListener("focus",changed);document.removeEventListener("visibilitychange",changed);};
 },[workspaceId,conversationId,sequence,covered,transcript,onRead]);
}
