package reddit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"unicode/utf8"
)

const maxPreflightFuzzBytes = 64 << 10

var errExtraJSONValueForTest = errors.New("extra JSON value")

type observedPreflightStats struct {
	things    int
	comments  int
	moreIDs   int
	bodyBytes int64
}

func TestPreflightJSONRecognizedAccountingParity(t *testing.T) {
	t.Parallel()

	limits := preflightLimits{
		maxThings:         64,
		maxComments:       64,
		maxMoreIDs:        64,
		maxBodyBytes:      1 << 10,
		maxTotalBodyBytes: 4 << 10,
	}
	tests := []struct {
		name    string
		payload string
		want    observedPreflightStats
		wantErr error
	}{
		{
			name: "escaped protocol keys and kind",
			payload: `{"child\u0072en":[` +
				`{"k\u0069nd":"t\u0031","d\u0061ta":{"b\u006fdy":"\u0061\uD83E\uDD86"}},` +
				`{"kind":"more","data":{"children":["c1","c2"]}}]}`,
			want: observedPreflightStats{things: 2, comments: 1, moreIDs: 2, bodyBytes: 5},
		},
		{
			name: "duplicate recognized arrays count every decoded occurrence",
			payload: `{"children":[{"kind":"t1","data":{"body":"a"}}],` +
				`"children":[{"kind":"t1","data":{"body":"bb"}}]}`,
			want: observedPreflightStats{things: 2, comments: 2, bodyBytes: 3},
		},
		{
			name: "duplicate kind uses final value",
			payload: `{"children":[` +
				`{"kind":"t1","kind":"more","data":{"body":"ignored"}},` +
				`{"kind":"more","kind":"t1","data":{"body":"x"}}]}`,
			want: observedPreflightStats{things: 2, comments: 1, bodyBytes: 1},
		},
		{
			name: "duplicate data uses final value",
			payload: `{"children":[` +
				`{"kind":"t1","data":{"body":"first"},"data":{"body":"x"}},` +
				`{"kind":"t1","data":{"body":"x"},"data":null}]}`,
			want: observedPreflightStats{things: 2, comments: 2, bodyBytes: 1},
		},
		{
			name: "duplicate body uses final value",
			payload: `{"children":[` +
				`{"kind":"t1","data":{"body":"long","body":"x"}},` +
				`{"kind":"t1","data":{"body":"x","body":null}}]}`,
			want: observedPreflightStats{things: 2, comments: 2, bodyBytes: 1},
		},
		{
			name: "nested recognized thing arrays",
			payload: `{"things":[` +
				`{"kind":"t1","data":{"body":"a","children":[{"kind":"t1","data":{"body":"bb"}}]}},` +
				`{"kind":"more","data":{"children":["c1"]}}]}`,
			want: observedPreflightStats{things: 3, comments: 2, moreIDs: 1, bodyBytes: 3},
		},
		{
			name:    "ordinary nested arrays are not things",
			payload: `{"other":[{"kind":"t1","data":{"body":"ignored"}}],"children":[]}`,
			want:    observedPreflightStats{},
		},
		{
			name: "mixed children representation fails closed",
			payload: `{"children":[` +
				`{"kind":"t1","data":{"body":"x"}},"c1"]}`,
			wantErr: errMalformedResponse,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			payload := []byte(test.payload)
			if err := decodeExactStandardJSON(payload); err != nil {
				t.Fatalf("test payload is not exact standard JSON: %v", err)
			}

			got, err := observePreflightStats(payload, limits)
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("preflight error = %v, want %v", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("preflight error = %v", err)
			}
			if got != test.want {
				t.Fatalf("preflight stats = %+v, want %+v", got, test.want)
			}
		})
	}
}

// FuzzPreflightJSONMatchesEncodingJSON proves the scanner's soundness boundary:
// every byte sequence it accepts must also be one exact JSON value according to the
// standard decoder. The reverse does not hold by design because preflight applies
// stricter nesting, field, auxiliary-array, and Reddit wire-shape limits.
func FuzzPreflightJSONMatchesEncodingJSON(f *testing.F) {
	f.Add([]byte(`null`))
	f.Add([]byte(`{"value":1}`))
	f.Add([]byte(`{"children":[{"kind":"t1","data":{"body":"duck"}}]}`))
	f.Add([]byte(`{"child\u0072en":[{"k\u0069nd":"t\u0031","d\u0061ta":{"b\u006fdy":"\uD83E\uDD86"}}]}`))
	f.Add([]byte(`{"children":[],"children":[{"kind":"t1","data":{"body":"x"}}]}`))
	f.Add([]byte(`{"children":[{"kind":"t1"},"c1"]}`))
	f.Add([]byte(`{"value":1} {"extra":2}`))
	f.Add([]byte{'{', '"', 0xff, '"', ':', '0', '}'})

	f.Fuzz(func(t *testing.T, payload []byte) {
		if len(payload) > maxPreflightFuzzBytes {
			return
		}
		err := preflightJSON(
			context.Background(),
			payload,
			absoluteMaxThings,
			absoluteMaxComments,
			absoluteMaxMoreIDs,
			absoluteMaxBodyBytes,
			absoluteMaxTotalBodyBytes,
		)
		if err != nil {
			return
		}
		if err := decodeExactStandardJSON(payload); err != nil {
			t.Fatalf("preflight accepted non-standard or non-exact JSON (%d bytes): %v", len(payload), err)
		}
	})
}

func observePreflightStats(payload []byte, limits preflightLimits) (observedPreflightStats, error) {
	if len(payload) == 0 || !utf8.Valid(payload) {
		return observedPreflightStats{}, errMalformedResponse
	}
	scanner := preflightScanner{
		ctx:     context.Background(),
		payload: payload,
		limits:  limits,
		stack:   make([]preflightFrame, 0, 64),
	}
	err := scanner.scan()
	return observedPreflightStats{
		things:    scanner.things,
		comments:  scanner.comments,
		moreIDs:   scanner.moreIDs,
		bodyBytes: scanner.bodyBytes,
	}, err
}

func decodeExactStandardJSON(payload []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errExtraJSONValueForTest
		}
		return err
	}
	return nil
}
