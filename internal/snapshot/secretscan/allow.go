// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Zero Root AI

package secretscan

import (
	"fmt"
	"regexp"
	"strings"
)

// Allow states that a pattern in the matched text is known to be safe. An
// operator sets it for an image whose memory holds text that looks like a
// secret and is not one, for example a test key that a program of the image
// carries. A finding is dropped when its Rule is the one of the Allow (an
// empty Rule means each rule) and the matched text, with its secret body, matches
// Pattern. The matched text is never printed.
type Allow struct {
	// Rule is the rule that the Allow covers, or empty for each rule.
	Rule string
	// Pattern is matched against the text of the finding.
	Pattern *regexp.Regexp
}

// ParseAllow reads one allow entry in the form "rule=pattern" or just
// "pattern" for each rule. The rule must be one that the scanner has, so a
// misspelled rule fails and does not allow nothing in silence. The pattern must
// compile as a Go regular expression, and it must not be empty.
func ParseAllow(entry string) (Allow, error) {
	rule, pattern := "", entry
	if i := strings.Index(entry, "="); i > 0 && ruleShaped.MatchString(entry[:i]) {
		if !RuleExists(entry[:i]) {
			return Allow{}, fmt.Errorf("secret scan allow %q: %q is not a rule (rules: %s)",
				entry, entry[:i], strings.Join(RuleNames(), ", "))
		}
		rule, pattern = entry[:i], entry[i+1:]
	}
	if pattern == "" {
		return Allow{}, fmt.Errorf("secret scan allow %q: the pattern is empty", entry)
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return Allow{}, fmt.Errorf("secret scan allow %q: %w", entry, err)
	}
	return Allow{Rule: rule, Pattern: re}, nil
}

// RuleExists reports whether name is the name of a builtin rule.
func RuleExists(name string) bool {
	for _, r := range builtinRules {
		if r.name == name {
			return true
		}
	}
	return false
}

// RuleNames lists the builtin rules, for a message.
func RuleNames() []string {
	out := make([]string, 0, len(builtinRules))
	for _, r := range builtinRules {
		out = append(out, r.name)
	}
	return out
}

func (a Allow) allows(rule string, text []byte) bool {
	return (a.Rule == "" || a.Rule == rule) && a.Pattern.Match(text)
}

// identifierTable reports whether a match of an assignment rule is a run of
// identifiers from a string table of a program, and not a secret. A Go or C
// binary keeps the names of its fields side by side, so its memory holds
// "SecretKeyRef:" followed by the next name, or "SecretName:%s>". The signs are
// all of these: the name is a CamelCase identifier with no separator,
// the colon is not followed by a space, and the value is a format string or
// letters only. A secret with such a value has no space after its colon in a
// config file, and it is a snake_case or a mixed name; these keep a hit.
func identifierTable(window []byte, m []int, nameGroup, valueGroup int) bool {
	if nameGroup == 0 || valueGroup == 0 || m[2*nameGroup] < 0 || m[2*valueGroup] < 0 {
		return false
	}
	name := string(window[m[2*nameGroup]:m[2*nameGroup+1]])
	value := string(window[m[2*valueGroup]:m[2*valueGroup+1]])
	between := window[m[2*nameGroup+1]:m[2*valueGroup]]
	if string(between) != ":" || !camelIdentifier.MatchString(name) {
		return false
	}
	return strings.HasPrefix(value, "%") || lettersOnly.MatchString(value)
}

// ruleShaped is the form of a rule name: lower case words with dashes.
var ruleShaped = regexp.MustCompile(`^[a-z]+(?:-[a-z0-9]+)+$`)

var (
	camelIdentifier = regexp.MustCompile(`^[A-Z][a-z0-9]+(?:[A-Z][a-z0-9]*)+$`)
	lettersOnly     = regexp.MustCompile(`^[A-Za-z]+$`)
)
