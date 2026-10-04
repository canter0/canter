"use client";

import { useCallback, useEffect, useLayoutEffect, useId, useRef, useState, useSyncExternalStore, type FormEvent } from "react";
import { useMediaQuery } from "./use-media-query";
import { usePopover } from "./use-popover";
import { useFocusContainment } from "./use-focus-containment";
import { moveMenuFocus } from "@/lib/interaction";
import { useRouter } from "next/navigation";
import { useOperatorAttachmentDraft } from "@/lib/operator-attachment-draft";
import { OperatorMessageContext, OperatorTurn } from "./operator-turn";
import { WorkspaceLoading } from "./workspace-loading";
import { operatorModelOptions, parseModelPreferences, type OperatorModelOptions } from "@/lib/operator-models";
import { OperatorComposer } from "./operator-composer";
import { GitHubRepositories } from "./github-repositories";
import { AppShell } from "./app-shell";
import type { SpotlightCommand } from "@/lib/spotlight";
import { shortcutLabel } from "@/lib/a-shortcuts";
import { useWorkspace } from "./workspace-context";
import { WorkspaceIcon } from "./workspace-icon";
import { OperatorSurfaceView } from "./operator-surface";
import { canterFetch, CanterAPIError } from "@/lib/canter-api";
import { conversationBase, isSurface, surfaceKey, surfaceLabels, type ConversationDetail, type OperatorEvent, type OperatorRun, type OperatorSurface } from "@/lib/operator-api";
import styles from "./operator-workspace.module.css";

const subscribeDraft = (onChange: () => void) => { window.addEventListener("canter-draft", onChange); return () => window.removeEventListener("canter-draft", onChange); };
const subscribeModel = (onChange: () => void) => { window.addEventListener("canter-model", onChange); return () => window.removeEventListener("canter-model", onChange); };
const activeRun = (run?: OperatorRun | null) => !!run && ["queued", "running"].includes(run.status);
const restoredSurfaces = (events: OperatorEvent[]) => [...new Map(events.filter(event => event.kind === "surface" && isSurface(event.data) && !["github", "compute", "storage"].includes(String(event.data.kind))).map(event => [surfaceKey(event.data as OperatorSurface), event.data as OperatorSurface])).values()];

