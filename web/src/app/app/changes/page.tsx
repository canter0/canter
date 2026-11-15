"use client";

import { Suspense } from "react";
import { AppShell } from "@/components/app-shell";
import { ActivityView } from "@/components/activity-view";
import { useWorkspace } from "@/components/workspace-context";
import { ContentTransition } from "@/components/site-motion";
import styles from "@/components/workspace.module.css";

function ActivityScreen() {
  const { data, error, loading, retry } = useWorkspace();
  return <ContentTransition name="canter-activity-content">
    {loading ? <p role="status" className={styles.loading}>Loading activity…</p> : error && !data ? <div role="alert" className={styles.error}>Couldn’t load activity. <button onClick={retry}>Try again</button></div> : data ? <ActivityView data={data} /> : null}
  </ContentTransition>;
}

export default function ActivityPage() {
  return <AppShell active="Changes"><section className={styles.contentPage}>
    <div className={styles.pageHeading} data-motion="reveal"><div><h1>Activity</h1><p>Changes and deployments your agents prepare, and what happened to them.</p></div></div>
    <Suspense><ActivityScreen /></Suspense>
  </section></AppShell>;
}
