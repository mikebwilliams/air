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
	SHA       string
	ParentSHA string
	Author    string
	Date      string
	Message   string
}

type CommitRecord struct {
	SHA                      string
	ParentSHA                string
	ProcessedAt              time.Time
	Status                   string
	SkipReason               string
	Model                    string
	ReasoningEffort          string
	PromptVersion            string
	Summary                  string
	RawResponse              string
	Usage                    *TokenUsage
	EstimatedCostMicrousd    *int64
	EstimatedCostMaxMicrousd *int64
	CostContext              string
	CostComplete             bool
	NewCount                 int
	ResolvedCount            int
}

type ReviewAttempt struct {
	ID                       int64
	Number                   int
	CommitSHA                string
	ReviewedAt               time.Time
	Model                    string
	ReasoningEffort          string
	PromptVersion            string
	Summary                  string
	RawResponse              string
	Usage                    TokenUsage
	EstimatedCostMicrousd    *int64
	EstimatedCostMaxMicrousd *int64
	CostContext              string
	CostComplete             bool
	NewCount                 int
	ResolvedCount            int
	Current                  bool
}

type ReviewIdentity struct {
	Model           Model
	ReasoningEffort string
}

type Finding struct {
	ID            int64   `json:"id"`
	IntroducedSHA string  `json:"introduced_sha"`
	ResolvedSHA   *string `json:"resolved_sha,omitempty"`
	Severity      string  `json:"severity"`
	Title         string  `json:"title"`
	Description   string  `json:"description"`
	File          *string `json:"file,omitempty"`
	Line          *int    `json:"line,omitempty"`
	Symbol        *string `json:"symbol,omitempty"`
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
	InputTokens           int64
	CachedInputTokens     int64
	CacheWriteTokens      *int64
	OutputTokens          int64
	ReasoningOutputTokens int64
}

type Reviewer interface {
	Review(ctx context.Context, input ReviewInput) (ReviewResult, error)
}
