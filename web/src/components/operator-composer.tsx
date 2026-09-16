"use client";
import { useEffect, useRef, useState, type RefObject } from "react";
import { type OperatorSurface, surfaceLabels } from "@/lib/operator-api";
import { WorkspaceIcon } from "./workspace-icon";
import shared from "./workspace.module.css";
import styles from "./operator-workspace.module.css";

export function OperatorComposer({ draft, onChange, onSend, onStop, running, disabled, inputRef, selected, model, onSelect }: { draft: string; onChange: (text: string) => void; onSend: () => void; onStop: () => void; running: boolean; disabled: boolean; inputRef: RefObject<HTMLTextAreaElement | null>; selected: OperatorSurface | null; model?: string; onSelect: (surface: OperatorSurface) => void }) {
  const [menu, setMenu] = useState<"context" | "model" | null>(null);
  const toolbar = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!menu) return;
    const dismiss = (event: PointerEvent) => { if (event.target instanceof Node && !toolbar.current?.contains(event.target)) setMenu(null); };
    const escape = (event: KeyboardEvent) => { if (event.key === "Escape") setMenu(null); };
    document.addEventListener("pointerdown", dismiss); document.addEventListener("keydown", escape);
    return () => { document.removeEventListener("pointerdown", dismiss); document.removeEventListener("keydown", escape); };
  }, [menu]);
  const modelLabel = model === "openai/gpt-5.6-luna" ? "GPT-5.6 Luna" : model?.split("/").at(-1) ?? "Connecting…";
  return <div className={`${shared.composerCard} ${styles.promptCard}`}><form className={shared.composer} onSubmit={event => { event.preventDefault(); onSend(); }}>
    {selected ? <div className={styles.context}><WorkspaceIcon name="panel" width="13" height="13" />{selected.path ?? selected.system ?? selected.repository ?? surfaceLabels[selected.kind]}</div> : null}
    <textarea ref={inputRef} aria-label="Message Canter" placeholder={running ? "Guide Canter’s next step…" : "Ask Canter to do something…"} rows={3} value={draft} maxLength={20000} onChange={event => onChange(event.target.value)} onKeyDown={event => { if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) { event.preventDefault(); if (!running) onSend(); } }} />
    <div ref={toolbar} className={shared.composerToolbar}><div className={shared.composerControls}>
      <div className={styles.menuAnchor}><button type="button" className={shared.iconButton} aria-label="Add workspace context" aria-expanded={menu === "context"} onClick={() => setMenu(menu === "context" ? null : "context")}><WorkspaceIcon name="plus" /></button>
        {menu === "context" ? <div className={`${shared.contextMenu} ${styles.composerMenu}`}><div className={shared.menuLabel}>Open workspace context</div>{(["github", "apps", "deployments", "billing"] as const).map(kind => <button key={kind} type="button" onClick={() => { onSelect({ kind }); setMenu(null); inputRef.current?.focus(); }}><WorkspaceIcon name={kind === "github" ? "folder" : "panel"} />{surfaceLabels[kind]}</button>)}</div> : null}
      </div>
      <div className={styles.menuAnchor}><button type="button" className={shared.modelTrigger} aria-label="Agent model" aria-expanded={menu === "model"} onClick={() => setMenu(menu === "model" ? null : "model")}>{modelLabel}<WorkspaceIcon name="down" width="12" height="12" /></button>
        {menu === "model" ? <div className={`${shared.modelMenu} ${styles.composerMenu}`}><div className={shared.menuLabel}>Workspace agent</div><div className={styles.configuredModel}><WorkspaceIcon name="check" width="14" height="14" />{modelLabel}</div><p className={styles.modelNote}>Uses your workspace’s configured model for conversations and tools.</p></div> : null}
      </div>
    </div>{running ? <button type="button" className={styles.stopCircle} aria-label="Stop response" onClick={onStop}><span /></button> : <button type="submit" className={shared.submitInstruction} aria-label="Send message" disabled={disabled || !draft.trim()}><WorkspaceIcon name="arrow" width="20" height="20" /></button>}</div>
    {running && draft ? <p className={styles.draftHint}>Your draft is saved. Send it when Canter finishes, or stop to redirect.</p> : null}
  </form></div>;
}
