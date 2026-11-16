"use client";

import { useRef } from "react";
import { useShortcut, useShortcutHint } from "@ui-sdk/react/shortcuts";
import { Input, InputClear, InputGroup } from "@ui-sdk/react/input";
import { Select, SelectContent, SelectEmpty, SelectInput, SelectItem, SelectItemIndicator, SelectItemText, SelectList, SelectTrigger, SelectValue } from "@ui-sdk/react/select";
import { WorkspaceIcon } from "./workspace-icon";
import styles from "./sandbox-controls.module.css";

export function SandboxSearch({ label, placeholder, value, onChange, shortcut }: {
  label: string; placeholder: string; value: string; onChange: (value: string) => void; shortcut?: string;
}) {
  const input = useRef<HTMLInputElement>(null);
  const hint = useShortcutHint(shortcut ?? "/");
  useShortcut(shortcut, event => { event.preventDefault(); input.current?.focus(); input.current?.select(); });
  return <InputGroup className={styles.search}>
    <WorkspaceIcon name="search" width="16" height="16" />
    <Input ref={input} aria-label={label} aria-keyshortcuts={shortcut ? hint.ariaKeyshortcuts : undefined} placeholder={placeholder} value={value} onValueChange={onChange} />
    {shortcut && !value ? <kbd className={styles.kbd} aria-hidden="true">{hint.text}</kbd> : null}
    <InputClear aria-label={`Clear ${label.toLowerCase()}`}><WorkspaceIcon name="close" width="14" height="14" /></InputClear>
  </InputGroup>;
}

export function SandboxSelect({ label, value, onChange, options, disabled, searchLabel, className = "" }: {
  label: string; value: string; onChange: (value: string) => void;
  options: { value: string; label: string }[]; disabled?: boolean; searchLabel?: string; className?: string;
}) {
  return <Select value={value} onValueChange={next => { if (next !== null) onChange(next); }} items={options} disabled={disabled}>
    <SelectTrigger aria-label={label} className={`${styles.select} ${className}`}>
      <SelectValue placeholder="Choose…" /><WorkspaceIcon name="down" width="14" height="14" />
    </SelectTrigger>
    <SelectContent className={`dashboard-theme ${styles.popup}`} align="end" sideOffset={6} alignItemWithTrigger={false}>
      {searchLabel ? <SelectInput aria-label={searchLabel} placeholder={searchLabel} className={styles.query} /> : null}
      <SelectList className={styles.options}>
        {options.map(option => <SelectItem key={option.value} value={option.value} label={option.label} className={styles.option}>
          <SelectItemText />
          <SelectItemIndicator><WorkspaceIcon name="check" width="14" height="14" /></SelectItemIndicator>
        </SelectItem>)}
      </SelectList>
      <SelectEmpty className={styles.empty}>No matches</SelectEmpty>
    </SelectContent>
  </Select>;
}
