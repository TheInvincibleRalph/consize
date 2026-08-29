package pricing

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"consize/internal/analysis"
)

// syntheticIndex mimics the EC2 offer-index shape with known math:
// sku1 2 vcpu / 8 GiB @ $0.096/hr, sku2 4 vcpu / 16 GiB @ $0.192/hr.
// sku3 (EBS-style, no vcpu) and sku4 ("N/A" memory) must be skipped.
const syntheticIndex = `{
  "products": {
    "sku1": {"attributes": {"vcpu": "2", "memory": "8 GiB"},
      "terms": {"OnDemand": {"t1": {"priceDimensions": {
        "d1": {"unit": "Hrs", "pricePerUnit": {"USD": "0.096"}}}}}}},
    "sku2": {"attributes": {"vcpu": "4", "memory": "16 GiB"},
      "terms": {"OnDemand": {"t1": {"priceDimensions": {
        "d1": {"unit": "Hrs", "pricePerUnit": {"USD": "0.192"}}}}}}},
    "sku3": {"attributes": {"memory": "1 GiB"},
      "terms": {"OnDemand": {"t1": {"priceDimensions": {
        "d1": {"unit": "Hrs", "pricePerUnit": {"USD": "0.05"}}}}}}},
    "sku4": {"attributes": {"vcpu": "1", "memory": "N/A"},
      "terms": {"OnDemand": {"t1": {"priceDimensions": {
        "d1": {"unit": "Hrs", "pricePerUnit": {"USD": "0.05"}}}}}}}
  }
}`

func TestParseIndex(t *testing.T) {
	p, err := ParseIndex([]byte(syntheticIndex))
	if err != nil {
		t.Fatal(err)
	}
	// per-core: 0.048, 0.048 → median 0.048 × 730 = 35.04
	if p.CPUPerCoreMonth != 35.04 {
		t.Fatalf("CPU: want 35.04, got %v", p.CPUPerCoreMonth)
	}
	// per-GiB: 0.012, 0.012 → × 730 = 8.76
	if p.MemPerGiBMonth != 8.76 {
		t.Fatalf("Mem: want 8.76, got %v", p.MemPerGiBMonth)
	}
}

func TestParseIndexEmpty(t *testing.T) {
	if _, err := ParseIndex([]byte(`{"products":{}}`)); err == nil {
		t.Fatal("want error for empty index")
	}
}

// countingService tracks fetch calls.
type countingService struct {
	n int
	p analysis.Prices
	e error
}

func (c *countingService) Prices(context.Context) (analysis.Prices, error) {
	c.n++
	return c.p, c.e
}

func TestCached(t *testing.T) {
	src := &countingService{p: DefaultStatic()}
	c := NewCached(src, time.Hour)

	p1, err := c.Prices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p2, _ := c.Prices(context.Background())
	if src.n != 1 {
		t.Fatalf("want 1 fetch for two calls within TTL, got %d", src.n)
	}
	if p1 != p2 {
		t.Fatal("cached result differs")
	}
}

func TestCachedRefreshesAfterTTL(t *testing.T) {
	src := &countingService{p: DefaultStatic()}
	c := NewCached(src, time.Nanosecond)
	_, _ = c.Prices(context.Background())
	time.Sleep(time.Millisecond)
	_, _ = c.Prices(context.Background())
	if src.n != 2 {
		t.Fatalf("want refresh after TTL, got %d fetches", src.n)
	}
}

func TestResilientFallsBack(t *testing.T) {
	failing := &countingService{e: errors.New("network down")}
	r := NewResilient(failing, DefaultStatic())
	p, err := r.Prices(context.Background())
	if err != nil {
		t.Fatalf("resilient must not fail: %v", err)
	}
	if p != DefaultStatic() {
		t.Fatal("want fallback prices")
	}
}

func TestResilientPassesThroughSuccess(t *testing.T) {
	custom := analysis.Prices{CPUPerCoreMonth: 1, MemPerGiBMonth: 2}
	ok := &countingService{p: custom}
	r := NewResilient(ok, DefaultStatic())
	p, err := r.Prices(context.Background())
	if err != nil || p != custom {
		t.Fatalf("want custom prices, got %v err=%v", p, err)
	}
}

// TestAWSSignature verifies fetchIndex sends a plausible SigV4 request:
// fixed clock, correct date header, AWS4-HMAC-SHA256 scheme with the
// pricing service in scope.
func TestAWSSignature(t *testing.T) {
	var gotAuth, gotDate string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotDate = r.Header.Get("Authorization"), r.Header.Get("X-Amz-Date")
		if r.URL.Path != "/offers/v1.0/aws/AmazonEC2/current/us-east-1/index.json" {
			t.Errorf("path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(syntheticIndex))
	}))
	defer srv.Close()

	a := &AWS{
		region: "us-east-1", baseURL: srv.URL, client: srv.Client(),
		accessKey: "AKIDEXAMPLE", secretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
		now: func() time.Time { return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC) },
	}
	p, err := a.Prices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.CPUPerCoreMonth != 35.04 {
		t.Fatalf("prices: %+v", p)
	}
	if gotDate != "20260801T120000Z" {
		t.Fatalf("x-amz-date: %q", gotDate)
	}
	if len(gotAuth) < 50 || !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 ") ||
		!strings.Contains(gotAuth, "/us-east-1/pricing/aws4_request") {
		t.Fatalf("authorization header: %q", gotAuth)
	}
}
