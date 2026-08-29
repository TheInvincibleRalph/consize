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
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"consize/internal/dbmetrics"
	"consize/internal/store"
)

// --- Cloud SQL Admin API (ListInstances) ---

const sqlPage1 = `{"kind":"sql#instancesList","items":[
{"name":"payments-prod","project":"acme-prod","state":"RUNNABLE","region":"us-central1",
 "databaseVersion":"POSTGRES_14",
 "settings":{"tier":"db-custom-1-3840","availabilityType":"ZONAL",
   "maintenanceWindow":{"day":1,"hour":2},
   "userLabels":{"consize.savings.dev/auto-db":"enabled","app":"payments"},
   "dataDiskSizeGb":"100"}},
{"name":"offline","project":"acme-prod","state":"FAILED","region":"us-central1",
 "settings":{"tier":"db-custom-2-7680","availabilityType":"ZONAL","dataDiskSizeGb":"10"}}
],"nextPageToken":"page-2"}`

const sqlPage2 = `{"kind":"sql#instancesList","items":[
{"name":"analytics","project":"acme-prod","state":"RUNNABLE","region":"europe-west1",
 "databaseVersion":"MYSQL_8_0",
 "settings":{"tier":"db-custom-2-7680","availabilityType":"REGIONAL",
   "maintenanceWindow":{"day":7,"hour":23},
   "dataDiskSizeGb":250}}
]}`

// sqlServer serves paginated instances.list and records the page tokens
// it saw plus the auth header.
func sqlServer(t *testing.T) (*httptest.Server, *[]string, *[]string) {
	t.Helper()
	var pages []string
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sql/v1beta4/projects/acme-prod/instances" {
			t.Fatalf("path: %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("auth header: %q", got)
		}
		pages = append(pages, r.URL.Query().Get("pageToken"))
		auths = append(auths, r.Header.Get("Authorization"))
		switch r.URL.Query().Get("pageToken") {
		case "":
			w.Write([]byte(sqlPage1))
		case "page-2":
			w.Write([]byte(sqlPage2))
		default:
			t.Fatalf("unexpected page token %q", r.URL.Query().Get("pageToken"))
		}
	}))
	return srv, &pages, &auths
}

func testSource(srvURL string) *Source {
	return &Source{
		project:  "acme-prod",
		sqlBase:  srvURL,
		monBase:  "http://unused",
		metaBase: "http://metadata.google.internal",
		client:   &http.Client{},
		tokenFunc: func(context.Context) (string, error) {
			return "test-token", nil
		},
	}
}

func TestListInstancesPaginationAndMapping(t *testing.T) {
	srv, pages, auths := sqlServer(t)
	defer srv.Close()
	s := testSource(srv.URL)

	insts, err := s.ListInstances(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(insts) != 2 {
		t.Fatalf("want 2 RUNNABLE instances across 2 pages (FAILED must drop), got %d", len(insts))
	}
	if got := *pages; len(got) != 2 || got[0] != "" || got[1] != "page-2" {
		t.Fatalf("pagination pageTokens: %v", got)
	}
	if len(*auths) != 2 {
		t.Fatalf("every request must authenticate: %v", *auths)
	}

	p := insts[0]
	if p.Name != "payments-prod" || p.Class != "db-custom-1-3840" || p.Namespace != "gcp" ||
		p.Provider != "gcp" || p.Replicas != 1 {
		t.Fatalf("instance 0: %+v", p)
	}
	if p.MaintenanceWindow != "mon:02:00-mon:03:00" {
		t.Fatalf("maintenance window (day 1 = monday, 1h span): %q", p.MaintenanceWindow)
	}
	if p.Labels["consize.savings.dev/auto-db"] != "enabled" || p.Labels["app"] != "payments" {
		t.Fatalf("userLabels must become labels: %v", p.Labels)
	}
	if p.Labels["consize.savings.dev/region"] != "us-central1" ||
		p.Labels["consize.savings.dev/tier"] != "db-custom-1-3840" ||
		p.Labels["consize.savings.dev/storage"] != "100GB" {
		t.Fatalf("derived labels: %v", p.Labels)
	}

	a := insts[1]
	if a.Name != "analytics" || a.Replicas != 2 {
		t.Fatalf("REGIONAL must report 2 replicas: %+v", a)
	}
	if a.MaintenanceWindow != "sun:23:00-mon:00:00" {
		t.Fatalf("hour 23 must wrap to the next day: %q", a.MaintenanceWindow)
	}
	if a.Labels["consize.savings.dev/storage"] != "250GB" {
		t.Fatalf("dataDiskSizeGb int form: %v", a.Labels)
	}
}

func TestListInstancesFilter(t *testing.T) {
	srv, _, _ := sqlServer(t)
	defer srv.Close()
	s := testSource(srv.URL)
	s.filter = []string{"payments-prod"}

	insts, err := s.ListInstances(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(insts) != 1 || insts[0].Name != "payments-prod" {
		t.Fatalf("CONSIZE_DB_FILTER: %+v", insts)
	}
}

func TestListInstancesProjectFromMetadata(t *testing.T) {
	metaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			t.Fatalf("metadata requests must carry Metadata-Flavor: Google: %+v", r.Header)
		}
		switch r.URL.Path {
		case "/computeMetadata/v1/project/project-id":
			w.Write([]byte("meta-proj"))
		case "/computeMetadata/v1/instance/service-accounts/default/token":
			w.Write([]byte(`{"access_token":"meta-token","expires_in":3600}`))
		default:
			t.Fatalf("unexpected metadata path %q", r.URL.Path)
		}
	}))
	defer metaSrv.Close()

	var sawProject string
	sqlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer meta-token" {
			t.Fatalf("token from metadata server: %q", r.Header.Get("Authorization"))
		}
		sawProject = r.URL.Path
		w.Write([]byte(`{"items":[]}`))
	}))
	defer sqlSrv.Close()

	s := &Source{
		sqlBase:  sqlSrv.URL,
		monBase:  "http://unused",
		metaBase: metaSrv.URL,
		client:   &http.Client{},
	}
	s.tokenFunc = s.tokenFromMetadata
	if _, err := s.ListInstances(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sawProject, "/projects/meta-proj/instances") {
		t.Fatalf("project must resolve from metadata (ADC default): %s", sawProject)
	}
	if s.project != "meta-proj" {
		t.Fatalf("resolved project must be cached: %q", s.project)
	}
}

