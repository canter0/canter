"use client";
import Link from "next/link";
import {displayName} from "@/lib/display-name";
import type {WrenMember} from "@/lib/wren-multiplayer";
import {CanterSelect} from "./canter-select";
import { useCallback, useEffect, useState, type FormEvent } from "react";
import { canterFetch, CanterAPIError } from "@/lib/canter-api";
import { knowledgeQuery, knowledgeTarget, sharedMemoryDestination, memoryForgetEvent, notifyMemoryForgotten, parseSkillCapabilities, roomMemoryAudience, skillInstallationLabel, visibleMemory, type KnowledgeTarget, type WrenMemoryRecord, type WrenSkillCapability, type WrenSkillInstallation, type WrenSkillRecord } from "@/lib/wren-knowledge";
import { downloadWrenBlob, fetchWrenBlob, notifyWorkUpdate, useWrenResource, workBase, workError, workUpdateEvent, type WrenArtifactRecord } from "@/lib/wren-work";
import type { Wren, WrenRoom } from "@/lib/wrens";
import { useWorkspace } from "./workspace-context";
import styles from "./wren-knowledge-panel.module.css";

const keepKnowledgeCurrent=()=>true;
function ErrorLine({ value, retry }: { value: string; retry?: () => void }) { return value ? <p className={styles.error} role="alert">{value}{retry ? <> <button type="button" onClick={retry}>Refresh</button></> : null}</p> : null; }
export function WrenKnowledgePanel({ workspaceId, conversationId, kind, memoryId, onUse }: { onUse?: (prompt:string)=>void; workspaceId: string; conversationId?: string; memoryId?: string; kind: "memory" | "skills" }) {
  const base = workBase(workspaceId), { data } = useWorkspace();
  const wrens = useWrenResource<{ wrens: Wren[] }>(`${base}/wrens`), rooms = useWrenResource<{ rooms: WrenRoom[] }>(`${base}/rooms`);
  const target = knowledgeTarget(conversationId, wrens.data?.wrens ?? [], rooms.data?.rooms ?? []);
  const loading = !target && (wrens.loading || rooms.loading), canWrite = !!data && data.workspace.role !== "viewer";
  return <section className={styles.panel} data-kind={kind} aria-label={kind === "memory" ? "Memory" : "Skills"}>
    {target ? <p className={styles.audience}>{target.label}<span>{kind === "skills" ? "Installed workflows apply here. The skill catalog is shared with your workspace." : target.scope.scope === "room" ? "Visible to this room’s participants." : "This Wren’s saved knowledge stays out of other rooms."}</span></p> : null}
    {loading ? <p className={styles.note} role="status">Loading this conversation…</p> : !target && (wrens.error || rooms.error) ? <ErrorLine value={wrens.error || rooms.error} retry={() => { wrens.refresh(); rooms.refresh(); }} /> : null}
    {kind === "memory" ? target ? <MemoryPanel key={`${knowledgeQuery(target.scope)}:${memoryId??""}`} memoryId={memoryId} workspaceId={workspaceId} target={target} rooms={rooms.data?.rooms ?? []} roomError={rooms.error} canWrite={canWrite} workspaceName={data?.workspace.name ?? "this workspace"} /> : !loading ? <p className={styles.note}>Open a Wren or shared-room conversation to view its memory.</p> : null : <SkillsPanel onUse={onUse} key={target ? knowledgeQuery(target.scope) : "catalog"} workspaceId={workspaceId} target={target} canWrite={canWrite} accountId={data?.account.id ?? ""} workspaceName={data?.workspace.name ?? "this workspace"} />}
  </section>;
}

