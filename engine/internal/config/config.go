// Package config reads Consize's environment configuration. Every knob
// has a sane default so the binaries run zero-config against the demo
// path (in-memory store) and only need DATABASE_URL / PROMETHEUS_URL etc.
// to point at real infrastructure.
package config

import (
	"os"
	"strconv"
	"time"
)

// Str returns the env value or def.
func Str(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Duration returns the env value as a duration ("15m", "1h") or def.
func Duration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// StepScaledDuration returns the observation window for an apply step.
// Step 1 uses the base duration, step 2 uses 2x, and so on. Missing or
// malformed step numbers fail to the safest first-step behavior.
func StepScaledDuration(base time.Duration, step int) time.Duration {
	if step < 1 {
		step = 1
	}
	return base * time.Duration(step)
}

// Int returns the env value as an int or def.
func Int(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// Float returns the env value as a float or def.
func Float(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

// Bool returns the env value as a bool ("1", "true", "TRUE", "yes" all
// count; anything else is false) or def.
func Bool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}
