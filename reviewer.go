package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

func parseReviewOutput(content string) (ReviewOutput, error) {
	content = strings.TrimSpace(content)
	if strings.HasPrefix(content, "```") {
		firstNewline := strings.IndexByte(content, '\n')
		lastFence := strings.LastIndex(content, "```")
		if firstNewline >= 0 && lastFence > firstNewline {
			content = strings.TrimSpace(content[firstNewline+1 : lastFence])
		}
	}
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	var output ReviewOutput
	if err := decoder.Decode(&output); err != nil {
		return ReviewOutput{}, fmt.Errorf("response is not valid review JSON: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return ReviewOutput{}, err
	}
	if output.NewFindings == nil {
		output.NewFindings = []NewFinding{}
	}
	if output.ResolvedFindings == nil {
		output.ResolvedFindings = []ResolvedFinding{}
	}
	return output, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("response contains more than one JSON value")
		}
		return fmt.Errorf("invalid trailing response data: %w", err)
	}
	return nil
}
