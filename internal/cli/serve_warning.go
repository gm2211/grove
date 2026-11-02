package cli

import "time"

// repeatWarningEvery is how often a tick that keeps failing the same way logs it again. Logging
// only changes would leave a failure that started before the log was read looking like a fix.
const repeatWarningEvery = 10 * time.Minute

// repeatedWarning keeps a per-tick failure from flooding the log: it is due when the message
// differs from the last one logged, or when that one was logged repeatEvery ago.
type repeatedWarning struct {
	msg string
	at  time.Time
}

// due reports whether msg should be logged now, and if so records it as logged.
func (w *repeatedWarning) due(msg string, now time.Time) bool {
	if msg == w.msg && now.Sub(w.at) < repeatWarningEvery {
		return false
	}
	w.msg, w.at = msg, now
	return true
}

// clear forgets the last warning, so the next failure is logged at once.
func (w *repeatedWarning) clear() { *w = repeatedWarning{} }
