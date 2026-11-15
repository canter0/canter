"use client";
import { useSyncExternalStore } from "react";
const eventName = "canter-wren-destination";
const key = (account: string, workspace: string) => `canter:last-wren:${account}:${workspace}`;
function subscribe(changed: () => void) {
  window.addEventListener(eventName, changed);
  window.addEventListener("storage", changed);
  return () => { window.removeEventListener(eventName, changed); window.removeEventListener("storage", changed); };
}
function destination(id: string) {
  if (/^room_[a-zA-Z0-9_-]{3,85}$/.test(id)) return `/app/wrens/rooms/${id}`;
  if (/^wren_[a-zA-Z0-9_-]{3,85}$/.test(id)) return `/app/wrens/${id}`;
  return "/app/wrens";
}
export function rememberWren(account: string, workspace: string, id: string) {
  document.cookie = `canter_last_wren=${encodeURIComponent(id)}; Path=/app/wrens; SameSite=Lax`;
  try { localStorage.setItem(key(account, workspace), id); } catch { /* Server fallback still validates the cookie. */ }
  window.dispatchEvent(new Event(eventName));
}
export function useWrenDestination(account?: string, workspace?: string) {
  return useSyncExternalStore(subscribe, () => {
    try { return account && workspace ? destination(localStorage.getItem(key(account, workspace)) ?? "") : "/app/wrens"; }
    catch { return "/app/wrens"; }
  }, () => "/app/wrens");
}
