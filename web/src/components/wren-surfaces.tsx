"use client";
import {WrenDesktop} from "./wren-desktop";
import {WrenArtifactPDFPreview} from "./wren-artifact-pdf-preview";
import {WrenJobSteering} from "./wren-job-steering";
import {WrenDialog} from "./wren-dialog";
import {CanterSelect} from "./canter-select";
import {WrenArtifactEditor} from "./wren-library-panel";
/* eslint-disable @next/next/no-img-element -- Authenticated Blob snapshots need native image dimensions for desktop input. */
import { useCallback, useEffect, useRef, useState } from "react";
import { canterFetch, CanterAPIError } from "@/lib/canter-api";
import { activeJob, compareWorkTimestamps, displayMoney, downloadWrenBlob, fetchWrenBlob, fileSize, jobLabel, notifyWorkUpdate, useWrenResource, workBase, workError, type WrenArtifactRecord, type WrenComputerList, type WrenComputerRecord, type WrenComputerCheckpointRecord, type WrenDesktopInput, type WrenFileChunk, type WrenFileEntry, type WrenJobRecord } from "@/lib/wren-work";
import { ResponseText } from "./operator-turn";
import { useWorkspace } from "./workspace-context";
import type {WrenRoom} from "@/lib/wrens";
import { WrenAvatar } from "./wren-avatar";
import { WorkspaceIcon } from "./workspace-icon";
import styles from "./wren-surfaces.module.css";

function ErrorLine({ error, retry }: { error: string; retry?: () => void }) {return error ? <p className={styles.error} role="alert">{error}{retry ? <> <button type="button" onClick={retry}>Try again</button></> : null}</p> : null;}
export function WrenJobView({ workspaceId, id }: { workspaceId: string; id: string }) {
  const resource = useWrenResource<WrenJobRecord>(`${workBase(workspaceId)}/wren-jobs/${encodeURIComponent(id)}`, 3000, activeJob);
  const job = resource.data;
  const [stopping, setStopping] = useState(false), [error, setError] = useState("");
  async function stop() { if (stopping) return; setStopping(true); setError(""); try { await canterFetch(`${workBase(workspaceId)}/wren-jobs/${encodeURIComponent(id)}/stop`, { method: "POST", body: "{}" }); resource.refresh(); notifyWorkUpdate(workspaceId, job?.originConversationId); } catch (cause) { setError(workError(cause, "Couldn’t stop this work.")); } finally { setStopping(false); } }
  return <section className={styles.panel} aria-label="Delegated work">
    <ErrorLine error={resource.error} retry={resource.refresh} /><ErrorLine error={error} />
    {resource.loading ? <p className={styles.note} role="status">Loading work…</p> : null}
    {job ? <><header className={styles.heading}><h2>{job.title}</h2><span className={styles.status} data-active={["queued", "running"].includes(job.status)}>{jobLabel(job)}</span></header>
      <div className={styles.identity}><WrenAvatar shape={job.lead.shape} palette={job.lead.palette} size={25} /><span>Delegated by <strong>{job.lead.name}</strong></span></div>
      {job.status === "queued" ? <p className={styles.note}>This worker is queued. You can keep talking in the main conversation.</p> : job.status === "running" ? <p className={styles.note}>The worker is running separately. You can keep talking in the main conversation.</p> : null}
      {job.failure ? <p className={styles.error}>{job.failure}</p> : null}
      {job.result ? <div className={styles.result}><ResponseText text={job.result} /></div> : job.status === "completed" && !job.publishedAt ? <p className={styles.note} role="status">Saving the worker’s result…</p> : null}
      {job.cancelledAt ? <p className={styles.note}>{job.status==="stop_requested" ? "Stop requested. Automatic follow-ups are disabled; waiting for the remote process to confirm it has stopped." : "This worker has stopped. Automatic follow-ups are disabled. Operations already submitted retain their own state."}</p> : null}
      {job.result && !job.returnRunId && !job.returnSuppressed ? <p className={styles.note}>The result is saved. {job.lead.name} will receive it when the conversation is ready.</p> : null}
      {["queued", "running"].includes(job.status) ? <button type="button" className={styles.secondary} disabled={stopping} onClick={() => void stop()}>{stopping ? "Stopping…" : "Stop work"}</button> : null}
      <WrenJobSteering key={job.id} workspaceId={workspaceId} job={job} refresh={resource.refresh} />
      <details className={styles.details}><summary>Assignment</summary><p className={styles.plain}>{job.prompt}</p><time dateTime={job.createdAt}>Created {new Date(job.createdAt).toLocaleString()}</time></details>
    </> : null}
  </section>;
}

