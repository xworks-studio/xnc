import { Fragment, useCallback, useEffect, useState, type ReactNode } from "react";
import { Link, useParams } from "react-router-dom";
import { api, APIError } from "../api";
import type { NodeDTO } from "../types";
import StatusBadge from "../components/StatusBadge";

/**
 * Node detail: info card + actions.
 * - Terminal → web terminal for this node.
 * - Remote Desktop → shows the `xnc rdp` CLI command (spec §52; RDP tunneling
 *   is a CLI feature, the web UI just surfaces the command).
 * - Disable/Enable → POST admin endpoints, owner-only server-side. 403 is
 *   rendered as "permission denied" (viewer/operator keeps the buttons but
 *   the server rejects them).
 */
export default function NodeDetail() {
  const { id } = useParams<{ id: string }>();
  const [node, setNode] = useState<NodeDTO | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [showRdp, setShowRdp] = useState(false);

  const load = useCallback(async () => {
    try {
      setNode(await api<NodeDTO>(`/api/nodes/${id}`));
      setError(null);
    } catch (err) {
      setError(err instanceof Error ? err.message : "failed to load node");
    }
  }, [id]);

  useEffect(() => {
    void load();
  }, [load]);

  async function toggleDisabled(enable: boolean) {
    setBusy(true);
    setActionError(null);
    try {
      await api<void>(`/api/nodes/${id}/${enable ? "enable" : "disable"}`, {
        method: "POST",
      });
      await load();
    } catch (err) {
      if (err instanceof APIError && err.code === "FORBIDDEN") {
        setActionError("permission denied");
      } else {
        setActionError(err instanceof Error ? err.message : "action failed");
      }
    } finally {
      setBusy(false);
    }
  }

  if (error && !node) {
    return (
      <div>
        <h1>Node</h1>
        <div className="form-error" role="alert">{error}</div>
      </div>
    );
  }
  if (!node) return <div className="loading">Loading…</div>;

  const rows: Array<[string, ReactNode]> = [
    ["Name", node.name],
    ["Cluster", node.cluster],
    ["Status", <StatusBadge key="s" status={node.status} />],
    ["Hostname", node.hostname],
    ["OS", node.os_version],
    ["Agent", node.agent_version],
    ["Shell", node.shell_type],
    ["Last seen", node.last_seen_at ? new Date(node.last_seen_at).toLocaleString() : "never"],
    ["ID", <span key="id" className="mono">{node.id}</span>],
  ];

  return (
    <div>
      <h1>{node.name}</h1>
      <div className="btn-row">
        <Link className="btn" to={`/terminal/${node.id}`}>Terminal</Link>
        <button type="button" onClick={() => setShowRdp((v) => !v)}>
          {showRdp ? "Hide Remote Desktop" : "Remote Desktop"}
        </button>
        {node.status !== "disabled" ? (
          <button type="button" className="danger" disabled={busy} onClick={() => toggleDisabled(false)}>
            Disable
          </button>
        ) : (
          <button type="button" disabled={busy} onClick={() => toggleDisabled(true)}>
            Enable
          </button>
        )}
        {actionError && <span className="form-error" role="alert">{actionError}</span>}
      </div>
      {showRdp && (
        <div className="rdp-hint">
          <div className="dim">Connect from a machine with the xnc CLI installed:</div>
          <code className="cmd">xnc rdp {node.name}</code>
        </div>
      )}
      <div className="card">
        <dl className="detail-grid">
          {rows.map(([label, value]) => (
            <Fragment key={label}>
              <dt>{label}</dt>
              <dd>{value}</dd>
            </Fragment>
          ))}
        </dl>
      </div>
    </div>
  );
}
