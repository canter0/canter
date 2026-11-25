"use client";

import { useEffect, useState } from "react";
import styles from "./landing-workloads.module.css";

type Chapter = { id: string; label: string };

export function LandingWorkloadIndex({ chapters }: { chapters: Chapter[] }) {
  const [active, setActive] = useState(chapters[0]?.id);

  useEffect(() => {
    const rows = chapters.flatMap(({ id }) => {
      const element = document.getElementById(id);
      return element ? [element] : [];
    });
    let frame = 0;

    function update() {
      frame = 0;
      const viewport = window.innerHeight;
      const readingLine = Math.min(160, viewport * .3);
      const positions = rows.map(row => ({ id: row.id, top: row.getBoundingClientRect().top }));
      const atEnd = window.scrollY + viewport >= document.documentElement.scrollHeight - 2;
      const target = positions.find(row => `#${row.id}` === window.location.hash);
      const current = positions.findLast(row => row.top <= readingLine) ?? positions[0];
      // Near the bottom the browser may be unable to scroll the requested row to the reading line.
      const bottom = target && target.top >= 0 && target.top < viewport ? target : positions.findLast(row => row.top < viewport);
      setActive((atEnd ? bottom ?? current : current)?.id);
    }

    function schedule() {
      if (!frame) frame = window.requestAnimationFrame(update);
    }

    update();
    window.addEventListener("scroll", schedule, { passive: true });
    window.addEventListener("resize", schedule);
    window.addEventListener("hashchange", schedule);
    return () => {
      window.cancelAnimationFrame(frame);
      window.removeEventListener("scroll", schedule);
      window.removeEventListener("resize", schedule);
      window.removeEventListener("hashchange", schedule);
    };
  }, [chapters]);

  return (
    <nav className={styles.index} aria-label="Workload sections">
      {chapters.map(({ id, label }) => (
        <a key={id} href={`#${id}`} aria-current={active === id ? "location" : undefined}>
          {label}
        </a>
      ))}
    </nav>
  );
}
