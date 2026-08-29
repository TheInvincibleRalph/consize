// Package cloudmonitoring implements dbmetrics.Source against Cloud SQL
// via the Cloud SQL Admin API and Cloud Monitoring, mirroring the k8s
// Prometheus path (and the CloudWatch/RDS adapter's shape):
// ListInstances pages the Admin API; Series folds Cloud Monitoring
// ALIGN_MEAN points into step-aligned buckets with the store's db_*
// metric names. No Google SDK — the repo stays dependency-light (same
// rationale as the AWS adapter and pricing). Tokens come from a
// hand-rolled RS256 JWT assertion exchanged at the OAuth2 token
// endpoint (GOOGLE_APPLICATION_CREDENTIALS service-account key),
// falling back to the in-cluster metadata server.
//
// Instance mapping: dbmetrics.Instance has no Region, Tier or Storage
// fields, so those are carried as Labels on the instance
// (consize.savings.dev/region, /tier, /storage). settings.userLabels
// entries become labels too — an operator opts a live instance into
// automatic class changes the same way the fixture does, by labeling
// it consize.savings.dev/auto-db=enabled. The derived facts above are
// set after userLabels so they cannot be clobbered. Availability maps
// to replicas: ZONAL → 1, REGIONAL → 2. The maintenance window
// (settings.maintenanceWindow day 1-7 = mon-sun per the Admin API's
// convention, hour 0-23 UTC)
// becomes the dbapply format "ddd:hh:mm-ddd:hh:mm" with a one-hour
// span (hour 23 wraps to the next day).
//
// Metric mapping (bucket metric ← Cloud Monitoring):
//
//	db_cpu_percent   ← cloudsql.googleapis.com/database/cpu/utilization
//	                   (fraction → ×100)
//	db_mem_percent   ← cloudsql.googleapis.com/database/memory/utilization
//	                   (fraction → ×100; Cloud SQL reports utilization
//	                   directly, so no class-memory baseline is needed,
//	                   unlike the CloudWatch adapter)
//	db_connections   ← cloudsql.googleapis.com/database/network/connections
//	db_iops          ← NOT AVAILABLE: Cloud Monitoring publishes no IOPS
//	                   series for Cloud SQL instances. Series returns no
//	                   data, and the verifier treats a fully-missing
//	                   metric as no-evidence (SLI "unavailable", never
//	                   FAIL) — the same contract as db_errors on the
//	                   CloudWatch adapter.
//	db_errors        ← NOT AVAILABLE (same no-evidence contract).
//
// Configuration: CONSIZE_GCP_PROJECT (default: project_id from the
// GOOGLE_APPLICATION_CREDENTIALS key file, else the instance metadata
// server, like ADC), CONSIZE_DB_FILTER (comma-separated instance
// names; empty = all), GOOGLE_APPLICATION_CREDENTIALS (service-account
// key JSON; else the metadata server provides the token).
package cloudmonitoring

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"consize/internal/config"
	"consize/internal/dbmetrics"
	"consize/internal/store"
)

// oauthScope is the scope for Cloud SQL Admin + Cloud Monitoring.
const oauthScope = "https://www.googleapis.com/auth/cloud-platform"

// tokenURIDefault is the OAuth2 token endpoint service-account keys
// default to (also the JWT audience).
const tokenURIDefault = "https://oauth2.googleapis.com/token"

// Source is a dbmetrics.Source backed by Cloud SQL + Cloud Monitoring.
type Source struct {
	// project is CONSIZE_GCP_PROJECT, else the key file's project_id;
	// resolved lazily from the metadata server on first use when both
	// are absent (ADC semantics).
	project string
	filter  []string // instance names; empty = all

	// tokenFunc supplies the Bearer token for API calls (tests inject
	// a stub; NewSource wires the GOOGLE_APPLICATION_CREDENTIALS →
	// metadata-server fallback chain).
	tokenFunc func(ctx context.Context) (string, error)

	sqlBase  string // https://sqladmin.googleapis.com (tests override)
	monBase  string // https://monitoring.googleapis.com
	metaBase string // http://metadata.google.internal (token/project fallback)
	client   *http.Client

	mu sync.Mutex // guards lazy metadata project resolution
}

