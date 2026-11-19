"use client";
import { createContext, useContext } from "react";
import type { SessionTabsState } from "@/lib/session-tabs";

export type SessionTabsController = {
  state: SessionTabsState | null;
  canBack: boolean;
  canForward: boolean;
  select: (id: string) => void;
  close: (id: string) => void;
  back: () => void;
  forward: () => void;
  travel: (delta: number) => void;
  openTab: () => void;
  openBackground: (href: string) => void;
};

export const SessionTabsContext = createContext<SessionTabsController | null>(null);
export function useSessionTabs() { return useContext(SessionTabsContext); }
