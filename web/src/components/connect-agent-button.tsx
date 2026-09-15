"use client";

import type { ReactNode } from "react";
import { useWorkspace } from "./workspace-context";

export function ConnectAgentButton({ children, className }: { children: ReactNode; className?: string }) {
  const { connectAgent, data } = useWorkspace();
  return <button type="button" className={className} disabled={!data} onClick={connectAgent}>{children}</button>;
}
