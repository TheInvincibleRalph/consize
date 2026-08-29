// Package store is the persistence boundary of Consize. The rest of the
// engine talks to the Store interface — never to SQL — which keeps every
// component testable with the in-memory implementation and swappable in
// the future (the audit trail gets INSERT-only tables in M2).
package store

import (
	"context"
	"time"
)

// Workload is a managed resource's current declared state, as recorded
// by the collector. Source distinguishes k8s workloads from databases
// (the M3 surface). The DBClass/DB* fields are meaningful only for
// source="db" workloads (ADR-030); k8s workloads leave them empty.
type Workload struct {
	ID              int64
	Name            string
	Namespace       string
	Kind            string
	Labels          map[string]string
	RequestCPUMilli int64
	LimitCPUMilli   int64
	RequestMemBytes int64
	LimitMemBytes   int64
	Source          string // k8s | db

	// DB-only fields (ADR-030), empty for k8s workloads.
	DBClass             string // e.g. db.t3.medium
	DBReplicas          int
	DBMaintenanceWindow string // UTC "ddd:hh:mm-ddd:hh:mm"
	DBProvider          string // aws | gcp | fixture

	// Team ownership (ADR-043). TeamID is zero when the workload has not
	// yet been assigned. The denormalized name/contact are read-only API
	// convenience fields populated by the store join.
	TeamID     int64
	TeamName   string
	TeamOnCall string
}

