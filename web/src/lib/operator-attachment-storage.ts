let database: Promise<IDBDatabase> | undefined;

export function openOperatorAttachmentDatabase(): Promise<IDBDatabase> {
  if (!database) {
    database = new Promise<IDBDatabase>((resolve, reject) => {
      const request = indexedDB.open("canter-drafts", 1);
      request.onupgradeneeded = () => request.result.createObjectStore("attachments");
      request.onsuccess = () => resolve(request.result);
      request.onerror = () => reject(request.error);
    }).catch(cause => {
      database = undefined;
      throw cause;
    });
  }
  return database;
}

export async function clearOperatorAttachmentStorage() {
  try {
    await withTimeout((async () => {
      const db = await openOperatorAttachmentDatabase();
      await new Promise<void>((resolve, reject) => {
        const transaction = db.transaction("attachments", "readwrite");
        transaction.objectStore("attachments").clear();
        transaction.oncomplete = () => resolve();
        transaction.onerror = () => reject(transaction.error);
        transaction.onabort = () => reject(transaction.error);
      });
    })(), 500);
  } catch { /* Local draft storage may be unavailable or blocked. */ }
}

function withTimeout<T>(operation: Promise<T>, milliseconds: number): Promise<T> {
  let timer: ReturnType<typeof setTimeout>;
  return Promise.race([
    operation,
    new Promise<never>((_, reject) => { timer = setTimeout(() => reject(new Error("IndexedDB cleanup timed out")), milliseconds); }),
  ]).finally(() => clearTimeout(timer!));
}
