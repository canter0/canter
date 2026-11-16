"use client";
import { useCallback, useEffect, useId, useState, type FormEvent } from "react";
import { useRouter } from "next/navigation";
import { rememberWren } from "@/lib/wren-navigation";
import { WrenLifecycleControls } from "./wren-lifecycle-controls";
import { canterFetch } from "@/lib/canter-api";
import { useWrenResource } from "@/lib/wren-work";
import type { WrenPublication } from "@/lib/wren-sharing";
import type { WrenMember } from "@/lib/wren-multiplayer";
import type { WrenRoom } from "@/lib/wrens";
import type { ConversationDetail } from "@/lib/operator-api";
import { useWrens } from "./wrens-context";
import { useWorkspace } from "./workspace-context";
import { WrenSidebar,ConversationSettingsNav } from "./wrens-workspace";
import { WorkspaceIcon } from "./workspace-icon";
import { WrenMark } from "./wren-mark";
import { WrenDialog } from "./wren-dialog";
import { CanterSelect } from "./canter-select";
import { WrenAvatar } from "./wren-avatar";
import { AppShell } from "./app-shell";
import { OperatorWorkspace } from "./operator-workspace";
import styles from "./wrens-workspace.module.css";

export function RoomAvatars({room}:{room:WrenRoom}) {return <span className={styles.roomAvatar} data-empty={!room.wrens.length}>{room.wrens.length?room.wrens.slice(0,4).map(wren=><WrenAvatar key={wren.id} shape={wren.shape} palette={wren.palette} size={19}/>):room.kind==="channel"?<span className={styles.roomHash}>#</span>:<WorkspaceIcon name="message" width="21" height="21"/>}</span>;}

export function NewRoom({kind,section}:{kind:"channel"|"group";section?:string}) {
 const router=useRouter();
 return <AppShell active="Wrens" agentView sidebarContent={<WrenSidebar/>} pageTitle="Wrens" pageIcon={<WrenMark/>}><WrenDialog showTitle title={kind==="channel"?"Create a channel":"Create a group chat"} onClose={()=>router.replace("/app/wrens")}><RoomForm compact={kind==="channel"} kind={kind} initialSection={section} onSaved={()=>{}}/></WrenDialog></AppShell>;
}

export function RoomConversation({room:initial,initialDetail,initialLoadedAt}:{room:WrenRoom;initialDetail:ConversationDetail|null;initialLoadedAt?:number}) {
 const {data}=useWorkspace();const accountID=data?.account.id,workspaceID=data?.workspace.id;const {rooms,people,loading,refresh}=useWrens();const room=rooms.find(item=>item.id===initial.id)??initial;
 const [editing,setEditing]=useState(false);
 const [settingsMounted,setSettingsMounted]=useState(false);
 const dismiss=useCallback(()=>setEditing(false),[]);
 const close=useCallback(()=>{setEditing(false);requestAnimationFrame(() => { const target = document.querySelector<HTMLElement>('[data-workspace-surface][aria-hidden="false"] [role="tab"][aria-selected="true"]') ?? document.querySelector<HTMLElement>("[data-page-title-action]"); target?.focus(); });},[]);
 const toggle=useCallback(()=>{setSettingsMounted(true);setEditing(v=>!v);},[]);
 useEffect(()=>{const open=(event:Event)=>{if((event as CustomEvent<{conversationId:string}>).detail?.conversationId===room.conversationId){setSettingsMounted(true);setEditing(true);}};window.addEventListener("canter-conversation-settings",open);return()=>window.removeEventListener("canter-conversation-settings",open);},[room.conversationId]);
 useEffect(()=>{if(accountID && workspaceID) rememberWren(accountID,workspaceID,room.id);},[accountID,workspaceID,room.id]);
 return <><OperatorWorkspace key={room.id} room={room} roomPeopleCount={loading?undefined:people.filter(person=>room.kind==="channel"||person.id===room.ownerId||room.memberIds.includes(person.id)).length} onConversationUpdate={refresh} id={room.conversationId} initialDetail={initialDetail} initialLoadedAt={initialLoadedAt} sidebarContent={<WrenSidebar />} onEditWren={toggle} inspectorOpen={editing} onCloseInspector={dismiss} inspector={settingsMounted ? <RoomForm key={room.id} room={room} kind={room.kind} onSaved={close}/> : undefined} /></>;
}
export function RoomForm({room,kind,onSaved,initialSection,compact=false}:{room?:WrenRoom;kind:"channel"|"group";onSaved?:()=>void;initialSection?:string;compact?:boolean}) {
 const {wrens,rooms,people,saveRoom,loading,refresh}=useWrens();const {data}=useWorkspace();const router=useRouter();const sectionID=useId(),purposeID=useId();
 const [draft,setDraft]=useState<WrenRoom>(()=>room??{id:`room_${crypto.randomUUID()}`,conversationId:"",ownerId:"",kind,name:"",purpose:"",section:initialSection??"",wrenIds:[],memberIds:[],wrens:[],status:"idle",updatedAt:"",lastMessage:""});
 const [saving,setSaving]=useState(false);const [error,setError]=useState("");
 const editable=(!room||room.ownerId===data?.account.id)&&data?.workspace.role!=="viewer";
 const members=useWrenResource<{members:WrenMember[]}>(room&&data?`/workspaces/${encodeURIComponent(data.workspace.id)}/wren-members`:"");
 const [transferTo,setTransferTo]=useState("");const [transferReview,setTransferReview]=useState(false);
 const canTransfer=!!room&&(room.ownerId===data?.account.id||room.canRecoverOwnership===true)&&data?.workspace.role!=="viewer";
 const transferCandidates=members.data?.members.filter(member=>member.accountId!==room?.ownerId&&member.role!=="viewer"&&(kind==="channel"||room?.memberIds.includes(member.accountId)))??[];
 async function transfer(){if(!room||!data||!transferTo||saving)return;setSaving(true);setError("");try{await canterFetch(`/workspaces/${encodeURIComponent(data.workspace.id)}/rooms/${encodeURIComponent(room.id)}/owner`,{method:"POST",body:JSON.stringify({accountId:transferTo})});setTransferReview(false);refresh();onSaved?.();router.refresh();}catch(cause){setError(cause instanceof Error?cause.message:"Couldn’t transfer this room.");}finally{setSaving(false);}}
 const publications=useWrenResource<{publications:WrenPublication[]}>(data?`/workspaces/${encodeURIComponent(data.workspace.id)}/wren-sharing`:"");
 const publishedChoices=publications.data?.publications.filter(item=>item.published&&!item.archived).map(item=>({id:item.wrenId,...item.profile}))??[];
 const choices=[...new Map([...publishedChoices,...wrens,...(room?.wrens??[])].map(wren=>[wren.id,wren])).values()];
 const sections=[...new Set([...wrens,...rooms].map(item=>item.section).filter(Boolean))];
 async function submit(event:FormEvent){event.preventDefault();if(saving||!editable)return;setSaving(true);setError("");try{const saved=await saveRoom(draft,!room);if(room){setSaving(false);onSaved?.();router.refresh();}else {onSaved?.();router.push(`/app/wrens/rooms/${saved.id}`);}}catch(cause){setError(cause instanceof Error?cause.message:"Couldn’t save this room.");setSaving(false);}}
 const extraFields=<> <label>What’s this room for?<textarea id={purposeID} rows={3} maxLength={3000} value={draft.purpose} placeholder="Give your team a shared direction." onChange={event=>setDraft({...draft,purpose:event.target.value})}/></label>
 <div className={styles.participantLabel}>Wrens · optional <span>Add up to four now, or choose them later in settings.</span></div>
 <div className={styles.participants}>{choices.map(wren=><label key={wren.id} className={styles.participant}><WrenAvatar shape={wren.shape} palette={wren.palette} size={29}/><span>{wren.name}{wren.role?<small>{wren.role}</small>:null}</span><input type="checkbox" aria-label={`Include ${wren.name}`} checked={draft.wrenIds.includes(wren.id)} disabled={!draft.wrenIds.includes(wren.id)&&draft.wrenIds.length>=4} onChange={event=>setDraft({...draft,wrenIds:event.target.checked?[...draft.wrenIds,wren.id]:draft.wrenIds.filter(id=>id!==wren.id)})}/></label>)}</div>
 {!choices.length?<p className={styles.roomNote}>{loading?"Loading Wrens…":<>You can start with people and add a Wren later.</>}</p>:null}
 {kind==="group"?<><div className={styles.participantLabel}>People</div><p className={styles.roomNote}>You’re included. Only invited workspace members can open this group.</p><div className={styles.participants}>{people.filter(person=>person.id!==data?.account.id).map(person=><label key={person.id} className={styles.participant}><span>{person.name}</span><input type="checkbox" aria-label={`Invite ${person.name}`} checked={draft.memberIds.includes(person.id)} onChange={event=>setDraft({...draft,memberIds:event.target.checked?[...draft.memberIds,person.id]:draft.memberIds.filter(id=>id!==person.id)})}/></label>)}</div></>:<p className={styles.roomNote}>Everyone in this workspace can read and contribute. Each Wren’s direct messages stay private.</p>}
 <details className={styles.organization}><summary>Organize</summary><label>Section<input list={sectionID} maxLength={48} value={draft.section} placeholder={kind==="channel"?"Channels":"Group chats"} onChange={event=>setDraft({...draft,section:event.target.value})}/></label><datalist id={sectionID}>{sections.map(section=><option key={section} value={section}/>)}</datalist></details></>;
 return <form className={styles.form} data-inspector={!!room} onSubmit={submit} aria-label={room?`${room.name} settings`:kind==="channel"?"Create a channel":"Create a group chat"}>
 {room?<><header className={styles.settingsHeading}><RoomAvatars room={room}/><div><h2>{room.name} settings</h2><p>{kind==="channel"?"Channel configuration":"Group configuration"}</p></div></header><ConversationSettingsNav name={room.name} destination={`/app/wrens/rooms/${encodeURIComponent(room.id)}`} purposeId={purposeID} onNavigate={onSaved}/></>:null}
 {!room&&!compact?<div className={styles.roomIntro}>{room?<RoomAvatars room={room}/>:<span className={styles.hash}>{kind==="channel"?"#":"＋"}</span>}<p>{kind==="channel"?"A shared place for your workspace to work with Wrens.":"A conversation for the people and Wrens you choose."}</p></div>:null}
 <fieldset disabled={saving||!editable}>
 <label>Name<input name="name" required maxLength={48} value={draft.name} placeholder={kind==="channel"?"e.g. product":"Name this conversation"} onChange={event=>setDraft({...draft,name:event.target.value})}/></label>
 {compact?<details className={styles.creationOptions}><summary>More options</summary>{extraFields}</details>:extraFields}
 </fieldset>
 {!editable?<p className={styles.roomNote}>The current room owner manages its participants.</p>:<footer><button className={styles.create} disabled={saving||!draft.name.trim()}>{saving?"Saving…":room?"Save changes":kind==="channel"?"Create channel":"Start group chat"}</button></footer>}
 {canTransfer?<details className={styles.organization}><summary>More settings</summary><p className={styles.roomNote}>{room?.canRecoverOwnership?"The previous owner no longer has operator access. Choose an existing member to recover management of this room.":"Transfer ownership to an existing room member. The new owner manages this conversation and its computer; its audience stays the same."}</p>{members.error?<p role="alert">{members.error}</p>:null}{transferCandidates.length?<><label>New owner<CanterSelect ariaLabel="New room owner" value={transferTo} onChange={setTransferTo} options={[{value:"",label:"Choose a room member"},...transferCandidates.map(member=>({value:member.accountId,label:member.email}))]} disabled={saving}/></label><button type="button" className={styles.transferSecondary} disabled={saving||!transferTo} onClick={()=>{setError("");setTransferReview(true);}}>{room?.canRecoverOwnership?"Review recovery…":"Review transfer…"}</button></>:<p className={styles.roomNote}>{members.loading?"Loading room members…":"Another room member with workspace operator access is needed to transfer ownership."}</p>}</details>:null}
 {transferReview&&canTransfer?<WrenDialog title={room?.canRecoverOwnership?"Recover room ownership":"Transfer room ownership"} showTitle onClose={()=>setTransferReview(false)}><p className={styles.roomNote}>Make {transferCandidates.find(member=>member.accountId===transferTo)?.email??"this member"} the owner of {room?.name}? They will manage settings, participants, the computer and saved workspaces. Existing messages and files stay in this conversation. Personal credentials and tool permissions are not copied.</p><p className={styles.roomNote}>Finish active computer work and saving or restoring before transferring. You will remain a room member.</p>{error?<p role="alert" className={styles.error}>{error}</p>:null}<div className={styles.transferActions}><button type="button" className={styles.create} disabled={saving||!transferTo} onClick={()=>void transfer()}>{saving?"Transferring…":"Transfer ownership"}</button><button type="button" className={styles.transferSecondary} disabled={saving} onClick={()=>setTransferReview(false)}>Cancel</button></div></WrenDialog>:null}
 {room&&data?<WrenLifecycleControls workspaceId={data.workspace.id} conversationId={room.conversationId} destination={`/app/wrens/rooms/${encodeURIComponent(room.id)}`}/>:null}
 {error?<p role="alert" className={styles.error}>{error}</p>:null}
 </form>;
}
