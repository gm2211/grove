package dispatch

import (
	"encoding/json"
	"fmt"
	"time"
)

// Duration is JobRequest.Timeout's wire type. Nobody should have to send raw nanoseconds (Go's
// default time.Duration JSON encoding) to submit a job — a caller that didn't know that convention
// once sent "timeout": 120000 meaning milliseconds, which decoded as 120000 nanoseconds (120µs),
// rounded down to "0" seconds of meta, and the job was killed instantly. So the wire format is
// deliberately NOT time.Duration's raw int64: UnmarshalJSON accepts either
//
//   - a JSON string, parsed by time.ParseDuration ("30m", "2h", "90s"), or
//   - a JSON number (integer or float), interpreted as SECONDS — never nanoseconds, however large
//     the number looks,
//
// and rejects anything else (bool, object, array) with a clear error. MarshalJSON always emits the
// string form (e.g. "2m0s") so a round-tripped Job.request.timeout is human-readable too.
//
// Use Duration() to get the underlying time.Duration for arithmetic/comparisons.
type Duration time.Duration

// Duration returns the underlying time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// String matches time.Duration's format ("2m0s", "1h30m0s", "0s").
func (d Duration) String() string { return time.Duration(d).String() }

// MarshalJSON always emits the duration-string form, e.g. "2m0s".
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON accepts a duration string ("30m", "2h", "90s") or a JSON number interpreted as
// seconds. See the Duration doc comment for why numbers are never nanoseconds.
func (d *Duration) UnmarshalJSON(data []byte) error {
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("dispatch: timeout: %w", err)
	}
	switch v := raw.(type) {
	case nil:
		*d = 0
		return nil
	case string:
		if v == "" {
			*d = 0
			return nil
		}
		parsed, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("dispatch: timeout %q is not a valid duration (send a string like \"30m\" or \"2h\", or a number of seconds): %w", v, err)
		}
		*d = Duration(parsed)
		return nil
	case float64:
		*d = Duration(v * float64(time.Second))
		return nil
	default:
		return fmt.Errorf("dispatch: timeout must be a duration string (e.g. \"30m\") or a number of seconds, got %T", raw)
	}
}
