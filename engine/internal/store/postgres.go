package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store sentinels are mapped to stable API status codes at the boundary.
var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

// Postgres implements Store on a Postgres database.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres opens a pool against DATABASE_URL-style connection string.
func NewPostgres(ctx context.Context, connString string) (*Postgres, error) {
	cfg, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, fmt.Errorf("parse connection string: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Postgres{pool: pool}, nil
}

// NewPostgresFromPool wraps an existing pool (tests, migration runners).
func NewPostgresFromPool(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

// Close releases the pool.
func (p *Postgres) Close() { p.pool.Close() }

func (p *Postgres) Health(ctx context.Context) error {
	return p.pool.Ping(ctx)
}

func (p *Postgres) UpsertWorkload(ctx context.Context, w Workload) (int64, error) {
	labels := map[string]any{}
	for k, v := range w.Labels {
		labels[k] = v
	}
	const q = `
		INSERT INTO workloads
			(name, namespace, kind, labels, request_cpu_milli, limit_cpu_milli,
			 request_mem_bytes, limit_mem_bytes, source,
			 db_class, db_replicas, db_maintenance_window, db_provider)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (source, name, namespace) DO UPDATE SET
			kind = EXCLUDED.kind,
			labels = EXCLUDED.labels,
			request_cpu_milli = EXCLUDED.request_cpu_milli,
			limit_cpu_milli = EXCLUDED.limit_cpu_milli,
			request_mem_bytes = EXCLUDED.request_mem_bytes,
			limit_mem_bytes = EXCLUDED.limit_mem_bytes,
			db_class = EXCLUDED.db_class,
			db_replicas = EXCLUDED.db_replicas,
			db_maintenance_window = EXCLUDED.db_maintenance_window,
			db_provider = EXCLUDED.db_provider,
			updated_at = now()
		RETURNING id`
	var id int64
	err := p.pool.QueryRow(ctx, q,
		w.Name, w.Namespace, w.Kind, labels,
		w.RequestCPUMilli, w.LimitCPUMilli,
		w.RequestMemBytes, w.LimitMemBytes, w.Source,
		w.DBClass, w.DBReplicas, w.DBMaintenanceWindow, w.DBProvider,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert workload %s/%s: %w", w.Namespace, w.Name, err)
	}
	return id, nil
}

const workloadCols = `w.id, w.name, w.namespace, w.kind, w.labels, w.request_cpu_milli, w.limit_cpu_milli,
	w.request_mem_bytes, w.limit_mem_bytes, w.source,
	w.db_class, w.db_replicas, w.db_maintenance_window, w.db_provider,
	COALESCE(w.team_id, 0), COALESCE(t.name, ''), COALESCE(t.on_call, '')`

func scanWorkload(row pgx.Row) (Workload, error) {
	var w Workload
	var labels map[string]any
	if err := row.Scan(&w.ID, &w.Name, &w.Namespace, &w.Kind, &labels,
		&w.RequestCPUMilli, &w.LimitCPUMilli, &w.RequestMemBytes, &w.LimitMemBytes, &w.Source,
		&w.DBClass, &w.DBReplicas, &w.DBMaintenanceWindow, &w.DBProvider,
		&w.TeamID, &w.TeamName, &w.TeamOnCall); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Workload{}, ErrNotFound
		}
		return Workload{}, err
	}
	w.Labels = map[string]string{}
	for k, v := range labels {
		if s, ok := v.(string); ok {
			w.Labels[k] = s
		}
	}
	return w, nil
}

