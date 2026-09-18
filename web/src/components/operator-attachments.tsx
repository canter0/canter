"use client";
import { useRef, useState } from "react";
import type { OperatorAttachment } from "@/lib/operator-api";
import { attachmentSize, attachmentURL } from "@/lib/operator-attachments";
import { WorkspaceIcon } from "./workspace-icon";
import styles from "./operator-workspace.module.css";

export function OperatorAttachments({ items, onRemove, disabled }: { items: OperatorAttachment[]; onRemove?: (id: string) => void; disabled?: boolean }) {
  const dialog = useRef<HTMLDialogElement>(null);
  const [preview, setPreview] = useState<OperatorAttachment | null>(null);
  return <>
    <div className={styles.attachments} aria-label="Attachments">{items.map(item => <div key={item.id} className={styles.attachment} data-image={item.mediaType.startsWith("image/")}>
      <button type="button" className={styles.attachmentPreview} aria-label={`Preview ${item.name}`} title={`${item.name} · ${attachmentSize(item.size)}`} onClick={() => { setPreview(item); dialog.current?.showModal(); }}>
        {/* Local validated data URLs do not need the remote image optimizer. */}
        {/* eslint-disable-next-line @next/next/no-img-element */}
        {item.mediaType.startsWith("image/") ? <img src={attachmentURL(item)} alt="" /> : <span className={styles.attachmentIcon}><WorkspaceIcon name="file" /></span>}
        <span><strong>{item.name}</strong><small>{attachmentSize(item.size)}</small></span>
      </button>
      {onRemove ? <button type="button" className={styles.removeAttachment} aria-label={`Remove ${item.name}`} disabled={disabled} onClick={() => onRemove(item.id)}><WorkspaceIcon name="close" width="12" height="12" /></button> : null}
    </div>)}</div>
    <dialog ref={dialog} aria-label={preview ? `Preview ${preview.name}` : "Attachment preview"} className={styles.attachmentDialog} onClick={event => { if (event.target === event.currentTarget) dialog.current?.close(); }}>
      {preview ? <><header><span>{preview.name}</span><button type="button" aria-label="Close preview" onClick={() => dialog.current?.close()}><WorkspaceIcon name="close" /></button></header>
        {/* eslint-disable-next-line @next/next/no-img-element */}
        {preview.mediaType.startsWith("image/") ? <img src={attachmentURL(preview)} alt={preview.name} /> : <pre>{new TextDecoder().decode(Uint8Array.from(atob(preview.dataBase64), c => c.charCodeAt(0)))}</pre>}
      </> : null}
    </dialog>
  </>;
}
