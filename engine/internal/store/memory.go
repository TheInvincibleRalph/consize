package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Memory is an in-memory Store used by unit tests and the demo path.
// It implements exactly the same semantics as the Postgres store —
// idempotent upserts, supersede-on-create, derived in-flight state —
// so behavior tests run against both.
type Memory struct {
	mu       sync.RWMutex
	next     int64
	ws       map[string]Workload // key: source|name|namespace
	byID     map[int64]Workload
	buck     map[string]Bucket // key: workload|metric|windowStart
	recs     []Recommendation
	events   []ApplyEvent
	verifs   []VerificationRun
	opps     []CostOpportunity
	costActs []CostAction
	iacPRs   []IaCPullRequest
	teams    map[int64]Team
	bySlug   map[string]int64
	users    []User
	byEmail  map[string]int     // email -> index in users
	byUserID map[int64]int      // id -> index in users
	sessions map[string]Session // token_hash -> session
	settings map[string]string
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{
		ws:       map[string]Workload{},
		byID:     map[int64]Workload{},
		buck:     map[string]Bucket{},
		teams:    map[int64]Team{},
		bySlug:   map[string]int64{},
		byEmail:  map[string]int{},
		byUserID: map[int64]int{},
		sessions: map[string]Session{},
		settings: map[string]string{},
	}
}

func (m *Memory) Health(_ context.Context) error { return nil }

func (m *Memory) UpsertWorkload(_ context.Context, w Workload) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := w.Source + "|" + w.Name + "|" + w.Namespace
	if existing, ok := m.ws[key]; ok {
		w.ID = existing.ID
		// Ownership is operator-managed metadata, not collector data. A
		// collector upsert must never make a workload silently unowned.
		w.TeamID = existing.TeamID
		w.TeamName = existing.TeamName
		w.TeamOnCall = existing.TeamOnCall
		m.ws[key] = w
		m.byID[w.ID] = w
		return w.ID, nil
	}
	m.next++
	w.ID = m.next
	m.ws[key] = w
	m.byID[w.ID] = w
	return w.ID, nil
}

func (m *Memory) SetWorkloadTeam(_ context.Context, workloadID int64, teamID *int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.byID[workloadID]
	if !ok {
		return fmt.Errorf("workload %d: %w", workloadID, ErrNotFound)
	}
	w.TeamID, w.TeamName, w.TeamOnCall = 0, "", ""
	if teamID != nil {
		t, ok := m.teams[*teamID]
		if !ok {
			return fmt.Errorf("team %d: %w", *teamID, ErrNotFound)
		}
		w.TeamID, w.TeamName, w.TeamOnCall = t.ID, t.Name, t.OnCall
	}
	m.byID[workloadID] = w
	m.ws[w.Source+"|"+w.Name+"|"+w.Namespace] = w
	return nil
}

func (m *Memory) CreateTeam(_ context.Context, team Team) (Team, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.bySlug[team.Slug]; exists {
		return Team{}, fmt.Errorf("team %q: %w", team.Slug, ErrConflict)
	}
	m.next++
	team.ID = m.next
	now := time.Now().UTC()
	team.CreatedAt, team.UpdatedAt = now, now
	m.teams[team.ID] = team
	m.bySlug[team.Slug] = team.ID
	return team, nil
}

func (m *Memory) GetTeam(_ context.Context, id int64) (Team, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.teams[id]
	if !ok {
		return Team{}, fmt.Errorf("team %d: %w", id, ErrNotFound)
	}
	return t, nil
}

func (m *Memory) ListTeams(_ context.Context) ([]Team, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Team, 0, len(m.teams))
	for _, t := range m.teams {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *Memory) UpdateTeam(_ context.Context, team Team) (Team, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.teams[team.ID]
	if !ok {
		return Team{}, fmt.Errorf("team %d: %w", team.ID, ErrNotFound)
	}
	current.Owner, current.OnCall, current.UpdatedAt = team.Owner, team.OnCall, time.Now().UTC()
	m.teams[current.ID] = current
	// Keep already-read workload views coherent. Future collector upserts
	// preserve this refreshed denormalized ownership data too.
	for id, w := range m.byID {
		if w.TeamID == current.ID {
			w.TeamName, w.TeamOnCall = current.Name, current.OnCall
			m.byID[id] = w
			m.ws[w.Source+"|"+w.Name+"|"+w.Namespace] = w
		}
	}
	return current, nil
}

func (m *Memory) ListWorkloads(_ context.Context) ([]Workload, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Workload, 0, len(m.ws))
	for _, w := range m.ws {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (m *Memory) GetWorkload(_ context.Context, id int64) (Workload, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	w, ok := m.byID[id]
	if !ok {
		return Workload{}, fmt.Errorf("workload %d: %w", id, ErrNotFound)
	}
	return w, nil
}

func (m *Memory) UpsertBucket(_ context.Context, b Bucket) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.buck[bucketKey(b.WorkloadID, b.Metric, b.WindowStart)] = b
	return nil
}

