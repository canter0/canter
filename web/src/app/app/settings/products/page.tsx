"use client";

import Link from "next/link";
import { useId, useRef, useState } from "react";
import { SettingsShell, SettingRow } from "@/components/settings-shell";
import { useRequestScope } from "@/components/use-request-scope";
import { useWorkspace } from "@/components/workspace-context";
import { CanterAPIError, canterFetch, type Product } from "@/lib/canter-api";
import styles from "./products.module.css";

export default function ProductSettingsPage() {
  const { data, resources, retry } = useWorkspace();
  const { loaded, error } = resources.products;

  return <SettingsShell active="Products" title="Products" description="Manage products for your organization.">
    {error ? <div className={styles.notice} role="alert">Couldn’t refresh products. <button type="button" onClick={retry}>Try again</button></div> : null}
    {!loaded && !error ? <p className={styles.muted} role="status">Loading products…</p> : null}
    {loaded ? <ProductSettings
      // Discard confirmations, errors, and requests across identity or role changes.
      key={`${data.account.id}:${data.workspace.id}:${data.workspace.role}`}
      workspaceId={data.workspace.id}
      products={data.products ?? []}
      owner={data.workspace.role === "owner"}
      unavailable={Boolean(error)}
      onChanged={retry}
    /> : null}
  </SettingsShell>;
}

function ProductSettings({ workspaceId, products, owner, unavailable, onChanged }: {
  workspaceId: string;
  products: Product[];
  owner: boolean;
  unavailable: boolean;
  onChanged: () => void;
}) {
  const { signal: requestSignal } = useRequestScope();
  const mutationLock = useRef(false);
  const [pending, setPending] = useState("");
  const [confirming, setConfirming] = useState("");
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const helpId = useId();
  // Reflect a confirmed mutation immediately, then accept fresh server snapshots.
  const [snapshot, setSnapshot] = useState({ source: products, products });
  if (snapshot.source !== products) setSnapshot({ source: products, products });
  const currentProducts = snapshot.source === products ? snapshot.products : products;

  async function setProduct(id: string, enabled: boolean) {
    const product = currentProducts.find(item => item.id === id);
    const signal = requestSignal();
    if (!owner || unavailable || !product || product.alwaysOn || mutationLock.current || signal.aborted) return;
    if (!enabled && confirming !== id) return;
    mutationLock.current = true;
    setPending(id);
    setError("");
    setNotice("");
    try {
      await canterFetch(`/workspaces/${encodeURIComponent(workspaceId)}/products/${encodeURIComponent(id)}`, {
        method: enabled ? "PUT" : "DELETE",
        signal,
      });
      if (signal.aborted) return;
      setSnapshot(current => ({ ...current, products: current.products.map(item => item.id === id ? { ...item, enabled } : item) }));
      setConfirming("");
      setNotice(`${product.name} is now ${enabled ? "on" : "off"}.`);
      onChanged();
    } catch (cause) {
      if (signal.aborted) return;
      const message = cause instanceof Error ? cause.message : "The change didn’t apply. Try again.";
      const conflict = cause instanceof CanterAPIError && cause.status === 409;
      setError(conflict && !enabled
        ? `Couldn’t turn off ${product.name}. ${message}`
        : message);
      // Permission or resource state may have changed since the last read.
      onChanged();
    } finally {
      if (!signal.aborted) {
        mutationLock.current = false;
        setPending("");
      }
    }
  }

  return <section className={styles.section} aria-label="Organization products">
    <div className={styles.explanation} id={helpId}>
      <p>Products activate when you first use them. Owners can also turn them on here.</p>
      <p>Turning a product off removes it from navigation. It doesn’t block authorized future use, which can activate it again.</p>
      {!owner ? <p>Only organization owners can change these settings.</p> : null}
    </div>
    <div className={styles.card}>
      {currentProducts.map(product => <div key={product.id} className={styles.entry}>
        <SettingRow title={product.name} description={product.description}>
          {product.alwaysOn ? <span className={styles.readOnly}>Always on</span> : owner ? <button
            type="button"
            role="switch"
            aria-label={product.name}
            aria-checked={product.enabled}
            aria-describedby={helpId}
            className={styles.toggle}
            disabled={Boolean(pending) || unavailable}
            onClick={() => {
              if (mutationLock.current) return;
              if (product.enabled) {
                setConfirming(product.id);
                setError("");
                setNotice("");
              } else {
                setConfirming("");
                void setProduct(product.id, true);
              }
            }}
          ><span>{pending === product.id ? "Saving…" : product.enabled ? "On" : "Off"}</span><span className={styles.track} aria-hidden="true"><span /></span></button> : <span className={styles.readOnly}>{product.enabled ? "On" : "Off"}</span>}
        </SettingRow>
        {owner && confirming === product.id && product.enabled && !product.alwaysOn ? <div className={styles.confirmation} role="group" aria-label={`Turn off ${product.name}`}>
          <p>Turn off {product.name}? Products with active resources may need attention first. Authorized future use can turn it back on.</p>
          <div className={styles.actions}>
            <button type="button" className={styles.button} disabled={Boolean(pending)} onClick={() => setConfirming("")}>Cancel</button>
            <button type="button" className={styles.button} disabled={Boolean(pending) || unavailable} onClick={() => void setProduct(product.id, false)}>{pending === product.id ? "Turning off…" : `Turn off ${product.name}`}</button>
          </div>
        </div> : null}
      </div>)}
      {!currentProducts.length ? <p className={styles.empty}>No products are available yet.</p> : null}
    </div>
    {pending ? <p className={styles.muted} role="status">Saving product settings…</p> : null}
    {error ? <p className={styles.notice} role="alert">{error}</p> : null}
    {notice ? <p className={styles.muted} role="status">{notice}</p> : null}
    <Link href="/app/products" className={styles.link}>Explore products</Link>
  </section>;
}
