/* lib/api.ts — the only module that talks to the backend.
 *
 * Base URL: /api/v1 on the same origin (next.config.ts rewrites it to
 * the API during dev/build). Local dev defaults to http://127.0.0.1:8080.
 * For a host without those rewrites, set NEXT_PUBLIC_API_BASE
 * (for example, "http://localhost:8080/api/v1").
 *
 * Responses are typed but the backend is evolving in parallel (risk
 * fields, extended /savings, /series) — every consumer must feature-
 * check with pick and never assume a field exists. */

import type {
  AlertingConfigResponse,
  AlertRoutingConfig,
  AlertTestResponse,
  ApplyEvent,
  ApplyPayload,
  ApplyResult,
  ByOwnerRaw,
  CostOpportunitiesResponse,
  CostScanResponse,
  GitHubIntegrationConfig,
  GitHubIntegrationResponse,
  IaCPullRequestResponse,
  LoginResponse,
  MeResponse,
  Pagination,
  ReportConfig,
  ReportConfigResponse,
  RecommendationsResponse,
  SavingsResponse,
  SavingsReportResponse,
  SeriesResponse,
  SendReportResponse,
  SystemStatusResponse,
  Team,
  User,
  VerificationRun,
  Workload,
} from "./types";

const BASE = (process.env.NEXT_PUBLIC_API_BASE || "/api/v1").replace(/\/+$/, "");
const FETCH_TIMEOUT_MS = 10_000;

function timeoutSignal(path: string): { signal: AbortSignal; cleanup: () => void } {
  const controller = new AbortController();
  const timeout = window.setTimeout(() => {
    controller.abort(new Error(`Request timed out after ${FETCH_TIMEOUT_MS / 1000}s on ${path}`));
  }, FETCH_TIMEOUT_MS);
  return {
    signal: controller.signal,
    cleanup: () => window.clearTimeout(timeout),
  };
}

export class ApiError extends Error {
  status: number;
  body: unknown;
  constructor(status: number, path: string, body: unknown) {
    super(`HTTP ${status} on ${path}`);
    this.name = "ApiError";
    this.status = status;
    this.body = body;
  }
}

export function apiBase(): string {
  return BASE;
}

/* First defined value among candidate keys — defensive feature-checking
 * for fields the backend is adding in parallel. */
export function pick<T>(obj: unknown, keys: string[]): T | undefined {
  if (!obj || typeof obj !== "object") return undefined;
  const o = obj as Record<string, unknown>;
  for (const k of keys) {
    if (o[k] !== undefined && o[k] !== null) return o[k] as T;
  }
  return undefined;
}

function queryString(params?: Record<string, unknown>): string {
  if (!params) return "";
  const pairs = Object.entries(params)
    .filter(([, v]) => v !== undefined && v !== null && v !== "")
    .map(([k, v]) => `${encodeURIComponent(k)}=${encodeURIComponent(String(v))}`);
  return pairs.length ? `?${pairs.join("&")}` : "";
}

async function request<T>(path: string, params?: Record<string, unknown>): Promise<T> {
  const url = `${BASE}${path}${queryString(params)}`;
  const timeout = timeoutSignal(`${BASE}${path}`);
  const res = await fetch(url, { credentials: "include", signal: timeout.signal }).finally(timeout.cleanup);
  let json: unknown = null;
  try {
    json = await res.json();
  } catch {
    /* non-JSON error body — carry the raw status text instead */
  }
  if (!res.ok) {
    throw new ApiError(res.status, `${BASE}${path}`, json);
  }
  return json as T;
}

/* POST with a JSON body; on non-2xx the parsed body travels on the
 * ApiError — the apply endpoint answers 422 with {"error","reasons"} and
 * the UI renders the reasons verbatim. */
async function post<T>(path: string, payload: unknown): Promise<T> {
  return write<T>("POST", path, payload);
}

async function write<T>(method: "POST" | "PATCH" | "PUT" | "DELETE", path: string, payload?: unknown): Promise<T> {
  const timeout = timeoutSignal(`${BASE}${path}`);
  const res = await fetch(`${BASE}${path}`, {
    method,
    credentials: "include",
    signal: timeout.signal,
    headers: { "Content-Type": "application/json" },
    body: method === "DELETE" ? undefined : JSON.stringify(payload ?? {}),
  }).finally(timeout.cleanup);
  const json: unknown = await res.json().catch(() => null);
  if (!res.ok) {
    throw new ApiError(res.status, `${BASE}${path}`, json);
  }
  return json as T;
}

