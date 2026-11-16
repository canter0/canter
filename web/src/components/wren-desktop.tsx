"use client";
/* eslint-disable @next/next/no-img-element -- Native desktop frame geometry. */
import {
  useEffect,
  useRef,
  useState,
  type KeyboardEvent,
  type PointerEvent,
} from "react";
import {
  desktopPoint,
  fetchWrenBlob,
  workError,
  type WrenDesktopInput,
} from "@/lib/wren-work";
import { readScreenFrames } from "@/lib/wren-screen-stream";
import { CanterAPIError } from "@/lib/canter-api";
import { desktopInputQueue, desktopKey } from "@/lib/wren-desktop-input";
import styles from "./wren-desktop.module.css";

export function WrenDesktop({
  endpoint,
  name,
  controlling,
  onInput,
  revision,
  notice,
}: {
  endpoint: string;
  name: string;
  controlling: boolean;
  onInput: (input: WrenDesktopInput) => Promise<boolean>;
  revision: number;
  notice?: string;
}) {
  const [frame, setFrame] = useState<{ url: string; at: number; width:number; height:number } | null>(null),
    [error, setError] = useState(""),
    [pending, setPending] = useState(0),
    [focused, setFocused] = useState(false),
    [retry, setRetry] = useState(0);
  const image = useRef<HTMLImageElement>(null),
    surface = useRef<HTMLDivElement>(null),
    send = useRef(onInput),
    control = useRef(controlling),
    screenReady = useRef(false),
    refresh = useRef(() => {});
  const [queue] = useState(() =>
    desktopInputQueue(async () => false, setPending),
  );
  const clickTimer = useRef<ReturnType<typeof setTimeout> | null>(null),
    clickPending = useRef<WrenDesktopInput | null>(null);
  const pointer = useRef<{
      x: number;
      y: number;
      button: number;
      id: number;
    } | null>(null),
    wheel = useRef(0),
    wheelTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(() => {
    queue.setSender(async (input) => {
      if (!control.current || !screenReady.current) return false;
      const ok = await send.current(input);
      refresh.current();
      return ok;
    });
    send.current = onInput;
    control.current = controlling;
    if (!controlling) {
      queue.clear();
      pointer.current = null;
      if (clickTimer.current) clearTimeout(clickTimer.current);
      clickPending.current = null;
    }
  }, [onInput, controlling, queue]);
  useEffect(
    () => () => {
      queue.clear();
      if (clickTimer.current) clearTimeout(clickTimer.current);
      if (wheelTimer.current) clearTimeout(wheelTimer.current);
    },
    [queue],
  );
  useEffect(() => {
    if (!frame) return;
    return () => URL.revokeObjectURL(frame.url);
  }, [frame]);
  useEffect(() => {
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout> | undefined;
    let request: AbortController | undefined;
    let pending = false, failures = 0, again = false, denied = false, legacy = false;
    function pauseInput() {
      screenReady.current = false;
      queue.clear();
      pointer.current = null;
      if (clickTimer.current) clearTimeout(clickTimer.current);
      clickPending.current = null;
      if (wheelTimer.current) clearTimeout(wheelTimer.current);
      wheel.current = 0;
    }
    pauseInput();
    const schedule = (delay: number) => { clearTimeout(timer); timer = setTimeout(load, delay); };
    async function show(blob: Blob, signal: AbortSignal) {
      const url = URL.createObjectURL(blob);
      try {
        const decoded = new Image(); decoded.src = url; await decoded.decode();
        if (signal.aborted) { URL.revokeObjectURL(url); return; }
        setFrame({url, at:Date.now(), width:decoded.naturalWidth, height:decoded.naturalHeight});
        screenReady.current = true;
        setError(""); failures = 0;
      } catch (cause) { URL.revokeObjectURL(url); throw cause; }
    }
    async function load() {
      if (denied || controller.signal.aborted || document.visibilityState !== "visible") return;
      if (pending) { if (legacy) again = true; return; }
      pending = true; request = new AbortController();
      const signal = AbortSignal.any([controller.signal, request.signal]);
      let watchdog: ReturnType<typeof setTimeout> | undefined;
      const arm = (delay = 8000) => { clearTimeout(watchdog); watchdog = setTimeout(() => request?.abort(new Error("Screen connection interrupted.")), delay); };
      let normal = false;
      try {
        arm();
        if (!legacy) {
          const response = await fetch(`/api/canter${endpoint}/screen`, {credentials:"include",cache:"no-store",signal});
          if (response.status === 501) { legacy = true; again = true; return; }
          if (!response.ok) throw new CanterAPIError(response.status, "Screen connection interrupted.");
          if (!response.body || response.headers.get("content-type") !== "application/x-wren-screen") throw new Error("Invalid screen connection.");
          await readScreenFrames(response.body, async bytes => {
            await show(new Blob([new Uint8Array(bytes)], {type:"image/jpeg"}), signal);
            arm(3000);
          });
          normal = true;
        } else {
          const blob = await fetchWrenBlob(`${endpoint}/screenshot`, signal);
          if (blob.type !== "image/png") throw new Error("The screen could not be read.");
          await show(blob, signal);
        }
      } catch (cause) {
        if (!controller.signal.aborted && document.visibilityState === "visible") {
          failures++;
          if (cause instanceof CanterAPIError && [401,403,404].includes(cause.status)) { denied = true; setFrame(null); }
          pauseInput();
          setError(workError(cause,"Screen connection interrupted. Reconnecting…"));
        }
      } finally {
        clearTimeout(watchdog); pending = false; request = undefined;
        if (!controller.signal.aborted && !denied) {
          schedule(again || normal ? 0 : failures ? Math.min(8000,1000 * 2 ** Math.min(failures,3)) : control.current ? 350 : 1200);
          again = false;
        }
      }
    }
    refresh.current = () => { if (!legacy && pending) return; clearTimeout(timer); void load(); };
    const visible = () => {
      if (document.visibilityState === "visible") { again = true; refresh.current(); }
      else { clearTimeout(timer); request?.abort(); pauseInput(); }
    };
    document.addEventListener("visibilitychange",visible); window.addEventListener("online",visible);
    void load();
    return () => {
      controller.abort(); clearTimeout(timer); pauseInput(); refresh.current = () => {};
      document.removeEventListener("visibilitychange",visible); window.removeEventListener("online",visible);
    };
  }, [endpoint, revision, retry, queue]);
  const interactive = controlling && !!frame && !error;
  function point(event: { clientX: number; clientY: number }) {
    const img = image.current;
    if (!img) return null;
    const rect = img.getBoundingClientRect();
    return desktopPoint(
      {
        ...rect,
        left: rect.left,
        top: rect.top,
        width: rect.width,
        height: rect.height,
        naturalWidth: img.naturalWidth,
        naturalHeight: img.naturalHeight,
      },
      event.clientX,
      event.clientY,
    );
  }
  function enqueue(input: WrenDesktopInput) {
    if (!interactive || !control.current || !screenReady.current) return;
    if (!queue.push(input))
      setError("Input is catching up. Wait for the screen, then try again.");
  }
  function down(event: PointerEvent<HTMLDivElement>) {
    if (!interactive) return;
    event.preventDefault();
    surface.current?.focus();
    const p = point(event);
    if (!p) return;
    pointer.current = {
      ...p,
      button: event.button === 2 ? 3 : 1,
      id: event.pointerId,
    };
    event.currentTarget.setPointerCapture(event.pointerId);
  }
  function flushClick() {
    if (clickTimer.current) clearTimeout(clickTimer.current);
    clickTimer.current = null;
    const click = clickPending.current;
    clickPending.current = null;
    if (click) enqueue(click);
  }
  function up(event: PointerEvent<HTMLDivElement>) {
    const start = pointer.current;
    pointer.current = null;
    if (!start || !interactive) return;
    const end = point(event);
    if (!end) return;
    const distance = Math.hypot(end.x - start.x, end.y - start.y);
    if (distance > 5) {
      flushClick();
      enqueue({
        kind: "drag",
        x: start.x,
        y: start.y,
        x2: end.x,
        y2: end.y,
        button: start.button,
      });
      return;
    }
    const previous = clickPending.current;
    if (
      previous &&
      previous.button === 1 &&
      start.button === 1 &&
      Math.hypot(end.x - (previous.x ?? 0), end.y - (previous.y ?? 0)) < 6
    ) {
      if (clickTimer.current) clearTimeout(clickTimer.current);
      clickPending.current = null;
      enqueue({ kind: "double_click", ...end });
      return;
    }
    flushClick();
    clickPending.current = { kind: "click", ...end, button: start.button };
    clickTimer.current = setTimeout(flushClick, 220);
  }
  function key(event: KeyboardEvent<HTMLDivElement>) {
    if (!interactive) return;
    if (event.key === "Escape" && event.ctrlKey && event.altKey) {
      event.preventDefault();
      surface.current?.blur();
      return;
    }
    if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "v")
      return;
    const input = desktopKey({
      ...event,
      isComposing: event.nativeEvent.isComposing,
    });
    if (input) {
      flushClick();
      event.preventDefault();
      event.stopPropagation();
      enqueue(input);
    }
  }
  // A non-passive native wheel listener consumes scrolling only while the
  // person explicitly controls and focuses the remote desktop.
  useEffect(() => {
    const node = surface.current;
    if (!node) return;
    const handle = (event: WheelEvent) => {
      if (!interactive || document.activeElement !== node) return;
      event.preventDefault();
      wheel.current += event.deltaY * (event.deltaMode === 1 ? 19 : 1);
      if (wheelTimer.current) return;
      wheelTimer.current = setTimeout(() => {
        wheelTimer.current = null;
        const amount = Math.max(
          -12,
          Math.min(12, Math.round(wheel.current / 50)),
        );
        wheel.current = 0;
        if (amount && control.current) queue.push({ kind: "scroll", amount });
      }, 80);
    };
    node.addEventListener("wheel", handle, { passive: false });
    return () => node.removeEventListener("wheel", handle);
  }, [interactive, queue]);
  return (
    <div className={styles.desktop} style={frame ? {maxWidth:`min(100%, max(240px, calc((100dvh - 270px) * ${frame.width/frame.height})))`} : undefined}>
      <div
        ref={surface}
        className={styles.screen}
        role="application"
        aria-label={`${name} computer desktop`}
        aria-describedby="desktop-help"
        tabIndex={interactive ? 0 : -1}
        data-controlling={interactive}
        data-stale={!!error}
        data-frame-received-at={frame?.at}
        onFocus={() => setFocused(true)}
        onBlur={() => {
          setFocused(false);
          pointer.current = null;
        }}
        onPointerDown={down}
        onPointerUp={up}
        onPointerCancel={() => {
          pointer.current = null;
        }}
        onContextMenu={(event) => {
          if (interactive) event.preventDefault();
        }}
        onDoubleClick={(event) => {
          event.preventDefault();
        }}
        onKeyDown={key}
        onCompositionEnd={(event) =>
          enqueue({ kind: "type", text: event.data })
        }
        onPaste={(event) => {
          if (interactive) {
            event.preventDefault();
            flushClick();
            enqueue({
              kind: "type",
              text: event.clipboardData.getData("text").slice(0, 8000),
            });
          }
        }}
      >
        {frame ? (
          <img
            ref={image}
            src={frame.url}
            alt={`${name}’s computer screen`}
            draggable={false}
          />
        ) : (
          <div className={styles.connecting} role="status">
            <span />
            Connecting to the screen…
          </div>
        )}
        {error ? (
          <div className={styles.reconnecting} role="status">
            <details className={styles.errorDetails}>
              <summary>Connection interrupted · {frame ? "last received screen" : "reconnecting"}</summary>
              <p>{error}</p>
            </details>
            <button
              type="button"
              onPointerDown={(event) => event.stopPropagation()}
              onClick={() => {
                setRetry((value) => value + 1);
              }}
            >
              Reconnect
            </button>
          </div>
        ) : notice ? <div className={styles.inputNotice} role="alert">{notice}</div> : null}
      </div>
      <div className={styles.caption} id="desktop-help">
        <span>
          {error
            ? "Input paused until the screen reconnects."
            : interactive
              ? focused
                ? "Click, type, or scroll · Ctrl + Alt + Esc releases keyboard"
                : "Click the screen to use the computer"
              : "You’re watching the screen"}
        </span>
        <span role="status">{pending ? "Sending input…" : ""}</span>
      </div>
    </div>
  );
}
