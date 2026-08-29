package pricing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"consize/internal/analysis"
)

// AWS implements Service against the AWS Price List API: it fetches the
// EC2 on-demand offer index for a region and derives a per-core and
// per-GiB monthly rate as the median across instance families.
type AWS struct {
	region  string
	baseURL string // override in tests
	client  *http.Client

	accessKey, secretKey, sessionToken string
	// now injectable for signature tests; zero = time.Now.
	now func() time.Time
}

// NewAWS builds an AWS pricing client from the standard env credentials
// (AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY / AWS_SESSION_TOKEN).
func NewAWS(region string) *AWS {
	return &AWS{
		region:       region,
		baseURL:      "https://api.pricing.us-east-1.amazonaws.com",
		client:       &http.Client{Timeout: 60 * time.Second},
		accessKey:    os.Getenv("AWS_ACCESS_KEY_ID"),
		secretKey:    os.Getenv("AWS_SECRET_ACCESS_KEY"),
		sessionToken: os.Getenv("AWS_SESSION_TOKEN"),
	}
}

// AWSSigner signs one AWS API request with SigV4 (payload-hash = SHA-256
// of the body). It is shared by the pricing client and the CloudWatch/RDS
// adapter (dbmetrics/cloudwatch) so credential handling and request
// signing live in one place — the repo stays dependency-light, no AWS SDK.
type AWSSigner struct {
	AccessKey    string
	SecretKey    string
	SessionToken string
	Region       string
	Service      string // e.g. "pricing", "rds", "monitoring"
	// Now injects the clock for signature tests; zero = time.Now.
	Now func() time.Time
}

// Sign returns the SigV4 headers (X-Amz-Date, X-Amz-Security-Token when
// a session token is set, Authorization) for a request to the given host
// and path with the given payload. extra names additional headers that
// must be covered by the signature (e.g. x-amz-target, content-type);
// they are sorted so the canonical request is deterministic. Values are
// trimmed per SigV4's canonical-header rules.
func (s *AWSSigner) Sign(method, host, path, payload string, extra map[string]string) http.Header {
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	canonicalHeaders := "host:" + host + "\n"
	signedHeaders := "host"
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, strings.ToLower(k))
	}
	sort.Strings(keys)
	for _, k := range keys {
		canonicalHeaders += k + ":" + strings.TrimSpace(extra[k]) + "\n"
		signedHeaders += ";" + k
	}
	canonicalHeaders += "x-amz-date:" + amzDate + "\n"
	signedHeaders += ";x-amz-date"
	if s.SessionToken != "" {
		canonicalHeaders += "x-amz-security-token:" + s.SessionToken + "\n"
		signedHeaders += ";x-amz-security-token"
	}
	canonicalRequest := method + "\n" + path + "\n" + "\n" + canonicalHeaders + "\n" +
		signedHeaders + "\n" + sha256Hex(payload)

	scope := dateStamp + "/" + s.Region + "/" + s.Service + "/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + sha256Hex(canonicalRequest)

	k := hmacSHA256([]byte("AWS4"+s.SecretKey), dateStamp)
	k = hmacSHA256(k, s.Region)
	k = hmacSHA256(k, s.Service)
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, stringToSign))

	hdr := http.Header{}
	hdr.Set("X-Amz-Date", amzDate)
	if s.SessionToken != "" {
		hdr.Set("X-Amz-Security-Token", s.SessionToken)
	}
	hdr.Set("Authorization", "AWS4-HMAC-SHA256 "+
		"Credential="+s.AccessKey+"/"+scope+", "+
		"SignedHeaders="+signedHeaders+", Signature="+sig)
	for k, v := range extra {
		hdr.Set(k, v)
	}
	return hdr
}

