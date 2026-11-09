"use client";
import {useEffect,useState} from "react";
import {WrenDialog} from "./wren-dialog";
import {WrenForm} from "./wrens-workspace";
import {RoomForm} from "./wren-rooms";
type Creation={kind:"wren"|"channel"|"group";section?:string};
export function WrenCreationModals(){
 const [creation,setCreation]=useState<Creation|null>(null);
 useEffect(()=>{const open=(event:Event)=>{const detail=(event as CustomEvent<Creation>).detail;if(detail&&["wren","channel","group"].includes(detail.kind))setCreation({kind:detail.kind,section:typeof detail.section==="string"?detail.section:undefined});};window.addEventListener("canter-create-conversation",open);return()=>window.removeEventListener("canter-create-conversation",open);},[]);
 return creation?<WrenDialog showTitle title={creation.kind==="wren"?"Create a Wren":creation.kind==="channel"?"Create a channel":"Create a group chat"} onClose={()=>setCreation(null)}>{creation.kind==="wren"?<WrenForm initialSection={creation.section} onSaved={()=>setCreation(null)}/>:<RoomForm compact={creation.kind==="channel"} kind={creation.kind} initialSection={creation.section} onSaved={()=>setCreation(null)}/>}</WrenDialog>:null;
}