export const api = {
  /* GET /workloads -> {workloads: [...]} */
  async workloads(): Promise<Workload[]> {
    const body = await request<{ workloads?: Workload[] }>("/workloads");
    return Array.isArray(body?.workloads) ? body.workloads : [];
  },

  async teams(): Promise<Team[]> {
    const body = await request<{ teams?: Team[] }>("/teams");
    return Array.isArray(body?.teams) ? body.teams : [];
  },

  async createTeam(payload: { name: string; owner: string; on_call: string }): Promise<Team> {
    const body = await post<{ team: Team }>("/teams", payload);
    return body.team;
  },

  async updateTeam(id: number, payload: { owner: string; on_call: string }): Promise<Team> {
    const body = await write<{ team: Team }>("PATCH", `/teams/${id}`, payload);
    return body.team;
  },

  async assignWorkloadTeam(workloadID: number, teamID: number): Promise<Workload> {
    const body = await write<{ workload: Workload }>("PUT", `/workloads/${workloadID}/team`, { team_id: teamID });
    return body.workload;
  },

  async unassignWorkloadTeam(workloadID: number): Promise<Workload> {
    const body = await write<{ workload: Workload }>("DELETE", `/workloads/${workloadID}/team`);
    return body.workload;
  },

  /* GET /workloads/{id} -> single workload object (404 for unknown ids) */
  workload(id: string | number): Promise<Workload> {
    return request<Workload>(`/workloads/${id}`);
  },

  /* GET /recommendations?status=&workload_id=&limit=&offset= */
  recommendations(params?: Record<string, unknown>): Promise<RecommendationsResponse> {
    return request<RecommendationsResponse>("/recommendations", params).then((body) => ({
      recommendations: Array.isArray(body?.recommendations) ? body.recommendations : [],
      pagination: (body?.pagination as Pagination | null) ?? null,
    }));
  },

  /* GET /savings — body is passed through and feature-checked by views. */
  savings(): Promise<SavingsResponse> {
    return request<SavingsResponse>("/savings");
  },

  systemStatus(): Promise<SystemStatusResponse> {
    return request<SystemStatusResponse>("/system/status");
  },

  alertingConfig(): Promise<AlertingConfigResponse> {
    return request<AlertingConfigResponse>("/alerting/config");
  },

  saveAlertingConfig(config: AlertRoutingConfig): Promise<AlertingConfigResponse> {
    return write<AlertingConfigResponse>("PUT", "/alerting/config", config);
  },

  testAlerting(config?: AlertRoutingConfig): Promise<AlertTestResponse> {
    return post<AlertTestResponse>("/alerting/test", config || {});
  },

  githubIntegration(): Promise<GitHubIntegrationResponse> {
    return request<GitHubIntegrationResponse>("/integrations/github");
  },

  saveGitHubIntegration(config: GitHubIntegrationConfig): Promise<GitHubIntegrationResponse> {
    return write<GitHubIntegrationResponse>("PUT", "/integrations/github", config);
  },

  reportConfig(): Promise<ReportConfigResponse> {
    return request<ReportConfigResponse>("/reports/config");
  },

  saveReportConfig(config: ReportConfig): Promise<ReportConfigResponse> {
    return write<ReportConfigResponse>("PUT", "/reports/config", config);
  },

  savingsReport(rangeDays: number): Promise<SavingsReportResponse> {
    return request<SavingsReportResponse>("/reports/savings", { range: `${rangeDays}d` });
  },

  reportPdfUrl(rangeDays: number): string {
    return `${BASE}/reports/savings?range=${encodeURIComponent(`${rangeDays}d`)}&format=pdf`;
  },

  sendReport(rangeDays: number): Promise<SendReportResponse> {
    return post<SendReportResponse>("/reports/send", { range_days: rangeDays });
  },

  /* GET /workloads/{id}/series?metric=&days= — envelope normalized. */
  async series(id: string | number, params?: Record<string, unknown>): Promise<SeriesResponse> {
    const body = await request<SeriesResponse>(`/workloads/${id}/series`, params);
    return body && typeof body === "object" ? body : {};
  },

  /* GET /applies?workload_id=&result= */
  async applies(params?: Record<string, unknown>): Promise<ApplyEvent[]> {
    const body = await request<{ applies?: ApplyEvent[] }>("/applies", params);
    return Array.isArray(body?.applies) ? body.applies : [];
  },

  
  async verificationRuns(params?: Record<string, unknown>): Promise<VerificationRun[]> {
    const body = await request<{ verification_runs?: VerificationRun[] }>("/verification-runs", params);
    return Array.isArray(body?.verification_runs) ? body.verification_runs : [];
  },

  /* POST /recommendations/{id}/apply — resolves with the apply result;
   * rejects with ApiError carrying status + parsed body (422 reasons). */
  apply(id: string | number, payload: ApplyPayload): Promise<ApplyResult> {
    return post<ApplyResult>(`/recommendations/${id}/apply`, payload);
  },

  async costOpportunities(status = ""): Promise<CostOpportunitiesResponse> {
    const body = await request<CostOpportunitiesResponse>("/cost-opportunities", { status });
    return {
      opportunities: Array.isArray(body?.opportunities) ? body.opportunities : [],
      latest_prs: body?.latest_prs || {},
      summary: body?.summary || { count: 0, monthly_savings: 0 },
    };
  },

  scanCostOpportunities(): Promise<CostScanResponse> {
    return post<CostScanResponse>("/cost-opportunities/scan", {});
  },

  prepareIaCPullRequest(id: string | number, repo?: string): Promise<IaCPullRequestResponse> {
    return post<IaCPullRequestResponse>(`/cost-opportunities/${id}/iac-pr`, { repo });
  },

  prepareRecommendationIaCPullRequest(
    id: string | number,
    payload?: { repo?: string; path?: string; terraform_addr?: string; actor?: string },
  ): Promise<IaCPullRequestResponse> {
    return post<IaCPullRequestResponse>(`/recommendations/${id}/iac-pr`, payload || {});
  },


  
  async me(): Promise<MeResponse> {
    try {
      const body = await request<MeResponse>("/auth/me");
      return { auth_enabled: !!body?.auth_enabled, user: body?.user ?? null };
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) {
        const body = (e.body ?? {}) as { needs_setup?: boolean };
        return { auth_enabled: true, user: null, needs_setup: body.needs_setup === true };
      }
      throw e;
    }
  },

  
  async setup(name: string, email: string, password: string): Promise<User> {
    const body = await post<LoginResponse>("/auth/setup", { name, email, password });
    return body?.user ?? null;
  },

  /* POST /auth/login — sets the httpOnly session cookie server-side;
   * resolves with the user. Rejects 401 on bad credentials. */
  async login(email: string, password: string): Promise<User> {
    const body = await post<LoginResponse>("/auth/login", { email, password });
    return body?.user ?? null;
  },

  
  async logout(): Promise<void> {
    await post<unknown>("/auth/logout", {});
  },
};

export type { ByOwnerRaw };
