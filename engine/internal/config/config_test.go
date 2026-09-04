package config

import (
	"testing"
	"time"
)

func TestStepScaledDuration(t *testing.T) {
	base := time.Hour
	for _, tt := range []struct {
		name string
		step int
		want time.Duration
	}{
		{name: "missing step uses first step", step: 0, want: time.Hour},
		{name: "first step uses base", step: 1, want: time.Hour},
		{name: "second step doubles base", step: 2, want: 2 * time.Hour},
		{name: "fourth step quadruples base", step: 4, want: 4 * time.Hour},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := StepScaledDuration(base, tt.step); got != tt.want {
				t.Fatalf("StepScaledDuration(%s, %d) = %s, want %s", base, tt.step, got, tt.want)
			}
		})
	}
}
