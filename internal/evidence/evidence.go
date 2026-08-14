package evidence

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	manifestSchema         = 1
	maximumResultBytes     = 1 << 20
	maximumLogBytes        = 16 << 20
	maximumLogLineBytes    = 64 << 10
	maximumBinaryBytes     = 64 << 20
	maximumVersionBytes    = 1 << 10
	maximumApprovalBytes   = 128
	maximumLogRecords      = 100_000
	maximumPostOutcomes    = 10_000
	maximumResultWords     = 10
	assignmentPostCount    = 200
	binaryInspectionWindow = 5 * time.Second
	binaryInspectionDrain  = time.Second
	maximumRunDuration     = 30 * time.Minute
	durationClockTolerance = 5 * time.Second
	filePermission         = 0o644
	stagingPermission      = 0o700
	directoryPermission    = 0o755
	buildDateLayout        = "2006-01-02T15:04:05Z"
	policyDateLayout       = "2006-01-02"
	fullLogResultMarker    = "--- DUCKWORDS RESULT JSON ---\n"
	fixedCommand           = "duckwords live assignment run (credentials via environment)"
	assignmentInputProfile = "assignment-default-v1"
)

var (
	// ErrInvalidConfig identifies unsafe or incomplete finalizer configuration.
	ErrInvalidConfig = errors.New("invalid evidence configuration")
	// ErrInvalidResult identifies output that is not the exact canonical top-ten JSON.
	ErrInvalidResult = errors.New("invalid result evidence")
	// ErrInvalidLog identifies malformed, unsafe, or internally inconsistent NDJSON.
	ErrInvalidLog = errors.New("invalid application log evidence")
	// ErrInvalidBinary identifies a binary whose provenance cannot be reconciled.
	ErrInvalidBinary = errors.New("invalid release binary evidence")
	// ErrPublish identifies an unsafe target or an all-or-nothing publication failure.
	ErrPublish = errors.New("publish evidence bundle")

	semverPattern    = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z]+([.-][0-9A-Za-z]+)*)?$`)
	goVersionPattern = regexp.MustCompile(`^go1\.26\.6$`)
	platformPattern  = regexp.MustCompile(`^[a-z0-9]{1,16}$`)
	postIDPattern    = regexp.MustCompile(`^[a-z0-9]{1,16}$`)
	approvalPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
)

// Config selects one completed run and a new destination. Now is injectable only
// for deterministic validation tests; nil selects the current UTC time.
type Config struct {
	ResultPath        string
	LogPath           string
	OutputDir         string
	BinaryPath        string
	ExitCode          int
	PolicyVerifiedAt  string
	ApprovalReference string
	Now               func() time.Time
}

// Manifest is the sanitized, machine-readable reconciliation record emitted with a
// bundle. It intentionally contains no source paths, URLs, raw content, or argv.
type Manifest struct {
	Schema         int               `json:"schema"`
	Command        string            `json:"command"`
	ExitCode       int               `json:"exit_code"`
	TerminalStatus string            `json:"terminal_status"`
	Partial        bool              `json:"partial"`
	ResultWords    int               `json:"result_words"`
	StartedAt      string            `json:"started_at"`
	FinishedAt     string            `json:"finished_at"`
	Build          BuildManifest     `json:"build"`
	Config         ConfigManifest    `json:"config"`
	Inputs         InputsManifest    `json:"inputs"`
	Summary        SummaryManifest   `json:"summary"`
	Requests       RequestManifest   `json:"requests"`
	Outcomes       []OutcomeManifest `json:"outcomes"`
	Policy         PolicyManifest    `json:"policy"`
	Artifacts      ArtifactManifest  `json:"artifacts"`
}

// BuildManifest records immutable release identity plus the inspected binary hash.
type BuildManifest struct {
	Version      string `json:"version"`
	Commit       string `json:"commit"`
	BuildDate    string `json:"build_date"`
	GoVersion    string `json:"go_version"`
	GOOS         string `json:"goos"`
	GOARCH       string `json:"goarch"`
	BinarySHA256 string `json:"binary_sha256"`
}

// ConfigManifest records the non-secret processing controls used by the run.
type ConfigManifest struct {
	Workers                  int     `json:"workers"`
	FailureMode              string  `json:"failure_mode"`
	InputProfile             string  `json:"input_profile"`
	RateLimitRPS             float64 `json:"rate_limit_rps"`
	RequestTimeout           string  `json:"request_timeout"`
	GlobalTimeout            string  `json:"global_timeout"`
	MaxRetries               int     `json:"max_retries"`
	RetryBudget              string  `json:"retry_budget"`
	SourceMaxRetries         int     `json:"source_max_retries"`
	SourceRetryBudget        string  `json:"source_retry_budget"`
	MaxDistinctWordsPerPost  int     `json:"max_distinct_words_per_post"`
	MaxInFlightResponseBytes int64   `json:"max_in_flight_response_bytes"`
	MaxRetainedThings        int     `json:"max_retained_things"`
	FilterCount              int     `json:"filter_count"`
}

// InputsManifest records only safe input provenance and independently reconciled hashes.
type InputsManifest struct {
	Posts      SourceManifest `json:"posts"`
	Dictionary SourceManifest `json:"dictionary"`
}

// SourceManifest describes one acquired and parsed assignment source.
type SourceManifest struct {
	Mode    string `json:"mode"`
	Origin  string `json:"origin"`
	Bytes   int64  `json:"bytes"`
	Entries int    `json:"entries"`
	SHA256  string `json:"sha256"`
	// IDsSHA256 is present only for the post source and binds its normalized,
	// source-ordered IDs to the outcome sequence.
	IDsSHA256 string `json:"ids_sha256,omitempty"`
}

// SummaryManifest contains the terminal counters reconciled against post outcomes.
type SummaryManifest struct {
	Duration             string `json:"duration"`
	PostsTotal           int    `json:"posts_total"`
	PostsCompleted       int    `json:"posts_completed"`
	PostsSkipped         int    `json:"posts_skipped"`
	PostsFailed          int    `json:"posts_failed"`
	PostsIncomplete      int    `json:"posts_incomplete"`
	Comments             uint64 `json:"comments"`
	BodiesVisited        uint64 `json:"bodies_visited"`
	MoreRequests         uint64 `json:"more_requests"`
	ContinuationRequests uint64 `json:"continuation_requests"`
	CountedTokens        uint64 `json:"counted_tokens"`
	DistinctWords        int    `json:"distinct_words"`
	DictionaryWords      int    `json:"dictionary_words"`
}

// RequestManifest contains aggregate retry and throttling evidence without URLs.
type RequestManifest struct {
	SourceRetries      uint64 `json:"source_retries"`
	RedditHTTPAttempts uint64 `json:"reddit_http_attempts"`
	RedditRetries      uint64 `json:"reddit_retries"`
	ThrottleWaits      uint64 `json:"throttle_waits"`
	ThrottleWait       string `json:"throttle_wait"`
}

// OutcomeManifest records one unique post's sanitized terminal outcome.
type OutcomeManifest struct {
	PostID               string `json:"post_id"`
	SourceLine           int    `json:"source_line"`
	Status               string `json:"status"`
	ErrorClass           string `json:"error_class,omitempty"`
	Operation            string `json:"operation,omitempty"`
	HTTPStatus           int    `json:"http_status,omitempty"`
	Comments             int    `json:"comments"`
	BodiesVisited        int    `json:"bodies_visited"`
	MoreRequests         int    `json:"more_requests"`
	ContinuationRequests int    `json:"continuation_requests"`
	CountedTokens        uint64 `json:"counted_tokens"`
}

// PolicyManifest records only the candidate-supplied approval attestation reference.
type PolicyManifest struct {
	RedditPolicyVerifiedAt string `json:"reddit_policy_verified_at"`
	ApprovalReference      string `json:"approval_reference"`
}

// ArtifactManifest records SHA-256 hashes for the other four bundle files. The
// manifest cannot hash itself without a recursive definition.
type ArtifactManifest struct {
	ResultSHA256          string `json:"result_sha256"`
	ApplicationLogSHA256  string `json:"application_log_sha256"`
	FullApplicationSHA256 string `json:"full_application_log_sha256"`
	RunDocumentSHA256     string `json:"run_document_sha256"`
}

type resultWord struct {
	Word  string `json:"word"`
	Count uint64 `json:"count"`
}

type logRecord map[string]json.RawMessage

type parsedLog struct {
	started           startedRecord
	sources           map[string]SourceManifest
	summary           summaryRecord
	output            outputRecord
	outcomes          []OutcomeManifest
	redditRetries     []retryRecord
	intermediateTimes []eventTimestamp
	sourceRetryEvents uint64
	redditRetryEvents uint64
}

type eventTimestamp struct {
	event string
	time  time.Time
}

type retryRecord struct {
	source     bool
	sourceKind string
	operation  string
	postID     string
	attempt    int
}

type startedRecord struct {
	time   time.Time
	config ConfigManifest
	build  BuildManifest
}

type summaryRecord struct {
	time           time.Time
	duration       time.Duration
	terminalStatus string
	partial        bool
	summary        SummaryManifest
	requests       RequestManifest
	postsHash      string
	postIDsHash    string
	dictionaryHash string
	failureMode    string
	workers        int
	inputProfile   string
	filterCount    int
	build          BuildManifest
}

type outputRecord struct {
	time        time.Time
	partial     bool
	resultWords int
	resultHash  string
}

// Finalize validates, reconciles, and atomically publishes a new evidence directory.
// The destination must not exist: refusing known destinations prevents a failed or
// partial rerun from silently destroying previously reviewed evidence. See publish
// for the precise cross-platform no-replace limitation of the standard library.
func Finalize(ctx context.Context, cfg Config) (Manifest, error) {
	if ctx == nil {
		return Manifest{}, fmt.Errorf("%w: context is required", ErrInvalidConfig)
	}
	if err := ctx.Err(); err != nil {
		return Manifest{}, fmt.Errorf("%w: finalization canceled", ErrPublish)
	}
	if err := validateConfig(cfg); err != nil {
		return Manifest{}, err
	}
	finalizedAt := time.Now().UTC()
	if cfg.Now != nil {
		finalizedAt = cfg.Now().UTC()
	}

	resultBytes, err := readRegularFile(cfg.ResultPath, maximumResultBytes)
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: read result: %v", ErrInvalidResult, err)
	}
	words, canonicalResult, err := parseResult(resultBytes)
	if err != nil {
		return Manifest{}, err
	}
	logBytes, err := readRegularFile(cfg.LogPath, maximumLogBytes)
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: read log: %v", ErrInvalidLog, err)
	}
	parsed, err := parseLog(logBytes)
	if err != nil {
		return Manifest{}, err
	}
	if cfg.PolicyVerifiedAt != parsed.started.time.UTC().Format(policyDateLayout) {
		return Manifest{}, fmt.Errorf("%w: policy review must match the run UTC date", ErrInvalidConfig)
	}
	if err := ctx.Err(); err != nil {
		return Manifest{}, fmt.Errorf("%w: finalization canceled", ErrPublish)
	}
	if err := reconcile(parsed, words, cfg.ExitCode, finalizedAt); err != nil {
		return Manifest{}, err
	}

	binaryHash, binaryBuild, err := inspectBinary(ctx, cfg.BinaryPath)
	if err != nil {
		return Manifest{}, err
	}
	if !sameBuild(parsed.started.build, parsed.summary.build) || !sameBuild(parsed.started.build, binaryBuild) {
		return Manifest{}, fmt.Errorf("%w: binary and lifecycle build identities differ", ErrInvalidBinary)
	}

	fullLog := make([]byte, 0, len(logBytes)+len(canonicalResult)+len(fullLogResultMarker)+1)
	fullLog = append(fullLog, logBytes...)
	if len(fullLog) > 0 && fullLog[len(fullLog)-1] != '\n' {
		fullLog = append(fullLog, '\n')
	}
	fullLog = append(fullLog, fullLogResultMarker...)
	fullLog = append(fullLog, canonicalResult...)

	manifest := Manifest{
		Schema:         manifestSchema,
		Command:        fixedCommand,
		ExitCode:       cfg.ExitCode,
		TerminalStatus: parsed.summary.terminalStatus,
		Partial:        parsed.summary.partial,
		ResultWords:    len(words),
		StartedAt:      parsed.started.time.Format(time.RFC3339Nano),
		FinishedAt:     parsed.output.time.Format(time.RFC3339Nano),
		Build:          parsed.started.build,
		Config:         parsed.started.config,
		Inputs: InputsManifest{
			Posts:      parsed.sources["posts"],
			Dictionary: parsed.sources["dictionary"],
		},
		Summary:  parsed.summary.summary,
		Requests: parsed.summary.requests,
		Outcomes: parsed.outcomes,
		Policy: PolicyManifest{
			RedditPolicyVerifiedAt: cfg.PolicyVerifiedAt,
			ApprovalReference:      cfg.ApprovalReference,
		},
		Artifacts: ArtifactManifest{
			ResultSHA256:          digest(canonicalResult),
			ApplicationLogSHA256:  digest(logBytes),
			FullApplicationSHA256: digest(fullLog),
		},
	}
	manifest.Build.BinarySHA256 = binaryHash

	runDocument := renderRunDocument(manifest)
	manifest.Artifacts.RunDocumentSHA256 = digest(runDocument)
	manifestBytes, err := marshalPretty(manifest)
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: encode manifest", ErrPublish)
	}
	files := map[string][]byte{
		"result.json":          canonicalResult,
		"application.log":      logBytes,
		"full-application.log": fullLog,
		"run-manifest.json":    manifestBytes,
		"RUN.md":               runDocument,
	}
	if err := publish(ctx, cfg.OutputDir, files); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func validateConfig(cfg Config) error {
	for name, value := range map[string]string{
		"result path": cfg.ResultPath, "log path": cfg.LogPath,
		"output directory": cfg.OutputDir, "binary path": cfg.BinaryPath,
	} {
		if !safePath(value) {
			return fmt.Errorf("%w: %s is empty or unsafe", ErrInvalidConfig, name)
		}
	}
	if cfg.ExitCode != 0 && cfg.ExitCode != 3 {
		return fmt.Errorf("%w: exit code must be 0 or 3", ErrInvalidConfig)
	}
	now := time.Now
	if cfg.Now != nil {
		now = cfg.Now
	}
	date, err := time.Parse(policyDateLayout, cfg.PolicyVerifiedAt)
	today, _ := time.Parse(policyDateLayout, now().UTC().Format(policyDateLayout))
	if err != nil || date.Format(policyDateLayout) != cfg.PolicyVerifiedAt || date.After(today) || date.Before(today.AddDate(0, 0, -1)) {
		return fmt.Errorf("%w: policy verification date must be current or previous UTC date", ErrInvalidConfig)
	}
	if !approvalPattern.MatchString(cfg.ApprovalReference) || len(cfg.ApprovalReference) > maximumApprovalBytes || containsUnsafeApprovalMarker(cfg.ApprovalReference) {
		return fmt.Errorf("%w: approval reference must be a non-secret safe identifier", ErrInvalidConfig)
	}
	return nil
}

func safePath(value string) bool {
	return value != "" && len(value) <= 4<<10 && strings.TrimSpace(value) == value &&
		!strings.ContainsRune(value, '\x00') && utf8.ValidString(value)
}

func readRegularFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 0 || info.Size() > limit {
		return nil, errors.New("expected a bounded regular file, not a symlink")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if int64(len(data)) > limit {
		_ = file.Close()
		return nil, errors.New("file exceeds byte limit")
	}
	after, err := file.Stat()
	closeErr := file.Close()
	if err != nil || closeErr != nil || !os.SameFile(info, after) || after.Size() != int64(len(data)) ||
		!after.ModTime().Equal(info.ModTime()) {
		return nil, errors.New("file changed while reading")
	}
	return data, nil
}

func parseResult(data []byte) ([]resultWord, []byte, error) {
	if len(data) == 0 || !utf8.Valid(data) || containsControl(data, true) {
		return nil, nil, fmt.Errorf("%w: unsafe or empty JSON", ErrInvalidResult)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var words []resultWord
	if err := decoder.Decode(&words); err != nil {
		return nil, nil, fmt.Errorf("%w: decode top-ten array", ErrInvalidResult)
	}
	if words == nil || len(words) > maximumResultWords || decoder.Decode(new(any)) != io.EOF {
		return nil, nil, fmt.Errorf("%w: expected one non-null array with at most ten entries", ErrInvalidResult)
	}
	seen := make(map[string]struct{}, len(words))
	for index, word := range words {
		if !validWord(word.Word) || word.Count == 0 {
			return nil, nil, fmt.Errorf("%w: invalid word entry", ErrInvalidResult)
		}
		if _, duplicate := seen[word.Word]; duplicate {
			return nil, nil, fmt.Errorf("%w: duplicate result word", ErrInvalidResult)
		}
		seen[word.Word] = struct{}{}
		if index > 0 && (words[index-1].Count < word.Count ||
			(words[index-1].Count == word.Count && words[index-1].Word >= word.Word)) {
			return nil, nil, fmt.Errorf("%w: entries are not in deterministic rank order", ErrInvalidResult)
		}
	}
	canonical, err := marshalPretty(words)
	if err != nil || !bytes.Equal(data, canonical) {
		return nil, nil, fmt.Errorf("%w: bytes do not match canonical pretty JSON", ErrInvalidResult)
	}
	return words, canonical, nil
}

func validWord(value string) bool {
	if value == "" || len(value) > 4<<10 || strings.ToLower(value) != value || !utf8.ValidString(value) {
		return false
	}
	runes := 0
	for _, char := range value {
		if !unicode.IsLetter(char) {
			return false
		}
		runes++
	}
	return runes >= 3 && runes <= 4<<10
}

func marshalPretty(value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func parseLog(data []byte) (parsedLog, error) {
	if len(data) == 0 || data[len(data)-1] != '\n' || bytes.ContainsRune(data, '\r') || !utf8.Valid(data) ||
		containsControl(data, true) {
		return parsedLog{}, fmt.Errorf("%w: unsafe or empty log", ErrInvalidLog)
	}
	parsed := parsedLog{sources: make(map[string]SourceManifest)}
	loaded := make(map[string]SourceManifest)
	parsedSources := make(map[string]SourceManifest)
	seenPosts := make(map[string]struct{})
	nextSourceRetryAttempt := map[string]int{"posts": 2, "dictionary": 2}
	counts := map[string]int{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4<<10), maximumLogLineBytes)
	line := 0
	outcomesStarted := false
	for scanner.Scan() {
		line++
		if line > maximumLogRecords || len(scanner.Bytes()) == 0 {
			return parsedLog{}, fmt.Errorf("%w: invalid record count or blank line", ErrInvalidLog)
		}
		record, err := decodeRecord(scanner.Bytes())
		if err != nil {
			return parsedLog{}, fmt.Errorf("%w: line %d", ErrInvalidLog, line)
		}
		event, err := stringField(record, "event", true)
		if err != nil {
			return parsedLog{}, fmt.Errorf("%w: line %d event", ErrInvalidLog, line)
		}
		if err := validateRecordSchema(record, event); err != nil {
			return parsedLog{}, fmt.Errorf("%w: line %d %s schema", ErrInvalidLog, line, event)
		}
		recordTime, err := timeField(record)
		if err != nil {
			return parsedLog{}, fmt.Errorf("%w: line %d %s time", ErrInvalidLog, line, event)
		}
		if counts["run_started"] == 0 && event != "run_started" {
			return parsedLog{}, fmt.Errorf("%w: run_started must be the first record", ErrInvalidLog)
		}
		if counts["output_written"] != 0 || (counts["run_summary"] != 0 && event != "output_written") {
			return parsedLog{}, fmt.Errorf("%w: record follows terminal lifecycle", ErrInvalidLog)
		}
		counts[event]++
		switch event {
		case "run_started":
			parsed.started, err = parseStarted(record)
		case "source_loaded":
			var source SourceManifest
			var kind string
			kind, source, err = parseLoaded(record)
			if err == nil {
				if (kind == "posts" && (len(loaded) != 0 || len(parsedSources) != 0)) ||
					(kind == "dictionary" && (parsedSources["posts"].SHA256 == "" || loaded["dictionary"].SHA256 != "" || outcomesStarted)) {
					err = errors.New("source loaded outside expected lifecycle stage")
				}
			}
			if err == nil {
				if _, duplicate := loaded[kind]; duplicate {
					err = errors.New("duplicate loaded source")
				} else {
					loaded[kind] = source
				}
			}
		case "source_parsed":
			var source SourceManifest
			var kind string
			kind, source, err = parseParsedSource(record)
			if err == nil {
				if loaded[kind].SHA256 == "" || (kind == "posts" && loaded["dictionary"].SHA256 != "") ||
					(kind == "dictionary" && parsedSources["posts"].SHA256 == "") || outcomesStarted {
					err = errors.New("source parsed outside expected lifecycle stage")
				}
			}
			if err == nil {
				if _, duplicate := parsedSources[kind]; duplicate {
					err = errors.New("duplicate parsed source")
				} else {
					parsedSources[kind] = source
				}
			}
		case "post_outcome":
			if parsedSources["dictionary"].SHA256 == "" {
				err = errors.New("post outcome precedes parsed inputs")
				break
			}
			outcomesStarted = true
			if len(parsed.outcomes) == maximumPostOutcomes {
				err = errors.New("too many post outcomes")
				break
			}
			var outcome OutcomeManifest
			outcome, err = parseOutcome(record)
			if err == nil {
				if _, duplicate := seenPosts[outcome.PostID]; duplicate {
					err = errors.New("duplicate post outcome")
				} else {
					seenPosts[outcome.PostID] = struct{}{}
					parsed.outcomes = append(parsed.outcomes, outcome)
				}
			}
		case "run_summary":
			if parsedSources["dictionary"].SHA256 == "" {
				err = errors.New("summary precedes parsed inputs")
				break
			}
			parsed.summary, err = parseSummary(record)
		case "output_written":
			parsed.output, err = parseOutput(record)
		case "request_retry":
			var retry retryRecord
			retry, err = validateRetry(record)
			if err == nil {
				if retry.source {
					kind := retry.sourceKind
					if outcomesStarted || (kind == "posts" && (loaded["posts"].SHA256 != "" || parsedSources["posts"].SHA256 != "")) ||
						(kind == "dictionary" && (parsedSources["posts"].SHA256 == "" || loaded["dictionary"].SHA256 != "")) {
						err = errors.New("source retry outside expected lifecycle stage")
					}
					if err == nil && retry.attempt != nextSourceRetryAttempt[kind] {
						err = errors.New("source retry attempt sequence is impossible")
					}
					if err == nil {
						nextSourceRetryAttempt[kind]++
						parsed.sourceRetryEvents++
					}
				} else {
					if parsedSources["dictionary"].SHA256 == "" || outcomesStarted {
						err = errors.New("reddit retry outside expected lifecycle stage")
					}
					if err == nil {
						parsed.redditRetries = append(parsed.redditRetries, retry)
						parsed.redditRetryEvents++
					}
				}
			}
		default:
			err = errors.New("unsupported lifecycle event")
		}
		if err != nil {
			return parsedLog{}, fmt.Errorf("%w: line %d %s: %v", ErrInvalidLog, line, event, err)
		}
		if event != "run_started" && event != "run_summary" && event != "output_written" {
			parsed.intermediateTimes = append(parsed.intermediateTimes, eventTimestamp{event: event, time: recordTime})
		}
		if counts["run_started"] > 1 || counts["run_summary"] > 1 || counts["output_written"] > 1 {
			return parsedLog{}, fmt.Errorf("%w: duplicate terminal lifecycle event", ErrInvalidLog)
		}
	}
	if err := scanner.Err(); err != nil {
		return parsedLog{}, fmt.Errorf("%w: scan records", ErrInvalidLog)
	}
	for _, event := range []string{"run_started", "run_summary", "output_written"} {
		if counts[event] != 1 {
			return parsedLog{}, fmt.Errorf("%w: expected exactly one %s event", ErrInvalidLog, event)
		}
	}
	for _, kind := range []string{"posts", "dictionary"} {
		if counts["source_loaded"] != 2 || counts["source_parsed"] != 2 {
			return parsedLog{}, fmt.Errorf("%w: expected two loaded and parsed sources", ErrInvalidLog)
		}
		loadedSource, loadedOK := loaded[kind]
		parsedSource, parsedOK := parsedSources[kind]
		if !loadedOK || !parsedOK || loadedSource.SHA256 != parsedSource.SHA256 {
			return parsedLog{}, fmt.Errorf("%w: source %s provenance mismatch", ErrInvalidLog, kind)
		}
		loadedSource.Entries = parsedSource.Entries
		loadedSource.IDsSHA256 = parsedSource.IDsSHA256
		parsed.sources[kind] = loadedSource
	}
	return parsed, nil
}

func decodeRecord(line []byte) (logRecord, error) {
	decoder := json.NewDecoder(bytes.NewReader(line))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("expected one JSON object")
	}
	record := make(logRecord)
	for decoder.More() {
		keyToken, keyErr := decoder.Token()
		key, keyOK := keyToken.(string)
		if keyErr != nil || !keyOK {
			return nil, errors.New("invalid JSON object key")
		}
		if _, duplicate := record[key]; duplicate {
			return nil, errors.New("duplicate JSON object key")
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, errors.New("invalid JSON object value")
		}
		if !safeFieldName(key) || len(raw) > maximumLogLineBytes {
			return nil, errors.New("unsafe JSON field")
		}
		record[key] = raw
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') || decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("expected one JSON object")
	}
	return record, nil
}

func validateRecordSchema(record logRecord, event string) error {
	allowed := map[string]struct{}{
		"time": {}, "level": {}, "msg": {}, "event": {},
	}
	eventFields := map[string][]string{
		"run_started":    {"workers", "failure_mode", "input_profile", "filter_count", "rate_limit_rps", "request_timeout", "global_timeout", "max_retries", "retry_budget", "source_max_retries", "source_retry_budget", "max_distinct_words_per_post", "max_in_flight_response_bytes", "max_retained_things", "app_version", "app_commit", "app_build_date", "go_version", "goos", "goarch"},
		"source_loaded":  {"source_kind", "source_mode", "source_origin", "source_bytes", "source_sha256"},
		"source_parsed":  {"source_kind", "stage", "entries", "source_sha256", "posts_sha256"},
		"request_retry":  {"operation", "post_id", "source_kind", "error_class", "http_status", "attempt", "delay"},
		"post_outcome":   {"post_id", "source_line", "status", "comments", "bodies_visited", "more_requests", "continuation_requests", "counted_tokens", "error_class", "operation", "http_status"},
		"run_summary":    {"terminal_status", "partial", "failure_mode", "workers", "input_profile", "filter_count", "duration", "posts_total", "posts_completed", "posts_skipped", "posts_failed", "posts_incomplete", "comments", "bodies_visited", "more_requests", "continuation_requests", "counted_tokens", "distinct_words", "dictionary_words", "source_retries", "reddit_http_attempts", "reddit_retries", "throttle_waits", "throttle_wait", "posts_sha256", "post_ids_sha256", "dictionary_sha256", "app_version", "app_commit", "app_build_date", "go_version", "goos", "goarch"},
		"output_written": {"partial", "result_words", "result_sha256"},
	}
	fields, known := eventFields[event]
	if !known {
		return errors.New("unsupported event schema")
	}
	for _, key := range fields {
		allowed[key] = struct{}{}
	}
	for key := range record {
		if _, ok := allowed[key]; !ok {
			return errors.New("unknown event field")
		}
	}
	level, err := stringField(record, "level", true)
	if err != nil {
		return err
	}
	wantLevel := "INFO"
	if event == "request_retry" {
		wantLevel = "WARN"
	}
	if level != wantLevel {
		return errors.New("unexpected event level")
	}
	message, err := stringField(record, "msg", true)
	if err != nil {
		return err
	}
	wantMessage := map[string]string{
		"run_started": "run started", "source_loaded": "source loaded", "source_parsed": "source parsed",
		"request_retry": "request retry scheduled", "post_outcome": "post processing completed",
		"run_summary": "processing summary", "output_written": "result written",
	}[event]
	if message != wantMessage {
		return errors.New("unexpected event message")
	}
	return nil
}

func safeFieldName(key string) bool {
	if key == "" || len(key) > 64 {
		return false
	}
	for index := range len(key) {
		char := key[index]
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '_' {
			return false
		}
	}
	return true
}

func parseStarted(record logRecord) (startedRecord, error) {
	timestamp, err := timeField(record)
	if err != nil {
		return startedRecord{}, fmt.Errorf("time: %w", err)
	}
	workers, err := intField(record, "workers", 1, 32)
	if err != nil {
		return startedRecord{}, fmt.Errorf("workers: %w", err)
	}
	filterCount, err := intField(record, "filter_count", 0, 100)
	if err != nil {
		return startedRecord{}, fmt.Errorf("filter_count: %w", err)
	}
	failureMode, err := enumField(record, "failure_mode", "best-effort", "strict")
	if err != nil {
		return startedRecord{}, fmt.Errorf("failure_mode: %w", err)
	}
	inputProfile, err := enumField(record, "input_profile", assignmentInputProfile, "custom")
	if err != nil {
		return startedRecord{}, fmt.Errorf("input_profile: %w", err)
	}
	rateLimit, err := floatField(record, "rate_limit_rps", 0.1, 1.5)
	if err != nil {
		return startedRecord{}, fmt.Errorf("rate_limit_rps: %w", err)
	}
	requestTimeout, err := durationField(record, "request_timeout", time.Second, 2*time.Minute)
	if err != nil {
		return startedRecord{}, fmt.Errorf("request_timeout: %w", err)
	}
	globalTimeout, err := durationField(record, "global_timeout", time.Second, 2*time.Hour)
	if err != nil {
		return startedRecord{}, fmt.Errorf("global_timeout: %w", err)
	}
	maxRetries, err := intField(record, "max_retries", 0, 5)
	if err != nil {
		return startedRecord{}, err
	}
	retryBudget, err := durationField(record, "retry_budget", time.Second, 5*time.Minute)
	if err != nil {
		return startedRecord{}, fmt.Errorf("retry_budget: %w", err)
	}
	sourceMaxRetries, err := intField(record, "source_max_retries", 0, 2)
	if err != nil {
		return startedRecord{}, err
	}
	sourceRetryBudget, err := durationField(record, "source_retry_budget", time.Second, 15*time.Second)
	if err != nil {
		return startedRecord{}, fmt.Errorf("source_retry_budget: %w", err)
	}
	maxDistinct, err := intField(record, "max_distinct_words_per_post", 1, 1_000_000)
	if err != nil {
		return startedRecord{}, err
	}
	maxResponse, err := int64Field(record, "max_in_flight_response_bytes", 1, 1<<30)
	if err != nil {
		return startedRecord{}, err
	}
	maxThings, err := intField(record, "max_retained_things", 1, 10_000_000)
	if err != nil {
		return startedRecord{}, err
	}
	build, err := parseBuild(record)
	if err != nil {
		return startedRecord{}, fmt.Errorf("build: %w", err)
	}
	return startedRecord{time: timestamp, build: build, config: ConfigManifest{
		Workers: workers, FailureMode: failureMode, InputProfile: inputProfile, RateLimitRPS: rateLimit,
		RequestTimeout: requestTimeout.String(), GlobalTimeout: globalTimeout.String(),
		MaxRetries: maxRetries, RetryBudget: retryBudget.String(), SourceMaxRetries: sourceMaxRetries,
		SourceRetryBudget: sourceRetryBudget.String(), MaxDistinctWordsPerPost: maxDistinct,
		MaxInFlightResponseBytes: maxResponse, MaxRetainedThings: maxThings, FilterCount: filterCount,
	}}, nil
}

func parseBuild(record logRecord) (BuildManifest, error) {
	version, err := stringField(record, "app_version", true)
	if err != nil || !semverPattern.MatchString(version) {
		return BuildManifest{}, errors.New("invalid release version")
	}
	commit, err := stringField(record, "app_commit", true)
	if err != nil || !validCommit(commit) {
		return BuildManifest{}, errors.New("invalid full commit")
	}
	buildDate, err := stringField(record, "app_build_date", true)
	if err != nil {
		return BuildManifest{}, err
	}
	if date, parseErr := time.Parse(buildDateLayout, buildDate); parseErr != nil || date.Format(buildDateLayout) != buildDate {
		return BuildManifest{}, errors.New("invalid build date")
	}
	goVersion, err := stringField(record, "go_version", true)
	if err != nil || !goVersionPattern.MatchString(goVersion) {
		return BuildManifest{}, errors.New("unexpected Go version")
	}
	goos, err := stringField(record, "goos", true)
	if err != nil || !platformPattern.MatchString(goos) {
		return BuildManifest{}, errors.New("invalid goos")
	}
	goarch, err := stringField(record, "goarch", true)
	if err != nil || !platformPattern.MatchString(goarch) {
		return BuildManifest{}, errors.New("invalid goarch")
	}
	return BuildManifest{Version: version, Commit: commit, BuildDate: buildDate, GoVersion: goVersion, GOOS: goos, GOARCH: goarch}, nil
}

func parseLoaded(record logRecord) (string, SourceManifest, error) {
	kind, err := enumField(record, "source_kind", "posts", "dictionary")
	if err != nil {
		return "", SourceManifest{}, err
	}
	mode, err := enumField(record, "source_mode", "https")
	if err != nil {
		return "", SourceManifest{}, err
	}
	origin, err := stringField(record, "source_origin", true)
	wantOrigin := "gist.githubusercontent.com"
	if kind == "dictionary" {
		wantOrigin = "raw.githubusercontent.com"
	}
	if err != nil || origin != wantOrigin {
		return "", SourceManifest{}, errors.New("invalid source origin")
	}
	bytesCount, err := int64Field(record, "source_bytes", 1, 64<<20)
	if err != nil {
		return "", SourceManifest{}, err
	}
	hash, err := digestField(record, "source_sha256")
	if err != nil {
		return "", SourceManifest{}, err
	}
	return kind, SourceManifest{Mode: mode, Origin: origin, Bytes: bytesCount, SHA256: hash}, nil
}

func parseParsedSource(record logRecord) (string, SourceManifest, error) {
	kind, err := enumField(record, "source_kind", "posts", "dictionary")
	if err != nil {
		return "", SourceManifest{}, err
	}
	if stage, stageErr := stringField(record, "stage", true); stageErr != nil || stage != "parsed" {
		return "", SourceManifest{}, errors.New("invalid source stage")
	}
	entriesMaximum := int64(10_000_000)
	entries, err := int64Field(record, "entries", 1, entriesMaximum)
	if err != nil {
		return "", SourceManifest{}, err
	}
	hash, err := digestField(record, "source_sha256")
	if err != nil {
		return "", SourceManifest{}, err
	}
	idsHash := ""
	_, hasIDsHash := record["posts_sha256"]
	if kind == "posts" {
		if !hasIDsHash {
			return "", SourceManifest{}, errors.New("post source lacks normalized ID digest")
		}
		idsHash, err = digestField(record, "posts_sha256")
		if err != nil {
			return "", SourceManifest{}, err
		}
	} else if hasIDsHash {
		return "", SourceManifest{}, errors.New("dictionary unexpectedly has a post ID digest")
	}
	return kind, SourceManifest{Entries: int(entries), SHA256: hash, IDsSHA256: idsHash}, nil
}

func parseOutcome(record logRecord) (OutcomeManifest, error) {
	postID, err := stringField(record, "post_id", true)
	if err != nil || !postIDPattern.MatchString(postID) {
		return OutcomeManifest{}, errors.New("invalid post ID")
	}
	sourceLine, err := intField(record, "source_line", 1, 10_000_000)
	if err != nil {
		return OutcomeManifest{}, err
	}
	// skipped is admitted only under the exact contract in validOutcomeFailure: the
	// comments endpoint reported not_found, which is the one signal that proves a post
	// is absent. A 403, or a 404 from an expansion endpoint, remains a failure.
	status, err := enumField(record, "status", "completed", "skipped", "failed", "incomplete")
	if err != nil {
		return OutcomeManifest{}, err
	}
	outcome := OutcomeManifest{PostID: postID, SourceLine: sourceLine, Status: status}
	if outcome.Comments, err = intField(record, "comments", 0, 10_000_000); err != nil {
		return OutcomeManifest{}, err
	}
	if outcome.BodiesVisited, err = intField(record, "bodies_visited", 0, 10_000_000); err != nil {
		return OutcomeManifest{}, err
	}
	if outcome.MoreRequests, err = intField(record, "more_requests", 0, 20_000); err != nil {
		return OutcomeManifest{}, err
	}
	if outcome.ContinuationRequests, err = intField(record, "continuation_requests", 0, 20_000); err != nil {
		return OutcomeManifest{}, err
	}
	if outcome.CountedTokens, err = uintField(record, "counted_tokens"); err != nil {
		return OutcomeManifest{}, err
	}
	if raw, exists := record["error_class"]; exists {
		if err := json.Unmarshal(raw, &outcome.ErrorClass); err != nil || !oneOf(outcome.ErrorClass,
			"forbidden", "not_found", "rate_limited", "server", "transport", "protocol", "incomplete", "resource_limit") {
			return OutcomeManifest{}, errors.New("invalid outcome error class")
		}
	}
	if raw, exists := record["operation"]; exists {
		if err := json.Unmarshal(raw, &outcome.Operation); err != nil || !oneOf(outcome.Operation, "oauth_token", "comments", "continuation", "morechildren") {
			return OutcomeManifest{}, errors.New("invalid outcome operation")
		}
	}
	if raw, exists := record["http_status"]; exists {
		if err := json.Unmarshal(raw, &outcome.HTTPStatus); err != nil || outcome.HTTPStatus < 100 || outcome.HTTPStatus > 599 {
			return OutcomeManifest{}, errors.New("invalid outcome HTTP status")
		}
	}
	if status == "completed" && (outcome.ErrorClass != "" || outcome.Operation != "" || outcome.HTTPStatus != 0) {
		return OutcomeManifest{}, errors.New("completed outcome contains failure metadata")
	}
	if status != "completed" && (outcome.ErrorClass == "" || outcome.CountedTokens != 0) {
		return OutcomeManifest{}, errors.New("untrusted outcome lacks classification or contains counted tokens")
	}
	if outcome.HTTPStatus != 0 && outcome.Operation == "" {
		return OutcomeManifest{}, errors.New("HTTP failure has no operation")
	}
	if (status == "incomplete") != oneOf(outcome.ErrorClass, "incomplete", "resource_limit") {
		return OutcomeManifest{}, errors.New("outcome status and error class differ")
	}
	if !validOutcomeFailure(outcome) {
		return OutcomeManifest{}, errors.New("outcome failure metadata is contradictory")
	}
	return outcome, nil
}

func validOutcomeFailure(outcome OutcomeManifest) bool {
	if outcome.Status == "completed" {
		return true
	}
	// A skipped post is provably absent. That is proven either by HTTP 404 on the
	// comments endpoint or by a validated empty post listing, which is a 200 response
	// and therefore carries no HTTP status of its own.
	if outcome.Status == "skipped" {
		return outcome.Operation == "comments" && outcome.ErrorClass == "not_found" &&
			(outcome.HTTPStatus == 404 || outcome.HTTPStatus == 0)
	}
	if outcome.Operation == "" {
		return outcome.Status == "incomplete" && outcome.ErrorClass == "resource_limit" && outcome.HTTPStatus == 0
	}
	switch outcome.ErrorClass {
	case "forbidden":
		return outcome.HTTPStatus == 403
	case "not_found":
		// The runner records an absent post as skipped, so a not_found failure can
		// only come from an expansion endpoint that could not complete the tree.
		return outcome.Status == "failed" && outcome.Operation != "comments" && outcome.HTTPStatus == 404
	case "rate_limited":
		return outcome.HTTPStatus == 429
	case "server":
		return outcome.HTTPStatus >= 500 && outcome.HTTPStatus <= 599
	case "transport":
		return outcome.HTTPStatus == 0 || outcome.HTTPStatus == 200 || outcome.HTTPStatus == 408
	case "protocol":
		return outcome.HTTPStatus == 0 || outcome.HTTPStatus >= 200 && outcome.HTTPStatus <= 599
	case "incomplete", "resource_limit":
		return outcome.HTTPStatus == 0 || outcome.HTTPStatus == 200
	default:
		return false
	}
}

func parseSummary(record logRecord) (summaryRecord, error) {
	timestamp, err := timeField(record)
	if err != nil {
		return summaryRecord{}, err
	}
	terminalStatus, err := enumField(record, "terminal_status", "complete", "partial")
	if err != nil {
		return summaryRecord{}, err
	}
	partial, err := boolField(record, "partial")
	if err != nil {
		return summaryRecord{}, err
	}
	failureMode, err := enumField(record, "failure_mode", "best-effort", "strict")
	if err != nil {
		return summaryRecord{}, err
	}
	workers, err := intField(record, "workers", 1, 32)
	if err != nil {
		return summaryRecord{}, err
	}
	inputProfile, err := enumField(record, "input_profile", assignmentInputProfile, "custom")
	if err != nil {
		return summaryRecord{}, err
	}
	filterCount, err := intField(record, "filter_count", 0, 100)
	if err != nil {
		return summaryRecord{}, err
	}
	duration, err := durationField(record, "duration", 0, maximumRunDuration)
	if err != nil {
		return summaryRecord{}, err
	}
	var summary SummaryManifest
	fields := []struct {
		name string
		dst  *int
		max  int64
	}{
		{"posts_total", &summary.PostsTotal, maximumPostOutcomes}, {"posts_completed", &summary.PostsCompleted, maximumPostOutcomes},
		{"posts_skipped", &summary.PostsSkipped, maximumPostOutcomes}, {"posts_failed", &summary.PostsFailed, maximumPostOutcomes},
		{"posts_incomplete", &summary.PostsIncomplete, maximumPostOutcomes}, {"distinct_words", &summary.DistinctWords, 10_000_000},
		{"dictionary_words", &summary.DictionaryWords, 10_000_000},
	}
	for _, field := range fields {
		value, fieldErr := int64Field(record, field.name, 0, field.max)
		if fieldErr != nil {
			return summaryRecord{}, fieldErr
		}
		*field.dst = int(value)
	}
	uints := []struct {
		name string
		dst  *uint64
	}{
		{"comments", &summary.Comments}, {"bodies_visited", &summary.BodiesVisited}, {"more_requests", &summary.MoreRequests},
		{"continuation_requests", &summary.ContinuationRequests}, {"counted_tokens", &summary.CountedTokens},
	}
	for _, field := range uints {
		if *field.dst, err = uintField(record, field.name); err != nil {
			return summaryRecord{}, err
		}
	}
	summary.Duration = duration.String()
	requests := RequestManifest{}
	if requests.SourceRetries, err = uintField(record, "source_retries"); err != nil {
		return summaryRecord{}, err
	}
	if requests.RedditHTTPAttempts, err = uintField(record, "reddit_http_attempts"); err != nil {
		return summaryRecord{}, err
	}
	if requests.RedditRetries, err = uintField(record, "reddit_retries"); err != nil {
		return summaryRecord{}, err
	}
	if requests.ThrottleWaits, err = uintField(record, "throttle_waits"); err != nil {
		return summaryRecord{}, err
	}
	wait, err := durationField(record, "throttle_wait", 0, 2*time.Hour)
	if err != nil {
		return summaryRecord{}, err
	}
	requests.ThrottleWait = wait.String()
	postsHash, err := digestField(record, "posts_sha256")
	if err != nil {
		return summaryRecord{}, err
	}
	postIDsHash, err := digestField(record, "post_ids_sha256")
	if err != nil {
		return summaryRecord{}, err
	}
	dictionaryHash, err := digestField(record, "dictionary_sha256")
	if err != nil {
		return summaryRecord{}, err
	}
	build, err := parseBuild(record)
	if err != nil {
		return summaryRecord{}, err
	}
	return summaryRecord{time: timestamp, duration: duration, terminalStatus: terminalStatus, partial: partial, summary: summary,
		requests: requests, postsHash: postsHash, postIDsHash: postIDsHash, dictionaryHash: dictionaryHash,
		failureMode: failureMode, workers: workers, inputProfile: inputProfile, filterCount: filterCount, build: build}, nil
}

func parseOutput(record logRecord) (outputRecord, error) {
	timestamp, err := timeField(record)
	if err != nil {
		return outputRecord{}, err
	}
	partial, err := boolField(record, "partial")
	if err != nil {
		return outputRecord{}, err
	}
	words, err := intField(record, "result_words", 0, maximumResultWords)
	if err != nil {
		return outputRecord{}, err
	}
	resultHash, err := digestField(record, "result_sha256")
	if err != nil {
		return outputRecord{}, err
	}
	return outputRecord{time: timestamp, partial: partial, resultWords: words, resultHash: resultHash}, nil
}

func validateRetry(record logRecord) (retryRecord, error) {
	if _, err := timeField(record); err != nil {
		return retryRecord{}, err
	}
	operation, err := enumField(record, "operation", "source_download", "oauth_token", "comments", "continuation", "morechildren")
	if err != nil {
		return retryRecord{}, err
	}
	maximumAttempt := 5
	maximumDelay := 45 * time.Second
	if operation == "source_download" {
		maximumAttempt = 3
		maximumDelay = 15 * time.Second
	} else if operation == "oauth_token" {
		maximumAttempt = 4
	}
	attempt, err := intField(record, "attempt", 2, maximumAttempt)
	if err != nil {
		return retryRecord{}, err
	}
	delay, err := durationField(record, "delay", 0, maximumDelay)
	if err != nil || delay >= maximumDelay {
		return retryRecord{}, errors.New("invalid retry delay")
	}
	errorClass, err := enumField(record, "error_class", "transport", "http_status", "read", "close",
		"authentication", "forbidden", "not_found", "rate_limited", "server", "protocol", "incomplete", "resource_limit", "canceled", "visitor")
	if err != nil {
		return retryRecord{}, err
	}
	status, hasStatus, err := retryStatusField(record)
	if err != nil {
		return retryRecord{}, err
	}
	if operation == "source_download" {
		if !oneOf(errorClass, "transport", "http_status", "read", "close") {
			return retryRecord{}, errors.New("invalid source retry class")
		}
		if _, exists := record["post_id"]; exists {
			return retryRecord{}, errors.New("source retry contains a post ID")
		}
		sourceKind, kindErr := enumField(record, "source_kind", "posts", "dictionary")
		if kindErr != nil {
			return retryRecord{}, kindErr
		}
		if delay <= 0 || !validSourceRetryStatus(errorClass, status, hasStatus) {
			return retryRecord{}, errors.New("source retry metadata contradicts producer contract")
		}
		return retryRecord{source: true, sourceKind: sourceKind, attempt: attempt}, nil
	}
	if _, exists := record["source_kind"]; exists {
		return retryRecord{}, errors.New("reddit retry contains source kind")
	}
	if !oneOf(errorClass, "transport", "rate_limited", "server", "authentication") {
		return retryRecord{}, errors.New("invalid Reddit retry class")
	}
	var postID string
	if raw, exists := record["post_id"]; !exists || json.Unmarshal(raw, &postID) != nil {
		return retryRecord{}, errors.New("invalid retry post ID")
	}
	if operation == "oauth_token" {
		if postID != "" || errorClass == "authentication" {
			return retryRecord{}, errors.New("OAuth retry unexpectedly names a post")
		}
	} else if !postIDPattern.MatchString(postID) {
		return retryRecord{}, errors.New("invalid retry post ID")
	}
	if !hasStatus {
		return retryRecord{}, errors.New("reddit retry lacks producer-emitted HTTP status")
	}
	if (errorClass == "authentication" && status != 401) ||
		(errorClass == "rate_limited" && status != 429) ||
		(errorClass == "server" && !oneOf(strconv.Itoa(status), "500", "502", "503", "504")) ||
		(errorClass == "transport" && status != 0 && status != 200 && status != 408) {
		return retryRecord{}, errors.New("retry class and HTTP status differ")
	}
	if (errorClass == "authentication") != (delay == 0) {
		return retryRecord{}, errors.New("retry class and delay differ")
	}
	return retryRecord{operation: operation, postID: postID, attempt: attempt}, nil
}

func retryStatusField(record logRecord) (int, bool, error) {
	raw, exists := record["http_status"]
	if !exists {
		return 0, false, nil
	}
	var status int
	if err := json.Unmarshal(raw, &status); err != nil || status < 0 || status > 599 || (status != 0 && status < 100) {
		return 0, false, errors.New("invalid retry HTTP status")
	}
	return status, true, nil
}

func validSourceRetryStatus(errorClass string, status int, hasStatus bool) bool {
	switch errorClass {
	case "transport", "close":
		return !hasStatus
	case "read":
		return hasStatus && status == 200
	case "http_status":
		return hasStatus && oneOf(strconv.Itoa(status), "408", "429", "500", "502", "503", "504")
	default:
		return false
	}
}

func reconcile(parsed parsedLog, resultWords []resultWord, exitCode int, finalizedAt time.Time) error {
	resultWordCount := len(resultWords)
	summary := parsed.summary.summary
	if parsed.started.time.After(parsed.summary.time) || parsed.summary.time.After(parsed.output.time) {
		return fmt.Errorf("%w: lifecycle timestamps are out of order", ErrInvalidLog)
	}
	if parsed.started.time.After(finalizedAt) || parsed.summary.time.After(finalizedAt) || parsed.output.time.After(finalizedAt) {
		return fmt.Errorf("%w: lifecycle timestamp is in the future", ErrInvalidLog)
	}
	for _, timestamp := range parsed.intermediateTimes {
		if timestamp.time.Before(parsed.started.time) || timestamp.time.After(parsed.summary.time) || timestamp.time.After(finalizedAt) {
			return fmt.Errorf("%w: %s timestamp is outside the run lifecycle", ErrInvalidLog, timestamp.event)
		}
	}
	observedDuration := parsed.summary.time.Sub(parsed.started.time)
	if observedDuration > maximumRunDuration+durationClockTolerance ||
		absoluteDurationDifference(observedDuration, parsed.summary.duration) > durationClockTolerance {
		return fmt.Errorf("%w: summary duration contradicts lifecycle timestamps", ErrInvalidLog)
	}
	if parsed.started.config.Workers != parsed.summary.workers || parsed.started.config.FailureMode != parsed.summary.failureMode ||
		parsed.started.config.InputProfile != parsed.summary.inputProfile || parsed.started.config.InputProfile != assignmentInputProfile ||
		parsed.started.config.FilterCount != parsed.summary.filterCount || parsed.started.config.FilterCount != 0 {
		return fmt.Errorf("%w: start and summary configuration differ", ErrInvalidLog)
	}
	wantConfig := ConfigManifest{
		Workers: 4, FailureMode: "best-effort", InputProfile: assignmentInputProfile,
		RateLimitRPS: 0.8, RequestTimeout: "20s", GlobalTimeout: "30m0s",
		MaxRetries: 3, RetryBudget: "45s", SourceMaxRetries: 2,
		SourceRetryBudget: "15s", MaxDistinctWordsPerPost: 50_000,
		MaxInFlightResponseBytes: 32 << 20, MaxRetainedThings: 500_000,
		FilterCount: 0,
	}
	if parsed.started.config != wantConfig {
		return fmt.Errorf("%w: execution did not use the reviewed assignment configuration", ErrInvalidLog)
	}
	if parsed.sources["posts"].SHA256 != parsed.summary.postsHash || parsed.sources["dictionary"].SHA256 != parsed.summary.dictionaryHash {
		return fmt.Errorf("%w: source and summary hashes differ", ErrInvalidLog)
	}
	if parsed.sources["posts"].IDsSHA256 != parsed.summary.postIDsHash || parsed.sources["posts"].IDsSHA256 == "" {
		return fmt.Errorf("%w: normalized post identity hashes differ", ErrInvalidLog)
	}
	if parsed.sources["posts"].Entries != assignmentPostCount || summary.PostsTotal != assignmentPostCount ||
		parsed.sources["posts"].Entries != summary.PostsTotal || parsed.sources["dictionary"].Entries != summary.DictionaryWords {
		return fmt.Errorf("%w: parsed input counts do not match summary", ErrInvalidLog)
	}
	if len(parsed.outcomes) != summary.PostsTotal || parsed.output.resultWords != resultWordCount ||
		parsed.output.resultHash != digest(mustMarshalResult(resultWords)) {
		return fmt.Errorf("%w: output or outcome cardinality mismatch", ErrInvalidLog)
	}
	if parsed.output.partial != parsed.summary.partial || (parsed.summary.terminalStatus == "partial") != parsed.summary.partial {
		return fmt.Errorf("%w: inconsistent partial indicators", ErrInvalidLog)
	}
	if (exitCode == 3) != parsed.summary.partial || (exitCode == 0) != !parsed.summary.partial {
		return fmt.Errorf("%w: exit code and terminal status differ", ErrInvalidLog)
	}
	// Skipped posts are provably absent and cannot be counted, so they never make a
	// run partial. Only failed and incomplete posts withhold countable data.
	if parsed.summary.partial != (summary.PostsFailed+summary.PostsIncomplete > 0) {
		return fmt.Errorf("%w: terminal completeness status contradicts outcomes", ErrInvalidLog)
	}
	if summary.PostsCompleted+summary.PostsSkipped+summary.PostsFailed+summary.PostsIncomplete != summary.PostsTotal {
		return fmt.Errorf("%w: summary post counts do not reconcile", ErrInvalidLog)
	}
	var completed, skipped, failed, incomplete int
	var comments, bodies, moreRequests, continuations, countedTokens uint64
	postIdentity := sha256.New()
	outcomesByPostID := make(map[string]OutcomeManifest, len(parsed.outcomes))
	lastSourceLine := 0
	for _, outcome := range parsed.outcomes {
		if outcome.SourceLine <= lastSourceLine {
			return fmt.Errorf("%w: outcomes are not in source order", ErrInvalidLog)
		}
		if outcome.BodiesVisited > outcome.Comments || (outcome.CountedTokens > 0 && outcome.BodiesVisited == 0) {
			return fmt.Errorf("%w: impossible per-post traversal counters", ErrInvalidLog)
		}
		lastSourceLine = outcome.SourceLine
		outcomesByPostID[outcome.PostID] = outcome
		_, _ = fmt.Fprintf(postIdentity, "%d:%s\n", outcome.SourceLine, outcome.PostID)
		switch outcome.Status {
		case "completed":
			completed++
			if !checkedAdd(&comments, uint64(outcome.Comments)) || !checkedAdd(&bodies, uint64(outcome.BodiesVisited)) ||
				!checkedAdd(&moreRequests, uint64(outcome.MoreRequests)) || !checkedAdd(&continuations, uint64(outcome.ContinuationRequests)) ||
				!checkedAdd(&countedTokens, outcome.CountedTokens) {
				return fmt.Errorf("%w: outcome counter overflow", ErrInvalidLog)
			}
		case "skipped":
			skipped++
		case "failed":
			failed++
		case "incomplete":
			incomplete++
		}
	}
	if hex.EncodeToString(postIdentity.Sum(nil)) != parsed.summary.postIDsHash {
		return fmt.Errorf("%w: outcomes do not match normalized post identity", ErrInvalidLog)
	}
	if completed != summary.PostsCompleted || skipped != summary.PostsSkipped || failed != summary.PostsFailed ||
		incomplete != summary.PostsIncomplete || comments != summary.Comments || bodies != summary.BodiesVisited ||
		moreRequests != summary.MoreRequests || continuations != summary.ContinuationRequests || countedTokens != summary.CountedTokens {
		return fmt.Errorf("%w: per-post counters do not match summary", ErrInvalidLog)
	}
	if parsed.sourceRetryEvents != parsed.summary.requests.SourceRetries || parsed.redditRetryEvents != parsed.summary.requests.RedditRetries {
		return fmt.Errorf("%w: retry events do not match summary", ErrInvalidLog)
	}
	if err := reconcileRedditRetries(parsed.redditRetries, outcomesByPostID); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidLog, err)
	}
	var rankedTokens uint64
	for _, word := range resultWords {
		if !checkedAdd(&rankedTokens, word.Count) {
			return fmt.Errorf("%w: ranked count overflow", ErrInvalidLog)
		}
	}
	if summary.PostsCompleted == 0 || resultWordCount != min(summary.DistinctWords, maximumResultWords) ||
		summary.DistinctWords > summary.DictionaryWords ||
		summary.BodiesVisited > summary.Comments || (summary.CountedTokens == 0) != (summary.DistinctWords == 0) ||
		rankedTokens > summary.CountedTokens || parsed.summary.requests.RedditHTTPAttempts < parsed.summary.requests.RedditRetries {
		return fmt.Errorf("%w: impossible terminal counters", ErrInvalidLog)
	}
	// A completed traversal proves one successful initial comments request and
	// each expansion it reports. Failed/incomplete traversals may consume their
	// logical request budget while queued behind a shared limiter without ever
	// reaching Client.Do, so their logical counters are not physical-attempt proof.
	minimumHTTPAttempts := uint64(1 + summary.PostsCompleted)
	for _, outcome := range parsed.outcomes {
		if outcome.Status != "completed" {
			continue
		}
		if !checkedAdd(&minimumHTTPAttempts, uint64(outcome.MoreRequests)) ||
			!checkedAdd(&minimumHTTPAttempts, uint64(outcome.ContinuationRequests)) {
			return fmt.Errorf("%w: request lower bound overflow", ErrInvalidLog)
		}
	}
	if parsed.summary.requests.RedditHTTPAttempts < minimumHTTPAttempts {
		return fmt.Errorf("%w: request attempts do not cover completed traversal", ErrInvalidLog)
	}
	buildDate, _ := time.Parse(buildDateLayout, parsed.started.build.BuildDate)
	if buildDate.After(parsed.started.time) {
		return fmt.Errorf("%w: build date follows run start", ErrInvalidLog)
	}
	return nil
}

type retrySequence struct {
	lastAttempt int
	sessions    int
}

func reconcileRedditRetries(retries []retryRecord, outcomes map[string]OutcomeManifest) error {
	// The producer emits the number of the next HTTP attempt synchronously but does
	// not expose an internal session identifier. Attempt two therefore starts a
	// logical request session and later attempts must be contiguous. Expansion
	// endpoints may reset to attempt two only as often as the reconciled post outcome
	// says that logical endpoint was requested; comments has exactly one session.
	sequences := make(map[string]retrySequence)
	for _, retry := range retries {
		key := retry.operation + "\x00" + retry.postID
		sequence := sequences[key]
		if retry.attempt == 2 {
			sequence.sessions++
		} else if sequence.lastAttempt == 0 || retry.attempt != sequence.lastAttempt+1 {
			return errors.New("reddit retry attempt sequence is impossible")
		}
		sequence.lastAttempt = retry.attempt
		sequences[key] = sequence

		if retry.operation == "oauth_token" {
			continue
		}
		outcome, exists := outcomes[retry.postID]
		if !exists {
			return errors.New("reddit retry does not belong to an assignment outcome")
		}
		maximumSessions := 1
		switch retry.operation {
		case "continuation":
			maximumSessions = outcome.ContinuationRequests
		case "morechildren":
			maximumSessions = outcome.MoreRequests
		}
		if sequence.sessions > maximumSessions {
			return errors.New("reddit retry sessions exceed the post outcome request count")
		}
	}
	return nil
}

func mustMarshalResult(words []resultWord) []byte {
	payload, err := marshalPretty(words)
	if err != nil {
		return nil
	}
	return payload
}

func checkedAdd(total *uint64, value uint64) bool {
	if *total > ^uint64(0)-value {
		return false
	}
	*total += value
	return true
}

func absoluteDurationDifference(left, right time.Duration) time.Duration {
	if left < right {
		return right - left
	}
	return left - right
}

func inspectBinary(parent context.Context, path string) (string, BuildManifest, error) {
	resolvedPath, resolveErr := filepath.Abs(path)
	if resolveErr != nil {
		return "", BuildManifest{}, fmt.Errorf("%w: resolve binary", ErrInvalidBinary)
	}
	binaryHash, before, err := digestRegularFile(resolvedPath, maximumBinaryBytes)
	if err != nil {
		return "", BuildManifest{}, fmt.Errorf("%w: inspect regular binary: %v", ErrInvalidBinary, err)
	}
	if before.Mode()&0o111 == 0 {
		return "", BuildManifest{}, fmt.Errorf("%w: binary is not executable", ErrInvalidBinary)
	}
	ctx, cancel := context.WithTimeout(parent, binaryInspectionWindow)
	defer cancel()
	command := exec.CommandContext(ctx, resolvedPath, "--version")
	// CommandContext kills the direct child at deadline, but descendants may inherit
	// its stdout/stderr pipes and otherwise keep Cmd.Wait blocked indefinitely. Bound
	// that post-exit drain window as part of the binary-inspection contract.
	command.WaitDelay = binaryInspectionDrain
	// Version inspection needs no ambient environment. In particular, a direct
	// finalizer invocation must not expose unrelated credentials to the child even
	// when it did not come through the hardened capture wrapper.
	command.Env = []string{}
	var output limitedBuffer
	output.limit = maximumVersionBytes
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Run(); err != nil || ctx.Err() != nil || output.overflow {
		return "", BuildManifest{}, fmt.Errorf("%w: version inspection failed", ErrInvalidBinary)
	}
	build, err := parseVersionLine(output.String())
	if err != nil {
		return "", BuildManifest{}, err
	}
	after, statErr := os.Lstat(resolvedPath)
	if statErr != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return "", BuildManifest{}, fmt.Errorf("%w: binary changed during inspection", ErrInvalidBinary)
	}
	return binaryHash, build, nil
}

// digestRegularFile hashes a bounded executable as a stream. Release binaries are
// evidence inputs, not application payloads, so retaining their complete contents
// in memory would create needless peak allocation during finalization.
func digestRegularFile(path string, limit int64) (string, os.FileInfo, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return "", nil, err
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() < 0 || before.Size() > limit {
		return "", nil, errors.New("expected a bounded regular file, not a symlink")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", nil, err
	}
	hash := sha256.New()
	written, copyErr := io.Copy(hash, io.LimitReader(file, limit+1))
	after, statErr := file.Stat()
	closeErr := file.Close()
	if copyErr != nil || statErr != nil || closeErr != nil {
		return "", nil, errors.New("hash regular file")
	}
	if written > limit || !os.SameFile(before, after) || after.Size() != written || !after.ModTime().Equal(before.ModTime()) {
		return "", nil, errors.New("file changed or exceeded its byte limit")
	}
	return hex.EncodeToString(hash.Sum(nil)), before, nil
}

type limitedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
}

func (buffer *limitedBuffer) Write(data []byte) (int, error) {
	original := len(data)
	remaining := buffer.limit - buffer.buffer.Len()
	if remaining < len(data) {
		buffer.overflow = true
		data = data[:max(remaining, 0)]
	}
	_, _ = buffer.buffer.Write(data)
	return original, nil
}

func (buffer *limitedBuffer) String() string { return buffer.buffer.String() }

func parseVersionLine(value string) (BuildManifest, error) {
	if !strings.HasSuffix(value, "\n") || strings.Count(value, "\n") != 1 {
		return BuildManifest{}, fmt.Errorf("%w: non-canonical --version output", ErrInvalidBinary)
	}
	fields := strings.Fields(strings.TrimSuffix(value, "\n"))
	if (len(fields) != 5 && len(fields) != 7) || fields[0] != "duckwords" {
		return BuildManifest{}, fmt.Errorf("%w: non-canonical --version output", ErrInvalidBinary)
	}
	values := make(map[string]string, 5)
	for _, field := range fields[1:] {
		parts := strings.SplitN(field, "=", 2)
		if len(parts) != 2 || parts[1] == "" {
			return BuildManifest{}, fmt.Errorf("%w: malformed --version field", ErrInvalidBinary)
		}
		values[parts[0]] = parts[1]
	}
	expectedValues := 4
	if len(fields) == 7 {
		expectedValues = 6
	}
	if len(values) != expectedValues || values["version"] == "" || values["commit"] == "" || values["built"] == "" || values["go"] == "" {
		return BuildManifest{}, fmt.Errorf("%w: incomplete --version identity", ErrInvalidBinary)
	}
	if values["goos"] == "" || values["goarch"] == "" {
		// Current primary CLI reports four values. Platform is reconciled from the
		// executed binary process and the log until the version line grows fields.
		values["goos"] = runtime.GOOS
		values["goarch"] = runtime.GOARCH
	}
	canonical := fmt.Sprintf("duckwords version=%s commit=%s built=%s go=%s\n",
		values["version"], values["commit"], values["built"], values["go"])
	if len(fields) == 7 {
		canonical = fmt.Sprintf("duckwords version=%s commit=%s built=%s go=%s goos=%s goarch=%s\n",
			values["version"], values["commit"], values["built"], values["go"], values["goos"], values["goarch"])
	}
	if value != canonical {
		return BuildManifest{}, fmt.Errorf("%w: non-canonical --version output", ErrInvalidBinary)
	}
	record := logRecord{
		"app_version": json.RawMessage(strconv.Quote(values["version"])), "app_commit": json.RawMessage(strconv.Quote(values["commit"])),
		"app_build_date": json.RawMessage(strconv.Quote(values["built"])), "go_version": json.RawMessage(strconv.Quote(values["go"])),
		"goos": json.RawMessage(strconv.Quote(values["goos"])), "goarch": json.RawMessage(strconv.Quote(values["goarch"])),
	}
	build, err := parseBuild(record)
	if err != nil {
		return BuildManifest{}, fmt.Errorf("%w: %v", ErrInvalidBinary, err)
	}
	return build, nil
}

func publish(ctx context.Context, outputDir string, files map[string][]byte) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: finalization canceled", ErrPublish)
	}
	if _, err := os.Lstat(outputDir); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: output directory already exists or cannot be inspected", ErrPublish)
	}
	parent := filepath.Dir(outputDir)
	parentInfo, err := os.Lstat(parent)
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: parent must be an existing real directory", ErrPublish)
	}
	parentDirectory, err := os.Open(parent)
	if err != nil {
		return fmt.Errorf("%w: open publication parent", ErrPublish)
	}
	defer func() { _ = parentDirectory.Close() }()
	openedParentInfo, err := parentDirectory.Stat()
	if err != nil || !os.SameFile(parentInfo, openedParentInfo) {
		return fmt.Errorf("%w: publication parent changed while opening", ErrPublish)
	}
	stage, err := os.MkdirTemp(parent, ".duckwords-evidence-")
	if err != nil {
		return fmt.Errorf("%w: create staging directory", ErrPublish)
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(stage)
		}
	}()
	createdStageInfo, err := os.Lstat(stage)
	if err != nil || !createdStageInfo.IsDir() || createdStageInfo.Mode()&os.ModeSymlink != 0 ||
		createdStageInfo.Mode().Perm() != stagingPermission {
		return fmt.Errorf("%w: inspect staging directory", ErrPublish)
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%w: finalization canceled", ErrPublish)
		}
		if filepath.Base(name) != name || name == "." {
			return fmt.Errorf("%w: unsafe artifact name", ErrPublish)
		}
		if err := writeFileExclusive(filepath.Join(stage, name), files[name]); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: finalization canceled", ErrPublish)
	}
	// MkdirTemp creates mode 0700. Keep artifacts private throughout assembly and
	// expose the reviewed bundle only at the final commit boundary.
	if err := os.Chmod(stage, directoryPermission); err != nil {
		return fmt.Errorf("%w: set staging permissions", ErrPublish)
	}
	if err := syncDirectory(stage); err != nil {
		return fmt.Errorf("%w: sync staging directory", ErrPublish)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: finalization canceled", ErrPublish)
	}
	currentParentInfo, parentErr := os.Lstat(parent)
	if parentErr != nil || !os.SameFile(parentInfo, currentParentInfo) || currentParentInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: publication parent changed before commit", ErrPublish)
	}
	// Recheck immediately before the commit to narrow the destination creation race.
	// Go's standard library does not expose a portable rename-no-replace primitive:
	// on POSIX, another process can still create an empty directory in the remaining
	// window and Rename may replace that empty directory. A non-empty reviewed bundle
	// cannot be replaced this way, and Windows Rename refuses an existing destination.
	if _, err := os.Lstat(outputDir); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: output directory appeared before commit", ErrPublish)
	}
	stageInfo, err := os.Lstat(stage)
	if err != nil || !os.SameFile(createdStageInfo, stageInfo) || !stageInfo.IsDir() || stageInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: staging directory changed before commit", ErrPublish)
	}
	if err := os.Rename(stage, outputDir); err != nil {
		return fmt.Errorf("%w: atomically publish directory", ErrPublish)
	}
	committed = true
	publishedInfo, err := os.Lstat(outputDir)
	if err != nil || !os.SameFile(stageInfo, publishedInfo) || !publishedInfo.IsDir() || publishedInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: published directory identity changed", ErrPublish)
	}
	if err := syncOpenDirectory(parentDirectory); err != nil {
		// Rename is already the commit point. Returning an error is deliberate: the
		// directory exists, but crash durability of its parent entry was not confirmed.
		return fmt.Errorf("%w: bundle published but parent directory sync failed", ErrPublish)
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := syncOpenDirectory(directory)
	closeErr := directory.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func syncOpenDirectory(directory *os.File) error {
	// Windows does not provide Go's directory handles with a portable equivalent of
	// POSIX fsync. The rename is still atomic there and refuses existing targets; the
	// explicit directory durability barrier is applied on the supported Unix hosts.
	if runtime.GOOS == "windows" {
		return nil
	}
	return directory.Sync()
}

func writeFileExclusive(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePermission)
	if err != nil {
		return fmt.Errorf("%w: create artifact", ErrPublish)
	}
	if err := file.Chmod(filePermission); err != nil {
		_ = file.Close()
		return fmt.Errorf("%w: set artifact permissions", ErrPublish)
	}
	written, writeErr := file.Write(data)
	if writeErr == nil && written != len(data) {
		writeErr = io.ErrShortWrite
	}
	if syncErr := file.Sync(); writeErr == nil {
		writeErr = syncErr
	}
	if closeErr := file.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		return fmt.Errorf("%w: write artifact", ErrPublish)
	}
	return nil
}

func renderRunDocument(manifest Manifest) []byte {
	status := "complete"
	if manifest.Partial {
		status = "partial (exit 3)"
	}
	text := fmt.Sprintf(`# DuckWords submission run

