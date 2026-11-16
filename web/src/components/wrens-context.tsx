"use client";
import { createContext, useCallback, useContext, useEffect, useState, useSyncExternalStore, type ReactNode } from "react";
import { canterFetch } from "@/lib/canter-api";
import { useWrenResource, workBase } from "@/lib/wren-work";
import { wrenLifecycleEvent, type WrenLifecycle } from "@/lib/wren-lifecycle";
import { wrenSharingEvent } from "@/lib/wren-sharing";
import type { Wren, WrenRoom, RoomPerson } from "@/lib/wrens";
import { useWorkspace } from "./workspace-context";

type WrenContext = { lifecycle: ReturnType<typeof useWrenResource<{lifecycles: WrenLifecycle[]}>>; sidebarSearch: string; setSidebarSearch: (value: string) => void; sidebarView: string; setSidebarView: (value: string) => void; rooms: WrenRoom[]; people: RoomPerson[]; saveRoom: (room: WrenRoom, create: boolean) => Promise<WrenRoom>; wrens: Wren[]; loading: boolean; error: string; refresh: () => void; save: (wren: Wren, create: boolean) => Promise<Wren>; collapsedSections: string[]; toggleSection: (section: string) => void };
const Context = createContext<WrenContext | null>(null);
function subscribeLayout(changed: () => void) { window.addEventListener("storage", changed); window.addEventListener("canter-wren-layout", changed); return () => { window.removeEventListener("storage", changed); window.removeEventListener("canter-wren-layout", changed); }; }

export function WrensProvider({ children }: { children: ReactNode }) {
  const { data } = useWorkspace();
  const [sidebarSearch,setSidebarSearch] = useState(""), [sidebarView,setSidebarView] = useState("active");
  const lifecycle = useWrenResource<{lifecycles: WrenLifecycle[]}>(data ? `${workBase(data.workspace.id)}/wren-lifecycle` : "", 10000);
  const refreshLifecycle = lifecycle.refresh;
  const base = data ? `/workspaces/${encodeURIComponent(data.workspace.id)}/wrens` : "";
  const layoutKey = data ? `canter:wren-sections:${data.account.id}:${data.workspace.id}` : "";
  const [layoutFallback, setLayoutFallback] = useState({ key: "", value: "[]" });
  const rawLayout = useSyncExternalStore(subscribeLayout, () => { try { return layoutKey ? localStorage.getItem(layoutKey) ?? "[]" : "[]"; } catch { return layoutFallback.key === layoutKey ? layoutFallback.value : "[]"; } }, () => "[]");
  let collapsedSections: string[] = [];
  try { const saved: unknown = JSON.parse(rawLayout); if (Array.isArray(saved) && saved.every(value => typeof value === "string")) collapsedSections = saved; } catch { /* An invalid preference does not hide any work. */ }
  function toggleSection(section: string) { const value = JSON.stringify(collapsedSections.includes(section) ? collapsedSections.filter(item => item !== section) : [...collapsedSections, section]); try { localStorage.setItem(layoutKey, value); } catch { /* Keep the preference for this visit. */ } setLayoutFallback({ key: layoutKey, value }); window.dispatchEvent(new Event("canter-wren-layout")); }
  const [state, setState] = useState<{ base: string; wrens: Wren[]; rooms: WrenRoom[]; people: RoomPerson[]; error: string }>({ base: "", wrens: [], rooms: [], people: [], error: "" });
  const conversationRevision = data?.conversations.map(item => `${item.id}:${item.status}:${item.updatedAt}`).join("|") ?? "";
  const [revision, setRevision] = useState(0);
  const refresh = useCallback(() => setRevision(v => v + 1), []);
  useEffect(() => {
    const changed = () => { refreshLifecycle(); refresh(); };
    window.addEventListener(wrenLifecycleEvent, changed); window.addEventListener(wrenSharingEvent, changed);
    return () => { window.removeEventListener(wrenLifecycleEvent, changed); window.removeEventListener(wrenSharingEvent, changed); };
  }, [refreshLifecycle, refresh]);
  useEffect(() => {
    if (!base) return;
    const controller = new AbortController();
    let pending = false;
    const load = async () => {
      if (pending || controller.signal.aborted) return;
      pending = true;
      try { const [result, rooms, people] = await Promise.all([canterFetch<{ wrens: Wren[] }>(base, { signal: controller.signal }), canterFetch<{ rooms: WrenRoom[] }>(base.replace(/wrens$/, "rooms"), { signal: controller.signal }), canterFetch<{ people: RoomPerson[] }>(base.replace(/wrens$/, "rooms/people"), { signal: controller.signal })]); if (!controller.signal.aborted) setState({ base, wrens: result.wrens, rooms: rooms.rooms, people: people.people, error: "" }); }
      catch (cause) { if (!controller.signal.aborted) setState(previous => ({ base, rooms: previous.base === base ? previous.rooms : [], people: previous.base === base ? previous.people : [], wrens: previous.base === base ? previous.wrens : [], error: cause instanceof Error ? cause.message : "Couldn’t load your Wrens." })); }
      finally { pending = false; }
    };
    void load();
    const refreshVisible = () => { if (document.visibilityState === "visible") void load(); };
    const timer = window.setInterval(refreshVisible, 10000);
    window.addEventListener("focus", refreshVisible);
    document.addEventListener("visibilitychange", refreshVisible);
    return () => { controller.abort(); window.clearInterval(timer); window.removeEventListener("focus", refreshVisible); document.removeEventListener("visibilitychange", refreshVisible); };
  }, [base, revision, conversationRevision]);
  async function save(wren: Wren, create: boolean) {
    const saved = await canterFetch<Wren>(create ? base : `${base}/${encodeURIComponent(wren.id)}`, { method: create ? "POST" : "PATCH", body: JSON.stringify({ id: wren.id, name: wren.name, purpose: wren.purpose, shape: wren.shape, palette: wren.palette, role: wren.role ?? "", section: wren.section ?? "", publicationRevision:wren.publicationRevision??0 }) });
    setState(previous => ({ ...previous, base, error: "", wrens: previous.base === base && previous.wrens.some(item => item.id === saved.id) ? previous.wrens.map(item => item.id === saved.id ? saved : item) : [...(previous.base === base ? previous.wrens : []), saved] }));
    return saved;
  }
  async function saveRoom(room: WrenRoom, create: boolean) {
 const roomBase=base.replace(/wrens$/, "rooms");
 const saved=await canterFetch<WrenRoom>(create?roomBase:`${roomBase}/${encodeURIComponent(room.id)}`,{method:create?"POST":"PATCH",body:JSON.stringify({id:room.id,kind:room.kind,name:room.name,purpose:room.purpose,section:room.section,wrenIds:room.wrenIds,memberIds:room.memberIds})});
 refresh(); return saved;
 }
 return <Context.Provider value={{ lifecycle, sidebarSearch, setSidebarSearch, sidebarView, setSidebarView, rooms: state.base === base ? state.rooms : [], people: state.base === base ? state.people : [], saveRoom, wrens: state.base === base ? state.wrens : [], loading: !!base && state.base !== base, error: state.base === base ? state.error : "", refresh, save, collapsedSections, toggleSection }}>{children}</Context.Provider>;
}
export function useWrens() { const value = useContext(Context); if (!value) throw new Error("WrensProvider is required"); return value; }
