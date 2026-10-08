"use client";

import { useEffect, useState } from "react";
import { createReleaseChecker, releaseCheckInterval } from "@/lib/release-update";

const loadedCommit = process.env.NEXT_PUBLIC_CANTER_RELEASE ?? "";
const dismissalKey = "canter:dismissed-release-update";

export function useReleaseUpdate() {
  const [commit, setCommit] = useState<string | null>(null);
  const [dismissed, setDismissed] = useState(() => {
    try { return sessionStorage.getItem(dismissalKey) ?? ""; } catch { return ""; }
  });
  useEffect(() => {
    const checker = createReleaseChecker(loadedCommit, {
      read: async signal => {
        const response = await fetch("/release.json", { cache: "no-store", credentials: "omit", signal });
        return response.ok ? response.json() : null;
      },
      onChange: setCommit,
    });
    const check = () => { if (document.visibilityState === "visible" && navigator.onLine) void checker.check(); };
    check();
    const timer = setInterval(check, releaseCheckInterval);
    window.addEventListener("focus", check);
    window.addEventListener("online", check);
    document.addEventListener("visibilitychange", check);
    return () => {
      clearInterval(timer);
      window.removeEventListener("focus", check);
      window.removeEventListener("online", check);
      document.removeEventListener("visibilitychange", check);
      checker.dispose();
    };
  }, []);
  return {
    available: commit && commit !== dismissed ? commit : null,
    dismiss() {
      if (!commit) return;
      setDismissed(commit);
      try { sessionStorage.setItem(dismissalKey, commit); } catch { /* Keep dismissal in memory. */ }
    },
    refresh() { window.location.reload(); },
  };
}