// --- maintenance window mapping ---

func TestMaintenanceWindowMapping(t *testing.T) {
	cases := []struct {
		day, hour int
		want      string
	}{
		{1, 2, "mon:02:00-mon:03:00"},
		{2, 0, "tue:00:00-tue:01:00"},
		{3, 9, "wed:09:00-wed:10:00"},
		{4, 12, "thu:12:00-thu:13:00"},
		{5, 18, "fri:18:00-fri:19:00"},
		{6, 23, "sat:23:00-sun:00:00"},
		{7, 23, "sun:23:00-mon:00:00"},
		{7, 1, "sun:01:00-sun:02:00"},
	}
	for _, c := range cases {
		if got := maintenanceWindow(c.day, c.hour); got != c.want {
			t.Fatalf("day=%d hour=%d: want %q, got %q", c.day, c.hour, c.want, got)
		}
	}
	// Out-of-range input yields no window (fail-closed, never a panic).
	for _, c := range []struct{ day, hour int }{{0, 2}, {8, 2}, {1, -1}, {1, 24}} {
		if got := maintenanceWindow(c.day, c.hour); got != "" {
			t.Fatalf("day=%d hour=%d: want no window, got %q", c.day, c.hour, got)
		}
	}
}

// --- Cloud Monitoring (Series) ---

// point renders one aligned point (double value).
func point(start string, v float64) string {
	b, _ := json.Marshal(map[string]any{
		"interval": map[string]string{"startTime": start, "endTime": start},
		"value":    map[string]any{"doubleValue": v},
	})
	return string(b)
}

// pointInt renders one aligned point (int64 value, e.g. connections).
func pointInt(start string, v int64) string {
	b, _ := json.Marshal(map[string]any{
		"interval": map[string]string{"startTime": start, "endTime": start},
		"value":    map[string]any{"int64Value": v},
	})
	return string(b)
}

// tsResp wraps points into a timeSeries response, optionally paging.
func tsResp(next string, points ...string) string {
	out := `{"timeSeries":[{"metric":{"type":"x"},"points":[` +
		strings.Join(points, ",") + `]}]}`
	if next != "" {
		out = out[:len(out)-1] + `,"nextPageToken":"` + next + `"}`
	}
	return out
}

// monServer serves canned timeSeries responses keyed by the metric
// type named in the filter and records the raw queries it saw.
func monServer(t *testing.T, canned map[string]string) (*httptest.Server, *[]string) {
	t.Helper()
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/projects/acme-prod/timeSeries" {
			t.Fatalf("path: %q", r.URL.Path)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Fatalf("missing Bearer auth: %q", r.Header.Get("Authorization"))
		}
		filter := r.URL.Query().Get("filter")
		queries = append(queries, r.URL.RawQuery)
		key := ""
		for _, k := range []string{"cpu/utilization", "memory/utilization", "network/connections"} {
			if strings.Contains(filter, k) {
				key = k
			}
		}
		resp, ok := canned[key]
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"code":400,"message":"no such metric"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(resp))
	}))
	return srv, &queries
}

