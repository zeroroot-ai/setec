/*
Copyright 2026 The Setec Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package nodeagent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// scriptedRunner records each command invocation and returns canned
// output/error pairs from a queue. Tests describe the exact transcript
// they expect by pushing entries onto the queue before invoking the
// method under test.
type scriptedRunner struct {
	t       *testing.T
	results []scriptedResult
	calls   []scriptedCall
}

type scriptedResult struct {
	out []byte
	err error
}

type scriptedCall struct {
	name string
	args []string
}

func (s *scriptedRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	s.calls = append(s.calls, scriptedCall{name: name, args: args})
	if len(s.results) == 0 {
		s.t.Fatalf("unexpected command: %s %v", name, args)
	}
	res := s.results[0]
	s.results = s.results[1:]
	return res.out, res.err
}

func TestEnsure_PoolAlreadyPresent(t *testing.T) {
	t.Parallel()
	runner := &scriptedRunner{t: t, results: []scriptedResult{
		// dmsetup status returns zero exit → pool is present.
		{out: []byte("0 100 thin-pool tx 0/100 ..."), err: nil},
	}}
	m := &ThinPoolManager{
		Config: Config{
			PoolName:       "setec-thinpool",
			DataDevice:     "/dev/vdb",
			MetadataDevice: "/dev/vdc",
			FillThreshold:  80,
		},
		Runner: runner,
	}
	if err := m.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure() err = %v, want nil", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("expected 1 dmsetup call, got %d", len(runner.calls))
	}
	if runner.calls[0].name != "dmsetup" || runner.calls[0].args[0] != "status" {
		t.Fatalf("unexpected first call: %v", runner.calls[0])
	}
}

func TestEnsure_PoolMissingCreatesIt(t *testing.T) {
	t.Parallel()
	runner := &scriptedRunner{t: t, results: []scriptedResult{
		// dmsetup status: not found.
		{out: []byte("No such device or address"), err: errors.New("exit 1")},
		// blockdev --getsz /dev/vdb returns 2048 sectors.
		{out: []byte("2048\n"), err: nil},
		// dmsetup create succeeds.
		{out: []byte(""), err: nil},
	}}
	m := &ThinPoolManager{
		Config: Config{
			PoolName:       "setec-thinpool",
			DataDevice:     "/dev/vdb",
			MetadataDevice: "/dev/vdc",
			FillThreshold:  80,
		},
		Runner: runner,
	}
	if err := m.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure() err = %v", err)
	}
	// 3 calls: status, blockdev, create.
	if len(runner.calls) != 3 {
		t.Fatalf("want 3 calls, got %d: %v", len(runner.calls), runner.calls)
	}
	if runner.calls[2].name != "dmsetup" || runner.calls[2].args[0] != "create" {
		t.Fatalf("expected dmsetup create, got %v", runner.calls[2])
	}
}

func TestEnsure_StatusBrokenReturnsError(t *testing.T) {
	t.Parallel()
	runner := &scriptedRunner{t: t, results: []scriptedResult{
		// dmsetup status: ambiguous failure → Ensure must propagate.
		{out: []byte("device-mapper: internal error"), err: errors.New("exit 1")},
	}}
	m := &ThinPoolManager{
		Config: Config{PoolName: "setec-thinpool", DataDevice: "/dev/vdb", MetadataDevice: "/dev/vdc"},
		Runner: runner,
	}
	if err := m.Ensure(context.Background()); err == nil {
		t.Fatal("expected error for broken dmsetup status")
	}
}

func TestEnsure_BlockdevFailure(t *testing.T) {
	t.Parallel()
	runner := &scriptedRunner{t: t, results: []scriptedResult{
		{out: []byte("No such device or address"), err: errors.New("exit 1")},
		// blockdev fails.
		{out: []byte("permission denied"), err: errors.New("exit 1")},
	}}
	m := &ThinPoolManager{
		Config: Config{PoolName: "setec-thinpool", DataDevice: "/dev/vdb", MetadataDevice: "/dev/vdc"},
		Runner: runner,
	}
	if err := m.Ensure(context.Background()); err == nil {
		t.Fatal("expected error on blockdev failure")
	}
}

func TestEnsure_CreateFailure(t *testing.T) {
	t.Parallel()
	runner := &scriptedRunner{t: t, results: []scriptedResult{
		{out: []byte("No such device or address"), err: errors.New("exit 1")},
		{out: []byte("2048\n"), err: nil},
		{out: []byte("dmsetup: create failed"), err: errors.New("exit 1")},
	}}
	m := &ThinPoolManager{
		Config: Config{PoolName: "setec-thinpool", DataDevice: "/dev/vdb", MetadataDevice: "/dev/vdc"},
		Runner: runner,
	}
	if err := m.Ensure(context.Background()); err == nil {
		t.Fatal("expected error on dmsetup create failure")
	}
}

func TestEnsure_MissingPoolName(t *testing.T) {
	t.Parallel()
	m := &ThinPoolManager{Config: Config{}}
	if err := m.Ensure(context.Background()); err == nil {
		t.Fatal("expected error when pool name is empty")
	}
}

func TestSample_Parses(t *testing.T) {
	t.Parallel()
	runner := &scriptedRunner{t: t, results: []scriptedResult{
		{out: []byte("0 10000 thin-pool txid 5000/10000 tx discard rw"), err: nil},
	}}
	m := &ThinPoolManager{
		Config: Config{PoolName: "setec-thinpool", FillThreshold: 80},
		Runner: runner,
	}
	s, err := m.Sample(context.Background())
	if err != nil {
		t.Fatalf("Sample(): %v", err)
	}
	if s.Used != 5000 || s.Total != 10000 {
		t.Fatalf("Sample.Used=%d Total=%d, want 5000/10000", s.Used, s.Total)
	}
	if s.FillPercent != 50 {
		t.Fatalf("FillPercent = %d, want 50", s.FillPercent)
	}
	if s.Degraded {
		t.Fatal("Degraded = true, want false (50 < 80)")
	}
}

func TestSample_Degraded(t *testing.T) {
	t.Parallel()
	runner := &scriptedRunner{t: t, results: []scriptedResult{
		{out: []byte("0 100 thin-pool tx 90/100 ..."), err: nil},
	}}
	m := &ThinPoolManager{
		Config: Config{PoolName: "setec-thinpool", FillThreshold: 80},
		Runner: runner,
	}
	s, err := m.Sample(context.Background())
	if err != nil {
		t.Fatalf("Sample(): %v", err)
	}
	if !s.Degraded {
		t.Fatal("expected Degraded=true for 90% fill")
	}
}

func TestParseThinPoolStatus_Malformed(t *testing.T) {
	t.Parallel()

	t.Run("too few fields", func(t *testing.T) {
		_, err := parseThinPoolStatus("0 100 thin-pool", 80)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("non-numeric fraction", func(t *testing.T) {
		_, err := parseThinPoolStatus("0 100 thin-pool tx notnum/100 ...", 80)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("no fraction at all", func(t *testing.T) {
		_, err := parseThinPoolStatus("0 100 thin-pool tx all integers only", 80)
		if err == nil || !strings.Contains(err.Error(), "fraction") {
			t.Fatalf("expected fraction error, got %v", err)
		}
	})
}

func TestSample_MissingPoolName(t *testing.T) {
	t.Parallel()
	m := &ThinPoolManager{Config: Config{}}
	if _, err := m.Sample(context.Background()); err == nil {
		t.Fatal("expected error when pool name is empty")
	}
}

func TestNewThinPoolManager_DefaultRunner(t *testing.T) {
	t.Parallel()
	m := NewThinPoolManager(Config{PoolName: "p"})
	if m.Runner == nil {
		t.Fatal("NewThinPoolManager should install a default runner")
	}
}
