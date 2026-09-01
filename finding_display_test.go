package main

import (
	"testing"
	"time"
)

func TestFormatFindingAge(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		openedAt time.Time
		want     string
	}{
		{name: "unknown", want: "?"},
		{name: "future", openedAt: now.Add(time.Hour), want: "<1m"},
		{name: "minutes", openedAt: now.Add(-23 * time.Minute), want: "23m"},
		{name: "hours", openedAt: now.Add(-7 * time.Hour), want: "7h"},
		{name: "days", openedAt: now.Add(-12 * 24 * time.Hour), want: "12d"},
		{name: "months", openedAt: now.Add(-91 * 24 * time.Hour), want: "3mo"},
		{name: "years", openedAt: now.Add(-800 * 24 * time.Hour), want: "2y"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := formatFindingAge(now, test.openedAt); got != test.want {
				t.Fatalf("formatFindingAge() = %q, want %q", got, test.want)
			}
		})
	}
}
