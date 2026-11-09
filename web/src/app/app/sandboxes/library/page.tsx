"use client";

import Link from "next/link";
import { AppShell } from "@/components/app-shell";
import { SandboxSavedEnvironments } from "@/components/sandbox-saved-environments";
import { useWorkspace } from "@/components/workspace-context";
import { WorkspaceIcon } from "@/components/workspace-icon";
import styles from "@/components/workspace.module.css";

export default function SandboxLibraryPage() {
	const { data, error, loading } = useWorkspace();
	if (!data) return <AppShell active="Sandboxes" pageTitle="Saved environments"><section className={styles.contentPage}>
		<p role={error ? "alert" : "status"}>{loading ? "Loading saved environments…" : "Couldn’t load this organization."}</p>
	</section></AppShell>;
	const writer = data.workspace.role === "owner" || data.workspace.role === "operator";
	return <AppShell active="Sandboxes" pageTitle="Saved environments" pageKey={data.workspace.id}><section className={styles.contentPage}>
		<nav className={styles.breadcrumb}><Link href="/app/sandboxes" className={styles.secondaryButton}><WorkspaceIcon name="arrow" width="14" height="14" />Sandboxes</Link></nav>
		<div className={styles.pageHeading} data-motion="reveal"><div><h1>Saved environments</h1><p>Snapshots and templates shared by this organization’s sandboxes.</p></div></div>
		<SandboxSavedEnvironments key={data.workspace.id} workspaceId={data.workspace.id} writer={writer} />
	</section></AppShell>;
}
