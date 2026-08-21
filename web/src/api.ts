/**
 * API client: fetch wrapper with JWT auth and structured error handling.
 *
 * - Attaches `Authorization: Bearer <xnc_token>` from localStorage.
 * - 401 away from /login: drop the token and hard-redirect to /login.
 *   (On /login itself the 401 is rethrown so the form can show
 *   "invalid credentials" instead of triggering a redirect loop.)
 * - Errors are thrown as APIError carrying {code, message} from the
 *   server's `{"error":{"code","message"}}` response body.
 */

export const TOKEN_KEY = "xnc_token";

export class APIError extends Error {
  readonly code: string;

  constructor(code: string, message: string) {
    super(message);
    this.name = "APIError";
    this.code = code;
  }
}

export async function api<T>(path: string, opts?: RequestInit): Promise<T> {
  const headers = new Headers(opts?.headers);
  const token = localStorage.getItem(TOKEN_KEY);
  if (token) headers.set("Authorization", `Bearer ${token}`);
  if (opts?.body != null && !headers.has("Content-Type")) {
    headers.set("Content-Type", "application/json");
  }

  const res = await fetch(path, { ...opts, headers });

  if (res.status === 401 && window.location.pathname !== "/login") {
    localStorage.removeItem(TOKEN_KEY);
    window.location.assign("/login");
    throw new APIError("unauthorized", "session expired");
  }

  if (!res.ok) {
    let code = "error";
    let message = `request failed: ${res.status} ${res.statusText}`;
    try {
      const body = (await res.json()) as { error?: { code?: string; message?: string } };
      if (body.error?.code) code = body.error.code;
      if (body.error?.message) message = body.error.message;
    } catch {
      // Non-JSON error body — keep the fallback message.
    }
    throw new APIError(code, message);
  }

  // 204 No Content (node disable/enable, member remove) — no JSON body to parse.
  if (res.status === 204) return undefined as T;

  return (await res.json()) as T;
}
