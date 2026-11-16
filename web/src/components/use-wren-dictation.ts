"use client";
import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
} from "react";
import { canterFetch, CanterAPIError } from "@/lib/canter-api";
import {
  dictationWav,
  monoAudioChunk,
  microphoneLevel,
  maxDictationBytes,
  maxDictationSeconds,
  type DictationReadiness,
  type DictationResult,
} from "@/lib/wren-dictation";

type Phase = "idle" | "requesting" | "recording" | "transcribing";
type State = {
  scope: string;
  phase: Phase;
  seconds: number;
  enabled: boolean;
  checked: boolean;
  reason: string;
  error: string;
  notice: string;
  pendingTranscript: string;
  levels: number[];
  recoveryURL: string;
};
const empty: State = {
  scope: "",
  phase: "idle",
  seconds: 0,
  enabled: false,
  checked: false,
  reason: "Checking dictation…",
  error: "",
  notice: "",
  pendingTranscript: "",
  levels: [0, 0, 0, 0, 0, 0],
  recoveryURL: "",
};
type Capture = {
  stream?: MediaStream;
  context: AudioContext;
  source?: MediaStreamAudioSourceNode;
  analyser?: AnalyserNode;
  animation?: number;
  processor?: ScriptProcessorNode;
  sink?: GainNode;
  chunks: Float32Array[];
  recording: Float32Array[];
  queued: number;
  samples: number;
  segmentSamples?: number;
  lastVoice?: number;
  speech?: boolean;
  uploading?: Promise<void>;
  inserted?: boolean;
  failed?: boolean;
  timer?: ReturnType<typeof setInterval>;
  deadline?: ReturnType<typeof setTimeout>;
};
function disposeCapture(capture: Capture | null) {
  if (!capture) return;
  if (capture.animation) cancelAnimationFrame(capture.animation);
  capture.analyser?.disconnect();
  clearInterval(capture.timer);
  clearTimeout(capture.deadline);
  if (capture.processor) {
    capture.processor.onaudioprocess = null;
    capture.processor.disconnect();
  }
  capture.source?.disconnect();
  capture.sink?.disconnect();
  for (const track of capture.stream?.getTracks() ?? []) {
    track.onended = null;
    track.stop();
  }
  void capture.context.close().catch(() => {});
}
function dictationError(cause: unknown): string {
  if (cause instanceof DOMException && cause.name === "NotAllowedError")
    return "Microphone access was denied. Allow it in your browser, then try again.";
  if (cause instanceof DOMException && cause.name === "NotFoundError")
    return "No microphone was found. Connect one and try again.";
  return cause instanceof Error
    ? cause.message
    : "Dictation could not be completed. Your draft is unchanged.";
}
export function useWrenDictation({
  workspaceId,
  conversationId,
  accountId,
  disabled,
  onTranscript,
}: {
  workspaceId?: string;
  conversationId?: string;
  accountId?: string;
  disabled: boolean;
  onTranscript: (text: string) => boolean;
}) {
  const scope =
    workspaceId && conversationId && accountId
      ? `${accountId}:${workspaceId}:${conversationId}`
      : "";
  const endpoint = scope
    ? `/workspaces/${encodeURIComponent(workspaceId!)}/conversations/${encodeURIComponent(conversationId!)}/dictation`
    : "";
  const [stored, setStored] = useState<State>(empty),
    [retry, setRetry] = useState(0);
  const state = stored.scope === scope ? stored : empty;
  const capture = useRef<Capture | null>(null),
    request = useRef<AbortController | null>(null),
    recoveryURL = useRef(""),
    generation = useRef(0),
    acceptTranscript = useRef(onTranscript);
  const limits = useRef({
    seconds: maxDictationSeconds,
    bytes: maxDictationBytes,
    incremental: false,
  });
  useLayoutEffect(() => {
    acceptTranscript.current = onTranscript;
  }, [onTranscript]);
  const release = useCallback(() => {
    request.current?.abort();
    request.current = null;
    const current = capture.current;
    capture.current = null;
    disposeCapture(current);
    if (current) current.chunks = [];
  }, []);
  const invalidate = useCallback(() => {
    generation.current++;
    release();
    if (recoveryURL.current) URL.revokeObjectURL(recoveryURL.current);
    recoveryURL.current = "";
  }, [release]);
  const cancel = useCallback(() => {
    invalidate();
    setStored((value) => ({
      ...value,
      phase: "idle",
      seconds: 0,
      error: "",
      notice: "Recording stopped. Text already added stays in your draft.",
      pendingTranscript: "",
      recoveryURL: "",
    }));
  }, [invalidate]);
  // Fence late permission/provider results in the commit that changes audience,
  // before passive effects or a newly focused conversation can receive them.
  useLayoutEffect(() => () => invalidate(), [scope, disabled, invalidate]);
  useEffect(() => {
    const controller = new AbortController(),
      token = ++generation.current;
    release();
    async function load() {
      const supported =
        typeof window !== "undefined" &&
        !!navigator.mediaDevices?.getUserMedia &&
        typeof AudioContext !== "undefined";
      if (!endpoint || disabled || !supported) {
        await Promise.resolve();
        if (!controller.signal.aborted && generation.current === token)
          setStored({
            ...empty,
            scope,
            checked: true,
            reason: !endpoint
              ? "Open a conversation to dictate a message."
              : disabled
                ? "Dictation is unavailable while this composer is disabled."
                : "Microphone recording is not supported in this browser.",
          });
        return;
      }
      try {
        const value = await canterFetch<DictationReadiness>(endpoint, {
          signal: controller.signal,
        });
        if (controller.signal.aborted || generation.current !== token) return;
        const enabled =
          value.enabled &&
          value.encoding === "pcm_s16le" &&
          value.formats.includes("audio/wav") &&
          value.maxDurationSeconds > 0 &&
          value.maxBytes >= 44;
        limits.current = {
          seconds: Math.min(maxDictationSeconds, value.maxDurationSeconds),
          bytes: Math.min(maxDictationBytes, value.maxBytes),
          incremental: value.incremental === true,
        };
        setStored({
          ...empty,
          scope,
          enabled,
          checked: true,
          reason: enabled
            ? "Dictate a message · up to 60 seconds"
            : "Dictation is not available in this workspace.",
        });
      } catch (cause) {
        if (!controller.signal.aborted && generation.current === token) {
          // A newer web client can meet an older API during a rolling release.
          const unsupported =
            cause instanceof CanterAPIError && cause.status === 404;
          setStored({
            ...empty,
            scope,
            checked: true,
            reason: unsupported
              ? "Dictation is not available in this workspace."
              : "Couldn’t check dictation availability.",
            error: unsupported ? "" : dictationError(cause),
          });
        }
      }
    }
    void load();
    return () => {
      controller.abort();
      invalidate();
    };
  }, [scope, endpoint, disabled, retry, release, invalidate]);
  useEffect(() => {
    window.addEventListener("pagehide", cancel);
    return () => window.removeEventListener("pagehide", cancel);
  }, [cancel]);

  function transcribe(
    current: Capture,
    chunks: Float32Array[],
    token: number,
    incremental: boolean,
  ) {
    const upload = async () => {
      if (generation.current !== token || current.failed) {
        chunks.length = 0; current.queued--; return;
      }
      const controller = new AbortController();
      request.current = controller;
      const timeout = setTimeout(() => controller.abort(new Error("Transcription took too long. Try again.")), 25000);
      try {
        const wav = dictationWav(
          chunks,
          current.context.sampleRate,
          limits.current.seconds,
        );
        if (wav.byteLength > limits.current.bytes)
          throw new Error(
            "This recording exceeds the workspace’s audio limit.",
          );
        const value = await canterFetch<DictationResult>(
          `${endpoint}${incremental ? "?segment=1" : ""}`,
          {
            method: "POST",
            headers: { "content-type": "audio/wav" },
            body: new Blob([wav], { type: "audio/wav" }),
            signal: controller.signal,
          },
        );
        if (controller.signal.aborted || generation.current !== token) return;
        if (
          typeof value.text !== "string" ||
          value.sent !== false ||
          value.editable !== true
        )
          throw new Error("The transcription response was not valid.");
        const text = value.text.trim();
        if (text) {
          const inserted = acceptTranscript.current(text);
          current.inserted = true;
          setStored((previous) => ({
            ...previous,
            pendingTranscript: inserted
              ? previous.pendingTranscript
              : [previous.pendingTranscript, text].filter(Boolean).join(" "),
            notice: "",
          }));
        }
      } catch (cause) {
        if (generation.current === token) {
          current.failed = true;
          disposeCapture(current);
          if (capture.current === current) capture.current = null;
          const saved = dictationWav(current.recording, current.context.sampleRate, limits.current.seconds);
          if (recoveryURL.current) URL.revokeObjectURL(recoveryURL.current);
          recoveryURL.current = URL.createObjectURL(new Blob([saved], {type:"audio/wav"}));
          setStored((previous) => ({
            ...previous,
            phase: "idle",
            recoveryURL: recoveryURL.current,
            error:
              dictationError(controller.signal.aborted ? controller.signal.reason : cause) +
              " Dictation stopped; text already in your draft is kept.",
          }));
        }
      } finally {
        clearTimeout(timeout);
        current.queued--;
        chunks.length = 0;
        if (request.current === controller) request.current = null;
      }
    };
    current.queued++;
    current.uploading = (current.uploading ?? Promise.resolve()).then(upload);
  }
  function flush(current: Capture, token: number) {
    const chunks = current.chunks;
    current.chunks = [];
    current.segmentSamples = 0;
    if (chunks.length && current.speech)
      transcribe(current, chunks, token, true);
    current.speech = false;
  }
  async function stop() {
    const current = capture.current;
    if (!current) return;
    const token = generation.current;
    capture.current = null;
    disposeCapture(current);
    setStored((value) => ({ ...value, phase: "transcribing", notice: "" }));
    if (limits.current.incremental) flush(current, token);
    else {
      const chunks = current.chunks;
      current.chunks = [];
      if (chunks.length) transcribe(current, chunks, token, false);
    }
    await current.uploading;
    if (generation.current !== token || current.failed) return;
    setStored((value) => ({
      ...value,
      phase: "idle",
      levels: [0, 0, 0, 0, 0, 0],
      notice: current.inserted ? "" : "No speech was transcribed. Try again.",
    }));
  }
  async function start() {
    if (
      !state.enabled ||
      state.phase !== "idle" ||
      state.pendingTranscript ||
      disabled ||
      !endpoint ||
      capture.current ||
      request.current
    )
      return;
    const token = ++generation.current;
    if (recoveryURL.current) URL.revokeObjectURL(recoveryURL.current);
    recoveryURL.current = "";
    setStored((value) => ({
      ...value,
      recoveryURL: "",
      phase: "requesting",
      seconds: 0,
      error: "",
      notice: "",
    }));
    try {
      // Resume in the explicit button gesture, including browsers that do not
      // carry activation across the asynchronous microphone permission prompt.
      const context = new AudioContext({ sampleRate: 16000 });
      const current: Capture = { context, chunks: [], recording: [], queued: 0, samples: 0 };
      capture.current = current;
      const resumed = context.resume().then(
        () => null,
        (cause) => cause,
      );
      const stream = await navigator.mediaDevices.getUserMedia({
        audio: {
          channelCount: 1,
          echoCancellation: true,
          noiseSuppression: true,
        },
        video: false,
      });
      if (generation.current !== token || capture.current !== current) {
        stream.getTracks().forEach((track) => track.stop());
        disposeCapture(current);
        return;
      }
      current.stream = stream;
      const resumeError = await resumed;
      if (resumeError) throw resumeError;
      if (generation.current !== token || capture.current !== current) {
        disposeCapture(current);
        return;
      }
      if (!stream.getAudioTracks().some((track) => track.readyState === "live"))
        throw new Error(
          "The microphone is no longer available. Try recording again.",
        );
      current.source = context.createMediaStreamSource(stream);
      current.analyser = context.createAnalyser();
      current.analyser.fftSize = 256;
      current.analyser.smoothingTimeConstant = 0.75;
      current.source.connect(current.analyser);
      const amplitudes = new Uint8Array(current.analyser.frequencyBinCount);
      let lastPaint = 0;
      const animate = (now: number) => {
        if (capture.current !== current) return;
        current.analyser!.getByteFrequencyData(amplitudes);
        if (now - lastPaint >= 50) {
          lastPaint = now;
          const levels = Array.from({ length: 6 }, (_, i) => {
            const begin = 2 + i * 7;
            let sum = 0;
            for (let j = begin; j < begin + 7; j++) sum += amplitudes[j] ?? 0;
            return Math.min(1, sum / (7 * 170));
          });
          setStored((value) => ({ ...value, levels }));
        }
        current.animation = requestAnimationFrame(animate);
      };
      current.animation = requestAnimationFrame(animate);
      current.processor = context.createScriptProcessor(4096, 1, 1);
      current.sink = context.createGain();
      current.sink.gain.value = 0;
      current.processor.onaudioprocess = (event) => {
        if (generation.current !== token || capture.current !== current) return;
        const remaining =
          Math.floor(context.sampleRate * limits.current.seconds) -
          current.samples;
        const channels = Array.from(
          { length: event.inputBuffer.numberOfChannels },
          (_, channel) => event.inputBuffer.getChannelData(channel),
        );
        const chunk = monoAudioChunk(
          channels,
          Math.min(remaining, event.inputBuffer.length),
        );
        if (chunk.length) {
          current.chunks.push(chunk);
          current.recording.push(chunk);
          current.samples += chunk.length;
          current.segmentSamples = (current.segmentSamples ?? 0) + chunk.length;
          if (microphoneLevel(chunk) > 0.009) {
            current.lastVoice = current.samples;
            current.speech = true;
          }
          if (limits.current.incremental) {
            const duration = (current.segmentSamples ?? 0) / context.sampleRate,
              silence =
                (current.samples - (current.lastVoice ?? 0)) /
                context.sampleRate;
            if (duration >= 5.5 || (duration >= 1.1 && silence >= 0.5))
              flush(current, token);
            if (current.queued >= 4) void stop();
          }
        }
        if (remaining <= event.inputBuffer.length) void stop();
      };
      for (const track of stream.getAudioTracks())
        track.onended = () => {
          if (capture.current === current) {
            generation.current++;
            release();
            setStored((value) => ({
              ...value,
              phase: "idle",
              error:
                "The microphone disconnected. This recording was discarded.",
            }));
          }
        };
      current.source.connect(current.processor);
      current.processor.connect(current.sink);
      current.sink.connect(context.destination);
      const began = performance.now();
      current.timer = setInterval(() => {
        if (capture.current === current)
          setStored((value) => ({
            ...value,
            seconds: Math.min(
              limits.current.seconds,
              Math.floor((performance.now() - began) / 1000),
            ),
          }));
      }, 1000);
      current.deadline = setTimeout(() => {
        if (capture.current === current) void stop();
      }, limits.current.seconds * 1000);
      setStored((value) => ({ ...value, phase: "recording" }));
    } catch (cause) {
      if (generation.current === token) {
        release();
        setStored((value) => ({
          ...value,
          phase: "idle",
          error: dictationError(cause),
        }));
      }
    }
  }
  function insertPending() {
    if (
      state.pendingTranscript &&
      acceptTranscript.current(state.pendingTranscript)
    )
      setStored((value) => ({
        ...value,
        pendingTranscript: "",
        error: "",
        notice: "Added to your draft. Review before sending.",
      }));
  }
  return {
    ...state,
    busy: state.phase !== "idle",
    disabled: disabled || !state.enabled || !!state.pendingTranscript,
    start,
    stop,
    cancel,
    retry: () => setRetry((value) => value + 1),
    editPending: (text: string) =>
      setStored((value) => ({ ...value, pendingTranscript: text })),
    insertPending,
  };
}
