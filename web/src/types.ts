/** REST API DTOs (snake_case matches the Go server's JSON encoding). */

export interface UserDTO {
  id: string;
  email: string;
  display_name: string;
}

export interface NodeDTO {
  id: string;
  name: string;
  cluster: string;
  hostname: string;
  os_version: string;
  agent_version: string;
  shell_type: string;
  status: "online" | "offline" | "disabled";
  last_seen_at: string | null;
}

export interface ClusterDTO {
  id: string;
  name: string;
}

/** GET /api/clusters/{id}/members rows. */
export interface MemberDTO {
  user_id: string;
  email: string;
  display_name: string;
  role: "owner" | "operator" | "viewer";
}

/** POST /api/auth/login response. */
export interface LoginResponse {
  token: string;
  user: UserDTO;
}
