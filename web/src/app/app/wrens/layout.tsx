import {WrenNavigation} from "@/components/wren-navigation";
import {WrenCreationModals} from "@/components/wren-creation-modals";
import type { ReactNode } from "react";
import { WrensProvider } from "@/components/wrens-context";
import { requireAuthenticated, selectedWorkspace, serverCanterFetch } from "@/lib/server-auth";
import type { Product } from "@/lib/canter-api";
import { notFound } from "next/navigation";
export default async function WrensLayout({ children }: { children: ReactNode }) {
  const me = await requireAuthenticated();
  const workspace = await selectedWorkspace(me);
  const { products } = await serverCanterFetch<{ products: Product[] }>(`/workspaces/${encodeURIComponent(workspace.id)}/products`);
  if (!products?.some(product => product.id === "wrens" && product.enabled)) notFound();
  return <WrensProvider>{children}<WrenNavigation/><WrenCreationModals/></WrensProvider>;
}
