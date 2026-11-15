"use client";

import { useEffect, useRef, type ReactNode } from "react";
import { WorkspaceIcon } from "./workspace-icon";
import styles from "./wren-context-dialog.module.css";

/** Keep drafts mounted while native dialog focus and modality follow visibility. */
export function WrenContextDialog({open,title,onClose,children,wide=false}:{open:boolean;title:string;onClose:()=>void;children:ReactNode;wide?:boolean}) {
  const root=useRef<HTMLDialogElement>(null);
  useEffect(()=>{
    const dialog=root.current;
    if(!open||!dialog)return;
    const origin=document.activeElement instanceof HTMLElement?document.activeElement:null;
    dialog.showModal();
    return()=>{dialog.close();requestAnimationFrame(()=>{if(origin?.isConnected&&!origin.closest('[hidden],[inert]'))origin.focus({preventScroll:true});});};
  },[open]);
  return <dialog ref={root} className={`dashboard-theme ${styles.dialog}`} data-wide={wide||undefined} aria-label={title} onCancel={event=>{event.preventDefault();onClose();}} onClick={event=>{if(event.target!==event.currentTarget)return;const r=event.currentTarget.getBoundingClientRect();if(event.clientX<r.left||event.clientX>r.right||event.clientY<r.top||event.clientY>r.bottom)onClose();}}>
    <header className={styles.header}><span>{title}</span><button type="button" aria-label={title==="Apps"?"Close apps":"Close configuration"} onClick={onClose}><WorkspaceIcon name="close" width="18" height="18"/></button></header>
    {children}
  </dialog>;
}