func (m *Memory) ListBuckets(_ context.Context, workloadID int64, metric string, from, to time.Time) ([]Bucket, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Bucket
	for _, b := range m.buck {
		if b.WorkloadID != workloadID || b.Metric != metric {
			continue
		}
		if b.WindowStart.Before(from) || b.WindowStart.After(to) {
			continue
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].WindowStart.Before(out[j].WindowStart) })
	return out, nil
}

func (m *Memory) LatestBucketTime(_ context.Context) (time.Time, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var latest time.Time
	for _, b := range m.buck {
		if b.WindowStart.After(latest) {
			latest = b.WindowStart
		}
	}
	return latest, !latest.IsZero(), nil
}

func (m *Memory) CreateRecommendations(_ context.Context, recs []Recommendation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Supersede prior pending recommendations for the same (workload, resource).
	for i := range m.recs {
		for _, r := range recs {
			if m.recs[i].WorkloadID == r.WorkloadID &&
				m.recs[i].Resource == r.Resource &&
				m.recs[i].Status == StatusPending {
				m.recs[i].Status = StatusSuperseded
			}
		}
	}
	for _, r := range recs {
		r = normalizeRecommendationSteps(r)
		m.next++
		r.ID = m.next
		r.CreatedAt = time.Now().UTC()
		if r.Status == "" {
			r.Status = StatusPending
		}
		m.recs = append(m.recs, r)
	}
	return nil
}

func (m *Memory) ListRecommendations(_ context.Context, workloadID *int64, status string, limit, offset int) ([]Recommendation, int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Recommendation
	for _, r := range m.recs {
		if workloadID != nil && r.WorkloadID != *workloadID {
			continue
		}
		if status != "" && r.Status != status {
			continue
		}
		// Denormalize workload name/namespace like the Postgres JOIN does.
		if w, ok := m.byID[r.WorkloadID]; ok {
			r.WorkloadName = w.Name
			r.Namespace = w.Namespace
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SavingsMonthly > out[j].SavingsMonthly })
	total := len(out)
	if offset >= len(out) {
		return nil, total, nil
	}
	out = out[offset:]
	if limit > 0 && limit < len(out) {
		out = out[:limit]
	}
	return out, total, nil
}

func (m *Memory) PruneRecommendations(_ context.Context, status string, cutoff time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.recs[:0]
	var pruned int64
	for _, r := range m.recs {
		if r.Status == status && r.CreatedAt.Before(cutoff) {
			pruned++
			continue
		}
		kept = append(kept, r)
	}
	m.recs = kept
	return pruned, nil
}

func (m *Memory) CreateFollowUpRecommendation(_ context.Context, r Recommendation) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r = normalizeRecommendationSteps(r)
	m.next++
	r.ID = m.next
	r.CreatedAt = time.Now().UTC()
	if r.Status == "" {
		r.Status = StatusPending
	}
	m.recs = append(m.recs, r)
	return r.ID, nil
}

func (m *Memory) GetRecommendation(_ context.Context, id int64) (Recommendation, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, r := range m.recs {
		if r.ID == id {
			if w, ok := m.byID[r.WorkloadID]; ok {
				r.WorkloadName = w.Name
				r.Namespace = w.Namespace
			}
			return r, nil
		}
	}
	return Recommendation{}, fmt.Errorf("recommendation %d: %w", id, ErrNotFound)
}

func (m *Memory) SetRecommendationStatus(_ context.Context, id int64, status string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.recs {
		if m.recs[i].ID == id {
			m.recs[i].Status = status
			return nil
		}
	}
	return fmt.Errorf("recommendation %d: %w", id, ErrNotFound)
}

func (m *Memory) CreateApplyEvent(_ context.Context, e ApplyEvent) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	e.ID = m.next
	e.CreatedAt = time.Now().UTC()
	m.events = append(m.events, e)
	return e.ID, nil
}

func (m *Memory) GetApplyEvent(_ context.Context, id int64) (ApplyEvent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, e := range m.events {
		if e.ID == id {
			return e, nil
		}
	}
	return ApplyEvent{}, fmt.Errorf("apply event %d: %w", id, ErrNotFound)
}

