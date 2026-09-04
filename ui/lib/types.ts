/* Wire types for the Consize API (/api/v1). Field names are the store's
 * Go struct fields verbatim (PascalCase); a few newer fields marshal as
 * snake_case JSON ("risk", "risk_reasons", "by_owner") — the client
 * feature-checks both cases defensively (see pick() in api.ts). */

export interface Workload {
  ID: number;
  Name: string;
  Namespace: string;
  Kind: string;
  Source: "k8s" | "db" | string; // "db" (RDS) vs "k8s" (compute)
  RequestCPUMilli: number;
  LimitCPUMilli: number;
  RequestMemBytes: number;
  LimitMemBytes: number;
  DBClass?: string;
  DBReplicas?: number;
  DBMaintenanceWindow?: string;
  DBProvider?: string;
  TeamID?: number;
  TeamName?: string;
  TeamOnCall?: string;
  Labels?: Record<string, string>;
  risk?: string;
  risk_reasons?: string[] | string;
  Risk?: string;
  RiskReasons?: string[] | string;
}

export type RecommendationResource = "cpu" | "memory" | "class";

export interface Recommendation {
  ID: number;
  WorkloadID: number;
  WorkloadName: string;
  Namespace: string;
  Resource: RecommendationResource | string;
  CurrentValue: number;
  ProposedValue: number;
  CurrentLimit: number;
  ProposedLimit: number;
  SavingsMonthly: number;
  Confidence: number; // 0..1
  Status: string; // pending | applied | verified | rolled_back | superseded | rejected
  CreatedAt: string;
  ClassCurrent?: string;
  ClassProposed?: string;
  StepNumber?: number;
  TotalSteps?: number;
  risk?: string;
  risk_reasons?: string[] | string;
  Risk?: string;
  RiskReasons?: string[] | string;
}

export interface Pagination {
  limit: number;
  offset: number;
  total: number;
}

export interface RecommendationsResponse {
  recommendations: Recommendation[];
  pagination: Pagination | null;
}

/* Apply-event Diff — wire keys are snake_case; class events name the
 * change via current_class/proposed_class (request/limit are 0). */
export interface ApplyDiff {
  resource?: string;
  current_request?: number;
  proposed_request?: number;
  current_limit?: number;
  proposed_limit?: number;
  current_class?: string;
  proposed_class?: string;
}

export interface ApplyEvent {
  ID: number;
  RecommendationID: number;
  WorkloadID: number;
  Actor: string;
  Mode: string; // dry_run | approved | auto
  Result: string; // planned | applied | reverted
  StepNumber: number;
  TotalSteps: number;
  Diff?: ApplyDiff | null;
  CreatedAt: string;
}

export interface VerificationRun {
  ID: number;
  ApplyEventID: number;
  Verdict: string; // passed | failed | inconclusive
  SLIs?: Record<string, unknown> | null;
  BaselineStart?: string;
  BaselineEnd?: string;
  PostStart?: string;
  PostEnd?: string;
  CreatedAt: string;
}

export interface SavingsResponse {
  projected_monthly_savings?: number;
  realized_monthly?: number;
  realized_monthly_savings?: number;
  realized_savings_monthly?: number;
  realized_yearly?: number;
  active_recommendations?: number;
  pending_recommendations?: number;
  pending_count?: number;
  by_owner?: ByOwnerRaw;
  price_table?: Record<string, number>;
}

export interface SystemStatusResponse {
  status: "healthy" | "attention" | "degraded" | "unavailable" | "empty" | string;
  generated_at: string;
  latest_usage_bucket?: string | null;
  telemetry_age_seconds?: number | null;
  stale_after_seconds: number;
  verify_window_seconds: number;
  workloads: number;
  pending_recommendations: number;
  in_flight_applies: number;
  verification_due: number;
  next_verification_due_at?: string | null;
  store: string;
  messages: string[];
}

export interface AlertIntegration {
  type: "slack" | string;
  webhook_env: string;
  webhook_url?: string;
  channel?: string;
  mention?: string;
}

export interface AlertContactPoint {
  name: string;
  integrations: AlertIntegration[];
}

export interface AlertNotificationPolicy {
  name: string;
  match: Record<string, string>;
  contact_point: string;
  continue?: boolean;
}

export interface AlertRoutingConfig {
  default_contact_point: string;
  contact_points: AlertContactPoint[];
  notification_policies: AlertNotificationPolicy[];
}

export interface AlertingConfigResponse {
  config: AlertRoutingConfig;
  source: "store" | "env" | string;
}

export interface AlertDeliveryResult {
  contact_point: string;
  type: string;
  status: "sent" | "failed" | string;
  error?: string;
}

export interface AlertTestResponse {
  results: AlertDeliveryResult[];
}

export interface ReportConfig {
  enabled: boolean;
  range_days: number;
}

export interface ReportConfigResponse {
  config: ReportConfig;
  source: "store" | "default" | string;
}

export interface ReportRecommendation {
  id: number;
  workload_id: number;
  workload_name: string;
  namespace: string;
  resource: string;
  current: string;
  proposed: string;
  savings_monthly: number;
  created_at: string;
}

export interface ReportApplyEvent {
  id: number;
  recommendation_id: number;
  workload_name: string;
  namespace: string;
  resource: string;
  change: string;
  actor: string;
  created_at: string;
}

export interface SavingsReport {
  generated_at: string;
  from: string;
  to: string;
  range_days: number;
  projected_monthly_savings: number;
  pending_recommendations: number;
  realized_this_period_monthly_savings: number;
  verified_applies: number;
  rollbacks: number;
  failed_verifications: number;
  inconclusive_verifications: number;
  latest_usage_bucket?: string | null;
  data_quality: string[];
  top_pending_recommendations: ReportRecommendation[];
  recent_rollbacks: ReportApplyEvent[];
}

