"use client";

import { useState } from "react";

export function CopyInstruction({ text }: { text: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <button
      className="rule-link justify-self-end"
      onClick={async () => {
        await navigator.clipboard.writeText(text);
        setCopied(true);
        window.setTimeout(() => setCopied(false), 1400);
      }}
    >
      {copied ? "Copied" : "Copy ↗"}
    </button>
  );
}