// NewSource builds a live Cloud SQL/Cloud Monitoring source from the
// environment. Project: CONSIZE_GCP_PROJECT, else the
// GOOGLE_APPLICATION_CREDENTIALS key file's project_id, else (deferred
// to first use) the metadata server — ADC semantics.
func NewSource() *Source {
	s := &Source{
		project:  config.Str("CONSIZE_GCP_PROJECT", ""),
		sqlBase:  "https://sqladmin.googleapis.com",
		monBase:  "https://monitoring.googleapis.com",
		metaBase: "http://metadata.google.internal",
		client:   &http.Client{Timeout: 60 * time.Second},
	}
	for _, id := range strings.Split(config.Str("CONSIZE_DB_FILTER", ""), ",") {
		if id = strings.TrimSpace(id); id != "" {
			s.filter = append(s.filter, id)
		}
	}
	if s.project == "" {
		if p, ok := saKeyProjectID(); ok {
			s.project = p
		}
	}
	s.tokenFunc = s.tokenFromADC
	return s
}

// Project returns the source's project id (env/file value; the
// metadata-server resolution is lazy and not forced here).
func (s *Source) Project() string { return s.project }

// --- Authentication ---

// saKey is the slice of a service-account key JSON we consume.
type saKey struct {
	Type         string `json:"type"`
	ProjectID    string `json:"project_id"`
	PrivateKeyID string `json:"private_key_id"`
	PrivateKey   string `json:"private_key"`
	ClientEmail  string `json:"client_email"`
	ClientID     string `json:"client_id"`
	TokenURI     string `json:"token_uri"`
}

// saKeyProjectID reads project_id from the GOOGLE_APPLICATION_CREDENTIALS
// key file, if configured (ADC semantics for the default project).
func saKeyProjectID() (string, bool) {
	p := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
	if p == "" {
		return "", false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", false
	}
	var k saKey
	if err := json.Unmarshal(b, &k); err != nil || k.ProjectID == "" {
		return "", false
	}
	return k.ProjectID, true
}

// tokenFromADC picks the token path: the SA key file when
// GOOGLE_APPLICATION_CREDENTIALS is set, else the metadata server.
func (s *Source) tokenFromADC(ctx context.Context) (string, error) {
	if p := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"); p != "" {
		return s.tokenFromSAKey(ctx, p)
	}
	return s.tokenFromMetadata(ctx)
}

// tokenFromSAKey exchanges a hand-rolled RS256 JWT assertion for an
// access token at the key's token endpoint (service-to-service OAuth2,
// RFC 7523). No OAuth client library — the repo stays dependency-light.
func (s *Source) tokenFromSAKey(ctx context.Context, keyPath string) (string, error) {
	b, err := os.ReadFile(keyPath)
	if err != nil {
		return "", fmt.Errorf("read service-account key %s: %w", keyPath, err)
	}
	var k saKey
	if err := json.Unmarshal(b, &k); err != nil {
		return "", fmt.Errorf("decode service-account key %s: %w", keyPath, err)
	}
	key, err := parseRSAKey(k.PrivateKey)
	if err != nil {
		return "", fmt.Errorf("parse service-account private key: %w", err)
	}
	uri := k.TokenURI
	if uri == "" {
		uri = tokenURIDefault
	}

	now := time.Now()
	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(map[string]any{
		"iss":   k.ClientEmail,
		"scope": oauthScope,
		"aud":   uri,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	})
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	assertion := signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)

	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {assertion},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uri,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("token endpoint %s: %w", uri, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", fmt.Errorf("token endpoint: status %s: %s", resp.Status, b)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("token endpoint: empty access_token")
	}
	return tok.AccessToken, nil
}