function MemoryPanel({ workspaceId, target, rooms, roomError, canWrite, workspaceName, memoryId }: { memoryId?: string; workspaceId: string; target: KnowledgeTarget; rooms: WrenRoom[]; roomError: string; canWrite: boolean; workspaceName: string }) {
  const base = `${workBase(workspaceId)}/wren-memory`, query = knowledgeQuery(target.scope);
  const people=useWrenResource<{members:WrenMember[]}>(`${workBase(workspaceId)}/wren-members`);
  const [searchQuery, setSearchQuery] = useState("");
  const list = useWrenResource<{ memories: WrenMemoryRecord[] }>(`${base}?${query}${searchQuery ? `&q=${encodeURIComponent(searchQuery)}` : ""}`,3000,keepKnowledgeCurrent);
  const [selected, setSelected] = useState(memoryId??""), [creating, setCreating] = useState(false), [editing, setEditing] = useState(false), [source, setSource] = useState<WrenMemoryRecord | null>(null);
  const [forgotten, setForgotten] = useState<Set<string>>(() => new Set()), [version, setVersion] = useState(""), [search, setSearch] = useState("");
  const resource = useWrenResource<WrenMemoryRecord>(selected ? `${base}/${encodeURIComponent(selected)}` : "",3000,keepKnowledgeCurrent);
  const revisions = useWrenResource<{ revisions: WrenMemoryRecord[] }>(selected ? `${base}/${encodeURIComponent(selected)}/revisions` : "",3000,keepKnowledgeCurrent);
  const [sharedCopy, setSharedCopy] = useState<WrenMemoryRecord | null>(null);
  const copyDestination = sharedCopy ? sharedMemoryDestination(sharedCopy, rooms) : null;
  const [savedRecord, setSavedRecord] = useState<WrenMemoryRecord | null>(null);
  const resolved = resource.error && !resource.data ? undefined : savedRecord?.id === selected && (!resource.data || savedRecord.latestRevision > resource.data.latestRevision) ? savedRecord : resource.data;
  const scoped = resolved?.conversationId === target.scope.conversationId && resolved?.scope === target.scope.scope ? resolved : undefined;
  const current = scoped ? visibleMemory(scoped, forgotten) : !resolved && !resource.error && editing && source ? visibleMemory(source, forgotten) : undefined;
  const shown = !current ? undefined : current.deleted ? current : version ? revisions.data?.revisions.find(item => item.revision === Number(version)) : current;
  const record = shown ? visibleMemory(shown, forgotten) : undefined;
  const [error, setError] = useState(""), [busy, setBusy] = useState(false), [confirmForget, setConfirmForget] = useState(false), [sharing, setSharing] = useState(false), [destination, setDestination] = useState(""), [notice, setNotice] = useState(""), [shareSource, setShareSource] = useState<WrenMemoryRecord | null>(null);
  useEffect(() => { const timer = window.setTimeout(() => { if (new TextEncoder().encode(search.trim()).length <= 160) setSearchQuery(search.trim()); }, 250); return () => window.clearTimeout(timer); }, [search]);
  const refreshList = list.refresh, refreshRecord = resource.refresh, refreshRevisions = revisions.refresh;
  const refresh = useCallback(() => { refreshList(); refreshRecord(); refreshRevisions(); }, [refreshList, refreshRecord, refreshRevisions]);
  const markForgotten = useCallback((id: string) => { setForgotten(previous => new Set([...previous, id])); setSource(previous => previous?.id === id ? null : previous); setShareSource(previous => previous?.id === id ? null : previous); if (selected === id) { setEditing(false); setSharing(false); setConfirmForget(false); } refresh(); }, [refresh, selected]);
  useEffect(() => { const changed = (event: Event) => { const detail = (event as CustomEvent<{ workspaceId: string; memoryId?: string; conversationId?: string }>).detail; if (detail?.workspaceId !== workspaceId) return; if (event.type === memoryForgetEvent && detail.memoryId) markForgotten(detail.memoryId); else if (!detail.conversationId || detail.conversationId === target.scope.conversationId) refresh(); }; window.addEventListener(workUpdateEvent, changed); window.addEventListener(memoryForgetEvent, changed); return () => { window.removeEventListener(workUpdateEvent, changed); window.removeEventListener(memoryForgetEvent, changed); }; }, [workspaceId, target.scope.conversationId, markForgotten, refresh]);
  function back() { setSharedCopy(null); setSelected(""); setEditing(false); setCreating(false); setSource(null); setVersion(""); setError(""); setNotice(""); setSharing(false); setShareSource(null); setConfirmForget(false); refreshList(); }
  async function forget() { if (!current || !canWrite || busy) return; setBusy(true); setError(""); try { const value = await canterFetch<WrenMemoryRecord>(`${base}/${encodeURIComponent(current.id)}/forget`, { method: "POST", body: JSON.stringify({ expectedRevision: current.latestRevision }) }); if (!value.deleted) throw new Error("The memory was not forgotten."); markForgotten(value.id); notifyMemoryForgotten(workspaceId, target.scope.conversationId, value.id); notifyWorkUpdate(workspaceId, target.scope.conversationId); } catch (cause) { setError(workError(cause, "Couldn’t forget this memory.")); } finally { setBusy(false); } }
  async function share() { if (!shareSource || shareSource.deleted || forgotten.has(shareSource.id) || !canWrite || !destination || busy) return; setBusy(true); setError(""); try { const value = await canterFetch<WrenMemoryRecord>(`${base}/${encodeURIComponent(shareSource.id)}/promote`, { method: "POST", body: JSON.stringify({ expectedRevision: shareSource.latestRevision, conversationId: destination }) }); if (value.conversationId !== destination) throw new Error("The room copy could not be confirmed."); setSharing(false); setShareSource(null); setSharedCopy(value); setNotice(`A separate copy was shared with ${rooms.find(room => room.conversationId === destination)?.name ?? "the selected room"}.`); notifyWorkUpdate(workspaceId, destination); } catch (cause) { setError(cause instanceof CanterAPIError && cause.status === 409 ? "This memory changed. Close sharing and review the latest version before trying again." : workError(cause, "Couldn’t share this memory.")); } finally { setBusy(false); } }
  async function downloadSource(memory: WrenMemoryRecord) {
    if (!memory.sources.artifactId || busy) return; setBusy(true); setError("");
    try { const path = `${workBase(workspaceId)}/wren-artifacts/${encodeURIComponent(memory.sources.artifactId)}`, query = memory.sources.artifactRevision ? `?revision=${memory.sources.artifactRevision}` : ""; const metadata = await canterFetch<WrenArtifactRecord>(`${path}${query}`); const blob = await fetchWrenBlob(`${path}/content?revision=${metadata.revision}`); downloadWrenBlob(blob, metadata.name); }
    catch (cause) { setError(workError(cause, "The source file could not be downloaded.")); }
    finally { setBusy(false); }
  }
  const saved = (value: WrenMemoryRecord) => { setSavedRecord(value); setSelected(value.id); setCreating(false); setEditing(false); setSource(null); setVersion(""); refresh(); notifyWorkUpdate(workspaceId, target.scope.conversationId); };
  const audience = rooms.find(room => room.conversationId === destination);
  return <>
    {selected || creating ? <button type="button" className={styles.back} onClick={back}>← Memory</button> : null}
    <ErrorLine value={error || resource.error || list.error} retry={refresh} />
    {current?.deleted || selected && forgotten.has(selected) ? <><h2>Forgotten memory</h2><p className={styles.note}>The saved text and revision text are no longer available. Copies previously shared with other rooms remain separate.</p></> : creating || editing && current ? <MemoryForm key={creating ? "new" : current!.id} workspaceId={workspaceId} target={target} initial={editing ? source ?? current : undefined} latestRevision={current?.latestRevision} readOnly={!canWrite || !!resource.error && !resource.data} onForgotten={id => { markForgotten(id); notifyMemoryForgotten(workspaceId, target.scope.conversationId, id); }} onSaved={saved} onCancel={() => { setCreating(false); setEditing(false); setSource(null); refresh(); }} /> : selected ? record ? <>
      <header className={styles.heading}><h2>{record.title}</h2><span className={styles.small}>Version {record.revision}</span></header><p className={styles.note}>{people.data?.members.some(person=>person.accountId===record.authorId)?displayName(people.data.members.find(person=>person.accountId===record.authorId)!.email):"a contributor"} · {new Date(record.createdAt).toLocaleString()}</p><p className={styles.content}>{record.content}</p>
      {record.scope === "room" && record.promotedFromRevision ? <p className={styles.copyOrigin}>Shared from a private memory · original version {record.promotedFromRevision}.<span>This room has its own copy. Changes here and in the original stay separate.</span></p> : null}
      {canWrite ? <div className={styles.actions}><button type="button" className={styles.secondary} onClick={() => { setSource(current!); setVersion(""); setEditing(true); }}>Edit latest</button>{target.scope.scope === "private" ? <button type="button" className={styles.secondary} onClick={() => { setVersion(""); setShareSource(current!); setSharing(true); setDestination(""); }}>Share a copy…</button> : null}<button type="button" className={styles.danger} onClick={() => setConfirmForget(true)}>Forget…</button></div> : null}
      {notice ? <div className={styles.shareReceipt} role="status"><p>{notice}</p>{copyDestination ? <Link className={styles.secondary} href={copyDestination.href}>Open copy in {copyDestination.name}</Link> : null}</div> : null}
      {confirmForget ? <div className={styles.confirm}><p>Forget this memory and all its saved revision text?</p><button type="button" className={styles.danger} disabled={busy} onClick={() => void forget()}>Forget permanently</button><button type="button" className={styles.secondary} onClick={() => setConfirmForget(false)}>Keep it</button></div> : null}
      {sharing ? <div className={styles.share}><h3>Share a copy with a room</h3><p className={styles.note}>Latest version {shareSource?.latestRevision}</p><p className={styles.sharePreview}>{shareSource?.content}</p><ErrorLine value={roomError} /><label>Room<CanterSelect ariaLabel="Share memory with room" value={destination} onChange={setDestination} options={[{value:"",label:"Choose a room"},...rooms.map(room=>({value:room.conversationId,label:`${room.kind==="channel"?"# ":"Private group · "}${room.name}`}))]}/></label>{audience ? <><p className={styles.audienceNotice}>{roomMemoryAudience(audience, workspaceName)}</p><p className={styles.note}>Only the latest text and source links are copied. Private source records stay private. Editing or forgetting the private memory will not change the room copy.</p></> : null}<button className={styles.primary} type="button" disabled={busy || !audience} onClick={() => void share()}>Share copy</button><button className={styles.secondary} type="button" onClick={() => { setSharing(false); setShareSource(null); }}>Cancel</button></div> : null}
      <details className={styles.details}><summary>Versions and sources</summary><ErrorLine value={revisions.error} retry={refreshRevisions} /><label>Saved version<CanterSelect ariaLabel="Saved memory version" value={version} onChange={setVersion} options={[{value:"",label:"Latest"},...(revisions.data?.revisions.filter(item=>!visibleMemory(item,forgotten).deleted).map(item=>({value:item.revision,label:`Version ${item.revision}`}))??[])]}/></label><p className={styles.note}>Saved {new Date(record.createdAt).toLocaleString()}</p>{record.sources.urls?.map(url => <a key={url} href={url} target="_blank" rel="noreferrer">{url}</a>)}{record.sources.artifactId ? <button type="button" className={styles.secondary} disabled={busy} onClick={() => void downloadSource(record)}>Download source file{record.sources.artifactRevision ? ` · version ${record.sources.artifactRevision}` : ""}</button> : null}{record.sources.runId ? <p className={styles.note}>Source run: {record.sources.runId}</p> : null}</details>
    </> : resource.loading ? <p className={styles.note} role="status">Loading memory…</p> : <p className={styles.note}>This memory isn’t available in this conversation.</p> : <><header className={styles.heading}><h2>Memory</h2>{canWrite ? <button type="button" className={styles.secondary} onClick={() => setCreating(true)}>Add</button> : null}</header><p className={styles.note}>{target.scope.scope === "private" ? "Facts and preferences saved for this Wren." : "Knowledge explicitly shared with this room."}</p><label className={styles.search}><span className={styles.srOnly}>Find saved memory</span><input value={search} onChange={event => setSearch(event.target.value)} placeholder="Find a memory" maxLength={160} /></label>{new TextEncoder().encode(search.trim()).length > 160 ? <p className={styles.error}>Keep the search under 160 bytes.</p> : null}{list.loading ? <p className={styles.note} role="status">Loading memory…</p> : null}{list.data?.memories.filter(item => !visibleMemory(item, forgotten).deleted && `${item.title} ${item.content}`.toLowerCase().includes(search.toLowerCase())).map(item => <button type="button" key={item.id} className={styles.row} onClick={() => { setSelected(item.id); setVersion(""); setNotice(""); setError(""); }}><span><strong>{item.title}</strong><small>{item.content.replace(/\s+/g, " ").slice(0, 110)}</small></span></button>)}{list.data?.memories.filter(item => !visibleMemory(item, forgotten).deleted).length === 0 ? <p className={styles.note}>No saved memory here yet.</p> : null}</>}
  </>;
}

