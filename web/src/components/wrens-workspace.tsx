"use client";
import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import { useCallback, useContext, useEffect, useId, useState, useRef, type FormEvent } from "react";
import { rememberWren } from "@/lib/wren-navigation";
import { moveMenuFocus } from "@/lib/interaction";
import { type Wren, wrenPalettes, wrenShapes } from "@/lib/wrens";
import type { ConversationDetail } from "@/lib/operator-api";
import { AppShell } from "./app-shell";
import { OperatorWorkspace } from "./operator-workspace";
import { useWorkspace } from "./workspace-context";
import { useWrens } from "./wrens-context";
import { MotionPresence } from "./motion-presence";
import { usePopover } from "./use-popover";
import { WrenDialog } from "./wren-dialog";
import { WrenAvatar } from "./wren-avatar";
import { WrenSharingControls } from "./wren-sharing-controls";
import { WrenLifecycleControls } from "./wren-lifecycle-controls";
import { lifecycleVisible } from "@/lib/wren-lifecycle";
import { WrenSearchResults } from "./wren-search-results";
import { WrenWorkNavigation } from "./wren-work-navigation";
import { WrenMark } from "./wren-mark";
import { WrenDashboard } from "./wren-dashboard";
import { conversationPreview } from "@/lib/wren-presentation";
import { WorkspaceIcon } from "./workspace-icon";
import styles from "./wrens-workspace.module.css";

