package main

import (
	"context"
	"fmt"
	"time"
)

type findingDisplayMetadata struct {
	CommitDate time.Time
	Blame      string
}

func loadFindingDisplayMetadata(
	ctx context.Context,
	repository *GitRepository,
	findings []Finding,
) (map[int64]findingDisplayMetadata, error) {
	display := make(map[int64]findingDisplayMetadata, len(findings))
	if len(findings) == 0 {
		return display, nil
	}
	attributions, err := repository.MasterCommitAttributions(ctx)
	if err != nil {
		return nil, err
	}
	for _, finding := range findings {
		attribution := attributions[finding.IntroducedSHA]
		display[finding.ID] = findingDisplayMetadata{
			CommitDate: attribution.Date,
			Blame:      attribution.Author,
		}
	}
	return display, nil
}

func formatFindingAge(now, commitDate time.Time) string {
	if commitDate.IsZero() {
		return "?"
	}
	age := now.Sub(commitDate)
	if age < 0 {
		age = 0
	}
	switch {
	case age < time.Minute:
		return "<1m"
	case age < time.Hour:
		return fmt.Sprintf("%dm", int(age/time.Minute))
	case age < 24*time.Hour:
		return fmt.Sprintf("%dh", int(age/time.Hour))
	case age < 30*24*time.Hour:
		return fmt.Sprintf("%dd", int(age/(24*time.Hour)))
	case age < 365*24*time.Hour:
		return fmt.Sprintf("%dmo", int(age/(30*24*time.Hour)))
	default:
		return fmt.Sprintf("%dy", int(age/(365*24*time.Hour)))
	}
}

func findingBlame(display findingDisplayMetadata) string {
	if display.Blame == "" {
		return "(unknown)"
	}
	return display.Blame
}
