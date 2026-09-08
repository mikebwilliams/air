package main

import (
	"context"
	"strings"
	"time"
)

const (
	codexReviewerName  = "codex"
	claudeReviewerName = "claude"
	geminiReviewerName = "gemini"
)

func normalizedHarness(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return codexReviewerName
	}
	return name
}

const (
	promptVersion           = "4"
	recheckPromptVersion    = "1"
	masterRef               = "refs/heads/master"
	maxDiffBytes            = 256 * 1024
	maxToolBytes            = 64 * 1024
	maxResolutionCandidates = 50
	defaultRecheckBatchSize = 20
	maxRecheckBatchSize     = 50
)

type CommitMetadata struct {
	SHA       string `json:"sha"`
	ParentSHA string `json:"parent_sha,omitempty"`
	Author    string `json:"author,omitempty"`
	Date      string `json:"date,omitempty"`
	Message   string `json:"message,omitempty"`
}

type CommitRecord struct {
	SHA                      string      `json:"sha"`
	ParentSHA                string      `json:"parent_sha,omitempty"`
	ProcessedAt              time.Time   `json:"processed_at"`
	Status                   string      `json:"status"`
	SkipReason               string      `json:"skip_reason,omitempty"`
	Harness                  string      `json:"harness,omitempty"`
	Model                    string      `json:"model,omitempty"`
	ReasoningEffort          string      `json:"reasoning_effort,omitempty"`
	PromptVersion            string      `json:"prompt_version,omitempty"`
	Summary                  string      `json:"summary,omitempty"`
	RawResponse              string      `json:"raw_response,omitempty"`
	Usage                    *TokenUsage `json:"usage,omitempty"`
	EstimatedCostMicrousd    *int64      `json:"estimated_cost_microusd,omitempty"`
	EstimatedCostMaxMicrousd *int64      `json:"estimated_cost_max_microusd,omitempty"`
	CostContext              string      `json:"cost_context,omitempty"`
	CostComplete             bool        `json:"cost_complete"`
	ReportedCostMicrousd     *int64      `json:"reported_cost_microusd,omitempty"`
	DurationMilliseconds     *int64      `json:"duration_ms,omitempty"`
	NewCount                 int         `json:"new_count"`
	ResolvedCount            int         `json:"resolved_count"`
}

type ReviewAttempt struct {
	ID                       int64      `json:"id"`
	Number                   int        `json:"number"`
	CommitSHA                string     `json:"commit_sha"`
	ReviewedAt               time.Time  `json:"reviewed_at"`
	Harness                  string     `json:"harness"`
	Model                    string     `json:"model"`
	ReasoningEffort          string     `json:"reasoning_effort,omitempty"`
	PromptVersion            string     `json:"prompt_version"`
	Summary                  string     `json:"summary"`
	RawResponse              string     `json:"raw_response"`
	Usage                    TokenUsage `json:"usage"`
	EstimatedCostMicrousd    *int64     `json:"estimated_cost_microusd,omitempty"`
	EstimatedCostMaxMicrousd *int64     `json:"estimated_cost_max_microusd,omitempty"`
	CostContext              string     `json:"cost_context,omitempty"`
	CostComplete             bool       `json:"cost_complete"`
	ReportedCostMicrousd     *int64     `json:"reported_cost_microusd,omitempty"`
	DurationMilliseconds     *int64     `json:"duration_ms,omitempty"`
	NewCount                 int        `json:"new_count"`
	ResolvedCount            int        `json:"resolved_count"`
	Current                  bool       `json:"current"`
}

type ReviewStats struct {
	Attempts                   int
	RecheckAttempts            int
	Commits                    int
	InputTokens                int64
	CachedInputTokens          int64
	CacheWriteTokens           int64
	CacheWritesUnreported      int
	OutputTokens               int64
	ReasoningOutputTokens      int64
	ReasoningOutputsUnreported int
	MinimumCostMicrousd        int64
	MaximumCostMicrousd        int64
	PricedAttempts             int
	UnknownCostAttempts        int
	DurationMilliseconds       int64
	TimedAttempts              int
	UntimedAttempts            int
	Groups                     []ReviewStatsGroup
}