function MemoryForm({ workspaceId, target, initial, latestRevision, readOnly, onForgotten, onSaved, onCancel }: { workspaceId: string; target: KnowledgeTarget; initial?: WrenMemoryRecord; latestRevision?:number; readOnly: boolean; onForgotten: (id: string) => void; onSaved: (value: WrenMemoryRecord) => void; onCancel: () => void }) {
  const [draft, setDraft] = useState(() => ({ id: initial?.id ?? `memory_${crypto.randomUUID()}`, revision: initial?.latestRevision ?? 0, title: initial?.title ?? "", content: initial?.content ?? "", urls: initial?.sources.urls?.join("\n") ?? "", sources: initial?.sources ?? {} }));
  const [busy, setBusy] = useState(false), [error, setError] = useState(""), [conflict, setConflict] = useState(false), [denied, setDenied] = useState(false);
  const [review, setReview] = useState<WrenMemoryRecord | null>(null);
  const base = `${workBase(workspaceId)}/wren-memory`;
  const revision = review?.latestRevision ?? draft.revision;
  const newerVersion = !!initial && !!latestRevision && latestRevision > revision;
  const needsReview = newerVersion || conflict;

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (busy || readOnly || denied || needsReview) return;
    setBusy(true); setError("");
    try {
      if (new TextEncoder().encode(draft.content).length > 8000) throw new Error("Keep saved memory under 8000 bytes.");
      const value = await canterFetch<WrenMemoryRecord>(`${base}${initial ? `/${encodeURIComponent(draft.id)}` : ""}`, {
        method: initial ? "PATCH" : "POST",
        body: JSON.stringify({ id: draft.id, ...target.scope, title: draft.title, content: draft.content,
          sources: { ...(review?.sources ?? draft.sources), urls: draft.urls.split("\n").map(url => url.trim()).filter(Boolean) }, expectedRevision: revision }),
      });
      onSaved(value);
    } catch (cause) {
      if (cause instanceof CanterAPIError && cause.status === 409 && initial) {
        setConflict(true);
        try {
          const latest = await canterFetch<WrenMemoryRecord>(`${base}/${encodeURIComponent(draft.id)}`);
          if (latest.deleted) { onForgotten(latest.id); return; }
        } catch { /* Preserve the draft and require a successful review before retry. */ }
      }
      if (cause instanceof CanterAPIError && [401, 403, 404].includes(cause.status)) setDenied(true);
      setError(cause instanceof CanterAPIError && cause.status === 409 ? "Someone saved a newer version. Your changes haven’t been applied." : workError(cause, "Couldn’t save this memory."));
    } finally { setBusy(false); }
  }

  async function reviewSaved() {
    if (busy || readOnly || denied) return;
    setBusy(true); setError("");
    try {
      const value = await canterFetch<WrenMemoryRecord>(`${base}/${encodeURIComponent(draft.id)}`);
      if (value.deleted) { onForgotten(value.id); return; }
      setReview(value); setConflict(false);
    } catch (cause) {
      if (cause instanceof CanterAPIError && [401, 403, 404].includes(cause.status)) setDenied(true);
      setError(workError(cause, "Couldn’t load saved memory. Your draft is kept."));
    } finally { setBusy(false); }
  }

  return <form className={styles.form} onSubmit={submit} aria-label={initial ? "Edit memory" : "New memory"}>
    <h2>{initial ? "Edit memory" : "New memory"}</h2>
    {needsReview ? <div className={styles.audienceNotice} role="status">
      <p>{newerVersion ? `Version ${latestRevision} was saved while you were editing.` : "This memory changed."} Your draft is kept.</p>
      <button className={styles.secondary} type="button" disabled={busy || readOnly || denied} onClick={() => void reviewSaved()}>Review saved changes</button>
    </div> : null}
    {review ? <section className={styles.memoryComparison} aria-label={`Saved memory version ${review.latestRevision}`}>
      <header><h3>Saved version {review.latestRevision}</h3><time dateTime={review.updatedAt}>{new Date(review.updatedAt).toLocaleTimeString([], { hour: "numeric", minute: "2-digit" })}</time></header>
      <div className={styles.memoryComparisonText}><strong>{review.title}</strong><p>{review.content}</p>{review.sources.urls?.length ? <><span>Source links</span><p>{review.sources.urls.join("\n")}</p></> : null}</div>
      <p className={styles.note}>Your draft is below. Combine any changes you want to keep, then save the reviewed version.</p>
    </section> : null}
    <fieldset disabled={busy || readOnly || denied}>
      <label>Title<input required maxLength={120} value={draft.title} onChange={event => setDraft({ ...draft, title: event.target.value })} /></label>
      <label>{review ? "Your version" : "Remember"}<textarea required rows={6} maxLength={8000} value={draft.content} onChange={event => setDraft({ ...draft, content: event.target.value })} /></label>
      <details className={styles.details}><summary>Source links</summary><label>One link per line<textarea rows={3} value={draft.urls} onChange={event => setDraft({ ...draft, urls: event.target.value })} /></label></details>
    </fieldset>
    <ErrorLine value={error} />
    {readOnly || denied ? <p className={styles.note}>This draft is read-only because access changed.</p> : null}
    <footer><button type="button" className={styles.secondary} onClick={onCancel}>Cancel</button><button className={styles.primary} disabled={busy || readOnly || denied || needsReview || !draft.title.trim() || !draft.content.trim()}>{busy ? "Working…" : review ? "Save reviewed changes" : "Save memory"}</button></footer>
  </form>;
}

