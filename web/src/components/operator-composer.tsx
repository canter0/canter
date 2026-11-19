"use client";
import { useCallback, useEffect, useLayoutEffect, useId, useRef, useState, useSyncExternalStore, type RefObject } from "react";
import { appendDictationDraft } from "@/lib/wren-dictation";
import { useWrenDictation } from "./use-wren-dictation";
import { WrenDictationControl, WrenDictationStatus } from "./wren-dictation-control";
import { usePopover } from "./use-popover";
import { focusMenuItemUnderPointer, moveMenuFocus } from "@/lib/interaction";
import { type OperatorAttachment, type OperatorSurface, surfaceLabels } from "@/lib/operator-api";
import { attachmentAccept, attachmentBytes, attachmentLimits, attachmentSize, readAttachmentBatch, transferredFiles } from "@/lib/operator-attachments";
import { OperatorAttachments } from "./operator-attachments";
import type { OperatorModelOptions } from "@/lib/operator-models";
import { OperatorModelPicker } from "./operator-model-picker";
import { useWorkspace } from "./workspace-context";
import { contextMention } from "@/lib/github-mention";
import { GitHubMentionPicker } from "./github-mention-picker";
import { WrenMentionPicker } from "./wren-mention-picker";
import { wrenMention, type WrenMentionChoice } from "@/lib/wren-mentions";
import type { Wren } from "@/lib/wrens";
import { WorkspaceIcon } from "./workspace-icon";
import { ProviderIcon } from "./provider-icon";
import { MotionPresence } from "./motion-presence";
import { WrenAppsMenu } from "./wren-apps-menu";
import shared from "./workspace.module.css";
import styles from "./operator-workspace.module.css";

const dismissedHints = new Set<string>();
const subscribeReady = () => () => {};
const clientReady = () => true;
const serverReady = () => false;
const subscribeHint = (onChange: () => void) => {
  window.addEventListener("storage", onChange);
  window.addEventListener("canter-composer-hint", onChange);
  return () => { window.removeEventListener("storage", onChange); window.removeEventListener("canter-composer-hint", onChange); };
};

