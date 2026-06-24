package reaper

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeClient is an in-memory SandboxClient for tests.
type fakeClient struct {
	mu        sync.Mutex
	list      []Sandbox
	listErr   error
	removeErr map[string]error // per-id remove error; nil entry => success
	removed   []string
}

func (f *fakeClient) ListNotReadySandboxes(context.Context) ([]Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]Sandbox(nil), f.list...), nil
}

func (f *fakeClient) ForceRemove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.removeErr != nil {
		if err, ok := f.removeErr[id]; ok && err != nil {
			return err
		}
	}
	f.removed = append(f.removed, id)
	return nil
}

func (f *fakeClient) removedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.removed...)
}

// fixedClock returns a clock pinned to t.
func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func TestReap_RemovesOldNotReadyKataSandboxes(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	fc := &fakeClient{list: []Sandbox{
		{ID: "old-kata-fc", RuntimeHandler: "kata-fc", CreatedAt: now.Add(-10 * time.Minute)},
		{ID: "old-kata-qemu", RuntimeHandler: "kata-qemu", CreatedAt: now.Add(-5 * time.Minute)},
	}}
	r := &OrphanReaper{Client: fc, MinAge: 3 * time.Minute, Clock: fixedClock(now)}

	n, err := r.reap(context.Background())
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if n != 2 {
		t.Fatalf("reaped = %d, want 2", n)
	}
	if got := fc.removedIDs(); len(got) != 2 {
		t.Fatalf("removed = %v, want both kata sandboxes", got)
	}
}

func TestReap_SkipsYoungSandboxes(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	fc := &fakeClient{list: []Sandbox{
		{ID: "young", RuntimeHandler: "kata-fc", CreatedAt: now.Add(-30 * time.Second)},
	}}
	r := &OrphanReaper{Client: fc, MinAge: 3 * time.Minute, Clock: fixedClock(now)}

	n, err := r.reap(context.Background())
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if n != 0 || len(fc.removedIDs()) != 0 {
		t.Fatalf("young sandbox should not be reaped: n=%d removed=%v", n, fc.removedIDs())
	}
}

func TestReap_SkipsNonKataHandlers(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	fc := &fakeClient{list: []Sandbox{
		{ID: "runc-old", RuntimeHandler: "runc", CreatedAt: now.Add(-1 * time.Hour)},
		{ID: "empty-handler", RuntimeHandler: "", CreatedAt: now.Add(-1 * time.Hour)},
		{ID: "gvisor", RuntimeHandler: "runsc", CreatedAt: now.Add(-1 * time.Hour)},
	}}
	r := &OrphanReaper{Client: fc, MinAge: 3 * time.Minute, Clock: fixedClock(now)}

	n, err := r.reap(context.Background())
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if n != 0 || len(fc.removedIDs()) != 0 {
		t.Fatalf("non-kata sandboxes must never be reaped: n=%d removed=%v", n, fc.removedIDs())
	}
}

func TestReap_CustomHandlers(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	fc := &fakeClient{list: []Sandbox{
		{ID: "gvisor", RuntimeHandler: "runsc", CreatedAt: now.Add(-1 * time.Hour)},
		{ID: "kata", RuntimeHandler: "kata-fc", CreatedAt: now.Add(-1 * time.Hour)},
	}}
	r := &OrphanReaper{Client: fc, MinAge: time.Minute, Handlers: []string{"runsc"}, Clock: fixedClock(now)}

	n, err := r.reap(context.Background())
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if n != 1 || len(fc.removedIDs()) != 1 || fc.removedIDs()[0] != "gvisor" {
		t.Fatalf("only runsc should be reaped: n=%d removed=%v", n, fc.removedIDs())
	}
}

