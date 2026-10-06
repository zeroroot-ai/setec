// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package guestagent

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestWithImageDefaults(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, ".setec"), 0o755)
	_ = os.WriteFile(filepath.Join(root, imageConfigPath), []byte(`{"entrypoint":["/usr/local/bin/gibson-runner"],
		"cmd":["-serve"],"env":["PATH=/usr/bin","LANG=C"],"user":"runner:runner","workingDir":"/home/runner"}`), 0o600)

	got := WithImageDefaults(root, Process{Env: []string{"LANG=en_US.UTF-8", "MISSION=7"}})
	want := Process{
		Argv: []string{"/usr/local/bin/gibson-runner", "-serve"},
		Env:  []string{"PATH=/usr/bin", "LANG=en_US.UTF-8", "MISSION=7"},
		User: "runner:runner", Dir: "/home/runner",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	own := WithImageDefaults(root, Process{Argv: []string{"nmap", "-h"}, User: "root", Dir: "/"})
	if own.Argv[0] != "nmap" || own.User != "root" || own.Dir != "/" {
		t.Fatalf("the values of the request must win: %+v", own)
	}
	if p := WithImageDefaults(t.TempDir(), Process{Argv: []string{"x"}}); p.Argv[0] != "x" || p.Env != nil {
		t.Fatalf("no image config must change nothing: %+v", p)
	}
}
