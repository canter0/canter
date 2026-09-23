"use client";

import { createContext, useContext, useState, type ReactNode } from "react";
import { AgentConnectionDialog } from "./agent-connection-dialog";
import { useWorkspaceOverview } from "@/lib/workspace-overview";

const WorkspaceContext = createContext<(ReturnType<typeof useWorkspaceOverview> & { collapsed: boolean; setCollapsed: (value: boolean) => void; connectAgent: () => void }) | null>(null);

export function WorkspaceProvider({ children }: { children: ReactNode }) {
  const overview = useWorkspaceOverview();
  const [collapsed, setCollapsed] = useState(false);
  const [connectionOpen, setConnectionOpen] = useState(false);
  return <WorkspaceContext.Provider value={{ ...overview, collapsed, setCollapsed, connectAgent: () => setConnectionOpen(true) }}>{children}{connectionOpen && overview.data ? <AgentConnectionDialog workspaceId={overview.data.workspace.id} defaultAuthority={overview.data.workspace.agentAuthority} onClose={() => setConnectionOpen(false)} onConnected={overview.retry} /> : null}</WorkspaceContext.Provider>;
}

export function useWorkspace() {
  const value = useContext(WorkspaceContext);
  if (!value) throw new Error("WorkspaceProvider is required.");
  return value;
}
