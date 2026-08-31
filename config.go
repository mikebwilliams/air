package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

type settingSpec struct {
	Key         string
	Environment []string
	Default     string
	AllowEmpty  bool
	Validate    func(string) error
}

type resolvedSetting struct {
	Value  string
	Source string
}

var codexSettings = []settingSpec{
	{Key: "model", Environment: []string{"AIR_MODEL"}, Validate: requireSettingValue},
	{Key: "effort", Environment: []string{"AIR_REASONING_EFFORT"}, Validate: requireSettingValue},
	{Key: "codex-bin", Environment: []string{"AIR_CODEX_BIN"}, Default: "codex", Validate: requireSettingValue},
	{Key: "codex-profile", Environment: []string{"AIR_CODEX_PROFILE"}, AllowEmpty: true},
	{
		Key:         "codex-timeout",
		Environment: []string{"AIR_CODEX_TIMEOUT"},
		Default:     defaultCodexTimeout.String(),
		Validate: func(value string) error {
			duration, err := time.ParseDuration(value)
			if err != nil || duration <= 0 {
				return errors.New("must be a positive duration")
			}
			return nil
		},
	},
}

func requireSettingValue(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("must not be empty")
	}
	return nil
}

func settingByKey(key string) (settingSpec, bool) {
	for _, setting := range codexSettings {
		if setting.Key == key {
			return setting, true
		}
	}
	return settingSpec{}, false
}

func normalizeSettingValue(value string) string {
	return strings.TrimSpace(value)
}

func validateSettingValue(setting settingSpec, value string) (string, error) {
	value = normalizeSettingValue(value)
	if value == "" {
		if setting.AllowEmpty {
			return value, nil
		}
		return "", errors.New("must not be empty")
	}
	if setting.Validate != nil {
		if err := setting.Validate(value); err != nil {
			return "", err
		}
	}
	return value, nil
}

func resolveSettingValue(
	ctx context.Context,
	store *Store,
	getenv func(string) string,
	setting settingSpec,
	commandLineValue string,
	commandLineSet bool,
) (resolvedSetting, error) {
	if commandLineSet {
		return checkedSetting(setting, commandLineValue, "command line")
	}
	if getenv != nil {
		for _, name := range setting.Environment {
			if value := getenv(name); value != "" {
				return checkedSetting(setting, value, name)
			}
		}
	}
	value, found, err := store.ConfigValue(ctx, setting.Key)
	if err != nil {
		return resolvedSetting{}, err
	}
	if found {
		return checkedSetting(setting, value, "database")
	}
	if setting.Default != "" {
		return checkedSetting(setting, setting.Default, "built-in")
	}
	return resolvedSetting{Source: "unset"}, nil
}

func checkedSetting(setting settingSpec, value, source string) (resolvedSetting, error) {
	normalized, err := validateSettingValue(setting, value)
	if err != nil {
		return resolvedSetting{}, fmt.Errorf("invalid %s %s: %w", source, setting.Key, err)
	}
	return resolvedSetting{Value: normalized, Source: source}, nil
}

func displaySettingValue(value string) string {
	if value == "" {
		return "<unset>"
	}
	return value
}
