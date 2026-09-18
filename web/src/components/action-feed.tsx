import Link from "next/link";
import { relativeTime, type WorkspaceAction } from "@/lib/canter-api";
import { actionHref, actionTitle } from "@/lib/workspace-actions";
import { WorkspaceIcon } from "./workspace-icon";
import styles from "./workspace.module.css";

export function ActionFeed({ actions }: { actions: WorkspaceAction[] }) {
  return <div className={styles.actionFeed}>{actions.map(action => {
    const href = actionHref(action);
    const content = <><span className={styles.actionMarker}><WorkspaceIcon name={action.actor.kind === "agent" ? "terminal" : "activity"} width="15" height="15" /></span><div><span>{actionTitle(action)}</span><small>{action.actor.displayName || (action.actor.kind === "agent" ? "Agent" : action.actor.kind === "human" ? "User" : "Canter")}{action.metadata.system ? ` · ${action.metadata.system}` : ""}</small></div>{action.metadata.outcome === "failed" ? <span className={styles.status} data-phase="failed">Failed</span> : null}<time dateTime={action.occurredAt} title={new Date(action.occurredAt).toLocaleString()}>{relativeTime(action.occurredAt)}</time></>;
    return href ? <Link key={action.id} className={styles.actionRow} href={href}>{content}</Link> : <div key={action.id} className={styles.actionRow}>{content}</div>;
  })}</div>;
}
