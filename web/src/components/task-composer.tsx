"use client";

import { ConnectAgentButton } from "@/components/connect-agent-button";

import { useRouter } from "next/navigation";
import { type FormEvent, useEffect, useRef, useState } from "react";
import { agentIsConnected, canterFetch, type TaskContext, type WorkspaceTask } from "@/lib/canter-api";
import { reasoningLevels, taskModels } from "@/lib/task-options";
import { useWorkspace } from "./workspace-context";
import { WorkspaceIcon } from "./workspace-icon";
import styles from "./workspace.module.css";

export function TaskComposer({ autoFocus = false }: { autoFocus?: boolean }) {
  const router = useRouter();
  const { data, loading, error: workspaceError } = useWorkspace();
  const [prompt, setPrompt] = useState("");
  const [model, setModel] = useState<string>(taskModels[0].id);
  const [reasoning, setReasoning] = useState<string>("medium");
  const [agentId, setAgentId] = useState("");
  const [context, setContext] = useState<TaskContext[]>([]);
  const [menu, setMenu] = useState<"model" | "context" | null>(null);
  const [reasoningMenu, setReasoningMenu] = useState<string | null>(null);
  const [picker, setPicker] = useState<"repository" | "app" | "task">("repository");
  const [pickerOpen, setPickerOpen] = useState(false);
  const [search, setSearch] = useState("");
  const [repositoryUrl, setRepositoryUrl] = useState("");
  const [error, setError] = useState("");
  const [pending, setPending] = useState(false);
  const [readingFiles, setReadingFiles] = useState(false);
  const toolbar = useRef<HTMLDivElement>(null);
  const dismissTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const fileInput = useRef<HTMLInputElement>(null);
  const dialog = useRef<HTMLDialogElement>(null);
  const modelButton = useRef<HTMLButtonElement>(null);
  const contextButton = useRef<HTMLButtonElement>(null);
  const agents = data?.installations.filter(agentIsConnected) ?? [];
  const selectedAgent = agents.find(agent => agent.id === agentId) ?? (agents.length === 1 ? agents[0] : undefined);
  const selectedModel = taskModels.find(item => item.id === model) ?? taskModels[0];

  useEffect(() => {
    if (!menu) return;
    const dismiss = (event: PointerEvent) => { if (event.target instanceof Node && !toolbar.current?.contains(event.target)) { setMenu(null); setReasoningMenu(null); } };
    const escape = (event: KeyboardEvent) => { if (event.key === "Escape") { event.preventDefault(); setMenu(null); setReasoningMenu(null); (menu === "model" ? modelButton : contextButton).current?.focus(); } };
    document.addEventListener("pointerdown", dismiss);
    document.addEventListener("keydown", escape);
    return () => { document.removeEventListener("pointerdown", dismiss); document.removeEventListener("keydown", escape); };
  }, [menu]);

  useEffect(() => () => { if (dismissTimer.current) clearTimeout(dismissTimer.current); }, []);

  function keepMenuOpen() {
    if (dismissTimer.current) clearTimeout(dismissTimer.current);
  }

  function dismissMenuAfterPointerLeaves() {
    keepMenuOpen();
    dismissTimer.current = setTimeout(() => { setMenu(null); setReasoningMenu(null); }, 180);
  }

  function addContext(item: TaskContext) {
    setContext(current => current.some(existing => item.kind !== "attachment" && existing.kind === item.kind && (item.referenceId ? existing.referenceId === item.referenceId : existing.url === item.url)) ? current : [...current, item].slice(0, 12));
    dialog.current?.close();
  }

  function openPicker(kind: typeof picker) {
    setPicker(kind); setSearch(""); setError(""); setMenu(null);
    setPickerOpen(true);
    dialog.current?.showModal();
  }

  async function attachFiles(files: File[]) {
    if (!files.length) return;
    setError("");
    if (files.length + context.length > 12) { setError("Add up to 12 context items per task."); return; }
    if (files.some(file => file.size > 2 * 1024 * 1024)) { setError("Each attachment can be up to 2 MB."); return; }
    if (files.reduce((total, file) => total + file.size, context.reduce((total, item) => total + (item.size ?? 0), 0)) > 8 * 1024 * 1024) { setError("Attachments can total up to 8 MB per task."); return; }
    setReadingFiles(true);
    try {
      const items = await Promise.all(files.map(async file => {
        const encoded = await new Promise<string>((resolve, reject) => { const reader = new FileReader(); reader.onload = () => resolve(String(reader.result).split(",")[1] ?? ""); reader.onerror = () => reject(new Error(`Couldn’t read ${file.name}.`)); reader.readAsDataURL(file); });
        return { id: crypto.randomUUID(), kind: "attachment" as const, name: file.name, mediaType: file.type || "application/octet-stream", dataBase64: encoded, size: file.size };
      }));
      setContext(current => [...current, ...items]);
    } catch (cause) { setError(cause instanceof Error ? cause.message : "Couldn’t read the attachment."); }
    finally { setReadingFiles(false); }
  }

  function addRepository(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    try {
      const url = new URL(repositoryUrl.trim());
      if (!["http:", "https:"].includes(url.protocol) || url.username || url.password) throw new Error("Use an HTTP or HTTPS repository URL without credentials.");
      addContext({ id: crypto.randomUUID(), kind: "repository", name: url.pathname.replace(/^\//, "").replace(/\/$/, "") || url.hostname, url: url.href });
      setRepositoryUrl(""); setError("");
    } catch (cause) { setError(cause instanceof Error ? cause.message : "Enter a valid repository URL."); }
  }

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!prompt.trim() || !data || pending || readingFiles || workspaceError) return;
    setPending(true); setMenu(null); setError("");
    try {
      const task = await canterFetch<WorkspaceTask>(`/workspaces/${encodeURIComponent(data.workspace.id)}/tasks`, { method: "POST", body: JSON.stringify({ prompt, model, reasoning, targetInstallationId: selectedAgent?.id ?? "", context }) });
      router.push(`/app/tasks/${encodeURIComponent(task.id)}`);
    } catch (cause) { setError(cause instanceof Error ? cause.message : "Your task could not be saved."); setPending(false); }
  }

  return <>
    <div className={styles.composerCard}>
      <form onSubmit={event => void submit(event)} className={styles.composer}>
        <label className="sr-only" htmlFor="task-prompt">Task prompt</label>
        <textarea id="task-prompt" autoFocus={autoFocus} value={prompt} onChange={event => setPrompt(event.target.value)} placeholder="Deploy an app, make a change, or investigate an issue…" maxLength={12000} disabled={pending} onKeyDown={event => {
          if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) {
            event.preventDefault();
            if (!event.repeat) event.currentTarget.form?.requestSubmit();
          }
        }} />
        {context.length > 0 ? <div className={styles.contextChips}>{context.map(item => <span key={item.id} className={styles.contextChip}><WorkspaceIcon name={item.kind === "repository" ? "folder" : item.kind === "app" ? "apps" : item.kind === "task" ? "message" : "file"} width="14" height="14" /><span title={item.name}>{item.name}</span><button type="button" disabled={pending} aria-label={`Remove ${item.name}`} onClick={() => setContext(current => current.filter(entry => entry.id !== item.id))}><WorkspaceIcon name="close" width="12" height="12" /></button></span>)}</div> : null}
        <div className={styles.composerToolbar} ref={toolbar}>
          <div className={styles.composerControls}>
            <div className={styles.menuAnchor} onPointerEnter={keepMenuOpen} onPointerLeave={event => { if (event.pointerType === "mouse") dismissMenuAfterPointerLeaves(); }} onBlur={event => { if (!event.currentTarget.contains(event.relatedTarget)) { setMenu(null); setReasoningMenu(null); } }}>
              <button ref={contextButton} type="button" disabled={pending || readingFiles || context.length >= 12} className={styles.iconButton} aria-label="Add context" aria-haspopup="dialog" aria-expanded={menu === "context"} onClick={() => { setMenu(menu === "context" ? null : "context"); setReasoningMenu(null); }}><WorkspaceIcon name="plus" width="21" height="21" /></button>
              {menu === "context" ? <div className={styles.contextMenu} role="dialog" aria-label="Add context">
                <button type="button" onClick={() => { setMenu(null); fileInput.current?.click(); }}><WorkspaceIcon name="attachment" />Upload attachment</button>
                <div className={styles.menuRule} />
                <button type="button" onClick={() => openPicker("repository")}><WorkspaceIcon name="folder" />Repository</button>
                <button type="button" onClick={() => openPicker("app")}><WorkspaceIcon name="apps" />Canter apps</button>
                <button type="button" onClick={() => openPicker("task")}><WorkspaceIcon name="message" />Previous tasks</button>
              </div> : null}
            </div>
            <div className={styles.menuAnchor} onPointerEnter={keepMenuOpen} onPointerLeave={event => { if (event.pointerType === "mouse") dismissMenuAfterPointerLeaves(); }} onBlur={event => { if (!event.currentTarget.contains(event.relatedTarget)) { setMenu(null); setReasoningMenu(null); } }}>
              <button ref={modelButton} type="button" className={styles.modelTrigger} disabled={pending} aria-label={`Model: ${selectedModel.label}, reasoning: ${reasoning}`} aria-haspopup="dialog" aria-expanded={menu === "model"} onClick={() => { setMenu(menu === "model" ? null : "model"); setReasoningMenu(null); }}>{selectedModel.label}<WorkspaceIcon name="down" width="13" height="13" /></button>
              {menu === "model" ? <div className={styles.modelMenu} role="dialog" aria-label="Model and reasoning">
                <div className={styles.menuLabel}>Model</div>
                {taskModels.map(item => <div key={item.id} className={styles.modelRow} onPointerEnter={event => { if (event.pointerType === "mouse") setReasoningMenu(item.id); }}>
                  <button type="button" className={styles.modelOption} aria-pressed={model === item.id} aria-expanded={reasoningMenu === item.id} onClick={() => { setModel(item.id); setReasoningMenu(item.id); }} onKeyDown={event => { if (event.key === "ArrowRight") { event.preventDefault(); setReasoningMenu(item.id); } }}><span className={styles.checkSlot}>{model === item.id ? <WorkspaceIcon name="check" width="15" height="15" /> : null}</span><span>{item.label}</span><WorkspaceIcon name="chevron" width="14" height="14" /></button>
                  {reasoningMenu === item.id ? <div className={styles.reasoningMenu} role="group" aria-label={`Reasoning for ${item.label}`}><div className={styles.menuLabel}>Reasoning</div>{reasoningLevels.map(level => <button type="button" key={level} aria-pressed={reasoning === level} onClick={() => { setModel(item.id); setReasoning(level); setMenu(null); setReasoningMenu(null); modelButton.current?.focus(); }}><span className={styles.checkSlot}>{reasoning === level ? <WorkspaceIcon name="check" width="15" height="15" /> : null}</span>{level.charAt(0).toUpperCase() + level.slice(1)}</button>)}</div> : null}
                </div>)}
              </div> : null}
            </div>
          </div>
          <button type="submit" className={styles.submitInstruction} disabled={!prompt.trim() || loading || Boolean(workspaceError) || pending || readingFiles} aria-label={pending ? "Saving task" : "Create task"} title="Create task"><WorkspaceIcon name="arrow" width="20" height="20" /></button>
        </div>
        <input ref={fileInput} type="file" multiple hidden onChange={event => { void attachFiles(Array.from(event.target.files ?? [])); event.target.value = ""; }} />
      </form>
      <div className={styles.connectionTray}>
        {agents.length === 1 ? <span><span className={styles.connectionDot} data-connected />{agents[0].name} connected</span> : agents.length ? <span className={styles.agentPicker}><WorkspaceIcon name="terminal" width="16" height="16" /><select aria-label="Agent for this task" value={selectedAgent?.id ?? ""} onChange={event => setAgentId(event.target.value)}><option value="">Any connected agent</option>{agents.map(agent => <option key={agent.id} value={agent.id}>{agent.name}</option>)}</select></span> : <span><WorkspaceIcon name="terminal" width="16" height="16" />Bring your agent</span>}
        <ConnectAgentButton>Connect<WorkspaceIcon name="external" width="14" height="14" /></ConnectAgentButton>
      </div>
    </div>
    {readingFiles ? <p role="status" className={styles.composerNote}>Reading attachments…</p> : null}
    {error && !pickerOpen ? <p role="alert" className={styles.error}>{error}</p> : null}
    <dialog ref={dialog} className={styles.handoff} aria-labelledby="context-title" onClose={() => setPickerOpen(false)}>
      <div className={styles.handoffHeader}><h2 id="context-title">{picker === "repository" ? "Add a repository" : picker === "app" ? "Add an app" : "Add a previous task"}</h2><button type="button" className={styles.iconButton} aria-label="Close context picker" onClick={() => { dialog.current?.close(); setError(""); }}><WorkspaceIcon name="close" /></button></div>
      {picker === "repository" ? <form onSubmit={addRepository}><label className={styles.pickerLabel} htmlFor="repository-url">Repository URL</label><input id="repository-url" type="url" required placeholder="https://github.com/owner/repository" className={styles.pickerInput} value={repositoryUrl} onChange={event => setRepositoryUrl(event.target.value)} /><p className={styles.pickerNote}>Your agent uses its own access to read the repository.</p><button type="submit" className={styles.primaryButton}>Add repository</button></form> : <>
        <input className={styles.pickerInput} aria-label={picker === "app" ? "Search apps" : "Search tasks"} placeholder={picker === "app" ? "Search apps…" : "Search tasks…"} value={search} onChange={event => setSearch(event.target.value)} />
        <div className={styles.contextResults}>{picker === "app" ? (data?.systems ?? []).filter(item => item.contract.metadata.name.toLowerCase().includes(search.toLowerCase())).map(item => <button type="button" key={item.contract.metadata.name} onClick={() => addContext({ id: crypto.randomUUID(), kind: "app", name: item.contract.metadata.name, referenceId: item.contract.metadata.name })}><WorkspaceIcon name="apps" /><span>{item.contract.metadata.name}</span><WorkspaceIcon name="plus" width="14" height="14" /></button>) : (data?.tasks ?? []).filter(item => item.prompt.toLowerCase().includes(search.toLowerCase())).map(item => <button type="button" key={item.id} onClick={() => addContext({ id: crypto.randomUUID(), kind: "task", name: item.prompt.slice(0, 100), referenceId: item.id })}><WorkspaceIcon name="message" /><span>{item.prompt.slice(0, 100)}</span><WorkspaceIcon name="plus" width="14" height="14" /></button>)}</div>
        {!(picker === "app" ? data?.systems.some(item => item.contract.metadata.name.toLowerCase().includes(search.toLowerCase())) : data?.tasks.some(item => item.prompt.toLowerCase().includes(search.toLowerCase()))) ? <p className={styles.pickerNote}>{search ? "No matches." : picker === "app" ? "No apps in this workspace yet." : "No previous tasks yet."}</p> : null}
      </>}
      {error ? <p role="alert" className={styles.error}>{error}</p> : null}
    </dialog>
  </>;
}
