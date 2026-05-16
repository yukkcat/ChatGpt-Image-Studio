package api

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"chatgpt2api/internal/config"
)

func TestResolveConfigSaveTargetUsesRequestedRedisValues(t *testing.T) {
	cfg := &config.Config{}
	cfg.Storage.ConfigBackend = "file"
	cfg.Storage.RedisAddr = "old-host:6379"
	cfg.Storage.RedisPassword = "old-password"
	cfg.Storage.RedisDB = 9
	cfg.Storage.RedisPrefix = "old-prefix"

	target := resolveConfigSaveTarget(cfg, configSaveTarget{
		ConfigBackend: "redis",
		RedisAddr:     "new-host:6379",
		RedisPassword: "",
		RedisDB:       0,
		RedisPrefix:   "new-prefix",
	})

	if target.ConfigBackend != "redis" {
		t.Fatalf("ConfigBackend = %q, want redis", target.ConfigBackend)
	}
	if target.RedisAddr != "new-host:6379" {
		t.Fatalf("RedisAddr = %q, want new-host:6379", target.RedisAddr)
	}
	if target.RedisPassword != "" {
		t.Fatalf("RedisPassword = %q, want empty string", target.RedisPassword)
	}
	if target.RedisDB != 0 {
		t.Fatalf("RedisDB = %d, want 0", target.RedisDB)
	}
	if target.RedisPrefix != "new-prefix" {
		t.Fatalf("RedisPrefix = %q, want new-prefix", target.RedisPrefix)
	}
}

func TestSaveConfigOverridesDoesNotRewriteBootstrapOnRedisFailure(t *testing.T) {
	rootDir := t.TempDir()
	cfg := config.New(rootDir)
	if err := cfg.Load(); err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	if err := cfg.SaveOverrides(map[string]map[string]any{
		"storage": {
			"config_backend": "file",
		},
		"app": {
			"auth_key": "file-auth",
		},
	}); err != nil {
		t.Fatalf("SaveOverrides() returned error: %v", err)
	}
	overridePath := cfg.Paths().Override
	before, err := os.ReadFile(overridePath)
	if err != nil {
		t.Fatalf("ReadFile(before) returned error: %v", err)
	}

	server := &Server{cfg: cfg}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err = server.saveConfigOverrides(ctx, map[string]map[string]any{
		"app": {
			"auth_key": "redis-auth",
		},
	}, configSaveTarget{
		ConfigBackend: "redis",
		RedisAddr:     "127.0.0.1:1",
		RedisPassword: "",
		RedisDB:       0,
		RedisPrefix:   "chatgpt2api:test:broken",
	})
	if err == nil {
		t.Fatal("expected Redis save to fail")
	}

	after, readErr := os.ReadFile(overridePath)
	if readErr != nil {
		t.Fatalf("ReadFile(after) returned error: %v", readErr)
	}
	if string(after) != string(before) {
		t.Fatalf("bootstrap override changed on failed Redis save:\nbefore=%s\nafter=%s", string(before), string(after))
	}
	if strings.Contains(string(after), `config_backend = "redis"`) {
		t.Fatalf("bootstrap override should not switch to redis on failed save: %s", string(after))
	}
}

func TestBuildConfigPayloadIncludesPublicImageBaseURL(t *testing.T) {
	cfg := &config.Config{}
	cfg.App.PublicImageBaseURL = "https://img.example.com"
	server := &Server{cfg: cfg}

	payload := server.buildConfigPayload()
	if payload.App.PublicImageBaseURL != "https://img.example.com" {
		t.Fatalf("PublicImageBaseURL = %q, want configured value", payload.App.PublicImageBaseURL)
	}
}
