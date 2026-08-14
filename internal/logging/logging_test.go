package logging

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestNewWritesStructuredJSONWithUTCTime(t *testing.T) {
	t.Parallel()

	fixedTime := time.Date(2026, time.August, 13, 13, 14, 15, 0, time.FixedZone("EEST", 3*60*60))
	var output strings.Builder
	sink, err := New(&output, Options{
		Level:  LevelInfo,
		Format: FormatJSON,
		ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
			if attr.Key == slog.TimeKey {
				attr.Value = slog.TimeValue(fixedTime)
			}
			return attr
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	sink.Logger().Info("run started", EventAttr(EventRunStarted), slog.String("mode", "strict"))
	if err := sink.Err(); err != nil {
		t.Fatalf("Sink.Err() = %v", err)
	}

	var record map[string]any
	if err := json.Unmarshal([]byte(output.String()), &record); err != nil {
		t.Fatalf("decode log %q: %v", output.String(), err)
	}
	if record[slog.TimeKey] != "2026-08-13T10:14:15Z" {
		t.Fatalf("time = %v, want UTC timestamp", record[slog.TimeKey])
	}
	if record[slog.LevelKey] != "INFO" || record[slog.MessageKey] != "run started" {
		t.Fatalf("standard attributes = %#v", record)
	}
	if record[KeyEvent] != string(EventRunStarted) || record["mode"] != "strict" {
		t.Fatalf("structured attributes = %#v", record)
	}
}

func TestNewWritesStructuredText(t *testing.T) {
	t.Parallel()

	var output strings.Builder
	sink, err := New(&output, Options{Format: FormatText})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	sink.Logger().Info("source loaded", EventAttr(EventSourceLoaded), slog.String(KeySourceKind, "file"))

	for _, want := range []string{"level=INFO", `msg="source loaded"`, "event=source_loaded", "source_kind=file"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("text log %q does not contain %q", output.String(), want)
		}
	}
}

func TestNewFiltersBelowConfiguredLevel(t *testing.T) {
	t.Parallel()

	var output strings.Builder
	sink, err := New(&output, Options{Level: LevelWarn, Format: FormatJSON})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	sink.Logger().Debug("debug-planted")
	sink.Logger().Info("info-planted")
	sink.Logger().Warn("visible-warning")

	if strings.Contains(output.String(), "planted") {
		t.Fatalf("filtered record was written: %q", output.String())
	}
	if !strings.Contains(output.String(), "visible-warning") {
		t.Fatalf("warning was filtered: %q", output.String())
	}
}

func TestNewRedactsSensitiveKeys(t *testing.T) {
	t.Parallel()

	secrets := []string{
		"planted-client-secret",
		"planted-bearer-token",
		"planted-raw-error",
		"planted-comment-body",
		"planted-user-agent",
		"planted-error-as-any",
	}
	var output strings.Builder
	sink, err := New(&output, Options{Format: FormatJSON})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	sink.Logger().Info(
		"safe lifecycle message",
		slog.String("oauth_client_secret", secrets[0]),
		slog.String("Authorization", secrets[1]),
		slog.String("raw_error", secrets[2]),
		slog.Group("payload", slog.String("comment_body", secrets[3])),
		slog.String("user_agent", secrets[4]),
		slog.Any("failure_value", errors.New(secrets[5])),
		slog.String("status", "complete"),
	)

	for _, secret := range secrets {
		if strings.Contains(output.String(), secret) {
			t.Fatalf("log exposed planted secret %q: %s", secret, output.String())
		}
	}
	if got := strings.Count(output.String(), redactedValue); got != len(secrets) {
		t.Fatalf("redaction count = %d, want %d in %s", got, len(secrets), output.String())
	}
	if !strings.Contains(output.String(), `"status":"complete"`) {
		t.Fatalf("safe attribute was not preserved: %s", output.String())
	}
}

func TestNewRedactsValuesInsideSensitiveGroups(t *testing.T) {
	t.Parallel()

	const secret = "planted-group-secret"
	var output strings.Builder
	sink, err := New(&output, Options{Format: FormatJSON})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	sink.Logger().Info("safe", slog.Group("authorization", slog.String("value", secret)))
	if strings.Contains(output.String(), secret) || !strings.Contains(output.String(), redactedValue) {
		t.Fatalf("sensitive group was not redacted: %s", output.String())
	}
}

func TestMandatoryRedactionRunsAfterCustomReplacement(t *testing.T) {
	t.Parallel()

	const planted = "planted-secret-from-hook"
	var output strings.Builder
	sink, err := New(&output, Options{
		Format: FormatJSON,
		ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
			if attr.Key == "client_secret" {
				attr.Key = "status"
				attr.Value = slog.StringValue(planted)
			}
			return attr
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	sink.Logger().Info("safe", slog.String("client_secret", "original-secret"))
	if strings.Contains(output.String(), planted) || strings.Contains(output.String(), "original-secret") {
		t.Fatalf("custom replacement bypassed redaction: %s", output.String())
	}
}

func TestSinkRecordsFirstWriterErrorAcrossDerivedLoggers(t *testing.T) {
	t.Parallel()

	first := errors.New("first writer failure")
	second := errors.New("second writer failure")
	writer := &sequenceErrorWriter{errors: []error{first, second}}
	sink, err := New(writer, Options{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	sink.Logger().With(slog.String("component", "test")).Info("first")
	sink.Logger().WithGroup("nested").Error("second")
	if err := sink.Err(); !errors.Is(err, first) {
		t.Fatalf("Sink.Err() = %v, want first writer error", err)
	}
}

func TestNewRejectsInvalidOptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		writer  io.Writer
		options Options
		wantErr error
	}{
		{name: "nil writer", wantErr: ErrNilWriter},
		{name: "invalid level", writer: &strings.Builder{}, options: Options{Level: "trace"}, wantErr: ErrInvalidLevel},
		{name: "invalid format", writer: &strings.Builder{}, options: Options{Format: "yaml"}, wantErr: ErrInvalidFormat},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := New(test.writer, test.options)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("New() error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestParseLevelAndFormat(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"debug", "info", "warn", "error"} {
		if level, err := ParseLevel(value); err != nil || string(level) != value {
			t.Fatalf("ParseLevel(%q) = %q, %v", value, level, err)
		}
	}
	if _, err := ParseLevel("INFO"); !errors.Is(err, ErrInvalidLevel) {
		t.Fatalf("ParseLevel() error = %v, want ErrInvalidLevel", err)
	}

	for _, value := range []string{"text", "json"} {
		if format, err := ParseFormat(value); err != nil || string(format) != value {
			t.Fatalf("ParseFormat(%q) = %q, %v", value, format, err)
		}
	}
	if _, err := ParseFormat("xml"); !errors.Is(err, ErrInvalidFormat) {
		t.Fatalf("ParseFormat() error = %v, want ErrInvalidFormat", err)
	}
}

func TestErrorClassAttrRejectsRawText(t *testing.T) {
	t.Parallel()

	if got := ErrorClassAttr("transport_timeout").Value.String(); got != "transport_timeout" {
		t.Fatalf("safe error class = %q", got)
	}
	if got := ErrorClassAttr("request failed: planted detail").Value.String(); got != "unknown" {
		t.Fatalf("unsafe error class = %q, want unknown", got)
	}
}

func TestEventAttrRejectsRawText(t *testing.T) {
	t.Parallel()

	if got := EventAttr(EventRunSummary).Value.String(); got != string(EventRunSummary) {
		t.Fatalf("safe event = %q", got)
	}
	if got := EventAttr("planted event with spaces").Value.String(); got != "unknown" {
		t.Fatalf("unsafe event = %q, want unknown", got)
	}
}

type sequenceErrorWriter struct {
	errors []error
	index  int
}

func (writer *sequenceErrorWriter) Write([]byte) (int, error) {
	if writer.index >= len(writer.errors) {
		return 0, writer.errors[len(writer.errors)-1]
	}
	err := writer.errors[writer.index]
	writer.index++
	return 0, err
}