func monSource(srvURL string) *Source {
	s := testSource("http://unused")
	s.monBase = srvURL
	return s
}

func TestSeriesFractionToPercent(t *testing.T) {
	// Hand-computed: cpu utilization 0.10 → 10.0%; memory 0.125 → 12.5%.
	canned := map[string]string{
		"cpu/utilization":     tsResp("", point("2026-08-10T00:00:00Z", 0.10), point("2026-08-10T00:15:00Z", 0.20)),
		"memory/utilization":  tsResp("", point("2026-08-10T00:00:00Z", 0.125)),
		"network/connections": tsResp("", pointInt("2026-08-10T00:00:00Z", 300)),
	}
	srv, queries := monServer(t, canned)
	defer srv.Close()
	s := monSource(srv.URL)

	inst := dbmetrics.Instance{Name: "payments-prod"}
	start := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	win := 15 * time.Minute

	// Two 15-minute windows: [00:00, 00:30).
	bs, err := s.Series(context.Background(), inst, store.MetricDBCPUPercent, start, start.Add(2*win), win)
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 2 || bs[0].P95 != 10.0 || bs[1].P95 != 20.0 {
		t.Fatalf("cpu fraction → percent: %+v", bs)
	}
	if bs[0].Samples != 1 || bs[0].P50 != bs[0].P95 || bs[0].P95 != bs[0].Max {
		t.Fatalf("single-sample aligned window expected: %+v", bs[0])
	}

	bs, err = s.Series(context.Background(), inst, store.MetricDBMemPercent, start, start.Add(win), win)
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 1 || bs[0].P95 != 12.5 {
		t.Fatalf("mem fraction 0.125 → 12.5%%: %+v", bs)
	}

	// Request shape: exact metric type + database_id filter, ALIGN_MEAN,
	// step-sized alignment period, RFC3339 interval (one request per
	// Series call: cpu and mem).
	if len(*queries) != 2 {
		t.Fatalf("want 2 requests, got %d", len(*queries))
	}
	q0, _ := url.ParseQuery((*queries)[0])
	if got := q0.Get("filter"); got != `metric.type="cloudsql.googleapis.com/database/cpu/utilization" AND resource.labels.database_id="acme-prod:payments-prod"` {
		t.Fatalf("filter: %q", got)
	}
	if q0.Get("aggregation.alignmentPeriod") != "900s" || q0.Get("aggregation.perSeriesAligner") != "ALIGN_MEAN" {
		t.Fatalf("aggregation: %v", q0)
	}
	if q0.Get("interval.startTime") != "2026-08-10T00:00:00Z" || q0.Get("interval.endTime") != "2026-08-10T00:30:00Z" {
		t.Fatalf("interval: %v", q0)
	}
}

func TestSeriesConnectionsPassThrough(t *testing.T) {
	canned := map[string]string{
		"network/connections": tsResp("", pointInt("2026-08-10T00:00:00Z", 300)),
	}
	srv, _ := monServer(t, canned)
	defer srv.Close()
	s := monSource(srv.URL)

	inst := dbmetrics.Instance{Name: "payments-prod"}
	start := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	bs, err := s.Series(context.Background(), inst, store.MetricDBConnections,
		start, start.Add(15*time.Minute), 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 1 || bs[0].P95 != 300 {
		t.Fatalf("connections pass through: %+v", bs)
	}
}

func TestSeriesStepAlignmentAndWindowExclusive(t *testing.T) {
	// Points at 00:00, 00:17:30 (off-grid → 00:15 window) and 01:00;
	// window is [00:00, 01:00) so 01:00 drops and the empty 00:30/00:45
	// periods are omitted.
	canned := map[string]string{
		"cpu/utilization": tsResp("",
			point("2026-08-10T00:00:00Z", 0.10),
			point("2026-08-10T00:17:30Z", 0.20),
			point("2026-08-10T01:00:00Z", 0.99)),
	}
	srv, _ := monServer(t, canned)
	defer srv.Close()
	s := monSource(srv.URL)

	inst := dbmetrics.Instance{Name: "payments-prod"}
	start := time.Date(2026, 8, 10, 0, 3, 0, 0, time.UTC) // unaligned start
	end := time.Date(2026, 8, 10, 1, 0, 0, 0, time.UTC)   // exclusive end
	bs, err := s.Series(context.Background(), inst, store.MetricDBCPUPercent, start, end, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 2 {
		t.Fatalf("want 2 step-aligned buckets (01:00 excluded, gaps omitted), got %d: %+v", len(bs), bs)
	}
	if bs[0].WindowStart != time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC) || bs[0].P95 != 10.0 {
		t.Fatalf("bucket 0: %+v", bs[0])
	}
	if bs[1].WindowStart != time.Date(2026, 8, 10, 0, 15, 0, 0, time.UTC) || bs[1].P95 != 20.0 {
		t.Fatalf("off-grid 00:17:30 must truncate into the 00:15 window: %+v", bs[1])
	}
}

func TestSeriesPagination(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("pageToken") {
		case "":
			w.Write([]byte(`{"timeSeries":[{"points":[` + point("2026-08-10T00:00:00Z", 0.10) + `]}],"nextPageToken":"p2"}`))
		case "p2":
			w.Write([]byte(`{"timeSeries":[{"points":[` + point("2026-08-10T00:15:00Z", 0.20) + `]}]}`))
		default:
			t.Fatalf("unexpected pageToken %q", r.URL.Query().Get("pageToken"))
		}
	}))
	defer srv.Close()
	s := monSource(srv.URL)

	inst := dbmetrics.Instance{Name: "payments-prod"}
	start := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	bs, err := s.Series(context.Background(), inst, store.MetricDBCPUPercent,
		start, start.Add(time.Hour), 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 2 || bs[0].P95 != 10.0 || bs[1].P95 != 20.0 {
		t.Fatalf("points across pages must merge: %+v", bs)
	}
}

