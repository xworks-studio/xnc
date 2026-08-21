import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { api } from "../api";
import type { ClusterDTO } from "../types";

/** Cluster list — rows link to the filtered node list. */
export default function Clusters() {
  const navigate = useNavigate();
  const [clusters, setClusters] = useState<ClusterDTO[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    api<ClusterDTO[]>("/api/clusters")
      .then((rows) => {
        if (!cancelled) setClusters(rows);
      })
      .catch((err: unknown) => {
        if (!cancelled) setError(err instanceof Error ? err.message : "failed to load clusters");
      });
    return () => {
      cancelled = true;
    };
  }, []);

  return (
    <div>
      <h1>Clusters</h1>
      {error && <div className="form-error" role="alert">{error}</div>}
      {clusters && (
        <div className="card flush">
          <table>
            <thead>
              <tr>
                <th>Name</th>
                <th>ID</th>
              </tr>
            </thead>
            <tbody>
              {clusters.map((c) => (
                <tr
                  key={c.id}
                  className="clickable"
                  onClick={() => navigate(`/nodes?cluster=${encodeURIComponent(c.name)}`)}
                >
                  <td>{c.name}</td>
                  <td className="mono dim">{c.id}</td>
                </tr>
              ))}
            </tbody>
          </table>
          {clusters.length === 0 && <div className="empty">No clusters.</div>}
        </div>
      )}
    </div>
  );
}