func (p *Postgres) ListWorkloads(ctx context.Context) ([]Workload, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+workloadCols+` FROM workloads w LEFT JOIN teams t ON t.id = w.team_id ORDER BY w.namespace, w.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Workload
	for rows.Next() {
		w, err := scanWorkload(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (p *Postgres) GetWorkload(ctx context.Context, id int64) (Workload, error) {
	return scanWorkload(p.pool.QueryRow(ctx, `SELECT `+workloadCols+` FROM workloads w LEFT JOIN teams t ON t.id = w.team_id WHERE w.id = $1`, id))
}

func (p *Postgres) SetWorkloadTeam(ctx context.Context, workloadID int64, teamID *int64) error {
	var id any
	if teamID != nil {
		id = *teamID
	}
	tag, err := p.pool.Exec(ctx, `UPDATE workloads SET team_id = $2, updated_at = now() WHERE id = $1`, workloadID, id)
	if err != nil {
		return fmt.Errorf("set workload team: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("workload %d: %w", workloadID, ErrNotFound)
	}
	return nil
}

func (p *Postgres) CreateTeam(ctx context.Context, team Team) (Team, error) {
	err := p.pool.QueryRow(ctx, `
		INSERT INTO teams (slug, name, owner, on_call)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (slug) DO NOTHING
		RETURNING id, created_at, updated_at`,
		team.Slug, team.Name, team.Owner, team.OnCall).Scan(&team.ID, &team.CreatedAt, &team.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Team{}, fmt.Errorf("team %q: %w", team.Slug, ErrConflict)
	}
	if err != nil {
		return Team{}, fmt.Errorf("create team: %w", err)
	}
	return team, nil
}

func (p *Postgres) GetTeam(ctx context.Context, id int64) (Team, error) {
	return scanTeam(p.pool.QueryRow(ctx, `SELECT id, slug, name, owner, on_call, created_at, updated_at FROM teams WHERE id = $1`, id))
}

func (p *Postgres) ListTeams(ctx context.Context) ([]Team, error) {
	rows, err := p.pool.Query(ctx, `SELECT id, slug, name, owner, on_call, created_at, updated_at FROM teams ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Team{}
	for rows.Next() {
		t, err := scanTeam(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (p *Postgres) UpdateTeam(ctx context.Context, team Team) (Team, error) {
	err := p.pool.QueryRow(ctx, `
		UPDATE teams SET owner = $2, on_call = $3, updated_at = now()
		WHERE id = $1
		RETURNING slug, name, created_at, updated_at`, team.ID, team.Owner, team.OnCall).
		Scan(&team.Slug, &team.Name, &team.CreatedAt, &team.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Team{}, fmt.Errorf("team %d: %w", team.ID, ErrNotFound)
	}
	if err != nil {
		return Team{}, fmt.Errorf("update team: %w", err)
	}
	return team, nil
}

func scanTeam(row pgx.Row) (Team, error) {
	var team Team
	if err := row.Scan(&team.ID, &team.Slug, &team.Name, &team.Owner, &team.OnCall, &team.CreatedAt, &team.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Team{}, ErrNotFound
		}
		return Team{}, err
	}
	return team, nil
}

func (p *Postgres) UpsertBucket(ctx context.Context, b Bucket) error {
	const q = `
		INSERT INTO usage_buckets
			(workload_id, metric, window_start, p50, p95, p99, max_value, sample_count)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (workload_id, metric, window_start) DO UPDATE SET
			p50 = EXCLUDED.p50, p95 = EXCLUDED.p95, p99 = EXCLUDED.p99,
			max_value = EXCLUDED.max_value, sample_count = EXCLUDED.sample_count`
	_, err := p.pool.Exec(ctx, q,
		b.WorkloadID, b.Metric, b.WindowStart.UTC(),
		b.P50, b.P95, b.P99, b.Max, b.Samples)
	if err != nil {
		return fmt.Errorf("upsert bucket %d/%s/%s: %w", b.WorkloadID, b.Metric, b.WindowStart, err)
	}
	return nil
}

func (p *Postgres) ListBuckets(ctx context.Context, workloadID int64, metric string, from, to time.Time) ([]Bucket, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT workload_id, metric, window_start, p50, p95, p99, max_value, sample_count
		FROM usage_buckets
		WHERE workload_id = $1 AND metric = $2 AND window_start >= $3 AND window_start <= $4
		ORDER BY window_start`,
		workloadID, metric, from.UTC(), to.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Bucket
	for rows.Next() {
		var b Bucket
		if err := rows.Scan(&b.WorkloadID, &b.Metric, &b.WindowStart,
			&b.P50, &b.P95, &b.P99, &b.Max, &b.Samples); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (p *Postgres) LatestBucketTime(ctx context.Context) (time.Time, bool, error) {
	var latest *time.Time
	if err := p.pool.QueryRow(ctx, `SELECT max(window_start) FROM usage_buckets`).Scan(&latest); err != nil {
		return time.Time{}, false, err
	}
	if latest == nil || latest.IsZero() {
		return time.Time{}, false, nil
	}
	return latest.UTC(), true, nil
}

func (p *Postgres) CreateRecommendations(ctx context.Context, recs []Recommendation) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	for _, r := range recs {
		r = normalizeRecommendationSteps(r)
		if _, err := tx.Exec(ctx, `
			UPDATE recommendations SET status = $1
			WHERE workload_id = $2 AND resource = $3 AND status = $4`,
			StatusSuperseded, r.WorkloadID, r.Resource, StatusPending); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO recommendations
				(workload_id, resource, current_value, proposed_value,
				 current_limit, proposed_limit,
				 savings_monthly, confidence, policy_version, status,
				 class_current, class_proposed, step_number, total_steps)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
			r.WorkloadID, r.Resource, r.CurrentValue, r.ProposedValue,
			r.CurrentLimit, r.ProposedLimit,
			r.SavingsMonthly, r.Confidence, r.PolicyVersion, orDefault(r.Status, StatusPending),
			r.ClassCurrent, r.ClassProposed, r.StepNumber, r.TotalSteps); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

const recCols = `r.id, r.workload_id, w.name, w.namespace, r.resource,
	r.current_value, r.proposed_value, r.current_limit, r.proposed_limit,
	r.savings_monthly, r.confidence, r.policy_version, r.status, r.created_at,
	r.class_current, r.class_proposed, r.step_number, r.total_steps`

func scanRecommendation(row pgx.Row) (Recommendation, error) {
	var r Recommendation
	if err := row.Scan(&r.ID, &r.WorkloadID, &r.WorkloadName, &r.Namespace,
		&r.Resource, &r.CurrentValue, &r.ProposedValue, &r.CurrentLimit, &r.ProposedLimit,
		&r.SavingsMonthly, &r.Confidence, &r.PolicyVersion, &r.Status, &r.CreatedAt,
		&r.ClassCurrent, &r.ClassProposed, &r.StepNumber, &r.TotalSteps); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Recommendation{}, ErrNotFound
		}
		return Recommendation{}, err
	}
	return r, nil
}

func (p *Postgres) ListRecommendations(ctx context.Context, workloadID *int64, status string, limit, offset int) ([]Recommendation, int, error) {
	where := ` WHERE 1=1`
	args := []any{}
	if workloadID != nil {
		args = append(args, *workloadID)
		where += fmt.Sprintf(" AND r.workload_id = $%d", len(args))
	}
	if status != "" {
		args = append(args, status)
		where += fmt.Sprintf(" AND r.status = $%d", len(args))
	}

	var total int
	if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM recommendations r
		JOIN workloads w ON w.id = r.workload_id`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	q := `SELECT ` + recCols + ` FROM recommendations r
		JOIN workloads w ON w.id = r.workload_id` + where + ` ORDER BY r.savings_monthly DESC`
	if limit > 0 {
		args = append(args, limit, offset)
		q += fmt.Sprintf(` LIMIT $%d OFFSET $%d`, len(args)-1, len(args))
	} else if offset > 0 {
		args = append(args, offset)
		q += fmt.Sprintf(` OFFSET $%d`, len(args))
	}

	rows, err := p.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Recommendation
	for rows.Next() {
		r, err := scanRecommendation(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

func (p *Postgres) PruneRecommendations(ctx context.Context, status string, cutoff time.Time) (int64, error) {
	tag, err := p.pool.Exec(ctx,
		`DELETE FROM recommendations WHERE status = $1 AND created_at < $2`, status, cutoff)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (p *Postgres) CreateFollowUpRecommendation(ctx context.Context, r Recommendation) (int64, error) {
	r = normalizeRecommendationSteps(r)
	const q = `
		INSERT INTO recommendations
			(workload_id, resource, current_value, proposed_value,
			 current_limit, proposed_limit,
			 savings_monthly, confidence, policy_version, status,
			 class_current, class_proposed, step_number, total_steps)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		RETURNING id`
	var id int64
	err := p.pool.QueryRow(ctx, q,
		r.WorkloadID, r.Resource, r.CurrentValue, r.ProposedValue,
		r.CurrentLimit, r.ProposedLimit,
		r.SavingsMonthly, r.Confidence, r.PolicyVersion, orDefault(r.Status, StatusPending),
		r.ClassCurrent, r.ClassProposed, r.StepNumber, r.TotalSteps).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("create follow-up recommendation: %w", err)
	}
	return id, nil
}

func (p *Postgres) GetRecommendation(ctx context.Context, id int64) (Recommendation, error) {
	return scanRecommendation(p.pool.QueryRow(ctx,
		`SELECT `+recCols+` FROM recommendations r
		JOIN workloads w ON w.id = r.workload_id WHERE r.id = $1`, id))
}

func (p *Postgres) SetRecommendationStatus(ctx context.Context, id int64, status string) error {
	tag, err := p.pool.Exec(ctx, `UPDATE recommendations SET status = $2 WHERE id = $1`, id, status)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("recommendation %d: %w", id, ErrNotFound)
	}
	return nil
}

func (p *Postgres) SavingsSummary(ctx context.Context) (float64, int, error) {
	var total float64
	var n int
	err := p.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(savings_monthly), 0), COUNT(*)
		FROM recommendations WHERE status = $1`, StatusPending).Scan(&total, &n)
	return total, n, err
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func normalizeRecommendationSteps(r Recommendation) Recommendation {
	if r.StepNumber <= 0 {
		r.StepNumber = 1
	}
	if r.TotalSteps < 0 {
		r.TotalSteps = 0
	}
	return r
}

// --- M2 audit trail ---

func (p *Postgres) CreateApplyEvent(ctx context.Context, e ApplyEvent) (int64, error) {
	diffJSON, err := json.Marshal(e.Diff)
	if err != nil {
		return 0, err
	}
	const q = `
		INSERT INTO apply_events
			(recommendation_id, workload_id, actor, mode, result, diff, step_number, total_steps)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id`
	var id int64
	if err := p.pool.QueryRow(ctx, q,
		e.RecommendationID, e.WorkloadID, e.Actor, e.Mode, e.Result,
		diffJSON, e.StepNumber, e.TotalSteps).Scan(&id); err != nil {
		return 0, fmt.Errorf("create apply event: %w", err)
	}
	return id, nil
}

func (p *Postgres) GetApplyEvent(ctx context.Context, id int64) (ApplyEvent, error) {
	e, err := scanApplyEvent(p.pool.QueryRow(ctx,
		`SELECT id, recommendation_id, workload_id, actor, mode, result, diff,
		        step_number, total_steps, created_at
		 FROM apply_events WHERE id = $1`, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ApplyEvent{}, ErrNotFound
		}
		return ApplyEvent{}, err
	}
	return e, nil
}

func (p *Postgres) ListApplyEvents(ctx context.Context, workloadID *int64, result string) ([]ApplyEvent, error) {
	q := `SELECT id, recommendation_id, workload_id, actor, mode, result, diff,
	        step_number, total_steps, created_at
	      FROM apply_events WHERE 1=1`
	args := []any{}
	if workloadID != nil {
		args = append(args, *workloadID)
		q += fmt.Sprintf(" AND workload_id = $%d", len(args))
	}
	if result != "" {
		args = append(args, result)
		q += fmt.Sprintf(" AND result = $%d", len(args))
	}
	q += ` ORDER BY created_at DESC`

	rows, err := p.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ApplyEvent{}
	for rows.Next() {
		e, err := scanApplyEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func scanApplyEvent(row pgx.Row) (ApplyEvent, error) {
	var e ApplyEvent
	var diffJSON []byte
	if err := row.Scan(&e.ID, &e.RecommendationID, &e.WorkloadID, &e.Actor,
		&e.Mode, &e.Result, &diffJSON, &e.StepNumber, &e.TotalSteps, &e.CreatedAt); err != nil {
		return ApplyEvent{}, err
	}
	if err := json.Unmarshal(diffJSON, &e.Diff); err != nil {
		return ApplyEvent{}, fmt.Errorf("decode diff: %w", err)
	}
	return e, nil
}

// ActiveApplyInNamespace reports an in-flight apply: an applied event
// with no verification run. Derived, never stored — a crash mid-verify
// leaves a retryable state, not a lie.
func (p *Postgres) ActiveApplyInNamespace(ctx context.Context, namespace string) (bool, error) {
	var active bool
	err := p.pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT EXISTS(
			SELECT 1 FROM apply_events ae
			JOIN workloads w ON w.id = ae.workload_id
			WHERE w.namespace = $1 AND ae.result = 'applied'
			  AND NOT EXISTS(SELECT 1 FROM verification_runs vr WHERE vr.apply_event_id = ae.id))`,
	), namespace).Scan(&active)
	return active, err
}

func (p *Postgres) ActiveApplyCount(ctx context.Context) (int, error) {
	var n int
	err := p.pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT COUNT(*) FROM apply_events ae
		WHERE ae.result = 'applied'
		  AND NOT EXISTS(SELECT 1 FROM verification_runs vr WHERE vr.apply_event_id = ae.id)`,
	)).Scan(&n)
	return n, err
}

func (p *Postgres) AppliedEventsUnverified(ctx context.Context) ([]ApplyEvent, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT ae.id, ae.recommendation_id, ae.workload_id, ae.actor, ae.mode,
		       ae.result, ae.diff, ae.step_number, ae.total_steps, ae.created_at
		FROM apply_events ae
		WHERE ae.result = 'applied'
		  AND NOT EXISTS(SELECT 1 FROM verification_runs vr WHERE vr.apply_event_id = ae.id)
		ORDER BY ae.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ApplyEvent{}
	for rows.Next() {
		e, err := scanApplyEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (p *Postgres) CreateVerificationRun(ctx context.Context, v VerificationRun) error {
	slisJSON, err := json.Marshal(v.SLIs)
	if err != nil {
		return err
	}
	thrJSON, err := json.Marshal(v.Thresholds)
	if err != nil {
		return err
	}
	const q = `
		INSERT INTO verification_runs
			(apply_event_id, baseline_start, baseline_end, post_start, post_end,
			 verdict, slis, thresholds)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (apply_event_id) DO UPDATE SET
			verdict = EXCLUDED.verdict, slis = EXCLUDED.slis, thresholds = EXCLUDED.thresholds`
	_, err = p.pool.Exec(ctx, q,
		v.ApplyEventID, v.BaselineStart.UTC(), v.BaselineEnd.UTC(),
		v.PostStart.UTC(), v.PostEnd.UTC(), v.Verdict, slisJSON, thrJSON)
	return err
}

func (p *Postgres) ListVerificationRuns(ctx context.Context, applyEventID *int64) ([]VerificationRun, error) {
	q := `SELECT id, apply_event_id, baseline_start, baseline_end, post_start, post_end,
	        verdict, slis, thresholds, created_at
	      FROM verification_runs WHERE 1=1`
	args := []any{}
	if applyEventID != nil {
		args = append(args, *applyEventID)
		q += fmt.Sprintf(" AND apply_event_id = $%d", len(args))
	}
	q += ` ORDER BY created_at DESC`

	rows, err := p.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []VerificationRun{}
	for rows.Next() {
		var v VerificationRun
		var slisJSON, thrJSON []byte
		if err := rows.Scan(&v.ID, &v.ApplyEventID,
			&v.BaselineStart, &v.BaselineEnd, &v.PostStart, &v.PostEnd,
			&v.Verdict, &slisJSON, &thrJSON, &v.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(slisJSON, &v.SLIs); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(thrJSON, &v.Thresholds); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (p *Postgres) CreateUser(ctx context.Context, u User) (User, error) {
	// Keep the returned value canonical too. Postgres receives the
	// lower-cased argument below, but without assigning it back the caller
	// would see different email casing from the Memory implementation.
	u.Email = strings.ToLower(u.Email)
	const q = `
		INSERT INTO users (email, name, password_hash, role)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (email) DO UPDATE SET email = EXCLUDED.email
		RETURNING id, created_at`
	err := p.pool.QueryRow(ctx, q, u.Email, u.Name, u.PasswordHash, u.Role).
		Scan(&u.ID, &u.CreatedAt)
	return u, err
}

func (p *Postgres) GetUserByEmail(ctx context.Context, email string) (User, error) {
	return p.scanUser(p.pool.QueryRow(ctx,
		`SELECT id, email, name, password_hash, role, created_at FROM users WHERE email = $1`,
		strings.ToLower(email)))
}

func (p *Postgres) GetUser(ctx context.Context, id int64) (User, error) {
	return p.scanUser(p.pool.QueryRow(ctx,
		`SELECT id, email, name, password_hash, role, created_at FROM users WHERE id = $1`, id))
}

func (p *Postgres) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := p.pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, err
}

func (p *Postgres) scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.Name, &u.PasswordHash, &u.Role, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

func (p *Postgres) CreateSession(ctx context.Context, s Session) error {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`,
		s.TokenHash, s.UserID, s.ExpiresAt.UTC())
	return err
}

