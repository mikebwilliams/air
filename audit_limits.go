package main

import (
	"bufio"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	auditQuotaExhausted = "quota_exhausted"
	auditRateLimited    = "rate_limited"
)

// Stored with the attempt, so a stopped coordinator retains both the reason
// for a pending assignment and the earliest time to retry a throttled request.
type auditProviderLimit struct {
	Kind    string     `json:"kind"`
	Message string     `json:"message"`
	RetryAt *time.Time `json:"retry_at,omitempty"`
}

var auditRetryDelayPattern = regexp.MustCompile(`(?i)(?:retry[ _-]after[:= ]*|try again in\s+)([0-9]+(?:\.[0-9]+)?)\s*(milliseconds?|ms|seconds?|s|minutes?|m|hours?|h)?\b`)

// Only provider error envelopes are inspected. Source code, tool output, and
// model messages can legitimately discuss quotas and must not stop the scan.
func classifyAuditProviderLimit(harness string, invocation auditInvocation, now time.Time) *auditProviderLimit {
	if harness != "codex" {
		return nil
	}
	var last map[string]any
	completed := false
	scanner := bufio.NewScanner(strings.NewReader(invocation.RawResponse))
	scanner.Buffer(make([]byte, 64*1024), maxCodexTranscriptBytes)
	for scanner.Scan() {
		var event map[string]any
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		switch event["type"] {
		case "error", "turn.failed":
			last, completed = event, false
		case "turn.completed":
			last, completed = nil, true
		}
	}
	if completed {
		return nil
	}
	if last == nil {
		// Startup/provider failures can precede the JSON event stream.
		last = map[string]any{"message": invocation.Stderr}
	}
	limit := auditProviderLimit{}
	var messages []string
	var inspect func(string, any)
	inspect = func(key string, value any) {
		key = strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key))
		switch v := value.(type) {
		case map[string]any:
			for k, child := range v {
				inspect(k, child)
			}
		case string:
			normalized := strings.ToLower(strings.NewReplacer("_", "", "-", "", " ", "").Replace(v))
			if key == "code" || key == "type" || key == "codexerrorinfo" {
				switch normalized {
				case "usagelimitexceeded", "usagelimitreached", "insufficientquota", "quotaexhausted", "billinghardlimitreached", "creditbalancetoolow":
					limit.Kind = auditQuotaExhausted
				case "ratelimitexceeded", "ratelimited", "toomanyrequests", "429":
					if limit.Kind == "" {
						limit.Kind = auditRateLimited
					}
				}
			}
			if key == "message" || key == "additionaldetails" || key == "error" {
				messages = append(messages, v)
				lower := strings.ToLower(strings.ReplaceAll(v, "’", "'"))
				for _, phrase := range []string{"you've hit your usage limit", "you have hit your usage limit", "usage limit exceeded", "usage limit reached", "usage limit has been reached", "usage_limit_reached", "usage_limit_exceeded", "insufficient_quota", "quota exhausted", "quota exceeded", "exceeded your current quota", "credit balance is too low", "out of credits"} {
					if strings.Contains(lower, phrase) {
						limit.Kind = auditQuotaExhausted
					}
				}
				if limit.Kind == "" && (strings.Contains(lower, "rate limit") || strings.Contains(lower, "rate_limit") || strings.Contains(lower, "too many requests") || strings.Contains(lower, "status 429") || strings.Contains(lower, "http 429")) {
					limit.Kind = auditRateLimited
				}
				if match := auditRetryDelayPattern.FindStringSubmatch(v); match != nil {
					seconds, _ := strconv.ParseFloat(match[1], 64)
					switch strings.ToLower(match[2]) {
					case "ms", "millisecond", "milliseconds":
						seconds /= 1000
					case "m", "minute", "minutes":
						seconds *= 60
					case "h", "hour", "hours":
						seconds *= 3600
					}
					limit.setRetryDelay(now, seconds)
				}
			}
			if key == "retryafter" || key == "retryafterseconds" {
				seconds, err := strconv.ParseFloat(v, 64)
				if err == nil {
					limit.setRetryDelay(now, seconds)
				} else if at, err := time.Parse(time.RFC1123, v); err == nil {
					limit.setRetryTime(at)
				}
			}
		case float64:
			if (key == "httpstatuscode" || key == "status" || key == "statuscode" || key == "code") && v == 429 && limit.Kind == "" {
				limit.Kind = auditRateLimited
			}
			if key == "retryafter" || key == "retryafterseconds" || key == "retryaftersecs" {
				limit.setRetryDelay(now, v)
			}
			if key == "retryafterms" {
				limit.setRetryDelay(now, v/1000)
			}
		}
	}
	inspect("", last)
	if limit.Kind == "" {
		return nil
	}
	sort.Strings(messages)
	limit.Message = strings.TrimSpace(strings.Join(messages, "; "))
	if limit.Message == "" {
		limit.Message = limit.Kind
	}
	return &limit
}

func (l *auditProviderLimit) setRetryDelay(now time.Time, seconds float64) {
	// Bound conversion to duration without wrapping on malformed input.
	if seconds > 0 && seconds <= 365*24*3600 {
		l.setRetryTime(now.Add(time.Duration(seconds * float64(time.Second))))
	}
}

func (l *auditProviderLimit) setRetryTime(at time.Time) {
	if l.RetryAt == nil || at.After(*l.RetryAt) {
		l.RetryAt = &at
	}
}

type auditThrottle struct {
	active bool
	rounds int
	probe  int64
	until  time.Time
	delay  time.Duration
}

func (t *auditThrottle) limited(limit *auditProviderLimit, now time.Time) bool {
	// All failures from a concurrent wave share one cooldown. After draining,
	// only one probe may run; its failure starts the next backoff round.
	if !t.active || t.probe != 0 {
		t.rounds++
	}
	t.active, t.probe = true, 0
	delay := t.delay
	if delay <= 0 {
		delay = 30 * time.Second
	}
	delay *= time.Duration(1 << min(max(t.rounds-1, 0), 3))
	t.until = maxTime(t.until, now.Add(delay))
	if limit.RetryAt != nil {
		t.until = maxTime(t.until, *limit.RetryAt)
	}
	limit.setRetryTime(t.until)
	return t.rounds <= 3
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
