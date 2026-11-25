"use client";

import Link from "next/link";
import { useEffect, useRef, useState, type SubmitEvent } from "react";
import { WorkspaceIcon } from "./workspace-icon";
import styles from "./landing-faq.module.css";

const sources = {
  hosting: { href: "/#workload-vps", label: "Explore app hosting" },
  sandboxes: { href: "/#workload-sandboxes", label: "Explore sandboxes" },
  ci: { href: "/#workload-ci", label: "Explore CI runners" },
  agents: { href: "/llms.txt", label: "Read the agent setup guide" },
  approval: { href: "/llms.txt", label: "Read how agent approvals work" },
  pricing: { href: "/#compare-costs", label: "Compare infrastructure costs" },
  examples: { href: "/examples/resources", label: "See deployment examples" },
};

type Reply = { answer: string; source: keyof typeof sources | "" };
type Turn = { question: string; reply: Reply };

export function LandingFAQChat() {
  const [draft, setDraft] = useState("");
  const [turns, setTurns] = useState<Turn[]>([]);
  const [pending, setPending] = useState("");
  const [error, setError] = useState("");
  const transcript = useRef<HTMLDivElement>(null);
  const composer = useRef<HTMLTextAreaElement>(null);
  const request = useRef<AbortController | null>(null);

  useEffect(() => () => request.current?.abort(), []);

  useEffect(() => {
    const log = transcript.current;
    if (log) log.scrollTop = log.scrollHeight;
  }, [turns, pending]);

  async function ask(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    const question = draft.trim();
    if (!question || request.current) return;

    const controller = new AbortController();
    request.current = controller;
    composer.current?.focus({ preventScroll: true });
    setDraft("");
    setPending(question);
    setError("");
    try {
      const response = await fetch("/api/canter/faq", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        credentials: "omit",
        signal: AbortSignal.any([controller.signal, AbortSignal.timeout(18000)]),
        body: JSON.stringify({
          question,
          history: turns.slice(-3).map(turn => ({ question: turn.question, answer: turn.reply.answer })),
        }),
      });
      if (response.status === 429) {
        const seconds = Number(response.headers.get("Retry-After"));
        const wait = seconds > 0 ? ` in ${Math.ceil(seconds / 60)} min` : " in a few minutes";
        setError(`You’ve reached the question limit. Try again${wait}.`);
        setDraft(question);
        return;
      }
      if (!response.ok) throw new Error("FAQ request failed");
      const reply: Reply = await response.json();
      if (typeof reply.answer !== "string" || !reply.answer.trim()) throw new Error("Missing answer");
      setTurns(previous => [...previous, { question, reply }]);
    } catch {
      if (controller.signal.aborted) return;
      setDraft(question);
      setError("Canter couldn’t answer just now. Try again or look through the questions above.");
    } finally {
      request.current = null;
      if (!controller.signal.aborted) setPending("");
    }
  }

  return (
    <section className={styles.chat} aria-labelledby="faq-chat-title">
      <h3 id="faq-chat-title">Still have a question?</h3>
      <div ref={transcript} role="log" aria-label="Ask Canter conversation" className={styles.transcript}>
        {turns.map(({ question, reply }, index) => {
          const source = reply.source && Object.hasOwn(sources, reply.source) ? sources[reply.source] : null;
          return (
            <div key={index} className={styles.turn}>
              <p className={styles.question}><span className="sr-only">You: </span>{question}</p>
              <div className={styles.answer}>
                <p><span className="sr-only">Canter: </span>{reply.answer}</p>
                {source && <Link href={source.href} className={styles.source}>{source.label}<WorkspaceIcon name="external" width={14} height={14} /></Link>}
              </div>
            </div>
          );
        })}
        {pending && <div className={styles.turn}>
          <p className={styles.question}><span className="sr-only">You: </span>{pending}</p>
          <p className={styles.pending}>Canter is answering…</p>
        </div>}
      </div>
      {error && <p className={styles.error} role="alert">{error}</p>}
      <form className={styles.composer} onSubmit={ask}>
        <textarea
          ref={composer}
          aria-label="Ask Canter about the product"
          placeholder={turns.length ? "Ask a follow-up…" : "Ask anything about Canter…"}
          rows={1}
          maxLength={500}
          value={draft}
          readOnly={Boolean(pending)}
          onChange={event => setDraft(event.target.value)}
          onKeyDown={event => {
            if (event.key !== "Enter" || event.shiftKey || event.nativeEvent.isComposing) return;
            event.preventDefault();
            event.currentTarget.form?.requestSubmit();
          }}
        />
        <button type="submit" aria-label="Send question" disabled={!draft.trim() || Boolean(pending)}>
          <WorkspaceIcon name="arrow" width={22} height={22} />
        </button>
      </form>
    </section>
  );
}
