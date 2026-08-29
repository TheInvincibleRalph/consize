package costscan

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
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"consize/internal/config"
	"consize/internal/store"
)

const gcpScope = "https://www.googleapis.com/auth/cloud-platform"
const gcpTokenURI = "https://oauth2.googleapis.com/token"

var terraformAddrSafe = regexp.MustCompile(`[^a-z0-9_]+`)

type GCPSource struct {
	project   string
	base      string
	metaBase  string
	client    *http.Client
	tokenFunc func(context.Context) (string, error)
	now       func() time.Time
}

func NewGCPSource() *GCPSource {
	return &GCPSource{
		project:  config.Str("CONSIZE_GCP_PROJECT", ""),
		base:     "https://compute.googleapis.com",
		metaBase: "http://metadata.google.internal",
		client:   &http.Client{Timeout: 60 * time.Second},
		now:      func() time.Time { return time.Now().UTC() },
	}
}

func (s *GCPSource) Scan(ctx context.Context) ([]store.CostOpportunity, error) {
	if s.client == nil {
		s.client = &http.Client{Timeout: 60 * time.Second}
	}
	if s.base == "" {
		s.base = "https://compute.googleapis.com"
	}
	if s.metaBase == "" {
		s.metaBase = "http://metadata.google.internal"
	}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.tokenFunc == nil {
		s.tokenFunc = s.tokenFromADC
	}
	if s.project == "" {
		s.project = projectFromSAKey()
	}
	if s.project == "" {
		if p, err := s.metadataProjectID(ctx); err == nil {
			s.project = p
		}
	}
	if s.project == "" {
		return nil, fmt.Errorf("CONSIZE_GCP_PROJECT is required when credentials/metadata do not name a project")
	}

	disks, err := s.scanDisks(ctx)
	if err != nil {
		return nil, err
	}
	instances, err := s.scanStoppedInstances(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]store.CostOpportunity, 0, len(disks)+len(instances))
	out = append(out, disks...)
	out = append(out, instances...)
	return out, nil
}

type gcpServiceAccountKey struct {
	ProjectID   string `json:"project_id"`
	PrivateKey  string `json:"private_key"`
	ClientEmail string `json:"client_email"`
	TokenURI    string `json:"token_uri"`
}

func projectFromSAKey() string {
	p := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
	if p == "" {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	var k gcpServiceAccountKey
	if err := json.Unmarshal(b, &k); err != nil {
		return ""
	}
	return k.ProjectID
}

func (s *GCPSource) tokenFromADC(ctx context.Context) (string, error) {
	if p := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"); p != "" {
		return s.tokenFromSAKey(ctx, p)
	}
	return s.tokenFromMetadata(ctx)
}

func (s *GCPSource) tokenFromSAKey(ctx context.Context, keyPath string) (string, error) {
	b, err := os.ReadFile(keyPath)
	if err != nil {
		return "", fmt.Errorf("read service-account key %s: %w", keyPath, err)
	}
	var k gcpServiceAccountKey
	if err := json.Unmarshal(b, &k); err != nil {
		return "", fmt.Errorf("decode service-account key %s: %w", keyPath, err)
	}
	key, err := parseGCPRSAKey(k.PrivateKey)
	if err != nil {
		return "", err
	}
	uri := k.TokenURI
	if uri == "" {
		uri = gcpTokenURI
	}
	now := time.Now()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{
		"iss":   k.ClientEmail,
		"scope": gcpScope,
		"aud":   uri,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	})
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uri, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
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
		return "", err
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("token endpoint: empty access_token")
	}
	return tok.AccessToken, nil
}

