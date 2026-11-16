/** Match the server's explicit-mention grammar without waking a Wren for ordinary chat. */
export function restoredRoomRecipient(value: string | null, wrens: {id:string}[]) {
  if (value === "team" && wrens.length > 1) return value;
  return value && wrens.some(wren => wren.id === value) ? value : "people";
}

export function roomMentions(text: string, wrens: { id: string; name: string }[]) {
  const position = (name: string) => new RegExp(`(^|\\s)@${name.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}($|[\\s,:.!?])`, "i").exec(text)?.index ?? -1;
  if (position("team") >= 0) return wrens;
  return wrens.map(wren => ({wren,position:position(wren.name)})).filter(item => item.position >= 0).sort((a,b)=>a.position-b.position).map(item=>item.wren);
}

export function roomAudience(selection: string, text: string, wrens: { id: string; name: string }[]) {
  const explicit = roomMentions(text, wrens);
  const recipients = explicit.length ? explicit : selection === "team" ? wrens : wrens.filter(wren => wren.id === selection);
  return { postOnly: recipients.length === 0, recipientIds: recipients.map(wren => wren.id) };
}