// parseRSAKey decodes a PEM private key (PKCS#8, as service-account
// keys ship; PKCS#1 accepted too).
func parseRSAKey(pemText string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, fmt.Errorf("no PEM block")
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rk, ok := k.(*rsa.PrivateKey); ok {
			return rk, nil
		}
		return nil, fmt.Errorf("key is not RSA")
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

// tokenFromMetadata asks the instance metadata server for the default
// service account's token (in-cluster deployments without a mounted
// key file).
func (s *Source) tokenFromMetadata(ctx context.Context) (string, error) {
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	err := s.metadataJSON(ctx, "/computeMetadata/v1/instance/service-accounts/default/token", &tok)
	if err != nil {
		return "", err
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("metadata token: empty access_token")
	}
	return tok.AccessToken, nil
}

// metadataProjectID asks the metadata server for the instance's project
// (the ADC default when neither env nor key file names one).
func (s *Source) metadataProjectID(ctx context.Context) (string, error) {
	b, err := s.metadata(ctx, "/computeMetadata/v1/project/project-id")
	if err != nil {
		return "", err
	}
	if p := strings.TrimSpace(string(b)); p != "" {
		return p, nil
	}
	return "", fmt.Errorf("metadata project-id: empty")
}

// metadata fetches one metadata-server path (Metadata-Flavor header
// required by GCP).
func (s *Source) metadata(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.metaBase+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("metadata %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("metadata %s: status %s: %s", path, resp.Status, b)
	}
	return io.ReadAll(resp.Body)
}

// metadataJSON fetches one metadata-server JSON document.
func (s *Source) metadataJSON(ctx context.Context, path string, v any) error {
	b, err := s.metadata(ctx, path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("decode metadata %s: %w", path, err)
	}
	return nil
}

// projectID returns the resolved project, resolving from the metadata
// server once when env/file gave nothing.
func (s *Source) projectID(ctx context.Context) (string, error) {
	if s.project != "" {
		return s.project, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.project != "" {
		return s.project, nil
	}
	p, err := s.metadataProjectID(ctx)
	if err != nil {
		return "", err
	}
	s.project = p
	return p, nil
}

// --- Cloud SQL Admin API (instances list) ---

// sqlInstance is the slice of a v1beta4 instances.list item we consume.
// dataDiskSizeGb is a string in v1beta4 (an int in v1), so it is
// decoded flexibly.
type sqlInstance struct {
	Name            string `json:"name"`
	State           string `json:"state"`
	Region          string `json:"region"`
	DatabaseVersion string `json:"databaseVersion"`
	Settings        struct {
		Tier              string `json:"tier"`
		AvailabilityType  string `json:"availabilityType"`
		MaintenanceWindow *struct {
			Day  int `json:"day"`  // 1-7, 1 = Monday (7 = Sunday)
			Hour int `json:"hour"` // 0-23 UTC
		} `json:"maintenanceWindow"`
		UserLabels     map[string]string `json:"userLabels"`
		DataDiskSizeGB intOrString       `json:"dataDiskSizeGb"`
	} `json:"settings"`
}

// intOrString decodes an API field that is a string in v1beta4 and a
// number in v1.
type intOrString string

// UnmarshalJSON accepts both a JSON string and a JSON number.
func (i *intOrString) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*i = intOrString(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err == nil {
		*i = intOrString(n.String())
		return nil
	}
	return fmt.Errorf("int-or-string field: %s", b)
}

type sqlInstancesList struct {
	Items         []sqlInstance `json:"items"`
	NextPageToken string        `json:"nextPageToken"`
}

// ListInstances pages instances.list and maps each RUNNABLE instance
// to a dbmetrics.Instance. CONSIZE_DB_FILTER restricts the result
// client-side (like the RDS adapter); the state filter is applied
// client-side too — the API's filter parameter would do the same work
// at the server, but a client-side check is easier to test and cannot
// surprise us with filter-parsing quirks.
func (s *Source) ListInstances(ctx context.Context) ([]dbmetrics.Instance, error) {
	project, err := s.projectID(ctx)
	if err != nil {
		return nil, err
	}
	var out []dbmetrics.Instance
	page := ""
	for {
		q := url.Values{}
		if page != "" {
			q.Set("pageToken", page)
		}
		body, err := s.get(ctx, "sqladmin", s.sqlBase,
			"/sql/v1beta4/projects/"+url.PathEscape(project)+"/instances", q)
		if err != nil {
			return nil, err
		}
		var d sqlInstancesList
		if err := json.Unmarshal(body, &d); err != nil {
			return nil, fmt.Errorf("decode instances list: %w", err)
		}
		for _, i := range d.Items {
			if i.State != "RUNNABLE" || !s.wanted(i.Name) {
				continue
			}
			out = append(out, mapInstance(i))
		}
		if d.NextPageToken == "" {
			return out, nil
		}
		page = d.NextPageToken
	}
}

// wanted reports whether an instance name passes the CONSIZE_DB_FILTER
// (empty filter = all instances).
func (s *Source) wanted(name string) bool {
	if len(s.filter) == 0 {
		return true
	}
	for _, f := range s.filter {
		if f == name {
			return true
		}
	}
	return false
}

// gcpDays maps the Admin API's maintenanceWindow day (1 = Monday,
// through 7 = Sunday — the Admin API convention, verified live: a
// Sunday window comes back as day 7) to the dbapply weekday names.
var gcpDays = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

// maintenanceWindow renders the Cloud SQL maintenance window as the
// dbapply format "ddd:hh:mm-ddd:hh:mm" (UTC): a one-hour span starting
// at the given hour; hour 23 wraps to the next day. Out-of-range input
// (the API contract is day 1-7, hour 0-23) yields no window rather
// than a panic — fail-closed, so applies stay blocked.
func maintenanceWindow(day, hour int) string {
	if day < 1 || day > 7 || hour < 0 || hour > 23 {
		return ""
	}
	d := gcpDays[day-1]
	if hour == 23 {
		return fmt.Sprintf("%s:23:00-%s:00:00", d, gcpDays[day%7])
	}
	return fmt.Sprintf("%s:%02d:00-%s:%02d:00", d, hour, d, hour+1)
}

// mapInstance maps one Cloud SQL row. dbmetrics.Instance carries no
// Region, Tier or Storage fields, so those ride as labels (see the
// package doc). REGIONAL availability reports 2 replicas (primary +
// standby); the maintenance window is a one-hour UTC span.
func mapInstance(i sqlInstance) dbmetrics.Instance {
	replicas := 1
	if i.Settings.AvailabilityType == "REGIONAL" {
		replicas = 2
	}
	labels := map[string]string{}
	for k, v := range i.Settings.UserLabels {
		labels[k] = v
	}
	// Derived facts last: userLabels cannot clobber them.
	labels["consize.savings.dev/region"] = i.Region
	labels["consize.savings.dev/tier"] = i.Settings.Tier
	labels["consize.savings.dev/storage"] = string(i.Settings.DataDiskSizeGB) + "GB"

	win := ""
	if i.Settings.MaintenanceWindow != nil {
		win = maintenanceWindow(i.Settings.MaintenanceWindow.Day, i.Settings.MaintenanceWindow.Hour)
	}
	return dbmetrics.Instance{
		Name:              i.Name,
		Namespace:         "gcp",
		Class:             i.Settings.Tier,
		Replicas:          replicas,
		MaintenanceWindow: win,
		Provider:          "gcp",
		Labels:            labels,
	}
}

// --- Cloud Monitoring (timeSeries) ---

// gcpMetricFor maps a store metric name to the Cloud Monitoring metric
// type, or ok=false when the metric has no GCP equivalent (no-evidence:
// db_iops, db_errors) or is unknown.
func gcpMetricFor(metric string) (string, bool) {
	switch metric {
	case store.MetricDBCPUPercent:
		return "cloudsql.googleapis.com/database/cpu/utilization", true
	case store.MetricDBMemPercent:
		return "cloudsql.googleapis.com/database/memory/utilization", true
	case store.MetricDBConnections:
		return "cloudsql.googleapis.com/database/network/connections", true
	case store.MetricDBIOPS, store.MetricDBErrors:
		return "", false // no Cloud Monitoring equivalent: no-evidence
	default:
		return "", false // unknown metric: no data, like a source that doesn't emit it
	}
}

// tsPoint is one Cloud Monitoring aligned point. Value is decoded
// flexibly: utilization is a doubleValue fraction, connections an
// int64Value.
type tsPoint struct {
	Interval struct {
		StartTime string `json:"startTime"`
	} `json:"interval"`
	Value struct {
		DoubleValue *float64 `json:"doubleValue"`
		Int64Value  *int64   `json:"int64Value"`
	} `json:"value"`
}

// value returns the point's numeric value (int64 values as float64).
func (p tsPoint) value() float64 {
	if p.Value.DoubleValue != nil {
		return *p.Value.DoubleValue
	}
	if p.Value.Int64Value != nil {
		return float64(*p.Value.Int64Value)
	}
	return 0
}

type timeSeriesResp struct {
	TimeSeries []struct {
		Points []tsPoint `json:"points"`
	} `json:"timeSeries"`
	NextPageToken string `json:"nextPageToken"`
}

// Series folds Cloud Monitoring ALIGN_MEAN points into step-aligned
// buckets over [start, end), one bucket per window the source observed
// (Cloud Monitoring omits empty periods, like Prometheus omits missing
// samples). Aligned points carry no sample count, so buckets are
// single-sample windows: P50=P95=P99=Max=the period mean. Utilization
// fractions become percents; connections pass through.
func (s *Source) Series(ctx context.Context, inst dbmetrics.Instance, metric string,
	start, end time.Time, step time.Duration) ([]store.Bucket, error) {

	monMetric, ok := gcpMetricFor(metric)
	if !ok {
		// No GCP equivalent (db_iops, db_errors) or unknown metric: the
		// verifier treats a fully-missing metric as no-evidence ("unavailable"
		// SLI, never FAIL), so no data is the correct contract.
		return nil, nil
	}
	project, err := s.projectID(ctx)
	if err != nil {
		return nil, err
	}
	start = start.UTC().Truncate(step)
	end = end.UTC()
	if !end.After(start) {
		return nil, nil
	}

	var pts []tsPoint
	page := ""
	for {
		q := url.Values{}
		q.Set("filter", fmt.Sprintf(
			`metric.type="%s" AND resource.labels.database_id="%s:%s"`,
			monMetric, project, inst.Name))
		q.Set("interval.startTime", start.Format(time.RFC3339))
		q.Set("interval.endTime", end.Format(time.RFC3339))
		q.Set("aggregation.alignmentPeriod", fmt.Sprintf("%ds", int(step.Seconds())))
		q.Set("aggregation.perSeriesAligner", "ALIGN_MEAN")
		if page != "" {
			q.Set("pageToken", page)
		}
		body, err := s.get(ctx, "monitoring", s.monBase,
			"/v3/projects/"+url.PathEscape(project)+"/timeSeries", q)
		if err != nil {
			return nil, err
		}
		var d timeSeriesResp
		if err := json.Unmarshal(body, &d); err != nil {
			return nil, fmt.Errorf("decode timeSeries response: %w", err)
		}
		for _, ts := range d.TimeSeries {
			pts = append(pts, ts.Points...)
		}
		if d.NextPageToken == "" {
			break
		}
		page = d.NextPageToken
	}

	byStart := map[int64]float64{}
	var keys []int64
	for _, p := range pts {
		ts, err := time.Parse(time.RFC3339, p.Interval.StartTime)
		if err != nil {
			return nil, fmt.Errorf("timeSeries timestamp %q: %w", p.Interval.StartTime, err)
		}
		ts = ts.UTC().Truncate(step)
		if ts.Before(start) || !ts.Before(end) {
			continue
		}
		if _, seen := byStart[ts.Unix()]; !seen {
			keys = append(keys, ts.Unix())
		}
		byStart[ts.Unix()] = p.value()
	}
	sort.Slice(keys, func(a, b int) bool { return keys[a] < keys[b] })

	out := make([]store.Bucket, 0, len(keys))
	for _, k := range keys {
		v := byStart[k]
		if metric == store.MetricDBCPUPercent || metric == store.MetricDBMemPercent {
			v = math.Round(v*1000) / 10 // fraction → percent, one decimal
			if v < 0 {
				v = 0 // defensive: the fraction is a 0..1 utilization
			} else if v > 100 {
				v = 100
			}
		}
		out = append(out, store.Bucket{
			WorkloadID:  -1, // assigned by the collector
			Metric:      metric,
			WindowStart: time.Unix(k, 0).UTC(),
			P50:         v, P95: v, P99: v, Max: v,
			Samples: 1,
		})
	}
	return out, nil
}

// --- HTTP plumbing ---

// get issues one authenticated GET and returns the body on 200.
func (s *Source) get(ctx context.Context, service, baseURL, path string,
	q url.Values) ([]byte, error) {

	tok, err := s.tokenFunc(ctx)
	if err != nil {
		return nil, err
	}
	u := baseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("%s: status %s: %s", service, resp.Status, b)
	}
	return io.ReadAll(resp.Body)
}
