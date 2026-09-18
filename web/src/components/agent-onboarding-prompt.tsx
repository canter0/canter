"use client";

import Link from "next/link";
import { useEffect, useRef, useState } from "react";
import styles from "@/app/home.module.css";

const onboardingPrompt = "Read https://canter.dev/llms.txt and help me connect you to Canter. Show me the authorization link and wait for my approval.";

export function AgentOnboardingPrompt() {
  const [status, setStatus] = useState<"idle" | "copied" | "manual">("idle");
  const promptRef = useRef<HTMLTextAreaElement>(null);
  const resetTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => () => {
    if (resetTimer.current) clearTimeout(resetTimer.current);
  }, []);

  async function copyPrompt() {
    if (resetTimer.current) clearTimeout(resetTimer.current);
    try {
      await navigator.clipboard.writeText(onboardingPrompt);
      setStatus("copied");
      resetTimer.current = setTimeout(() => setStatus("idle"), 3500);
    } catch {
      promptRef.current?.focus();
      promptRef.current?.select();
      setStatus("manual");
    }
  }

  return (
    <div className={styles.onboarding}>
      <label htmlFor="agent-onboarding-prompt" className={styles.promptLabel}>Or connect your own agent</label>
      <div className={styles.promptField}>
        <textarea id="agent-onboarding-prompt" ref={promptRef} className={styles.prompt} readOnly rows={2} value={onboardingPrompt} spellCheck={false} aria-describedby="agent-prompt-hint" />
      </div>
      <button type="button" className={styles.copyButton} onClick={copyPrompt}>
        {status === "copied" ? "Prompt copied" : "Copy prompt to onboard your agent"}
        <svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
          {status === "copied" ? <path d="m5 12 4 4L19 6" /> : <><rect x="8" y="8" width="12" height="13" rx="2" /><path d="M16 8V5a2 2 0 0 0-2-2H5a2 2 0 0 0-2 2v9a2 2 0 0 0 2 2h3" /></>}
        </svg>
      </button>
      <p id="agent-prompt-hint" className={styles.promptHint} role="status" aria-live="polite">
        {status === "manual" ? "Prompt selected. Press ⌘C or Ctrl+C, then paste it into your agent." : status === "copied" ? "Paste it into your agent to get connected." : "Paste into Claude Code, Codex, Cursor, or your agent of choice."}
      </p>
      <Link href="/onboarding/agent" className={styles.connectionLink}>Already have a connection code? <span aria-hidden="true">↗</span></Link>
    </div>
  );
}
