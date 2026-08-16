package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

type settingSpec struct {
	Key         string
	Environment []string
	Default     string
	AllowEmpty  bool
	Sensitive   bool
	Validate    func(string) error
}

type resolvedSetting struct {
	Value  string
	Source string
}

var reviewerSettings = []settingSpec{
	{
		Key:         "reviewer",
		Environment: []string{"AIR_REVIEWER"},
		Default:     "codex",
		Validate: func(value string) error {
			if value != "codex" && value != "http" {
				return errors.New("must be codex or http")
			}
			return nil
		},
	},
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
	{
		Key:         "base-url",
		Environment: []string{"AIR_BASE_URL"},
		Default:     "https://api.openai.com/v1",
		Validate: func(value string) error {
			parsed, err := url.Parse(value)
			if err != nil || parsed.Scheme == "" || parsed.Host == "" {
				return errors.New("must be an absolute URL")
			}
			return nil
		},
	},
	{
		Key:         "api-key-env",
		Environment: []string{"AIR_API_KEY_ENV"},
		Validate: func(value string) error {
			if value == "" || strings.ContainsRune(value, '=') {
				return errors.New("must be an environment variable name")
			}
			return nil
		},
	},
	{
		Key:         "api-key",
		Environment: []string{"AIR_API_KEY", "OPENAI_API_KEY"},
		Sensitive:   true,
		Validate:    requireSettingValue,
	},
}

func requireSettingValue(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("must not be empty")
	}
	return nil
}

func settingByKey(key string) (settingSpec, bool) {
	for _, setting := range reviewerSettings {
		if setting.Key == key {
			return setting, true
		}
	}
	return settingSpec{}, false
}

func normalizeSettingValue(setting settingSpec, value string) string {
	if setting.Sensitive {
		return value
	}
	value = strings.TrimSpace(value)
	if setting.Key == "reviewer" {
		return strings.ToLower(value)
	}
	return value
}

func validateSettingValue(setting settingSpec, value string) (string, error) {
	value = normalizeSettingValue(setting, value)
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

func displaySettingValue(setting settingSpec, value string) string {
	if setting.Sensitive && value != "" {
		return "<redacted>"
	}
	if value == "" {
		return "<unset>"
	}
	return value
}

func configuredAPIKey(
	ctx context.Context,
	store *Store,
	getenv func(string) string,
	apiKey string,
	apiKeySet bool,
	apiKeyEnv string,
	apiKeyEnvSet bool,
) (string, string, error) {
	if apiKeySet {
		setting, _ := settingByKey("api-key")
		resolved, err := checkedSetting(setting, apiKey, "command line")
		return resolved.Value, resolved.Source, err
	}
	if apiKeyEnvSet && apiKeyEnv != "" {
		return keyFromEnvironment(getenv, apiKeyEnv, "command line")
	}
	if getenv != nil {
		if name := getenv("AIR_API_KEY_ENV"); name != "" {
			return keyFromEnvironment(getenv, name, "AIR_API_KEY_ENV")
		}
		if value := getenv("AIR_API_KEY"); value != "" {
			return value, "AIR_API_KEY", nil
		}
		if value := getenv("OPENAI_API_KEY"); value != "" {
			return value, "OPENAI_API_KEY", nil
		}
	}
	databaseEnvironment, found, err := store.ConfigValue(ctx, "api-key-env")
	if err != nil {
		return "", "", err
	}
	if found && databaseEnvironment != "" {
		return keyFromEnvironment(getenv, databaseEnvironment, "database api-key-env")
	}
	databaseKey, found, err := store.ConfigValue(ctx, "api-key")
	if err != nil {
		return "", "", err
	}
	if found {
		setting, _ := settingByKey("api-key")
		resolved, err := checkedSetting(setting, databaseKey, "database")
		return resolved.Value, resolved.Source, err
	}
	return "", "unset", nil
}

func keyFromEnvironment(getenv func(string) string, name, source string) (string, string, error) {
	setting, _ := settingByKey("api-key-env")
	validatedName, err := validateSettingValue(setting, name)
	if err != nil || validatedName == "" {
		if err == nil {
			err = errors.New("must not be empty")
		}
		return "", name, fmt.Errorf("invalid %s api-key-env: %w", source, err)
	}
	name = validatedName
	if getenv == nil || getenv(name) == "" {
		return "", name, fmt.Errorf("API key environment variable %s selected by %s is empty", name, source)
	}
	return getenv(name), name, nil
}
