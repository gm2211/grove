package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gm2211/grove/internal/server"
)

func TestBuildSnoozeRequest(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.Local)
	if _, err := buildSnoozeRequest(snoozeFlags{forDur: "1h", until: "18:00"}, now); err == nil {
		t.Error("--for and --until together accepted")
	}
	if _, err := buildSnoozeRequest(snoozeFlags{forDur: "soon"}, now); err == nil {
		t.Error("bad --for accepted")
	}
	if _, err := buildSnoozeRequest(snoozeFlags{until: "25:99"}, now); err == nil {
		t.Error("bad --until accepted")
	}
	req, err := buildSnoozeRequest(snoozeFlags{forDur: "90m", now: true, note: "zoom"}, now)
	if err != nil || req.For != "1h30m0s" || !req.Now || req.Note != "zoom" || req.Until != "" {
		t.Fatalf("req = %+v %v", req, err)
	}
	// Later today.
	req, _ = buildSnoozeRequest(snoozeFlags{until: "18:00"}, now)
	got, _ := time.Parse(time.RFC3339, req.Until)
	if want := time.Date(2026, 11, 1, 18, 0, 0, 0, time.Local); !got.Equal(want) {
		t.Errorf("until = %v, want %v", got, want)
	}
	// Already past: tomorrow.
	req, _ = buildSnoozeRequest(snoozeFlags{until: "09:30"}, now)
	got, _ = time.Parse(time.RFC3339, req.Until)
	if want := time.Date(2026, 11, 2, 9, 30, 0, 0, time.Local); !got.Equal(want) {
		t.Errorf("past until = %v, want %v", got, want)
	}
}

type fakeLookup struct {
	scopes  []string
	workers []string
	whoErr  error
}

func (f fakeLookup) WhoAmI(context.Context) (*server.Principal, error) {
	if f.whoErr != nil {
		return nil, f.whoErr
	}
	return &server.Principal{Scopes: f.scopes}, nil
}
func (f fakeLookup) Fleet(context.Context) (*server.FleetResponse, error) {
	r := &server.FleetResponse{}
	for _, w := range f.workers {
		r.Workers = append(r.Workers, server.FleetEntry{Name: w})
	}
	return r, nil
}

func TestDefaultWorkerResolution(t *testing.T) {
	ctx := context.Background()
	host := func() (string, error) { return "G-MacBook-M5.fritz.box", nil }
	none := func() (string, bool) { return "", false }

	// 1. the installer's identity wins
	if got, _ := defaultWorker(ctx, fakeLookup{scopes: []string{"worker:snooze:other"}}, func() (string, bool) { return "from-identity", true }, host); got != "from-identity" {
		t.Errorf("identity: %q", got)
	}
	// 2. a single snooze scope
	if got, _ := defaultWorker(ctx, fakeLookup{scopes: []string{"read", "worker:snooze:g-macbook-m5.local"}}, none, host); got != "g-macbook-m5.local" {
		t.Errorf("scope: %q", got)
	}
	// two scopes are ambiguous: fall through to the host name
	if got, _ := defaultWorker(ctx, fakeLookup{scopes: []string{"worker:snooze:a", "worker:snooze:b"}, workers: []string{"g-macbook-m5.local", "mini"}}, none, host); got != "g-macbook-m5.local" {
		t.Errorf("ambiguous scopes: %q", got)
	}
	// 3. host name matched against the fleet
	if got, err := defaultWorker(ctx, fakeLookup{whoErr: errors.New("x"), workers: []string{"mini.local", "G-MacBook-M5.local"}}, none, host); err != nil || got != "G-MacBook-M5.local" {
		t.Errorf("hostname: %q %v", got, err)
	}
	if _, err := defaultWorker(ctx, fakeLookup{workers: []string{"mini"}}, none, host); err == nil {
		t.Error("resolved with no match")
	}
}

func TestSnoozeAPIError403PrintsTheGrant(t *testing.T) {
	err := snoozeAPIError(errors.New("grove api: POST /workers/w/snooze: 403 Forbidden: forbidden"), "g-macbook-m5.local")
	if !strings.Contains(err.Error(), "grove access issue --name g-macbook-m5.local-owner --scope read --scope worker:snooze:g-macbook-m5.local --expires-in 180d") {
		t.Errorf("err = %v", err)
	}
	other := errors.New("500")
	if snoozeAPIError(other, "w") != other {
		t.Error("non-403 wrapped")
	}
}

func TestHostLabel(t *testing.T) {
	for in, want := range map[string]string{"G-MacBook-M5.local": "g-macbook-m5", "Giulio's Mac.": "giulio-s-mac", "mini": "mini"} {
		if got := hostLabel(in); got != want {
			t.Errorf("hostLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
