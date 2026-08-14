package main

import (
	"errors"
	"strings"
	"testing"
)

func TestCredentialsFromEnvironmentRequiresExplicitApprovalBeforeCredentials(t *testing.T) {
	t.Parallel()

	lookedUp := make([]string, 0, 1)
	credentials, err := credentialsFromEnvironment(func(name string) (string, bool) {
		lookedUp = append(lookedUp, name)
		return "", false
	})
	if credentials != (redditCredentials{}) || !errors.Is(err, errApprovalRequired) {
		t.Fatalf("credentials = %+v, error = %v", credentials, err)
	}
	if len(lookedUp) != 1 || lookedUp[0] != envRedditAPIApproved {
		t.Fatalf("lookups = %v, want approval only", lookedUp)
	}
}

func TestCredentialsFromEnvironmentAcceptsCompleteExactConfiguration(t *testing.T) {
	t.Parallel()

	values := map[string]string{
		envRedditAPIApproved:  "true",
		envRedditClientID:     "client-id",
		envRedditClientSecret: "client-secret",
		envRedditUserAgent:    "darwin:duckwords:1.0 (by /u/example)",
	}
	credentials, err := credentialsFromEnvironment(mapLookup(values))
	if err != nil {
		t.Fatalf("credentialsFromEnvironment() error = %v", err)
	}
	if !credentials.approved || credentials.clientID != values[envRedditClientID] ||
		credentials.clientSecret != values[envRedditClientSecret] ||
		credentials.userAgent != values[envRedditUserAgent] {
		t.Fatalf("credentials = %+v, want complete environment values", credentials)
	}
}

func TestCredentialsFromEnvironmentRejectsMissingValuesWithoutLeakingSecrets(t *testing.T) {
	t.Parallel()

	const canary = "planted-secret"
	tests := []struct {
		name   string
		change func(map[string]string)
		want   string
	}{
		{name: "missing client id", change: func(values map[string]string) { delete(values, envRedditClientID) }, want: envRedditClientID},
		{name: "blank secret", change: func(values map[string]string) { values[envRedditClientSecret] = "" }, want: envRedditClientSecret},
		{name: "padded user agent", change: func(values map[string]string) { values[envRedditUserAgent] = " " + canary + " " }, want: envRedditUserAgent},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			values := map[string]string{
				envRedditAPIApproved:  "true",
				envRedditClientID:     canary,
				envRedditClientSecret: canary,
				envRedditUserAgent:    canary,
			}
			test.change(values)
			credentials, err := credentialsFromEnvironment(mapLookup(values))
			if credentials != (redditCredentials{}) || !errors.Is(err, errEnvironmentConfig) {
				t.Fatalf("credentials = %+v, error = %v", credentials, err)
			}
			if !strings.Contains(err.Error(), test.want) || strings.Contains(err.Error(), canary) {
				t.Fatalf("unsafe or unactionable error = %q", err)
			}
		})
	}
}

func TestCredentialsFromEnvironmentRejectsNilLookup(t *testing.T) {
	t.Parallel()

	_, err := credentialsFromEnvironment(nil)
	if !errors.Is(err, errEnvironmentConfig) {
		t.Fatalf("error = %v, want errEnvironmentConfig", err)
	}
}

func mapLookup(values map[string]string) environmentLookup {
	return func(name string) (string, bool) {
		value, found := values[name]
		return value, found
	}
}
