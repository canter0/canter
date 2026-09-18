import type { OperatorAttachment } from "./operator-api";

export const attachmentAccept = ".png,.jpg,.jpeg,.gif,.webp,image/png,image/jpeg,image/gif,image/webp,.txt,.md,.json,.csv,.ts,.tsx,.js,.jsx,.css,.html,.yaml,.yml,.go,.py,.sh,.log,.xml,.toml";
export function attachmentURL(item: OperatorAttachment) { return `data:${item.mediaType};base64,${item.dataBase64}`; }
export function attachmentSize(size: number) { return size < 1024 ? `${size} B` : size < 1048576 ? `${Math.ceil(size / 1024)} KB` : `${(size / 1048576).toFixed(1)} MB`; }

export async function readAttachments(files: File[], current: OperatorAttachment[]): Promise<OperatorAttachment[]> {
  if (files.length + current.length > 4) throw new Error("Add up to 4 attachments per message.");
  if (files.some(file => !file.size || file.size > 2 * 1048576)) throw new Error("Each file must be nonempty and under 2 MB.");
  if ([...files, ...current].reduce((sum, file) => sum + file.size, 0) > 5 * 1048576) throw new Error("Attachments can total up to 5 MB.");
  return Promise.all(files.map(async file => {
    const image = /^(image\/(png|jpeg|gif|webp))$/.test(file.type);
    if (!image && !/\.(txt|md|json|csv|tsx?|jsx?|css|html|ya?ml|go|py|sh|log|xml|toml)$/i.test(file.name)) throw new Error("Choose PNG, JPEG, GIF, WebP, or a text/code file.");
    if (!image) {
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
    return { id: crypto.randomUUID(), name: file.name, mediaType: image ? file.type : "text/plain", dataBase64, size: file.size };
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