func parseGCPRSAKey(pemText string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, fmt.Errorf("service-account key has no PEM block")
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rk, ok := k.(*rsa.PrivateKey); ok {
			return rk, nil
		}
		return nil, fmt.Errorf("service-account key is not RSA")
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

func (s *GCPSource) tokenFromMetadata(ctx context.Context) (string, error) {
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := s.metadataJSON(ctx, "/computeMetadata/v1/instance/service-accounts/default/token", &tok); err != nil {
		return "", err
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("metadata token: empty access_token")
	}
	return tok.AccessToken, nil
}

func (s *GCPSource) metadataProjectID(ctx context.Context) (string, error) {
	b, err := s.metadata(ctx, "/computeMetadata/v1/project/project-id")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func (s *GCPSource) metadata(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.metaBase+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("metadata %s: status %s: %s", path, resp.Status, b)
	}
	return io.ReadAll(resp.Body)
}

func (s *GCPSource) metadataJSON(ctx context.Context, path string, v any) error {
	b, err := s.metadata(ctx, path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

type gcpAggregatedDisks struct {
	Items map[string]struct {
		Disks []gcpDisk `json:"disks"`
	} `json:"items"`
	NextPageToken string `json:"nextPageToken"`
}

type gcpDisk struct {
	ID                  string   `json:"id"`
	Name                string   `json:"name"`
	SelfLink            string   `json:"selfLink"`
	Zone                string   `json:"zone"`
	Type                string   `json:"type"`
	Status              string   `json:"status"`
	SizeGB              string   `json:"sizeGb"`
	Users               []string `json:"users"`
	CreationTimestamp   string   `json:"creationTimestamp"`
	LastAttachTimestamp string   `json:"lastAttachTimestamp"`
}

func (s *GCPSource) scanDisks(ctx context.Context) ([]store.CostOpportunity, error) {
	var out []store.CostOpportunity
	page := ""
	for {
		q := url.Values{}
		if page != "" {
			q.Set("pageToken", page)
		}
		body, err := s.get(ctx, "/compute/v1/projects/"+url.PathEscape(s.project)+"/aggregated/disks", q)
		if err != nil {
			return nil, fmt.Errorf("list gcp disks: %w", err)
		}
		var resp gcpAggregatedDisks
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("decode gcp disks: %w", err)
		}
		for _, scoped := range resp.Items {
			for _, d := range scoped.Disks {
				if !isUnattachedDisk(d) {
					continue
				}
				size := parseFloat(d.SizeGB)
				diskType := lastPath(d.Type)
				monthly := size * gcpDiskPriceGiBMonth(diskType)
				seen := parseGCPTime(d.LastAttachTimestamp)
				if seen.IsZero() {
					seen = parseGCPTime(d.CreationTimestamp)
				}
				if seen.IsZero() {
					seen = s.now()
				}
				out = append(out, store.CostOpportunity{
					Provider:       "gcp",
					Account:        s.project,
					Region:         zoneRegion(lastPath(d.Zone)),
					ResourceType:   TypeUnattachedVolume,
					ResourceID:     orDefaultString(d.SelfLink, d.ID),
					Name:           d.Name,
					MonthlyCost:    monthly,
					Recommendation: "Delete detached Persistent Disk after snapshot/owner confirmation.",
					Action:         "delete_disk",
					Risk:           "medium",
					Status:         store.OpportunityOpen,
					Evidence: map[string]any{
						"disk_type":        diskType,
						"size_gib":         size,
						"status":           d.Status,
						"attached_to_vms":  len(d.Users),
						"last_attach_time": nonEmpty(d.LastAttachTimestamp, "unknown"),
					},
					IaCPath:       "terraform/compute.tf",
					TerraformAddr: terraformAddr("google_compute_disk", d.Name),
					FirstSeenAt:   seen,
					LastSeenAt:    s.now(),
				})
			}
		}
		if resp.NextPageToken == "" {
			return out, nil
		}
		page = resp.NextPageToken
	}
}

func isUnattachedDisk(d gcpDisk) bool {
	return strings.EqualFold(d.Status, "READY") && len(d.Users) == 0 && d.Name != ""
}

type gcpAggregatedInstances struct {
	Items map[string]struct {
		Instances []gcpInstance `json:"instances"`
	} `json:"items"`
	NextPageToken string `json:"nextPageToken"`
}

type gcpInstance struct {
	ID                string            `json:"id"`
	Name              string            `json:"name"`
	SelfLink          string            `json:"selfLink"`
	Zone              string            `json:"zone"`
	Status            string            `json:"status"`
	CreationTimestamp string            `json:"creationTimestamp"`
	Labels            map[string]string `json:"labels"`
	Disks             []struct {
		Type       string `json:"type"`
		Boot       bool   `json:"boot"`
		Source     string `json:"source"`
		DiskSizeGB string `json:"diskSizeGb"`
	} `json:"disks"`
}

func (s *GCPSource) scanStoppedInstances(ctx context.Context) ([]store.CostOpportunity, error) {
	var out []store.CostOpportunity
	page := ""
	for {
		q := url.Values{}
		if page != "" {
			q.Set("pageToken", page)
		}
		body, err := s.get(ctx, "/compute/v1/projects/"+url.PathEscape(s.project)+"/aggregated/instances", q)
		if err != nil {
			return nil, fmt.Errorf("list gcp instances: %w", err)
		}
		var resp gcpAggregatedInstances
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("decode gcp instances: %w", err)
		}
		for _, scoped := range resp.Items {
			for _, inst := range scoped.Instances {
				if !strings.EqualFold(inst.Status, "TERMINATED") || inst.Name == "" {
					continue
				}
				var diskGiB float64
				var persistentDisks int
				for _, d := range inst.Disks {
					if !strings.EqualFold(d.Type, "PERSISTENT") {
						continue
					}
					persistentDisks++
					diskGiB += parseFloat(d.DiskSizeGB)
				}
				monthly := diskGiB * gcpDiskPriceGiBMonth("pd-balanced")
				if monthly <= 0 {
					continue
				}
				created := parseGCPTime(inst.CreationTimestamp)
				if created.IsZero() {
					created = s.now()
				}
				out = append(out, store.CostOpportunity{
					Provider:       "gcp",
					Account:        s.project,
					Region:         zoneRegion(lastPath(inst.Zone)),
					ResourceType:   TypeStoppedInstance,
					ResourceID:     orDefaultString(inst.SelfLink, inst.ID),
					Name:           inst.Name,
					MonthlyCost:    monthly,
					Recommendation: "Terminate stopped VM after owner confirmation; attached Persistent Disks continue to accrue cost.",
					Action:         "terminate_instance",
					Risk:           "low",
					Status:         store.OpportunityOpen,
					Evidence: map[string]any{
						"state":            inst.Status,
						"persistent_disks": persistentDisks,
						"disk_gib":         diskGiB,
						"cost_basis":       "attached persistent disk estimate",
					},
					IaCPath:       "terraform/compute.tf",
					TerraformAddr: terraformAddr("google_compute_instance", inst.Name),
					FirstSeenAt:   created,
					LastSeenAt:    s.now(),
				})
			}
		}
		if resp.NextPageToken == "" {
			return out, nil
		}
		page = resp.NextPageToken
	}
}

func (s *GCPSource) get(ctx context.Context, path string, q url.Values) ([]byte, error) {
	tok, err := s.tokenFunc(ctx)
	if err != nil {
		return nil, fmt.Errorf("token: %w", err)
	}
	u := s.base + path
	if encoded := q.Encode(); encoded != "" {
		u += "?" + encoded
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
		return nil, fmt.Errorf("status %s: %s", resp.Status, b)
	}
	return io.ReadAll(resp.Body)
}

func gcpDiskPriceGiBMonth(t string) float64 {
	switch t {
	case "pd-standard":
		return 0.04
	case "pd-ssd":
		return 0.17
	case "pd-extreme":
		return 0.125
	default:
		return 0.10 // pd-balanced and unknown persistent disk classes
	}
}

func parseFloat(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f
}

func parseGCPTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t.UTC()
}

func lastPath(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

func zoneRegion(zone string) string {
	parts := strings.Split(zone, "-")
	if len(parts) < 2 {
		return zone
	}
	return strings.Join(parts[:len(parts)-1], "-")
}

func terraformAddr(kind, name string) string {
	safe := strings.Trim(strings.ToLower(terraformAddrSafe.ReplaceAllString(name, "_")), "_")
	if safe == "" {
		safe = "resource"
	}
	return kind + "." + safe
}

func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func orDefaultString(s, fallback string) string {
	if strings.TrimSpace(s) != "" {
		return s
	}
	return fallback
}
