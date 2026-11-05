package service

import (
	"testing"
	"time"
)

func TestProviderBreakerTripThresholdAndWindow(t *testing.T) {
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	failures := []time.Time{now.Add(-4 * time.Minute), now.Add(-2 * time.Minute), now.Add(-time.Minute)}

	if providerBreakerShouldTrip(failures[:2], now, 3, 5*time.Minute) {
		t.Fatal("N-1 failures must not trip the breaker")
	}
	if !providerBreakerShouldTrip(failures, now, 3, 5*time.Minute) {
		t.Fatal("N failures within the window must trip the breaker")
	}
	if providerBreakerShouldTrip([]time.Time{now.Add(-6 * time.Minute), now.Add(-time.Minute), now}, now, 3, 5*time.Minute) {
		t.Fatal("a failure outside the window must not count")
	}
}
