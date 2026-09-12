package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"time"
)

type auditRunOptions struct {
	Jobs          int
	Limit         int
	Duration      time.Duration
	Timeout       time.Duration // zero uses the saved scan timeout
	RetryFailed   bool
	throttleDelay time.Duration // tests can exercise cooldowns without waiting minutes
}

func runAudit(ctx context.Context, store *inventoryStore, repository *GitRepository, scan auditScan, options auditRunOptions, runner auditRunner, progress io.Writer) (returnedErr error) {
	if options.Jobs < 1 || options.Jobs > 32 || options.Limit < 0 || options.Duration < 0 {
		return errors.New("jobs must be 1..32; limit and duration must not be negative")
	}
	if options.Timeout < 0 {
		return errors.New("timeout override must be positive")
	}
	modelConfig := scan.Spec.Model
	if options.Timeout > 0 {
		modelConfig.Timeout = options.Timeout
	}
	if modelConfig.Timeout <= 0 {
		return errors.New("per-assignment timeout must be positive")
	}
	if runner == nil {
		return errors.New("no assignment runner")
	}
	expectedPrompt := auditPromptVersion
	unit := "assignment"
	if scan.Spec.Recheck != nil {
		expectedPrompt = auditRecheckPromptVersion
		unit = "batch"
		if scan.Spec.PromptVersion == auditRecheckPromptVersionV1 {
			expectedPrompt = auditRecheckPromptVersionV1
			unit = "assignment"
		}
	}
	if scan.Spec.PromptVersion != expectedPrompt {
		return fmt.Errorf("unsupported scan prompt version %q", scan.Spec.PromptVersion)
	}
	if scan.Status == "invalid" {
		return errors.New("scan was invalidated by a checkout change; create a new scan")
	}
	if progress == nil {
		progress = io.Discard
	}
	database, err := reposeDatabasePath(ctx, repository)
	if err != nil {
		return err
	}
	lock, err := acquireScanLock(filepath.Join(filepath.Dir(database), "scan.lock"))
	if err != nil {
		return fmt.Errorf("cannot run scan: %w", err)
	}
	defer lock.Close()
	record, err := store.Inventory(ctx, scan.Spec.Plan.InventoryID)
	if err != nil {
		return err
	}
	if err = checkInventory(ctx, repository, record.Inventory); err != nil {
		return err
	}
	pendingLimits, err := store.auditPendingLimits(ctx, scan.ID)
	if err != nil {
		return err
	}
	if err = store.recoverAudit(ctx, scan, options.RetryFailed, time.Now()); err != nil {
		return err
	}
	fmt.Fprintf(progress, "per-%s timeout: %s\n", unit, modelConfig.Timeout)
	workContext, cancel := context.WithCancel(ctx)
	defer cancel()
	type finished struct {
		task       auditTask
		invocation auditInvocation
		err        error
	}
	results := make(chan finished, options.Jobs)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	// State writes outlive cancellation so completed work and interruption records
	// reach SQLite before the coordinator exits.
	writeContext := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), 10*time.Second)
	}
	defer func() {
		if returnedErr != nil {
			cleanup, done := writeContext()
			defer done()
			_, _ = store.db.ExecContext(cleanup, "UPDATE audit_scans SET status='paused' WHERE id=? AND status='running'", scan.ID)
		}
	}()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	started := time.Now()
	active, dispatched := 0, 0
	stopped := false
	contextDone := ctx.Done()
	throttle := auditThrottle{delay: options.throttleDelay}
	for _, limit := range pendingLimits {
		if limit.Kind == auditRateLimited {
			throttle.active = true
			if limit.RetryAt != nil {
				throttle.until = maxTime(throttle.until, *limit.RetryAt)
			}
		}
	}
	if throttle.active {
		fmt.Fprintln(progress, "resuming with one provider probe before parallel dispatch")
		if time.Now().Before(throttle.until) {
			fmt.Fprintf(progress, "saved provider cooldown ends at %s\n", throttle.until.Format(time.RFC3339))
		}
	}
	var limitPause *auditProviderLimit
	processResult := func(result finished) error {
		active--
		status := "completed"
		var output *auditOutput
		if result.err == nil {
			parsed, parseErr := parseAuditTaskOutput([]byte(result.invocation.StructuredOutput), result.task.Input)
			if parseErr != nil {
				result.err = parseErr
			} else {
				output = &parsed
				status = parsed.Status
			}
		}
		var providerLimit *auditProviderLimit
		if result.err != nil {
			status = "failed"
			if errors.Is(result.err, context.Canceled) || workContext.Err() != nil {
				status = "interrupted"
			} else {
				providerLimit = classifyAuditProviderLimit(scan.Spec.Model.Harness, result.invocation, time.Now())
			}
		}
		if providerLimit != nil {
			status = providerLimit.Kind
			result.invocation.ProviderLimit = providerLimit
			result.err = fmt.Errorf("%w: %s", result.err, providerLimit.Message)
			if status == auditQuotaExhausted {
				stopped = true
				limitPause = providerLimit
				fmt.Fprintln(progress, "quota exhausted: stopping dispatch; active workers may finish, blocked assignments stay pending")
			} else {
				if !throttle.limited(providerLimit, time.Now()) {
					stopped = true
					if limitPause == nil {
						limitPause = providerLimit
					}
					fmt.Fprintln(progress, "provider is still throttling after three retries: pausing dispatch")
				} else {
					fmt.Fprintf(progress, "provider throttled: draining active workers; next single probe no earlier than %s\n", throttle.until.Format(time.RFC3339))
				}
			}
		} else if result.task.AttemptID == throttle.probe {
			throttle.active, throttle.probe, throttle.rounds = false, 0, 0
			throttle.until = time.Time{}
			fmt.Fprintln(progress, "provider probe finished without throttling; restoring parallel dispatch")
		}
		persist, done := writeContext()
		err := store.finishAuditTask(persist, scan, result.task, status, result.invocation, output, result.err, time.Now())
		done()
		if err != nil {
			return err
		}
		count := 0
		if output != nil {
			count = len(output.Findings)
		}
		if output != nil && len(auditRecheckResults(*output)) > 0 {
			for _, result := range auditRecheckResults(*output) {
				fmt.Fprintf(progress, "rechecked finding #%d: %s\n", result.FindingID, result.Outcome)
			}
		} else {
			fmt.Fprintf(progress, "%s %s %d %s: %d findings\n", status, unit, result.task.Ordinal, shortSHA(result.task.ID), count)
		}
		if result.err != nil {
			fmt.Fprintf(progress, "  %s\n", inventoryDisplay(result.err.Error()))
		}
		return nil
	}
	for {
		if ctx.Err() != nil {
			stopped = true
			cancel()
			contextDone = nil
		}
		if options.Duration > 0 && time.Since(started) >= options.Duration {
			stopped = true
		}
		if options.Limit > 0 && dispatched >= options.Limit {
			stopped = true
		}
		controlContext, done := writeContext()
		var control string
		err = store.db.QueryRowContext(controlContext, "SELECT control FROM audit_scans WHERE id=?", scan.ID).Scan(&control)
		done()
		if err != nil {
			return err
		}
		if control != "" {
			stopped = true
			if control == "interrupt" {
				cancel()
			}
		}
		// Observe finished provider failures before replacing workers. Otherwise a
		// backlog of fast failures could dispatch the entire queue before pausing.
		select {
		case result := <-results:
			if err := processResult(result); err != nil {
				return err
			}
			continue
		default:
		}
		canDispatch := !stopped && active < options.Jobs
		if throttle.active {
			canDispatch = canDispatch && active == 0 && !time.Now().Before(throttle.until)
		}
		if canDispatch {
			task, err := store.claimAuditTask(ctx, scan.ID, modelConfig.Timeout, time.Now())
			if err != nil {
				if ctx.Err() != nil {
					continue
				}
				return err
			}
			if task == nil {
				// In-flight assignments can return to pending after throttling.
				// Exhausting the queue only finishes a run once they have drained.
				stopped = active == 0
			} else {
				active++
				dispatched++
				if throttle.active {
					throttle.probe = task.AttemptID
				}
				workers.Add(1)
				fmt.Fprintf(progress, "started %s %d %s (attempt %d)\n", unit, task.Ordinal, shortSHA(task.ID), task.AttemptID)
				go func(task auditTask) {
					defer workers.Done()
					invocation, err := runner(workContext, modelConfig, task.Input.Prompt)
					results <- finished{task, invocation, err}
				}(*task)
				continue
			}
		}
		if active == 0 && stopped {
			break
		}
		select {
		case result := <-results:
			if err := processResult(result); err != nil {
				return err
			}
		case <-contextDone:
			stopped = true
			cancel()
			contextDone = nil
		case <-ticker.C:
		}
	}
	finishContext, done := writeContext()
	defer done()
	counts, err := store.auditCounts(finishContext, scan.ID)
	if err != nil {
		return err
	}
	status := "paused"
	if counts["pending"] == 0 && counts["running"] == 0 {
		status = "completed"
		if counts["failed"]+counts["unable_to_assess"] > 0 {
			status = "incomplete"
		}
	}
	if err = checkInventory(finishContext, repository, record.Inventory); err != nil {
		_, _ = store.db.ExecContext(finishContext, "UPDATE audit_scans SET status='invalid' WHERE id=?", scan.ID)
		return fmt.Errorf("scan checkout changed; results require review: %w", err)
	}
	if _, err = store.db.ExecContext(finishContext, "UPDATE audit_scans SET status=?,control='' WHERE id=?", status, scan.ID); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if limitPause != nil {
		return fmt.Errorf("scan paused: %s; blocked assignments remain pending; resume when provider capacity is available", limitPause.Kind)
	}
	if throttle.active {
		return errors.New("scan paused with throttled assignments pending; resume will respect their saved cooldown")
	}
	if counts["failed"] > 0 {
		return fmt.Errorf("scan has %d failed assignments; inspect scan attempts and resume with --retry-failed", counts["failed"])
	}
	return nil
}