func TestSeriesIOPSAndErrorsEmptyNoEvidence(t *testing.T) {
	// db_iops and db_errors have no Cloud Monitoring equivalent: Series
	// must return no data and never touch the network.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("no-evidence metric must not call the API: %s", r.URL)
	}))
	defer srv.Close()
	s := monSource(srv.URL)
	s.tokenFunc = func(context.Context) (string, error) {
		t.Fatal("no-evidence metric must not fetch a token")
		return "", nil
	}

	inst := dbmetrics.Instance{Name: "payments-prod"}
	start := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	for _, metric := range []string{store.MetricDBIOPS, store.MetricDBErrors} {
		bs, err := s.Series(context.Background(), inst, metric, start, start.Add(24*time.Hour), 15*time.Minute)
		if err != nil || len(bs) != 0 {
			t.Fatalf("%s must be empty no-evidence (got %d buckets, err=%v)", metric, len(bs), err)
		}
	}
}

func TestSeriesUnknownMetricIsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unknown metric must not call the API: %s", r.URL)
	}))
	defer srv.Close()
	s := monSource(srv.URL)

	inst := dbmetrics.Instance{Name: "payments-prod"}
	start := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	bs, err := s.Series(context.Background(), inst, "not_a_metric", start, start.Add(time.Hour), 15*time.Minute)
	if err != nil || len(bs) != 0 {
		t.Fatalf("unknown metric: %d err=%v", len(bs), err)
	}
}

func TestSeriesEmptyWindowIsEmpty(t *testing.T) {
	srv, _ := monServer(t, map[string]string{"cpu/utilization": tsResp("", point("2026-08-10T00:00:00Z", 0.1))})
	defer srv.Close()
	s := monSource(srv.URL)

	inst := dbmetrics.Instance{Name: "payments-prod"}
	start := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	bs, err := s.Series(context.Background(), inst, store.MetricDBCPUPercent, start, start, 15*time.Minute)
	if err != nil || len(bs) != 0 {
		t.Fatalf("empty [start,end) must yield no buckets: %d err=%v", len(bs), err)
	}
}

// --- token acquisition ---

