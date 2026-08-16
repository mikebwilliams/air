package main

import (
	"context"
	"time"
)

const (
	promptVersion = "2"
	masterRef     = "refs/heads/master"
	maxDiffBytes  = 256 * 1024
	maxToolBytes  = 64 * 1024
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
	NewCount                 int         `json:"new_count"`
	ResolvedCount            int         `json:"resolved_count"`
}

type ReviewAttempt struct {
	ID                       int64      `json:"id"`
	Number                   int        `json:"number"`
	CommitSHA                string     `json:"commit_sha"`
	ReviewedAt               time.Time  `json:"reviewed_at"`
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
	NewCount                 int        `json:"new_count"`
	ResolvedCount            int        `json:"resolved_count"`
	Current                  bool       `json:"current"`
}

type ReviewStats struct {
	Attempts              int
	Commits               int
	InputTokens           int64
	CachedInputTokens     int64
	CacheWriteTokens      int64
	CacheWritesUnreported int
	OutputTokens          int64
	ReasoningOutputTokens int64
	MinimumCostMicrousd   int64
	MaximumCostMicrousd   int64
	PricedAttempts        int
	UnknownCostAttempts   int
	Groups                []ReviewStatsGroup
}

type ReviewStatsGroup struct {
	Model                 string
	ReasoningEffort       string
	Attempts              int
	InputTokens           int64
	CachedInputTokens     int64
	CacheWriteTokens      int64
	CacheWritesUnreported int
	OutputTokens          int64
	ReasoningOutputTokens int64
	MinimumCostMicrousd   int64
	MaximumCostMicrousd   int64
	PricedAttempts        int
	UnknownCostAttempts   int
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
	Model           string    `json:"model,omitempty"`
	ReasoningEffort string    `json:"reasoning_effort,omitempty"`
	Force           bool      `json:"force"`
}

type ReviewIdentity struct {
	Model           Model
	ReasoningEffort string
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
	Diff         string
	OpenFindings []Finding
}

type ReviewResult struct {
	Output      ReviewOutput
	RawResponse string
	Usage       *TokenUsage
}

type TokenUsage struct {
	InputTokens           int64  `json:"input_tokens"`
	CachedInputTokens     int64  `json:"cached_input_tokens"`
	CacheWriteTokens      *int64 `json:"cache_write_tokens"`
	OutputTokens          int64  `json:"output_tokens"`
	ReasoningOutputTokens int64  `json:"reasoning_output_tokens"`
}

type Reviewer interface {
	Review(ctx context.Context, input ReviewInput) (ReviewResult, error)
}
