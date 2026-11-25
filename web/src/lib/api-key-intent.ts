export const apiKeysPath = "/app/account/api-keys";
export const apiKeyExpiryOptions = [
  { days: 30, label: "30 days" },
  { days: 90, label: "90 days" },
  { days: 365, label: "365 days" },
];

export type APIKeyInput = { name: string; expiresInDays: number };
const storageKey = "canter:api-key-create-intent";
const intentTTL = 10 * 60 * 1000;

export function apiKeyResumeID(url: URL) {
  if (url.pathname !== apiKeysPath) return null;
  const ids = url.searchParams.getAll("resume");
  return ids.length === 1 && /^[\da-f]{8}-[\da-f]{4}-4[\da-f]{3}-[89ab][\da-f]{3}-[\da-f]{12}$/i.test(ids[0]) ? ids[0] : null;
}

export function storeAPIKeyIntent({ name, expiresInDays }: APIKeyInput, accountId: string) {
  try {
    const id = window.crypto.randomUUID();
    window.sessionStorage.setItem(storageKey, JSON.stringify({ id, accountId, name, expiresInDays, createdAt: Date.now() }));
    return id;
  } catch {
    // Confirmation still works in browsers without usable tab storage or crypto.
    return null;
  }
}

function takeMatchingIntent(id: string) {
  try {
    const storage = window.sessionStorage;
    const raw = storage.getItem(storageKey);
    if (!raw) return null;
    const value: unknown = JSON.parse(raw);
    if (!value || typeof value !== "object" || !("id" in value) || value.id !== id) return null;
    // Never send a request unless the matching draft was actually removed.
    storage.removeItem(storageKey);
    return value;
  } catch {
    return null;
  }
}

export function cancelAPIKeyIntent(id: string) {
  takeMatchingIntent(id);
}

export function takeAPIKeyIntent(id: string, accountId: string): APIKeyInput | null {
  const value = takeMatchingIntent(id);
  if (!value || !("accountId" in value) || value.accountId !== accountId ||
    !("createdAt" in value) || typeof value.createdAt !== "number" || !Number.isSafeInteger(value.createdAt) ||
    !("name" in value) || typeof value.name !== "string" ||
    !("expiresInDays" in value) || typeof value.expiresInDays !== "number" ||
    !apiKeyExpiryOptions.some(option => option.days === value.expiresInDays)) return null;

  const age = Date.now() - value.createdAt;
  const name = value.name.trim();
  if (age < 0 || age > intentTTL || !name || name.length > 80 || /[\u0000-\u001f\u007f-\u009f]/.test(name)) return null;
  return { name, expiresInDays: value.expiresInDays };
}
