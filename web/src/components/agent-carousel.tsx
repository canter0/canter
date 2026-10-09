"use client";

import Image from "next/image";
import { useEffect, useRef } from "react";
import styles from "./agent-carousel.module.css";

// Brand marks from Lobe Icons; the MIT license is bundled with the assets.
const agents = [
  { name: "Claude Code", logo: "claudecode" },
  { name: "Codex", logo: "codex" },
  { name: "Cursor", logo: "cursor" },
  { name: "OpenCode", logo: "opencode" },
  { name: "Antigravity", logo: "antigravity" },
  { name: "Devin", logo: "devin" },
  { name: "Pi", logo: "pi" },
];

export function AgentCarousel() {
  const carousel = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const element = carousel.current;
    if (!element) return;
    let visible = false;
    const update = () => {
      element.dataset.active = String(visible && document.visibilityState === "visible");
    };
    const observer = new IntersectionObserver(entries => {
      visible = entries[0].isIntersecting;
      update();
    });
    element.dataset.enhanced = "true";
    observer.observe(element);
    document.addEventListener("visibilitychange", update);
    return () => {
      observer.disconnect();
      document.removeEventListener("visibilitychange", update);
      delete element.dataset.enhanced;
      delete element.dataset.active;
    };
  }, []);

  return (
    <div ref={carousel} className={styles.carousel} role="group" aria-label="Agent carousel" tabIndex={0} data-motion="fade">
      <div className={styles.viewport}>
        <div className={styles.track}>
          {[false, true].map(duplicate => (
            <ul key={String(duplicate)} className={styles.group} aria-label={duplicate ? undefined : "Compatible coding agents"} aria-hidden={duplicate || undefined} inert={duplicate || undefined}>
              {agents.map(agent => (
                <li key={agent.logo} className={styles.agent}>
                  <Image src={`/agent-logos/${agent.logo}.svg`} width={38} height={38} alt="" loading="eager" unoptimized />
                  <span>{agent.name}</span>
                </li>
              ))}
            </ul>
          ))}
        </div>
      </div>
    </div>
  );
}
