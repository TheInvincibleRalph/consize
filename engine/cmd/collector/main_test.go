package main

import (
	"strings"
	"testing"

	"consize/internal/dbmetrics"
	"consize/internal/dbmetrics/cloudmonitoring"
	"consize/internal/dbmetrics/cloudwatch"
)

// TestDBSourceFor pins the CONSIZE_DBMETRICS switch: each shipped source
// constructs its adapter, unset/none means k8s only, and unknown values
// fail fast.
func TestDBSourceFor(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	t.Setenv("CONSIZE_GCP_PROJECT", "")

	cases := []struct {
		kind    string
		adapter bool // false = no database surface (k8s only)
	}{
		{"", false},
		{"none", false},
		{"fixture", true},
		{"cloudwatch", true},
		{"gcp", true},
	}
	for _, c := range cases {
		src, via, err := dbSourceFor(c.kind)
		if err != nil {
			t.Fatalf("dbSourceFor(%q): %v", c.kind, err)
		}
		if !c.adapter {
			if src != nil || via != "" {
				t.Fatalf("dbSourceFor(%q): want no source, got %T via=%q", c.kind, src, via)
			}
			continue
		}
		// The constructed source must be the expected concrete adapter.
		var ok bool
		switch c.kind {
		case "fixture":
			_, ok = src.(*dbmetrics.Fixture)
		case "cloudwatch":
			_, ok = src.(*cloudwatch.Source)
		case "gcp":
			_, ok = src.(*cloudmonitoring.Source)
		}
		if !ok {
			t.Fatalf("dbSourceFor(%q): %T", c.kind, src)
		}
		if !strings.Contains(via, c.kind) {
			t.Fatalf("dbSourceFor(%q) via: %q", c.kind, via)
		}
	}

	if src, via, err := dbSourceFor("bogus"); err == nil {
		t.Fatalf("unknown source must fail fast: src=%v via=%q", src, via)
	}
}

// TestDBSourceForGCPEnv: CONSIZE_DBMETRICS=gcp reads the GCP project
// configuration like a live run would.
func TestDBSourceForGCPEnv(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	t.Setenv("CONSIZE_GCP_PROJECT", "acme-prod")
	t.Setenv("CONSIZE_DB_FILTER", "payments-prod")

	src, _, err := dbSourceFor("gcp")
	if err != nil {
		t.Fatal(err)
	}
	s, ok := src.(*cloudmonitoring.Source)
	if !ok {
		t.Fatalf("dbSourceFor(gcp): %T", src)
	}
	if s.Project() != "acme-prod" {
		t.Fatalf("project: %q", s.Project())
	}
}
