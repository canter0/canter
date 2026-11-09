import type { Product } from "./canter-api";
import type { WorkspaceIconName } from "../components/workspace-icon";

export type QuicknavItem = { href: string; label: string; icon?: WorkspaceIconName; wren?: boolean };

const productNav: Record<string, { href: string; icon: WorkspaceIconName }> = {
  wrens: { href: "/app/wrens", icon: "agent" },
  sandboxes: { href: "/app/sandboxes", icon: "terminal" },
};

// Quicknav shows Home, then one item per enabled product in catalog order,
// then Products. Wrens uses the wren mark instead of a workspace icon.
export function quicknavItems(products: Product[] | undefined): QuicknavItem[] {
  const items: QuicknavItem[] = [{ href: "/app", label: "Home", icon: "home" }];
  for (const product of products ?? []) {
    const nav = productNav[product.id];
    if (!product.enabled || !nav) continue;
    items.push(product.id === "wrens" ? { href: nav.href, label: product.name, wren: true } : { href: nav.href, label: product.name, icon: nav.icon });
  }
  items.push({ href: "/app/products", label: "Products", icon: "apps" });
  return items;
}
