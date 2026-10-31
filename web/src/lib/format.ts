// Dates in lists are short and fixed-width enough to scan down a column:
// time today, day and month this year, the full date before that.

const pad = (n: number) => String(n).padStart(2, "0");

export function listTime(iso: string, now = new Date()): string {
  const d = new Date(iso);
  if (d.toDateString() === now.toDateString()) return `${pad(d.getHours())}:${pad(d.getMinutes())}`;
  if (d.getFullYear() === now.getFullYear()) return `${d.toLocaleString("en", { month: "short" })} ${d.getDate()}`;
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}

/** Local time with offset, for the reading pane. */
export function fullTime(iso: string): string {
  const d = new Date(iso);
  const off = -d.getTimezoneOffset();
  const sign = off >= 0 ? "+" : "-";
  const tz = `${sign}${pad(Math.floor(Math.abs(off) / 60))}${pad(Math.abs(off) % 60)}`;
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())} ${tz}`;
}

export function sender(name: string, addr: string): string {
  return name.trim() || addr || "(no sender)";
}

/** The local part of prefix@zone. */
export function localPart(address: string): string {
  const i = address.lastIndexOf("@");
  return i > 0 ? address.slice(0, i) : address;
}

export const count = (n: number) => n.toLocaleString("en");
