"use client";
import type { useWrenDictation } from "./use-wren-dictation";
import { WorkspaceIcon } from "./workspace-icon";
import styles from "./wren-dictation.module.css";
type Dictation = ReturnType<typeof useWrenDictation>;
export function WrenDictationControl({ dictation }: { dictation: Dictation }) {
  const recording = dictation.phase === "recording",
    pending =
      dictation.phase === "requesting" || dictation.phase === "transcribing";
  return (
    <span className={styles.controls} data-active={dictation.busy}>
      {dictation.busy ? (
        <>
          <button
            type="button"
            className={styles.cancel}
            aria-label="Cancel dictation"
            title="Cancel remaining transcription"
            onClick={dictation.cancel}
          >
            <WorkspaceIcon name="close" width="13" height="13" />
          </button>
          <span className={styles.waveform} aria-hidden="true">
            {dictation.levels.map((level, index) => (
              <i
                key={index}
                style={{
                  height: `${4 + (recording ? level : 0) * 17}px`,
                  opacity: 0.35 + (recording ? level : 0) * 0.65,
                }}
              />
            ))}
          </span>
        </>
      ) : null}
      <button
        type="button"
        className={styles.microphone}
        data-recording={recording || undefined}
        disabled={pending || (!recording && dictation.disabled)}
        title={recording ? "Finish dictation" : dictation.reason}
        aria-label={
          recording
            ? "Stop dictation"
            : pending
              ? dictation.phase === "requesting"
                ? "Waiting for microphone permission"
                : "Finishing dictation"
              : "Dictate message"
        }
        onClick={() => void (recording ? dictation.stop() : dictation.start())}
      >
        {recording ? (
          <span className={styles.stop} />
        ) : pending ? (
          <span className={styles.spinner} />
        ) : (
          <svg
            width="18"
            height="18"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            strokeWidth="1.5"
            strokeLinecap="round"
            strokeLinejoin="round"
            aria-hidden="true"
          >
            <rect x="8.5" y="2.5" width="7" height="12" rx="3.5" />
            <path d="M5.5 10.5v1a6.5 6.5 0 0 0 13 0v-1M12 18v3.5M8.5 21.5h7" />
          </svg>
        )}
      </button>
    </span>
  );
}
export function WrenDictationStatus({ dictation }: { dictation: Dictation }) {
  const status =
    dictation.phase === "requesting"
      ? "Waiting for microphone permission…"
      : dictation.phase === "recording"
        ? `Listening · ${Math.floor(dictation.seconds / 60)}:${String(dictation.seconds % 60).padStart(2, "0")}`
        : dictation.phase === "transcribing"
          ? "Finishing dictation…"
          : dictation.notice;
  return (
    <div className={styles.feedback}>
      {status ? (
        <p
          role="status"
          className={dictation.busy ? styles.listening : undefined}
        >
          {status}
        </p>
      ) : null}
      {dictation.error ? (
        <p role="alert">
          {dictation.error}
          {!dictation.enabled && dictation.checked ? (
            <button type="button" onClick={dictation.retry}>
              Retry dictation
            </button>
          ) : null}
        </p>
      ) : null}
      {dictation.recoveryURL ? <a href={dictation.recoveryURL} download="dictation.wav">Save recording</a> : null}
      {dictation.pendingTranscript ? (
        <div className={styles.review}>
          <label>
            Dictation transcript
            <textarea
              rows={3}
              value={dictation.pendingTranscript}
              onChange={(event) => dictation.editPending(event.target.value)}
            />
          </label>
          <div>
            <button type="button" onClick={dictation.insertPending}>
              Add to draft
            </button>
            <button type="button" onClick={dictation.cancel}>
              Discard transcript
            </button>
          </div>
        </div>
      ) : null}
    </div>
  );
}
