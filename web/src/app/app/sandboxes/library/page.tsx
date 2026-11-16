"use client";

import Link from "next/link";
import { AppShell } from "@/components/app-shell";
import { SandboxSavedEnvironments } from "@/components/sandbox-saved-environments";
import { useWorkspace } from "@/components/workspace-context";
import styles from "@/components/workspace.module.css";

export default function SandboxLibraryPage() {
	const { data, error, loading } = useWorkspace();
	if (!data) return <AppShell active="Sandboxes" pageTitle="Saved environments"><section className={styles.contentPage}>
		<p role={error ? "alert" : "status"}>{loading ? "Loading saved environments…" : "Couldn’t load this organization."}</p>
	</section></AppShell>;
	return <AppShell active="Sandboxes" pageTitle="Saved environments" pageKey={data.workspace.id}><section className={styles.contentPage}>
		<div className={styles.pageHeading} data-motion="reveal"><div><h1>Saved environments</h1></div>
			<Link href="/app" className={styles.primaryButton}>Ask Canter</Link></div>
		<SandboxSavedEnvironments key={data.workspace.id} workspaceId={data.workspace.id} writer={false} />
	</section></AppShell>;
}