function messageTime(value?: string) {
  if (!value) return "";
  const date = new Date(value), today = new Date();
  return date.toDateString() === today.toDateString() ? date.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" }) : date.toLocaleDateString([], { month: "short", day: "numeric" });
}
export function WrenSidebar() {
  const { wrens, rooms, loading, error, refresh, collapsedSections, toggleSection, lifecycle, sidebarSearch: search, setSidebarSearch: setSearch, sidebarView: listView, setSidebarView: setListView } = useWrens();
  const { data } = useWorkspace();
  const pathname = usePathname();
  const refreshLifecycle = lifecycle.refresh;
  const [optionsOpen, setOptionsOpen] = useState(false), options = useRef<HTMLDivElement>(null);
  const closeOptions = useCallback(() => setOptionsOpen(false), []);
  usePopover(optionsOpen, options, closeOptions);
  const prefix = useId(), shown = (conversationId: string) => !!lifecycle.data && lifecycleVisible(lifecycle.data?.lifecycles.find(item => item.conversationId === conversationId), listView);
  const visible = wrens.filter(wren => shown(wren.conversationId) && `${wren.name} ${wren.role ?? ""} ${wren.section ?? ""} ${wren.purpose}`.toLowerCase().includes(search.toLowerCase()));
  const visibleRooms = rooms.filter(room => shown(room.conversationId) && `${room.name} ${room.section} ${room.purpose}`.toLowerCase().includes(search.toLowerCase()));
  const roomSection = (room: typeof rooms[number]) => room.section || (room.kind === "channel" ? "Channels" : "Group chats");
  const wrenSection = (wren: Wren) => wren.section || "Direct messages";
  const custom = [...new Set([...rooms.map(room => room.section), ...wrens.map(wren => wren.section)].filter((section): section is string => !!section && !["Channels", "Group chats", "Direct messages"].includes(section)))];
  const groups = ["Channels", "Group chats", ...custom, "Direct messages"].filter(group => !search || visibleRooms.some(room => roomSection(room) === group) || visible.some(wren => wrenSection(wren) === group));
  function creation(group: string) {
    const room = rooms.find(item => item.section === group);
    const kind = group === "Channels" ? "channel" : group === "Group chats" ? "group" : room?.kind;
    const query = new URLSearchParams(kind ? {kind} : {});
    if (custom.includes(group)) query.set("section", group);
    return { kind:kind??"wren", href: `${kind ? "/app/wrens/rooms/new" : "/app/wrens/new"}${query.size ? `?${query}` : ""}`, label: kind === "channel" ? "New channel" : kind === "group" ? "New group chat" : "New Wren" };
  }
  return <div className={styles.roster} data-compact={false}>
    <Link className={styles.overviewLink} href="/app/wrens" aria-current={pathname === "/app/wrens" ? "page" : undefined} title="Conversations"><WrenMark/><span>Conversations</span></Link>
    <div className={styles.rosterSearchRow}>
      <label className={styles.search}><WorkspaceIcon name="search" width="16" height="16"/><input value={search} onChange={event => setSearch(event.target.value)} placeholder="Search" aria-label="Search conversations, messages and work"/></label>
      <div className={styles.creationMenu} ref={options}><button className={styles.newWren} type="button" aria-label="Conversation list options" aria-expanded={optionsOpen} aria-haspopup="menu" onClick={() => setOptionsOpen(!optionsOpen)}><WorkspaceIcon name="more" width="18" height="18"/></button>
        <MotionPresence open={optionsOpen}><div className={styles.creationChoices} role="menu" aria-label="Conversation list options" onKeyDown={moveMenuFocus}>
          <Link role="menuitem" tabIndex={-1} href="/app/wrens/library" onClick={closeOptions}><WrenMark/>Browse library</Link>
          <Link role="menuitem" tabIndex={-1} href="/app/customize" onClick={closeOptions}><WorkspaceIcon name="apps" width="15" height="15"/>Apps</Link>
          <p className={styles.menuLabel}>Conversations</p>
          {([ ["active", "Active"], ["hidden", "Hidden by me"], ["archived", "Archived"], ["all", "All conversations"] ] as const).map(([value, label]) => <button type="button" role="menuitemradio" tabIndex={-1} aria-checked={listView === value} key={value} onClick={() => { setListView(value); closeOptions(); }}><span>{label}</span>{listView === value ? <WorkspaceIcon name="check" width="14" height="14"/> : null}</button>)}
        </div></MotionPresence>
      </div>
    </div>
    <nav className={styles.rosterList} aria-label="Conversations">
      {listView !== "active" ? <button type="button" className={styles.viewNotice} onClick={() => setListView("active")}>{listView === "hidden" ? "Hidden by me" : listView === "archived" ? "Archived" : "All conversations"}<WorkspaceIcon name="close" width="12" height="12"/></button> : null}
      {lifecycle.error ? <p role="alert">Couldn’t check hidden and archived conversations. <button onClick={refreshLifecycle}>Retry</button></p> : null}
      {search.trim() && data ? <WrenSearchResults workspaceId={data.workspace.id} query={search} wrens={wrens} rooms={rooms} onSelect={() => setSearch("")}/> : null}
      {loading || lifecycle.loading ? <p role="status">Loading Wrens…</p> : null}
      {error ? <div className={styles.rosterError} role="alert"><p>{error}</p><button onClick={refresh}>Try again</button></div> : null}
      {groups.map((group, index) => {
        const collapsedGroup = !search && collapsedSections.includes(group), listID = `${prefix}-section-${index}`, add = creation(group);
        return <section key={group} className={styles.group} aria-label={group}>
          <div className={styles.groupHeader}><button className={styles.groupHeading} type="button" aria-expanded={!collapsedGroup} aria-controls={listID} onClick={() => toggleSection(group)}><WorkspaceIcon name="down" width="12" height="12"/><span>{group}</span></button>{data?.workspace.role !== "viewer" ? <button type="button" className={styles.groupCreate} aria-label={`${add.label} in ${group}`} title={add.label} onClick={()=>window.dispatchEvent(new CustomEvent("canter-create-conversation",{detail:{kind:add.kind,section:custom.includes(group)?group:undefined}}))}><WorkspaceIcon name="plus" width="15" height="15"/></button> : null}</div>
          <div id={listID} hidden={collapsedGroup}>
            {visibleRooms.filter(room => roomSection(room) === group).map(room => <div className={styles.rosterItem} key={room.id} data-active={pathname === `/app/wrens/rooms/${room.id}`}><Link href={`/app/wrens/rooms/${room.id}`} className={styles.rosterRow} aria-current={pathname === `/app/wrens/rooms/${room.id}` ? "page" : undefined}><span className={styles.roomAvatar} aria-hidden="true" data-empty={!room.wrens.length}>{room.wrens.length?room.wrens.slice(0,4).map(wren => <WrenAvatar key={wren.id} shape={wren.shape} palette={wren.palette} size={19}/>):room.kind==="channel"?<span className={styles.roomHash}>#</span>:<WorkspaceIcon name="message" width="21" height="21"/>}</span><span className={styles.rosterIdentity}><span className={styles.rowHeading}>{room.unread ? <span className={styles.unread} aria-label={`${room.unread} unread`}>{room.unread > 99 ? "99+" : room.unread}</span> : null}<strong>{room.kind === "channel" && room.wrens.length>0 ? "# " : ""}{room.kind==="channel"?(room.name.replace(/^\s*#\s*/,"")||room.name):room.name}</strong>{room.lastMessageAt ? <time dateTime={room.lastMessageAt}>{messageTime(room.lastMessageAt)}</time> : null}</span><span className={styles.previewText}>{conversationPreview(room)}</span></span></Link></div>)}
            {visible.filter(wren => wrenSection(wren) === group).map(wren => <div className={styles.rosterItem} key={wren.id} data-active={pathname === `/app/wrens/${wren.id}`}><Link href={`/app/wrens/${encodeURIComponent(wren.id)}`} className={styles.rosterRow} aria-current={pathname === `/app/wrens/${wren.id}` ? "page" : undefined} title={wren.name}><WrenAvatar shape={wren.shape} palette={wren.palette} status={wren.status} size={36}/><span className={styles.rosterIdentity}><span className={styles.rowHeading}>{wren.unread ? <span className={styles.unread} aria-label={`${wren.unread} unread`}>{wren.unread > 99 ? "99+" : wren.unread}</span> : null}<strong>{wren.name}</strong>{wren.lastMessageAt ? <time dateTime={wren.lastMessageAt}>{messageTime(wren.lastMessageAt)}</time> : null}</span><span className={styles.previewText}>{conversationPreview(wren)}</span></span></Link></div>)}
          </div>
        </section>;
      })}
      {!loading && !lifecycle.loading && !lifecycle.error && !error && !visible.length && !visibleRooms.length ? search || listView !== "active" || wrens.length || rooms.length ? <p>No matching conversations</p> : <p>No conversations yet</p> : null}
    </nav>
  </div>;
}

export function WrensHome({section,create=false,autoCreate=false}:{section?:string;create?:boolean;autoCreate?:boolean} = {}) {
 const router=useRouter();
 const {data}=useWorkspace();
 const [showWelcome,setShowWelcome]=useState(autoCreate);
 return <AppShell active="Wrens" agentView onNewInstruction={()=>{if(data?.workspace.role!=="viewer")window.dispatchEvent(new CustomEvent("canter-create-conversation",{detail:{kind:"wren"}}));}} sidebarContent={<WrenSidebar/>} pageTitle="Wrens" pageIcon={<WrenMark/>}><WrenDashboard/>{create||showWelcome?<WrenDialog showTitle title="Create a Wren" onClose={()=>create?router.replace("/app/wrens"):setShowWelcome(false)}><WrenForm initialSection={section}/></WrenDialog>:null}</AppShell>;
}

export function WrenConversation({ wren: initial, initialDetail, initialLoadedAt }: { wren: Wren; initialDetail: ConversationDetail | null; initialLoadedAt?: number }) {
  const { wrens, refresh } = useWrens();
 const {data}=useWorkspace();
 const accountID=data?.account.id,workspaceID=data?.workspace.id;
  const wren = wrens.find(item => item.id === initial.id) ?? initial;
  const [editing, setEditing] = useState(false);
  const [settingsMounted, setSettingsMounted] = useState(false);
  const dismissSettings = useCallback(() => setEditing(false), []);
  const closeSettings = useCallback(() => { setEditing(false); requestAnimationFrame(() => { const target = document.querySelector<HTMLElement>('[data-workspace-surface][aria-hidden="false"] [role="tab"][aria-selected="true"]') ?? document.querySelector<HTMLElement>("[data-page-title-action]"); target?.focus(); }); }, []);
  const toggleSettings = useCallback(() => { setSettingsMounted(true); setEditing(value => !value); }, []);
  useEffect(() => { const open = (event:Event) => { if ((event as CustomEvent<{conversationId:string}>).detail?.conversationId === wren.conversationId) { setSettingsMounted(true); setEditing(true); } }; window.addEventListener("canter-conversation-settings",open);return () => window.removeEventListener("canter-conversation-settings",open); },[wren.conversationId]);
  useEffect(() => { if(accountID && workspaceID) rememberWren(accountID,workspaceID,wren.id); }, [accountID,workspaceID,wren.id]);
  return <><OperatorWorkspace key={wren.id} id={wren.conversationId} initialDetail={initialDetail} initialLoadedAt={initialLoadedAt} wren={wren} onConversationUpdate={refresh} sidebarContent={<WrenSidebar />} onEditWren={toggleSettings} inspectorOpen={editing} onCloseInspector={dismissSettings} inspector={settingsMounted ? <WrenForm key={wren.id} wren={wren} onSaved={closeSettings}/> : undefined} /></>;
}

export function ConversationSettingsNav({name,destination,purposeId,onNavigate}:{name:string;destination:string;purposeId:string;onNavigate?:()=>void}) {
  const openWork = useContext(WrenWorkNavigation);
  return <nav data-wren-settings-navigation className={styles.settingsNav} aria-label={`${name} settings sections`}><button type="button" aria-current="page" onClick={() => { const field=document.getElementById(purposeId);field?.scrollIntoView({block:"nearest"});field?.focus({preventScroll:true}); }}>Purpose</button>{([ ["wren-memory","Memory"], ["wren-skills","Skills"], ["wren-extensions","Tools"], ["computer","Computer"] ] as const).map(([kind,label]) => openWork ? <button key={kind} type="button" onClick={() => openWork({kind})}>{label}</button> : <Link key={kind} href={`${destination}?work=${kind}`} onNavigate={onNavigate}>{label}</Link>)}</nav>;
}

export function WrenForm({ wren, onSaved, initialSection }: { wren?: Wren; onSaved?: () => void; initialSection?:string }) {
  const { save, wrens, refresh:refreshWrens } = useWrens();
  const { retry,data } = useWorkspace(), router = useRouter();
  const sectionID = useId(), purposeID = useId();
  const [draft, setDraft] = useState<Wren>(() => wren ?? { id: `wren_${crypto.randomUUID()}`, conversationId: "", name: "", purpose: "", role: "", section: initialSection??"", shape: "pebble", palette: 0, updatedAt: "", status: "idle" });
  const [saving, setSaving] = useState(false), [error, setError] = useState("");
  const canEdit=data?.workspace.role!=="viewer"&&(!wren||wren.canManageProfile!==false);
  const dirty = !wren || (["name", "purpose", "role", "section", "shape", "palette"] as const).some(key => (draft[key] ?? "") !== (wren[key] ?? ""));
  const shuffle=()=>setDraft(value=>({...value,shape:wrenShapes[Math.floor(Math.random()*wrenShapes.length)],palette:Math.floor(Math.random()*wrenPalettes.length)}));
  async function submit(event: FormEvent) {
    event.preventDefault(); if (saving||!canEdit) return; setSaving(true); setError("");
    try { const result = await save(draft, !wren); setDraft(result); retry(); if (!wren) {onSaved?.();router.push(`/app/wrens/${encodeURIComponent(result.id)}`);} else { setSaving(false); onSaved?.(); router.refresh(); } }
    catch (cause) { setError(cause instanceof Error ? cause.message : "Couldn’t save your Wren."); setSaving(false); }
  }
  const appearance=<details className={styles.appearance} open={!wren}><summary>Appearance</summary><div className={styles.colors} role="group" aria-label="Wren color">{wrenPalettes.map((colors,i)=><button type="button" key={i} aria-label={`Color ${i+1}`} aria-pressed={draft.palette===i} style={{background:`linear-gradient(135deg,${colors[0]},${colors[1]})`}} onClick={()=>setDraft({...draft,palette:i})}/>)}</div><div className={styles.shapePicker} role="group" aria-label="Wren shape">{wrenShapes.map(shape=><button type="button" key={shape} aria-label={`${shape} shape`} aria-pressed={draft.shape===shape} onClick={()=>setDraft({...draft,shape})}><WrenAvatar shape={shape} palette={draft.palette} size={26} animated={false}/></button>)}</div>{wren?<button type="button" className={styles.textAction} onClick={shuffle}>Try another look</button>:null}</details>;
  const organization=<details className={styles.organization}><summary>Organization</summary><label>Section<input list={sectionID} maxLength={48} value={draft.section??""} onChange={event=>setDraft({...draft,section:event.target.value})} placeholder="Optional"/></label><datalist id={sectionID}>{[...new Set(wrens.map(item=>item.section).filter(Boolean))].map(section=><option key={section} value={section}/>)}</datalist></details>;
  return <form className={styles.form} data-inspector={!!wren} onSubmit={submit} aria-label={wren ? `${wren.name} settings` : "Create a Wren"}>
    {wren?<><header className={styles.settingsHeading}><WrenAvatar shape={draft.shape} palette={draft.palette} size={34}/><div><h2>{wren.name} settings</h2><p>{wren.shared?"Managed identity · your private chat":"Purpose and configuration"}</p></div></header><ConversationSettingsNav name={wren.name} destination={`/app/wrens/${encodeURIComponent(wren.id)}`} purposeId={purposeID} onNavigate={onSaved}/></>:<div className={styles.avatarPreview}><WrenAvatar shape={draft.shape} palette={draft.palette} size={64}/><button type="button" className={styles.shuffle} aria-label="Try another Wren appearance" title="Try another look" disabled={!canEdit} onClick={shuffle}><WorkspaceIcon name="customize" width="15" height="15"/></button></div>}
    <fieldset disabled={saving||!canEdit}>
      <label>Name<input name="name" autoComplete="off" maxLength={48} required value={draft.name} onChange={event=>setDraft({...draft,name:event.target.value})} placeholder="Name your Wren"/></label>
      {wren?<label>Role<input maxLength={32} value={draft.role??""} onChange={event=>setDraft({...draft,role:event.target.value})} placeholder="A short title, if you’d like"/></label>:null}
      <label>{wren?"Purpose and instructions":"What should this Wren help with?"}<textarea id={purposeID} rows={wren?6:3} maxLength={3000} value={draft.purpose} onChange={event=>setDraft({...draft,purpose:event.target.value})} placeholder="Describe the work you’d like to give it."/></label>
      {!wren?<details className={styles.creationOptions}><summary>Appearance and organization</summary>{appearance}{organization}<label>Role<input maxLength={32} value={draft.role??""} onChange={event=>setDraft({...draft,role:event.target.value})} placeholder="Optional"/></label></details>:null}
    </fieldset>
    {error?<p className={styles.error} role="alert">{error}</p>:null}
    {wren&&draft.publicationRevision!==wren.publicationRevision?<p className={styles.roomNote}>The managed profile changed. Your draft is kept here. <button type="button" onClick={()=>setDraft(wren)}>Discard draft and load current profile</button></p>:null}
    {!canEdit?<p className={styles.roomNote}>{data?.workspace.role==="viewer"?"Your workspace role gives read-only access.":"The publisher and designated managers maintain this shared profile. Your private chat stays yours."}</p>:<footer>{wren && dirty ? <span className={styles.unsaved} role="status">Unsaved changes</span> : null}<button className={styles.create} disabled={saving||!draft.name.trim()||!dirty}>{saving?"Saving…":wren?"Save changes":"Create Wren"}</button></footer>}
    {wren?<details className={styles.moreSettings}><summary>More settings</summary><fieldset disabled={saving||!canEdit}>{appearance}{organization}</fieldset>{data?<><WrenSharingControls workspaceId={data.workspace.id} wren={wren} onChanged={()=>{refreshWrens();retry();router.refresh();}}/><WrenLifecycleControls workspaceId={data.workspace.id} conversationId={wren.conversationId} destination={`/app/wrens/${encodeURIComponent(wren.id)}`} onChanged={retry}/></>:null}</details>:null}
  </form>;
}