type Props = { wrens?: Wren[]; sharedConversation?: boolean; wrenApps?: boolean; apps?: boolean; conversationId?: string; managedModel?: boolean; replying?: boolean; recipient?: string; draft: string; onChange: (text: string) => void; onSend: () => void; onStop: () => void; running: boolean; stopLabel?: string; stopping?: boolean; disabled: boolean; sending: boolean; inputRef: RefObject<HTMLTextAreaElement | null>; model?: string; onModelChange: (model: string) => void; modelOptions: Record<string, OperatorModelOptions>; onModelOptionsChange: (model: string, options: OperatorModelOptions) => void; onSelect: (surface: OperatorSurface) => void; attachments: OperatorAttachment[]; onAttachments: (items: OperatorAttachment[]) => void; onClearContext: () => void; context: OperatorSurface | null };
export function OperatorComposer({ wrens = [], sharedConversation = false, wrenApps = false, apps = false, conversationId, draft, onChange, onSend, onStop, running, stopLabel = "Stop response", stopping, disabled, sending, inputRef, model, onModelChange, modelOptions, onModelOptionsChange, onSelect, attachments, onAttachments, context, onClearContext, recipient, replying = false, managedModel = false, placeholder, dictationScope }: Props & {placeholder?:string;dictationScope?:string}) {
  const [menu, setMenu] = useState<"context" | "model" | "apps" | null>(null);
  const { data } = useWorkspace();
  const ready = useSyncExternalStore(subscribeReady, clientReady, serverReady);
  const hintKey = `canter:composer-hint-dismissed:${data?.account.id ?? "local"}`;
  const hintDismissed = useSyncExternalStore(subscribeHint, () => {
    try { return dismissedHints.has(hintKey) || localStorage.getItem(hintKey) === "true"; } catch { return dismissedHints.has(hintKey); }
  }, () => true);
  useEffect(() => {
    if (!draft || hintDismissed) return;
    dismissedHints.add(hintKey);
    try { localStorage.setItem(hintKey, "true"); } catch { /* Dismiss for this visit when storage is unavailable. */ }
    window.dispatchEvent(new Event("canter-composer-hint"));
  }, [draft, hintDismissed, hintKey]);
  const [caret, setCaret] = useState(draft.length);
  const [mentionDismissed, setMentionDismissed] = useState(false);
  const mention = mentionDismissed ? null : wrenApps ? wrenMention(draft, caret, wrens) : contextMention(draft, caret);
  const wrenPicker = wrenApps && !!mention && (mention.category !== "github" && (mention.category !== "conversations" || sharedConversation));
  const contextCategories = wrenApps ? sharedConversation ? ["github"] as const : ["github", "conversations"] as const : ["github", "apps", "deployments", "billing", "conversations"] as const;
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
  function pickWrenMention(choice: WrenMentionChoice) {
    if (!mention) return;
    if (choice.kind === "apps") {
      onChange(draft.slice(0, mention.start) + draft.slice(mention.end));
      setMentionDismissed(true);
      setMenu("apps");
      return;
    }
    const token = `@${choice.token} `;
    onChange(draft.slice(0, mention.start) + token + draft.slice(mention.end));
    const next = mention.start + token.length;
    setCaret(next); setMentionDismissed(choice.kind === "wren" || choice.kind === "team");
    requestAnimationFrame(() => { inputRef.current?.focus(); inputRef.current?.setSelectionRange(next,next); });
  }
  function openApps() {
    setMenu("apps"); setMentionDismissed(true);
  }
  const dictation = useWrenDictation({workspaceId:data?.workspace.id,conversationId,draftScope:dictationScope,accountId:data?.account.id,disabled:disabled||sending,onTranscript:text=>{
    const next=appendDictationDraft(inputRef.current?.value??draft,text);
    if(next===null)return false;
    onChange(next);setCaret(next.length);setMentionDismissed(true);
    requestAnimationFrame(()=>{if(document.activeElement===inputRef.current)inputRef.current?.setSelectionRange(next.length,next.length);});
    return true;
  }});
  const [reading, setReading] = useState(false);
  const [dragging, setDragging] = useState(false);
  const [attachmentErrors, setAttachmentErrors] = useState<string[]>([]);
  const contextMenu = useRef<HTMLDivElement>(null);
  const appsMenu = useRef<HTMLDivElement>(null);
  const closeMenu = useCallback(() => setMenu(null), []);
  const changeModelMenu = useCallback((open: boolean) => setMenu(open ? "model" : null), []);
  const hintId = useId();
  const uploadHintId = useId();
  const contextLabelId = useId();
  usePopover(menu === "context", contextMenu, closeMenu);
  usePopover(menu === "apps", appsMenu, closeMenu);
  const appsEnabled = wrenApps || apps;
  const fileInput = useRef<HTMLInputElement>(null);
  const addButton = useRef<HTMLButtonElement>(null);
  const readLock = useRef(false);
  const resizeAnimation = useRef<Animation | null>(null);
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
  useLayoutEffect(() => {
    const input = inputRef.current;
    if (!input) return;
    const measure = () => {
      const previous = input.getBoundingClientRect().height;
      resizeAnimation.current?.cancel();
      input.style.height = "0px";
      const height = Math.min(220, Math.max(28, input.scrollHeight));
      input.style.height = `${height}px`;
      if (Math.abs(height - previous) > 1 && !window.matchMedia("(prefers-reduced-motion: reduce)").matches) {
        resizeAnimation.current = input.animate([{ height: `${previous}px` }, { height: `${height}px` }], { duration: 240, easing: "cubic-bezier(0.22, 1, 0.36, 1)" });
      }
    };
    measure();
    let width = input.clientWidth;
    const observer = new ResizeObserver(() => { if (width !== input.clientWidth) { width = input.clientWidth; measure(); } });
    observer.observe(input);
    return () => observer.disconnect();
  }, [draft, inputRef]);
  useEffect(() => () => resizeAnimation.current?.cancel(), []);
  async function attach(files: File[]) {
    if (!files.length || sending) return;
    queuedFiles.current.push(...files);
    if (readLock.current) return;
    readLock.current = true; setReading(true); setAttachmentErrors([]); setMenu(null); setMentionDismissed(true);
    const errors: string[] = [];
    try {
      while (queuedFiles.current.length) {
        const batch = queuedFiles.current.splice(0);
        const result = await readAttachmentBatch(batch, attachmentsRef.current);
        attachmentsRef.current = [...attachmentsRef.current, ...result.items];
        if (result.items.length) onAttachments(attachmentsRef.current);
        errors.push(...result.errors);
        setAttachmentErrors([...errors]);
      }
    } finally { readLock.current = false; setReading(false); }
  }
  const canSend = ready && !disabled && !reading && !dictation.busy && (!!draft.trim() || attachments.length > 0);
  const showStop = running && !canSend;
  const statusHint = sending ? "Sending…" : stopping ? "Requesting stop…" : running && (draft || attachments.length) ? "Send another message while work continues." : "";
  const showHint = !!statusHint || (!hintDismissed && !draft);
  const attachedBytes = attachmentBytes(attachments);
  const attachmentFull = attachments.length >= attachmentLimits.count || attachedBytes >= attachmentLimits.totalMB * 1048576;
  return <div className={styles.promptCard} data-operator-composer data-dragging={dragging} data-sending={sending} data-running={running} onDragOver={event => { if (event.dataTransfer.types.includes("Files")) { event.preventDefault(); event.dataTransfer.dropEffect = sending || attachmentFull ? "none" : "copy"; setDragging(!sending); } }} onDragLeave={event => { if (!event.currentTarget.contains(event.relatedTarget as Node | null)) setDragging(false); }} onDrop={event => { setDragging(false); const files = transferredFiles(event.dataTransfer); if (files.length) { event.preventDefault(); void attach(files); } }}>
    {mention && wrenPicker ? <WrenMentionPicker wrens={wrens} shared={sharedConversation} query={mention.query} blockedConversation={sharedConversation && mention.category === "conversations"} inputRef={inputRef} onPick={pickWrenMention} onClose={() => setMentionDismissed(true)}/> : mention ? <GitHubMentionPicker workspaceId={data?.workspace.id} query={mention.query} category={mention.category} systems={(data?.systems ?? []).map(item => item.contract.metadata.name)} deployments={data?.initialDeployments ?? []} conversations={sharedConversation ? [] : data?.conversations ?? []} inputRef={inputRef} onPick={pickMention} onClose={() => setMentionDismissed(true)} /> : null}
    <form className={styles.promptForm} data-has-apps={appsEnabled||undefined} aria-busy={sending || reading || dictation.busy} onSubmit={event => { event.preventDefault(); if (canSend) onSend(); }}>
      <div className={styles.composerInput}>
      {attachments.length ? <OperatorAttachments items={attachments} disabled={sending || reading} onRemove={id => onAttachments(attachments.filter(item => item.id !== id))} /> : null}
      {reading ? <p className={styles.attachmentStatus} role="status">Preparing attachments…</p> : null}
      <textarea ref={inputRef} aria-label={recipient ? `Message ${recipient}` : "Message Canter"} aria-controls={mention ? "context-mention-picker" : undefined} aria-haspopup="dialog" placeholder={placeholder??(replying ? "Reply to message…" : recipient ? `Message ${recipient}…` : "Ask Canter anything…")} aria-describedby={showHint ? hintId : undefined} rows={1} value={draft} readOnly={sending} maxLength={20000} onChange={event => { onChange(event.target.value); setCaret(event.target.selectionStart); setMentionDismissed(false); }} onSelect={event => setCaret(event.currentTarget.selectionStart)} onPaste={event => { const files = transferredFiles(event.clipboardData); if (files.length) { event.preventDefault(); void attach(files); } }} onKeyDown={event => { if (event.defaultPrevented) return; if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) { event.preventDefault(); if (!event.repeat && canSend) onSend(); } }} />
      {context ? <button className={styles.contextPill} type="button" aria-label="Remove context" title="Remove context" onClick={onClearContext}><WorkspaceIcon name={context.kind === "conversation" ? "message" : "file"} width="13" height="13" />{context.kind === "conversation" ? data?.conversations.find(item => item.id === context.id)?.title ?? "Conversation" : context.path?.split("/").at(-1) ?? context.repository ?? context.system ?? surfaceLabels[context.kind]}<WorkspaceIcon name="close" width="12" height="12" /></button> : null}
      </div>
      <div className={styles.promptToolbar}><div className={styles.promptControls}>
        <div ref={contextMenu} className={styles.composerAddAnchor}><button ref={addButton} type="button" className={`${shared.iconButton} ${styles.composerAdd}`} aria-label="Attach files or add context" title="Attach files or add context" aria-haspopup="menu" aria-expanded={menu === "context"} disabled={sending} onClick={() => setMenu(menu === "context" ? null : "context")}><WorkspaceIcon name="plus" /></button>
          <MotionPresence open={menu === "context"}><div className={`${shared.contextMenu} ${styles.composerMenu} ${styles.contextPicker}`} role="menu" aria-label="Attach files or add context" onKeyDown={event => { event.currentTarget.dataset.keyboard = ""; moveMenuFocus(event); }} onPointerMove={focusMenuItemUnderPointer}>
            <button type="button" role="menuitem" tabIndex={-1} className={styles.uploadItem} aria-describedby={uploadHintId} onClick={() => { setMenu(null); fileInput.current?.click(); requestAnimationFrame(() => addButton.current?.focus()); }}>
              <WorkspaceIcon name="attachment" />
              <span>Upload images or files
                <small id={uploadHintId} aria-hidden="true">
                  {attachments.length ? `${attachments.length}/${attachmentLimits.count} files · ${attachmentSize(attachedBytes)} of ${attachmentLimits.totalMB} MB` : `${attachmentLimits.count} files · ${attachmentLimits.fileMB} MB each · ${attachmentLimits.totalMB} MB total`}
                </small>
              </span>
            </button>
            <div className={styles.menuDivider} role="separator" />
            <div role="group" aria-labelledby={contextLabelId}>
              <div id={contextLabelId} className={styles.contextLabel}>Add context<kbd aria-hidden="true">@</kbd></div>
              {contextCategories.map(kind => <button key={kind} type="button" role="menuitem" tabIndex={-1} onClick={() => insertContext(kind)}>{kind === "github" ? <ProviderIcon provider="github" /> : <WorkspaceIcon name={kind === "apps" ? "apps" : kind === "conversations" ? "message" : kind === "billing" ? "file" : "activity"} />}{kind === "github" ? "GitHub repositories" : kind === "conversations" ? "Conversations" : surfaceLabels[kind]}</button>)}
            </div>
            {appsEnabled ? <><div className={styles.menuDivider} role="separator" /><button type="button" role="menuitem" tabIndex={-1} onClick={openApps}><WorkspaceIcon name="apps"/>Apps and connections</button></> : null}

          </div></MotionPresence>
        </div>
        {appsEnabled?<div ref={appsMenu} className={styles.menuAnchor}><MotionPresence open={menu==="apps"}>{data?<WrenAppsMenu workspaceId={data.workspace.id} conversationId={conversationId} onClose={closeMenu}/>:null}</MotionPresence></div>:null}
      </div><div className={styles.promptControls}>
        <WrenDictationControl dictation={dictation}/>
        {running && canSend ? <button type="button" className={styles.stopCircle} aria-label={stopping ? "Requesting stop" : stopLabel} title={`${stopLabel} · Completed operations remain saved`} disabled={!ready || stopping} onClick={onStop}><span /></button> : null}
        <button type={showStop ? "button" : "submit"} className={styles.primaryAction} data-stop={showStop} aria-label={showStop ? stopping ? "Requesting stop" : stopLabel : sending ? "Sending message" : "Send message"} title={showStop ? `${stopLabel} · Completed operations remain saved` : "Send message · Enter"} disabled={showStop ? !ready || stopping : !canSend} onClick={showStop ? onStop : undefined}>
          <span className={styles.actionGlyph} data-visible={!showStop}><WorkspaceIcon name="arrow" width="18" height="18" /></span>
          <span className={styles.actionGlyph} data-visible={showStop}><span className={styles.stopSquare} /></span>
        </button></div></div>
      <input ref={fileInput} type="file" accept={attachmentAccept} multiple hidden onChange={event => { void attach(Array.from(event.target.files ?? [])); event.target.value = ""; }} />
      {dragging ? <div className={styles.dropOverlay}><WorkspaceIcon name="attachment" /><span>{attachmentFull ? "Remove an attachment to make room" : "Drop files to attach"}</span></div> : null}
    </form>
    <div className={styles.composerMeta} data-managed-model={managedModel || undefined}>
      {!managedModel ? <OperatorModelPicker model={model} onChange={onModelChange} options={modelOptions} onOptionsChange={onModelOptionsChange} open={menu === "model"} onOpenChange={changeModelMenu} disabled={sending} /> : null}
      <div className={styles.composerHintReveal} data-visible={showHint} aria-hidden={!showHint}><div><p id={hintId} className={styles.composerHint} role="status">{statusHint || <><span>Enter to send · Shift + Enter for a new line</span><span>{wrenApps ? "@ to mention a Wren or source" : "@ to add context"}</span></>}</p></div></div>
    </div>
      <WrenDictationStatus dictation={dictation}/>
      {attachmentErrors.length ? (
        <div className={styles.attachmentError}>
          <div role="alert">
            {attachmentErrors.length === 1 ? attachmentErrors[0] : <details>
              <summary>{attachmentErrors.length} files couldn’t be attached</summary>
              <ul>{attachmentErrors.map((error, i) => <li key={i}>{error}</li>)}</ul>
            </details>}
          </div>
          <button type="button" aria-label="Dismiss attachment errors" onClick={() => setAttachmentErrors([])}><WorkspaceIcon name="close" width="14" height="14" /></button>
        </div>
      ) : null}
  </div>;
}
