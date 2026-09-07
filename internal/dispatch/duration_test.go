package dispatch

import (
	"encoding/json"
	"testing"
	"time"
)

// TestDuration_UnmarshalJSON covers the wire formats JobRequest.Timeout must accept — a duration
// string, an integer or float number of seconds (never nanoseconds, however large the number
// looks), and rejection of anything else. This guards the actual production defect: a caller that
// sent "timeout": 120000 meaning milliseconds got silently decoded as 120000 nanoseconds under the
// old time.Duration wire format, rounding down to "0" seconds server-side and killing the job
// instantly (exit 124).
func TestDuration_UnmarshalJSON(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    time.Duration
		wantErr bool
	}{
		{name: "duration string minutes", in: `"30m"`, want: 30 * time.Minute},
		{name: "duration string hours", in: `"2h"`, want: 2 * time.Hour},
		{name: "duration string seconds", in: `"90s"`, want: 90 * time.Second},
		{name: "duration string compound", in: `"1h30m"`, want: 90 * time.Minute},
		{name: "integer seconds", in: `120`, want: 120 * time.Second},
		{name: "float seconds", in: `1.5`, want: 1500 * time.Millisecond},
		{
			// The exact shape of the real defect: a caller sends what it thinks is a big,
			// safely-long timeout as a raw number. It must be interpreted as seconds — never
			// nanoseconds — no matter how huge or "ns-looking" it is.
			name: "large ns-looking integer is still seconds",
			in:   `120000`,
			want: 120000 * time.Second,
		},
		{name: "zero", in: `0`, want: 0},
		{name: "empty string", in: `""`, want: 0},
		{name: "null", in: `null`, want: 0},
		{name: "invalid duration string", in: `"soon"`, wantErr: true},
		{name: "bool rejected", in: `true`, wantErr: true},
		{name: "object rejected", in: `{}`, wantErr: true},
		{name: "array rejected", in: `[1,2]`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var d Duration
			err := json.Unmarshal([]byte(tt.in), &d)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Unmarshal(%s): want error, got nil (parsed %s)", tt.in, d.Duration())
				}
				return
			}
			if err != nil {
				t.Fatalf("Unmarshal(%s): unexpected error: %v", tt.in, err)
			}
			if d.Duration() != tt.want {
				t.Fatalf("Unmarshal(%s) = %s, want %s", tt.in, d.Duration(), tt.want)
			}
		})
	}
}

// TestDuration_MarshalJSON asserts the wire format always round-trips through the string form,
// never the raw int64 nanosecond count.
func TestDuration_MarshalJSON(t *testing.T) {
	d := Duration(2 * time.Minute)
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if got, want := string(b), `"2m0s"`; got != want {
		t.Fatalf("Marshal(2m) = %s, want %s", got, want)
	}
}

// TestDuration_RoundTripInJobRequest guards the field wiring itself: a JobRequest decoded from a
// duration string, and one decoded from a number of seconds, must produce the same Timeout.
func TestDuration_RoundTripInJobRequest(t *testing.T) {
	var stringReq, numberReq JobRequest
	if err := json.Unmarshal([]byte(`{"kind":"shell","pool":"linux","script":"true","timeout":"2m"}`), &stringReq); err != nil {
		t.Fatalf("unmarshal string timeout: %v", err)
	}
	if err := json.Unmarshal([]byte(`{"kind":"shell","pool":"linux","script":"true","timeout":120}`), &numberReq); err != nil {
		t.Fatalf("unmarshal number timeout: %v", err)
	}
	if stringReq.Timeout.Duration() != 2*time.Minute {
		t.Fatalf("string timeout: got %s, want 2m", stringReq.Timeout.Duration())
	}
	if numberReq.Timeout.Duration() != 2*time.Minute {
		t.Fatalf("number timeout: got %s, want 2m", numberReq.Timeout.Duration())
	}
}
