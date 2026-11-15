"use client";

import Link from "next/link";
import { useState } from "react";
import { AppShell } from "@/components/app-shell";
import { useWorkspace } from "@/components/workspace-context";
import { WorkspaceIcon } from "@/components/workspace-icon";
import { canterFetch, type Product } from "@/lib/canter-api";
import styles from "@/components/workspace.module.css";

const productHrefs: Record<string, string> = { wrens: "/app/wrens", sandboxes: "/app/sandboxes" };

export default function ProductsPage() {
  const { data, error, loading, retry } = useWorkspace();
  const owner = data?.workspace.role === "owner";
  const writer = owner || data?.workspace.role === "operator";
  const [pending, setPending] = useState("");
  const [actionError, setActionError] = useState("");
  async function setProduct(product: Product, enabled: boolean) {
    setPending(product.id + enabled);
    setActionError("");
    try {
      await canterFetch(`/workspaces/${encodeURIComponent(data!.workspace.id)}/products/${encodeURIComponent(product.id)}`, { method: enabled ? "PUT" : "DELETE" });
      retry();
    } catch (cause) {
      setActionError(cause instanceof Error ? cause.message : "That product change didn't apply.");
    } finally {
      setPending("");
    }
  }
  return <AppShell active="Products" pageTitle="Products"><section className={styles.contentPage}>
    <div className={styles.pageHeading} data-motion="reveal"><div><h1>Products</h1><p>Turn organization capabilities on and off.</p></div></div>
    {loading ? <p role="status" className={styles.loading}>Loading products…</p> : error && !data ? <div role="alert" className={styles.error}>Couldn’t load products. <button onClick={retry}>Try again</button></div> : <div className={styles.cardGrid}>{(data?.products ?? []).map(product => <div key={product.id} className={styles.resourceCard} data-motion="fade" data-motion-card>
      <div className={styles.resourceCardHeader}><div><WorkspaceIcon name={product.id === "wrens" ? "agent" : "terminal"} /><h2>{product.name}</h2></div>{product.alwaysOn ? <span className={styles.status}>Included</span> : product.enabled ? <Link className={styles.secondaryButton} href={productHrefs[product.id] ?? "/app/products"}>Open</Link> : writer ? <button className={styles.primaryButton} disabled={pending !== ""} onClick={() => void setProduct(product, true)}>Enable</button> : null}</div>
      <p>{product.description}</p>
      {owner && product.enabled && !product.alwaysOn ? <div className={styles.resourceCardFooter}><button className={styles.secondaryButton} disabled={pending !== ""} onClick={() => void setProduct(product, false)}>Turn off</button></div> : null}
    </div>)}</div>}
    {actionError ? <div role="alert" className={styles.error}>{actionError}</div> : null}
  </section></AppShell>;
}
