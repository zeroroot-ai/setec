// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package secretscan

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// program is a small Go program with the shapes that made the scan refuse a
// warm base (setec#412): types whose fields are named like secrets, reflection
// over them, struct tags, JSON, TLS and HTTP code of the standard library, and a
// dump of its own memory. It holds no assignment of a secret.
const program = `package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strings"
	"text/template"
)

type SecretKeySelector struct {
	Name string ` + "`json:\"name\"`" + `
	Key  string ` + "`json:\"key\"`" + `
}

type EnvVarSource struct {
	ConfigMapRef *SecretKeySelector ` + "`json:\"configMapRef,omitempty\"`" + `
	SecretKeyRef *SecretKeySelector ` + "`json:\"secretKeyRef,omitempty\"`" + `
}

type SecretVolumeSource struct {
	SecretName string ` + "`json:\"secretName\"`" + `
	Password   string ` + "`json:\"password\"`" + `
	Token      string ` + "`json:\"token\"`" + `
	ApiToken   string
	ClientSecret string
}

type TokenRequest struct {
	Audience          string
	ServiceAccountTokenProjection int64
}

func main() {
	vals := []any{EnvVarSource{}, SecretVolumeSource{}, TokenRequest{}, SecretKeySelector{}}
	var names []string
	for _, v := range vals {
		t := reflect.TypeOf(v)
		for i := 0; i < t.NumField(); i++ {
			names = append(names, t.Name()+"."+t.Field(i).Name+":"+string(t.Field(i).Tag))
		}
		b, _ := json.Marshal(v)
		names = append(names, string(b))
	}
	_ = template.Must(template.New("x").Parse("{{.SecretName}} {{.Password}} {{.Token}}"))
	_ = tls.Config{}
	_ = http.Header{}
	if len(os.Args) < 2 {
		fmt.Println(strings.Join(names, "\n"))
		return
	}
	maps, _ := os.ReadFile("/proc/self/maps")
	mem, err := os.Open("/proc/self/mem")
	if err != nil {
		panic(err)
	}
	out, _ := os.Create(os.Args[1])
	for _, line := range strings.Split(string(maps), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || !strings.HasPrefix(f[1], "r") || strings.HasPrefix(line, "ffff") || strings.Contains(line, "[vvar") {
			continue
		}
		var lo, hi uint64
		fmt.Sscanf(f[0], "%x-%x", &lo, &hi)
		buf := make([]byte, min(hi-lo, 8<<20))
		if n, _ := mem.ReadAt(buf, int64(lo)); n > 0 {
			out.Write(buf[:n])
		}
	}
	out.Close()
	fmt.Fprintln(os.Stderr, len(names))
}
`

// realAssignments are assignments of real secrets in the shapes that the scan
// keeps strict.
var realAssignments = []string{
	"DB_PASSWORD=hunter2hunter2",
	"export API_TOKEN=abcdEFGH12345678ijkl",
	"client_secret: \"correcthorsebattery\"",
	"password: correcthorsebattery",
	"SecretKey=Abcdefgh1234567",
	`{"apiToken":"s3cr3tTokenValue9988"}`,
	"AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
	"Token:ghp_abcdefghijklmnopqrstuvwxyz0123456789",
	"SecretKeyRef:3fa9c2d17b8e4a60b5d1e2f3a4b5c6d7",
	"-----BEGIN RSA PRIVATE KEY-----",
}

// TestGoBinaryAndItsMemory builds a Go program, scans its executable and a dump
// of its process image, and requires no finding for the identifiers. It then
// puts each real assignment into the same buffer and requires a finding for
// each (setec#412).
func TestGoBinaryAndItsMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a Go program")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain:", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module probe\n\ngo 1.21\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(program), 0o600); err != nil {
		t.Fatal(err)
	}
	bin, dump := filepath.Join(dir, "probe"), filepath.Join(dir, "mem.bin")
	build := exec.Command(goBin, "build", "-o", bin, ".")
	build.Dir = dir
	build.Env = append(os.Environ(), "GOFLAGS=", "CGO_ENABLED=0")
	if out, berr := build.CombinedOutput(); berr != nil {
		t.Fatalf("build: %v\n%s", berr, out)
	}
	// The process runs with no inherited environment, which may hold a real
	// secret of the machine that runs the test.
	runCmd := exec.Command(bin, dump)
	runCmd.Env = []string{"PATH=/usr/bin"}
	if out, rerr := runCmd.CombinedOutput(); rerr != nil {
		t.Fatalf("run: %v\n%s", rerr, out)
	}
	for name, path := range map[string]string{"executable": bin, "process image": dump} {
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatal(rerr)
		}
		if got := scanText(t, New(), string(raw)); len(got) != 0 {
			t.Errorf("%s: %d findings in identifiers; first: %v", name, len(got), got[:min(len(got), 3)])
		}
		var buf bytes.Buffer
		buf.Write(raw[:len(raw)/2])
		for _, a := range realAssignments {
			buf.WriteString("\n" + a + "\n")
		}
		buf.Write(raw[len(raw)/2:])
		found := scanText(t, New(), buf.String())
		text := buf.String()
		for _, a := range realAssignments {
			at := int64(strings.Index(text, a))
			ok := false
			for _, f := range found {
				if f.Offset >= at-1 && f.Offset <= at+int64(len(a)) {
					ok = true
				}
			}
			if !ok {
				t.Errorf("%s: no finding for the real assignment %q", name, a)
			}
		}
	}
}
