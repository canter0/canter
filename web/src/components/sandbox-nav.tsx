"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { Toolbar, ToolbarLink } from "@ui-sdk/react/toolbar";
import { Tooltip, TooltipContent, TooltipTrigger } from "@ui-sdk/react/tooltip";
import { WorkspaceIcon, type WorkspaceIconName } from "./workspace-icon";
import styles from "./sandbox-shell.module.css";

// Section navigation for the Sandboxes product: a vertical icon rail beside the
// global quicknav, like the side panels every other product surface has. UI SDK
// toolbar gives it one tab stop and arrow-key navigation between sections.
const sections: { href: string; label: string; icon: WorkspaceIconName }[] = [
  { href: "/app/sandboxes", label: "Computers", icon: "desktop" },
  { href: "/app/sandboxes/super", label: "Super Sandboxes", icon: "stack" },
  { href: "/app/sandboxes/library", label: "Saved environments", icon: "bookmark" },
];

function isRoute(pathname: string, href: string) {
  return pathname === href || pathname.startsWith(`${href}/`);
}

function isCurrent(pathname: string, href: string) {
  if (href === "/app/sandboxes") {
    return isRoute(pathname, href) && !isRoute(pathname, "/app/sandboxes/super") && !isRoute(pathname, "/app/sandboxes/library");
  }
  return isRoute(pathname, href);
}

export function SandboxNav() {
  const pathname = usePathname();
  return (
    <Toolbar orientation="vertical" aria-label="Sandboxes sections" className={styles.subnav}>
      {sections.map(section => (
        <Tooltip key={section.href}>
          <TooltipTrigger
            render={
              <ToolbarLink
                render={<Link href={section.href} prefetch={true} />}
                aria-label={section.label}
                aria-current={isCurrent(pathname, section.href) ? "page" : undefined}
                className={styles.subnavItem}
              />
            }
          >
            <WorkspaceIcon name={section.icon} width="19" height="19" />
          </TooltipTrigger>
          <TooltipContent side="right" sideOffset={10} className={`dashboard-theme ${styles.tip}`}>
            {section.label}
          </TooltipContent>
        </Tooltip>
      ))}
    </Toolbar>
  );
}
