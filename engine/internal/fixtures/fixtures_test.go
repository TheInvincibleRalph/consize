package fixtures

import (
	"testing"
)

// TestFixturesDataQuality guards the shipped fixture set: day-aligned
// buckets with full coverage. This is the test that would have caught the
// mid-day-epoch bug (a spike straddling two day-keys silently turned a
// bursty workload into "already optimal").
//
// beta-service is the documented exception: it is intentionally truncated
// to 3 days of data so the insufficient-data skip path is exercised.
func TestFixturesDataQuality(t *testing.T) {
	for _, w := range Workloads() {
		t.Run(w.Name, func(t *testing.T) {
			byDay := map[int64]int{}
			for _, b := range w.Buckets {
				byDay[b.WindowStart/86400]++
			}
			if w.Name == "beta-service" {
				if len(byDay) != 3 {
					t.Errorf("beta-service must have exactly 3 days (insufficient-data fixture), got %d", len(byDay))
				}
			} else if len(byDay) != demoDays {
				t.Errorf("want %d distinct days, got %d", demoDays, len(byDay))
			}
			for day, n := range byDay {
				if n != bucketsPerDay {
					t.Errorf("day %d has %d buckets, want %d (day alignment broken?)", day, n, bucketsPerDay)
				}
			}
		})
	}
}
