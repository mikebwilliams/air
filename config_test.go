package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestSettingPrecedence(t *testing.T) {
	ctx := context.Background()
	store, err := CreateStore(ctx, filepath.Join(t.TempDir(), "air.sqlite"), strings.Repeat("0", 40))
	if err != nil {
		t.Fatalf("CreateStore: %v", err)
	}
	defer store.Close()
	if err := store.SetConfig(ctx, "model", "database-model"); err != nil {
		t.Fatal(err)
	}

	model, _ := settingByKey("model")
	resolved, err := resolveSettingValue(ctx, store, func(string) string { return "" }, model, "", false)
	if err != nil || resolved.Value != "database-model" || resolved.Source != "database" {
		t.Fatalf("database resolution = %+v, %v", resolved, err)
	}
	resolved, err = resolveSettingValue(ctx, store, func(name string) string {
		if name == "AIR_MODEL" {
			return "environment-model"
		}
		return ""
	}, model, "", false)
	if err != nil || resolved.Value != "environment-model" || resolved.Source != "AIR_MODEL" {
		t.Fatalf("environment resolution = %+v, %v", resolved, err)
	}
	resolved, err = resolveSettingValue(ctx, store, func(name string) string {
		return "environment-model"
	}, model, "command-line-model", true)
	if err != nil || resolved.Value != "command-line-model" || resolved.Source != "command line" {
		t.Fatalf("command-line resolution = %+v, %v", resolved, err)
	}

	reviewer, _ := settingByKey("reviewer")
	resolved, err = resolveSettingValue(ctx, store, func(string) string { return "" }, reviewer, "", false)
	if err != nil || resolved.Value != "codex" || resolved.Source != "built-in" {
		t.Fatalf("built-in resolution = %+v, %v", resolved, err)
	}
	timeout, _ := settingByKey("codex-timeout")
	resolved, err = resolveSettingValue(ctx, store, func(string) string { return "" }, timeout, "", false)
	if err != nil || resolved.Value != "20m0s" || resolved.Source != "built-in" {
		t.Fatalf("built-in timeout = %+v, %v", resolved, err)
	}
}

func TestSettingValidationIdentifiesSource(t *testing.T) {
	ctx := context.Background()
	store, err := CreateStore(ctx, filepath.Join(t.TempDir(), "air.sqlite"), strings.Repeat("0", 40))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	timeout, _ := settingByKey("codex-timeout")
	_, err = resolveSettingValue(ctx, store, func(name string) string {
		if name == "AIR_CODEX_TIMEOUT" {
			return "eventually"
		}
		return ""
	}, timeout, "", false)
	if err == nil || !strings.Contains(err.Error(), "AIR_CODEX_TIMEOUT") || !strings.Contains(err.Error(), "positive duration") {
		t.Fatalf("invalid timeout error = %v", err)
	}
}

func TestAPIKeyPrecedence(t *testing.T) {
	ctx := context.Background()
	store, err := CreateStore(ctx, filepath.Join(t.TempDir(), "air.sqlite"), strings.Repeat("0", 40))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.SetConfig(ctx, "api-key", "database-secret"); err != nil {
		t.Fatal(err)
	}

	key, source, err := configuredAPIKey(ctx, store, func(string) string { return "" }, "", false, "", false)
	if err != nil || key != "database-secret" || source != "database" {
		t.Fatalf("database API key = %q from %q, %v", key, source, err)
	}
	getenv := func(name string) string {
		values := map[string]string{
			"AIR_API_KEY": "environment-secret",
			"NAMED_KEY":   "named-secret",
		}
		return values[name]
	}
	key, source, err = configuredAPIKey(ctx, store, getenv, "", false, "", false)
	if err != nil || key != "environment-secret" || source != "AIR_API_KEY" {
		t.Fatalf("environment API key = %q from %q, %v", key, source, err)
	}
	key, source, err = configuredAPIKey(ctx, store, getenv, "", false, "NAMED_KEY", true)
	if err != nil || key != "named-secret" || source != "NAMED_KEY" {
		t.Fatalf("named API key = %q from %q, %v", key, source, err)
	}
	key, source, err = configuredAPIKey(ctx, store, getenv, "command-secret", true, "NAMED_KEY", true)
	if err != nil || key != "command-secret" || source != "command line" {
		t.Fatalf("command-line API key = %q from %q, %v", key, source, err)
	}
}