export interface CostOpportunity {
  ID: number;
  Provider: string;
  Account: string;
  Region: string;
  ResourceType: string;
  ResourceID: string;
  Name: string;
  MonthlyCost: number;
  Recommendation: string;
  Action: string;
  Risk: string;
  Status: string;
  Evidence?: Record<string, unknown>;
  IaCRepo?: string;
  IaCPath?: string;
  TerraformAddr?: string;
  FirstSeenAt: string;
  LastSeenAt: string;
  CreatedAt: string;
  UpdatedAt: string;
}

export interface IaCPullRequest {
  ID: number;
  OpportunityID?: number;
  RecommendationID?: number;
  ChangeKind?: "recommendation" | "cost_opportunity" | string;
  Actor: string;
  Provider: string;
  Repo: string;
  Branch: string;
  Title: string;
  Body: string;
  Diff: string;
  Status: string;
  URL?: string;
  Error?: string;
  CreatedAt: string;
}

export interface CostAction {
  ID: number;
  OpportunityID: number;
  Actor: string;
  Mode: string;
  Result: "requested" | "dry_run" | "applied" | "failed" | string;
  Message: string;
  Evidence?: Record<string, unknown>;
  CreatedAt: string;
}

export interface CostOpportunitiesResponse {
  opportunities: CostOpportunity[];
  actions?: CostAction[];
  latest_prs?: Record<string, IaCPullRequest>;
  summary?: {
    count: number;
    monthly_savings: number;
  };
}

export interface CostScanResponse {
  opportunities: CostOpportunity[];
  source: string;
  summary?: {
    count: number;
    monthly_savings: number;
  };
}

export interface CostApplyResult {
  OpportunityID: number;
  Provider: string;
  ResourceType: string;
  ResourceID: string;
  Name: string;
  Mode: string;
  Applied: boolean;
  Message: string;
}

export interface CostApplyResponse {
  result: CostApplyResult;
  opportunity: CostOpportunity;
  actor: string;
}

export interface IaCPullRequestResponse {
  opportunity?: CostOpportunity;
  recommendation?: Recommendation;
  workload?: Workload;
  pull_request: IaCPullRequest;
}

export interface GitHubRepository {
  alias?: string;
  repo: string;
  default_branch?: string;
  root_path?: string;
}

export interface GitHubIntegrationConfig {
  enabled: boolean;
  organization: string;
  token_env: string;
  default_repo: string;
  default_path: string;
  default_terraform_addr: string;
  repositories: GitHubRepository[];
}

export interface GitHubIntegrationResponse {
  config: GitHubIntegrationConfig;
  source: "store" | "default" | string;
  token_present: boolean;
}

export interface SavingsReportResponse {
  report: SavingsReport;
}

export interface SendReportResponse {
  report: SavingsReport;
  results: AlertDeliveryResult[];
}

export type ByOwnerRaw =
  | ByOwnerRow[]
  | Record<string, ByOwnerValue | number>;

export interface ByOwnerRow {
  owner?: string;
  name?: string;
  actor?: string;
  owner_name?: string;
  ownerName?: string;
  projected_monthly_savings?: number;
  projected_monthly?: number;
  projected_savings_monthly?: number;
  projected?: number;
  savings?: number;
  amount?: number;
  realized_monthly_savings?: number;
  realized_monthly?: number;
  realized_savings_monthly?: number;
  realized?: number;
  value?: number;
}

type ByOwnerValue = Omit<ByOwnerRow, "owner">;

/* Series endpoint — envelope, timestamp names and percentiles are all
 * feature-checked; `unit` labels the metric ("percent" for DB % metrics,
 * "millicores"/"bytes" for compute). */
export interface SeriesPoint {
  ts: string | number;
  p50?: number;
  p95?: number;
  p99?: number;
  max?: number;
}

export interface SeriesResponse {
  points?: SeriesPoint[];
  series?: SeriesPoint[];
  data?: SeriesPoint[];
  metric?: string;
  unit?: string;
  workload_id?: number;
  days?: number;
}

/* Auth — roles: viewer (read-only), operator (can apply),
 * admin (everything). */
export type UserRole = "viewer" | "operator" | "admin";

export interface User {
  id: number;
  email: string;
  name: string;
  role: UserRole | string;
}

export interface Team {
  ID: number;
  Slug: string;
  Name: string;
  Owner: string;
  OnCall: string;
  CreatedAt: string;
  UpdatedAt: string;
}

/* GET /auth/me — auth_enabled distinguishes "login required" (auth
 * enforced, no session yet) from "auth not enforced" (demo build);
 * needs_setup rides the 401 body and is true only while the users table
 * is empty — the first-run wizard's signal (ADR-037 §6 amendment). */
export interface MeResponse {
  auth_enabled: boolean;
  user: User | null;
  needs_setup?: boolean;
}

/* POST /auth/login — resolves with the user, or rejects 401 with
 * {"error":"invalid credentials"}. */
export interface LoginResponse {
  user: User;
}

/* POST /recommendations/{id}/apply result. */
export interface ApplyResult {
  EventID: number;
  DryRun: boolean;
  Applied: boolean;
  Diff?: ApplyDiff | null;
  StepNumber: number;
  TotalSteps: number;
  FollowUpID: number;
  Blocked: boolean;
  BlockReasons?: string[];
  InWindow?: boolean;
  Window?: string;
}

export interface ApplyPayload {
  mode: "dry_run" | "approved" | "auto";
  actor?: string;
}
