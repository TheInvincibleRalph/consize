// Command demo runs Consize's analysis engine against the shipped fixture
// workloads (docs/demo.md scenario) and prints the recommendation report:
// what's wasteful, what to consize it to, what it saves — and who was
// skipped and why.
//
// Usage:
//
//	cd engine && go run ./cmd/demo
package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"consize/internal/analysis"
	"consize/internal/fixtures"
)

func main() {
	workloads := fixtures.Workloads()
	res := analysis.Analyze(workloads, analysis.DefaultPrices())

	fmt.Println("Consize — demo run")
	fmt.Println("──────────────────────────────────────────────────────")
	fmt.Printf("Scanning %d workloads, 14 days of 15-minute buckets (shipped fixtures)\n", len(workloads))
	fmt.Println("Policy: request = p95×1.2 · limit = max(2×request, p99) · downsize-only · min 5 days data")
	fmt.Println()

	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "WORKLOAD\tRESOURCE\tCURRENT\tPROPOSED\tSAVINGS/MO\tCONFIDENCE")

	var total float64
	for _, r := range res.Recommendations {
		total += r.SavingsMonth
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t$%.2f\t%.2f\n",
			r.Workload,
			r.Resource,
			human(r.Resource, r.Current),
			human(r.Resource, r.Recommended),
			r.SavingsMonth,
			r.Confidence,
		)
	}
	tw.Flush()

	fmt.Printf("\nTOTAL PROJECTED: $%.2f / month across %d recommendations\n", total, len(res.Recommendations))
	fmt.Printf("\nSkipped (%d):\n", len(res.Skipped))
	for _, s := range res.Skipped {
		fmt.Printf("  - %s [%s]: %s\n", s.Workload, s.Namespace, s.Reason)
	}
	fmt.Println("\nConsize your infrastructure.")
}

// human renders a resource value for display.
func human(resource string, v int64) string {
	switch resource {
	case analysis.ResourceMemory:
		if v >= analysis.GiB {
			return fmt.Sprintf("%.2f GiB", float64(v)/float64(analysis.GiB))
		}
		return fmt.Sprintf("%d MiB", v/analysis.MiB)
	case analysis.ResourceCPU:
		return fmt.Sprintf("%.2f cores", float64(v)/1000)
	}
	return fmt.Sprintf("%d", v)
}
