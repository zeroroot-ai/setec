// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package grpcserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/zeroroot-ai/setec/internal/nodeagent/launchersandbox"
	"github.com/zeroroot-ai/setec/internal/podspec"
)

// launcherPaths makes the work volume of one launcher Pod.
func launcherPaths(t *testing.T) launchersandbox.Paths {
	t.Helper()
	pods := t.TempDir()
	const uid = "0a1b2c3d-0000-4000-8000-0000000000aa"
	r := launchersandbox.Resolver{PodsDir: pods}
	work, err := r.WorkDir(uid)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(work, "vm"), 0o700); err != nil {
		t.Fatal(err)
	}
	p, err := r.Resolve(t.Context(), uid)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// framed returns a framed snapshot stream of state and memory.
func framed(t *testing.T, state, memory string) *os.File {
	t.Helper()
	dir := t.TempDir()
	sp, mp, dp := filepath.Join(dir, "s"), filepath.Join(dir, "m"), filepath.Join(dir, "d")
	_ = os.WriteFile(sp, []byte(state), 0o600)
	_ = os.WriteFile(mp, []byte(memory), 0o600)
	_ = os.WriteFile(dp, []byte("DISK"), 0o600)
	rc, err := makeLauncherFramedReader("", sp, mp, dp)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "framed")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.ReadFrom(rc); err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	_, _ = f.Seek(0, 0)
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// fakeLauncher plays the launcher: it waits for the staged marker, checks
// the files, and writes ev.
func fakeLauncher(t *testing.T, p launchersandbox.Paths, ev podspec.RestoreEvidence) <-chan string {
	t.Helper()
	sawState := make(chan string, 1)
	go func() {
		for range 200 {
			if _, err := os.Stat(launchersandbox.HostPath(p, podspec.LauncherRestoreStaged)); err == nil {
				b, _ := os.ReadFile(launchersandbox.HostPath(p, podspec.LauncherRestoreState))
				sawState <- string(b)
				raw, _ := json.Marshal(ev)
				tmp := launchersandbox.HostPath(p, podspec.LauncherRestoreEvidence) + ".tmp"
				_ = os.WriteFile(tmp, raw, 0o600)
				_ = os.Rename(tmp, launchersandbox.HostPath(p, podspec.LauncherRestoreEvidence))
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	return sawState
}

func TestRestoreLauncher_StagesTheFilesAndReturnsTheEvidence(t *testing.T) {
	t.Parallel()
	p := launcherPaths(t)
	saw := fakeLauncher(t, p, podspec.RestoreEvidence{EntropyReseeded: true, Uniquified: true, ClockSet: true})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	resp, err := (&Server{}).restoreLauncher(ctx, p, framed(t, "STATE", "MEMORY"), memBackend{encrypted: true}, 0)
	if err != nil || !resp.GetSuccess() || !resp.GetEntropyReseeded() || !resp.GetUniquified() || !resp.GetEncryptedAtRest() {
		t.Fatalf("restoreLauncher = %+v, %v", resp, err)
	}
	if got := <-saw; got != "STATE" {
		t.Fatalf("the launcher read state %q", got)
	}
	if disk, _ := os.ReadFile(launchersandbox.HostPath(p, podspec.LauncherWritableDisk)); string(disk) != "DISK" {
		t.Fatalf("the writable layer = %q, want the one of the snapshot", disk)
	}
	for _, f := range []string{podspec.LauncherRestoreState, podspec.LauncherRestoreMemory} {
		if _, err := os.Stat(launchersandbox.HostPath(p, f)); !os.IsNotExist(err) {
			t.Fatalf("%s stays on the node after the restore", f)
		}
	}
}

func TestRestoreLauncher_FailsClosedOnMissingEvidence(t *testing.T) {
	t.Parallel()
	p := launcherPaths(t)
	fakeLauncher(t, p, podspec.RestoreEvidence{EntropyReseeded: true, ClockSet: true})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	resp, err := (&Server{}).restoreLauncher(ctx, p, framed(t, "S", "M"), memBackend{encrypted: true}, 0)
	if err == nil || resp.GetSuccess() {
		t.Fatalf("a restore with no identity confirmation succeeded: %+v", resp)
	}

	q := launcherPaths(t)
	ctx2, cancel2 := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel2()
	if resp, err := (&Server{}).restoreLauncher(ctx2, q, framed(t, "S", "M"), memBackend{encrypted: true}, 0); err == nil || resp.GetSuccess() {
		t.Fatalf("a restore with no launcher succeeded: %+v", resp)
	}
}

// TestRestoreLauncher_ReseedModeReachesTheLauncher proves both modes of
// --entropy-reseed. Under require, the marker asks for a reseed and a guest
// with no reseed fails closed. Under off, the marker tells the launcher, and
// the response reports no reseed, so the operator gate refuses the restore.
func TestRestoreLauncher_ReseedModeReachesTheLauncher(t *testing.T) {
	t.Parallel()
	noReseed := podspec.RestoreEvidence{Uniquified: true, ClockSet: true}

	p := launcherPaths(t)
	fakeLauncher(t, p, noReseed)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require := &Server{}
	resp, err := require.restoreLauncher(ctx, p, framed(t, "S", "M"), memBackend{encrypted: true}, 0)
	if err == nil || resp.GetSuccess() {
		t.Fatalf("require mode accepted a guest with no reseed: %+v", resp)
	}
	if m, _ := os.ReadFile(launchersandbox.HostPath(p, podspec.LauncherRestoreStaged)); len(m) != 0 {
		t.Fatalf("require mode wrote the marker %q, want an empty marker", m)
	}

	q := launcherPaths(t)
	fakeLauncher(t, q, noReseed)
	resp, err = (&Server{EntropyReseedOff: true}).restoreLauncher(ctx, q, framed(t, "S", "M"), memBackend{encrypted: true}, 0)
	if err != nil || !resp.GetSuccess() || resp.GetEntropyReseeded() || !resp.GetUniquified() {
		t.Fatalf("off mode = %+v, %v; want a success that reports no reseed", resp, err)
	}
	if m, _ := os.ReadFile(launchersandbox.HostPath(q, podspec.LauncherRestoreStaged)); string(m) != podspec.LauncherStagedNoReseed {
		t.Fatalf("off mode wrote the marker %q, want %q", m, podspec.LauncherStagedNoReseed)
	}
}

// memBackend is a storage backend in memory for the launcher tests.
type memBackend struct {
	encrypted bool
	blobs     map[string][]byte
}

func (m memBackend) Save(_ context.Context, id string, r io.Reader) (int64, string, error) {
	b, err := io.ReadAll(r)
	m.blobs[id] = b
	return int64(len(b)), id, err
}
func (m memBackend) Open(_ context.Context, ref string) (io.ReadCloser, error) {
	b, ok := m.blobs[ref]
	if !ok {
		return nil, errors.New("no such snapshot")
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (m memBackend) Delete(context.Context, string) error { return nil }
func (m memBackend) Stat(_ context.Context, ref string) (int64, bool, error) {
	return int64(len(m.blobs[ref])), true, nil
}
func (m memBackend) EncryptedAtRest() bool { return m.encrypted }

// TestWriteIdentityGeneration_ReplacesTheValueWhole proves that the
// launcher reads either the old or the new generation, never a part.
func TestWriteIdentityGeneration_ReplacesTheValueWhole(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "identity-generation")
	for _, gen := range []int64{2, 13} {
		if err := writeIdentityGeneration(path, gen); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(path)
		if err != nil || string(raw) != strconv.FormatInt(gen, 10)+"\n" {
			t.Fatalf("generation file = %q, %v", raw, err)
		}
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("the temporary file stays")
	}
}
