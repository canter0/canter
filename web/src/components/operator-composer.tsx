"use client";
import { useEffect, useRef, useState, type RefObject } from "react";
import { type OperatorAttachment, type OperatorSurface, surfaceLabels } from "@/lib/operator-api";
import { attachmentAccept, readAttachmentBatch, transferredFiles } from "@/lib/operator-attachments";
import { OperatorAttachments } from "./operator-attachments";
import { useWorkspace } from "./workspace-context";
import { contextMention } from "@/lib/github-mention";
import { GitHubMentionPicker } from "./github-mention-picker";
import { WorkspaceIcon } from "./workspace-icon";
import shared from "./workspace.module.css";
import styles from "./operator-workspace.module.css";

type Props = { draft: string; onChange: (text: string) => void; onSend: () => void; onStop: () => void; running: boolean; disabled: boolean; sending: boolean; inputRef: RefObject<HTMLTextAreaElement | null>; model?: string; onSelect: (surface: OperatorSurface) => void; attachments: OperatorAttachment[]; onAttachments: (items: OperatorAttachment[]) => void; onClearContext: () => void; context: OperatorSurface | null };
export function OperatorComposer({ draft, onChange, onSend, onStop, running, disabled, sending, inputRef, model, onSelect, attachments, onAttachments, context, onClearContext }: Props) {
  const [menu, setMenu] = useState<"context" | "model" | null>(null);
  const { data } = useWorkspace();
  const [caret, setCaret] = useState(draft.length);
  const [mentionDismissed, setMentionDismissed] = useState(false);
  const mention = mentionDismissed ? null : contextMention(draft, caret);
  function insertContext(kind: "github" | "apps" | "deployments" | "billing" | "conversations") {
    const input = inputRef.current;
    const start = input?.selectionStart ?? draft.length;
    const end = input?.selectionEnd ?? start;
    const token = `${start && !/\s/.test(draft[start - 1]) ? " " : ""}@${kind} `;
    onChange(draft.slice(0, start) + token + draft.slice(end));
    const next = start + token.length;
    setCaret(next); setMentionDismissed(kind === "billing"); setMenu(null);
    if (kind === "billing") onSelect({ kind: "billing" });
    requestAnimationFrame(() => { input?.focus(); input?.setSelectionRange(next, next); });
  }
  function pickMention(choice: { token: string; surface: OperatorSurface }) {
    if (!mention) return;
    const isCategory = ["github", "apps", "deployments"].includes(choice.surface.kind);
    const token = `@${choice.token} `;
    onChange(draft.slice(0, mention.start) + token + draft.slice(mention.end));
    if (!isCategory) onSelect(choice.surface);
    const next = mention.start + token.length;
    setCaret(next); setMentionDismissed(!isCategory);
    requestAnimationFrame(() => { inputRef.current?.focus(); inputRef.current?.setSelectionRange(next, next); });
  }
  const [reading, setReading] = useState(false);
  const [dragging, setDragging] = useState(false);
  const [error, setError] = useState("");
  const toolbar = useRef<HTMLDivElement>(null);
  const fileInput = useRef<HTMLInputElement>(null);
  const addButton = useRef<HTMLButtonElement>(null);
  const modelButton = useRef<HTMLButtonElement>(null);
  const readLock = useRef(false);
  const queuedFiles = useRef<File[]>([]);
  const attachmentsRef = useRef(attachments);
  useEffect(() => { attachmentsRef.current = attachments; }, [attachments]);
  useEffect(() => {
    // An accidental drop outside the input must not navigate away from the draft.
    const preventFileNavigation = (event: DragEvent) => { if (event.dataTransfer?.types.includes("Files")) event.preventDefault(); };
    window.addEventListener("dragover", preventFileNavigation);
    window.addEventListener("drop", preventFileNavigation);
    return () => { window.removeEventListener("dragover", preventFileNavigation); window.removeEventListener("drop", preventFileNavigation); };
  }, []);
  useEffect(() => {
    const input = inputRef.current;
    if (input) { input.style.height = "0px"; input.style.height = `${Math.min(220, Math.max(64, input.scrollHeight))}px`; }
  }, [draft, inputRef]);
  useEffect(() => {
    if (!menu) return;
    const dismiss = (event: PointerEvent) => { if (event.target instanceof Node && !toolbar.current?.contains(event.target)) setMenu(null); };
    const escape = (event: KeyboardEvent) => { if (event.key === "Escape") { setMenu(null); (menu === "context" ? addButton : modelButton).current?.focus(); } };
    document.addEventListener("pointerdown", dismiss); document.addEventListener("keydown", escape);
    return () => { document.removeEventListener("pointerdown", dismiss); document.removeEventListener("keydown", escape); };
  }, [menu]);
  async function attach(files: File[]) {
    if (!files.length || sending) return;
    queuedFiles.current.push(...files);
    if (readLock.current) return;
    readLock.current = true; setReading(true); setError(""); setMenu(null); setMentionDismissed(true);
    const errors: string[] = [];
    try {
      while (queuedFiles.current.length) {
        const batch = queuedFiles.current.splice(0);
        const result = await readAttachmentBatch(batch, attachmentsRef.current);
        attachmentsRef.current = [...attachmentsRef.current, ...result.items];
        onAttachments(attachmentsRef.current);
        errors.push(...result.errors);
        setError(errors.join("\n"));
      }
    } finally { readLock.current = false; setReading(false); }
  }
  const modelLabel = model === "openai/gpt-5.6-luna" ? "GPT 5.6 Luna" : model?.split("/").at(-1) ?? "Connecting…";
  const canSend = !disabled && !reading && (!!draft.trim() || attachments.length > 0);
  return <div className={styles.promptCard} data-dragging={dragging} data-sending={sending} data-running={running} onDragOver={event => { if (event.dataTransfer.types.includes("Files")) { event.preventDefault(); setDragging(true); } }} onDragLeave={event => { if (!event.currentTarget.contains(event.relatedTarget as Node | null)) setDragging(false); }} onDrop={event => { if (transferredFiles(event.dataTransfer).length) { event.preventDefault(); setDragging(false); void attach(transferredFiles(event.dataTransfer)); } }}>
    {mention ? <GitHubMentionPicker workspaceId={data?.workspace.id} query={mention.query} category={mention.category} systems={(data?.systems ?? []).map(item => item.contract.metadata.name)} deployments={data?.initialDeployments ?? []} conversations={data?.conversations ?? []} inputRef={inputRef} onPick={pickMention} onClose={() => setMentionDismissed(true)} /> : null}
    <form className={styles.promptForm} onSubmit={event => { event.preventDefault(); if (canSend) onSend(); }}>
      {attachments.length ? <OperatorAttachments items={attachments} disabled={sending || reading} onRemove={id => onAttachments(attachments.filter(item => item.id !== id))} /> : null}
      <textarea ref={inputRef} aria-label="Message Canter" aria-controls={mention ? "context-mention-picker" : undefined} aria-haspopup="dialog" placeholder={running ? "Guide Canter’s next step…" : "Ask Canter to build, deploy, or explore your code…"} rows={2} value={draft} disabled={sending} maxLength={20000} onChange={event => { onChange(event.target.value); setCaret(event.target.selectionStart); setMentionDismissed(false); }} onSelect={event => setCaret(event.currentTarget.selectionStart)} onPaste={event => { const files = transferredFiles(event.clipboardData); if (files.length) { event.preventDefault(); void attach(files); } }} onKeyDown={event => { if (event.defaultPrevented) return; if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) { event.preventDefault(); if (!event.repeat && canSend) onSend(); } }} />
      {context ? <button className={styles.contextPill} type="button" aria-label="Remove context" title="Remove context" onClick={onClearContext}><WorkspaceIcon name={context.kind === "conversation" ? "message" : "file"} width="13" height="13" />{context.kind === "conversation" ? data?.conversations.find(item => item.id === context.id)?.title ?? "Conversation" : context.path?.split("/").at(-1) ?? context.repository ?? context.system ?? surfaceLabels[context.kind]}<WorkspaceIcon name="close" width="12" height="12" /></button> : null}
      <div ref={toolbar} className={styles.promptToolbar}><div className={styles.promptControls}>
        <div className={styles.menuAnchor}><button ref={addButton} type="button" className={shared.iconButton} aria-label="Attach files or add context" title="Attach files or add context" aria-expanded={menu === "context"} disabled={sending} onClick={() => setMenu(menu === "context" ? null : "context")}><WorkspaceIcon name="plus" /></button>
          {menu === "context" ? <div className={`${shared.contextMenu} ${styles.composerMenu} ${styles.contextPicker}`} role="dialog" aria-label="Attach files or add context">
            <button type="button" onClick={() => { setMenu(null); fileInput.current?.click(); }}><WorkspaceIcon name="attachment" />Upload images or files</button><p className={styles.uploadHint}>Up to 4 files · 2 MB each · 5 MB total</p><div className={styles.menuDivider} />
            {(["github", "apps", "deployments", "billing", "conversations"] as const).map(kind => <button key={kind} type="button" onClick={() => insertContext(kind)}><WorkspaceIcon name={kind === "github" ? "folder" : kind === "apps" ? "apps" : kind === "conversations" ? "message" : kind === "billing" ? "file" : "activity"} />{kind === "github" ? "GitHub repositories" : kind === "conversations" ? "Conversations" : surfaceLabels[kind]}</button>)}

          </div> : null}
        </div>
        <div className={styles.menuAnchor}><button ref={modelButton} type="button" className={shared.modelTrigger} aria-label="Model" aria-expanded={menu === "model"} onClick={() => setMenu(menu === "model" ? null : "model")}>{modelLabel}<WorkspaceIcon name="down" width="12" height="12" /></button>
          {menu === "model" ? <div className={`${shared.modelMenu} ${styles.composerMenu}`}><div className={shared.menuLabel}>Model</div><button type="button" className={styles.configuredModel} onClick={() => { setMenu(null); modelButton.current?.focus(); }}><WorkspaceIcon name="check" width="14" height="14" />{modelLabel}</button></div> : null}
        </div>
      </div><div className={styles.promptControls}>{running ? <button type="button" className={styles.stopCircle} aria-label="Stop response" onClick={onStop}><span /></button> : null}{!running || canSend ? <button type="submit" className={shared.submitInstruction} aria-label="Send message" title="Send message · Enter" disabled={!canSend}><WorkspaceIcon name="arrow" width="18" height="18" /></button> : null}</div></div>
      <input ref={fileInput} type="file" accept={attachmentAccept} multiple hidden onChange={event => { void attach(Array.from(event.target.files ?? [])); event.target.value = ""; }} />
      {reading ? <p className={styles.draftHint} role="status">Preparing attachments… You can keep typing.</p> : null}
      {error ? <p className={styles.attachmentError} role="alert">{error}</p> : null}
      {running && (draft || attachments.length) ? <p className={styles.draftHint}>Send to guide Canter’s next step.</p> : null}
    </form>
    {dragging ? <div className={styles.dropOverlay}><WorkspaceIcon name="attachment" /><span>Drop images or files</span></div> : null}
  </div>;
}