const keepComputerListCurrent = () => true;
const computerActive = (value: WrenComputerRecord) => !["deleted", "failed"].includes(value.phase);
export function WrenComputerView({ workspaceId, conversationId, id, chrome }: { chrome?:{close:()=>void;expand:()=>void;wide:boolean}; workspaceId: string; conversationId?: string; id?: string }) {
  const base = `${workBase(workspaceId)}/computers`;
  const { data: workspace } = useWorkspace();
  const list = useWrenResource<WrenComputerList>(base,5000,keepComputerListCurrent);
  const [created, setCreated] = useState<WrenComputerRecord | null>(null);
  const [woken,setWoken]=useState<{source:string;computer:WrenComputerRecord}|null>(null);
  const candidate = (woken?.source===(id??"")?woken.computer.id:undefined) ?? id ?? (created?.conversationId === conversationId ? created?.id : undefined) ?? list.data?.computers.find(item => item.conversationId === conversationId && item.phase !== "deleted")?.id;
  const resource = useWrenResource<WrenComputerRecord>(candidate ? `${base}/${encodeURIComponent(candidate)}` : "", 5000, computerActive);
  const computer = resource.data ?? (resource.error ? undefined : created?.id === candidate ? created : list.data?.computers.find(item => item.id === candidate));
  const checkpointConversation=computer?.conversationId??conversationId;
  const computerName=computer?.name??list.data?.computers.find(item=>item.conversationId===checkpointConversation)?.name;
  const checkpoints=useWrenResource<{checkpoints:WrenComputerCheckpointRecord[]}>(checkpointConversation?`${base}/checkpoints?conversationId=${encodeURIComponent(checkpointConversation)}`:"",5000,keepComputerListCurrent);
  const latestSaved=checkpoints.data?.checkpoints.find(item=>item.state==="ready"),restoring=checkpoints.data?.checkpoints.some(item=>item.state==="restoring");
  const allocationAbsent=!computer||computer.phase==="deleted";
  const wakeRooms=useWrenResource<{rooms:WrenRoom[]}>(latestSaved?.scope==="room"||checkpointConversation?.startsWith("conv_room_")?`${workBase(workspaceId)}/rooms`:"");
  const [error, setError] = useState(""), [busy, setBusy] = useState(false), [view, setView] = useState<"screen" | "files">("screen"), [confirmRemoval, setConfirmRemoval] = useState(false);
  const [control, setControl] = useState<WrenComputerRecord | null>(null);
  const [controlAction,setControlAction]=useState<"human"|"agent"|null>(null);
  const [commandReview,setCommandReview]=useState<{computerId:string;ids:string[];codingJobs:{id:string;title:string}[]}|null>(null);
  const lease = useRef<{ id: string; expiresAt: string } | null>(null);
  const current = control && computer && control.id === computer.id && compareWorkTimestamps(control.updatedAt, computer.updatedAt) >= 0 ? control : computer;
  const haveControl = !!current && !!control && current.id === control.id && current.controlExpiresAt === control.controlExpiresAt && current.controlAccountId === workspace?.account.id && Date.parse(current.controlExpiresAt ?? "") > Date.now();
  const otherControl = !!current?.controlAccountId && current.controlAccountId !== workspace?.account.id && Date.parse(current.controlExpiresAt ?? "") > Date.now();
  const [clock, setClock] = useState(0);
  useEffect(() => { const tick=()=>setClock(Date.now());tick();const timer=window.setInterval(tick,15000);return()=>window.clearInterval(timer); }, []);
  const expired = !!computer && clock > 0 && Date.parse(computer.expiresAt) <= clock;
  const ready = !expired && !!computer && computer.phase === "ready" && !computer.deleteRequested;
  const canControl = !!workspace && workspace.workspace.role !== "viewer";
  const scopeOwner = computer?.ownerId ?? (checkpointConversation?.startsWith("conv_room_") ? wakeRooms.data?.rooms.find(room=>room.conversationId===checkpointConversation)?.ownerId : workspace?.account.id);
  const canManage = canControl && scopeOwner === workspace?.account.id;
  const endsSoon = ready && clock > 0 && Date.parse(computer!.expiresAt)-clock <= 15*60*1000;
  const savingWorkspace = checkpoints.data?.checkpoints.some(item=>item.state==="creating");
  const savedThisComputer = checkpoints.data?.checkpoints.find(item=>item.state==="ready"&&(item.computerId===computer?.id||item.id===computer?.restoreCheckpointId));
  const savedCopyLimit = (checkpoints.data?.checkpoints.length ?? 0) >= 3;
  const computerOptions = useRef<HTMLDetailsElement>(null);
  function manageSavedCopies() {
    const options=computerOptions.current;if(!options)return;
    options.open=true;
    const saved=options.querySelector("details");if(saved){saved.open=true;saved.querySelector("summary")?.focus();}
  }
  const [savingNow,setSavingNow]=useState(false),[saveError,setSaveError]=useState("");
  async function saveBeforeExpiry() {
    if(!computer||!canManage||savingNow||savingWorkspace)return;
    setSavingNow(true);setSaveError("");
    try { await canterFetch(`${base}/${encodeURIComponent(computer.id)}/checkpoint`,{method:"POST",body:JSON.stringify({includeBrowser:false})});checkpoints.refresh();refresh();notifyWorkUpdate(workspaceId,computer.conversationId); }
    catch(cause){setSaveError(workError(cause,"Couldn’t save these files."));}
    finally{setSavingNow(false);}
  }
  const [screenRevision, setScreenRevision] = useState(0);
  const controlLane = useRef<Promise<unknown>>(Promise.resolve());
  function inControlLane<T>(operation:()=>Promise<T>):Promise<T> { const next=controlLane.current.catch(()=>{}).then(operation);controlLane.current=next;return next; }
  const [directory, setDirectory] = useState("/workspace"), [fileError, setFileError] = useState(""), [downloading, setDownloading] = useState("");
  const files = useWrenResource<{ files: WrenFileEntry[] }>(ready && view === "files" ? `${base}/${encodeURIComponent(computer!.id)}/files?path=${encodeURIComponent(directory)}` : "");
  const refreshResource = resource.refresh, refreshList = list.refresh, computerID = computer?.id;
  const refresh = useCallback(() => { refreshResource(); refreshList(); }, [refreshResource, refreshList]);
  const checkpointRevision=checkpoints.data?.checkpoints.map(item=>`${item.id}:${item.state}:${item.updatedAt}`).join("|")??"";
  useEffect(()=>{if(checkpointRevision)refresh();},[checkpointRevision,refresh]);
  const canWake=canManage&&(latestSaved?.scope!=="room"||wakeRooms.data?.rooms.find(room=>room.conversationId===checkpointConversation)?.ownerId===workspace?.account.id);
  async function wakeSavedWorkspace(checkpoint:WrenComputerCheckpointRecord){
    if(busy||!canWake)return;setBusy(true);setError("");
    try{const value=await canterFetch<WrenComputerRecord>(`${base}/checkpoints/${encodeURIComponent(checkpoint.id)}/wake`,{method:"POST",body:"{}"});setCreated(value);setWoken({source:id??"",computer:value});refresh();checkpoints.refresh();notifyWorkUpdate(workspaceId,value.conversationId);}
    catch(cause){setError(workError(cause,"Couldn’t wake this saved workspace."));checkpoints.refresh();throw cause;}
    finally{setBusy(false);}
  }

  useEffect(() => {
    if (!haveControl || !computerID || !ready) return;
    const endpoint = `${base}/${encodeURIComponent(computerID)}/control`;
    const session = lease.current;
    if (!session || session.id !== computerID) return;
    let active = true, released = false, renewal: Promise<void> | null = null;
    const release = () => {
      if (released) return; released = true;
      const pending = renewal;
      void (async () => {
        // Wait for our own heartbeat before releasing its exact lease. A late
        // cleanup cannot release a newer panel's control of the same account.
        if (pending) await pending.catch(() => undefined);
        if (lease.current !== session) return;
        const proof = session;
        try { const value = await inControlLane(()=>canterFetch<WrenComputerRecord>(endpoint, { method: "POST", body: JSON.stringify({ mode: "agent", expectedExpiresAt: proof.expiresAt }), keepalive: true })); if (lease.current === session) { lease.current = null; if (active) { setControl(value); refresh(); } } }
        catch { if (active && lease.current === session) { setControl(null); refresh(); } /* Otherwise the short server lease expires. */ }
      })();
    };
    const heartbeat = () => {
      if (renewal || !active || released || document.visibilityState !== "visible") return;
      if (lease.current !== session) return; const proof = session;
      renewal = (async () => {
        try { const value = await inControlLane(async()=>{const next=await canterFetch<WrenComputerRecord>(endpoint, { method: "POST", body: JSON.stringify({ mode: "human", expectedExpiresAt: proof.expiresAt }) }); if(next.controlExpiresAt)session.expiresAt=next.controlExpiresAt;return next;}); if (lease.current === session) { if (value.controlExpiresAt) session.expiresAt = value.controlExpiresAt; if (active) setControl(value); } }
        catch (cause) { if (active && lease.current === session) { setError(workError(cause, "Computer control could not be renewed.")); setControl(null); refresh(); } }
        finally { renewal = null; }
      })();
    };
    const visible = () => { if (document.visibilityState === "hidden") release(); else refresh(); };
    const timer = window.setInterval(heartbeat, 20000); document.addEventListener("visibilitychange", visible);
    return () => { active = false; window.clearInterval(timer); document.removeEventListener("visibilitychange", visible); release(); };
  }, [base, computerID, haveControl, ready, refresh]);

  async function enable() { const targetConversation = checkpointConversation; if (busy || !targetConversation) return; setBusy(true); setError(""); try { const value = await canterFetch<WrenComputerRecord>(`${base}/enable`, { method: "POST", body: JSON.stringify({ conversationId: targetConversation }) }); setCreated(value);setWoken({source:id??"",computer:value}); notifyWorkUpdate(workspaceId, targetConversation); list.refresh(); } catch (cause) { setError(workError(cause, "Couldn’t enable this computer.")); } finally { setBusy(false); } }
  async function changeControl(mode: "human" | "agent", stopCommands?:string[], stopCodingJobs?:string[]) { if (!computer || busy) return; setBusy(true); setControlAction(mode); setError(""); try { const value = await inControlLane(()=>canterFetch<WrenComputerRecord>(`${base}/${encodeURIComponent(computer.id)}/control`, { method: "POST", body: JSON.stringify({ mode, ...(stopCommands?{stopCommands}:{}), ...(stopCodingJobs?{stopCodingJobs}:{}), ...(mode === "agent" && lease.current?.id === computer.id ? { expectedExpiresAt: lease.current.expiresAt } : {}) }) })); lease.current = value.controlExpiresAt && mode === "human" ? { id: computer.id, expiresAt: value.controlExpiresAt } : null; setControl(value); setCommandReview(null); refresh(); } catch (cause) {
    const detail=cause instanceof CanterAPIError&&cause.code==="computer_work_running"?cause.details:null;
    const ids=detail&&typeof detail==="object"&&"commandIds" in detail?detail.commandIds:[];
    const jobs=detail&&typeof detail==="object"&&"codingJobs" in detail?detail.codingJobs:[];
    const commands=ids??[],codingJobs=jobs??[];
    if(detail&&Array.isArray(commands)&&Array.isArray(codingJobs)&&commands.length+codingJobs.length>0&&commands.length+codingJobs.length<=64&&commands.every(id=>typeof id==="string")&&codingJobs.every(job=>job&&typeof job==="object"&&typeof job.id==="string"&&typeof job.title==="string")){setCommandReview({computerId:computer.id,ids:commands,codingJobs});setError("");}
    else setError(workError(cause, "Computer control could not be changed."));
  } finally { setBusy(false); setControlAction(null); } }
  async function input(value: WrenDesktopInput) {
    if (!computer || !ready || !haveControl || busy) return false;
    try { return await inControlLane(async()=>{
      const proof=lease.current;
      if(!proof||proof.id!==computer.id)return false;
      await canterFetch(`${base}/${encodeURIComponent(computer.id)}/input`, { method: "POST", signal:AbortSignal.timeout(10000), body: JSON.stringify({ ...value, expectedExpiresAt: proof.expiresAt }) });
      setError("");
      return true;
    }); }
    catch(cause){setError(workError(cause,"The computer did not accept that input. Queued input was stopped."));if(cause instanceof CanterAPIError&&[401,403,409].includes(cause.status)){lease.current=null;setControl(null);}refresh();return false;}
  }
  async function download(entry: WrenFileEntry) { if (!computer || downloading) return; const path = `${directory.replace(/\/$/, "")}/${entry.name}`; setDownloading(entry.name); setFileError(""); try { const chunks: ArrayBuffer[] = []; let offset = 0, total = 0; for (;;) { const chunk = await canterFetch<WrenFileChunk>(`${base}/${encodeURIComponent(computer.id)}/file?path=${encodeURIComponent(path)}&offset=${offset}&limit=8388608`); const binary = atob(chunk.data), bytes = Uint8Array.from(binary, char => char.charCodeAt(0)); total += bytes.length; if (total > 32 * 1048576) throw new Error("This download exceeds the 32 MB browser limit. Ask your Wren to split or export a smaller file."); chunks.push(new Uint8Array(bytes).buffer); if (chunk.eof) break; if (chunk.nextOffset <= offset) throw new Error("The computer returned an incomplete file range."); offset = chunk.nextOffset; } downloadWrenBlob(new Blob(chunks), entry.name); } catch (cause) { setFileError(workError(cause, "Couldn’t download this file.")); } finally { setDownloading(""); } }
  async function remove() { if (!computer || busy) return; setBusy(true); setError(""); try { await canterFetch(`${base}/${encodeURIComponent(computer.id)}/delete`, { method: "POST", body: "{}" }); setConfirmRemoval(false); refresh(); notifyWorkUpdate(workspaceId, computer.conversationId); } catch (cause) { setError(workError(cause, "Couldn’t request computer removal.")); } finally { setBusy(false); } }
  const checkpointControls = checkpointConversation?<ComputerCheckpointControls workspaceId={workspaceId} conversationId={checkpointConversation} computer={computer??undefined} canWrite={canManage} accountId={workspace?.account.id??""} resource={checkpoints} externalBusy={busy} onChanged={refresh} onWake={wakeSavedWorkspace}/>:null;
  return <section className={`${styles.panel} ${styles.computerPanel}`} aria-label="Computer">
    <div className={styles.computerTopline}>
      <span className={styles.computerTitle}><WorkspaceIcon name="panel" width="16" height="16"/>{computerName ? `${computerName}’s computer` : "Computer"}</span><div className={styles.computerHeaderActions}>
      <details ref={computerOptions} className={styles.computerOptions} onKeyDown={event=>{if(event.key==="Escape"){event.preventDefault();event.stopPropagation();event.currentTarget.open=false;event.currentTarget.querySelector("summary")?.focus();}}} onBlur={event=>{if(event.relatedTarget&&!event.currentTarget.contains(event.relatedTarget))event.currentTarget.open=false;}}>
        <summary aria-label="Computer options"><WorkspaceIcon name="more" width="18" height="18"/></summary>
        <div className={styles.computerOptionsBody}>
          {ready ? <><button type="button" className={styles.secondary} onClick={()=>setView(view === "files" ? "screen" : "files")}>{view === "files" ? "Show screen" : "Browse files"}</button><button type="button" className={styles.secondary} onClick={()=>setScreenRevision(value=>value+1)}>Refresh screen</button></> : null}
          {computer ? <p className={styles.note}>{computer.scopeKind === "room" ? "Shared with this conversation." : "Private to this conversation."}</p> : null}
          {checkpointControls}
          {computer ? <>{computer.failure ? <p className={styles.error}>{computer.failure}</p> : null}<details className={styles.details}><summary>Computer details</summary><dl><div><dt>Compute</dt><dd>{computer.vcpus} CPU · {Math.round(computer.memoryMiB / 1024 * 10) / 10} GB memory · {computer.diskGiB} GB disk</dd></div><div><dt>Estimated allocation</dt><dd>{displayMoney(computer.estimatedCents)}</dd></div><div><dt>Monthly rate</dt><dd>{displayMoney(computer.monthlyCents)}</dd></div><div><dt>Expires</dt><dd><time dateTime={computer.expiresAt}>{new Date(computer.expiresAt).toLocaleString()}</time></dd></div></dl>{!computer.suspendSupported ? <p className={styles.note}>Live memory and running processes are not restored. Saved workspaces can recreate files and an optional browser disk profile.</p> : null}{computer.phase !== "deleted" && !computer.deleteRequested && canManage ? confirmRemoval ? <div className={styles.confirm}><p>Remove this computer? Files kept only here will be lost. Saved conversation artifacts remain available.</p><button type="button" className={styles.danger} disabled={busy} onClick={() => void remove()}>Remove computer</button><button type="button" className={styles.secondary} onClick={() => setConfirmRemoval(false)}>Keep computer</button></div> : <button type="button" className={styles.danger} onClick={() => setConfirmRemoval(true)}>Remove computer…</button> : null}</details></> : null}
        </div>
      </details>
      {chrome?<><button type="button" className={styles.icon} onClick={chrome.expand} aria-label={chrome.wide?"Restore split view":"Expand computer"}><WorkspaceIcon name={chrome.wide?"collapse":"expand"} width="16" height="16"/></button><button type="button" className={styles.icon} onClick={chrome.close} aria-label="Back to conversation"><WorkspaceIcon name="close" width="17" height="17"/></button></>:null}
      </div>
    </div>
    {!(ready && computer && view === "screen") ? <><ErrorLine error={resource.error || list.error} retry={refresh}/><ErrorLine error={error}/></> : null}
    {allocationAbsent ? <ErrorLine error={checkpoints.error || wakeRooms.error} retry={()=>{checkpoints.refresh();wakeRooms.refresh();}}/> : null}
    {endsSoon && computer ? <div className={styles.computerExpiry}>
      <div><strong>{Date.parse(computer.expiresAt)<=clock?"This computer is closing":"This computer closes at "}{Date.parse(computer.expiresAt)>clock?<time dateTime={computer.expiresAt}>{new Date(computer.expiresAt).toLocaleTimeString([],{hour:"numeric",minute:"2-digit"})}</time>:null}</strong>
        <p>{savingWorkspace?"Saving files… Keep this computer open until saving finishes.":savedThisComputer?<>Saved copy from <time dateTime={savedThisComputer.createdAt}>{new Date(savedThisComputer.createdAt).toLocaleTimeString([],{hour:"numeric",minute:"2-digit"})}</time>. Later changes need another save.</>:"Files kept only on this computer will be lost. Your conversation stays here."}</p>
        {!canManage?<p>The conversation owner can save these files.</p>:null}
        {canManage&&savedCopyLimit?<p>Three copies are saved. Remove an older copy before saving again.</p>:null}
        <ErrorLine error={saveError||checkpoints.error}/>
      </div>
      {canManage&&Date.parse(computer.expiresAt)>clock?<button type="button" className={styles.secondary} disabled={savingNow||savingWorkspace||busy||!!computer.busyReason||otherControl} onClick={()=>savedCopyLimit?manageSavedCopies():void saveBeforeExpiry()}>{savingNow||savingWorkspace?"Saving…":savedCopyLimit?"Manage copies":savedThisComputer?"Save again":"Save files"}</button>:null}
    </div>:null}
    {ready && computer ? view === "files" ? <><div className={styles.fileHeading}><button type="button" className={styles.secondary} onClick={()=>setView("screen")}>Back to screen</button><button type="button" className={styles.icon} aria-label="Refresh files" onClick={files.refresh}><WorkspaceIcon name="activity" width="15" height="15"/></button></div><div className={styles.fileView}><div className={styles.directory}><button type="button" disabled={directory === "/workspace"} onClick={() => setDirectory(value => value.slice(0, value.lastIndexOf("/")) || "/workspace")}>↑</button><span>{directory}</span></div><ErrorLine error={files.error || fileError} retry={files.refresh} />{files.loading ? <p className={styles.note} role="status">Loading files…</p> : null}{files.data?.files.length === 0 ? <p className={styles.note}>This folder is empty.</p> : null}{files.data?.files.map(entry => <button className={styles.fileRow} key={entry.name} type="button" disabled={!!downloading || entry.symlink} title={entry.symlink ? "Links cannot be opened here." : undefined} onClick={() => entry.directory ? setDirectory(`${directory.replace(/\/$/, "")}/${entry.name}`) : void download(entry)}><WorkspaceIcon name={entry.directory ? "folder" : "file"} width="16" height="16" /><span>{entry.name}</span><small>{downloading === entry.name ? "Downloading…" : entry.directory ? "Open" : fileSize(entry.size)}</small></button>)}</div></> : <div className={styles.desktopStage}>
      <WrenDesktop key={computer.id} endpoint={`${base}/${encodeURIComponent(computer.id)}`} name={computer.name} controlling={haveControl&&!busy&&!savingWorkspace} onInput={input} revision={screenRevision} notice={error || resource.error || list.error}/>
      {commandReview?.computerId===computer.id&&!haveControl?<WrenDialog title="Take over this computer" showTitle onClose={()=>{if(!busy)setCommandReview(null);}}><p className={styles.note}>Work may still be running on this computer. Stop this work before taking control? Changes already made will remain.</p>{commandReview.codingJobs.length?<ul className={styles.note}>{commandReview.codingJobs.map(job=><li key={job.id}>{job.title}</li>)}</ul>:null}{commandReview.ids.length?<p className={styles.note}>{commandReview.ids.length===1?"1 command":`${commandReview.ids.length} commands`}</p>:null}<ErrorLine error={error}/><div className={styles.artifactActions}><button type="button" className={styles.primary} disabled={busy||otherControl} onClick={()=>void changeControl("human",commandReview.ids,commandReview.codingJobs.map(job=>job.id))}>{busy?"Stopping work…":"Stop work and take over"}</button><button type="button" className={styles.secondary} disabled={busy} onClick={()=>setCommandReview(null)}>Keep watching</button></div></WrenDialog>:null}
      <div className={styles.takeover}><span>{savingWorkspace ? "Saving files…" : haveControl ? "You have control" : otherControl ? `${current?.controlName || "Another person"} has control` : current?.busyReason ? "A Wren is using the computer" : (current?.unsettledCommands || current?.unsettledCodingWork) ? "Work may still be running" : "Ready for your Wrens"}</span><button type="button" className={styles.primary} disabled={busy || savingWorkspace || !canControl || otherControl} aria-busy={!!controlAction} onClick={()=>void changeControl(haveControl ? "agent" : "human")}>{controlAction === "human" ? "Taking over…" : controlAction === "agent" ? "Giving back…" : haveControl ? "Give back control" : "Take over"}</button></div>

    </div> : <div className={styles.computerEmpty}>
      <WorkspaceIcon name="panel" width="32" height="32"/>
      {restoring || (!!computer && !["failed","deleted"].includes(computer.phase)) || resource.loading || list.loading || checkpoints.loading ? <><h2>{computer?.deleteRequested || expired ? "Closing…" : "Getting ready…"}</h2><p role="status">You can keep chatting.</p></> : latestSaved ? <><h2>Your saved files are ready</h2><p>{latestSaved.fileCount} files saved on {new Date(latestSaved.createdAt).toLocaleString()}. Open a new computer to restore them. Running apps will need to be started again.</p>{canWake ? <button type="button" className={styles.primary} disabled={busy} onClick={()=>void wakeSavedWorkspace(latestSaved).catch(()=>{})}>{busy ? "Opening…" : "Restore computer"}</button> : <p>The conversation owner can restore this computer.</p>}</> : <><h2>{computer?.failure ? "Couldn’t open the computer" : "A place to work together"}</h2><p>{computer?.failure ? "This computer stopped during setup. Your conversation and published files are safe. Details and removal are in Computer options." : "Watch your Wren work, or take over when you need to."}</p>{list.data?.enabled && checkpointConversation && canManage ? <button type="button" className={styles.primary} disabled={busy} onClick={()=>computer?.phase==="failed"?refresh():void enable()}>{busy ? "Opening…" : computer?.phase==="failed" ? "Check again" : "Open computer"}</button> : <p className={styles.note}>{!canManage ? "The conversation owner can open this computer." : "Computers aren’t available in this workspace yet."}</p>}</>}
    </div>}
  </section>;

}