export function OperatorWorkspace({ id, githubResult, focusComposer, initialDetail = null, initialLoadedAt }: { id?: string; githubResult?: string; focusComposer?: boolean; initialDetail?: ConversationDetail | null; initialLoadedAt?: number }) {
  const router = useRouter();
  const { data, conversationCache, retry: refreshWorkspace } = useWorkspace();
  const workspace = data?.workspace.id;
  const [snapshot] = useState(() => id ? conversationCache.peek(id) : undefined);
  const [detail, setDetail] = useState<ConversationDetail | null>(snapshot?.detail ?? initialDetail);
  const [sourceDetail, setSourceDetail] = useState(initialDetail);
  if (sourceDetail !== initialDetail) {
    setSourceDetail(initialDetail);
    if (initialDetail && (!id || !conversationCache.peek(id))) setDetail(initialDetail);
  }
  const [events, setEvents] = useState<OperatorEvent[]>(snapshot?.events ?? []);
  const latestEvents = useRef(events);
  const [selected, setSelected] = useState<OperatorSurface | null>(() => restoredSurfaces(snapshot?.events ?? []).at(-1) ?? null);
  const [opened, setOpened] = useState<OperatorSurface[]>(() => restoredSurfaces(snapshot?.events ?? []));
  const [wide, setWide] = useState(false);
  const [viewMenu, setViewMenu] = useState(false);
  const [showScroll, setShowScroll] = useState(false);
  const [hasEarlierMessages, setHasEarlierMessages] = useState(false);
  const [panelOpen, setPanelOpen] = useState<boolean | null>(null);
  const panelId = useId();
  const [submittedPrompt, setSubmittedPrompt] = useState("");
  const [submittedSurface, setSubmittedSurface] = useState<OperatorSurface | null>(null);
  const composerArea = useRef<HTMLDivElement>(null);
  const composerOrigin = useRef<DOMRect | null>(null);
  const [inlineGitHub, setInlineGitHub] = useState(!!githubResult);
  const [fallbackDraft, setFallbackDraft] = useState("");
  const [error, setError] = useState("");
  const [connectionError, setConnectionError] = useState("");
  const [deliveryNotice, setDeliveryNotice] = useState("");
  const [sending, setSending] = useState(false);
  const [stopping, setStopping] = useState(false);
  const sendLock = useRef(false);
  const stopLock = useRef(false);
  const reducedMotion = useMediaQuery("(prefers-reduced-motion: reduce)");
  const overlayPanel = useMediaQuery("(max-width: 1100px)");
  const surfacePanel = useRef<HTMLElement>(null);
  const panelTrigger = useRef<HTMLButtonElement>(null);
  const closePanel = useCallback(() => setPanelOpen(false), []);
  const closeViewMenu = useCallback(() => setViewMenu(false), []);
  const [attempt, setAttempt] = useState(0);
  const [attachedContext, setAttachedContext] = useState<OperatorSurface | null | undefined>(undefined);
  const composer = useRef<HTMLTextAreaElement>(null);
  const viewMenuAnchor = useRef<HTMLDivElement>(null);
  const transcript = useRef<HTMLDivElement>(null);
  const transcriptContent = useRef<HTMLDivElement>(null);
  const followScroll = useRef(true);
  const pending = useRef<{ id: string; requestId: string; message: string; signature: string } | null>(null);
  const storageKey = workspace ? `canter:conversation-draft:${workspace}:${id ?? "new"}` : null;
  const modelKey = workspace && data ? `canter:conversation-model:${data.account.id}:${workspace}:${id ?? "new"}` : null;
  const [modelFallback, setModelFallback] = useState<{ key: string; value: string } | null>(null);
  const modelOverride = useSyncExternalStore(subscribeModel, () => {
    try { return modelKey ? sessionStorage.getItem(modelKey) ?? (modelFallback?.key === modelKey ? modelFallback.value : "") : ""; }
    catch { return modelFallback?.key === modelKey ? modelFallback?.value ?? "" : ""; }
  }, () => "");
  const selectedModel = modelOverride || (detail?.conversation.id === id ? detail?.run?.model : undefined) || data?.agent.model;
  const optionsKey = modelKey ? `${modelKey}:options` : null;
  const [optionsFallback, setOptionsFallback] = useState<{ key: string; value: string } | null>(null);
  const optionsRaw = useSyncExternalStore(subscribeModel, () => {
    const fallback = optionsFallback?.key === optionsKey ? optionsFallback.value : "";
    try { return optionsKey ? sessionStorage.getItem(optionsKey) ?? fallback : ""; } catch { return fallback; }
  }, () => "");
  const modelPreferences = { ...(detail && detail.conversation.id === id && detail.run ? { [detail.run.model]: detail.run.modelOptions ?? {} } : {}), ...parseModelPreferences(optionsRaw) };
  const selectedModelOptions = operatorModelOptions(selectedModel, selectedModel ? modelPreferences[selectedModel] : undefined);
  const running = activeRun(detail?.run);
  const attachmentDraft = useOperatorAttachmentDraft(storageKey);
  const showPanel = panelOpen ?? !!selected;
  useFocusContainment(overlayPanel && showPanel, surfacePanel, closePanel, panelTrigger);
  usePopover(viewMenu, viewMenuAnchor, closeViewMenu);
  useEffect(() => {
    if (!focusComposer || id || !workspace || !attachmentDraft.loaded) return;
    const frame = requestAnimationFrame(() => composer.current?.focus());
    return () => cancelAnimationFrame(frame);
  }, [focusComposer, id, workspace, attachmentDraft.loaded]);

  const draft = useSyncExternalStore(subscribeDraft, () => {
    try { return storageKey ? sessionStorage.getItem(storageKey) ?? fallbackDraft : fallbackDraft; } catch { return fallbackDraft; }
  }, () => "");

  useEffect(() => {
    latestEvents.current = events;
    if (id && detail) conversationCache.save(id, detail, events, detail === initialDetail ? initialLoadedAt : undefined);
  }, [id, detail, events, conversationCache, initialDetail, initialLoadedAt]);

  useEffect(() => {
    if (!id || !workspace) return;
    const controller = new AbortController();
    let cursor = latestEvents.current.at(-1)?.sequence ?? 0;
    let restoring = cursor === 0;
    async function sync(refresh = false) {
      const value = await conversationCache.load(id!, refresh);
      if (!controller.signal.aborted) { setDetail(value); setDeliveryNotice(""); }
    }
    async function follow() {
      try {
        await sync(attempt > 0);
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
            if (batch.some(event => event.kind === "queued" || event.kind === "finished" || event.kind === "title")) { await sync(true); refreshWorkspace(); }
            else if (batch.some(event => event.kind === "working")) setDetail(current => current?.run ? { ...current, run: { ...current.run, status: "running" } } : current);
          }
          if (batch.length < 300) restoring = false;
          setConnectionError("");
        }
      } catch (cause) {
        if (!controller.signal.aborted) {
          if (cause instanceof CanterAPIError && [401, 403, 404].includes(cause.status)) { conversationCache.invalidate(id!); setDetail(null); setEvents([]); setOpened([]); setSelected(null); }
          setConnectionError(cause instanceof Error ? cause.message : "Updates were interrupted. Your work continues on the server.");
        }
      }
    }
    void follow();
    return () => controller.abort();
  }, [id, workspace, attempt, refreshWorkspace, conversationCache]);

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

  useLayoutEffect(() => {
    const origin = composerOrigin.current;
    const element = composerArea.current;
    if (!origin || !element) return;
    composerOrigin.current = null;
    if (reducedMotion) return;
    const destination = element.getBoundingClientRect();
    const animation = element.animate([{ transform: `translateY(${origin.top - destination.top}px)` }, { transform: "translateY(0)" }], { duration: 240, easing: "cubic-bezier(.2,.8,.2,1)" });
    return () => animation.cancel();
  }, [submittedPrompt, reducedMotion]);

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
  function selectModel(value: string) {
    if (!modelKey) return;
    setModelFallback({ key: modelKey, value });
    try { sessionStorage.setItem(modelKey, value); window.dispatchEvent(new Event("canter-model")); } catch { /* Keep the selection in memory when storage is unavailable. */ }
  }
  function selectModelOptions(model: string, options: OperatorModelOptions) {
    if (!optionsKey) return;
    const value = JSON.stringify({ ...modelPreferences, [model]: options });
    setOptionsFallback({ key: optionsKey, value });
    try { sessionStorage.setItem(optionsKey, value); window.dispatchEvent(new Event("canter-model")); } catch { /* Preserve settings in memory. */ }
  }
  async function send(event?: FormEvent, chosenRepository?: string) {
    event?.preventDefault();
    const message = (chosenRepository ? `Deploy https://github.com/${chosenRepository}` : ((composer.current?.value ?? draft).trim() || (attachmentDraft.items.length ? "Please review the attached files." : "")));
    const attachments = chosenRepository ? [] : attachmentDraft.items;
    const requestSurface: OperatorSurface | null = chosenRepository ? { kind: "repository", repository: chosenRepository } : (attachedContext === undefined ? selected : attachedContext);
    const signature = JSON.stringify({ message, attachments, surface: requestSurface, model: selectedModel, modelOptions: selectedModelOptions });
    if (!workspace || !message || sendLock.current || !data?.agent.available) return false;
    sendLock.current = true;
    composerOrigin.current = composerArea.current?.getBoundingClientRect() ?? null;
    setSubmittedPrompt(message);
    setSubmittedSurface(requestSurface);
    setSending(true); setError(""); followScroll.current = true;
    if (!pending.current || pending.current.signature !== signature) pending.current = { id: id ?? `conv_${crypto.randomUUID()}`, requestId: crypto.randomUUID(), message, signature };
    const { id: requestConversationId, requestId } = pending.current;
    const request = { id: requestConversationId, requestId, message, attachments, model: selectedModel, modelOptions: selectedModelOptions };
    try {
      const base = conversationBase(workspace);
      await canterFetch(id ? `${base}/${encodeURIComponent(id)}/messages` : base, { method: "POST", body: JSON.stringify(id ? { requestId: request.requestId, message, attachments, surface: requestSurface, model: selectedModel, modelOptions: selectedModelOptions } : { ...request, surface: requestSurface }) });
      if (!chosenRepository) { editDraft(""); attachmentDraft.update([]); }
      else if (!id) {
        if (draft) { try { sessionStorage.setItem(`canter:conversation-draft:${workspace}:${request.id}`, draft); } catch { /* Nonessential storage. */ } }
        if (attachmentDraft.items.length) attachmentDraft.update(attachmentDraft.items, `canter:conversation-draft:${workspace}:${request.id}`);
      }
      pending.current = null;
      conversationCache.invalidate(requestConversationId);
      refreshWorkspace();
      if (!id) {
        if (selectedModel) { try { sessionStorage.setItem(`canter:conversation-model:${data.account.id}:${workspace}:${request.id}`, selectedModel); sessionStorage.setItem(`canter:conversation-model:${data.account.id}:${workspace}:${request.id}:options`, JSON.stringify(modelPreferences)); } catch { /* The persisted run also carries the model. */ } }
        const destination = `/app/conversations/${encodeURIComponent(request.id)}`;
        router.prefetch(destination);
        router.push(destination);
      }
      else {
        // The message is already accepted. A failed refresh must not invite a duplicate send.
        try { setDetail(await conversationCache.load(id, true)); }
        catch { setDeliveryNotice("Message sent. Reconnecting for updates."); setConnectionError("Updates are disconnected."); setAttempt(value => value + 1); }
      }
      return true;
    } catch (cause) { setSubmittedPrompt(""); setSubmittedSurface(null); setError(cause instanceof Error ? cause.message : "Your message could not be sent. It is saved here so you can retry."); requestAnimationFrame(() => composer.current?.focus()); return false; }
    finally { sendLock.current = false; setSending(false); }
  }
  async function stop() {
    if (!id || !workspace || stopLock.current) return;
    stopLock.current = true; setStopping(true);
    try {
      await canterFetch(`${conversationBase(workspace)}/${encodeURIComponent(id)}/stop`, { method: "POST" });
      try { setDetail(await conversationCache.load(id, true)); }
      catch { setDeliveryNotice("Stop requested. Reconnecting for updates."); setConnectionError("Updates are disconnected."); setAttempt(value => value + 1); }
    } catch (cause) { setError(cause instanceof Error ? cause.message : "Could not stop this response."); }
    finally { stopLock.current = false; setStopping(false); }
  }
  function openSurface(surface: OperatorSurface) {
    if (surface.kind === "compute" || surface.kind === "storage") { setPanelOpen(false); suggestPrompt(surface.kind); return; }
    if (surface.kind === "github") { setInlineGitHub(true); setPanelOpen(false); followScroll.current = true; requestAnimationFrame(() => transcript.current?.scrollTo({ top: transcript.current.scrollHeight })); return; }
    setOpened(current => current.some(item => surfaceKey(item) === surfaceKey(surface)) ? current : [...current, surface]);
    setSelected(surface); setPanelOpen(true); setViewMenu(false);
    requestAnimationFrame(() => surfacePanel.current?.querySelector<HTMLButtonElement>('[role="tab"][aria-selected="true"]')?.focus());
  }
  function closeSurface(surface: OperatorSurface) {
    const remaining = opened.filter(item => surfaceKey(item) !== surfaceKey(surface));
    setOpened(remaining);
    const next = selected && surfaceKey(selected) !== surfaceKey(surface) ? selected : remaining.at(-1) ?? null;
    setSelected(next);
    if (!remaining.length) { setPanelOpen(false); setWide(false); }
    requestAnimationFrame(() => {
      const target = remaining.length ? surfacePanel.current?.querySelector<HTMLButtonElement>('[role="tab"][aria-selected="true"]') : document.querySelector<HTMLButtonElement>('button[aria-label="Show workspace panel"]');
      target?.focus();
    });
  }
  const tabLabel = (surface: OperatorSurface) => surface.path?.split("/").at(-1) ?? (surface.kind === "repository" ? surface.repository?.split("/").at(-1) : undefined) ?? surface.system ?? surfaceLabels[surface.kind];
  const githubRun = events.findLast(event => event.kind === "surface" && event.data.kind === "github")?.runId;
  const hasMessages = !!detail?.messages.length;
  const github = workspace ? <GitHubRepositories inline workspaceId={workspace} conversationId={id} result={githubResult} busy={sending || !data?.agent.available} onDeploy={async repository => { await send(undefined, repository); }} /> : null;
  function focusPanel() {
    requestAnimationFrame(() => (surfacePanel.current?.querySelector<HTMLElement>('[role="tab"][aria-selected="true"]') ?? surfacePanel.current)?.focus());
  }
  function selectTab(index: number) {
    const surface = opened[index];
    if (!surface) return;
    setSelected(surface);
    setPanelOpen(true);
    focusPanel();
  }
  const workspaceCommands: SpotlightCommand[] = [
    { id: "right-panel", title: !showPanel ? "Open right sidebar" : wide || overlayPanel ? "Close right sidebar" : "Expand right sidebar fully", key: "e", icon: "panel", run: () => {
      if (!showPanel) { setWide(false); setPanelOpen(true); focusPanel(); }
      else if (!wide && !overlayPanel) { setWide(true); focusPanel(); }
      else { setPanelOpen(false); setWide(false); requestAnimationFrame(() => panelTrigger.current?.focus()); }
    } },
    ...opened.map((surface, index) => ({ id: `tab-${surfaceKey(surface)}`, title: `Tab ${index + 1}: ${tabLabel(surface)}`, key: index < 9 ? String(index + 1) : undefined, icon: "folder" as const, run: () => selectTab(index) })),
    ...(opened.length > 1 ? ([
      { id: "previous-tab", title: "Previous workspace tab", key: "[", icon: "panel", run: () => selectTab((opened.findIndex(surface => selected && surfaceKey(surface) === surfaceKey(selected)) - 1 + opened.length) % opened.length) },
      { id: "next-tab", title: "Next workspace tab", key: "]", icon: "panel", run: () => selectTab((opened.findIndex(surface => selected && surfaceKey(surface) === surfaceKey(selected)) + 1) % opened.length) },
    ] satisfies SpotlightCommand[]) : []),
  ];

  return <AppShell active="Home" agentView pageTitle={detail?.conversation.title} workspaceCommands={workspaceCommands} onNewInstruction={() => { if (id) router.push("/app?compose=1"); else { setInlineGitHub(false); setPanelOpen(false); requestAnimationFrame(() => composer.current?.focus()); } }}>
    <div className={styles.workspace} data-has-surface={showPanel} data-working={running} data-wide={wide && showPanel} data-empty={!id && !hasMessages && !inlineGitHub && !submittedPrompt}>
      <div className={styles.conversationPane} inert={(wide || overlayPanel) && showPanel}>
        <section className={styles.conversation} aria-label="Canter conversation" data-scrolled={hasEarlierMessages}>
          <div className={styles.transcript} ref={transcript} tabIndex={0} role="region" aria-label="Conversation messages" onWheel={event => { if (event.deltaY < 0) followScroll.current = false; }} onTouchMove={() => { followScroll.current = false; }} onKeyDown={event => { if (["ArrowUp", "PageUp", "Home"].includes(event.key)) followScroll.current = false; }} onScroll={() => { const element = transcript.current; if (element) { const away = element.scrollHeight - element.scrollTop - element.clientHeight >= 80; setShowScroll(away); setHasEarlierMessages(element.scrollTop > 1); if (!away) followScroll.current = true; } }}>
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
            {showScroll ? <button type="button" className={styles.scrollLatest} aria-label="Scroll to latest message" onClick={() => { followScroll.current = true; transcript.current?.scrollTo({ top: transcript.current.scrollHeight, behavior: reducedMotion ? "instant" : "smooth" }); }}><WorkspaceIcon name="down" width="16" height="16" /></button> : null}
            {!id && !hasMessages && !submittedPrompt ? <div className={styles.startBrand}><span className="wordmark">canter</span></div> : null}
            {error ? <p className={styles.error} role="alert">{error}</p> : null}
            {connectionError ? <p className={styles.error} role="status">{deliveryNotice || connectionError} <button onClick={() => setAttempt(value => value + 1)}>Reconnect</button></p> : null}
            {data && !data.agent.available ? <p className={styles.error} role="alert">The workspace agent is unavailable. Ask your administrator to configure its model connection.</p> : null}
            {attachmentDraft.error ? <p className={styles.error} role="status">{attachmentDraft.error}</p> : null}
            <OperatorComposer attachments={attachmentDraft.items} onAttachments={attachmentDraft.update} sending={sending} context={(attachedContext === undefined ? selected : attachedContext)} onClearContext={() => setAttachedContext(null)} draft={draft} onChange={editDraft} onSend={() => void send()} onStop={() => void stop()} stopping={stopping} running={running} disabled={sending || !data?.agent.available || !attachmentDraft.loaded} inputRef={composer} model={selectedModel} onModelChange={selectModel} modelOptions={modelPreferences} onModelOptionsChange={selectModelOptions} onSelect={surface => { setAttachedContext(surface); if (surface.kind === "github") openSurface(surface); }} />
            {!id && !hasMessages && !submittedPrompt ? <div className={styles.starters} aria-label="Try a workspace action"><button onClick={() => suggestPrompt("compute")}>Plan a VPS</button><button onClick={() => suggestPrompt("storage")}>Create a bucket</button><button onClick={() => suggestPrompt("github")}>Connect GitHub</button></div> : null}

          </div>
        </section>
      </div>
      <aside ref={surfacePanel} tabIndex={-1} role={overlayPanel && showPanel ? "dialog" : undefined} aria-modal={overlayPanel && showPanel || undefined} id={panelId} className={styles.surface} data-workspace-surface data-empty={!selected} aria-label={selected ? `${surfaceLabels[selected.kind]} view` : "Workspace view"} aria-hidden={!showPanel} inert={!showPanel}>
        <div className={styles.surfaceToolbar}>
          <button type="button" className={styles.panelClose} title="Close workspace panel" aria-label="Close workspace panel" onClick={() => { closePanel(); requestAnimationFrame(() => document.querySelector<HTMLButtonElement>('button[aria-label="Show workspace panel"]')?.focus()); }}><WorkspaceIcon name="close" width="18" height="18" /></button>
          <div className={styles.surfaceTabs} role="tablist" aria-label="Workspace tabs">{opened.map((surface, index) => <div className={styles.surfaceTab} key={surfaceKey(surface)} data-active={!!selected && surfaceKey(selected) === surfaceKey(surface)}>
            <button type="button" role="tab" id={`${panelId}-tab-${index}`} aria-controls={`${panelId}-view-${index}`} aria-selected={!!selected && surfaceKey(selected) === surfaceKey(surface)} tabIndex={selected && surfaceKey(selected) === surfaceKey(surface) ? 0 : -1} title={`${surface.path ?? surface.repository ?? tabLabel(surface)}${index < 9 ? ` · ${shortcutLabel(String(index + 1))}` : ""}`} onClick={() => setSelected(surface)} onKeyDown={event => { if (["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) { event.preventDefault(); const next = event.key === "Home" ? 0 : event.key === "End" ? opened.length - 1 : (index + (event.key === "ArrowRight" ? 1 : -1) + opened.length) % opened.length; setSelected(opened[next]); document.getElementById(`${panelId}-tab-${next}`)?.focus(); } if (event.key === "Delete") { event.preventDefault(); closeSurface(surface); } }}><WorkspaceIcon name={surface.kind === "repository-changes" ? "changes" : surface.kind === "file" ? "file" : "folder"} width="14" height="14" /><span>{tabLabel(surface)}</span></button>
            <button type="button" className={styles.closeTab} aria-label={`Close ${tabLabel(surface)} tab`} onClick={() => closeSurface(surface)}><WorkspaceIcon name="close" width="12" height="12" /></button>
          </div>)}</div>
          <div className={styles.surfaceTools}>
            <div ref={viewMenuAnchor} className={styles.menuAnchor}><button type="button" className={styles.surfaceTool} aria-label="Open workspace view" aria-haspopup="menu" aria-expanded={viewMenu} onClick={() => setViewMenu(!viewMenu)}><WorkspaceIcon name="plus" width="17" height="17" /></button>{viewMenu ? <div className={styles.viewMenu} role="menu" aria-label="Workspace views" onKeyDown={moveMenuFocus}>{(["compute", "storage", "github", "apps", "deployments", "activity"] as const).map(kind => <button type="button" role="menuitem" tabIndex={-1} key={kind} onClick={() => { openSurface({kind}); setViewMenu(false); }}>{kind === "github" ? "GitHub repositories" : surfaceLabels[kind]}</button>)}</div> : null}</div>
            <button type="button" className={styles.surfaceTool} hidden={overlayPanel} aria-label={wide ? "Restore split view" : "Expand workspace view"} aria-pressed={wide} onClick={() => setWide(!wide)}><WorkspaceIcon name={wide ? "collapse" : "expand"} width="17" height="17" /></button>
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
      <button ref={panelTrigger} type="button" className={styles.panelToggle} hidden={showPanel} title="Open workspace panel · A + E" aria-label="Show workspace panel" aria-expanded={showPanel} aria-controls={panelId} onClick={() => setPanelOpen(!showPanel)}><WorkspaceIcon name="panel" /></button>
    </div>
  </AppShell>;
}
