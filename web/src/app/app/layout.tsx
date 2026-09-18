import type { ReactNode } from "react";
import { requireAuthenticated } from "@/lib/server-auth";
import { WorkspaceProvider } from "@/components/workspace-context";

export default async function AuthenticatedLayout({ children }: { children: ReactNode }) {
  await requireAuthenticated();
  return <WorkspaceProvider>{children}</WorkspaceProvider>;
}