This directory is an atomically published, sanitized evidence bundle for one %s run.

- Command: %s
- Status: %s
- Started (UTC): %s
- Finished (UTC): %s
- Release: %s
- Commit: %s
- Built: %s
- Toolchain: %s (%s/%s)
- Reddit policy checked: %s
- Approval reference: %s
- Posts: %d total, %d complete, %d skipped, %d failed, %d incomplete
- Result: %d ranked words; SHA-256 %s

`+"`result.json`"+` is the exact canonical stdout document. `+"`application.log`"+` is the exact
captured JSON operational log. `+"`full-application.log`"+` appends the exact result after a
fixed marker. `+"`run-manifest.json`"+` contains reconciled counters, provenance, and hashes.
Credentials, OAuth tokens, URLs, local paths, comment bodies, and raw command arguments are
intentionally absent.
`, status, manifest.Command, status, manifest.StartedAt, manifest.FinishedAt, manifest.Build.Version, manifest.Build.Commit,
		manifest.Build.BuildDate, manifest.Build.GoVersion, manifest.Build.GOOS, manifest.Build.GOARCH,
		manifest.Policy.RedditPolicyVerifiedAt, manifest.Policy.ApprovalReference,
		manifest.Summary.PostsTotal, manifest.Summary.PostsCompleted, manifest.Summary.PostsSkipped,
		manifest.Summary.PostsFailed, manifest.Summary.PostsIncomplete, manifest.ResultWords,
		manifest.Artifacts.ResultSHA256)
	return []byte(text)
}

func sameBuild(left, right BuildManifest) bool {
	return left.Version == right.Version && left.Commit == right.Commit && left.BuildDate == right.BuildDate &&
		left.GoVersion == right.GoVersion && left.GOOS == right.GOOS && left.GOARCH == right.GOARCH
}

func digest(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func digestField(record logRecord, key string) (string, error) {
	value, err := stringField(record, key, true)
	if err != nil || !validDigest(value) {
		return "", errors.New("invalid SHA-256")
	}
	return value, nil
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for index := range len(value) {
		char := value[index]
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func validCommit(value string) bool {
	if len(value) != 40 {
		return false
	}
	for index := range len(value) {
		char := value[index]
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func stringField(record logRecord, key string, required bool) (string, error) {
	raw, exists := record[key]
	if !exists {
		if required {
			return "", errors.New("missing string field")
		}
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || value == "" || len(value) > 4<<10 || strings.TrimSpace(value) != value || containsControl([]byte(value), false) {
		return "", errors.New("invalid string field")
	}
	return value, nil
}

func enumField(record logRecord, key string, values ...string) (string, error) {
	value, err := stringField(record, key, true)
	if err != nil || !oneOf(value, values...) {
		return "", errors.New("invalid enum field")
	}
	return value, nil
}

func oneOf(value string, values ...string) bool {
	for _, candidate := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

func intField(record logRecord, key string, minimum, maximum int) (int, error) {
	value, err := int64Field(record, key, int64(minimum), int64(maximum))
	return int(value), err
}

func int64Field(record logRecord, key string, minimum, maximum int64) (int64, error) {
	raw, exists := record[key]
	if !exists {
		return 0, errors.New("missing integer field")
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil || value < minimum || value > maximum {
		return 0, errors.New("invalid integer field")
	}
	return value, nil
}

func uintField(record logRecord, key string) (uint64, error) {
	raw, exists := record[key]
	if !exists {
		return 0, errors.New("missing unsigned integer field")
	}
	var value uint64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, errors.New("invalid unsigned integer field")
	}
	return value, nil
}

func floatField(record logRecord, key string, minimum, maximum float64) (float64, error) {
	raw, exists := record[key]
	if !exists {
		return 0, errors.New("missing float field")
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil || value < minimum || value > maximum {
		return 0, errors.New("invalid float field")
	}
	return value, nil
}

func boolField(record logRecord, key string) (bool, error) {
	raw, exists := record[key]
	if !exists {
		return false, errors.New("missing boolean field")
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, errors.New("invalid boolean field")
	}
	return value, nil
}

// durationField reads a canonical Go duration string such as "20s" or "1m30s". The
// logging sink emits that form in both encodings, so the evidence log carries an
// explicit unit rather than a bare integer whose scale a reader has to assume.
func durationField(record logRecord, key string, minimum, maximum time.Duration) (time.Duration, error) {
	raw, exists := record[key]
	if !exists {
		return 0, errors.New("missing duration field")
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return 0, errors.New("invalid duration field")
	}
	duration, err := time.ParseDuration(text)
	if err != nil {
		return 0, errors.New("invalid duration field")
	}
	// Reject a value that does not round-trip so an unnormalized or padded encoding
	// cannot smuggle a second representation of the same instant into the evidence.
	if duration.String() != text {
		return 0, errors.New("non-canonical duration field")
	}
	if duration < minimum || duration > maximum {
		return 0, errors.New("duration outside limit")
	}
	return duration, nil
}

func timeField(record logRecord) (time.Time, error) {
	value, err := stringField(record, "time", true)
	if err != nil {
		return time.Time{}, err
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	_, offset := parsed.Zone()
	if err != nil || offset != 0 || parsed.Format(time.RFC3339Nano) != value {
		return time.Time{}, errors.New("invalid UTC timestamp")
	}
	return parsed, nil
}

func containsControl(data []byte, permitFormatting bool) bool {
	for _, char := range data {
		if char >= 0x20 && char != 0x7f {
			continue
		}
		if permitFormatting && (char == '\n' || char == '\r' || char == '\t') {
			continue
		}
		return true
	}
	return false
}

func containsUnsafeApprovalMarker(value string) bool {
	lower := strings.ToLower(value)
	for _, marker := range []string{"secret", "token", "password", "authorization", "bearer", "client_id", "client-id"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
