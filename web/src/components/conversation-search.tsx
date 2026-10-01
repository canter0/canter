"use client";

import { useCallback, useEffect, useId, useLayoutEffect, useMemo, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { useRouter } from "next/navigation";
import type { Conversation } from "@/lib/operator-api";
import { conversationCompletion, searchConversations } from "@/lib/conversation-search";
import { searchCommands, type SpotlightCommand } from "@/lib/spotlight";
import { shortcutLabel } from "@/lib/a-shortcuts";
import { useDialog } from "./use-dialog";
import { useWorkspace } from "./workspace-context";
import { WorkspaceIcon } from "./workspace-icon";
import styles from "./conversation-search.module.css";

export function ConversationSearch({ conversations, commands, shortcutsEnabled, onShortcutsChange, onClose, onNavigate }: { conversations: Conversation[]; commands: SpotlightCommand[]; shortcutsEnabled: boolean; onShortcutsChange: (enabled: boolean) => void; onClose: () => void; onNavigate: () => void }) {
  const router = useRouter();
  const { prefetchConversation } = useWorkspace();
  const dialog = useRef<HTMLDialogElement>(null);
  const input = useRef<HTMLInputElement>(null);
  const list = useRef<HTMLDivElement>(null);
  const positions = useRef(new Map<string, number>());
  const listId = useId();
  const helpId = useId();
  const [query, setQuery] = useState("");
  const [selection, setSelection] = useState<string | null>(null);
  const [closing, setClosing] = useState(false);
  const [composing, setComposing] = useState(false);
  const [atEnd, setAtEnd] = useState(true);
  const results = useMemo(() => {
    const history = searchConversations(conversations, query).map(conversation => ({ id: `conversation-${conversation.id}`, title: conversation.title, icon: "message" as const, conversation, command: undefined, key: undefined }));
    const actions = searchCommands(commands, query).map(command => ({ ...command, id: `command-${command.id}`, command, conversation: undefined }));
    return query.trim() ? [...actions, ...history] : [...history.slice(0, 4), ...actions];
  }, [conversations, commands, query]);
  const selectedIndex = Math.max(0, results.findIndex(item => item.id === selection));
  const selected = results[selectedIndex];
  const selectedConversationId = selected?.conversation?.id;
  useEffect(() => {
    if (selectedConversationId) prefetchConversation(selectedConversationId);
  }, [selectedConversationId, prefetchConversation]);
  const completion = selected && atEnd && !composing ? conversationCompletion(selected.title, query) : "";
  useDialog(dialog, "input");

  const close = useCallback(() => setClosing(true), []);
  useEffect(() => {
    if (!closing) return;
    const timer = window.setTimeout(onClose, window.matchMedia("(prefers-reduced-motion: reduce)").matches ? 0 : 140);
    return () => window.clearTimeout(timer);
  }, [closing, onClose]);

  useLayoutEffect(() => {
    const rows = list.current?.querySelectorAll<HTMLElement>("[data-result]");
    const next = new Map<string, number>();
    const reducedMotion = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
    rows?.forEach(row => {
      const id = row.dataset.result!;
      const top = row.offsetTop;
      const previous = positions.current.get(id);
      next.set(id, top);
      const transform = getComputedStyle(row).transform;
      const offset = transform === "none" ? 0 : new DOMMatrixReadOnly(transform).m42;
      row.getAnimations().forEach(animation => animation.cancel());
      if (!reducedMotion && previous !== undefined && previous - top + offset !== 0) {
        row.animate([{ transform: `translateY(${previous - top + offset}px)` }, { transform: "translateY(0)" }], { duration: 180, easing: "cubic-bezier(.2,.8,.2,1)" });
      } else if (!reducedMotion && previous === undefined) {
        row.animate([{ opacity: 0 }, { opacity: 1 }], { duration: 160, easing: "ease-out" });
      }
    });
    positions.current = next;
  }, [results]);

  useEffect(() => {
    list.current?.querySelector('[aria-selected="true"]')?.scrollIntoView({ block: "nearest" });
  }, [selected?.id]);

  function openResult(result: (typeof results)[number]) {
    if (closing) return;
    onNavigate();
    onClose();
    // Let dialog focus restoration finish before a command moves focus elsewhere.
    requestAnimationFrame(() => requestAnimationFrame(() => {
      if (result.command) result.command.run();
      else if (result.conversation) router.push(`/app/conversations/${encodeURIComponent(result.conversation.id)}`);
    }));
  }

  function complete() {
    if (!selected) return;
    setQuery(selected.title);
    setSelection(selected.id);
    requestAnimationFrame(() => { input.current?.setSelectionRange(selected.title.length, selected.title.length); });
  }

  return createPortal(<dialog ref={dialog} className={styles.dialog} data-closing={closing} aria-label="Spotlight" onCancel={event => { event.preventDefault(); close(); }} onClick={event => { if (event.target === event.currentTarget) close(); }}>
    <div className={styles.panel}>
      <div className={styles.searchBar}>
        <WorkspaceIcon name="search" width="22" height="22" />
        <div className={styles.inputWrap}>
          {completion ? <div className={styles.completion} aria-hidden="true"><span>{query}</span>{completion}</div> : null}
          <input ref={input} role="combobox" aria-label="Search conversations and commands" aria-autocomplete="both" aria-expanded="true" aria-controls={listId} aria-activedescendant={selected ? `${listId}-${selected.id}` : undefined} aria-describedby={helpId} placeholder="Search conversations and commands…" autoComplete="off" spellCheck={false} value={query} onCompositionStart={() => setComposing(true)} onCompositionEnd={() => setComposing(false)} onSelect={event => setAtEnd(event.currentTarget.selectionStart === query.length && event.currentTarget.selectionEnd === query.length)} onChange={event => { setQuery(event.target.value); setSelection(null); setAtEnd(event.target.selectionStart === event.target.value.length); }} onKeyDown={event => {
            if (event.nativeEvent.isComposing || composing || closing) return;
            if (["ArrowDown", "ArrowUp"].includes(event.key)) {
              event.preventDefault();
              if (results.length) setSelection(results[(selectedIndex + (event.key === "ArrowDown" ? 1 : -1) + results.length) % results.length].id);
            } else if (event.key === "Enter" && !event.repeat && selected) {
              event.preventDefault(); openResult(selected);
            } else if ((event.key === "Tab" && !event.shiftKey && query && selected && query !== selected.title) || (event.key === "ArrowRight" && completion)) {
              event.preventDefault(); complete();
            }
          }} />
        </div>
        <button className={styles.close} type="button" aria-label="Close search" onClick={close}><WorkspaceIcon name="close" width="16" height="16" /></button>
      </div>
      <div className={styles.sectionLabel}>{query.trim() ? "Results" : "Recent conversations & commands"}<span role="status" className="sr-only">{results.length} results</span></div>
      <div className={styles.resultsViewport}>
        <div ref={list} id={listId} role="listbox" aria-label="Conversations and commands" className={styles.results}>
          {selected ? <div className={styles.selection} aria-hidden="true" style={{ transform: `translateY(${selectedIndex * 52}px)` }} /> : null}
          {results.map((result, index) => <div key={result.id} id={`${listId}-${result.id}`} role="option" aria-selected={index === selectedIndex} className={styles.result} data-result={result.id} onPointerMove={() => setSelection(result.id)} onMouseDown={event => event.preventDefault()} onClick={() => openResult(result)}>
            <WorkspaceIcon name={result.icon} width="18" height="18" />
            <span className={styles.resultTitle}>{result.title}</span>
            {result.key && shortcutsEnabled ? <kbd className={styles.shortcut}>{shortcutLabel(result.key)}</kbd> : null}
            <span className={styles.openHint} aria-hidden="true">↵</span>
          </div>)}
        </div>
        {!results.length ? <div className={styles.empty}><WorkspaceIcon name="search" width="24" height="24" /><p>No results found</p><span>Try a conversation title, page, or command.</span></div> : null}
      </div>
      <div id={helpId} className={styles.footer}><span><kbd>↑</kbd><kbd>↓</kbd> Navigate</span><span><kbd>↵</kbd> Open</span>{query && selected && query !== selected.title ? <span><kbd>Tab</kbd> Complete</span> : null}<span className={styles.escape}><kbd>Esc</kbd> Close</span></div>
      <div className={styles.shortcutHelp}><span>{shortcutsEnabled ? "Hold A + a key outside text fields. A + Space opens Spotlight." : "A shortcuts are off. Open Spotlight from the search button."}</span><button type="button" aria-pressed={shortcutsEnabled} onClick={() => onShortcutsChange(!shortcutsEnabled)}>A shortcuts {shortcutsEnabled ? "on" : "off"}</button></div>
    </div>
  </dialog>, document.body);
}
