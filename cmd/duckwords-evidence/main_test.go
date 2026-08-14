package main

import (
	"context"
	"strings"
	"testing"
)

func TestRunRejectsInvalidArgumentsWithoutEchoingThem(t *testing.T) {
	t.Parallel()
	var stderr strings.Builder
	secret := "client_secret=must-not-echo"
	if code := run(context.Background(), []string{"--unknown", secret}, &stderr); code != 2 {
		t.Fatalf("run() = %d, want 2", code)
	}
	if strings.Contains(stderr.String(), secret) || !strings.Contains(stderr.String(), "invalid arguments") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunReportsSanitizedFinalizationFailure(t *testing.T) {
	t.Parallel()
	var stderr strings.Builder
	args := []string{"--result", "missing-secret-result", "--log", "missing-log", "--output-dir", "out",
		"--exit-code", "0", "--binary", "missing-bin"}
	if code := run(context.Background(), args, &stderr); code != 1 {
		t.Fatalf("run() = %d, want 1", code)
	}
	if strings.Contains(stderr.String(), "missing-secret-result") || stderr.String() != "duckwords-evidence: evidence validation or publication failed\n" {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestValidArgumentsRequiresEachExactFlagOnce(t *testing.T) {
	t.Parallel()
	valid := []string{
		"--binary", "bin/duckwords",
		"--exit-code", "0",
		"--log", "application.log",
		"--output-dir", "artifacts/submission",
		"--result", "result.json",
	}
	if !validArguments(valid) {
		t.Fatalf("validArguments(%q) = false", valid)
	}

	tests := [][]string{
		nil,
		valid[:len(valid)-1],
		append(append([]string{}, valid...), "positional"),
		{"--result", "result.json", "--result", "other.json", "--log", "application.log", "--output-dir", "out", "--exit-code", "0", "--binary", "bin"},
		{"-result", "result.json", "--log", "application.log", "--output-dir", "out", "--exit-code", "0", "--binary", "bin"},
		{"--result", "", "--log", "application.log", "--output-dir", "out", "--exit-code", "0", "--binary", "bin"},
	}
	for _, args := range tests {
		if validArguments(args) {
			t.Errorf("validArguments(%q) = true", args)
		}
	}
}

func TestRunRejectsDuplicateFlagWithoutLeakingValue(t *testing.T) {
	t.Parallel()
	secret := "client_secret=must-not-echo"
	args := []string{
		"--result", secret,
		"--result", "other.json",
		"--log", "application.log",
		"--output-dir", "out",
		"--exit-code", "0",
		"--binary", "bin",
	}
	var stderr strings.Builder
	if code := run(context.Background(), args, &stderr); code != 2 {
		t.Fatalf("run() = %d, want 2", code)
	}
	if strings.Contains(stderr.String(), secret) || !strings.Contains(stderr.String(), "invalid arguments") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
