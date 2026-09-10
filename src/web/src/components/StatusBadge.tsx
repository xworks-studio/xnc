import type { NodeDTO } from "../types";

/** Colored status pill: green=online, gray=offline, red=disabled. */
export default function StatusBadge({ status }: { status: NodeDTO["status"] }) {
  return <span className={`badge badge-${status}`}>{status}</span>;
}
