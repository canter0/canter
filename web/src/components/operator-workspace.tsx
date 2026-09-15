"use client";

import { useEffect, useRef, useState, useSyncExternalStore, type FormEvent } from "react";
import { useRouter } from "next/navigation";
import Markdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { AppShell } from "./app-shell";
import { useWorkspace } from "./workspace-context";
import { WorkspaceIcon } from "./workspace-icon";
import { OperatorSurfaceView } from "./operator-surface";
import { canterFetch } from "@/lib/canter-api";
import { conversationBase, conversationDetail, isSurface, surfaceKey, surfaceLabels, type ConversationDetail, type OperatorEvent, type OperatorRun, type OperatorSurface } from "@/lib/operator-api";
import styles from "./operator-workspace.module.css";

const toolLabels: Record<string, string> = {
  canter_show_repositories: "Opening GitHub repositories",
  canter_show_apps: "Reading apps", canter_show_deployments: "Reading deployments", canter_show_billing: "Reading billing", canter_show_activity: "Reading activity", canter_show_agents: "Reading agent access",
  canter_inspect_repository: "Inspecting repository", canter_read_repository_file: "Reading source", canter_prepare_repository_deployment: "Preparing deployment", canter_capabilities: "Checking capabilities",
  canter_inspect_system: "Inspecting app", canter_inspect_initial_deployment: "Reading deployment", canter_inspect_initial_deployment_execution: "Reading execution", canter_inspect_change: "Reading change", canter_draft_change: "Preparing change", canter_list_changes: "Reading changes", canter_list_standing_policies: "Checking policies", canter_apply_change_under_policy: "Applying authorized change",
};
const subscribeDraft = (onChange: () => void) => { window.addEventListener("canter-draft", onChange); return () => window.removeEventListener("canter-draft", onChange); };
const activeRun = (run?: OperatorRun | null) => !!run && ["queued", "running"].includes(run.status);
function ResponseText({ text, compact }: { text: string; compact?: boolean }) {
  const split = compact && text.length > 500 ? text.indexOf("\n\n") : -1;
  const render = (content: string) => <Markdown remarkPlugins={[remarkGfm]} skipHtml disallowedElements={["img", "hr"]} components={{ a: ({ href, children }) => <a href={href} target="_blank" rel="noopener noreferrer">{children}</a> }}>{content}</Markdown>;
  return <div className={styles.markdown}>{render(split > 0 ? text.slice(0, split) : text)}{split > 0 ? <details className={styles.fullResponse}><summary>Read full response</summary>{render(text.slice(split + 2))}</details> : null}</div>;
}