func (p *Postgres) GetSessionByTokenHash(ctx context.Context, tokenHash string) (Session, error) {
	var s Session
	q := `DELETE FROM sessions WHERE expires_at <= now() RETURNING token_hash`
	// Opportunistic expiry sweep on the read path keeps the table bounded
	// without a dedicated janitor; the query below re-checks expiry anyway.
	rows, err := p.pool.Query(ctx, q)
	if err != nil {
		return Session{}, err
	}
	rows.Close()

	err = p.pool.QueryRow(ctx,
		`SELECT token_hash, user_id, expires_at, created_at FROM sessions
		 WHERE token_hash = $1 AND expires_at > now()`, tokenHash).
		Scan(&s.TokenHash, &s.UserID, &s.ExpiresAt, &s.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	return s, err
}

func (p *Postgres) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, tokenHash)
	return err
}

func (p *Postgres) GetSetting(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := p.pool.QueryRow(ctx, `SELECT value FROM app_settings WHERE key = $1`, key).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

func (p *Postgres) PutSetting(ctx context.Context, key, value string) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO app_settings (key, value)
		VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
		key, value)
	return err
}

func (p *Postgres) UpsertCostOpportunities(ctx context.Context, opportunities []CostOpportunity) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, o := range opportunities {
		evidenceJSON, err := json.Marshal(o.Evidence)
		if err != nil {
			return fmt.Errorf("encode cost opportunity evidence: %w", err)
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO cost_opportunities
				(provider, account, region, resource_type, resource_id, name,
				 monthly_cost, recommendation, action, risk, status, evidence,
				 iac_repo, iac_path, terraform_addr, first_seen_at, last_seen_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
			        $13, $14, $15, COALESCE(NULLIF($16::timestamptz, '0001-01-01'::timestamptz), now()),
			        COALESCE(NULLIF($17::timestamptz, '0001-01-01'::timestamptz), now()))
			ON CONFLICT (provider, account, region, resource_type, resource_id) DO UPDATE SET
				name = EXCLUDED.name,
				monthly_cost = EXCLUDED.monthly_cost,
				recommendation = EXCLUDED.recommendation,
				action = EXCLUDED.action,
				risk = EXCLUDED.risk,
				evidence = EXCLUDED.evidence,
				iac_repo = EXCLUDED.iac_repo,
				iac_path = EXCLUDED.iac_path,
				terraform_addr = EXCLUDED.terraform_addr,
				last_seen_at = EXCLUDED.last_seen_at,
				updated_at = now()`,
			o.Provider, o.Account, o.Region, o.ResourceType, o.ResourceID, o.Name,
			o.MonthlyCost, o.Recommendation, o.Action, orDefault(o.Risk, "low"),
			orDefault(o.Status, OpportunityOpen), evidenceJSON,
			o.IaCRepo, o.IaCPath, o.TerraformAddr, o.FirstSeenAt.UTC(), o.LastSeenAt.UTC())
		if err != nil {
			return fmt.Errorf("upsert cost opportunity %s/%s: %w", o.ResourceType, o.ResourceID, err)
		}
	}
	return tx.Commit(ctx)
}

const costOpportunityCols = `id, provider, account, region, resource_type, resource_id, name,
	monthly_cost, recommendation, action, risk, status, evidence,
	iac_repo, iac_path, terraform_addr, first_seen_at, last_seen_at, created_at, updated_at`

func scanCostOpportunity(row pgx.Row) (CostOpportunity, error) {
	var o CostOpportunity
	var evidenceJSON []byte
	if err := row.Scan(&o.ID, &o.Provider, &o.Account, &o.Region, &o.ResourceType, &o.ResourceID, &o.Name,
		&o.MonthlyCost, &o.Recommendation, &o.Action, &o.Risk, &o.Status, &evidenceJSON,
		&o.IaCRepo, &o.IaCPath, &o.TerraformAddr, &o.FirstSeenAt, &o.LastSeenAt, &o.CreatedAt, &o.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return CostOpportunity{}, ErrNotFound
		}
		return CostOpportunity{}, err
	}
	if len(evidenceJSON) > 0 {
		if err := json.Unmarshal(evidenceJSON, &o.Evidence); err != nil {
			return CostOpportunity{}, fmt.Errorf("decode cost opportunity evidence: %w", err)
		}
	}
	if o.Evidence == nil {
		o.Evidence = map[string]any{}
	}
	return o, nil
}

func (p *Postgres) ListCostOpportunities(ctx context.Context, status string) ([]CostOpportunity, error) {
	q := `SELECT ` + costOpportunityCols + ` FROM cost_opportunities WHERE 1=1`
	args := []any{}
	if status != "" {
		args = append(args, status)
		q += fmt.Sprintf(" AND status = $%d", len(args))
	}
	q += ` ORDER BY monthly_cost DESC, last_seen_at DESC`
	rows, err := p.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CostOpportunity{}
	for rows.Next() {
		o, err := scanCostOpportunity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (p *Postgres) GetCostOpportunity(ctx context.Context, id int64) (CostOpportunity, error) {
	return scanCostOpportunity(p.pool.QueryRow(ctx, `SELECT `+costOpportunityCols+` FROM cost_opportunities WHERE id = $1`, id))
}

func (p *Postgres) SetCostOpportunityStatus(ctx context.Context, id int64, status string) error {
	tag, err := p.pool.Exec(ctx, `UPDATE cost_opportunities SET status = $2, updated_at = now() WHERE id = $1`, id, status)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) CreateCostAction(ctx context.Context, action CostAction) (CostAction, error) {
	if action.Evidence == nil {
		action.Evidence = map[string]any{}
	}
	evidenceJSON, err := json.Marshal(action.Evidence)
	if err != nil {
		return action, fmt.Errorf("encode cost action evidence: %w", err)
	}
	action.Mode = orDefault(action.Mode, "dry_run")
	action.Result = orDefault(action.Result, CostActionRequested)
	if err := p.pool.QueryRow(ctx, `
		INSERT INTO cost_actions
			(opportunity_id, actor, mode, result, message, evidence)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, created_at`,
		action.OpportunityID, action.Actor, action.Mode, action.Result, action.Message, evidenceJSON,
	).Scan(&action.ID, &action.CreatedAt); err != nil {
		return action, fmt.Errorf("create cost action: %w", err)
	}
	return action, nil
}

func (p *Postgres) ListCostActions(ctx context.Context, opportunityID *int64) ([]CostAction, error) {
	q := `SELECT id, opportunity_id, actor, mode, result, message, evidence, created_at
	      FROM cost_actions WHERE 1=1`
	args := []any{}
	if opportunityID != nil {
		args = append(args, *opportunityID)
		q += fmt.Sprintf(" AND opportunity_id = $%d", len(args))
	}
	q += ` ORDER BY created_at DESC`
	rows, err := p.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CostAction{}
	for rows.Next() {
		var action CostAction
		var evidenceJSON []byte
		if err := rows.Scan(&action.ID, &action.OpportunityID, &action.Actor, &action.Mode, &action.Result, &action.Message, &evidenceJSON, &action.CreatedAt); err != nil {
			return nil, err
		}
		if len(evidenceJSON) > 0 {
			if err := json.Unmarshal(evidenceJSON, &action.Evidence); err != nil {
				return nil, fmt.Errorf("decode cost action evidence: %w", err)
			}
		}
		if action.Evidence == nil {
			action.Evidence = map[string]any{}
		}
		out = append(out, action)
	}
	return out, rows.Err()
}

func (p *Postgres) CreateIaCPullRequest(ctx context.Context, pr IaCPullRequest) (IaCPullRequest, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return pr, err
	}
	defer tx.Rollback(ctx)
	changeKind := orDefault(pr.ChangeKind, IaCChangeKindCostOpportunity)
	pr.ChangeKind = changeKind
	pr.Provider = orDefault(pr.Provider, "terraform")
	pr.Status = orDefault(pr.Status, IaCPRPlanned)
	if err := tx.QueryRow(ctx, `
		INSERT INTO iac_pull_requests
			(opportunity_id, recommendation_id, change_kind, actor, provider, repo, branch, title, body, diff, status, url, error)
		VALUES (NULLIF($1, 0), NULLIF($2, 0), $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING id, created_at`,
		pr.OpportunityID, pr.RecommendationID, pr.ChangeKind, pr.Actor, pr.Provider, pr.Repo, pr.Branch,
		pr.Title, pr.Body, pr.Diff, pr.Status, pr.URL, pr.Error,
	).Scan(&pr.ID, &pr.CreatedAt); err != nil {
		return pr, fmt.Errorf("create iac pull request: %w", err)
	}
	if pr.OpportunityID > 0 {
		nextStatus := OpportunityPRReady
		if pr.Status == IaCPROpened {
			nextStatus = OpportunityPROpened
		}
		if _, err := tx.Exec(ctx, `UPDATE cost_opportunities SET status = $2, updated_at = now() WHERE id = $1`, pr.OpportunityID, nextStatus); err != nil {
			return pr, err
		}
	}
	return pr, tx.Commit(ctx)
}

func (p *Postgres) ListIaCPullRequests(ctx context.Context, opportunityID *int64) ([]IaCPullRequest, error) {
	q := `SELECT id, COALESCE(opportunity_id, 0), COALESCE(recommendation_id, 0), change_kind,
		  actor, provider, repo, branch, title, body, diff, status, url, error, created_at
	      FROM iac_pull_requests WHERE 1=1`
	args := []any{}
	if opportunityID != nil {
		args = append(args, *opportunityID)
		q += fmt.Sprintf(" AND opportunity_id = $%d", len(args))
	}
	q += ` ORDER BY created_at DESC`
	rows, err := p.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []IaCPullRequest{}
	for rows.Next() {
		var pr IaCPullRequest
		if err := rows.Scan(&pr.ID, &pr.OpportunityID, &pr.RecommendationID, &pr.ChangeKind,
			&pr.Actor, &pr.Provider, &pr.Repo, &pr.Branch,
			&pr.Title, &pr.Body, &pr.Diff, &pr.Status, &pr.URL, &pr.Error, &pr.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, pr)
	}
	return out, rows.Err()
}
