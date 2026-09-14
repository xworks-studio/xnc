/**
 * API client: fetch wrapper with JWT auth and structured error handling.
 *
 * - Attaches `Authorization: Bearer <xnc_token>` from localStorage.
 * - 401 away from /login: drop the token and hard-redirect to /login.
 *   (On /login itself the 401 is rethrown so the form can show
 *   "invalid credentials" instead of triggering a redirect loop.)
 *   调用方可传 opts.on401 = "throw" 让 401 跳过该自动登出：改密端点的
 *   401 = 当前密码错误（服务端防枚举统一文案），须在页内展示服务端错误。
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

/** api() 选项：在 RequestInit 之上扩展 401 自动登出的豁免开关。 */
export interface ApiOptions extends RequestInit {
  /** 传 "throw" 时 401 不按"会话过期"清 token + 跳转 /login，
   *  而是走常规 !res.ok 路径抛出携带服务端消息的 APIError。
   *  用于改密端点：其 401 = 当前密码错误而非会话过期，错误须页内展示。 */
  on401?: "throw";
}

export async function api<T>(path: string, opts?: ApiOptions): Promise<T> {
  // 剥离自定义字段，不透传给 fetch
  const { on401, ...init } = opts ?? {};
  const headers = new Headers(init.headers);
  const token = localStorage.getItem(TOKEN_KEY);
  if (token) headers.set("Authorization", `Bearer ${token}`);
  if (init.body != null && !headers.has("Content-Type")) {
    headers.set("Content-Type", "application/json");
  }

  const res = await fetch(path, { ...init, headers });

  // 改密端点 401=当前密码错误（防枚举）而非会话过期：on401:"throw" 时
  // 跳过清 token + 跳转，落入下方 !res.ok 路径页内展示服务端错误。
  if (res.status === 401 && on401 !== "throw" && window.location.pathname !== "/login") {
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
  // 2xx 空 body（如 adminDeleteNode 的 200 空 body）同样返回 undefined——
  // 直接 res.json() 会对空体抛 "Unexpected end of JSON input"。
  if (res.status === 204) return undefined as T;
  const text = await res.text();
  return (text ? JSON.parse(text) : undefined) as T;
}