func (m *Memory) ListApplyEvents(_ context.Context, workloadID *int64, result string) ([]ApplyEvent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []ApplyEvent{}
	for _, e := range m.events {
		if workloadID != nil && e.WorkloadID != *workloadID {
			continue
		}
		if result != "" && e.Result != result {
			continue
		}
		out = append(out, e)
	}
	// Newest first, matching the Postgres ordering.
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (m *Memory) ActiveApplyInNamespace(_ context.Context, namespace string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, e := range m.events {
		if e.Result != EventApplied {
			continue
		}
		w, ok := m.byID[e.WorkloadID]
		if !ok || w.Namespace != namespace {
			continue
		}
		done := false
		for _, v := range m.verifs {
			if v.ApplyEventID == e.ID {
				done = true
				break
			}
		}
		if !done {
			return true, nil
		}
	}
	return false, nil
}

func (m *Memory) ActiveApplyCount(_ context.Context) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	done := map[int64]bool{}
	for _, v := range m.verifs {
		done[v.ApplyEventID] = true
	}
	n := 0
	for _, e := range m.events {
		if e.Result == EventApplied && !done[e.ID] {
			n++
		}
	}
	return n, nil
}

func (m *Memory) AppliedEventsUnverified(_ context.Context) ([]ApplyEvent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	done := map[int64]bool{}
	for _, v := range m.verifs {
		done[v.ApplyEventID] = true
	}
	out := []ApplyEvent{}
	for _, e := range m.events {
		if e.Result == EventApplied && !done[e.ID] {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *Memory) CreateVerificationRun(_ context.Context, v VerificationRun) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.verifs {
		if m.verifs[i].ApplyEventID == v.ApplyEventID {
			m.verifs[i] = v // upsert semantics, like Postgres ON CONFLICT
			return nil
		}
	}
	m.next++
	v.ID = m.next
	v.CreatedAt = time.Now().UTC()
	m.verifs = append(m.verifs, v)
	return nil
}

func (m *Memory) ListVerificationRuns(_ context.Context, applyEventID *int64) ([]VerificationRun, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []VerificationRun{}
	for _, v := range m.verifs {
		if applyEventID != nil && v.ApplyEventID != *applyEventID {
			continue
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (m *Memory) SavingsSummary(_ context.Context) (float64, int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var total float64
	var n int
	for _, r := range m.recs {
		if r.Status == StatusPending {
			total += r.SavingsMonthly
			n++
		}
	}
	return total, n, nil
}

func (m *Memory) CreateUser(_ context.Context, u User) (User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u.Email = strings.ToLower(u.Email) // normalized like Postgres
	if i, ok := m.byEmail[u.Email]; ok {
		return m.users[i], nil // idempotent on email, like Postgres ON CONFLICT
	}
	m.next++
	u.ID = m.next
	u.CreatedAt = time.Now().UTC()
	m.users = append(m.users, u)
	m.byEmail[u.Email] = len(m.users) - 1
	m.byUserID[u.ID] = len(m.users) - 1
	return u, nil
}

func (m *Memory) GetUserByEmail(_ context.Context, email string) (User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if i, ok := m.byEmail[strings.ToLower(email)]; ok {
		return m.users[i], nil
	}
	return User{}, ErrNotFound
}

func (m *Memory) GetUser(_ context.Context, id int64) (User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if i, ok := m.byUserID[id]; ok {
		return m.users[i], nil
	}
	return User{}, ErrNotFound
}

func (m *Memory) CountUsers(_ context.Context) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.users), nil
}

func (m *Memory) CreateSession(_ context.Context, s Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s.CreatedAt = time.Now().UTC()
	m.sessions[s.TokenHash] = s
	return nil
}

func (m *Memory) GetSessionByTokenHash(_ context.Context, tokenHash string) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[tokenHash]
	if !ok || !s.ExpiresAt.After(time.Now().UTC()) {
		delete(m.sessions, tokenHash) // expired sessions are gone, like Postgres
		return Session{}, ErrNotFound
	}
	return s, nil
}

func (m *Memory) DeleteSession(_ context.Context, tokenHash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, tokenHash)
	return nil
}

func (m *Memory) GetSetting(_ context.Context, key string) (string, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	value, ok := m.settings[key]
	return value, ok, nil
}

func (m *Memory) PutSetting(_ context.Context, key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.settings[key] = value
	return nil
}

func opportunityKey(o CostOpportunity) string {
	return strings.Join([]string{o.Provider, o.Account, o.Region, o.ResourceType, o.ResourceID}, "|")
}

func (m *Memory) UpsertCostOpportunities(_ context.Context, opportunities []CostOpportunity) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	byKey := map[string]int{}
	for i, o := range m.opps {
		byKey[opportunityKey(o)] = i
	}
	for _, o := range opportunities {
		key := opportunityKey(o)
		if i, ok := byKey[key]; ok {
			current := m.opps[i]
			o.ID = current.ID
			o.Status = orDefault(o.Status, current.Status)
			o.FirstSeenAt = current.FirstSeenAt
			o.CreatedAt = current.CreatedAt
			o.UpdatedAt = now
			if o.LastSeenAt.IsZero() {
				o.LastSeenAt = now
			}
			m.opps[i] = o
			continue
		}
		m.next++
		o.ID = m.next
		o.Status = orDefault(o.Status, OpportunityOpen)
		if o.FirstSeenAt.IsZero() {
			o.FirstSeenAt = now
		}
		if o.LastSeenAt.IsZero() {
			o.LastSeenAt = now
		}
		o.CreatedAt, o.UpdatedAt = now, now
		m.opps = append(m.opps, o)
	}
	return nil
}

