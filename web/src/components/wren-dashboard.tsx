"use client";
import Link from "next/link";
import { useState } from "react";
import { conversationHref, conversationState, plainPreview } from "@/lib/wren-presentation";
import { lifecycleVisible } from "@/lib/wren-lifecycle";
import { useWorkspace } from "./workspace-context";
import { useWrens } from "./wrens-context";
import { WrenAvatar } from "./wren-avatar";
import { WorkspaceIcon } from "./workspace-icon";
import styles from "./wren-dashboard.module.css";
function create(kind:"wren"|"channel"|"group") { window.dispatchEvent(new CustomEvent("canter-create-conversation",{detail:{kind}})); }
function dateLabel(value?:string) { if(!value)return "";const date=new Date(value);if(!Number.isFinite(date.getTime()))return "";return date.toDateString()===new Date().toDateString()?date.toLocaleTimeString([],{hour:"numeric",minute:"2-digit"}):date.toLocaleDateString([],{month:"short",day:"numeric"}); }
export function WrenDashboard() {
  const {data}=useWorkspace();const {wrens,rooms,lifecycle,loading,error,refresh}=useWrens();
  const [filter,setFilter]=useState("all"),[search,setSearch]=useState("");
  const visible=(conversationId:string)=>!!lifecycle.data&&lifecycleVisible(lifecycle.data.lifecycles.find(item=>item.conversationId===conversationId),"active");
  const team=wrens.filter(wren=>visible(wren.conversationId));
  const all=[...team,...rooms.filter(room=>visible(room.conversationId))].sort((a,b)=>Date.parse(b.lastMessageAt||b.updatedAt)-Date.parse(a.lastMessageAt||a.updatedAt));
  const shown=all.filter(item=>(filter==="unread"?!!item.unread:filter==="rooms"?"kind" in item:true)&&(!search||`${item.name} ${item.lastMessage} ${item.purpose}`.toLowerCase().includes(search.toLowerCase())));
  const pending=loading||lifecycle.loading,failed=error||lifecycle.error,canCreate=data?.workspace.role!=="viewer";
  return <div className={styles.home}>
    <header className={styles.heading}><h2>Conversations</h2>{canCreate?<button className={styles.primary} aria-label="New conversation" onClick={()=>create("group")}><WorkspaceIcon name="plus" width="16" height="16"/>New chat</button>:null}</header>
    <section className={styles.team} aria-label="Wrens"><div className={styles.teamHeading}><h3>Wrens</h3><Link href="/app/wrens/library">Browse library<WorkspaceIcon name="right" width="13" height="13"/></Link></div><div className={styles.people}>{team.map(wren=><Link key={wren.id} href={conversationHref(wren)} className={styles.person} title={wren.role||wren.purpose}><WrenAvatar shape={wren.shape} palette={wren.palette} status={wren.status} size={46} animated={false}/><span>{wren.name}</span></Link>)}{canCreate?<button className={styles.person} onClick={()=>create("wren")}><span className={styles.add}><WorkspaceIcon name="plus" width="20" height="20"/></span><span>New Wren</span></button>:null}</div></section>
    <div className={styles.listHeading}><div className={styles.filters} role="group" aria-label="Filter conversations">{[["all","All"],["unread","Unread"],["rooms","Rooms"]].map(([value,label])=><button type="button" key={value} aria-pressed={filter===value} onClick={()=>setFilter(value)}>{label}{value==="unread"&&all.some(item=>item.unread)?<span>{all.filter(item=>item.unread).length}</span>:null}</button>)}</div><label className={styles.search}><WorkspaceIcon name="search" width="16" height="16"/><input aria-label="Search" placeholder="Search" value={search} onChange={event=>setSearch(event.target.value)}/></label></div>
    {failed?<div className={styles.notice} role="alert">Couldn’t refresh conversations. <button onClick={()=>{refresh();lifecycle.refresh();}}>Retry</button></div>:null}
    {pending?<p className={styles.empty} role="status">Loading conversations…</p>:shown.length?<div className={styles.conversations}>{shown.map(item=>{const room="kind" in item,state=conversationState(item);return <Link className={styles.row} key={item.id} href={conversationHref(item)}><span className={styles.avatar}>{room?<span className={styles.roomAvatar}>{item.kind==="channel"?"#":<WorkspaceIcon name="message" width="23" height="23"/>}</span>:<WrenAvatar shape={item.shape} palette={item.palette} status={item.status} size={40} animated={false}/>}</span><span className={styles.message}><span className={styles.title}><strong>{item.name.replace(/^\s*#\s*/,"")}</strong><small>{room?item.kind==="channel"?"Channel":"Group chat":"Wren"}</small>{state.tone!=="quiet"?<span className={styles.status} data-tone={state.tone}>{state.label}</span>:null}</span><span className={styles.preview}>{plainPreview(item.lastMessage)||plainPreview(item.purpose)||"No messages yet"}</span></span><span className={styles.trailing}><time dateTime={item.lastMessageAt||item.updatedAt}>{dateLabel(item.lastMessageAt)}</time>{!!item.unread?<span className={styles.unread} aria-label={`${item.unread} unread messages`}>{item.unread>99?"99+":item.unread}</span>:null}</span><WorkspaceIcon className={styles.arrow} name="right" width="16" height="16"/></Link>;})}</div>:<section className={styles.empty}><h3>{search?"No matches":filter==="unread"?"No unread messages":filter==="rooms"?"No rooms yet":"No conversations yet"}</h3>{search?<p>Try another name or phrase.</p>:null}{canCreate&&!search&&filter!=="unread"?<button className={styles.primary} onClick={()=>create(filter==="rooms"?"group":"wren")}>{filter==="rooms"?"Create a room":"Create a Wren"}</button>:null}</section>}
  </div>;
}