// TestTokenFromSAKey exercises the hand-rolled RS256 JWT path end to
// end: the token server decodes the assertion, verifies its signature
// with the key's public half, and checks the standard claims.
func TestTokenFromSAKey(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: pkcs8,
	})

	var jwsErr error
	var tokenSrv *httptest.Server
	tokenSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("token endpoint method: %s", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		form, err := url.ParseQuery(string(body))
		if err != nil {
			t.Fatalf("token form: %v", err)
		}
		if form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			t.Fatalf("grant type: %q", form.Get("grant_type"))
		}
		parts := strings.Split(form.Get("assertion"), ".")
		if len(parts) != 3 {
			jwsErr = fmt.Errorf("assertion must be header.claims.sig")
			return
		}
		hdrB, err := base64.RawURLEncoding.DecodeString(parts[0])
		if err != nil {
			jwsErr = fmt.Errorf("header base64: %w", err)
			return
		}
		claimsB, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			jwsErr = fmt.Errorf("claims base64: %w", err)
			return
		}
		sigB, err := base64.RawURLEncoding.DecodeString(parts[2])
		if err != nil {
			jwsErr = fmt.Errorf("sig base64: %w", err)
			return
		}
		var hdr map[string]string
		if err := json.Unmarshal(hdrB, &hdr); err != nil || hdr["alg"] != "RS256" {
			jwsErr = fmt.Errorf("header: %s err=%v", hdrB, err)
			return
		}
		var claims map[string]any
		if err := json.Unmarshal(claimsB, &claims); err != nil {
			jwsErr = fmt.Errorf("claims: %v", err)
			return
		}
		if claims["iss"] != "consize@acme-prod.iam.gserviceaccount.com" ||
			claims["aud"] != tokenSrv.URL ||
			claims["scope"] != oauthScope {
			jwsErr = fmt.Errorf("claims: %v", claims)
			return
		}
		exp := claims["exp"].(float64)
		iat := claims["iat"].(float64)
		if exp-iat != 3600 || iat < 0 {
			jwsErr = fmt.Errorf("iat/exp: %v", claims)
			return
		}
		digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sigB); err != nil {
			jwsErr = fmt.Errorf("signature: %w", err)
			return
		}
		w.Write([]byte(`{"access_token":"sa-token","expires_in":3600,"token_type":"Bearer"}`))
	}))
	defer tokenSrv.Close()

	keyJSON, _ := json.Marshal(map[string]any{
		"type":         "service_account",
		"project_id":   "acme-prod",
		"private_key":  string(pemBytes),
		"client_email": "consize@acme-prod.iam.gserviceaccount.com",
		"token_uri":    tokenSrv.URL,
	})
	keyFile := t.TempDir() + "/sa-key.json"
	if err := os.WriteFile(keyFile, keyJSON, 0o600); err != nil {
		t.Fatal(err)
	}

	s := &Source{client: tokenSrv.Client()}
	tok, err := s.tokenFromSAKey(context.Background(), keyFile)
	if err != nil {
		t.Fatalf("tokenFromSAKey: %v (jws: %v)", err, jwsErr)
	}
	if jwsErr != nil {
		t.Fatalf("token server rejected the assertion: %v", jwsErr)
	}
	if tok != "sa-token" {
		t.Fatalf("token: %q", tok)
	}
}

func TestTokenFromMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/computeMetadata/v1/instance/service-accounts/default/token" {
			t.Fatalf("path: %q", r.URL.Path)
		}
		if r.Header.Get("Metadata-Flavor") != "Google" {
			t.Fatalf("metadata requests must carry Metadata-Flavor: Google")
		}
		w.Write([]byte(`{"access_token":"meta-token","expires_in":3600}`))
	}))
	defer srv.Close()

	s := &Source{metaBase: srv.URL, client: srv.Client()}
	tok, err := s.tokenFromMetadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok != "meta-token" {
		t.Fatalf("token: %q", tok)
	}
}

// --- configuration ---

func TestNewSourceConfigAndDefaults(t *testing.T) {
	keyJSON, _ := json.Marshal(map[string]any{"type": "service_account", "project_id": "file-proj"})
	keyFile := t.TempDir() + "/sa.json"
	if err := os.WriteFile(keyFile, keyJSON, 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", keyFile)
	t.Setenv("CONSIZE_GCP_PROJECT", "env-proj")
	t.Setenv("CONSIZE_DB_FILTER", "a, b ,c")
	s := NewSource()
	if s.project != "env-proj" {
		t.Fatalf("CONSIZE_GCP_PROJECT must win: %q", s.project)
	}
	if len(s.filter) != 3 || s.filter[0] != "a" || s.filter[1] != "b" || s.filter[2] != "c" {
		t.Fatalf("filter: %v", s.filter)
	}
	if s.sqlBase != "https://sqladmin.googleapis.com" || s.monBase != "https://monitoring.googleapis.com" ||
		s.metaBase != "http://metadata.google.internal" {
		t.Fatalf("endpoints: %+v", s)
	}
	if s.tokenFunc == nil {
		t.Fatal("tokenFunc must be wired")
	}

	t.Setenv("CONSIZE_GCP_PROJECT", "")
	s = NewSource()
	if s.project != "file-proj" {
		t.Fatalf("project must default to the key file's project_id (ADC): %q", s.project)
	}

	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	t.Setenv("CONSIZE_DB_FILTER", "")
	s = NewSource()
	if s.project != "" || len(s.filter) != 0 || s.tokenFunc == nil {
		t.Fatalf("no env: project must defer to metadata, filter empty: %+v", s)
	}
}