// Team is the ownership and escalation boundary for a group of workloads
// (ADR-043). Owner and OnCall are intentionally free-form contacts: an email,
// Slack channel, and incident-management schedule can all be represented
// without hard-wiring Consize to one external identity provider.
type Team struct {
	ID        int64
	Slug      string
	Name      string
	Owner     string
	OnCall    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Bucket is one 15-minute usage window, summarized by percentile.
// For v1 the collector samples at 15-minute steps, so P50/P95/P99/Max
// of a window are all the sampled point — sub-window sampling is a
// future refinement. The engine consumes P95 as the window's value.
type Bucket struct {
	WorkloadID  int64
	Metric      string // cpu_used_milli | mem_used_bytes
	WindowStart time.Time
	P50         float64
	P95         float64
	P99         float64
	Max         float64
	Samples     int
}

// Recommendation is one sized resource, persisted for the API and
// the apply engine (M2). Status lifecycle:
//
//	pending → applied → verified | rolled_back
//	pending → superseded   (a newer analysis replaced it)
//	pending → rejected     (operator declined)
type Recommendation struct {
	ID            int64
	WorkloadID    int64
	WorkloadName  string // denormalized for API convenience
	Namespace     string
	Resource      string // cpu | memory
	CurrentValue  int64
	ProposedValue int64
	// Limit pair from the policy: the apply engine patches request +
	// limit together. Equal values mean "limit unchanged" (limits only
	// decrease in v1, and downsize-only may leave a limit as-is).
	CurrentLimit   int64
	ProposedLimit  int64
	SavingsMonthly float64
	Confidence     float64
	PolicyVersion  string
	Status         string // pending | applied | verified | rolled_back | superseded | rejected
	CreatedAt      time.Time

	// Class pair for database recommendations (ADR-030): Resource is
	// "class" and these name the current/proposed instance class. Empty
	// for cpu/memory recommendations.
	ClassCurrent  string
	ClassProposed string
}

// Diff is one resource's before/after for a patch attempt — the
// evidence attached to every apply event. For database applies
// (Resource="class", ADR-030) the class pair names the change and the
// request/limit fields are zero.
type Diff struct {
	Resource      string `json:"resource"` // cpu | memory | class
	CurrentReq    int64  `json:"current_request"`
	ProposedReq   int64  `json:"proposed_request"`
	CurrentLimit  int64  `json:"current_limit"`
	ProposedLimit int64  `json:"proposed_limit"`
	ClassCurrent  string `json:"current_class"`
	ClassProposed string `json:"proposed_class"`
}

// ApplyEvent is one audit-trail row: one patch attempt (or dry-run
// plan). Rows are INSERT-only — outcomes are new rows, never edits —
// so the trail cannot be rewritten (ADR-008). Rollback is its own
// reverted event against the same recommendation.
type ApplyEvent struct {
	ID               int64
	RecommendationID int64
	WorkloadID       int64
	Actor            string // operator | auto | api:<user>
	Mode             string // dry_run | approved | auto
	Result           string // planned | applied | reverted
	Diff             Diff
	StepNumber       int
	TotalSteps       int
	CreatedAt        time.Time
}

// VerificationRun is the verdict for one applied event: SLIs compared
// over the baseline (pre-apply) and post windows, per the verifier.
type VerificationRun struct {
	ID            int64
	ApplyEventID  int64
	BaselineStart time.Time
	BaselineEnd   time.Time
	PostStart     time.Time
	PostEnd       time.Time
	Verdict       string // passed | failed | inconclusive
	SLIs          map[string]any
	Thresholds    map[string]any
	CreatedAt     time.Time
}

// CostOpportunity is an unmanaged cloud-cost finding discovered outside
// workload rightsizing: unattached disks, idle load balancers, unused NAT
// gateways, stopped instances, and similar resources that still accrue
// spend. It is intentionally separate from Recommendation because these
// findings do not have a workload_id or a verification window.
type CostOpportunity struct {
	ID             int64
	Provider       string
	Account        string
	Region         string
	ResourceType   string
	ResourceID     string
	Name           string
	MonthlyCost    float64
	Recommendation string
	Action         string
	Risk           string
	Status         string // open | pr_ready | pr_opened | resolved | dismissed
	Evidence       map[string]any
	IaCRepo        string
	IaCPath        string
	TerraformAddr  string
	FirstSeenAt    time.Time
	LastSeenAt     time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// IaCPullRequest is the audit record for a Terraform PR workflow. The MVP
// can return a planned PR/diff without pushing to a VCS provider; a future
// GitHub/GitLab adapter will use the same shape when credentials are present.
type IaCPullRequest struct {
	ID               int64
	OpportunityID    int64
	RecommendationID int64
	ChangeKind       string // recommendation | cost_opportunity
	Actor            string
	Provider         string
	Repo             string
	Branch           string
	Title            string
	Body             string
	Diff             string
	Status           string // planned | opened | failed
	URL              string
	Error            string
	CreatedAt        time.Time
}

// User is one operator account (ADR-037). Roles: viewer (read-only),
// operator (can approve and apply), admin (everything). Roles map to
// docs/security.md's consize:view / consize:operator / consize:admin.
// PasswordHash is bcrypt and must never leave the store via the API.
type User struct {
	ID           int64
	Email        string
	Name         string
	PasswordHash string
	Role         string // viewer | operator | admin
	CreatedAt    time.Time
}

// Session is one login. The raw token (32 random bytes) is handed to the
// client exactly once; only its SHA-256 hash is stored, so the sessions
// table is not a credential store and a leak cannot be replayed.
type Session struct {
	TokenHash string
	UserID    int64
	ExpiresAt time.Time
	CreatedAt time.Time
}

// Roles.
const (
	RoleViewer   = "viewer"
	RoleOperator = "operator"
	RoleAdmin    = "admin"
)

// Event results.
const (
	EventPlanned  = "planned"  // dry-run evaluated, nothing touched
	EventApplied  = "applied"  // patch sent to the cluster
	EventReverted = "reverted" // rollback patch sent
)

// Verification verdicts.
const (
	VerdictPassed       = "passed"
	VerdictFailed       = "failed"
	VerdictInconclusive = "inconclusive"
)

// Statuses.
const (
	StatusPending    = "pending"
	StatusApplied    = "applied"
	StatusVerified   = "verified"
	StatusRolled     = "rolled_back"
	StatusSuperseded = "superseded"
	StatusRejected   = "rejected"
)

// Cost opportunity statuses.
const (
	OpportunityOpen      = "open"
	OpportunityPRReady   = "pr_ready"
	OpportunityPROpened  = "pr_opened"
	OpportunityResolved  = "resolved"
	OpportunityDismissed = "dismissed"
)

// IaC PR statuses.
const (
	IaCPRPlanned = "planned"
	IaCPROpened  = "opened"
	IaCPRFailed  = "failed"
)

const (
	IaCChangeKindCostOpportunity = "cost_opportunity"
	IaCChangeKindRecommendation  = "recommendation"
)

// Resources. Class recommendations (ADR-030) name a database instance
// class change via ClassCurrent/ClassProposed instead of byte values.
const (
	ResourceCPU    = "cpu"
	ResourceMemory = "memory"
	ResourceClass  = "class"
)

// Metrics.
const (
	MetricCPUMilli = "cpu_used_milli"
	MetricMemBytes = "mem_used_bytes"

	// DB metrics (ADR-030): percents are measured directly; IOPS and
	// connections are absolute counts so the analyzer can divide by a
	// candidate class's catalog baselines.
	MetricDBCPUPercent  = "db_cpu_percent"
	MetricDBIOPS        = "db_iops"
	MetricDBConnections = "db_connections"
	MetricDBMemPercent  = "db_mem_percent"
	MetricDBErrors      = "db_errors" // counter, verifier SLI
)

// Store is the persistence contract.
type Store interface {
	// Health reports store availability. The API's /readyz gates on it:
	// applies must never run without the audit trail (fail-safe principle).
	Health(ctx context.Context) error

	// UpsertWorkload inserts or updates by (source, name, namespace) and
	// returns the workload ID.
	UpsertWorkload(ctx context.Context, w Workload) (int64, error)
	ListWorkloads(ctx context.Context) ([]Workload, error)
	GetWorkload(ctx context.Context, id int64) (Workload, error)
	// SetWorkloadTeam changes a workload's human ownership. A nil team ID
	// deliberately unassigns it; collection/upsert must preserve this choice.
	SetWorkloadTeam(ctx context.Context, workloadID int64, teamID *int64) error

	// Teams are a small, durable ownership directory. Slugs are stable API
	// identifiers; updates change contacts, not identity.
	CreateTeam(ctx context.Context, team Team) (Team, error)
	GetTeam(ctx context.Context, id int64) (Team, error)
	ListTeams(ctx context.Context) ([]Team, error)
	UpdateTeam(ctx context.Context, team Team) (Team, error)

	// UpsertBucket is idempotent on (workload_id, metric, window_start).
	UpsertBucket(ctx context.Context, b Bucket) error
	// ListBuckets returns buckets for a workload/metric within a window,
	// ordered by window_start.
	ListBuckets(ctx context.Context, workloadID int64, metric string, from, to time.Time) ([]Bucket, error)
	// LatestBucketTime returns the newest telemetry window in the store.
	// ok is false when collection has not produced any buckets yet.
	LatestBucketTime(ctx context.Context) (t time.Time, ok bool, err error)

	// CreateRecommendations persists a batch and supersedes prior pending
	// recommendations for the same (workload, resource).
	CreateRecommendations(ctx context.Context, recs []Recommendation) error
	// ListRecommendations filters by workload and/or status ("" = all,
	// nil workload = all workloads). Ordered by savings descending.
	// limit <= 0 means no limit; offset applies before limit. Returns the
	// page and the total count matching the filters (before slicing), so
	// clients can render "showing N of M".
	ListRecommendations(ctx context.Context, workloadID *int64, status string, limit, offset int) ([]Recommendation, int, error)
	// PruneRecommendations deletes recommendations with the given status
	// created before cutoff (housekeeping: superseded rows are replaced by
	// construction — the audit of what was *applied* lives in apply_events
	// and verification_runs, which are never pruned). Returns the number
	// of rows removed.
	PruneRecommendations(ctx context.Context, status string, cutoff time.Time) (int64, error)
	// GetRecommendation loads one recommendation by ID.
	GetRecommendation(ctx context.Context, id int64) (Recommendation, error)
	// SetRecommendationStatus transitions a recommendation's status
	// (pending → applied → verified | rolled_back).
	SetRecommendationStatus(ctx context.Context, id int64, status string) error
	// CreateFollowUpRecommendation inserts a pending continuation of a
	// stepped apply WITHOUT superseding other pending recommendations —
	// the follow-up represents an in-flight step plan, not a fresh
	// analysis, so it must not invalidate newer analysis output.
	CreateFollowUpRecommendation(ctx context.Context, r Recommendation) (int64, error)

	// SavingsSummary returns the projected monthly savings across all
	// active (pending) recommendations.
	SavingsSummary(ctx context.Context) (projectedMonthly float64, activeCount int, err error)

	// --- M2 audit trail (INSERT-only; apply engine + verifier) ---

	// CreateApplyEvent records one patch attempt or plan; returns the ID.
	CreateApplyEvent(ctx context.Context, e ApplyEvent) (int64, error)
	GetApplyEvent(ctx context.Context, id int64) (ApplyEvent, error)
	// ListApplyEvents filters by workload and/or result ("" = all).
	// Ordered newest first.
	ListApplyEvents(ctx context.Context, workloadID *int64, result string) ([]ApplyEvent, error)
	// ActiveApplyInNamespace reports whether the namespace has an applied
	// event whose verification has not concluded (in-flight apply).
	ActiveApplyInNamespace(ctx context.Context, namespace string) (bool, error)
	// ActiveApplyCount is the global in-flight count.
	ActiveApplyCount(ctx context.Context) (int, error)
	// AppliedEventsUnverified lists applied events with no verification
	// run yet — the cmd/verify work queue (derived, never stored).
	AppliedEventsUnverified(ctx context.Context) ([]ApplyEvent, error)

	// CreateVerificationRun records one verdict. One per apply event
	// (unique on apply_event_id).
	CreateVerificationRun(ctx context.Context, v VerificationRun) error
	ListVerificationRuns(ctx context.Context, applyEventID *int64) ([]VerificationRun, error)

	// --- Auth (ADR-037) ---

	// CreateUser inserts a user; idempotent on email (an existing row is
	// returned, never duplicated).
	CreateUser(ctx context.Context, u User) (User, error)
	// GetUserByEmail loads one user; ErrNotFound when absent.
	GetUserByEmail(ctx context.Context, email string) (User, error)
	// GetUser loads one user by ID; ErrNotFound when absent.
	GetUser(ctx context.Context, id int64) (User, error)
	// CountUsers reports the number of users (the bootstrap-admin gate:
	// the first admin is created only when the table is empty).
	CountUsers(ctx context.Context) (int, error)

	// CreateSession stores one session token hash.
	CreateSession(ctx context.Context, s Session) error
	// GetSessionByTokenHash loads the session by token hash, deleting and
	// returning ErrNotFound when it is absent or expired.
	GetSessionByTokenHash(ctx context.Context, tokenHash string) (Session, error)
	// DeleteSession revokes a session (logout).
	DeleteSession(ctx context.Context, tokenHash string) error

	// --- Installation settings ---

	// GetSetting loads a durable installation setting. ok=false means unset.
	GetSetting(ctx context.Context, key string) (value string, ok bool, err error)
	// PutSetting upserts a durable installation setting. Secrets must be
	// referenced, not stored as values.
	PutSetting(ctx context.Context, key, value string) error

	// --- Unmanaged cloud-cost opportunities ---

	UpsertCostOpportunities(ctx context.Context, opportunities []CostOpportunity) error
	ListCostOpportunities(ctx context.Context, status string) ([]CostOpportunity, error)
	GetCostOpportunity(ctx context.Context, id int64) (CostOpportunity, error)
	CreateIaCPullRequest(ctx context.Context, pr IaCPullRequest) (IaCPullRequest, error)
	ListIaCPullRequests(ctx context.Context, opportunityID *int64) ([]IaCPullRequest, error)
}
