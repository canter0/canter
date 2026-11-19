"use client";

import Link from "next/link";
import { AppShell } from "@/components/app-shell";
import { useWorkspace } from "@/components/workspace-context";
import { WorkspaceIcon, type WorkspaceIconName } from "@/components/workspace-icon";
import styles from "./products.module.css";

const destinations: Record<string, { href: string; icon: WorkspaceIconName; description: string }> = {
  wrens: { href: "/app/wrens", icon: "agent", description: "Persistent agents that work alongside your organization." },
  sandboxes: { href: "/app/sandboxes", icon: "terminal", description: "Cloud computers for you and your agents." },
  ci: { href: "/app/ci", icon: "changes", description: "GitHub Actions runners and build results." },
};

export default function ProductsPage() {
  const { data, resources, retry } = useWorkspace();
  const owner = data?.workspace.role === "owner";
  const products = (data?.products ?? []).filter(product => Object.hasOwn(destinations, product.id));
  const { loaded, error } = resources.products;

  return <AppShell active="Products" pageTitle="Products">
    <section className={styles.page}>
      <header className={styles.heading} data-motion="reveal">
        <div><h1>Products</h1><p>Use products in conversations. Explore their work here.</p></div>
        {owner ? <Link className={styles.manage} href="/app/settings/products">Manage products<WorkspaceIcon name="right" width="14" height="14" /></Link> : null}
      </header>
      {error ? <div className={styles.notice} role="alert">Couldn’t refresh products. <button type="button" onClick={retry}>Try again</button></div> : null}
      {!loaded && !error ? <p className={styles.muted} role="status">Loading products…</p> : null}
      {loaded ? <div className={styles.list}>
        {products.map(product => {
          const destination = destinations[product.id];
          return <Link key={product.id} href={destination.href} className={styles.card} data-motion="fade" data-motion-card>
            <span className={styles.icon}><WorkspaceIcon name={destination.icon} width="21" height="21" /></span>
            <div className={styles.copy}><h2>{product.name}</h2><p>{destination.description}</p></div>
            <WorkspaceIcon className={styles.arrow} name="right" width="18" height="18" />
          </Link>;
        })}
        {!products.length ? <p className={styles.muted}>No products are available yet.</p> : null}
      </div> : null}
    </section>
  </AppShell>;
}
