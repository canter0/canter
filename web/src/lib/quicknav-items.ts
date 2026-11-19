import type { Product } from "./canter-api";
import type { WorkspaceIconName } from "../components/workspace-icon";

export type QuicknavItem = { href: string; label: string; icon?: WorkspaceIconName; wren?: boolean; description?: string; preview?: "recent" | "product" };

const productNav: Record<string, { href: string; icon: WorkspaceIconName }> = {
  wrens: { href: "/app/wrens", icon: "agent" },
  sandboxes: { href: "/app/sandboxes", icon: "terminal" },
  ci: { href: "/app/ci", icon: "changes" },
};

// Quicknav shows Home, then one item per enabled product in catalog order,
// then Products. Wrens uses the wren mark instead of a workspace icon.
export function quicknavItems(products: Product[] | undefined): QuicknavItem[] {
  const items: QuicknavItem[] = [{ href: "/app", label: "Home", icon: "home", preview: "recent" }];
  for (const product of products ?? []) {
    const nav = productNav[product.id];
    if (!product.enabled || !nav) continue;
    items.push({ href: nav.href, label: product.name, description: product.description, preview: "product", ...(product.id === "wrens" ? { wren: true } : { icon: nav.icon }) });
  }
  items.push({ href: "/app/products", label: "Products", icon: "apps" });
  return items;
}

export const quicknavFooterItems: QuicknavItem[] = [
  { href: "/app/settings", label: "Settings", icon: "settings" },
  { href: "/app/customize", label: "Customize", icon: "customize" },
];