type ReviewStatsGroup struct {
	Harness                    string
	Model                      string
	ReasoningEffort            string
	Attempts                   int
	InputTokens                int64
	CachedInputTokens          int64
	CacheWriteTokens           int64
	CacheWritesUnreported      int
	OutputTokens               int64
	ReasoningOutputTokens      int64
	ReasoningOutputsUnreported int
	MinimumCostMicrousd        int64
	MaximumCostMicrousd        int64
	PricedAttempts             int
	UnknownCostAttempts        int
}

type ReviewDurationStats struct {
	TotalMilliseconds int64
	TimedAttempts     int
}

type FindingStats struct {
	Open      int
	Dismissed int
	Resolved  int
	Skipped   int
}

type ScanFailure struct {
	SHA             string    `json:"sha"`
	ParentSHA       string    `json:"parent_sha,omitempty"`
	FailedAt        time.Time `json:"failed_at"`
	AttemptCount    int       `json:"attempt_count"`
	Error           string    `json:"error"`
	Harness         string    `json:"harness,omitempty"`
	Model           string    `json:"model,omitempty"`
	ReasoningEffort string    `json:"reasoning_effort,omitempty"`
	Force           bool      `json:"force"`
}

type ReviewIdentity struct {
	Harness         string
	Model           Model
	ReasoningEffort string
	PromptVersion   string
}

type Finding struct {
	ID            int64      `json:"id"`
	IntroducedSHA string     `json:"introduced_sha"`
	ResolvedSHA   *string    `json:"resolved_sha,omitempty"`
	DismissedAt   *time.Time `json:"dismissed_at,omitempty"`
	DismissReason string     `json:"dismiss_reason,omitempty"`
	Severity      string     `json:"severity"`
	Title         string     `json:"title"`
	Description   string     `json:"description"`
	File          *string    `json:"file,omitempty"`
	Line          *int       `json:"line,omitempty"`
	Symbol        *string    `json:"symbol,omitempty"`
}

type FindingEvent struct {
	ID        int64
	FindingID int64
	ReviewID  *int64
	SHA       *string
	Action    string
	Note      string
	CreatedAt time.Time
}

type FindingReview struct {
	ID              int64
	Number          int
	CommitSHA       string
	ReviewedAt      time.Time
	Harness         string
	Model           string
	ReasoningEffort string
}

type NewFinding struct {
	Severity    string  `json:"severity"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	File        *string `json:"file"`
	Line        *int    `json:"line"`
	Symbol      *string `json:"symbol"`
}

type ResolvedFinding struct {
	ID     int64  `json:"id"`
	Reason string `json:"reason"`
}

type ReviewOutput struct {
	NewFindings      []NewFinding      `json:"new_findings"`
	ResolvedFindings []ResolvedFinding `json:"resolved_findings"`
	Summary          string            `json:"summary"`
}

type ReviewInput struct {
	Commit       CommitMetadata
	OpenFindings []Finding
	Staged       bool
}

type ReviewResult struct {
	Output               ReviewOutput
	RawResponse          string
	Usage                *TokenUsage
	ReportedCostMicrousd *int64
	Duration             time.Duration
}

type RecheckFindingResult struct {
	ID      int64  `json:"id"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
}

type RecheckOutput struct {
	Findings []RecheckFindingResult `json:"findings"`
	Summary  string                 `json:"summary"`
}

type RecheckInput struct {
	HeadSHA  string
	Findings []Finding
}

type RecheckResult struct {
	Output               RecheckOutput
	RawResponse          string
	Usage                *TokenUsage
	ReportedCostMicrousd *int64
	Duration             time.Duration
}

type TokenUsage struct {
	InputTokens                     int64  `json:"input_tokens"`
	CachedInputTokens               int64  `json:"cached_input_tokens"`
	CacheWriteTokens                *int64 `json:"cache_write_tokens"`
	OutputTokens                    int64  `json:"output_tokens"`
	ReasoningOutputTokens           int64  `json:"reasoning_output_tokens"`
	ReasoningOutputTokensUnreported bool   `json:"reasoning_output_tokens_unreported,omitempty"`
}

type Reviewer interface {
	Review(ctx context.Context, input ReviewInput) (ReviewResult, error)
}

type RecheckReviewer interface {
	Recheck(ctx context.Context, input RecheckInput) (RecheckResult, error)
}
