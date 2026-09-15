"use client";
import { createContext, useContext, type ReactNode } from "react";
const SurfaceWorkspace = createContext<string | undefined>(undefined);
export const useSurfaceWorkspace = () => useContext(SurfaceWorkspace);
export function EmbeddedAppSurface({ workspaceId, children }: { workspaceId: string; children: ReactNode }) {
  return <SurfaceWorkspace.Provider value={workspaceId}>{children}</SurfaceWorkspace.Provider>;
}
