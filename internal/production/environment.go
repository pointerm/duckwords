package production

import (
	"errors"
	"fmt"
	"strings"
)

const (
	envRedditClientID     = "REDDIT_CLIENT_ID"
	envRedditClientSecret = "REDDIT_CLIENT_SECRET"
	envRedditUserAgent    = "REDDIT_USER_AGENT"
	envRedditAPIApproved  = "REDDIT_API_ACCESS_APPROVED"
)

var (
	errEnvironmentConfig = errors.New("invalid Reddit environment configuration")
	errApprovalRequired  = errors.New("explicit Reddit Data API approval confirmation is required")
)

type environmentLookup func(string) (string, bool)

type redditCredentials struct {
	clientID     string
	clientSecret string
	userAgent    string
	approved     bool
}

// credentialsFromEnvironment keeps credentials out of process arguments and fails
// before source or Reddit I/O. The approval value is checked first and its value is
// intentionally exact: Reddit's Responsible Builder Policy requires approval before
// Data API access, and holding a registered application's credentials is not that
// approval. The CLI never infers one from the other, so an unapproved environment
// cannot reach the network at all.
func credentialsFromEnvironment(lookup environmentLookup) (redditCredentials, error) {
	if lookup == nil {
		return redditCredentials{}, fmt.Errorf("%w: environment lookup is required", errEnvironmentConfig)
	}
	approved, present := lookup(envRedditAPIApproved)
	if !present || approved != "true" {
		return redditCredentials{}, errApprovalRequired
	}

	clientID, err := requiredEnvironmentValue(lookup, envRedditClientID)
	if err != nil {
		return redditCredentials{}, err
	}
	clientSecret, err := requiredEnvironmentValue(lookup, envRedditClientSecret)
	if err != nil {
		return redditCredentials{}, err
	}
	userAgent, err := requiredEnvironmentValue(lookup, envRedditUserAgent)
	if err != nil {
		return redditCredentials{}, err
	}
	return redditCredentials{
		clientID:     clientID,
		clientSecret: clientSecret,
		userAgent:    userAgent,
		approved:     true,
	}, nil
}

func requiredEnvironmentValue(lookup environmentLookup, name string) (string, error) {
	value, present := lookup(name)
	if !present || value == "" || strings.TrimSpace(value) != value {
		return "", &environmentValueError{name: name}
	}
	return value, nil
}

// environmentValueError exposes only a fixed variable name, never its value.
type environmentValueError struct {
	name string
}

func (e *environmentValueError) Error() string {
	if e == nil {
		return errEnvironmentConfig.Error()
	}
	return fmt.Sprintf("%s: %s is required", errEnvironmentConfig, e.name)
}

func (e *environmentValueError) Is(target error) bool {
	return target == errEnvironmentConfig
}