export function OperatorWorkspace({ id, githubResult }: { id?: string; githubResult?: string }) {
  const router = useRouter();
  const { data, error: workspaceError, retry: refreshWorkspace } = useWorkspace();
  const workspace = data?.workspace.id;
  const [detail, setDetail] = useState<ConversationDetail | null>(null);
  const [events, setEvents] = useState<OperatorEvent[]>([]);
  const [selected, setSelected] = useState<OperatorSurface | null>(githubResult ? { kind: "github" } : null);
  const [fallbackDraft, setFallbackDraft] = useState("");
  const [error, setError] = useState("");
  const [connectionError, setConnectionError] = useState("");
  const [sending, setSending] = useState(false);
  const [attempt, setAttempt] = useState(0);
  const composer = useRef<HTMLTextAreaElement>(null);
  const transcript = useRef<HTMLDivElement>(null);
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
            cursor = batch[batch.length - 1].sequence;
            setEvents(current => [...current.filter(event => !batch.some(next => next.sequence === event.sequence)), ...batch].sort((a, b) => a.sequence - b.sequence));
            const surfaces = batch.filter(event => event.kind === "surface" && isSurface(event.data));
            const last = surfaces[surfaces.length - 1];
            // A newly requested view can open immediately, while a form the user
            // is editing stays in place. Every view also remains in the transcript.
            const editing = document.activeElement?.closest("[data-workspace-surface] input, [data-workspace-surface] select, [data-workspace-surface] textarea");
            if (last && !editing) {
              const surface = last.data as OperatorSurface;
              if (restoring) setSelected(current => current ?? surface); else setSelected(surface);
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
  }, [events, detail]);

  function editDraft(value: string) {
    setFallbackDraft(value);
    if (storageKey) { try { sessionStorage.setItem(storageKey, value); window.dispatchEvent(new Event("canter-draft")); } catch { /* Nonessential storage. */ } }
  }
  async function send(event?: FormEvent, chosenRepository?: string) {
    event?.preventDefault();
    const message = chosenRepository ? `Deploy @${chosenRepository}` : draft.trim();
    if (!workspace || !message || sending || running || !data?.agent.available) return;
    setSending(true); setError(""); followScroll.current = true;
    if (!pending.current || pending.current.message !== message) pending.current = { id: id ?? `conv_${crypto.randomUUID()}`, requestId: crypto.randomUUID(), message };
    const request = pending.current;
    try {
      const base = conversationBase(workspace);
      await canterFetch(id ? `${base}/${encodeURIComponent(id)}/messages` : base, { method: "POST", body: JSON.stringify(id ? { requestId: request.requestId, message, surface: selected } : { ...request, surface: selected }) });
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
  const runEvents = events.filter(event => event.runId === detail?.run?.id);
  const texts = runEvents.filter(event => event.kind === "text");
  const text = texts[texts.length - 1]?.data.content;
  const toolEvents = runEvents.filter(event => event.kind === "tool");
  const tools = [...new Map(toolEvents.map(event => [String(event.data.callId), event])).values()];
  const shownSurfaces = [...new Map(events.filter(event => event.kind === "surface" && isSurface(event.data)).map(event => { const surface = event.data as OperatorSurface; return [surfaceKey(surface), surface] as const; })).values()];
  const hasMessages = !!detail?.messages.length;

  return <AppShell active="Home" agentView onNewInstruction={() => { if (id) router.push("/app"); else { editDraft(""); setSelected(null); composer.current?.focus(); } }}>
    <div className={styles.workspace} data-has-surface={!!selected}>
      <section className={styles.conversation} aria-label="Canter conversation">
        <header className={styles.conversationHeader}><span>{detail?.conversation.title ?? "Your workspace"}</span><div className={styles.viewShortcuts}><button onClick={() => setSelected({ kind: "deployments" })}>Deployments</button><button onClick={() => setSelected({ kind: "billing" })}>Billing</button></div></header>
        <div className={styles.transcript} ref={transcript} onScroll={() => { const element = transcript.current; if (element) followScroll.current = element.scrollHeight - element.scrollTop - element.clientHeight < 100; }}>
          {!id && !hasMessages ? <div className={styles.welcome}><h1>What would you like to do?</h1><p>Deploy a repository, check on your apps, or manage your workspace.</p></div> : null}
          {id && !detail && !connectionError ? <p className={styles.note} role="status">Loading your conversation…</p> : null}
          {detail?.messages.map(message => <article key={message.id} className={styles.message} data-role={message.role}><span className={styles.speaker}>{message.role === "user" ? "You" : "Canter"}</span>{message.role === "assistant" ? <ResponseText text={message.content} compact={events.some(event => event.runId === message.runId && event.kind === "surface")} /> : <div className={styles.messageText}>{message.content}</div>}</article>)}
          {running && typeof text === "string" && text ? <article className={styles.message} data-role="assistant"><span className={styles.speaker}>Canter</span><ResponseText text={text} /></article> : null}
          {running ? <div className={styles.progress} role="status"><span className={styles.pulse} />{detail?.run?.status === "queued" && !tools.length ? "Waiting for the agent…" : tools.length && tools[tools.length - 1].data.status === "running" ? toolLabels[String(tools[tools.length - 1].data.name)] ?? "Working in your workspace…" : "Working…"}</div> : null}
          {tools.length ? <details className={styles.operations}><summary>{tools.length} workspace operation{tools.length === 1 ? "" : "s"}</summary>{tools.map(event => <div key={String(event.data.callId)}><span>{toolLabels[String(event.data.name)] ?? String(event.data.name).replace(/^canter_/, "").replaceAll("_", " ")}</span><small>{String(event.data.status)}</small></div>)}</details> : null}
          {shownSurfaces.length ? <div className={styles.surfaces} aria-label="Views in this conversation">{shownSurfaces.map(surface => <button key={surfaceKey(surface)} onClick={() => setSelected(surface)} aria-pressed={!!selected && surfaceKey(selected) === surfaceKey(surface)}><WorkspaceIcon name={surface.kind === "repository" ? "folder" : "panel"} width="15" height="15" />{surface.repository ?? surface.system ?? surfaceLabels[surface.kind]}</button>)}</div> : null}
          {detail?.run?.status === "failed" ? <p className={styles.error} role="alert">{detail.run.failure || "The response failed."} You can continue below.</p> : null}
          {detail?.run?.status === "cancelled" ? <p className={styles.note}>Stopped. Completed operations remain saved.</p> : null}
        </div>
        <div className={styles.composerArea}>
          {workspaceError || error ? <p className={styles.error} role="alert">{error || workspaceError}{workspaceError ? <button onClick={refreshWorkspace}>Retry</button> : null}</p> : null}
          {connectionError ? <p className={styles.error} role="status">Updates disconnected. Reconnecting… <button onClick={() => setAttempt(value => value + 1)}>Retry now</button></p> : null}
          {data && !data.agent.available ? <p className={styles.error} role="alert">The workspace agent is unavailable. Ask your administrator to configure its model connection.</p> : null}
          <form className={styles.composer} onSubmit={send}>
            {selected ? <div className={styles.context}><WorkspaceIcon name="panel" width="13" height="13" />Viewing {selected.system ?? selected.repository ?? surfaceLabels[selected.kind]}</div> : null}
            <textarea ref={composer} aria-label="Message Canter" placeholder="Ask Canter to do something…" rows={3} value={draft} maxLength={20000} onChange={event => editDraft(event.target.value)} onKeyDown={event => { if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) { event.preventDefault(); void send(); } }} />
            <div className={styles.composerFooter}><span>Canter <span className={styles.agentState}>{running ? "is working" : "workspace agent"}</span></span>{running ? <button type="button" className={styles.stop} onClick={() => void stop()}>Stop</button> : <button className={styles.send} aria-label="Send message" disabled={sending || !draft.trim() || !data?.agent.available}><WorkspaceIcon name="arrow" /></button>}</div>
          </form>
          {!id ? <div className={styles.suggestions}>{["Show my deployments", "Show billing", "Help me deploy a repository"].map(prompt => <button key={prompt} onClick={() => { editDraft(prompt); composer.current?.focus(); }}>{prompt}</button>)}</div> : null}
        </div>
      </section>
      {selected && workspace ? <aside className={styles.surface} data-workspace-surface aria-label={`${surfaceLabels[selected.kind]} view`}><header className={styles.surfaceHeader}><span>{surfaceLabels[selected.kind]}</span><button aria-label="Close view" onClick={() => setSelected(null)}><WorkspaceIcon name="close" /></button></header><div className={styles.surfaceContent}><OperatorSurfaceView key={surfaceKey(selected)} surface={selected} workspaceId={workspace} onSelect={setSelected} conversationId={id} githubResult={githubResult} busy={sending || running || !data?.agent.available} onDeploy={repository => send(undefined, repository)} /></div></aside> : null}
    </div>
  </AppShell>;
}
