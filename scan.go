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
	Commits       []string
	Force         bool
	DryRun        bool
	Limit         int
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
	if !options.DryRun {
		lock, err := acquireScanLock(repository.LockPath())
		if err != nil {
			return err
		}
		defer lock.Close()
	}

	commits := append([]string(nil), options.Commits...)
	var err error
	if len(commits) > 0 {
		// Explicit commits are already resolved and validated by the caller.
	} else if options.RevisionRange == "" {
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
	remaining := commits
	if !options.Force {
		processed, err := store.ProcessedSHAs(ctx)
		if err != nil {
			return err
		}
		remaining = make([]string, 0, len(commits))
		for _, sha := range commits {
			if _, exists := processed[sha]; !exists {
				remaining = append(remaining, sha)
			}
		}
	}
	if options.Limit > 0 && len(remaining) > options.Limit {
		remaining = remaining[:options.Limit]
	}
	if len(remaining) == 0 {
		if options.DryRun {
			fmt.Fprintln(options.Output, "No pending commits.")
		} else {
			fmt.Fprintln(options.Output, "No unprocessed commits.")
		}
		return nil
	}

	now := options.Now
	if now == nil {
		now = time.Now
	}
	var reviewer Reviewer
	var identity ReviewIdentity
	reviewableCount := 0
	skippedCount := 0
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
		skipReason := diffSkipReason(diff)
		if options.DryRun {
			if skipReason == "" {
				reviewableCount++
				fmt.Fprintf(options.Output, "%s  review\n", shortSHA(sha))
			} else {
				skippedCount++
				fmt.Fprintf(options.Output, "%s  skip: %s\n", shortSHA(sha), skipReason)
			}
			continue
		}
		if skipReason != "" {
			if options.Force {
				return fmt.Errorf("cannot rescan commit %s: %s", shortSHA(sha), skipReason)
			}
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
		var openFindings []Finding
		if options.Force {
			openFindings, err = store.OpenFindingsExcludingCommit(ctx, sha)
		} else {
			openFindings, err = store.OpenFindings(ctx)
		}
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
	if options.DryRun {
		commitLabel := "commits"
		if len(remaining) == 1 {
			commitLabel = "commit"
		}
		fmt.Fprintf(options.Output, "Pending: %d %s (%d reviewable, %d skipped)\n",
			len(remaining), commitLabel, reviewableCount, skippedCount)
	}
	return nil
}

func diffSkipReason(diff DiffResult) string {
	switch {
	case diff.Oversized:
		return "textual diff exceeds 256 KiB"
	case len(diff.BinaryFiles) > 0 && diff.Text == "":
		return "binary-only diff"
	case diff.Empty:
		return "empty diff"
	default:
		return ""
	}
}
