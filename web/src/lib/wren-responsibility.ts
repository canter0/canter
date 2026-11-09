import type { Wren } from "./wrens";
export type ResponsibilitySchedule = { kind: "interval" | "cron" | "once" | "event"; everySeconds?: number; cron?: string; timeZone: string; at?: string; eventName?: string };
export type WrenResponsibility = { reportPolicy?: "always" | "changes_only"; id: string; workspaceId: string; conversationId: string; ownerId: string; wrenId: string; name: string; instruction: string; schedule: ResponsibilitySchedule; state: "active" | "paused" | "revoked"; revision: number; nextRunAt?: string; createdAt: string; updatedAt: string };
export type WrenResponsibilityWake = { reportPolicy?: "always" | "changes_only"; id: string; responsibilityId: string; acceptedRevision: number; eventKey: string; triggerKind: string; scheduledFor: string; timeZone: string; instruction: string; wren: Wren; payload: unknown; status: string; runId?: string; reason?: string; failure?: string; createdAt: string };
export type ScheduleMode = "daily" | "weekdays" | "weekly" | "interval" | "once" | "event" | "custom";
export function scheduleMode(schedule: ResponsibilitySchedule): ScheduleMode {
  if (schedule.kind !== "cron") return schedule.kind;
  const parts = schedule.cron?.trim().split(/\s+/) ?? [];
  if (parts.length !== 5 || !/^\d+$/.test(parts[0]) || Number(parts[0]) > 59 || !/^\d+$/.test(parts[1]) || Number(parts[1]) > 23 || parts[2] !== "*" || parts[3] !== "*") return "custom";
  return parts[4] === "*" ? "daily" : parts[4] === "1-5" ? "weekdays" : /^[0-7]$/.test(parts[4]) ? "weekly" : "custom";
}
export function presetCron(mode: "daily" | "weekdays" | "weekly", time: string, weekday: string) {
  const match = /^(\d{2}):(\d{2})$/.exec(time);
  if (!match || Number(match[1]) > 23 || Number(match[2]) > 59 || mode === "weekly" && !/^[0-6]$/.test(weekday)) throw new Error("Choose a valid time and day.");
  return `${Number(match[2])} ${Number(match[1])} * * ${mode === "daily" ? "*" : mode === "weekdays" ? "1-5" : weekday}`;
}
function wallParts(time: number, timeZone: string) {
  const fields = new Intl.DateTimeFormat("en-CA", { timeZone, hourCycle: "h23", year: "numeric", month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", second: "2-digit" }).formatToParts(time);
  return Object.fromEntries(fields.filter(field => ["year", "month", "day", "hour", "minute", "second"].includes(field.type)).map(field => [field.type, field.value]));
}
export function localResponsibilityTime(instant: string, timeZone: string) {
  const p = wallParts(Date.parse(instant), timeZone);
  return `${p.year}-${p.month}-${p.day}T${p.hour}:${p.minute}:${p.second}`;
}
// Return the actual instants matching a wall-clock time. There can be none
// during a spring-forward gap or two during a fall-back overlap.
export function responsibilityTimeCandidates(local: string, timeZone: string) {
  const match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})(?::(\d{2}))?$/.exec(local);
  if (!match) throw new Error("Choose a date and time.");
  const [year, month, day, hour, minute, second] = match.slice(1).map(value => Number(value ?? 0));
  const wall = Date.UTC(year, month - 1, day, hour, minute, second);
  const check = new Date(wall);
  if (check.getUTCFullYear() !== year || check.getUTCMonth() !== month - 1 || check.getUTCDate() !== day || hour > 23 || minute > 59 || second > 59) throw new Error("Choose a valid date and time.");
  const offsets = new Set<number>();
  for (let hours = -36; hours <= 36; hours += 6) {
    const sample = wall + hours * 3600000, p = wallParts(sample, timeZone);
    offsets.add(Date.UTC(Number(p.year), Number(p.month) - 1, Number(p.day), Number(p.hour), Number(p.minute), Number(p.second)) - sample);
  }
  const matches = [...offsets].map(offset => wall - offset).filter(candidate => {
    const p = wallParts(candidate, timeZone);
    return Number(p.year) === year && Number(p.month) === month && Number(p.day) === day && Number(p.hour) === hour && Number(p.minute) === minute && Number(p.second) === second;
  });
  return [...new Set(matches)].sort((a, b) => a - b).map(time => new Date(time).toISOString());
}
export function responsibilityDate(value: string, timeZone: string) {
  try { return new Intl.DateTimeFormat(undefined, { year: "numeric", month: "short", day: "numeric", hour: "numeric", minute: "2-digit", timeZone, timeZoneName: "short" }).format(new Date(value)); } catch { return value; }
}
export function responsibilityScheduleLabel(schedule: ResponsibilitySchedule) {
  if (schedule.kind === "event") return `When ${schedule.eventName ?? "an event arrives"}`;
  if (schedule.kind === "once") return schedule.at ? `Once · ${responsibilityDate(schedule.at, schedule.timeZone)}` : "Once";
  if (schedule.kind === "interval") {
    const seconds = schedule.everySeconds ?? 0, unit = seconds % 86400 === 0 ? 86400 : seconds % 3600 === 0 ? 3600 : seconds % 60 === 0 ? 60 : 1;
    const count = seconds / unit, name = unit === 86400 ? "day" : unit === 3600 ? "hour" : unit === 60 ? "minute" : "second";
    return `Every ${count} ${name}${count === 1 ? "" : "s"}`;
  }
  const mode = scheduleMode(schedule), parts = schedule.cron?.split(/\s+/) ?? [];
  if (mode === "custom") return "Custom schedule";
  const time = new Date(Date.UTC(2000, 0, 1, Number(parts[1]), Number(parts[0]))).toLocaleTimeString([], { timeZone: "UTC", hour: "numeric", minute: "2-digit" });
  const day = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"][Number(parts[4]) % 7];
  return `${mode === "daily" ? "Every day" : mode === "weekdays" ? "Weekdays" : `Every ${day}`} · ${time}`;
}
