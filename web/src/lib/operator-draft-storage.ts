export function clearOperatorTextDrafts(storage: Pick<Storage, "length" | "key" | "removeItem">) {
  for (let index = storage.length - 1; index >= 0; index--) {
    const key = storage.key(index);
    if (key?.startsWith("canter:conversation-draft:")) storage.removeItem(key);
  }
}
