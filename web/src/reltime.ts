/** Format an ISO timestamp as a human-friendly relative time string. */
export function formatRelativeTime(iso: string): string {
  const t = new Date(iso);
  if (isNaN(t.getTime())) return iso; // unparseable, return as-is
  const d = Date.now() - t.getTime();
  const s = Math.floor(d / 1000);
  if (s < 10) return "just now";
  if (s < 60) return `${s}s ago`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ago`;
  const dd = Math.floor(h / 24);
  if (dd < 7) return `${dd}d ago`;
  if (dd < 30) return `${Math.floor(dd / 7)}w ago`;
  return t.toLocaleDateString();
}
