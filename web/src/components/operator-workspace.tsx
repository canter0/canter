"use client";
import { WrenContextDialog } from "./wren-context-dialog";
import { appsPageHref } from "@/lib/wren-apps";
import { WrenWorkOverview } from "./wren-work-overview";
import { wrenSurfacePlacement } from "@/lib/wren-surface-placement";
import { WrenWorkNavigation } from "./wren-work-navigation";

import { completeOnboarding, openWrenOnboarding } from "@/lib/onboarding";
import { operatorDraftKey, operatorReplyDraftKey, serializeOperatorReplyAnchor, parseOperatorReplyAnchor, isReplyTargetForAnchor } from "@/lib/operator-draft-storage";
import {CanterSelect} from "./canter-select";

import { useCallback, useEffect, useEffectEvent, useLayoutEffect, useId, useRef, useState, useSyncExternalStore, type FormEvent, type ReactNode } from "react";
import { useMediaQuery } from "./use-media-query";
import { usePopover } from "./use-popover";
import { useFocusContainment } from "./use-focus-containment";
import { moveMenuFocus } from "@/lib/interaction";
import { useRouter, useSearchParams } from "next/navigation";
import { useOperatorAttachmentDraft } from "@/lib/operator-attachment-draft";
import { OperatorMessageContext, OperatorTurn, ResponseText } from "./operator-turn";
import { MorphLabel } from "./conversation-motion";
import { MotionPresence } from "./motion-presence";
import { WorkspaceLoading } from "./workspace-loading";
import { operatorModelOptions, parseModelPreferences, type OperatorModelOptions } from "@/lib/operator-models";
import { OperatorComposer } from "./operator-composer";
import { githubConnectURL, type GitHubRepositoryResult } from "@/lib/github-connection";
import { ProviderIcon } from "./provider-icon";
import { OnboardingPrism } from "./onboarding-prism";
import { GitHubRepositories } from "./github-repositories";
import { AppShell } from "./app-shell";
import { WrenAvatar } from "./wren-avatar";
import { WrenConversationIntro } from "./wren-conversation-intro";
import { useWrenRead } from "./use-wren-read";
import type { OperatorMessage, OperatorReplyTarget } from "@/lib/operator-api";
import { WrenTranscript } from "./wren-transcript";
import { WrenWorkStrip } from "./wren-work-strip";
import { notifyWorkUpdate } from "@/lib/wren-work";
import { RoomTranscript } from "./room-transcript";
import type { Wren, WrenRoom } from "@/lib/wrens";
import type { SpotlightCommand } from "@/lib/spotlight";
import { shortcutLabel } from "@/lib/a-shortcuts";
import { useWorkspace } from "./workspace-context";
import { WorkspaceIcon } from "./workspace-icon";
import { OperatorSurfaceView } from "./operator-surface";
import { canterFetch, CanterAPIError } from "@/lib/canter-api";
import { conversationBase, conversationMessages, isSurface, surfaceKey, surfaceLabels, type ConversationDetail, type OperatorEvent, type OperatorRun, type OperatorSurface } from "@/lib/operator-api";
import { mergeOperatorEvents } from "@/lib/operator-events";
import { visibleSubscription } from "@/lib/visible-subscription";
import { mergeConversationHistory, mergeEarlierConversationPage } from "@/lib/conversation-history";
import { pendingRepositoryPicker, takeReviewSurface } from "@/lib/operator-surface-events";
import { roomAudience, roomMentions, restoredRoomRecipient } from "@/lib/room-audience";
import { surfaceResultName } from "@/lib/surface-result-name";
import { displayName } from "@/lib/display-name";
import { type WrenLifecycle, wrenLifecycleEvent } from "@/lib/wren-lifecycle";
import styles from "./operator-workspace.module.css";

const welcomeFallback = "Welcome to Canter. Work with people and Wrens—AI teammates you shape around what you need.\n\nCreate your first Wren, or skip to a general conversation. GitHub is optional when you want to work with code.";
function projectWelcomeFallback(value: GitHubRepositoryResult) {
  const names = value.repositories.slice(0, 2).map(repo => repo.full_name);
  return names.length ? `GitHub is connected. I can see ${names.join(" and ")} in your shared repositories. What would you like to work on with a Wren, or in a general conversation?` : "GitHub is connected. No repositories are shared with Canter yet, but you can start with a Wren or a general conversation. What would you like to work on?";
}

const subscribeDraft = (onChange: () => void) => { window.addEventListener("canter-draft", onChange); return () => window.removeEventListener("canter-draft", onChange); };
const subscribeModel = (onChange: () => void) => { window.addEventListener("canter-model", onChange); return () => window.removeEventListener("canter-model", onChange); };
const activeRun = (run?: OperatorRun | null) => !!run && ["queued", "running"].includes(run.status);
const restoredSurfaces = (events: OperatorEvent[]) => [...new Map(events.filter(event => event.kind === "surface" && isSurface(event.data) && !["github", "compute", "storage"].includes(String(event.data.kind))).map(event => [surfaceKey(event.data as OperatorSurface), event.data as OperatorSurface])).values()];