function SkillsPanel({ workspaceId, target, canWrite, accountId, workspaceName, onUse }: { onUse?: (prompt:string)=>void; workspaceId: string; target: KnowledgeTarget | null; canWrite: boolean; accountId: string; workspaceName: string }) {
  const base = `${workBase(workspaceId)}/wren-skills`, query = target ? knowledgeQuery(target.scope) : "";
  const catalog = useWrenResource<{ skills: WrenSkillRecord[] }>(base,3000,keepKnowledgeCurrent), installed = useWrenResource<{ installations: WrenSkillInstallation[] }>(target ? `${base}/installations?${query}` : "",3000,keepKnowledgeCurrent);
  const [selected, setSelected] = useState(""), [view, setView] = useState<"installed" | "catalog">(target ? "installed" : "catalog"), [creating, setCreating] = useState(false), [editing, setEditing] = useState(false), [editSource, setEditSource] = useState<WrenSkillRecord | null>(null), [version, setVersion] = useState("");
  const resource = useWrenResource<WrenSkillRecord>(selected ? `${base}/${encodeURIComponent(selected)}` : "",3000,keepKnowledgeCurrent), versions = useWrenResource<{ versions: WrenSkillRecord[] }>(selected ? `${base}/${encodeURIComponent(selected)}/versions` : "",3000,keepKnowledgeCurrent);
  const latest = resource.data ?? (editing && editSource?.id === selected ? editSource : undefined);
  const skill = version ? versions.data?.versions.find(item => item.version === Number(version)) : latest;
  const installation = installed.data?.installations.find(item => item.skillId === selected);
  const availability = useWrenResource<{ capabilities: WrenSkillCapability[] }>(skill ? `${base}/${encodeURIComponent(skill.id)}/availability?version=${skill.version}${target ? `&conversationId=${encodeURIComponent(target.scope.conversationId)}` : ""}` : "",3000,keepKnowledgeCurrent);
  const [busy, setBusy] = useState(false), [error, setError] = useState(""), [conflict, setConflict] = useState(false), [notice, setNotice] = useState("");
  const refreshCatalog = catalog.refresh, refreshInstalled = installed.refresh, refreshSkill = resource.refresh, refreshVersions = versions.refresh, refreshAvailability = availability.refresh;
  const refresh = useCallback(() => { refreshCatalog(); refreshInstalled(); refreshSkill(); refreshVersions(); refreshAvailability(); }, [refreshCatalog, refreshInstalled, refreshSkill, refreshVersions, refreshAvailability]);
  useEffect(() => { const changed = (event: Event) => { const detail = (event as CustomEvent<{ workspaceId: string }>).detail; if (detail?.workspaceId === workspaceId) refresh(); }; window.addEventListener(workUpdateEvent, changed); return () => window.removeEventListener(workUpdateEvent, changed); }, [workspaceId, refresh]);
  function back() { setSelected(""); setCreating(false); setEditing(false); setEditSource(null); setVersion(""); setError(""); setConflict(false); setNotice(""); refresh(); }
  function open(id: string, pinned?: number) { setSelected(id); setVersion(pinned ? String(pinned) : ""); setError(""); setConflict(false); setNotice(""); }
  async function install(pin: number, enabled: boolean) {
    if (!target || !skill || !canWrite || busy) return; setBusy(true); setError(""); setConflict(false); setNotice("");
    try { const value = await canterFetch<WrenSkillInstallation>(`${base}/installations`, { method: "POST", body: JSON.stringify({ ...target.scope, skillId: skill.id, version: pin, enabled, expectedRevision: installation?.revision ?? 0 }) }); setVersion(String(value.version)); setNotice(`${value.skill.name} is pinned to version ${value.version}${value.enabled ? " and enabled here" : " and disabled here"}.`); refresh(); notifyWorkUpdate(workspaceId, target.scope.conversationId); }
    catch (cause) { setConflict(cause instanceof CanterAPIError && cause.status === 409); setError(workError(cause, "Couldn’t change this installation.")); }
    finally { setBusy(false); }
  }
  async function uninstall() {
    if (!target || !installation || busy || !canWrite) return; setBusy(true); setError(""); setConflict(false); setNotice("");
    try { await canterFetch(`${base}/installations`, { method: "DELETE", body: JSON.stringify({ ...target.scope, skillId: installation.skillId, expectedRevision: installation.revision }) }); setNotice("Uninstalled here. The workspace skill and its versions are unchanged."); refresh(); notifyWorkUpdate(workspaceId, target.scope.conversationId); }
    catch (cause) { setConflict(cause instanceof CanterAPIError && cause.status === 409); setError(workError(cause, "Couldn’t uninstall this skill.")); }
    finally { setBusy(false); }
  }
  const saved = (value: WrenSkillRecord) => { setSelected(value.id); setVersion(""); setCreating(false); setEditing(false); setEditSource(null); refresh(); notifyWorkUpdate(workspaceId, target?.scope.conversationId); };
  return <>
    {selected || creating ? <button type="button" className={styles.back} onClick={back}>← Skills</button> : null}
    <ErrorLine value={error || resource.error || catalog.error || installed.error} retry={refresh} />{conflict ? <p className={styles.note}>This installation changed. Refresh it and review its current version before trying again.</p> : null}
    {creating || editing && latest ? <SkillForm key={creating ? "new-skill" : latest!.id} workspaceId={workspaceId} workspaceName={workspaceName} initial={editing ? editSource ?? latest : undefined} readOnly={!canWrite || !!resource.error && !resource.data} onSaved={saved} onCancel={() => { setCreating(false); setEditing(false); setEditSource(null); refresh(); }} /> : selected ? skill && latest ? <>
      <header className={styles.heading}><h2>{skill.name}</h2><span className={styles.small}>Version {skill.version}</span></header><p className={styles.note}>{skill.description}</p>
      {installation ? <p className={styles.installStatus}>{skillInstallationLabel(installation)} · pinned to version {installation.version}</p> : target ? <p className={styles.note}>Not installed in {target.scope.scope === "room" ? "this room" : "this Wren’s private conversation"}.</p> : null}
      {latest.disabled ? <p className={styles.note}>The author disabled this skill for the workspace. Its instructions are not loaded, including older installed versions.</p> : null}
      <label className={styles.version}>View version<CanterSelect ariaLabel="Skill version" value={version} onChange={setVersion} options={[{value:"",label:`Latest · version ${latest.latestVersion}`},...(versions.data?.versions.map(item=>({value:item.version,label:`Version ${item.version}`}))??[])]}/></label><ErrorLine value={versions.error} retry={refreshVersions} />
      {installation&&skill.version!==installation.version?<div className={styles.confirm}><p>Reviewing version {skill.version}. This conversation still uses version {installation.version} until you apply the change.</p><details className={styles.details}><summary>Compare with installed version {installation.version}</summary><p className={styles.content}>{installation.skill.instructions}</p><p>Required tools: {installation.skill.requiredCapabilities.join(", ")||"None declared"}</p></details></div>:null}
      <p className={styles.content}>{skill.instructions}</p>
      {onUse&&installation?.enabled&&!latest.disabled&&skill.version===installation.version?<button type="button" className={styles.primary} onClick={()=>onUse(`Use the installed skill “${installation.skill.name}” to help me with `)}>Use in chat</button>:null}
      {target && canWrite ? <div className={styles.actions}>{!installation ? <button className={styles.primary} type="button" disabled={busy || latest.disabled} onClick={() => void install(skill.version, true)}>Install version {skill.version}</button> : <><button className={styles.secondary} type="button" disabled={busy || latest.disabled && !installation.enabled} onClick={() => void install(installation.version, !installation.enabled)}>{installation.enabled ? "Disable here" : "Enable here"}</button>{skill.version !== installation.version ? <button className={styles.primary} type="button" disabled={busy || latest.disabled && installation.enabled} onClick={() => void install(skill.version, installation.enabled)}>{skill.version < installation.version ? "Roll back to" : "Use"} version {skill.version}</button> : installation.version < latest.latestVersion ? <button className={styles.primary} type="button" disabled={busy || latest.disabled && installation.enabled} onClick={() => setVersion(String(latest.latestVersion))}>Review version {latest.latestVersion}</button> : null}<button className={styles.danger} type="button" disabled={busy} onClick={() => void uninstall()}>Uninstall here</button></>}</div> : null}
      {notice ? <p className={styles.note} role="status">{notice}</p> : null}
      <p className={styles.note}>Installation changes apply on the next turn. They do not grant tool access or cancel work already running.</p>
      <details className={styles.details} open={!!availability.error || availability.data?.capabilities.some(item => !item.available)}><summary>Required tools</summary><ErrorLine value={availability.error} retry={refreshAvailability} />{availability.loading ? <p className={styles.note} role="status">Checking tool availability…</p> : null}{!skill.requiredCapabilities.length ? <p className={styles.note}>This skill declares no required tools.</p> : availability.data?.capabilities.map(capability => <div className={styles.capability} key={capability.name}><span>{capability.name}<small>{capability.available ? "Available" : "Unavailable"}</small></span>{capability.reason ? <p>{capability.reason}</p> : null}</div>)}<p className={styles.note}>Availability describes the current tools. Resource permissions are checked when the Wren uses them.</p></details>
      {latest.ownerId === accountId && canWrite ? <details className={styles.details}><summary>Manage workspace skill</summary><p className={styles.note}>Changes create a new workspace version. Existing installations keep their pinned version. Disabling the skill also prevents old versions from loading.</p><button className={styles.secondary} type="button" onClick={() => { setEditSource(latest); setEditing(true); }}>Edit latest version</button></details> : null}
    </> : resource.loading || versions.loading ? <p className={styles.note} role="status">Loading skill…</p> : null : <>
      <header className={styles.heading}><h2>Skills</h2>{canWrite ? <button type="button" className={styles.secondary} onClick={() => setCreating(true)}>Create</button> : null}</header><div className={styles.tabs} role="group" aria-label="Skill lists">{target ? <button type="button" aria-pressed={view === "installed"} onClick={() => setView("installed")}>Installed here</button> : null}<button type="button" aria-pressed={view === "catalog"} onClick={() => setView("catalog")}>Workspace catalog</button></div>
      {view === "installed" && target ? <>{installed.loading ? <p className={styles.note} role="status">Loading installed skills…</p> : null}{installed.data?.installations.length === 0 ? <p className={styles.note}>No skills are installed here. Browse the workspace catalog to choose one.</p> : null}{installed.data?.installations.map(item => <button type="button" key={item.id} className={styles.row} onClick={() => open(item.skillId, item.version)}><span><strong>{item.skill.name}</strong><small>{skillInstallationLabel(item)} · version {item.version}{item.skill.latestVersion>item.version?` · Version ${item.skill.latestVersion} available`:""}</small></span></button>)}</> : <><p className={styles.note}>Reusable workflows shared with {workspaceName}. Installing one adds instructions, not new permissions.</p>{catalog.loading ? <p className={styles.note} role="status">Loading skills…</p> : null}{catalog.data?.skills.length === 0 ? <p className={styles.note}>No workspace skills have been saved yet.</p> : null}{catalog.data?.skills.map(item => <button type="button" key={item.id} className={styles.row} onClick={() => open(item.id)}><span><strong>{item.name}</strong><small>{item.description || (item.disabled ? "Disabled by author" : `Version ${item.version}`)}</small></span>{item.disabled ? <small className={styles.small}>Disabled</small> : null}</button>)}</>}
    </>}
  </>;
}

