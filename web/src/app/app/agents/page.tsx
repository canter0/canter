"use client";

import { ConnectAgentButton } from "@/components/connect-agent-button";

import Link from "next/link";
import { AppShell } from "@/components/app-shell";
import { useWorkspace } from "@/components/workspace-context";
import { WorkspaceIcon } from "@/components/workspace-icon";
import { agentIsConnected, relativeTime } from "@/lib/canter-api";
import styles from "@/components/workspace.module.css";

export default function AgentsPage() {
  const { data, error, loading, retry } = useWorkspace();
  const installations = data?.installations.filter(agentIsConnected) ?? [];

  return <AppShell active="Agents"><section className={styles.contentPage}>
    <div className={styles.pageHeading}><div><h1>Your agents</h1><p>Bring the agents you already work with.</p></div><ConnectAgentButton className={styles.primaryButton}><WorkspaceIcon name="plus" width="16" height="16" />Connect agent</ConnectAgentButton></div>
    {loading ? <p role="status" className={styles.loading}>Loading your agents…</p> : error ? <div role="alert" className={styles.error}>Couldn’t load your agents. <button onClick={retry}>Try again</button></div> : installations.length ? <div className={styles.cardGrid}>{installations.map(agent => <Link href={`/app/agents/${encodeURIComponent(agent.id)}`} key={agent.id} className={styles.resourceCard}><div className={styles.resourceCardHeader}><div><WorkspaceIcon name="agent" /><h2>{agent.name}</h2></div><WorkspaceIcon name="external" width="16" height="16" /></div><p>{agent.harness}</p><div className={styles.resourceCardFooter}><span className={styles.status}><span className={styles.connectionDot} data-connected={agentIsConnected(agent)} />{!agentIsConnected(agent) ? "Disconnected" : agent.expiresAt ? "Temporary" : "Connected"}</span><span>{agent.lastSeenAt ? `Seen ${relativeTime(agent.lastSeenAt)}` : "Not seen yet"}</span></div>{agent.workers?.length ? <div className={styles.workerList}>{agent.workers.map(worker => <span key={worker.id}><WorkspaceIcon name="agent" width="13" height="13" />{worker.workerName}<small>{worker.workerDraft ? "Can prepare changes" : "Read only"}</small></span>)}</div> : null}</Link>)}</div> : <div className={styles.emptyState}><span className={styles.emptyIcon}><WorkspaceIcon name="agent" width="24" height="24" /></span><h2>No agents connected</h2><p>Connect your coding agent to deploy apps and pick up where you left off.</p><ConnectAgentButton className={styles.secondaryButton}>Connect your first agent<WorkspaceIcon name="right" width="15" height="15" /></ConnectAgentButton></div>}
  </section></AppShell>;
}