// Prices implements Service.
func (a *AWS) Prices(ctx context.Context) (analysis.Prices, error) {
	body, err := a.fetchIndex(ctx, "AmazonEC2")
	if err != nil {
		return analysis.Prices{}, fmt.Errorf("fetch EC2 price index: %w", err)
	}
	p, err := ParseIndex(body)
	if err != nil {
		return analysis.Prices{}, err
	}
	if p.CPUPerCoreMonth <= 0 || p.MemPerGiBMonth <= 0 {
		return analysis.Prices{}, fmt.Errorf("price index yielded non-positive rates: %+v", p)
	}
	return p, nil
}

// fetchIndex GETs the offer index with SigV4 signing.
func (a *AWS) fetchIndex(ctx context.Context, offer string) ([]byte, error) {
	path := fmt.Sprintf("/offers/v1.0/aws/%s/current/%s/index.json", offer, a.region)
	url := a.baseURL + path

	signer := &AWSSigner{
		AccessKey: a.accessKey, SecretKey: a.secretKey, SessionToken: a.sessionToken,
		Region: a.region, Service: "pricing", Now: a.now,
	}
	amzHeaders := signer.Sign(http.MethodGet, hostOf(a.baseURL), path, "", nil)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, vs := range amzHeaders {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	resp, err := a.client.Do(req)
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

// ParseIndex derives the median on-demand $/vcpu-hr and $/GiB-hr from an
// EC2 offer index, converted to monthly rates (×730h). Pure, unit-tested.
func ParseIndex(body []byte) (analysis.Prices, error) {
	var idx struct {
		Products map[string]struct {
			Attributes struct {
				VCPU string `json:"vcpu"`
				Mem  string `json:"memory"`
			} `json:"attributes"`
			Terms map[string]map[string]struct {
				PriceDimensions map[string]struct {
					Unit         string `json:"unit"`
					PricePerUnit struct {
						USD string `json:"USD"`
					} `json:"pricePerUnit"`
				} `json:"priceDimensions"`
			} `json:"terms"`
		} `json:"products"`
	}
	if err := json.Unmarshal(body, &idx); err != nil {
		return analysis.Prices{}, fmt.Errorf("decode offer index: %w", err)
	}

	var perCore, perGiB []float64
	for _, p := range idx.Products {
		vcpu, ok := atoi(p.Attributes.VCPU)
		if !ok || vcpu <= 0 {
			continue
		}
		memGiB, ok := parseGiB(p.Attributes.Mem)
		if !ok || memGiB <= 0 {
			continue
		}
		ondemand, ok := p.Terms["OnDemand"]
		if !ok {
			continue
		}
		price := 0.0
		for _, term := range ondemand {
			for _, dim := range term.PriceDimensions {
				if dim.Unit != "Hrs" {
					continue
				}
				if v, err := strconv.ParseFloat(dim.PricePerUnit.USD, 64); err == nil {
					price = v
				}
				break
			}
			if price > 0 {
				break
			}
		}
		if price <= 0 {
			continue
		}
		perCore = append(perCore, price/float64(vcpu))
		perGiB = append(perGiB, price/memGiB)
	}
	if len(perCore) == 0 || len(perGiB) == 0 {
		return analysis.Prices{}, fmt.Errorf("no usable EC2 on-demand products in index")
	}
	return analysis.Prices{
		CPUPerCoreMonth: median(perCore) * 730,
		MemPerGiBMonth:  median(perGiB) * 730,
	}, nil
}

func parseGiB(s string) (float64, bool) {
	// Attributes look like "8 GiB"; some families have "N/A".
	s = strings.TrimSpace(strings.TrimSuffix(s, " GiB"))
	if s == "" || s == "N/A" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	return v, err == nil
}

func atoi(s string) (int, bool) {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	return v, err == nil
}

func median(vs []float64) float64 {
	sort.Float64s(vs)
	n := len(vs)
	if n%2 == 1 {
		return vs[n/2]
	}
	return (vs[n/2-1] + vs[n/2]) / 2
}

func hostOf(u string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	return s
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}
