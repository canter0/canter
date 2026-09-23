import type { SVGProps } from "react";

const paths = {
  more: "M5 12h.01M12 12h.01M19 12h.01",
  edit: "m15 4 5 5M4 20l5-1L21 7l-5-5L4 14z",
  trash: "M3 6h18M9 6V3h6v3M5 6l1 15h12l1-15M10 10v7M14 10v7",
  lock: "M5 10h14v11H5zM8 10V6a4 4 0 0 1 8 0v4M12 14v3",
  changes: "M6 3v12a3 3 0 1 0 3 3H6M18 21V9a3 3 0 1 0-3-3h3M6 7h12",
  expand: "M8 3H3v5M16 3h5v5M3 16v5h5M21 16v5h-5",
  collapse: "M3 8h5V3M21 8h-5V3M8 21v-5H3M16 21v-5h5",
  plus: "M12 5v14M5 12h14",
  apps: "M3 3h7v7H3zM14 3h7v7h-7zM3 14h7v7H3zM14 14h7v7h-7z",
  activity: "M3 12h4l3-8 4 16 3-8h4",
  agent: "M8 4h8M12 4V2M5 8h14v12H5zM2 12v4M22 12v4M9 12h.01M15 12h.01M9 16h6",
  settings: "M4 7h16M4 17h16M8 4v6M16 14v6",
  panel: "M3 4h18v16H3zM9 4v16",
  arrow: "M12 19V5M5 12l7-7 7 7",
  right: "M5 12h14M13 6l6 6-6 6",
  external: "M7 17 17 7M7 7h10v10",
  copy: "M8 8h12v12H8zM16 8V4H4v12h4",
  check: "m5 12 4 4L19 6",
  close: "m6 6 12 12M6 18 18 6",
  terminal: "M3 4h18v16H3zM7 9l3 3-3 3M13 15h4",
  search: "M21 21l-5-5M18 10a8 8 0 1 1-16 0 8 8 0 0 1 16 0",
  chevron: "m9 5 7 7-7 7",
  down: "m6 9 6 6 6-6",
  attachment: "m8 12 6-6a3 3 0 0 1 4 4l-8 8a5 5 0 0 1-7-7l8-8M6 14l7-7",
  folder: "M3 5h6l2 3h10v12H3z",
  file: "M5 3h9l5 5v13H5zM14 3v6h5M8 13h8M8 17h6",
  message: "M3 4h18v13H8l-5 4z",
} as const;

export type WorkspaceIconName = keyof typeof paths;

export function WorkspaceIcon({ name, ...props }: SVGProps<SVGSVGElement> & { name: WorkspaceIconName }) {
  return <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" {...props}><path d={paths[name]} /></svg>;
}
