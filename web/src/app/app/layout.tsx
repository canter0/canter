import type { ReactNode } from "react";
import type { Metadata } from "next";
import { workspaceBootstrap } from "@/lib/server-auth";
import { WorkspaceProvider } from "@/components/workspace-context";

// The persistent workspace provider owns the title, including live conversation names.
export const metadata: Metadata = { title: null };

export default async function AuthenticatedLayout({ children }: { children: ReactNode }) {
  const initial = await workspaceBootstrap();
  return <WorkspaceProvider key={`${initial.data.account.id}:${initial.data.workspace.id}`} initial={initial}>{children}</WorkspaceProvider>;
}
