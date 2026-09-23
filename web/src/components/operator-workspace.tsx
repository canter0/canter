"use client";

import { useEffect, useLayoutEffect, useId, useRef, useState, useSyncExternalStore, type FormEvent } from "react";
import { useRouter } from "next/navigation";
import { useOperatorAttachmentDraft } from "@/lib/operator-attachment-draft";
import { OperatorMessageContext, OperatorTurn } from "./operator-turn";
import { WorkspaceLoading } from "./workspace-loading";
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
  const [opened, setOpened] = useState<OperatorSurface[]>([]);
  const [wide, setWide] = useState(false);
  const [viewMenu, setViewMenu] = useState(false);
  const [showScroll, setShowScroll] = useState(false);
  const [panelOpen, setPanelOpen] = useState<boolean | null>(null);
  const panelId = useId();
  const [submittedPrompt, setSubmittedPrompt] = useState("");
  const [submittedSurface, setSubmittedSurface] = useState<OperatorSurface | null>(null);
  const composerArea = useRef<HTMLDivElement>(null);
  const composerOrigin = useRef<DOMRect | null>(null);
  const composerMotion = useRef<Animation | null>(null);
  const [inlineGitHub, setInlineGitHub] = useState(!!githubResult);
  const [fallbackDraft, setFallbackDraft] = useState("");
  const [error, setError] = useState("");
  const [connectionError, setConnectionError] = useState("");
  const [sending, setSending] = useState(false);
  const [attempt, setAttempt] = useState(0);
  const [attachedContext, setAttachedContext] = useState<OperatorSurface | null | undefined>(undefined);
  const composer = useRef<HTMLTextAreaElement>(null);
  const viewMenuAnchor = useRef<HTMLDivElement>(null);
  const transcript = useRef<HTMLDivElement>(null);
  const transcriptContent = useRef<HTMLDivElement>(null);
  const followScroll = useRef(true);
  const pending = useRef<{ id: string; requestId: string; message: string; signature: string } | null>(null);
  const storageKey = workspace ? `canter:conversation-draft:${workspace}:${id ?? "new"}` : null;
  const running = activeRun(detail?.run);
  const attachmentDraft = useOperatorAttachmentDraft(storageKey);

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
            const surfaces = batch.filter(event => event.kind === "surface" && isSurface(event.data) && !["github", "compute", "storage"].includes(String(event.data.kind)));
            const last = surfaces[surfaces.length - 1];
            // A newly requested view can open immediately, while a form the user
            // is editing stays in place. Every view also remains in the transcript.
            const editing = document.activeElement?.closest("[data-workspace-surface] input, [data-workspace-surface] select, [data-workspace-surface] textarea");
            if (last && !editing) {
              const surface = last.data as OperatorSurface;
              setOpened(current => [...new Map([...current, ...surfaces.map(event => event.data as OperatorSurface)].map(item => [surfaceKey(item), item])).values()]);
              if (restoring) setSelected(current => current ?? surface); else { setSelected(surface); setPanelOpen(true); }
            }
            if (batch.some(event => event.kind === "queued" || event.kind === "finished" || event.kind === "title")) { await sync(); refreshWorkspace(); }
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

  useEffect(() => {
    if (selected && (panelOpen ?? true)) {
      const index = opened.findIndex(item => surfaceKey(item) === surfaceKey(selected));
      const tab = document.getElementById(`${panelId}-tab-${index}`);
      const reveal = () => tab?.scrollIntoView({ block: "nearest", inline: "nearest" });
      reveal();
      const observer = new ResizeObserver(reveal);
      const tablist = tab?.closest('[role="tablist"]');
      if (tablist) observer.observe(tablist);
      return () => observer.disconnect();
    }
  }, [selected, opened, panelOpen, panelId]);

  useEffect(() => {
    if (!viewMenu) return;
    const dismiss = (event: PointerEvent) => { if (event.target instanceof Node && !viewMenuAnchor.current?.contains(event.target)) setViewMenu(false); };
    const escape = (event: KeyboardEvent) => { if (event.key === "Escape") { setViewMenu(false); viewMenuAnchor.current?.querySelector("button")?.focus(); } };
    document.addEventListener("pointerdown", dismiss); document.addEventListener("keydown", escape);
    return () => { document.removeEventListener("pointerdown", dismiss); document.removeEventListener("keydown", escape); };
  }, [viewMenu]);

  useLayoutEffect(() => {
    const origin = composerOrigin.current;
    const element = composerArea.current;
    if (!origin || !element) return;
    composerOrigin.current = null;
    if (window.matchMedia("(prefers-reduced-motion: reduce)").matches) return;
    const destination = element.getBoundingClientRect();
    composerMotion.current = element.animate([{ transform: `translateY(${origin.top - destination.top}px)` }, { transform: "translateY(0)" }], { duration: 520, easing: "cubic-bezier(.22,1,.36,1)" });
  }, [submittedPrompt]);

  function suggestPrompt(kind: "compute" | "storage" | "github") {
    const prompts = {
      compute: "Help me plan one or more VPSs. If I have not specified a count, assume one and label that assumption. Give me a useful starter configuration in CPU, RAM, disk, and OS terms, with Canter's monthly compute estimate—not internal size labels. When I describe an outcome instead of choosing infrastructure, compare managed app hosting with VM control only if that choice matters. Ask only what changes the plan, and make that question easy to spot.",
      storage: "Help me create a storage bucket. Ask me what I want to store, then guide me through the requirements one step at a time.",
      github: "Help me connect GitHub and choose a repository to work on.",
    };
    editDraft(draft.trim() ? `${draft.trim()}\n\n${prompts[kind]}` : prompts[kind]);
    setAttachedContext(null);
    requestAnimationFrame(() => { composer.current?.focus(); composer.current?.setSelectionRange(composer.current.value.length, composer.current.value.length); });
  }

  function editDraft(value: string) {
    setFallbackDraft(value);
    if (storageKey) { try { sessionStorage.setItem(storageKey, value); window.dispatchEvent(new Event("canter-draft")); } catch { /* Nonessential storage. */ } }
  }
  async function send(event?: FormEvent, chosenRepository?: string) {
    event?.preventDefault();
    const message = (chosenRepository ? `Deploy https://github.com/${chosenRepository}` : ((composer.current?.value ?? draft).trim() || (attachmentDraft.items.length ? "Please review the attached files." : "")));
    const attachments = chosenRepository ? [] : attachmentDraft.items;
    const requestSurface: OperatorSurface | null = chosenRepository ? { kind: "repository", repository: chosenRepository } : (attachedContext === undefined ? selected : attachedContext);
    const signature = JSON.stringify({ message, attachments, surface: requestSurface });
    if (!workspace || !message || sending || !data?.agent.available) return false;
    composerOrigin.current = composerArea.current?.getBoundingClientRect() ?? null;
    setSubmittedPrompt(message);
    setSubmittedSurface(requestSurface);
    setSending(true); setError(""); followScroll.current = true;
    if (!pending.current || pending.current.signature !== signature) pending.current = { id: id ?? `conv_${crypto.randomUUID()}`, requestId: crypto.randomUUID(), message, signature };
    const { id: requestConversationId, requestId } = pending.current;
    const request = { id: requestConversationId, requestId, message, attachments };
    try {
      const base = conversationBase(workspace);
      await canterFetch(id ? `${base}/${encodeURIComponent(id)}/messages` : base, { method: "POST", body: JSON.stringify(id ? { requestId: request.requestId, message, attachments, surface: requestSurface } : { ...request, surface: requestSurface }) });
      if (!chosenRepository) { editDraft(""); attachmentDraft.update([]); }
      else if (!id) {
        if (draft) { try { sessionStorage.setItem(`canter:conversation-draft:${workspace}:${request.id}`, draft); } catch { /* Nonessential storage. */ } }
        if (attachmentDraft.items.length) attachmentDraft.update(attachmentDraft.items, `canter:conversation-draft:${workspace}:${request.id}`);
      }
      pending.current = null;
      refreshWorkspace();
      if (!id) {
        const destination = `/app/conversations/${encodeURIComponent(request.id)}`;
        router.prefetch(destination);
        await composerMotion.current?.finished.catch(() => {});
        router.push(destination);
      }
      else setDetail(await conversationDetail(workspace, id));
      return true;
    } catch (cause) { setSubmittedPrompt(""); setSubmittedSurface(null); setError(cause instanceof Error ? cause.message : "Your message could not be sent. It is saved here so you can retry."); return false; }
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
    if (surface.kind === "compute" || surface.kind === "storage") { suggestPrompt(surface.kind); return; }
    if (surface.kind === "github") { setInlineGitHub(true); setPanelOpen(false); followScroll.current = true; requestAnimationFrame(() => transcript.current?.scrollTo({ top: transcript.current.scrollHeight })); return; }
    setOpened(current => current.some(item => surfaceKey(item) === surfaceKey(surface)) ? current : [...current, surface]);
    setSelected(surface); setPanelOpen(true); setViewMenu(false);
  }
  function closeSurface(surface: OperatorSurface) {
    const remaining = opened.filter(item => surfaceKey(item) !== surfaceKey(surface));
    setOpened(remaining);
    if (selected && surfaceKey(selected) === surfaceKey(surface)) setSelected(remaining.at(-1) ?? null);
    if (!remaining.length) { setPanelOpen(false); setWide(false); }
  }
  const tabLabel = (surface: OperatorSurface) => surface.path?.split("/").at(-1) ?? (surface.kind === "repository" ? surface.repository?.split("/").at(-1) : undefined) ?? surface.system ?? surfaceLabels[surface.kind];
  const githubRun = events.findLast(event => event.kind === "surface" && event.data.kind === "github")?.runId;
  const hasMessages = !!detail?.messages.length;
  const showPanel = panelOpen ?? !!selected;
  const github = workspace ? <GitHubRepositories inline workspaceId={workspace} conversationId={id} result={githubResult} busy={sending || !data?.agent.available} onDeploy={async repository => { await send(undefined, repository); }} /> : null;

  return <AppShell active="Home" agentView onNewInstruction={() => { if (id) router.push("/app"); else { setSubmittedPrompt(""); setSubmittedSurface(null); editDraft(""); attachmentDraft.update([]); setAttachedContext(null); setOpened([]); setSelected(null); setPanelOpen(null); setInlineGitHub(false); composer.current?.focus(); } }}>
    <div className={styles.workspace} data-has-surface={showPanel} data-working={running} data-wide={wide && showPanel} data-empty={!id && !hasMessages && !inlineGitHub && !submittedPrompt}>
      <div className={styles.conversationPane} inert={wide && showPanel}>
        <header className={styles.conversationHeader}><span>{detail?.conversation.title ?? ""}</span></header>
        <section className={styles.conversation} aria-label="Canter conversation">
          <div className={styles.transcript} ref={transcript} onWheel={event => { if (event.deltaY < 0) followScroll.current = false; }} onTouchMove={() => { followScroll.current = false; }} onKeyDown={event => { if (["ArrowUp", "PageUp", "Home"].includes(event.key)) followScroll.current = false; }} onScroll={() => { const element = transcript.current; if (element) { const away = element.scrollHeight - element.scrollTop - element.clientHeight >= 80; setShowScroll(away); if (!away) followScroll.current = true; } }}>
            <div ref={transcriptContent}>
            {id && !detail && !connectionError ? <WorkspaceLoading variant="conversation" /> : null}
            {detail?.messages.filter(message => message.role === "user").map(message => <OperatorTurn key={message.id} message={message} answer={detail.messages.find(answer => answer.role === "assistant" && answer.runId === message.runId)} events={events.filter(event => event.runId === message.runId)} running={running && message.runId === detail.run?.id} onSelect={openSurface} conversations={data?.conversations ?? []} inline={<>{!inlineGitHub && githubRun === message.runId ? github : null}</>} />)}
            {inlineGitHub ? github : null}
            {submittedPrompt && !hasMessages ? <section className={styles.turn}><article className={styles.message} data-role="user"><div className={styles.messageText}>{submittedPrompt}</div><OperatorMessageContext surface={submittedSurface} conversations={data?.conversations ?? []} /></article><div className={styles.progress} role="status"><span className={styles.pulse} />Starting your conversation…</div></section> : null}
            {detail?.run?.status === "failed" ? <p className={styles.error} role="alert">{detail.run.failure || "The response failed."} You can continue below.</p> : null}
            {detail?.run?.status === "cancelled" ? <p className={styles.note}>Stopped. Completed operations remain saved.</p> : null}
            </div>
          </div>
          <div className={styles.composerArea} ref={composerArea}>
            {showScroll ? <button type="button" className={styles.scrollLatest} aria-label="Scroll to latest message" onClick={() => { followScroll.current = true; transcript.current?.scrollTo({ top: transcript.current.scrollHeight, behavior: "smooth" }); }}><WorkspaceIcon name="down" width="16" height="16" /></button> : null}
            {!id && !hasMessages && !submittedPrompt ? <div className={styles.startBrand}><span className="wordmark">canter</span></div> : null}
            {workspaceError || error ? <p className={styles.error} role="alert">{error || workspaceError}{workspaceError ? <button onClick={refreshWorkspace}>Retry</button> : null}</p> : null}
            {connectionError ? <p className={styles.error} role="status">Updates disconnected. Reconnecting… <button onClick={() => setAttempt(value => value + 1)}>Retry now</button></p> : null}
            {data && !data.agent.available ? <p className={styles.error} role="alert">The workspace agent is unavailable. Ask your administrator to configure its model connection.</p> : null}
            {attachmentDraft.error ? <p className={styles.error} role="status">{attachmentDraft.error}</p> : null}
            <OperatorComposer attachments={attachmentDraft.items} onAttachments={attachmentDraft.update} sending={sending} context={(attachedContext === undefined ? selected : attachedContext)} onClearContext={() => setAttachedContext(null)} draft={draft} onChange={editDraft} onSend={() => void send()} onStop={() => void stop()} running={running} disabled={sending || !data?.agent.available || !attachmentDraft.loaded} inputRef={composer} model={data?.agent.model} onSelect={surface => { setAttachedContext(surface); if (surface.kind === "github") openSurface(surface); }} />
            {!id && !hasMessages && !submittedPrompt ? <div className={styles.starters} aria-label="Try a workspace action"><button onClick={() => suggestPrompt("compute")}>Plan a VPS</button><button onClick={() => suggestPrompt("storage")}>Create a bucket</button><button onClick={() => suggestPrompt("github")}>Connect GitHub</button></div> : null}

          </div>
        </section>
      </div>
      <aside id={panelId} className={styles.surface} data-workspace-surface data-empty={!selected} aria-label={selected ? `${surfaceLabels[selected.kind]} view` : "Workspace view"} aria-hidden={!showPanel} inert={!showPanel}>
        <div className={styles.surfaceToolbar}>
          <div className={styles.surfaceTabs} role="tablist" aria-label="Workspace tabs">{opened.map((surface, index) => <div className={styles.surfaceTab} key={surfaceKey(surface)} data-active={!!selected && surfaceKey(selected) === surfaceKey(surface)}>
            <button type="button" role="tab" id={`${panelId}-tab-${index}`} aria-controls={`${panelId}-view-${index}`} aria-selected={!!selected && surfaceKey(selected) === surfaceKey(surface)} tabIndex={selected && surfaceKey(selected) === surfaceKey(surface) ? 0 : -1} title={surface.path ?? surface.repository ?? tabLabel(surface)} onClick={() => setSelected(surface)} onKeyDown={event => { if (["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) { event.preventDefault(); const next = event.key === "Home" ? 0 : event.key === "End" ? opened.length - 1 : (index + (event.key === "ArrowRight" ? 1 : -1) + opened.length) % opened.length; setSelected(opened[next]); document.getElementById(`${panelId}-tab-${next}`)?.focus(); } if (event.key === "Delete") { closeSurface(surface); requestAnimationFrame(() => document.getElementById(`${panelId}-tab-${Math.max(0, index - 1)}`)?.focus()); } }}><WorkspaceIcon name={surface.kind === "repository-changes" ? "changes" : surface.kind === "file" ? "file" : "folder"} width="14" height="14" /><span>{tabLabel(surface)}</span></button>
            <button type="button" className={styles.closeTab} aria-label={`Close ${tabLabel(surface)} tab`} onClick={() => closeSurface(surface)}><WorkspaceIcon name="close" width="12" height="12" /></button>
          </div>)}</div>
          <div className={styles.surfaceTools}>
            <div ref={viewMenuAnchor} className={styles.menuAnchor}><button type="button" className={styles.surfaceTool} aria-label="Open workspace view" aria-expanded={viewMenu} onClick={() => setViewMenu(!viewMenu)}><WorkspaceIcon name="plus" width="17" height="17" /></button>{viewMenu ? <div className={styles.viewMenu} onBlur={event => { if (!event.currentTarget.contains(event.relatedTarget)) setViewMenu(false); }} onKeyDown={event => { if (event.key === "Escape") setViewMenu(false); }}>{(["compute", "storage", "github", "apps", "deployments", "activity"] as const).map(kind => <button type="button" key={kind} onClick={() => { openSurface({kind}); setViewMenu(false); }}>{kind === "github" ? "GitHub repositories" : surfaceLabels[kind]}</button>)}</div> : null}</div>
            <button type="button" className={styles.surfaceTool} aria-label={wide ? "Restore split view" : "Expand workspace view"} aria-pressed={wide} onClick={() => setWide(!wide)}><WorkspaceIcon name={wide ? "collapse" : "expand"} width="17" height="17" /></button>
          </div>
        </div>
        <div className={styles.surfaceContent}>
          {opened.length && workspace ? opened.map((surface, index) => <div key={surfaceKey(surface)} role="tabpanel" id={`${panelId}-view-${index}`} aria-labelledby={`${panelId}-tab-${index}`} hidden={!selected || surfaceKey(selected) !== surfaceKey(surface)}><OperatorSurfaceView surface={surface} workspaceId={workspace} onSelect={openSurface} conversationId={id} githubResult={githubResult} busy={sending || !data?.agent.available} onDeploy={async repository => { await send(undefined, repository); }} /></div>) : <div className={styles.surfacePlaceholder}>
            <div className={styles.surfaceHints}>
              <div><WorkspaceIcon name="apps" /><p>Apps<span>Apps your agent opens</span></p></div>
              <div><WorkspaceIcon name="file" /><p>Files<span>Code and files it works with</span></p></div>
              <div><WorkspaceIcon name="check" /><p>Changes<span>Proposals ready for your review</span></p></div>
              <div><WorkspaceIcon name="activity" /><p>Activity<span>Actions and results as it works</span></p></div>
            </div>
          </div>}
        </div>
      </aside>
      <button type="button" className={styles.panelToggle} aria-label={showPanel ? "Hide right panel" : "Show right panel"} aria-expanded={showPanel} aria-controls={panelId} onClick={() => setPanelOpen(!showPanel)}><WorkspaceIcon name="panel" /></button>
    </div>
  </AppShell>;
}
