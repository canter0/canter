"use client";
import { useEffect, useSyncExternalStore } from "react";
import type { OperatorAttachment } from "./operator-api";

type Draft = { items: OperatorAttachment[]; error: string; loaded: boolean };
const empty: Draft = { items: [], error: "", loaded: false };
const drafts = new Map<string, Draft>();
const listeners = new Set<() => void>();
const subscribe = (listener: () => void) => { listeners.add(listener); return () => { listeners.delete(listener); }; };
const publish = (key: string, value: Draft) => { drafts.set(key, value); listeners.forEach(listener => listener()); };
let database: Promise<IDBDatabase> | undefined;
function open() {
  return database ??= new Promise((resolve, reject) => {
    const request = indexedDB.open("canter-drafts", 1);
    request.onupgradeneeded = () => request.result.createObjectStore("attachments");
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => { database = undefined; reject(request.error); };
  });
}
export function useOperatorAttachmentDraft(key: string | null) {
  const draft = useSyncExternalStore(subscribe, () => key ? drafts.get(key) ?? empty : empty, () => empty);
  useEffect(() => {
    if (!key || drafts.has(key)) return;
    let cancelled = false;
    void open().then(db => {
      const request = db.transaction("attachments").objectStore("attachments").get(key);
      request.onsuccess = () => { if (!cancelled && !drafts.has(key)) publish(key, { items: request.result ?? [], error: "", loaded: true }); };
      request.onerror = () => { if (!cancelled) publish(key, { items: [], error: "Attachments cannot be restored in this browser.", loaded: true }); };
    }).catch(() => { if (!cancelled) publish(key, { items: [], error: "Attachments will stay here until you leave this page; browser storage is unavailable.", loaded: true }); });
    return () => { cancelled = true; };
  }, [key]);
  function update(items: OperatorAttachment[], targetKey = key) {
    const key = targetKey;
    if (!key) return;
    publish(key, { items, error: "", loaded: true });
    void open().then(db => new Promise<void>((resolve, reject) => {
      const transaction = db.transaction("attachments", "readwrite");
      if (items.length) transaction.objectStore("attachments").put(items, key);
      else transaction.objectStore("attachments").delete(key);
      transaction.oncomplete = () => resolve();
      transaction.onerror = () => reject(transaction.error);
      transaction.onabort = () => reject(transaction.error);
    })).catch(() => { const current = drafts.get(key); if (current?.items === items) publish(key, { ...current, error: "Your files are attached, but could not be saved for a reload. Keep this page open until you send." }); });
  }
  return { ...draft, update };
}

export function clearOperatorAttachmentDraft(key: string) {
  publish(key, { items: [], error: "", loaded: true });
  void open().then(db => {
    db.transaction("attachments", "readwrite").objectStore("attachments").delete(key);
  }).catch(() => { /* Local draft storage may be unavailable. */ });
}
