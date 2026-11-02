// Package conformance holds tests that pin Grove's Go code to the formal models in spec/: each
// test replays, against the real packages, a scenario a model checker or proof reasons about, so
// a model that drifts from the code (or code that drifts from the model) fails here. See
// spec/README.md for the property each test backs.
package conformance