export function WrenArtifactView({ workspaceId, id }: { workspaceId: string; id: string }) {
  const base = `${workBase(workspaceId)}/wren-artifacts/${encodeURIComponent(id)}`;
  const [revision, setRevision] = useState<string>("");
  const query = revision ? `?revision=${encodeURIComponent(revision)}` : "";
  const resource = useWrenResource<WrenArtifactRecord>(`${base}${query}`), revisions = useWrenResource<{ revisions: WrenArtifactRecord[] }>(`${base}/revisions`);
  const artifact = resource.data;
  const [preview, setPreview] = useState<{ key: string; text?: string; image?: string; truncated?: boolean } | null>(null), [error, setError] = useState(""), [downloading, setDownloading] = useState(false);
  const previewKey = artifact ? `${artifact.id}:${artifact.revision}:${artifact.sha256}` : "";
  const textMedia = !!artifact && (artifact.mediaType.startsWith("text/") || ["application/json", "application/xml"].includes(artifact.mediaType));
  const imageMedia = !!artifact && ["image/png", "image/jpeg", "image/webp", "image/gif"].includes(artifact.mediaType);
  const editableMedia = !!artifact && (artifact.mediaType.startsWith("text/") || ["application/json", "application/yaml", "application/x-yaml"].includes(artifact.mediaType));
  const generationRecord = artifact?.provenance.summary?.match(/^Image generation operation (media_[A-Za-z0-9_-]+)$/)?.[1];
  useEffect(() => { if (!preview?.image) return; return () => URL.revokeObjectURL(preview.image!); }, [preview?.image]);
  useEffect(() => {
    if (!artifact || !textMedia && !imageMedia) return;
    const controller = new AbortController(), key = previewKey;
    async function load() { try { const blob = await fetchWrenBlob(`${base}/content?revision=${artifact!.revision}`, controller.signal); if (controller.signal.aborted) return; if (textMedia) { const text = await blob.text(); if (!controller.signal.aborted) { setPreview({ key, text: text.slice(0, 256000), truncated: text.length > 256000 }); setError(""); } } else { const image = URL.createObjectURL(blob); if (controller.signal.aborted) URL.revokeObjectURL(image); else { setPreview({ key, image }); setError(""); } } } catch (cause) { if (!controller.signal.aborted) setError(workError(cause, "Couldn’t preview this file.")); } }
    void load(); return () => controller.abort();
  }, [base, previewKey, textMedia, imageMedia, artifact]);
  async function download() { if (!artifact || downloading) return; setDownloading(true); setError(""); try { downloadWrenBlob(await fetchWrenBlob(`${base}/content?revision=${artifact.revision}`), artifact.name); } catch (cause) { setError(workError(cause, "Couldn’t download this file.")); } finally { setDownloading(false); } }
  return <section className={styles.panel} aria-label="Saved file">
    <ErrorLine error={resource.error || revisions.error} retry={() => { resource.refresh(); revisions.refresh(); }} /><ErrorLine error={error} />
    {resource.loading ? <p className={styles.note} role="status">Loading file…</p> : null}
    {artifact ? <><header className={styles.heading}><h2>{artifact.name}</h2><span className={styles.status}>{fileSize(artifact.size)}</span></header><div className={styles.artifactActions}><button className={styles.primary} type="button" disabled={downloading} onClick={() => void download()}><WorkspaceIcon name="file" width="14" height="14" />{downloading ? "Downloading…" : "Download"}</button><label>Version<CanterSelect ariaLabel="File version" value={revision} onChange={setRevision} options={[{value:"",label:"Latest"},...(revisions.data?.revisions.map(item=>({value:item.revision,label:`Version ${item.revision}`}))??[])]}/></label></div>
      {artifact.mediaType==="application/pdf"?<WrenArtifactPDFPreview workspaceId={workspaceId} artifact={artifact}/>:preview?.key === previewKey && preview.text !== undefined ? <>{artifact.mediaType === "text/markdown" ? <div className={styles.result}><ResponseText text={preview.text} /></div> : <pre className={styles.textPreview}>{preview.text}</pre>}{preview.truncated ? <p className={styles.note}>This preview is shortened. Download the file to read it all.</p> : null}</> : preview?.key === previewKey && preview.image ? <div className={styles.imagePreview}><img src={preview.image} alt={artifact.name} /></div> : textMedia || imageMedia ? !error ? <p className={styles.note} role="status">Preparing preview…</p> : null : <p className={styles.note}>Download this file to open it in its application.</p>}
      {editableMedia ? <WrenArtifactEditor workspaceId={workspaceId} id={id} revision={revision?Number(revision):undefined} onSaved={()=>{resource.refresh();revisions.refresh();}}/> : null}
      {artifact.provenance.summary && !generationRecord ? <p className={styles.note}>{artifact.provenance.summary}</p> : null}
      <details className={styles.details}><summary>File details</summary><dl>{generationRecord ? <div><dt>Generation record</dt><dd>{generationRecord}</dd></div> : null}<div><dt>Format</dt><dd>{artifact.mediaType}</dd></div><div><dt>Version</dt><dd>{artifact.revision}</dd></div><div><dt>Updated</dt><dd><time dateTime={artifact.updatedAt}>{new Date(artifact.updatedAt).toLocaleString()}</time></dd></div></dl>{artifact.provenance.sources?.length ? <div className={styles.sources}><p>Sources</p>{artifact.provenance.sources.map((source, index) => /^https?:\/\//i.test(source) ? <a key={index} href={source} target="_blank" rel="noreferrer">{source}</a> : <p key={index} className={styles.plain}>{source}</p>)}</div> : null}</details>
    </> : null}
  </section>;
}

const checkpointLabel=(state:WrenComputerCheckpointRecord["state"])=>({creating:"Saving…",ready:"Saved",restoring:"Restoring files…",deleting:"Deleting saved copy…",failed:"Save failed"})[state];
function ComputerCheckpointControls({workspaceId,conversationId,computer,canWrite,accountId,onChanged,onWake,resource,externalBusy}:{workspaceId:string;conversationId:string;computer?:WrenComputerRecord;canWrite:boolean;accountId:string;onChanged:()=>void;onWake:(value:WrenComputerCheckpointRecord)=>Promise<void>;resource:{data?:{checkpoints:WrenComputerCheckpointRecord[]};loading:boolean;error:string;refresh:()=>void};externalBusy:boolean}){
 const base=`${workBase(workspaceId)}/computers`;
 const shared=computer?.scopeKind==="room"||resource.data?.checkpoints.some(item=>item.scope==="room"),rooms=useWrenResource<{rooms:WrenRoom[]}>(shared?`${workBase(workspaceId)}/rooms`:"");
 const scopeOwner=shared?rooms.data?.rooms.find(room=>room.conversationId===conversationId)?.ownerId===accountId:computer?computer.ownerId===accountId:true;
 const allowed=canWrite&&scopeOwner,active=!!computer&&computer.phase!=="deleted",pending=resource.data?.checkpoints.some(item=>["creating","restoring","deleting"].includes(item.state)),full=(resource.data?.checkpoints.length??0)>=3;
 const [browser,setBrowser]=useState(false),[busy,setBusy]=useState(false),[sleepReview,setSleepReview]=useState(false),[error,setError]=useState(""),[notice,setNotice]=useState(""),[removeReview,setRemoveReview]=useState<WrenComputerCheckpointRecord|null>(null);
 const refresh=resource.refresh;
 useEffect(()=>{const changed=(event:Event)=>{const detail=(event as CustomEvent<{workspaceId:string;conversationId?:string}>).detail;if(detail?.workspaceId===workspaceId&&(!detail.conversationId||detail.conversationId===conversationId))refresh();};window.addEventListener("canter-wren-work-updated",changed);return()=>window.removeEventListener("canter-wren-work-updated",changed);},[workspaceId,conversationId,refresh]);
 async function save(sleep:boolean){if(!computer||busy||!allowed)return;setBusy(true);setError("");setNotice("");try{await canterFetch<WrenComputerCheckpointRecord>(`${base}/${encodeURIComponent(computer.id)}/${sleep?"sleep":"checkpoint"}`,{method:"POST",body:JSON.stringify({includeBrowser:browser})});setSleepReview(false);setNotice(sleep?"Saving requested. The computer stays allocated until its encrypted checkpoint has been verified, then removal begins.":"Saving requested. This keeps the current computer allocated.");refresh();onChanged();notifyWorkUpdate(workspaceId,conversationId);}catch(cause){setError(workError(cause,"Couldn’t save this workspace."));refresh();}finally{setBusy(false);}}
 async function wake(checkpoint:WrenComputerCheckpointRecord){if(busy||externalBusy||!allowed)return;setBusy(true);setError("");setNotice("");try{await onWake(checkpoint);setNotice("Wake requested. A computer is being prepared from these saved files.");refresh();}catch(cause){setError(workError(cause,"Couldn’t wake this saved workspace."));}finally{setBusy(false);}}
 async function removeCheckpoint(){if(!removeReview||busy||externalBusy||!allowed)return;setBusy(true);setError("");try{await canterFetch(`${base}/checkpoints/${encodeURIComponent(removeReview.id)}/delete`,{method:"POST",body:"{}"});setRemoveReview(null);setNotice("Saved-copy removal requested.");refresh();notifyWorkUpdate(workspaceId,conversationId);}catch(cause){setError(workError(cause,"Couldn’t confirm saved-copy removal."));refresh();}finally{setBusy(false);}}

 return <details className={styles.details}><summary>{pending?"Updating saved workspace…":active?"Saved workspace · sleep and wake":"Other saved workspaces"}</summary><p className={styles.note}>Save files from /workspace. Wake creates a computer and restores those files; it does not resume memory or running processes.</p><ErrorLine error={resource.error||rooms.error||error} retry={()=>{refresh();rooms.refresh();}}/>{computer?.phase==="ready"&&!computer.deleteRequested&&allowed?<><label className={styles.checkpointBrowser}><input type="checkbox" checked={browser} disabled={busy||externalBusy||pending} onChange={event=>setBrowser(event.target.checked)}/>Include the browser’s saved profile</label>{browser?<p className={styles.note}>{shared?"This profile can contain signed-in browser sessions and stays in this shared room’s scope.":"The profile stays in this private conversation’s scope."} You may need to sign in again after waking.</p>:null}{computer.busyReason?<p className={styles.note}>{computer.busyReason} Stop active computer work before saving.</p>:null}<div className={styles.artifactActions}><button type="button" className={styles.secondary} disabled={busy||externalBusy||pending||full||!!computer.busyReason} onClick={()=>void save(false)}>Save workspace</button><button type="button" className={styles.secondary} disabled={busy||externalBusy||pending||full||!!computer.busyReason} onClick={()=>setSleepReview(true)}>Sleep…</button></div>{full?<p className={styles.note}>Three copies are saved. Remove an older copy below before saving again.</p>:null}{sleepReview?<div className={styles.confirm}><p>Save this workspace and remove its compute allocation? Removal starts only after encrypted storage verification. If saving fails, the computer is kept. Saved conversation artifacts remain available.</p><button type="button" className={styles.secondary} disabled={busy||externalBusy||pending} onClick={()=>void save(true)}>Save and sleep computer</button><button type="button" className={styles.secondary} disabled={busy} onClick={()=>setSleepReview(false)}>Keep computer awake</button></div>:null}</>:!allowed?<p className={styles.note}>{canWrite?"The current conversation owner manages saved workspaces and compute.":"Your workspace role gives read-only access to saved workspaces."}</p>:null}
 {resource.loading?<p className={styles.note} role="status">Loading saved workspaces…</p>:null}{resource.data?.checkpoints.map(checkpoint=><div className={styles.checkpointRow} key={checkpoint.id}><strong>{checkpointLabel(checkpoint.state)} · {new Date(checkpoint.createdAt).toLocaleString()}</strong>{checkpoint.state==="ready"||checkpoint.state==="restoring"?<p className={styles.note}>{checkpoint.fileCount} saved files · {fileSize(checkpoint.rawBytes)} · {checkpoint.includeBrowser?"Browser profile included":"Files only"}</p>:null}{checkpoint.failure?<p className={styles.error}>{checkpoint.failure}</p>:null}{checkpoint.sleepAfterSaving&&checkpoint.computerId===computer?.id?<p className={styles.note}>{computer.phase==="deleted"?"The original compute allocation was removed. This saved workspace can be used to wake a new computer.":computer.deleteRequested?"Saved workspace verified; compute removal is in progress.":checkpoint.state==="creating"?"The original computer stays allocated while saving is verified.":"Sleep was requested. Check the computer’s actual removal state."}</p>:null}{allowed&&checkpoint.state==="ready"?<button type="button" className={styles.secondary} disabled={busy||externalBusy||active||pending} onClick={()=>void wake(checkpoint)}>{active?"Computer already allocated":"Wake from these files"}</button>:null}{allowed&&!(["creating","restoring","deleting"].includes(checkpoint.state))?<button type="button" className={styles.icon} aria-label={`Remove saved workspace from ${new Date(checkpoint.createdAt).toLocaleString()}`} title="Remove saved copy" disabled={busy||externalBusy} onClick={()=>{setError("");setRemoveReview(checkpoint);}}><WorkspaceIcon name="trash" width="15" height="15"/></button>:null}</div>)}{resource.data?.checkpoints.length===0?<p className={styles.note}>No saved workspaces yet.</p>:null}{notice?<p className={styles.note} role="status">{notice}</p>:null}{removeReview?<WrenDialog title="Remove saved workspace" showTitle onClose={()=>setRemoveReview(null)}><p className={styles.note}>Permanently remove the saved copy from {new Date(removeReview.createdAt).toLocaleString()}? Files stored only in this copy cannot be recovered. This does not remove a running computer or saved conversation artifacts.</p><ErrorLine error={error}/><div className={styles.artifactActions}><button type="button" className={styles.danger} disabled={busy} onClick={()=>void removeCheckpoint()}>Remove saved copy</button><button type="button" className={styles.secondary} disabled={busy} onClick={()=>setRemoveReview(null)}>Cancel</button></div></WrenDialog>:null}</details>;
}
