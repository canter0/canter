"use client";
import {useRef,type ReactNode} from "react";
import {useDialog} from "./use-dialog";
import {WorkspaceIcon} from "./workspace-icon";
import styles from "./wren-dialog.module.css";
export function WrenDialog({title,onClose,children,showTitle=false}:{title:string;onClose:()=>void;children:ReactNode;showTitle?:boolean}){
 const dialog=useRef<HTMLDialogElement>(null),dismiss=useDialog(dialog,'input[name="name"]');
 function close(){dismiss(onClose);}
 return <dialog ref={dialog} className={`dashboard-theme ${styles.dialog}`} aria-label={title} onCancel={event=>{event.preventDefault();close();}} onClick={event=>{if(event.target!==event.currentTarget)return;const bounds=event.currentTarget.getBoundingClientRect();if(event.clientX<bounds.left||event.clientX>bounds.right||event.clientY<bounds.top||event.clientY>bounds.bottom)close();}}><button type="button" className={styles.close} aria-label={`Close ${title}`} onClick={close}><WorkspaceIcon name="close" width="17" height="17"/></button>{showTitle?<h2 className={styles.title}>{title}</h2>:null}{children}</dialog>;
}
