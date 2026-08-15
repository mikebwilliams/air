package main

import (
	"context"
	"fmt"
	"io"
	"time"
)

type reviewerFactory func() (Reviewer, ReviewIdentity, error)

type scanOptions struct {
	RevisionRange string
	Limit         int
	Pricing       *Pricing
	Output        io.Writer
	Now           func() time.Time
	NewReviewer   reviewerFactory
}

func scanRepository(
	ctx context.Context,
	repository *GitRepository,
	store *Store,
	options scanOptions,
) error {
	if options.Limit < 0 {
		return fmt.Errorf("scan limit must not be negative")
	}
	lock, err := acquireScanLock(repository.LockPath())
	if err != nil {
		return err
	}
	defer lock.Close()

	var commits []string
	if options.RevisionRange == "" {
		startSHA, err := store.Config(ctx, "start_sha")
		if err != nil {
			return err
		}
		commits, err = repository.EnumerateDefault(ctx, startSHA)
		if err != nil {
			return err
		}
	} else {
		commits, err = repository.EnumerateRange(ctx, options.RevisionRange)
		if err != nil {
			return err
		}
	}
	processed, err := store.ProcessedSHAs(ctx)
	if err != nil {
		return err
	}
	remaining := make([]string, 0, len(commits))
	for _, sha := range commits {
		if _, exists := processed[sha]; !exists {
			remaining = append(remaining, sha)
		}
	}
	if options.Limit > 0 && len(remaining) > options.Limit {
		remaining = remaining[:options.Limit]
	}
	if len(remaining) == 0 {
		fmt.Fprintln(options.Output, "No unprocessed commits.")
		return nil
	}

	now := options.Now
	if now == nil {
		now = time.Now
	}
	var reviewer Reviewer
	var identity ReviewIdentity
	for _, sha := range remaining {
		if err := ctx.Err(); err != nil {
			return err
		}
		metadata, err := repository.CommitMetadata(ctx, sha)
		if err != nil {
			return err
		}
		diff, err := repository.CommitDiff(ctx, metadata.ParentSHA, metadata.SHA)
		if err != nil {
			return err
		}
		skipReason := ""
		switch {
		case diff.Oversized:
			skipReason = "textual diff exceeds 256 KiB"
		case len(diff.BinaryFiles) > 0 && diff.Text == "":
			skipReason = "binary-only diff"
		case diff.Empty:
			skipReason = "empty diff"
		}
		if skipReason != "" {
			if err := store.InsertSkipped(ctx, metadata, skipReason, now()); err != nil {
				return err
			}
			fmt.Fprintf(options.Output, "%s  skipped: %s\n", shortSHA(sha), skipReason)
			continue
		}

		if reviewer == nil {
			if options.NewReviewer == nil {
				return fmt.Errorf("no model reviewer is configured")
			}
			reviewer, identity, err = options.NewReviewer()
			if err != nil {
				return err
			}
		}
		openFindings, err := store.OpenFindings(ctx)
		if err != nil {
			return err
		}
		result, err := reviewer.Review(ctx, ReviewInput{
			Commit:       metadata,
			Diff:         diff.Text,
			OpenFindings: openFindings,
		})
		if err != nil {
			return fmt.Errorf("review commit %s: %w", shortSHA(sha), err)
		}
		allowed := make(map[int64]struct{}, len(openFindings))
		for _, finding := range openFindings {
			allowed[finding.ID] = struct{}{}
		}
		if err := validateReviewOutput(result.Output, allowed); err != nil {
			return fmt.Errorf("review commit %s: %w", shortSHA(sha), err)
		}
		if result.Usage == nil {
			return fmt.Errorf("review commit %s: reviewer did not report token usage", shortSHA(sha))
		}
		if options.Pricing != nil {
			cost, err := options.Pricing.EstimateMicrousd(*result.Usage)
			if err != nil {
				return fmt.Errorf("review commit %s: estimate cost: %w", shortSHA(sha), err)
			}
			result.EstimatedCostMicrousd = &cost
		}
		newIDs, err := store.ApplyReview(ctx, metadata, identity, result, now())
		if err != nil {
			return err
		}
		fmt.Fprintf(
			options.Output,
			"%s  %d new, %d resolved\n",
			shortSHA(sha),
			len(newIDs),
			len(result.Output.ResolvedFindings),
		)
	}
	return nil
}
