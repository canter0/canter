import type { OperatorAttachment } from "./operator-api";

export const attachmentAccept = ".pdf,.docx,.xlsx,.png,.jpg,.jpeg,.gif,.webp,image/png,image/jpeg,image/gif,image/webp,.txt,.md,.json,.csv,.ts,.tsx,.js,.jsx,.css,.html,.yaml,.yml,.go,.py,.sh,.log,.xml,.toml";
// Match the control plane's chat attachment limits.
export const attachmentLimits = { count: 8, fileMB: 8, totalMB: 10 } as const;
const bytesPerMB = 1048576;
export function attachmentBytes(items: ReadonlyArray<{ size: number }>) { return items.reduce((sum, item) => sum + item.size, 0); }
export function attachmentURL(item: OperatorAttachment) { return `data:${item.mediaType};base64,${item.dataBase64}`; }
export function attachmentSize(size: number) { return size < 1024 ? `${size} B` : size < 1048576 ? `${Math.ceil(size / 1024)} KB` : `${(size / 1048576).toFixed(1)} MB`; }

export async function readAttachments(files: File[], current: OperatorAttachment[]): Promise<OperatorAttachment[]> {
  if (files.length + current.length > attachmentLimits.count) throw new Error(`This message can hold up to ${attachmentLimits.count} files. Remove an attachment to make room.`);
  if (files.some(file => !file.size)) throw new Error("This file is empty.");
  if (files.some(file => file.size > attachmentLimits.fileMB * bytesPerMB)) throw new Error(`This file exceeds ${attachmentLimits.fileMB} MB.`);
  if (attachmentBytes(files) + attachmentBytes(current) > attachmentLimits.totalMB * bytesPerMB) throw new Error(`This would exceed ${attachmentLimits.totalMB} MB total. Remove an attachment or pick a smaller file.`);
  return Promise.all(files.map(async file => {
    const image = /^(image\/(png|jpeg|gif|webp))$/.test(file.type);
    const office = /\.(pdf|docx|xlsx)$/i.test(file.name);
    if (!image && !office && !/\.(txt|md|json|csv|tsx?|jsx?|css|html|ya?ml|go|py|sh|log|xml|toml)$/i.test(file.name)) throw new Error("Choose PNG, JPEG, GIF, WebP, PDF, DOCX, XLSX, or a text/code file.");
    if (!image && !office) {
      if (file.size > 65536) throw new Error("Text and code files must be under 64 KB.");
      const bytes = await file.arrayBuffer();
      const text = new TextDecoder("utf-8", { fatal: true }).decode(bytes);
      if (text.includes("\0")) throw new Error(`${file.name} is not a text file.`);
    }
    const dataBase64 = await new Promise<string>((resolve, reject) => {
      const reader = new FileReader();
      reader.onload = () => resolve(String(reader.result).split(",")[1]);
      reader.onerror = () => reject(new Error(`Could not read ${file.name}.`));
      reader.readAsDataURL(file);
    });
    return { id: crypto.randomUUID(), name: file.name, mediaType: image ? file.type : office ? (/\.pdf$/i.test(file.name) ? "application/pdf" : /\.docx$/i.test(file.name) ? "application/vnd.openxmlformats-officedocument.wordprocessingml.document" : "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet") : "text/plain", dataBase64, size: file.size };
  }));
}

// A bad file must not discard valid neighbors in a multi-file drop.
export async function readAttachmentBatch(files: File[], current: OperatorAttachment[]) {
  const items: OperatorAttachment[] = [];
  const errors: string[] = [];
  for (const file of files) {
    try { items.push(...await readAttachments([file], [...current, ...items])); }
    catch (cause) { errors.push(`${file.name || "File"}: ${cause instanceof Error ? cause.message : "Could not read this file."}`); }
  }
  return { items, errors };
}

export function transferredFiles(transfer: DataTransfer): File[] {
  const files = Array.from(transfer.files);
  if (files.length) return files;
  return Array.from(transfer.items).filter(item => item.kind === "file").map(item => item.getAsFile()).filter((file): file is File => file !== null);
}
