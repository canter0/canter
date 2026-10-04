"use client";

import Link from "next/link";
import { useEffect } from "react";
import { useWorkspace } from "@/components/workspace-context";
import styles from "@/components/workspace.module.css";

export default function WorkspaceError({ retry }: { error: Error & { digest?: string }; retry: () => void }) {
  const { setPageTitle } = useWorkspace();
  useEffect(() => setPageTitle("View unavailable"), [setPageTitle]);
  return <main className="dashboard-theme"><section className={styles.loadFailure} role="alert"><h1>This view couldn’t be opened</h1><p>Try loading it again, or return to your workspace. Conversation drafts saved in this browser session remain available.</p><button className={styles.primaryButton} onClick={retry}>Try again</button><Link className={styles.secondaryButton} href="/app">Back to workspace</Link></section></main>;
}