export function OperatorWorkspace({ id: initialId, githubResult, focusComposer, initialDetail = null, initialLoadedAt, welcome = false, wren, room, roomPeopleCount, onConversationUpdate, sidebarContent, onEditWren, inspector, inspectorOpen, onCloseInspector, prefillPrompt }: { wren?: Wren; room?: WrenRoom; roomPeopleCount?:number; onConversationUpdate?: () => void; sidebarContent?: ReactNode; onEditWren?: () => void; inspector?: ReactNode; inspectorOpen?: boolean; onCloseInspector?: () => void; id?: string; githubResult?: string; focusComposer?: boolean; initialDetail?: ConversationDetail | null; initialLoadedAt?: number; welcome?: boolean; prefillPrompt?: string }) {
  const router = useRouter();
  const searchParams=useSearchParams();
  const jumpMessage=searchParams.get("message"),jumpWork=searchParams.get("work"),jumpWorkId=searchParams.get("workId");
  const jumpHandled=useRef("");
  const wrenWorkspace = !!wren || !!room || !!initialId?.startsWith("conv_wren_") || !!initialId?.startsWith("conv_room_");
 const managedModel = true;
 const [recipientFallback,setRecipientFallback]=useState<{key:string;value:string}|null>(null);
 const [replyDraftState,setReplyDraftState]=useState<{key:string;raw:string;target:OperatorReplyTarget|null;status:"checking"|"ready"|"error";notice:string}|null>(null);
 const [replyFallback,setReplyFallback]=useState<{key:string;raw:string}|null>(null),[replyAttempt,setReplyAttempt]=useState(0);
 const replyGeneration=useRef(0),replyRecipientApplied=useRef("");
  const [id, setId] = useState(initialId);
  const prefillApplied = useRef(false);
  useEffect(() => {
    if (!prefillPrompt || prefillApplied.current) return;
    prefillApplied.current = true;
    editDraft(prefillPrompt);
    requestAnimationFrame(() => composer.current?.focus());
  }, [prefillPrompt]); // eslint-disable-line react-hooks/exhaustive-deps -- prefill runs once per mount
  const [welcomeStage, setWelcomeStage] = useState<"arrival" | "welcome" | "leaving" | "done">(welcome && !initialId ? "arrival" : "done");
  const onboarding = welcomeStage === "arrival" || welcomeStage === "welcome";
  const intro = useRef<HTMLDivElement>(null);
  const [introPosition, setIntroPosition] = useState<{ left: number; top: number; width: number } | null>(null);
  const [introText, setIntroText] = useState("");
  const [projectWelcome, setProjectWelcome] = useState("");
  const [welcomeGitHub, setWelcomeGitHub] = useState<GitHubRepositoryResult | null>(null);
  const [welcomeGitHubChecked, setWelcomeGitHubChecked] = useState(false);
  const [welcomeReady, setWelcomeReady] = useState(false);
  const [creatingFirstWren,setCreatingFirstWren]=useState(false),[onboardingActionError,setOnboardingActionError]=useState("");
  const firstWrenLock=useRef(false);
  const welcomeController = useRef<AbortController | null>(null);
  const onboardingMotion = useRef(false);
  const { data, conversationCache, retry: refreshWorkspace } = useWorkspace();
  const workspace = data?.workspace.id;
  const [lifecycle,setLifecycle]=useState<{key:string;value:WrenLifecycle}|null>(null);
  const archived=lifecycle?.key===`${workspace}:${id}`&&lifecycle.value.archived;
  const personaUnavailable=wren?.sharedAvailable===false;
  useEffect(()=>{
    if(!wrenWorkspace||!workspace||!id)return;
    const controller=new AbortController();let busy=false;
    async function refresh(){if(busy||controller.signal.aborted)return;busy=true;try{const value=await canterFetch<WrenLifecycle>(`/workspaces/${encodeURIComponent(workspace!)}/wren-lifecycle?conversationId=${encodeURIComponent(id!)}`,{signal:controller.signal});if(!controller.signal.aborted)setLifecycle({key:`${workspace}:${id}`,value});}catch{/* Existing archived state remains until a successful refresh. Server validates every send. */}finally{busy=false}}
    const changed=()=>{void refresh()};void refresh();window.addEventListener(wrenLifecycleEvent,changed);const timer=setInterval(()=>{if(document.visibilityState==="visible")void refresh()},15000);
    return()=>{controller.abort();clearInterval(timer);window.removeEventListener(wrenLifecycleEvent,changed)};
  },[workspace,id,wrenWorkspace]);

  const [snapshot] = useState(() => id ? conversationCache.peek(id) : undefined);
  const [detail, setDetail] = useState<ConversationDetail | null>(snapshot?.detail ?? initialDetail);
  const [sourceDetail, setSourceDetail] = useState(initialDetail);
  if (sourceDetail !== initialDetail) {
    setSourceDetail(initialDetail);
    if (initialDetail && (!id || !conversationCache.peek(id))) setDetail(current => mergeConversationHistory(current, initialDetail));
  }
  const [events, setEvents] = useState<OperatorEvent[]>(snapshot?.events ?? []);
  const [readSequence,setReadSequence]=useState(0);
  const latestEvents = useRef(events);
  const latestDetail=useRef(detail);
  useLayoutEffect(()=>{latestDetail.current=detail;},[detail]);
  const detailReady=!!detail;
  const [selected, setSelected] = useState<OperatorSurface | null>(() => restoredSurfaces(snapshot?.events ?? []).filter(surface => !wrenWorkspace || wrenSurfacePlacement(surface.kind) === "split").at(-1) ?? null);
  const [opened, setOpened] = useState<OperatorSurface[]>(() => restoredSurfaces(snapshot?.events ?? []).filter(surface => !wrenWorkspace || wrenSurfacePlacement(surface.kind) === "split"));
  const [wide, setWide] = useState(false);
  const [surfaceNames, setSurfaceNames] = useState<Record<string, string>>({});
  const [workOpen, setWorkOpen] = useState(false);
  const workCanvas = useRef<HTMLDivElement>(null);
  const [workCollection, setWorkCollection] = useState<OperatorSurface | null>(null);
  const [workCollections, setWorkCollections] = useState<OperatorSurface[]>([]);
  const [configurationOpenRequest, setConfigurationOpenRequest] = useState(0);
  const [configurationOpen, setConfigurationOpen] = useState(false);
  const [configuration, setConfiguration] = useState<OperatorSurface | null>(null);
  const [configurations, setConfigurations] = useState<OperatorSurface[]>([]);
  const closeConfiguration = useCallback(() => { setConfigurationOpen(false); onCloseInspector?.(); }, [onCloseInspector]);
  const [viewMenu, setViewMenu] = useState(false);
  const [showScroll, setShowScroll] = useState(false);
  const [hasEarlierMessages, setHasEarlierMessages] = useState(false);
  const [historyLoading, setHistoryLoading] = useState<{ id: string; workspace: string; request: number } | null>(null);
  const [historyProblem, setHistoryProblem] = useState<{ id: string; workspace: string; message: string } | null>(null);
  const historyRequest = useRef(0);
  const historyRoute = useRef({ id, workspace });
  useLayoutEffect(()=>{
    historyRoute.current = { id, workspace };
    historyRequest.current++;
  },[id,workspace]);
  const loadingEarlier = !!historyLoading && historyLoading.id === id && historyLoading.workspace === workspace;
  const historyError = historyProblem && historyProblem.id === id && historyProblem.workspace === workspace ? historyProblem.message : "";
  const [panelOpen, setPanelOpen] = useState(false);
  const panelId = useId();
  const [submittedPrompt, setSubmittedPrompt] = useState("");
  const [submittedSurface, setSubmittedSurface] = useState<OperatorSurface | null>(null);
  const composerArea = useRef<HTMLDivElement>(null);
  const composerOrigin = useRef<DOMRect | null>(null);
  const [inlineGitHub, setInlineGitHub] = useState(!!githubResult && !welcome);
  const [dismissedGitHubSequence, setDismissedGitHubSequence] = useState(0);
  const [fallbackDraft, setFallbackDraft] = useState("");
  const [error, setError] = useState("");
  const [connectionError, setConnectionError] = useState("");
  const [deliveryNotice, setDeliveryNotice] = useState("");
  const [sending, setSending] = useState(false);
  const [stopping, setStopping] = useState(false);
  const sendLock = useRef(false);
  const stopLock = useRef(false);
  const reducedMotion = useMediaQuery("(prefers-reduced-motion: reduce)");
  useEffect(() => {
    if (welcomeStage !== "leaving") return;
    const timer = window.setTimeout(() => { setWelcomeStage("done"); setIntroText(""); setProjectWelcome(""); }, reducedMotion ? 0 : 1000);
    return () => window.clearTimeout(timer);
  }, [welcomeStage, reducedMotion]);
  useEffect(() => {
    if (!welcome || !onboarding || initialId || !workspace) return;
    const controller = new AbortController();
    welcomeController.current = controller;
    const base = `/workspaces/${encodeURIComponent(workspace)}`;
    async function introduce() {
      const [greeting, github] = await Promise.allSettled([
        canterFetch<{ text: string }>(`${base}/welcome`, { method: "POST", signal: controller.signal }),
        canterFetch<GitHubRepositoryResult>(`${base}/github/repositories`, { signal: controller.signal }),
      ]);
      if (controller.signal.aborted) return;
      setIntroText(greeting.status === "fulfilled" ? greeting.value.text.trim() || welcomeFallback : welcomeFallback);
      const connected = github.status === "fulfilled" ? github.value : null;
      setWelcomeGitHub(connected);
      setWelcomeGitHubChecked(true);
      if (!connected?.connection.connected) return;
      try {
        const response = await canterFetch<{ text: string }>(`${base}/welcome?stage=projects`, { method: "POST", signal: controller.signal });
        if (!controller.signal.aborted) setProjectWelcome(response.text.trim() || projectWelcomeFallback(connected));
      } catch {
        if (!controller.signal.aborted) setProjectWelcome(projectWelcomeFallback(connected));
      }
      if (!controller.signal.aborted) setWelcomeReady(true);
    }
    // Strict Mode's setup/cleanup probe must not consume a generation budget.
    const timer = window.setTimeout(() => void introduce(), 0);
    return () => { window.clearTimeout(timer); controller.abort(); };
  }, [welcome, onboarding, initialId, workspace]);
  const overlayPanel = useMediaQuery("(max-width: 1100px)");
  const surfacePanel = useRef<HTMLElement>(null);
  const panelTrigger = useRef<HTMLButtonElement>(null);
  const panelReturnFocus = useRef<HTMLElement | null>(null);
  const closePanel = useCallback(() => {
    setPanelOpen(false); onCloseInspector?.();
    requestAnimationFrame(() => {
      const target = panelReturnFocus.current;
      if (target?.isConnected && target.getClientRects().length && !target.closest('[inert], [hidden]')) target.focus({ preventScroll: true });
      else panelTrigger.current?.focus({ preventScroll: true });
    });
  }, [onCloseInspector]);
  const closeViewMenu = useCallback(() => setViewMenu(false), []);
  const [attempt, setAttempt] = useState(0);
  const [contextFallback, setContextFallback] = useState<{key:string|null;raw:string}|null>(null);
  const composer = useRef<HTMLTextAreaElement>(null);
  const viewMenuAnchor = useRef<HTMLDivElement>(null);
  const transcript = useRef<HTMLDivElement>(null);
  const transcriptContent = useRef<HTMLDivElement>(null);
  const followScroll = useRef(true);
  const pending = useRef<{ id: string; requestId: string; message: string; signature: string } | null>(null);
  const storageKey = operatorDraftKey(data?.account.id, workspace, id);
  const contextStorageKey = storageKey ? `${storageKey}:context` : null;
  const contextRaw = useSyncExternalStore(subscribeDraft, () => {
    const fallback = contextFallback?.key === contextStorageKey ? contextFallback.raw : "";
    try { return contextStorageKey ? sessionStorage.getItem(contextStorageKey) ?? fallback : ""; } catch { return fallback; }
  }, () => "");
  let attachedContext: OperatorSurface | null | undefined = undefined;
  if (contextRaw) {
    attachedContext = null;
    try {
      const value = JSON.parse(contextRaw);
      if (value && typeof value === "object" && isSurface(value)) attachedContext = value;
    } catch { /* An invalid saved reference must not select an unrelated panel. */ }
  }
  const setAttachedContext = useCallback((value: OperatorSurface | null) => {
    const raw = JSON.stringify(value);
    setContextFallback({key:contextStorageKey,raw});
    try { if (contextStorageKey) sessionStorage.setItem(contextStorageKey,raw); } catch { /* Retain this visit's explicit choice. */ }
    window.dispatchEvent(new Event("canter-draft"));
  }, [contextStorageKey]);
  // Work panels are optional views in Wren chats, not implicit attachments.
  // Keep an explicit context choice with the draft across chat navigation.
  const messageContext = attachedContext === undefined ? (wrenWorkspace ? null : selected) : attachedContext;
  const recipientStorageKey=room&&storageKey?`${storageKey}:audience`:null;
  const roomRecipientRaw=useSyncExternalStore(subscribeDraft,()=>{
    const fallback=recipientFallback?.key===recipientStorageKey?recipientFallback.value:"people";
    try{return recipientStorageKey?sessionStorage.getItem(recipientStorageKey)??fallback:"people";}catch{return fallback;}
  },()=>"people");
  const roomRecipient=restoredRoomRecipient(roomRecipientRaw,room?.wrens??[]);
  const setRoomRecipient=useCallback((value:string)=>{
    if(!recipientStorageKey)return;
    setRecipientFallback({key:recipientStorageKey,value});
    try{sessionStorage.setItem(recipientStorageKey,value);}catch{/* Keep the current visit usable when storage is unavailable. */}
    window.dispatchEvent(new Event("canter-draft"));
  },[recipientStorageKey]);
  const replyStorageKey=operatorReplyDraftKey(data?.account.id,workspace,id);
  const replyInRoom=!!room;
  const replyRaw=useSyncExternalStore(subscribeDraft,()=>{
    const fallback=replyFallback?.key===replyStorageKey?replyFallback.raw:"";
    try{return replyStorageKey?sessionStorage.getItem(replyStorageKey)??fallback:"";}catch{return fallback;}
  },()=>"");
  const currentReplyState=replyDraftState?.key===replyStorageKey&&replyDraftState.raw===replyRaw?replyDraftState:null;
  const replyTo=currentReplyState?.target??null;
  const replyChecking=!!replyRaw&&currentReplyState?.status!=="ready";
  const replyNotice=currentReplyState?.notice??"";
  useLayoutEffect(()=>()=>{replyGeneration.current++;replyRecipientApplied.current="";},[replyStorageKey]);
  function setReplyTo(target:OperatorReplyTarget|null,notice=""){
    replyGeneration.current++;
    if(!replyStorageKey)return;
    const raw=target?serializeOperatorReplyAnchor(target):"";
    replyRecipientApplied.current=target?`${replyStorageKey}:${raw}`:"";
    setReplyFallback({key:replyStorageKey,raw});
    try{if(raw)sessionStorage.setItem(replyStorageKey,raw);else sessionStorage.removeItem(replyStorageKey);}catch{/* Keep this visit's scoped anchor in memory. */}
    setReplyDraftState({key:replyStorageKey,raw,target,status:target?"checking":"ready",notice});
    window.dispatchEvent(new Event("canter-draft"));
  }
  useEffect(()=>{
    if(!replyStorageKey||!replyRaw||!id||!workspace)return;
    const controller=new AbortController(),version=replyGeneration.current,key=replyStorageKey,raw=replyRaw;
    async function restore(){
      const anchor=parseOperatorReplyAnchor(raw,id);
      try{
        if(!anchor)throw new CanterAPIError(404,"Invalid saved reply");
        const value=await canterFetch<OperatorReplyTarget>(`/workspaces/${encodeURIComponent(workspace!)}/wren-replies?conversationId=${encodeURIComponent(id!)}&messageId=${encodeURIComponent(anchor.messageId)}`,{signal:controller.signal});
        if(controller.signal.aborted||replyGeneration.current!==version)return;
        if(!isReplyTargetForAnchor(value,anchor))throw new CanterAPIError(404,"Invalid reply target");
        setReplyDraftState({key,raw,target:value,status:"ready",notice:""});
        if(replyInRoom&&replyRecipientApplied.current!==`${key}:${raw}`){replyRecipientApplied.current=`${key}:${raw}`;setRoomRecipient(value.role==="assistant"&&value.canWake&&value.wrenId?value.wrenId:"people");}
      }catch(cause){
        if(controller.signal.aborted||replyGeneration.current!==version)return;
        if(cause instanceof CanterAPIError&&(cause.status===403||cause.status===404)){
          replyGeneration.current++;
          try{sessionStorage.removeItem(key);}catch{/* Optional storage. */}
          setReplyFallback({key,raw:""});setReplyDraftState({key,raw:"",target:null,status:"ready",notice:"The original reply message is no longer available. The reply was removed; your draft is unchanged."});window.dispatchEvent(new Event("canter-draft"));
        }else setReplyDraftState(value=>({key,raw,target:value?.key===key&&value.raw===raw?value.target:null,status:"error",notice:"Couldn’t verify the saved reply. Retry before sending; your draft is unchanged."}));
      }
    }
    void restore();return()=>controller.abort();
  },[replyStorageKey,replyRaw,id,workspace,replyAttempt,replyInRoom,setRoomRecipient]);

  const modelKey = !managedModel && workspace && data ? `canter:conversation-model:${data.account.id}:${workspace}:${id ?? "new"}` : null;
  const [modelFallback, setModelFallback] = useState<{ key: string; value: string } | null>(null);
  const modelOverride = useSyncExternalStore(subscribeModel, () => {
    try { return modelKey ? sessionStorage.getItem(modelKey) ?? (modelFallback?.key === modelKey ? modelFallback.value : "") : ""; }
    catch { return modelFallback?.key === modelKey ? modelFallback?.value ?? "" : ""; }
  }, () => "");
  const selectedModel = managedModel ? undefined : modelOverride || (detail?.conversation.id === id ? detail?.run?.model : undefined) || data?.agent.model;
  const optionsKey = modelKey ? `${modelKey}:options` : null;
  const [optionsFallback, setOptionsFallback] = useState<{ key: string; value: string } | null>(null);
  const optionsRaw = useSyncExternalStore(subscribeModel, () => {
    const fallback = optionsFallback?.key === optionsKey ? optionsFallback.value : "";
    try { return optionsKey ? sessionStorage.getItem(optionsKey) ?? fallback : ""; } catch { return fallback; }
  }, () => "");
  const modelPreferences = { ...(detail && detail.conversation.id === id && detail.run ? { [detail.run.model]: detail.run.modelOptions ?? {} } : {}), ...parseModelPreferences(optionsRaw) };
  const selectedModelOptions = operatorModelOptions(selectedModel, selectedModel ? modelPreferences[selectedModel] : undefined);
  const running = activeRun(detail?.run);
  const ownRun = detail?.conversation.id === id ? detail?.ownRun : null;
  const canStop = activeRun(ownRun) && data?.workspace.role !== "viewer";
 const lastMessageID=detail?.messages.at(-1)?.id;
 const runStatus=detail?.run?.status;
 useEffect(()=>{onConversationUpdate?.();},[onConversationUpdate,lastMessageID,runStatus]);
  const attachmentDraft = useOperatorAttachmentDraft(storageKey);
  const inspecting = inspectorOpen ?? !!inspector;
  const showPanel = wrenWorkspace ? panelOpen && !inspecting && !configurationOpen && !workOpen : panelOpen || inspecting;
  useWrenRead({workspaceId:wrenWorkspace?workspace:undefined,conversationId:id,sequence:readSequence,covered:!!jumpMessage||workOpen||configurationOpen||inspecting||((wide||overlayPanel)&&showPanel),transcript,onRead:onConversationUpdate});
  function replyToMessage(message:OperatorMessage){
    const target:OperatorReplyTarget={messageId:message.id,conversationId:id!,wrenId:message.role==="assistant"?(message.speaker?.id??wren?.id):undefined,role:message.role,senderName:(message.role==="assistant"?message.speaker?.name:undefined)||message.senderName||(message.role==="assistant"?wren?.name:"Person")||"Message",excerpt:message.content.slice(0,280),canWake:message.role==="assistant"&&(room?room.wrenIds.includes(message.speaker?.id??""):!!wren)};
    setReplyTo(target); if(room)setRoomRecipient(target.canWake&&target.wrenId?target.wrenId:"people");requestAnimationFrame(()=>composer.current?.focus());
  }
  useEffect(() => {
    if (!inspecting || wrenWorkspace) return;
    if (document.activeElement instanceof HTMLElement && !surfacePanel.current?.contains(document.activeElement)) {
      const active = document.activeElement;
      panelReturnFocus.current = active.closest('[role="menu"]')?.parentElement?.querySelector<HTMLElement>('button[aria-haspopup="menu"]') ?? active;
    }
    const frame = requestAnimationFrame(() => (surfacePanel.current?.querySelector<HTMLInputElement>("input[name=name]:not(:disabled)") ?? surfacePanel.current)?.focus());
    return () => cancelAnimationFrame(frame);
  }, [inspecting, wrenWorkspace]);
  useFocusContainment(overlayPanel && showPanel, surfacePanel, closePanel, panelReturnFocus);
  usePopover(viewMenu, viewMenuAnchor, closeViewMenu);
  useEffect(() => {
    if (!wrenWorkspace || !id || searchParams.get("apps") !== "1") return;
    const target = new URLSearchParams({ conversation: id });
    const github = searchParams.get("github"), connection = searchParams.get("appConnection");
    if (github) { target.set("github", github); target.set("app", "github"); }
    if (connection) { target.set("appConnection", connection); target.set("app", "linear"); }
    router.replace(`/app/customize?${target}`);
  }, [wrenWorkspace, id, searchParams, router]);
  useEffect(() => {
    if (!focusComposer || id || !workspace || !attachmentDraft.loaded) return;
    const frame = requestAnimationFrame(() => composer.current?.focus());
    return () => cancelAnimationFrame(frame);
  }, [focusComposer, id, workspace, attachmentDraft.loaded]);

  const draft = useSyncExternalStore(subscribeDraft, () => {
    try { return storageKey ? sessionStorage.getItem(storageKey) ?? fallbackDraft : fallbackDraft; } catch { return fallbackDraft; }
  }, () => "");

  const audience = room ? roomAudience(roomRecipient, draft, room.wrens) : undefined;
  const mentionedWrens = room ? roomMentions(draft, room.wrens) : [];
  const peopleOnly = audience?.postOnly ?? false;
  const roomRecipientIDs = audience?.recipientIds;

  useEffect(() => {
    latestEvents.current = events;
    if (id && detail) conversationCache.save(id, detail, events, detail === initialDetail ? initialLoadedAt : undefined);
  }, [id, detail, events, conversationCache, initialDetail, initialLoadedAt]);

  useEffect(() => {
    if (!id || !workspace) return;
    let cursor = latestEvents.current.at(-1)?.sequence ?? 0;
    const seenReviews = new Set<string>();
    takeReviewSurface(latestEvents.current, seenReviews);
    async function sync(signal: AbortSignal, refresh = false) {
      const value = await conversationCache.load(id!, refresh);
      if (!signal.aborted) { setDetail(current => mergeConversationHistory(current, value)); setReadSequence(cursor); setDeliveryNotice(""); setConnectionError(""); }
    }
    async function follow(signal: AbortSignal) {
      let restoring = true;
      // Render cached history immediately, then reconcile with the server.
      // Replayed events may already be at the latest cursor even when a
      // restored route snapshot is stale, so events alone cannot repair it.
      await sync(signal, true);
      while (!signal.aborted) {
        const result = await canterFetch<{ events: OperatorEvent[] }>(`${conversationBase(workspace!)}/${encodeURIComponent(id!)}/events?after=${cursor}`, { signal });
        if (signal.aborted) return;
        const batch = result.events ?? [];
        if (batch.length) {
          if (!restoring && batch.some(event => event.kind === "surface" && event.data.kind === "github")) setInlineGitHub(false);
          cursor = batch[batch.length - 1].sequence;
          setEvents(current => mergeOperatorEvents(current, batch));
          const surfaces = batch.filter(event => event.kind === "surface" && isSurface(event.data) && !["github", "compute", "storage"].includes(String(event.data.kind)) && (!wrenWorkspace || wrenSurfacePlacement((event.data as OperatorSurface).kind) === "split"));
          if (surfaces.length) {
            setOpened(current => [...new Map([...current, ...surfaces.map(event => event.data as OperatorSurface)].map(item => [surfaceKey(item), item])).values()]);
          }
          if (batch.some(event => event.kind === "finished" || event.kind === "artifact" || event.kind === "message_draft" || event.kind === "message_inbox" || event.kind === "message_receipt" || event.kind === "surface" && ["wren-artifact", "wren-messaging", "wren-projects"].includes(String(event.data.kind)))) notifyWorkUpdate(workspace!, id);
          // Preserve the user's focus and chosen tab during background work.
          // Replayed or dismissed reviews never reopen the panel.
          const review = takeReviewSurface(batch, seenReviews);
          const editing = document.activeElement?.closest('input, select, textarea, [contenteditable="true"]');
          if (review && !restoring && !editing) { setSelected(review); setPanelOpen(true); }
          if (batch.some(event => event.kind === "queued" || event.kind === "working" || event.kind === "finished" || event.kind === "job_result" || event.kind === "artifact" || event.kind === "title")) { await sync(signal, true); if (signal.aborted) return; refreshWorkspace(); }
        }
        if (batch.length < 300) restoring = false;
        setConnectionError("");
      }
    }
    return visibleSubscription(document, follow, cause => {
      if (cause instanceof CanterAPIError && [401, 403, 404].includes(cause.status)) { conversationCache.invalidate(id!); setDetail(null); setEvents([]); setOpened([]); setSelected(null); }
      setConnectionError(cause instanceof Error ? cause.message : "Updates were interrupted. Your work continues on the server.");
    });
  }, [id, workspace, attempt, refreshWorkspace, conversationCache, wrenWorkspace]);

  useEffect(() => {
    const element = transcript.current;
    const content = transcriptContent.current;
    if (!element || !content) return;
    let frame = 0;
    let previous = 0;
    let initialized = false;
    const tick = (time: number) => {
      frame = 0;
      if (!followScroll.current) return;
      const target = Math.max(0, element.scrollHeight - element.clientHeight);
      const gap = target - element.scrollTop;
      if (reducedMotion || Math.abs(gap) < 1) { element.scrollTop = target; return; }
      const elapsed = previous ? Math.min(64, time - previous) : 16;
      previous = time;
      // Follow a moving destination instead of restarting native smooth scroll
      // for every token. User scrolling immediately stops the glide.
      element.scrollTop += gap * (1 - Math.exp(-elapsed / 70));
      frame = requestAnimationFrame(tick);
    };
    const schedule = () => {
      if (!followScroll.current || !content.childElementCount) return;
      if (!initialized) {
        element.scrollTop = element.scrollHeight;
        initialized = true;
        return;
      }
      if (!frame) { previous = 0; frame = requestAnimationFrame(tick); }
    };
    const observer = new ResizeObserver(schedule);
    observer.observe(element);
    observer.observe(content);
    element.addEventListener("canter-follow-latest", schedule);
    document.addEventListener("visibilitychange", schedule);
    schedule();
    return () => {
      observer.disconnect();
      cancelAnimationFrame(frame);
      element.removeEventListener("canter-follow-latest", schedule);
      document.removeEventListener("visibilitychange", schedule);
    };
  }, [reducedMotion]);

  useEffect(() => {
    if (selected && panelOpen) {
      const index = opened.findIndex(item => surfaceKey(item) === surfaceKey(selected));
      const tab = document.getElementById(`${panelId}-tab-${index}`);
      const reveal = () => tab?.scrollIntoView({ block: "nearest", inline: "nearest" });
      reveal();
      const observer = new ResizeObserver(reveal);
      const tablist = tab?.closest('[role="tablist"]');
      if (tablist) observer.observe(tablist);
      return () => observer.disconnect();
    }
  }, [selected, opened, panelOpen, panelId]);

  useLayoutEffect(() => {
    const origin = composerOrigin.current;
    const morphing = onboardingMotion.current;
    const element = morphing ? composer.current?.closest<HTMLElement>("[data-operator-composer]") : composerArea.current;
    if (!origin || !element) return;
    composerOrigin.current = null;
    onboardingMotion.current = false;
    if (reducedMotion) return;
    const destination = element.getBoundingClientRect();
    const animation = element.animate(morphing ? [
      { transform: `translate(${origin.left - destination.left}px, ${origin.top - destination.top}px)`, width: `${origin.width}px` },
      { transform: "translate(0,0)", width: `${destination.width}px` },
    ] : [{ transform: `translateY(${origin.top - destination.top}px)` }, { transform: "translateY(0)" }], { duration: morphing ? 820 : 240, easing: "cubic-bezier(.22,.8,.18,1)" });
    return () => animation.cancel();
  }, [submittedPrompt, reducedMotion, welcomeStage]);

  async function createFirstWren(){
    if(firstWrenLock.current||!onboarding)return;
    firstWrenLock.current=true;setCreatingFirstWren(true);setOnboardingActionError("");
    try{await openWrenOnboarding(canterFetch,destination=>{welcomeController.current?.abort();router.push(destination);});}
    catch{firstWrenLock.current=false;setCreatingFirstWren(false);setOnboardingActionError("Couldn’t open Wrens. Try again.");}
  }
  function enterWorkspace() {
    if (!onboarding) return;
    const position = intro.current?.getBoundingClientRect();
    if (position) setIntroPosition({ left: position.left, top: position.top, width: position.width });
    welcomeController.current?.abort();
    // Keep the transition immediate and let the write finish across navigation.
    void completeOnboarding(canterFetch).catch(() => { /* A later welcome can retry completion. */ });
    window.history.replaceState(null, "", "/app?compose=1");
    const composerBox = composer.current?.closest<HTMLElement>("[data-operator-composer]")?.getBoundingClientRect();
    composerOrigin.current = composerBox?.width ? composerBox : null;
    onboardingMotion.current = true;
    setWelcomeStage("leaving");
    requestAnimationFrame(() => composer.current?.focus({ preventScroll: true }));
  }

  function suggestPrompt(kind: "compute" | "storage" | "github") {
    const prompts = {
      compute: "Help me plan one or more VPSs. If I have not specified a count, assume one and label that assumption. Give me a useful starter configuration in CPU, RAM, disk, and OS terms, with Canter's monthly compute estimate—not internal size labels. When I describe an outcome instead of choosing infrastructure, compare managed app hosting with VM control only if that choice matters. Ask only what changes the plan, and make that question easy to spot.",
      storage: "Help me create a storage bucket. Ask me what I want to store, then guide me through the requirements one step at a time.",
      github: "Help me connect GitHub and choose a repository to work on.",
    };
    editDraft(draft.trim() ? `${draft.trim()}\n\n${prompts[kind]}` : prompts[kind]);
    setAttachedContext(null);
    requestAnimationFrame(() => { composer.current?.focus(); composer.current?.setSelectionRange(composer.current.value.length, composer.current.value.length); });
  }

  function editDraft(value: string) {
    if (onboarding && value.trim()) enterWorkspace();
    setFallbackDraft(value);
    if (storageKey) { try { sessionStorage.setItem(storageKey, value); window.dispatchEvent(new Event("canter-draft")); } catch { /* Nonessential storage. */ } }
  }
  function selectModel(value: string) {
    if (!modelKey) return;
    setModelFallback({ key: modelKey, value });
    try { sessionStorage.setItem(modelKey, value); window.dispatchEvent(new Event("canter-model")); } catch { /* Keep the selection in memory when storage is unavailable. */ }
  }
  function selectModelOptions(model: string, options: OperatorModelOptions) {
    if (!optionsKey) return;
    const value = JSON.stringify({ ...modelPreferences, [model]: options });
    setOptionsFallback({ key: optionsKey, value });
    try { sessionStorage.setItem(optionsKey, value); window.dispatchEvent(new Event("canter-model")); } catch { /* Preserve settings in memory. */ }
  }
  async function send(event?: FormEvent, chosenRepository?: string) {
    if(archived||personaUnavailable){event?.preventDefault();return;}
    event?.preventDefault();
    const message = (chosenRepository ? `Deploy https://github.com/${chosenRepository}` : ((composer.current?.value ?? draft).trim() || (attachmentDraft.items.length ? "Please review the attached files." : "")));
    const attachments = chosenRepository ? [] : attachmentDraft.items;
    const requestSurface: OperatorSurface | null = chosenRepository ? { kind: "repository", repository: chosenRepository } : messageContext;
    const requestModel = managedModel ? {} : { model: selectedModel, modelOptions: selectedModelOptions };
    if (!workspace || !message || sendLock.current || (!data?.agent.available && !(peopleOnly))) return false;
    sendLock.current = true;
    setSending(true);setError("");
    let replyMessageId=replyTo?.messageId;
    if(replyRaw){
      const version=replyGeneration.current,anchor=parseOperatorReplyAnchor(replyRaw,id);
      try{
        if(!anchor)throw new CanterAPIError(404,"Invalid saved reply");
        const latest=await canterFetch<OperatorReplyTarget>(`/workspaces/${encodeURIComponent(workspace)}/wren-replies?conversationId=${encodeURIComponent(id!)}&messageId=${encodeURIComponent(anchor.messageId)}`);
        if(replyGeneration.current!==version){sendLock.current=false;setSending(false);return false;}
        if(!isReplyTargetForAnchor(latest,anchor))throw new CanterAPIError(404,"Invalid reply target");
        replyMessageId=latest.messageId;
        if(room&&latest.role==="assistant"&&!latest.canWake&&!peopleOnly){
          if(replyStorageKey)setReplyDraftState({key:replyStorageKey,raw:replyRaw,target:latest,status:"ready",notice:"This Wren is no longer available here. Your draft was not sent. You can post the reply to people only."});
          setRoomRecipient("people");sendLock.current=false;setSending(false);return false;
        }
      }catch(cause){
        sendLock.current=false;setSending(false);
        if(replyGeneration.current!==version)return false;
        if(cause instanceof CanterAPIError&&(cause.status===403||cause.status===404))setReplyTo(null,"The original reply message is no longer available. The reply was removed; your draft was not sent.");
        else if(replyStorageKey)setReplyDraftState(value=>({key:replyStorageKey,raw:replyRaw,target:value?.target??null,status:"error",notice:"Couldn’t verify this reply. Retry before sending; your draft was not sent."}));
        return false;
      }
    }
    const signature = JSON.stringify({ message, attachments, surface: requestSurface, replyToMessageId:replyMessageId, ...requestModel, recipientIds:roomRecipientIDs,postOnly:peopleOnly });
    if (onboarding) enterWorkspace();
    else composerOrigin.current = composerArea.current?.getBoundingClientRect() ?? null;
    setSubmittedPrompt(message);
    setSubmittedSurface(requestSurface);
    followScroll.current = true;
    if (!pending.current || pending.current.signature !== signature) pending.current = { id: id ?? `conv_${crypto.randomUUID()}`, requestId: crypto.randomUUID(), message, signature };
    const { id: requestConversationId, requestId } = pending.current;
    const request = { id: requestConversationId, requestId, message, attachments, ...requestModel };
    try {
      const base = conversationBase(workspace);
      await canterFetch(id ? `${base}/${encodeURIComponent(id)}/messages` : base, { method: "POST", body: JSON.stringify(id ? { replyToMessageId:replyMessageId, postOnly: peopleOnly, recipientIds: roomRecipientIDs, requestId: request.requestId, message, attachments, surface: requestSurface, ...requestModel } : { ...request, surface: requestSurface }) });
      if (chosenRepository) { setInlineGitHub(false); setDismissedGitHubSequence(events.at(-1)?.sequence ?? 0); }
      if (!chosenRepository) { setReplyTo(null); editDraft(""); attachmentDraft.update([]); }
      else if (!id) {
        const targetDraftKey = operatorDraftKey(data?.account.id, workspace, request.id);
        if (draft && targetDraftKey) { try { sessionStorage.setItem(targetDraftKey, draft); } catch { /* Nonessential storage. */ } }
        if (attachmentDraft.items.length && targetDraftKey) attachmentDraft.update(attachmentDraft.items, targetDraftKey);
      }
      pending.current = null;
      conversationCache.invalidate(requestConversationId);
      refreshWorkspace();
      if (!id) {
        if (!managedModel && selectedModel) { try { sessionStorage.setItem(`canter:conversation-model:${data.account.id}:${workspace}:${request.id}`, selectedModel); sessionStorage.setItem(`canter:conversation-model:${data.account.id}:${workspace}:${request.id}:options`, JSON.stringify(modelPreferences)); } catch { /* The persisted run also carries the model. */ } }
        const destination = `/app/conversations/${encodeURIComponent(request.id)}`;
        if (welcome) {
          // Keep this composer mounted as the first-run conversation becomes the
          // normal dashboard. The canonical URL still supports reload and sharing.
          try { setDetail(await conversationCache.load(request.id, true)); }
          catch { setDeliveryNotice("Message sent. Reconnecting for updates."); }
          setId(request.id);
          window.history.replaceState(null, "", destination);
        } else {
          router.prefetch(destination);
          router.push(destination);
        }
      }
      else {
        // The message is already accepted. A failed refresh must not invite a duplicate send.
        try { setDetail(await conversationCache.load(id, true)); }
        catch { setDeliveryNotice("Message sent. Reconnecting for updates."); setConnectionError("Updates are disconnected."); setAttempt(value => value + 1); }
      }
      return true;
    } catch (cause) { setSubmittedPrompt(""); setSubmittedSurface(null); setError(cause instanceof Error ? cause.message : "Your message could not be sent. It is saved here so you can retry."); requestAnimationFrame(() => composer.current?.focus()); return false; }
    finally { sendLock.current = false; setSending(false); }
  }
  async function stop() {
    if (!id || !workspace || !ownRun || !canStop || stopLock.current) return;
    stopLock.current = true; setStopping(true);
    try {
      await canterFetch(`${conversationBase(workspace)}/${encodeURIComponent(id)}/stop`, { method: "POST", body: JSON.stringify({ runId: ownRun.id }) });
      try { setDetail(await conversationCache.load(id, true)); }
      catch { setDeliveryNotice("Stop requested. Reconnecting for updates."); setConnectionError("Updates are disconnected."); setAttempt(value => value + 1); }
    } catch (cause) { setError(cause instanceof Error ? cause.message : "Could not stop this response."); }
    finally { stopLock.current = false; setStopping(false); }
  }
  async function loadEarlierMessages() {
    if (!id || !workspace || !detail?.nextCursor || loadingEarlier) return;
    const request = ++historyRequest.current;
    const requestedId = id;
    const requestedWorkspace = workspace;
    const cursor = detail.nextCursor;
    setHistoryLoading({ id: requestedId, workspace: requestedWorkspace, request });
    setHistoryProblem({ id: requestedId, workspace: requestedWorkspace, message: "" });
    followScroll.current = false;
    const element = transcript.current;
    const previousHeight = element?.scrollHeight ?? 0;
    try {
      const page = await conversationMessages(requestedWorkspace, requestedId, cursor);
      if (historyRequest.current !== request || historyRoute.current.id !== requestedId || historyRoute.current.workspace !== requestedWorkspace) return;
      setDetail(current => historyRoute.current.id === requestedId && historyRoute.current.workspace === requestedWorkspace
        ? mergeEarlierConversationPage(current, requestedId, { ...page, messages: [...page.messages].reverse() })
        : current);
      requestAnimationFrame(() => { if (element && historyRequest.current === request) element.scrollTop += element.scrollHeight - previousHeight; });
    } catch (cause) {
      if (historyRequest.current === request && historyRoute.current.id === requestedId && historyRoute.current.workspace === requestedWorkspace) setHistoryProblem({ id: requestedId, workspace: requestedWorkspace, message: cause instanceof Error ? cause.message : "Earlier messages could not be loaded." });
    } finally { if (historyRequest.current === request) setHistoryLoading(null); }
  }
  async function openRunReply(runId: string) {
    if (!id || !workspace || !latestDetail.current) return;
    const requestedId = id, requestedWorkspace = workspace;
    let history = latestDetail.current, pages = 0;
    const answer = () => history.messages.find(message => message.runId === runId && message.role === "assistant");
    while (!answer() && history.nextCursor && pages++ < 50) {
      const page = await conversationMessages(workspace, id, history.nextCursor);
      if (historyRoute.current.id !== requestedId || historyRoute.current.workspace !== requestedWorkspace) return;
      history = mergeEarlierConversationPage(history, id, { ...page, messages: [...page.messages].reverse() })!;
    }
    const message = answer();
    if (!message) throw new Error("This run has no reply yet. Its status and any failure are kept in Recent work.");
    followScroll.current = false;
    setDetail(current => current ? mergeConversationHistory(history, current) : history);
    setWorkOpen(false); setPanelOpen(false); closeConfiguration();
    requestAnimationFrame(() => requestAnimationFrame(() => {
      if (historyRoute.current.id !== requestedId || historyRoute.current.workspace !== requestedWorkspace) return;
      const target = [...transcript.current?.querySelectorAll<HTMLElement>("[data-message-id]") ?? []].find(node => node.dataset.messageId === message.id);
      target?.scrollIntoView({ block: "center" }); target?.focus({ preventScroll: true });
    }));
  }
  const openLinkedSurface = useEffectEvent((surface: OperatorSurface) => openSurface(surface));
  useEffect(()=>{
    if(!id||!workspace||!latestDetail.current||!wrenWorkspace)return;
    const key=`${id}:${jumpMessage??""}:${jumpWork??""}:${jumpWorkId??""}`;
    if(jumpHandled.current===key)return;
    if((jumpWork==="wren-artifact"||jumpWork==="wren-job")&&jumpWorkId || ["computer","wren-memory","wren-skills","wren-extensions","wren-responsibility","wren-projects","wren-library","wren-messaging","wren-members"].includes(jumpWork??"")){
      const surface:OperatorSurface={kind:jumpWork as OperatorSurface["kind"],...(jumpWorkId?{id:jumpWorkId}:{})};
      jumpHandled.current=key;openLinkedSurface(surface);return;
    }
    if(!jumpMessage)return;
    jumpHandled.current=key;followScroll.current=false;
    const controller=new AbortController();
    async function jump(){
      let history=latestDetail.current!;let pages=0;
      try{
        while(!history.messages.some(message=>message.id===jumpMessage)&&history.nextCursor&&pages++<50){
          const page=await conversationMessages(workspace!,id!,history.nextCursor,controller.signal);
          history=mergeEarlierConversationPage(history,id!,{...page,messages:[...page.messages].reverse()})!;
        }
        if(controller.signal.aborted)return;
        setDetail(current=>current?mergeConversationHistory(history,current):history);
        requestAnimationFrame(()=>requestAnimationFrame(()=>{if(controller.signal.aborted)return;const target=[...transcript.current?.querySelectorAll<HTMLElement>("[data-message-id]")??[]].find(node=>node.dataset.messageId===jumpMessage);if(target){target.scrollIntoView({block:"center"});target.focus({preventScroll:true});}else setDeliveryNotice("That message is not in the loaded history. Load earlier messages to continue looking.");}));
      }catch(cause){if(!controller.signal.aborted)setError(cause instanceof Error?cause.message:"Couldn’t open that message.");}
    }
    void jump();return()=>{controller.abort();if(jumpHandled.current===key)jumpHandled.current="";};
  },[id,workspace,jumpMessage,jumpWork,jumpWorkId,wrenWorkspace,detailReady]);
  function openSurface(requested: OperatorSurface, label?: string) {
    const surface: OperatorSurface = requested.kind === "computer" && wrenWorkspace ? {kind:"computer"} : requested;
    if (label) setSurfaceNames(current => ({ ...current, [surfaceKey(surface)]: label }));
    if (document.activeElement instanceof HTMLElement && !surfacePanel.current?.contains(document.activeElement)) {
      const active = document.activeElement;
      panelReturnFocus.current = active.closest('[role="menu"]')?.parentElement?.querySelector<HTMLElement>('button[aria-haspopup="menu"]') ?? active;
    }
    if (surface.kind === "wren-artifact" && workspace) notifyWorkUpdate(workspace, id);
    onCloseInspector?.();
    if (wrenWorkspace) {
      if (surface.kind === "wren-extensions") { setViewMenu(false); router.push(appsPageHref(id, { installation: surface.id, source: surface.catalogSlug })); return; }
      const placement = wrenSurfacePlacement(surface.kind);
      setViewMenu(false);
      if (placement === "dialog") {
        setConfigurationOpenRequest(value=>value+1);
        setConfiguration(surface); setConfigurations(current => current.some(item => surfaceKey(item) === surfaceKey(surface)) ? current : [...current,surface]);
        setConfigurationOpen(true); setPanelOpen(false); return;
      }
      if (placement === "work") {
        setWorkCollection(surface); setWorkCollections(current => current.some(item => surfaceKey(item) === surfaceKey(surface)) ? current : [...current,surface]);
        setWorkOpen(true); setPanelOpen(false); setConfigurationOpen(false);
        requestAnimationFrame(() => workCanvas.current?.querySelector<HTMLElement>('[data-collection-heading]')?.focus({preventScroll:true})); return;
      }
      setWorkOpen(false); setConfigurationOpen(false);
    }
    if (surface.kind === "compute" || surface.kind === "storage") { setPanelOpen(false); suggestPrompt(surface.kind); return; }
    if (surface.kind === "github") { setInlineGitHub(true); setPanelOpen(false); followScroll.current = true; requestAnimationFrame(() => transcript.current?.scrollTo({ top: transcript.current.scrollHeight })); return; }
    setOpened(current => current.some(item => surfaceKey(item) === surfaceKey(surface)) ? current : [...current, surface]);
    setSelected(surface); setPanelOpen(true); setViewMenu(false);
    requestAnimationFrame(() => surfacePanel.current?.querySelector<HTMLButtonElement>('[role="tab"][aria-selected="true"]')?.focus());
  }
  function closeSurface(surface: OperatorSurface) {
    const remaining = opened.filter(item => surfaceKey(item) !== surfaceKey(surface));
    setOpened(remaining);
    const next = selected && surfaceKey(selected) !== surfaceKey(surface) ? selected : remaining.at(-1) ?? null;
    setSelected(next);
    if (!remaining.length) { closePanel(); setWide(false); }
    if (remaining.length) requestAnimationFrame(() => surfacePanel.current?.querySelector<HTMLButtonElement>('[role="tab"][aria-selected="true"]')?.focus());
  }
  const tabLabel = (surface: OperatorSurface) => surfaceNames[surfaceKey(surface)] ?? surfaceResultName(surface, events) ?? surface.path?.split("/").at(-1) ?? (surface.kind === "repository" ? surface.repository?.split("/").at(-1) : undefined) ?? surface.system ?? surfaceLabels[surface.kind];
  const githubRequest = pendingRepositoryPicker(events);
  const githubRun = githubRequest && githubRequest.sequence > dismissedGitHubSequence ? githubRequest.runId : undefined;
  const welcomeText = introText || (data && !data.agent.available ? welcomeFallback : "");
  const introPending = !welcomeText || (!!welcomeGitHub?.connection.connected && !welcomeReady);
  const hasMessages = !!detail?.messages.length;
  const showConversationWelcome = welcomeStage === "done" && !wren && !id && !hasMessages && !inlineGitHub && !submittedPrompt;
  const github = workspace ? <GitHubRepositories inline workspaceId={workspace} conversationId={id} result={githubResult} busy={sending || !data?.agent.available} onDeploy={async repository => { await send(undefined, repository); }} /> : null;
  function focusPanel() {
    requestAnimationFrame(() => (surfacePanel.current?.querySelector<HTMLElement>('[role="tab"][aria-selected="true"]') ?? surfacePanel.current)?.focus());
  }
  function openPanel() {
    if(wrenWorkspace){if(showPanel&&!inspecting&&selected?.kind==="computer"){closePanel();return}openSurface({kind:"computer"});return}
    onCloseInspector?.();
    setSelected(current => current ?? opened.at(-1) ?? null);
    setPanelOpen(true);
    focusPanel();
  }
  function selectTab(index: number) {
    onCloseInspector?.();
    const surface = opened[index];
    if (!surface) return;
    setSelected(surface);
    setPanelOpen(true);
    focusPanel();
  }
  const workspaceCommands: SpotlightCommand[] = [
    { id: "right-panel", title: !showPanel ? "Open right sidebar" : wide || overlayPanel ? "Close right sidebar" : "Expand right sidebar fully", key: "e", icon: "panel", run: () => {
      if (!showPanel) { setWide(false); openPanel(); }
      else if (!wide && !overlayPanel) { setWide(true); focusPanel(); }
      else { closePanel(); setWide(false); requestAnimationFrame(() => panelTrigger.current?.focus()); }
    } },
    ...opened.map((surface, index) => ({ id: `tab-${surfaceKey(surface)}`, title: `Tab ${index + 1}: ${tabLabel(surface)}`, key: index < 9 ? String(index + 1) : undefined, icon: "folder" as const, run: () => selectTab(index) })),
    ...(opened.length > 1 ? ([
      { id: "previous-tab", title: "Previous workspace tab", key: "[", icon: "panel", run: () => selectTab((opened.findIndex(surface => selected && surfaceKey(surface) === surfaceKey(selected)) - 1 + opened.length) % opened.length) },
      { id: "next-tab", title: "Next workspace tab", key: "]", icon: "panel", run: () => selectTab((opened.findIndex(surface => selected && surfaceKey(surface) === surfaceKey(selected)) + 1) % opened.length) },
    ] satisfies SpotlightCommand[]) : []),
  ];

  return <AppShell pageAction={wrenWorkspace && workspace && id ? <WrenWorkStrip membersLabel={room ? roomPeopleCount===undefined?"Members":`${roomPeopleCount??0} ${(roomPeopleCount??0)===1?"person":"people"}${room.wrens.length?` · ${room.wrens.length} ${room.wrens.length===1?"Wren":"Wrens"}`:""}` : undefined} triggerRef={panelTrigger} workOpen={workOpen} onChat={()=>setWorkOpen(false)} onBrowse={()=>{setWorkOpen(true);setWorkCollection(null);setPanelOpen(false);}} onConfigure={onEditWren} computerOpen={showPanel&&!inspecting&&selected?.kind==="computer"} workspaceId={workspace} conversationId={id} events={events} onSelect={(surface,label)=>surface.kind==="computer"?openPanel():openSurface(surface,label)}/> : null} active={wren || room ? "Wrens" : "Home"} sidebarContent={sidebarContent} pageIcon={room ? <span className={styles.roomHeaderMark}>{room.kind === "channel" ? <span aria-hidden="true">#</span> : <WorkspaceIcon name="message" width="22" height="22"/>}</span> : wren ? <WrenAvatar shape={wren.shape} palette={wren.palette} status={running ? "running" : "idle"} size={26} /> : undefined} pageTitleAction={wren || room ? onEditWren : undefined} agentView onboarding={onboarding} onboardingTransition={welcomeStage === "leaving"} pageTitle={room ? room.name.replace(/^\s*#\s*/, "") : wren?.name ?? detail?.conversation.title} workspaceCommands={workspaceCommands} onNewInstruction={() => { if (wren || room) { window.dispatchEvent(new CustomEvent("canter-create-conversation",{detail:{kind:"wren"}})); return; } if (id) router.push("/app?compose=1"); else { setInlineGitHub(false); setPanelOpen(false); requestAnimationFrame(() => composer.current?.focus()); } }}>
    <div className={styles.workspace} data-wren={wrenWorkspace || undefined} data-inspecting={inspecting || undefined} data-onboarding={onboarding || undefined} data-welcome-stage={onboarding && welcomeText ? "welcome" : welcomeStage} data-has-surface={showPanel} data-context-panel={showPanel && ((!wrenWorkspace && inspecting) || !!selected && ["wren-projects", "wren-messaging", "wren-memory", "wren-skills", "wren-responsibility", "wren-members", "wren-extensions"].includes(selected.kind)) || undefined} data-working={running} data-wide={wide && showPanel} data-empty={(!id || !!wren || !!room) && !hasMessages && !inlineGitHub && !submittedPrompt}>
      {welcomeStage !== "done" ? <OnboardingPrism leaving={welcomeStage === "leaving"} reducedMotion={reducedMotion} /> : null}
      {onboarding ? <button type="button" className={styles.onboardingSkip} disabled={creatingFirstWren} onClick={enterWorkspace}>Skip</button> : null}
      <div className={styles.conversationPane} inert={(wide || overlayPanel) && showPanel}>
        {wrenWorkspace && workspace && id ? <div ref={workCanvas} className={styles.workCanvas} hidden={!workOpen}>
          {workCollection ? <div className={styles.collectionHeading} data-collection-heading tabIndex={-1}><button type="button" onClick={()=>{setWorkCollection(null);requestAnimationFrame(()=>workCanvas.current?.querySelector<HTMLElement>("h2")?.focus({preventScroll:true}));}}><WorkspaceIcon name="right" style={{transform:"rotate(180deg)"}} width="14" height="14"/>All work</button><span>{tabLabel(workCollection)}</span></div> : null}
          <div hidden={!!workCollection}><WrenWorkOverview workspaceId={workspace} conversationId={id} name={wren?.name ?? room?.name ?? "This conversation"} onSelect={openSurface}/></div>
          {workCollections.map(surface=><div className={styles.collectionContent} key={surfaceKey(surface)} hidden={!workCollection || surfaceKey(surface)!==surfaceKey(workCollection)}><OperatorSurfaceView onOpenRun={openRunReply} surface={surface} workspaceId={workspace} conversationId={id} onSelect={openSurface} githubResult={githubResult??searchParams.get("github")??undefined} onUseApp={prompt=>{if(room&&roomRecipient==="people"&&room.wrens[0])setRoomRecipient(room.wrens[0].id);editDraft(draft.trim()?`${draft}\n\n${prompt}`:prompt);closeConfiguration();setWorkOpen(false);requestAnimationFrame(()=>requestAnimationFrame(()=>{composer.current?.focus();composer.current?.setSelectionRange(composer.current.value.length,composer.current.value.length);}));}} busy={sending} onDeploy={async repository=>{await send(undefined,repository);}}/></div>)}
        </div> : null}
        <section hidden={wrenWorkspace && workOpen} className={styles.conversation} aria-label={room ? `${room.name} shared conversation` : wren ? `Conversation with ${wren.name}` : "Canter conversation"} data-scrolled={hasEarlierMessages}>
          <div className={styles.conversationWelcome} data-visible={showConversationWelcome} aria-hidden={!showConversationWelcome} inert={!showConversationWelcome}>
            <div className={styles.startBrand}><span className="wordmark">canter</span></div>
            <p>What can I help with?</p>
            <div className={styles.starters} aria-label="Try a workspace action"><button onClick={() => suggestPrompt("compute")}>Plan a VPS</button><button onClick={() => suggestPrompt("storage")}>Create a bucket</button><button onClick={() => suggestPrompt("github")}>Connect GitHub</button></div>
          </div>
          <div className={styles.transcript} ref={transcript} tabIndex={0} role="region" aria-label="Conversation messages" onWheel={event => { if (event.deltaY < 0) followScroll.current = false; }} onTouchMove={() => { followScroll.current = false; }} onKeyDown={event => { if (["ArrowUp", "PageUp", "Home"].includes(event.key)) followScroll.current = false; }} onScroll={() => { const element = transcript.current; if (element) { const gap = element.scrollHeight - element.scrollTop - element.clientHeight; setShowScroll(!followScroll.current && gap >= 80); setHasEarlierMessages(element.scrollTop > 1); if (gap <= 4) followScroll.current = true; } }}>
            <div ref={transcriptContent}>
            {!welcome && id && !detail && !connectionError && !submittedPrompt ? <WorkspaceLoading variant="conversation" /> : null}
            {wren && detail && !hasMessages && !submittedPrompt && !running && !connectionError ? <WrenConversationIntro wren={wren} onEdit={onEditWren} /> : null}
            {room && detail && !hasMessages && !submittedPrompt && !running ? <section className={styles.roomArrival}><span aria-hidden="true">{room.kind === "channel" ? "#" : <WorkspaceIcon name="message" width="28" height="28"/>}</span><h2>{room.name.replace(/^\s*#\s*/, "")}</h2><p>{room.purpose || "Start the conversation. Share an idea, ask a question, or bring something to work on together."}</p><small>{room.kind === "channel" ? "Shared with everyone in this workspace" : "Only invited people can see this conversation"}</small></section> : null}
            {detail?.hasMore ? <div className={styles.earlierMessages}><button type="button" onClick={() => void loadEarlierMessages()} disabled={loadingEarlier}>{loadingEarlier ? "Loading earlier messages…" : "Load earlier messages"}</button>{historyError ? <p className={styles.error} role="alert">{historyError}</p> : null}</div> : null}
            {welcomeStage !== "done" ? <div ref={intro} className={styles.onboardingIntro} data-stage={welcomeStage === "leaving" ? "leaving" : introPending ? "arrival" : "welcome"} aria-hidden={welcomeStage === "leaving" || undefined} style={welcomeStage === "leaving" && introPosition ? { position: "fixed", ...introPosition } : undefined}>
              <div className={styles.onboardingIdentity}><span className={`wordmark ${styles.onboardingWordmark}`}>canter</span>{introPending ? <span className={styles.onboardingThinking} role="status">Thinking</span> : null}</div>
              {welcomeText ? <div className={styles.onboardingGreeting}><ResponseText text={welcomeText} /></div> : null}
              <div className={styles.onboardingChoices}>
                <button type="button" className={`${styles.onboardingConnect} ${styles.onboardingCreate}`} disabled={creatingFirstWren||!onboarding} onClick={()=>void createFirstWren()}>{creatingFirstWren?"Opening Wrens…":"Create a Wren"}<WorkspaceIcon name="right" width="16" height="16"/></button>
                <p>An AI teammate with its own purpose. Work together privately or bring people and Wrens into shared conversations.</p>
                {onboardingActionError?<p role="alert">{onboardingActionError}</p>:null}
              </div>
              {welcomeText && welcomeGitHubChecked && !welcomeReady && !welcomeGitHub?.connection.connected ? <div className={styles.onboardingGitHub}>
                {welcomeGitHub?.connection.enabled && workspace ? <a className={styles.onboardingConnect} href={githubConnectURL(welcomeGitHub.connection, workspace, "/app?welcome=1")}><ProviderIcon provider="github" />{welcomeGitHub.connection.reconnect ? "Reconnect GitHub" : "Connect GitHub"}<WorkspaceIcon name="external" width="14" height="14" /></a> : <button className={styles.onboardingConnect} disabled><ProviderIcon provider="github" />Connect GitHub</button>}
                <p>Optional for coding. {welcomeGitHub?.connection.enabled ? welcomeGitHub.connection.appEnabled ? "Choose which repositories to share with Canter. Access is read-only." : "GitHub requests repository access, including write permission. Canter uses this connection to read source." : "GitHub connection isn’t available in this environment yet. You can skip and start with an idea."}</p>
                {githubResult && githubResult !== "connected" ? <p>You can try connecting again, or skip for now.</p> : null}
              </div> : null}
              {projectWelcome ? <div className={`${styles.onboardingGreeting} ${styles.onboardingProjects}`}><ResponseText text={projectWelcome} /></div> : null}
            </div> : null}
            {room && detail ? <RoomTranscript ownEmail={data?.account.email} onReply={!archived&&!personaUnavailable&&data?.workspace.role!=="viewer"?replyToMessage:undefined} detail={detail} events={events} onSelect={openSurface} conversations={data?.conversations ?? []} /> : wrenWorkspace && detail ? <WrenTranscript onReply={!archived&&!personaUnavailable&&data?.workspace.role!=="viewer"?replyToMessage:undefined} detail={detail} events={events} wren={wren} onSelect={openSurface} conversations={data?.conversations ?? []} inlineForRun={runID=><>{!inlineGitHub && githubRun===runID?github:null}</>} /> : detail?.messages.filter(message => message.role === "user").map(message => <OperatorTurn key={message.id} message={message} answer={detail.messages.find(answer => answer.role === "assistant" && answer.runId === message.runId)} events={events.filter(event => event.runId === message.runId)} running={running && message.runId === detail.run?.id} onSelect={openSurface} conversations={data?.conversations ?? []} inline={<>{!inlineGitHub && githubRun === message.runId ? github : null}</>} />)}
            {inlineGitHub ? github : null}
            {submittedPrompt && !hasMessages ? <section className={styles.turn}><article className={styles.message} data-role="user" data-arriving="true"><div className={styles.messageText}>{submittedPrompt}</div><OperatorMessageContext surface={submittedSurface} conversations={data?.conversations ?? []} /></article><div className={styles.activityLine} role="status"><MorphLabel text="Thinking" shimmer /></div></section> : null}
            {!room && detail?.run?.status === "failed" ? <p className={styles.error} role="alert">{detail.run.failure || "The response failed."} You can continue below.</p> : null}
            {!room && detail?.run?.status === "cancelled" ? <p className={styles.note}>Stopped. Completed operations remain saved.</p> : null}
            </div>
          </div>
          <div className={styles.composerArea} ref={composerArea}>
            {showScroll ? <button type="button" className={styles.scrollLatest} aria-label="Scroll to latest message" onClick={() => { followScroll.current = true; setShowScroll(false); transcript.current?.dispatchEvent(new Event("canter-follow-latest")); }}><WorkspaceIcon name="down" width="16" height="16" /></button> : null}
            {error ? <p className={styles.error} role="alert">{error}</p> : null}
            {connectionError ? <p className={styles.error} role="status">{deliveryNotice || connectionError} <button onClick={() => setAttempt(value => value + 1)}>Reconnect</button></p> : null}
            {!onboarding && data && !data.agent.available ? <p className={styles.error} role="alert">{managedModel ? "Wrens are temporarily unavailable. You can keep your message here and try again shortly." : "The workspace agent is unavailable. Ask your administrator to configure its model connection."}</p> : null}
            {attachmentDraft.error ? <p className={styles.error} role="status">{attachmentDraft.error}</p> : null}
            <div className={styles.onboardingComposerReveal} hidden={onboarding && !welcomeReady}>
            {room && room.wrens.length>0 ? <div className={styles.roomComposerContext}>{mentionedWrens.length?<div className={styles.roomMentionAudience} role="status"><span>{mentionedWrens.length>1?"Reply order":"Reply from mention"}</span><strong>{mentionedWrens.map(item=>item.name).join(" → ")}</strong></div>:<><span className={styles.roomParticipants}>{room.wrens.map(wren=><button key={wren.id} type="button" title={`Ask ${wren.name}`} onClick={()=>{editDraft(`${draft}${draft&&!/\s$/.test(draft)?" ":""}@${wren.name} `);composer.current?.focus();}}><WrenAvatar shape={wren.shape} palette={wren.palette} size={19}/><span>{wren.name}</span></button>)}</span><label><CanterSelect ariaLabel="Message audience" className="canter-select-compact" value={roomRecipient} onChange={setRoomRecipient} options={[...room.wrens.map(wren=>({value:wren.id,label:`Ask ${wren.name}`})),...(room.wrens.length>1?[{value:"team",label:"Ask all Wrens"}]:[]),{value:"people",label:"Message the room"}]}/></label></>}</div> : null}
            {personaUnavailable&&!archived?<p className={styles.note} role="status">This Wren is unavailable. Your saved conversation and files remain here.</p>:null}
            {archived?<p className={styles.note} role="status">Archived. History and files remain available; restore this conversation in its settings before starting new work.</p>:null}
            {data?.workspace.role==="viewer"&&!archived?<p className={styles.note} role="status">You have read-only access to this organization.</p>:null}
            {replyTo?<div className={styles.replyContext} role="region" aria-label="Reply preview"><span className={styles.replyContextText}><span className={styles.replyContextSender}>{displayName(replyTo.senderName)}:</span> <span>{replyTo.excerpt.replace(/\s+/g," ")}</span></span><button type="button" aria-label="Cancel reply" onClick={()=>{setReplyTo(null);composer.current?.focus();}}><svg viewBox="0 0 16 16" width="16" height="16" aria-hidden="true"><path d="m5 5 6 6m0-6-6 6" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round"/></svg></button></div>:null}
            {replyChecking||replyNotice?<p className={styles.draftHint} role="status">{replyNotice||"Checking reply…"}{currentReplyState?.status==="error"?<button type="button" onClick={()=>setReplyAttempt(value=>value+1)}>Retry reply</button>:null}{replyChecking&&!replyTo?<button type="button" onClick={()=>setReplyTo(null,"Reply removed. Your draft is unchanged.")}>Remove reply</button>:null}</p>:null}
            <OperatorComposer wrens={room?.wrens ?? (wren ? [wren] : [])} sharedConversation={!!room} wrenApps={wrenWorkspace} conversationId={id} replying={!!replyTo} managedModel={managedModel} recipient={room ? `${room.kind==="channel"?"#":""}${room.name}` : wren?.name} attachments={attachmentDraft.items} onAttachments={attachmentDraft.update} sending={sending} context={messageContext} onClearContext={() => setAttachedContext(null)} draft={draft} onChange={editDraft} onSend={() => void send()} onStop={() => void stop()} stopping={stopping} running={canStop} stopLabel={room ? ownRun?.status === "queued" ? "Cancel my queued request" : "Stop my request" : "Stop response"} disabled={archived || personaUnavailable || replyChecking || data?.workspace.role === "viewer" || sending || (!data?.agent.available && !(peopleOnly)) || !attachmentDraft.loaded} inputRef={composer} model={selectedModel} onModelChange={selectModel} modelOptions={modelPreferences} onModelOptionsChange={selectModelOptions} onSelect={surface => { setAttachedContext(surface); if (surface.kind === "github") openSurface(surface); }} />
            </div>

          </div>
        </section>
      </div>
      <aside ref={surfacePanel} tabIndex={-1} role={overlayPanel && showPanel ? "dialog" : undefined} aria-modal={overlayPanel && showPanel || undefined} id={panelId} className={styles.surface} data-workspace-surface data-empty={!selected && !inspecting} aria-label={inspecting ? room ? "Room settings" : "Wren settings" : selected ? `${surfaceLabels[selected.kind]} view` : "Workspace view"} aria-hidden={!showPanel} inert={!showPanel} onKeyDown={event => {
        if (event.key === "Escape" && !event.defaultPrevented && !overlayPanel && !(event.target instanceof Element && event.target.closest("dialog[open]"))) { event.preventDefault(); closePanel(); }
      }}>
        <div className={styles.surfaceToolbar} hidden={wrenWorkspace&&selected?.kind==="computer"} data-overlay={overlayPanel||undefined}>
          <button type="button" className={styles.panelClose} hidden={wrenWorkspace} title="Close workspace panel" aria-label="Close workspace panel" onClick={closePanel}><WorkspaceIcon name="close" width="18" height="18" /></button>
          {!wrenWorkspace && inspecting ? <span className={styles.inspectorTitle}>{room ? "Room settings" : "Wren settings"}</span> : <div className={styles.surfaceTabs} role="tablist" aria-label="Workspace tabs">{opened.map((surface, index) => <div className={styles.surfaceTab} key={surfaceKey(surface)} data-active={!!selected && surfaceKey(selected) === surfaceKey(surface)}>
            <button type="button" role="tab" id={`${panelId}-tab-${index}`} aria-controls={`${panelId}-view-${index}`} aria-selected={!!selected && surfaceKey(selected) === surfaceKey(surface)} tabIndex={selected && surfaceKey(selected) === surfaceKey(surface) ? 0 : -1} title={`${surface.path ?? surface.repository ?? tabLabel(surface)}${index < 9 ? ` · ${shortcutLabel(String(index + 1))}` : ""}`} onClick={() => setSelected(surface)} onKeyDown={event => { if (["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) { event.preventDefault(); const next = event.key === "Home" ? 0 : event.key === "End" ? opened.length - 1 : (index + (event.key === "ArrowRight" ? 1 : -1) + opened.length) % opened.length; setSelected(opened[next]); document.getElementById(`${panelId}-tab-${next}`)?.focus(); } if (event.key === "Delete") { event.preventDefault(); closeSurface(surface); } }}><WorkspaceIcon name={surface.kind === "repository-changes" ? "changes" : surface.kind === "file" ? "file" : "folder"} width="14" height="14" /><span>{tabLabel(surface)}</span></button>
            <button type="button" className={styles.closeTab} aria-label={`Close ${tabLabel(surface)} tab`} onClick={() => closeSurface(surface)}><WorkspaceIcon name="close" width="12" height="12" /></button>
          </div>)}</div>}
          <div className={styles.surfaceTools}>
            {wrenWorkspace ? <button type="button" className={`${styles.surfaceTool} ${styles.mobilePanelReturn}`} aria-label="Back to conversation" title="Back to conversation" onClick={closePanel}><WorkspaceIcon name="right" style={{transform:"rotate(180deg)"}} width="16" height="16" /><span>Chat</span></button> : null}
            <div ref={viewMenuAnchor} className={styles.menuAnchor}><button type="button" className={styles.surfaceTool} aria-label="Open workspace view" aria-haspopup="menu" aria-expanded={viewMenu} onClick={() => setViewMenu(!viewMenu)}><WorkspaceIcon name="plus" width="17" height="17" /></button><MotionPresence open={viewMenu}><div className={styles.viewMenu} role="menu" aria-label="Workspace views" onKeyDown={moveMenuFocus}>{(wrenWorkspace ? ["computer", "wren-responsibility", "wren-projects", "wren-memory", "wren-skills", "wren-extensions", "wren-members", "wren-library"] as const : ["compute", "storage", "github", "apps", "deployments", "activity"] as const).map(kind => <button type="button" role="menuitem" tabIndex={-1} key={kind} onClick={() => { openSurface({kind}); setViewMenu(false); }}>{kind === "github" ? "GitHub repositories" : surfaceLabels[kind]}</button>)}{wrenWorkspace && onEditWren ? <button type="button" role="menuitem" tabIndex={-1} onClick={() => { setViewMenu(false); onEditWren(); }}>{room ? "Room settings" : "Wren settings"}</button> : null}</div></MotionPresence></div>
            <button type="button" className={styles.surfaceTool} hidden={overlayPanel} aria-label={wide ? "Restore split view" : "Expand workspace view"} aria-pressed={wide} onClick={() => setWide(!wide)}><WorkspaceIcon name={wide ? "collapse" : "expand"} width="17" height="17" /></button>
          </div>
        </div>
        <div className={styles.surfaceContent}>
          {!wrenWorkspace && inspector ? <div className={styles.inspectorContent} hidden={!inspecting}><WrenWorkNavigation.Provider value={openSurface}>{inspector}</WrenWorkNavigation.Provider></div> : null}
          {opened.length && workspace ? opened.map((surface, index) => <div key={surfaceKey(surface)} role="tabpanel" id={`${panelId}-view-${index}`} aria-labelledby={`${panelId}-tab-${index}`} hidden={(!wrenWorkspace && inspecting) || !selected || surfaceKey(selected) !== surfaceKey(surface)}>{wrenWorkspace && surface.kind === "wren-artifact" || (!inspecting || wrenWorkspace) && showPanel && selected && surfaceKey(selected) === surfaceKey(surface) ? <OperatorSurfaceView onOpenRun={openRunReply} computerChrome={wrenWorkspace?{close:closePanel,expand:()=>setWide(!wide),wide}:undefined} surface={surface} workspaceId={workspace} onSelect={openSurface} conversationId={id} githubResult={githubResult} busy={sending || !data?.agent.available} onDeploy={async repository => { await send(undefined, repository); }} /> : null}</div>) : <div className={styles.surfacePlaceholder} hidden={inspecting}>
            <div className={styles.surfaceHints}>
              <div><WorkspaceIcon name="apps" /><p>Apps<span>Apps your agent opens</span></p></div>
              <div><WorkspaceIcon name="file" /><p>Files<span>Code and files it works with</span></p></div>
              <div><WorkspaceIcon name="check" /><p>Changes<span>Proposals ready for your review</span></p></div>
              <div><WorkspaceIcon name="activity" /><p>Activity<span>Actions and results as it works</span></p></div>
            </div>
          </div>}
        </div>
      </aside>
      {wrenWorkspace && workspace ? <WrenContextDialog open={inspecting || configurationOpen} title={!inspecting&&configuration ? `${wren?.name ?? room?.name ?? "Conversation"} · ${surfaceLabels[configuration.kind]}` : `${wren?.name ?? room?.name ?? "Conversation"} · Settings`} onClose={closeConfiguration}>
        <nav hidden={!inspecting&&configuration?.kind==="wren-memory"&&!!configuration.id} className={styles.configurationNav} aria-label="Configuration sections">{onEditWren ? <button type="button" aria-pressed={inspecting} onClick={()=>{setConfigurationOpen(false);if(!inspecting)onEditWren();}}>Purpose</button> : null}{([['wren-memory','Memory'],['wren-skills','Skills'],['wren-members','People']] as const).map(([kind,label])=><button type="button" key={kind} aria-pressed={!inspecting && configuration?.kind===kind} onClick={()=>openSurface({kind})}>{label}</button>)}</nav>
        <WrenWorkNavigation.Provider value={openSurface}><div hidden={!inspecting}>{inspector}</div></WrenWorkNavigation.Provider>
        {configurations.map(surface=><div className={styles.configurationBody} key={surfaceKey(surface)} hidden={inspecting || !configuration || surfaceKey(configuration)!==surfaceKey(surface)}><OperatorSurfaceView onOpenRun={openRunReply} openRequest={configuration&&surfaceKey(configuration)===surfaceKey(surface)?configurationOpenRequest:0} surface={surface} workspaceId={workspace} conversationId={id} onSelect={openSurface} githubResult={githubResult??searchParams.get("github")??undefined} onUseApp={prompt=>{if(room&&roomRecipient==="people"&&room.wrens[0])setRoomRecipient(room.wrens[0].id);editDraft(draft.trim()?`${draft}\n\n${prompt}`:prompt);closeConfiguration();setWorkOpen(false);requestAnimationFrame(()=>requestAnimationFrame(()=>{composer.current?.focus();composer.current?.setSelectionRange(composer.current.value.length,composer.current.value.length);}));}} busy={sending} onDeploy={async repository=>{await send(undefined,repository);}}/></div>)}
      </WrenContextDialog> : null}
      <button ref={wrenWorkspace ? undefined : panelTrigger} type="button" className={styles.panelToggle} hidden={wrenWorkspace || onboarding || (!wrenWorkspace&&showPanel) || (overlayPanel&&showPanel)} title={wrenWorkspace?"Toggle computer · A + E":"Open workspace panel · A + E"} aria-label={wrenWorkspace?(showPanel&&selected?.kind==="computer"?"Hide computer":"Show computer"):"Show workspace panel"} aria-expanded={showPanel&&(!wrenWorkspace||selected?.kind==="computer")} aria-controls={panelId} onClick={openPanel}><WorkspaceIcon name="panel" style={{transform:"scaleX(-1)"}}/></button>
    </div>
  </AppShell>;
}
