"use client";
import { displayName } from "@/lib/display-name";
import { WorkspaceIcon } from "./workspace-icon";
import type { ReactNode } from "react";
import type { OperatorMessage } from "@/lib/operator-api";
import styles from "./operator-workspace.module.css";
export function WrenMessageFrame({message,onReply,children,replySenderName,own}:{own?:boolean;message:OperatorMessage;replySenderName?:string;onReply?:(message:OperatorMessage)=>void;children:ReactNode}) {
 return <div className={styles.replyMessage} tabIndex={-1} data-message-id={message.id} data-message-role={message.role} data-own={own}>
  {message.replyTo?<blockquote className={styles.replyQuote}><strong>{displayName(message.replyTo.senderName)}</strong><span>{message.replyTo.excerpt}</span></blockquote>:null}
  {children}{onReply?<button type="button" className={styles.replyAction} title="Reply" onClick={()=>onReply(message)} aria-label={`Reply to ${(message.role==="assistant"?(message.speaker?.name||replySenderName):undefined) || displayName(message.senderName)}`}><WorkspaceIcon name="reply" width="16" height="16"/></button>:null}
 </div>;
}
