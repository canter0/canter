export type DictationReadiness = {
  enabled: boolean;
  formats: string[];
  encoding: string;
  maxBytes: number;
  maxDurationSeconds: number;
  autoSend: false;
  incremental?: boolean;
  maxSegmentSeconds?: number;
};
export type DictationResult = {
  text: string;
  durationSeconds: number;
  editable: true;
  sent: false;
};
export const maxDictationSeconds = 60;
export const maxDictationBytes = 8 * 1024 * 1024;

export function monoAudioChunk(
  channels: readonly Float32Array[],
  limit = channels[0]?.length ?? 0,
): Float32Array {
  if (!channels.length) return new Float32Array();
  const length = Math.min(limit, ...channels.map((channel) => channel.length));
  const result = new Float32Array(Math.max(0, length));
  for (const channel of channels) {
    for (let index = 0; index < result.length; index++)
      result[index] +=
        (Number.isFinite(channel[index]) ? channel[index] : 0) /
        channels.length;
  }
  return result;
}

// The browser capture rate can differ by device. Emit one portable 16 kHz,
// mono PCM16 WAV with duration calculated from actual captured samples.
export function dictationWav(
  chunks: readonly Float32Array[],
  sourceRate: number,
  seconds = maxDictationSeconds,
): Uint8Array<ArrayBuffer> {
  if (
    !Number.isFinite(sourceRate) ||
    sourceRate < 8000 ||
    sourceRate > 192000 ||
    !Number.isFinite(seconds) ||
    seconds <= 0 ||
    seconds > maxDictationSeconds
  )
    throw new Error("The microphone returned an unsupported recording rate.");
  const count = chunks.reduce((sum, chunk) => sum + chunk.length, 0);
  if (!count) throw new Error("No audio was captured. Try recording again.");
  if (count / sourceRate > seconds + 1 / sourceRate)
    throw new Error("Keep dictation to 60 seconds or less.");
  const outputRate = 16000;
  const outputCount = Math.min(
    Math.floor(seconds * outputRate),
    Math.floor((count * outputRate) / sourceRate),
  );
  if (!outputCount)
    throw new Error("No audio was captured. Try recording again.");
  const bytes = 44 + outputCount * 2;
  if (bytes > maxDictationBytes)
    throw new Error("This recording is too large.");
  const samples = new Float32Array(count);
  let offset = 0;
  for (const chunk of chunks) {
    samples.set(chunk, offset);
    offset += chunk.length;
  }
  const output = new Uint8Array(bytes),
    view = new DataView(output.buffer);
  const ascii = (at: number, text: string) => {
    for (let i = 0; i < text.length; i++)
      view.setUint8(at + i, text.charCodeAt(i));
  };
  ascii(0, "RIFF");
  view.setUint32(4, bytes - 8, true);
  ascii(8, "WAVE");
  ascii(12, "fmt ");
  view.setUint32(16, 16, true);
  view.setUint16(20, 1, true);
  view.setUint16(22, 1, true);
  view.setUint32(24, outputRate, true);
  view.setUint32(28, outputRate * 2, true);
  view.setUint16(32, 2, true);
  view.setUint16(34, 16, true);
  ascii(36, "data");
  view.setUint32(40, outputCount * 2, true);
  for (let index = 0; index < outputCount; index++) {
    const position = (index * sourceRate) / outputRate,
      left = Math.min(count - 1, Math.floor(position)),
      right = Math.min(count - 1, left + 1);
    const a = Number.isFinite(samples[left]) ? samples[left] : 0,
      b = Number.isFinite(samples[right]) ? samples[right] : 0;
    const interpolated = a + (b - a) * (position - left);
    const sample = Math.max(
      -1,
      Math.min(1, Number.isFinite(interpolated) ? interpolated : 0),
    );
    view.setInt16(
      44 + index * 2,
      Math.round(sample * (sample < 0 ? 32768 : 32767)),
      true,
    );
  }
  return output;
}

// Never replace concurrently typed text or silently truncate spoken words.
export function appendDictationDraft(
  draft: string,
  transcript: string,
  limit = 20000,
): string | null {
  const text = transcript.trim();
  if (!text) return draft;
  const result = draft + (draft && !/\s$/.test(draft) ? " " : "") + text;
  return result.length <= limit ? result : null;
}

export function microphoneLevel(samples: Float32Array) {
  let energy = 0;
  for (const sample of samples)
    energy += (Number.isFinite(sample) ? sample : 0) ** 2;
  return samples.length ? Math.sqrt(energy / samples.length) : 0;
}