function SkillForm({ workspaceId, workspaceName, initial, readOnly, onSaved, onCancel }: { workspaceId: string; workspaceName: string; initial?: WrenSkillRecord; readOnly: boolean; onSaved: (value: WrenSkillRecord) => void; onCancel: () => void }) {
  const [draft, setDraft] = useState(() => ({ id: initial?.id ?? `skill_${crypto.randomUUID()}`, version: initial?.latestVersion ?? 0, name: initial?.name ?? "", description: initial?.description ?? "", instructions: initial?.instructions ?? "", required: initial?.requiredCapabilities.join("\n") ?? "", disabled: initial?.disabled ?? false }));
  const [busy, setBusy] = useState(false), [error, setError] = useState(""), [conflict, setConflict] = useState(false), [denied, setDenied] = useState(false);
  const base = `${workBase(workspaceId)}/wren-skills`;
  async function submit(event: FormEvent) { event.preventDefault(); if (busy || readOnly || denied) return; setBusy(true); setError(""); setConflict(false); try { if (new TextEncoder().encode(draft.instructions).length > 16000 || new TextEncoder().encode(draft.description).length > 1000) throw new Error("Keep instructions under 16000 bytes and the description under 1000 bytes."); const value = await canterFetch<WrenSkillRecord>(`${base}${initial ? `/${encodeURIComponent(draft.id)}` : ""}`, { method: initial ? "PATCH" : "POST", body: JSON.stringify({ id: draft.id, name: draft.name, description: draft.description, instructions: draft.instructions, requiredCapabilities: parseSkillCapabilities(draft.required), expectedVersion: draft.version, disabled: draft.disabled }) }); onSaved(value); } catch (cause) { setConflict(cause instanceof CanterAPIError && cause.status === 409); if (cause instanceof CanterAPIError && [401, 403, 404].includes(cause.status)) setDenied(true); setError(workError(cause, "Couldn’t save this skill.")); setBusy(false); } }
  async function reload() { setBusy(true); try { const value = await canterFetch<WrenSkillRecord>(`${base}/${encodeURIComponent(draft.id)}`); setDraft({ id: value.id, version: value.latestVersion, name: value.name, description: value.description, instructions: value.instructions, required: value.requiredCapabilities.join("\n"), disabled: value.disabled }); setConflict(false); setError(""); } catch (cause) { if (cause instanceof CanterAPIError && [401, 403, 404].includes(cause.status)) setDenied(true); setError(workError(cause, "Couldn’t load the latest skill.")); } finally { setBusy(false); } }
  return <form className={styles.form} onSubmit={submit} aria-label={initial ? "Edit workspace skill" : "New workspace skill"}><h2>{initial ? "New skill version" : "New skill"}</h2><p className={styles.audienceNotice}>Visible to everyone in {workspaceName}.</p><fieldset disabled={busy || readOnly || denied}><label>Name<input required maxLength={100} value={draft.name} onChange={event => setDraft({ ...draft, name: event.target.value })} /></label><label>Description<textarea rows={2} maxLength={1000} value={draft.description} onChange={event => setDraft({ ...draft, description: event.target.value })} /></label><label>Instructions<textarea required rows={7} maxLength={16000} value={draft.instructions} onChange={event => setDraft({ ...draft, instructions: event.target.value })} /></label><details className={styles.details}><summary>Dependencies and availability</summary><label>Required tool names<textarea rows={3} value={draft.required} onChange={event => setDraft({ ...draft, required: event.target.value })} placeholder="One tool name per line" /></label><p className={styles.note}>These describe dependencies. They do not grant tool access.</p><label className={styles.checkbox}><input type="checkbox" checked={draft.disabled} onChange={event => setDraft({ ...draft, disabled: event.target.checked })} />Disable this skill for the workspace, including installed versions</label></details></fieldset><ErrorLine value={error} />{conflict ? <div className={styles.confirm}><p>This skill changed. Your draft is kept; loading the saved version replaces it.</p><button className={styles.secondary} type="button" disabled={busy} onClick={() => void reload()}>Load latest version</button></div> : null}{readOnly || denied ? <p className={styles.note}>This draft is read-only because access changed.</p> : null}<footer><button type="button" className={styles.secondary} onClick={onCancel}>Cancel</button><button className={styles.primary} disabled={busy || readOnly || denied || !draft.name.trim() || !draft.instructions.trim()}>{busy ? "Saving…" : initial ? "Publish new version" : "Save to workspace"}</button></footer></form>;
}
