"use client";

import { useEffect, useRef, useState, useSyncExternalStore, type FormEvent } from "react";
import { useRouter } from "next/navigation";
import { OperatorTurn } from "./operator-turn";
import { OperatorComposer } from "./operator-composer";
import { GitHubRepositories } from "./github-repositories";
import { AppShell } from "./app-shell";
import { useWorkspace } from "./workspace-context";
import { WorkspaceIcon } from "./workspace-icon";
import { OperatorSurfaceView } from "./operator-surface";
import { canterFetch } from "@/lib/canter-api";
import { conversationBase, conversationDetail, isSurface, surfaceKey, surfaceLabels, type ConversationDetail, type OperatorEvent, type OperatorRun, type OperatorSurface } from "@/lib/operator-api";
import styles from "./operator-workspace.module.css";

const subscribeDraft = (onChange: () => void) => { window.addEventListener("canter-draft", onChange); return () => window.removeEventListener("canter-draft", onChange); };
const activeRun = (run?: OperatorRun | null) => !!run && ["queued", "running"].includes(run.status);

export function OperatorWorkspace({ id, githubResult }: { id?: string; githubResult?: string }) {
  const router = useRouter();
  const { data, error: workspaceError, retry: refreshWorkspace } = useWorkspace();
  const workspace = data?.workspace.id;
  const [detail, setDetail] = useState<ConversationDetail | null>(null);
  const [events, setEvents] = useState<OperatorEvent[]>([]);
  const [selected, setSelected] = useState<OperatorSurface | null>(null);
  const [panelOpen, setPanelOpen] = useState(true);
  const [inlineGitHub, setInlineGitHub] = useState(!!githubResult);
  const [fallbackDraft, setFallbackDraft] = useState("");
  const [error, setError] = useState("");
  const [connectionError, setConnectionError] = useState("");
  const [sending, setSending] = useState(false);
  const [attempt, setAttempt] = useState(0);
  const composer = useRef<HTMLTextAreaElement>(null);
  const transcript = useRef<HTMLDivElement>(null);
  const transcriptContent = useRef<HTMLDivElement>(null);
  const followScroll = useRef(true);
  const pending = useRef<{ id: string; requestId: string; message: string } | null>(null);
  const storageKey = workspace ? `canter:conversation-draft:${workspace}:${id ?? "new"}` : null;
  const running = activeRun(detail?.run);

  const draft = useSyncExternalStore(subscribeDraft, () => {
    try { return storageKey ? sessionStorage.getItem(storageKey) ?? fallbackDraft : fallbackDraft; } catch { return fallbackDraft; }
  }, () => "");

  useEffect(() => {
    if (!id || !workspace) return;
    const controller = new AbortController();
    let cursor = 0;
    let restoring = true;
    async function sync() {
      const value = await conversationDetail(workspace!, id!, controller.signal);
      if (!controller.signal.aborted) setDetail(value);
    }
    async function follow() {
      try {
        await sync();
        while (!controller.signal.aborted) {
          const result = await canterFetch<{ events: OperatorEvent[] }>(`${conversationBase(workspace!)}/${encodeURIComponent(id!)}/events?after=${cursor}`, { signal: controller.signal });
          if (controller.signal.aborted) return;
          const batch = result.events ?? [];
          if (batch.length) {
            if (!restoring && batch.some(event => event.kind === "surface" && event.data.kind === "github")) setInlineGitHub(false);
            cursor = batch[batch.length - 1].sequence;
            setEvents(current => [...current.filter(event => !batch.some(next => next.sequence === event.sequence)), ...batch].sort((a, b) => a.sequence - b.sequence));
            const surfaces = batch.filter(event => event.kind === "surface" && isSurface(event.data) && event.data.kind !== "github");
            const last = surfaces[surfaces.length - 1];
            // A newly requested view can open immediately, while a form the user
            // is editing stays in place. Every view also remains in the transcript.
            const editing = document.activeElement?.closest("[data-workspace-surface] input, [data-workspace-surface] select, [data-workspace-surface] textarea");
            if (last && !editing) {
              const surface = last.data as OperatorSurface;
              if (restoring) setSelected(current => current ?? surface); else { setSelected(surface); setPanelOpen(true); }
            }
            if (batch.some(event => event.kind === "queued" || event.kind === "finished")) { await sync(); refreshWorkspace(); }
            else if (batch.some(event => event.kind === "working")) setDetail(current => current?.run ? { ...current, run: { ...current.run, status: "running" } } : current);
          }
          if (batch.length < 300) restoring = false;
          setConnectionError("");
        }
      } catch (cause) {
        if (!controller.signal.aborted) setConnectionError(cause instanceof Error ? cause.message : "Updates were interrupted. Your work continues on the server.");
      }
    }
    void follow();
    return () => controller.abort();
  }, [id, workspace, attempt, refreshWorkspace]);

  useEffect(() => {
    if (!connectionError) return;
    const timer = setTimeout(() => setAttempt(value => value + 1), 3000);
    return () => clearTimeout(timer);
  }, [connectionError, attempt]);

  useEffect(() => {
    if (followScroll.current && transcript.current) transcript.current.scrollTop = transcript.current.scrollHeight;
  }, [events, detail, selected, panelOpen]);

  useEffect(() => {
    const observer = new ResizeObserver(() => { if (followScroll.current && transcript.current) transcript.current.scrollTop = transcript.current.scrollHeight; });
    if (transcript.current) observer.observe(transcript.current);
    if (transcriptContent.current) observer.observe(transcriptContent.current);
    return () => observer.disconnect();
  }, []);

  function editDraft(value: string) {
    setFallbackDraft(value);
    if (storageKey) { try { sessionStorage.setItem(storageKey, value); window.dispatchEvent(new Event("canter-draft")); } catch { /* Nonessential storage. */ } }
  }
  async function send(event?: FormEvent, chosenRepository?: string) {
    event?.preventDefault();
    const message = chosenRepository ? `Deploy https://github.com/${chosenRepository}` : (composer.current?.value ?? draft).trim();
    if (!workspace || !message || sending || running || !data?.agent.available) return;
    setSending(true); setError(""); followScroll.current = true;
    if (!pending.current || pending.current.message !== message) pending.current = { id: id ?? `conv_${crypto.randomUUID()}`, requestId: crypto.randomUUID(), message };
    const request = pending.current;
    try {
      const base = conversationBase(workspace);
      await canterFetch(id ? `${base}/${encodeURIComponent(id)}/messages` : base, { method: "POST", body: JSON.stringify(id ? { requestId: request.requestId, message, surface: chosenRepository ? { kind: "repository", repository: chosenRepository } : selected } : { ...request, surface: chosenRepository ? { kind: "repository", repository: chosenRepository } : selected }) });
      if (!chosenRepository) editDraft("");
      else if (!id && draft) { try { sessionStorage.setItem(`canter:conversation-draft:${workspace}:${request.id}`, draft); } catch { /* Nonessential storage. */ } }
      pending.current = null;
      refreshWorkspace();
      if (!id) router.push(`/app/conversations/${encodeURIComponent(request.id)}`);
      else setDetail(await conversationDetail(workspace, id));
    } catch (cause) { setError(cause instanceof Error ? cause.message : "Your message could not be sent. It is saved here so you can retry."); }
    finally { setSending(false); }
  }
  async function stop() {
    if (!id || !workspace) return;
    try {
      await canterFetch(`${conversationBase(workspace)}/${encodeURIComponent(id)}/stop`, { method: "POST" });
      setDetail(await conversationDetail(workspace, id));
    } catch (cause) { setError(cause instanceof Error ? cause.message : "Could not stop this response."); }
  }
  function openSurface(surface: OperatorSurface) {
    if (surface.kind === "github") { setInlineGitHub(true); setPanelOpen(false); followScroll.current = true; requestAnimationFrame(() => transcript.current?.scrollTo({ top: transcript.current.scrollHeight })); return; }
    setSelected(surface); setPanelOpen(true);
  }
  const shownSurfaces = [...new Map([...events.filter(event => event.kind === "surface" && isSurface(event.data) && event.data.kind !== "github").map(event => event.data as OperatorSurface), ...(selected ? [selected] : [])].map(surface => [surfaceKey(surface), surface])).values()];
  const githubRun = events.findLast(event => event.kind === "surface" && event.data.kind === "github")?.runId;
  const hasMessages = !!detail?.messages.length;
  const showPanel = panelOpen && !!selected;
  const github = workspace ? <GitHubRepositories inline workspaceId={workspace} conversationId={id} result={githubResult} busy={sending || running || !data?.agent.available} onDeploy={repository => send(undefined, repository)} /> : null;

  return <AppShell active="Home" agentView onNewInstruction={() => { if (id) router.push("/app"); else { editDraft(""); setSelected(null); setInlineGitHub(false); composer.current?.focus(); } }}>
    <div className={styles.workspace} data-has-surface={showPanel} data-working={running} data-empty={!id && !hasMessages && !inlineGitHub}>
      <header className={styles.conversationHeader}><span>{detail?.conversation.title ?? ""}</span><button className={styles.panelToggle} aria-label={showPanel ? "Hide right panel" : "Show right panel"} aria-expanded={showPanel} onClick={() => { if (!selected) setSelected(shownSurfaces.at(-1) ?? { kind: "apps" }); setPanelOpen(!showPanel); }}><WorkspaceIcon name="panel" /></button></header>
      <section className={styles.conversation} aria-label="Canter conversation">
        <div className={styles.transcript} ref={transcript} onWheel={event => { if (event.deltaY < 0) followScroll.current = false; }} onTouchMove={() => { followScroll.current = false; }} onKeyDown={event => { if (["ArrowUp", "PageUp", "Home"].includes(event.key)) followScroll.current = false; }} onScroll={() => { const element = transcript.current; if (element && element.scrollHeight - element.scrollTop - element.clientHeight < 50) followScroll.current = true; }}>
          <div ref={transcriptContent}>
          {id && !detail && !connectionError ? <p className={styles.note} role="status">Loading your conversation…</p> : null}
          {detail?.messages.filter(message => message.role === "user").map(message => <OperatorTurn key={message.id} message={message} answer={detail.messages.find(answer => answer.role === "assistant" && answer.runId === message.runId)} events={events.filter(event => event.runId === message.runId)} running={running && message.runId === detail.run?.id} onSelect={openSurface} inline={!inlineGitHub && githubRun === message.runId ? github : undefined} />)}
          {inlineGitHub ? github : null}
          {detail?.run?.status === "failed" ? <p className={styles.error} role="alert">{detail.run.failure || "The response failed."} You can continue below.</p> : null}
          {detail?.run?.status === "cancelled" ? <p className={styles.note}>Stopped. Completed operations remain saved.</p> : null}
          </div>
        </div>
        <div className={styles.composerArea}>
          {!id && !hasMessages ? <div className={styles.startBrand}><span className="wordmark">canter</span></div> : null}
          {workspaceError || error ? <p className={styles.error} role="alert">{error || workspaceError}{workspaceError ? <button onClick={refreshWorkspace}>Retry</button> : null}</p> : null}
          {connectionError ? <p className={styles.error} role="status">Updates disconnected. Reconnecting… <button onClick={() => setAttempt(value => value + 1)}>Retry now</button></p> : null}
          {data && !data.agent.available ? <p className={styles.error} role="alert">The workspace agent is unavailable. Ask your administrator to configure its model connection.</p> : null}
          <OperatorComposer draft={draft} onChange={editDraft} onSend={() => void send()} onStop={() => void stop()} running={running} disabled={sending || !data?.agent.available} inputRef={composer} selected={selected} model={data?.agent.model} onSelect={openSurface} />

        </div>
      </section>
      {showPanel && selected && workspace ? <aside className={styles.surface} data-workspace-surface aria-label={`${surfaceLabels[selected.kind]} view`}><header className={styles.surfaceHeader}><nav className={styles.surfaceTabs} aria-label="Workspace views">{shownSurfaces.map(surface => <button key={surfaceKey(surface)} aria-pressed={surfaceKey(surface) === surfaceKey(selected)} onClick={() => openSurface(surface)} title={surface.path ?? surface.repository ?? surfaceLabels[surface.kind]}><WorkspaceIcon name={surface.kind === "file" || surface.kind === "repository-changes" ? "file" : "panel"} width="14" height="14" /><span>{surface.path?.split("/").at(-1) ?? surface.system ?? surfaceLabels[surface.kind]}</span></button>)}</nav><button className={styles.closePanel} aria-label="Close view" onClick={() => setPanelOpen(false)}><span className={styles.backToConversation}>Back</span><WorkspaceIcon name="close" /></button></header><div className={styles.surfaceContent}><OperatorSurfaceView key={surfaceKey(selected)} surface={selected} workspaceId={workspace} onSelect={openSurface} conversationId={id} githubResult={githubResult} busy={sending || running || !data?.agent.available} onDeploy={repository => send(undefined, repository)} /></div></aside> : null}
    </div>
  </AppShell>;
}