func (m *Memory) ListCostOpportunities(_ context.Context, status string) ([]CostOpportunity, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []CostOpportunity{}
	for _, o := range m.opps {
		if status != "" && o.Status != status {
			continue
		}
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].MonthlyCost == out[j].MonthlyCost {
			return out[i].LastSeenAt.After(out[j].LastSeenAt)
		}
		return out[i].MonthlyCost > out[j].MonthlyCost
	})
	return out, nil
}

func (m *Memory) GetCostOpportunity(_ context.Context, id int64) (CostOpportunity, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, o := range m.opps {
		if o.ID == id {
			return o, nil
		}
	}
	return CostOpportunity{}, fmt.Errorf("cost opportunity %d: %w", id, ErrNotFound)
}

func (m *Memory) SetCostOpportunityStatus(_ context.Context, id int64, status string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.opps {
		if m.opps[i].ID == id {
			m.opps[i].Status = status
			m.opps[i].UpdatedAt = time.Now().UTC()
			return nil
		}
	}
	return fmt.Errorf("cost opportunity %d: %w", id, ErrNotFound)
}

func (m *Memory) CreateCostAction(_ context.Context, action CostAction) (CostAction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	found := false
	for _, o := range m.opps {
		if o.ID == action.OpportunityID {
			found = true
			break
		}
	}
	if !found {
		return CostAction{}, fmt.Errorf("cost opportunity %d: %w", action.OpportunityID, ErrNotFound)
	}
	m.next++
	action.ID = m.next
	if action.Mode == "" {
		action.Mode = "dry_run"
	}
	if action.Result == "" {
		action.Result = CostActionRequested
	}
	if action.Evidence == nil {
		action.Evidence = map[string]any{}
	}
	action.CreatedAt = time.Now().UTC()
	m.costActs = append(m.costActs, action)
	return action, nil
}

func (m *Memory) ListCostActions(_ context.Context, opportunityID *int64) ([]CostAction, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []CostAction{}
	for _, action := range m.costActs {
		if opportunityID != nil && action.OpportunityID != *opportunityID {
			continue
		}
		out = append(out, action)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (m *Memory) CreateIaCPullRequest(_ context.Context, pr IaCPullRequest) (IaCPullRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if pr.ChangeKind == "" {
		if pr.RecommendationID > 0 {
			pr.ChangeKind = IaCChangeKindRecommendation
		} else {
			pr.ChangeKind = IaCChangeKindCostOpportunity
		}
	}
	if pr.OpportunityID > 0 {
		found := false
		for i := range m.opps {
			if m.opps[i].ID == pr.OpportunityID {
				found = true
				if pr.Status == IaCPROpened {
					m.opps[i].Status = OpportunityPROpened
				} else if pr.Status == IaCPRPlanned {
					m.opps[i].Status = OpportunityPRReady
				}
				m.opps[i].UpdatedAt = time.Now().UTC()
				break
			}
		}
		if !found {
			return IaCPullRequest{}, fmt.Errorf("cost opportunity %d: %w", pr.OpportunityID, ErrNotFound)
		}
	} else if pr.RecommendationID > 0 {
		found := false
		for _, r := range m.recs {
			if r.ID == pr.RecommendationID {
				found = true
				break
			}
		}
		if !found {
			return IaCPullRequest{}, fmt.Errorf("recommendation %d: %w", pr.RecommendationID, ErrNotFound)
		}
	} else {
		return IaCPullRequest{}, fmt.Errorf("iac pull request source: %w", ErrNotFound)
	}
	m.next++
	pr.ID = m.next
	if pr.Status == "" {
		pr.Status = IaCPRPlanned
	}
	pr.CreatedAt = time.Now().UTC()
	m.iacPRs = append(m.iacPRs, pr)
	return pr, nil
}

func (m *Memory) ListIaCPullRequests(_ context.Context, opportunityID *int64) ([]IaCPullRequest, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []IaCPullRequest{}
	for _, pr := range m.iacPRs {
		if opportunityID != nil && pr.OpportunityID != *opportunityID {
			continue
		}
		out = append(out, pr)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func bucketKey(id int64, metric string, t time.Time) string {
	return fmt.Sprintf("%d|%s|%d", id, metric, t.Unix())
}