func TestReap_ZeroCreatedAtIsEligible(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	fc := &fakeClient{list: []Sandbox{
		{ID: "no-timestamp", RuntimeHandler: "kata-fc"}, // CreatedAt zero
	}}
	r := &OrphanReaper{Client: fc, MinAge: 3 * time.Minute, Clock: fixedClock(now)}

	n, err := r.reap(context.Background())
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if n != 1 {
		t.Fatalf("zero-timestamp sandbox should be eligible: n=%d", n)
	}
}

func TestReap_ListErrorIncrementsMetricAndReturns(t *testing.T) {
	var errCount int
	fc := &fakeClient{listErr: errors.New("containerd down")}
	r := &OrphanReaper{Client: fc, Metrics: Metrics{Errors: func() { errCount++ }}}

	n, err := r.reap(context.Background())
	if err == nil {
		t.Fatal("expected list error")
	}
	if n != 0 {
		t.Fatalf("n = %d, want 0", n)
	}
	if errCount != 1 {
		t.Fatalf("error metric = %d, want 1", errCount)
	}
}

func TestReap_RemoveErrorContinuesAndCountsReaped(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	var reaped, errs int
	fc := &fakeClient{
		list: []Sandbox{
			{ID: "fails", RuntimeHandler: "kata-fc", CreatedAt: now.Add(-10 * time.Minute)},
			{ID: "ok", RuntimeHandler: "kata-fc", CreatedAt: now.Add(-10 * time.Minute)},
		},
		removeErr: map[string]error{"fails": errors.New("rmp timeout")},
	}
	r := &OrphanReaper{
		Client:  fc,
		MinAge:  time.Minute,
		Clock:   fixedClock(now),
		Metrics: Metrics{Reaped: func(string) { reaped++ }, Errors: func() { errs++ }},
	}

	n, err := r.reap(context.Background())
	if err == nil {
		t.Fatal("expected first remove error to be returned")
	}
	if n != 1 {
		t.Fatalf("reaped count = %d, want 1 (the 'ok' sandbox)", n)
	}
	if reaped != 1 || errs != 1 {
		t.Fatalf("metrics: reaped=%d errs=%d, want 1/1", reaped, errs)
	}
	if got := fc.removedIDs(); len(got) != 1 || got[0] != "ok" {
		t.Fatalf("removed = %v, want [ok]", got)
	}
}

func TestReap_ReapedMetricLabelledByHandler(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	handlers := map[string]int{}
	fc := &fakeClient{list: []Sandbox{
		{ID: "a", RuntimeHandler: "kata-fc", CreatedAt: now.Add(-10 * time.Minute)},
		{ID: "b", RuntimeHandler: "kata-qemu", CreatedAt: now.Add(-10 * time.Minute)},
	}}
	r := &OrphanReaper{
		Client:  fc,
		MinAge:  time.Minute,
		Clock:   fixedClock(now),
		Metrics: Metrics{Reaped: func(h string) { handlers[h]++ }},
	}
	if _, err := r.reap(context.Background()); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if handlers["kata-fc"] != 1 || handlers["kata-qemu"] != 1 {
		t.Fatalf("handler metric = %v, want one each", handlers)
	}
}

func TestRun_DisabledWithoutClient(t *testing.T) {
	var logged []string
	r := &OrphanReaper{Logger: func(f string, _ ...any) { logged = append(logged, f) }}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled; Run must return promptly
	r.Run(ctx)
	if len(logged) != 1 {
		t.Fatalf("expected a single 'disabled' log line, got %v", logged)
	}
}

func TestRun_SweepsThenStopsOnContextCancel(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	fc := &fakeClient{list: []Sandbox{
		{ID: "old", RuntimeHandler: "kata-fc", CreatedAt: now.Add(-10 * time.Minute)},
	}}
	r := &OrphanReaper{Client: fc, MinAge: time.Minute, Interval: time.Hour, Clock: fixedClock(now)}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()

	// The immediate up-front sweep should reap the orphan; poll for it.
	deadline := time.After(2 * time.Second)
	for {
		if len(fc.removedIDs()) == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("immediate sweep did not reap within 2s")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}
