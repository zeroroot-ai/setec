// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package launcher

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// CheckCPUTemplate makes sure that the CPU template at path is in the
// launcher image. When it is not, the error names the template and lists
// the templates that the image holds, so a class with a wrong or new
// instance family fails at once and in plain words (setec#239).
func CheckCPUTemplate(path string) error {
	info, err := os.Stat(path)
	if err == nil && info.Mode().IsRegular() {
		return nil
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("cpu template %q: %w", path, err)
	}
	name := strings.TrimSuffix(filepath.Base(path), ".json")
	entries, rerr := os.ReadDir(filepath.Dir(path))
	if rerr != nil {
		return fmt.Errorf("cpu template %q is not in the launcher image, and the template directory cannot be read: %w", name, rerr)
	}
	var have []string
	for _, e := range entries {
		if n, ok := strings.CutSuffix(e.Name(), ".json"); ok {
			have = append(have, n)
		}
	}
	sort.Strings(have)
	return fmt.Errorf("cpu template %q is not in the launcher image (templates: %s)", name, strings.Join(have, ", "))
}
