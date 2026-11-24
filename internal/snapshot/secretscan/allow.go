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

// idName is the name of a field or a type in program text: one word that
// starts with a capital ("Password", "SecretName") or a lower case word with a
// capital inside ("apiToken"). It holds no separator. A name of an environment
// variable or of a config key ("DB_PASSWORD", "client_secret", "password") is
// not one.
var idName = regexp.MustCompile(`^(?:[A-Z][a-z0-9]*(?:[A-Z][a-z0-9]*)*|[a-z][a-z0-9]*(?:[A-Z][a-z0-9]*)+)$`)

// identifierSegment cuts a run into the parts of an identifier: a word, a
// word with a capital, a run of capitals, or a number.
var identifierSegment = regexp.MustCompile(`[A-Z]?[a-z]+|[A-Z]+(?:[a-z]|$)?|[0-9]+`)

// identifierLike reports whether v reads as names put side by side
// ("integerunsignedstring", "Uint64Int32"), and not as a random token. Most
// of its characters lie in words of three or more letters or in numbers of one or two digits. A random token has short runs.
func identifierLike(v string) bool {
	long := 0
	for _, seg := range identifierSegment.FindAllString(v, -1) {
		digits := seg[0] >= '0' && seg[0] <= '9'
		if digits && len(seg) <= 2 || !digits && len(seg) >= 3 {
			long += len(seg)
		}
	}
	return long*10 >= len(v)*7
}

// secretLikeValue reports whether v has the content of a secret: a known
// token format, or a run of at least 12 characters that is not made of names.
func secretLikeValue(v string) bool {
	for _, p := range secretPrefixes {
		if strings.HasPrefix(v, p) {
			return true
		}
	}
	return len(v) >= 12 && !identifierLike(v)
}

// printablePrefix is the leading run of v in printable ASCII.
func printablePrefix(v string) string {
	for i := 0; i < len(v); i++ {
		if v[i] < 0x21 || v[i] > 0x7e {
			return v[:i]
		}
	}
	return v
}

// notAnAssignment reports whether a match of an assignment rule is names next
// to names, as the string tables, the type descriptors and the reflection data
// of a program in memory hold them ("SecretKeyRef:integerunsignedstring"), and
// not a secret. All of these must hold:
//
//   - the name is an identifier of program text (idName), and
//   - the colon follows the name at once, and the value follows the colon at
//     once, or after one quote, or, when colonOnly is not set, after spaces or
//     tabs, and
//   - the printable part of the value has no secret content (secretLikeValue),
//     or it starts with a format verb.
//
// A name with a separator or in one case ("DB_PASSWORD", "client_secret",
// "password"), an equals sign, a quoted name ("apiToken":"..."), a value after
// a space in the quoted form, and a value with a known token format or with
// random content stay findings. The rule does not decide on the bytes that
// follow the value: a value that ends at a non-printable byte is judged on its
// printable part. The limit: a secret that is a few words, in letters only, in
// a compact literal such as Config{ClientSecret:"correcthorsebattery"}, reads
// as names and is not a finding.
func notAnAssignment(window []byte, m []int, rl rule) bool {
	// Both rules that carry the guard hold the name in group 1 and the value in
	// group 2.
	const g, v = 1, 2
	if m[2*g] < 0 || m[2*v] < 0 {
		return false
	}
	name := string(window[m[2*g]:m[2*g+1]])
	between := string(window[m[2*g+1]:m[2*v]])
	// A quote after the colon is the start of the value in text. With no space
	// before it, it is as likely the start of the next string of a table.
	between = strings.TrimSuffix(strings.TrimSuffix(between, `"`), "'")
	colon := between == ":" || !rl.colonOnly && strings.HasPrefix(between, ":") && strings.Trim(between[1:], " \t") == ""
	if !colon {
		return false
	}
	// A qualified name ("SecretVolumeSource.ClientSecret") is judged by its last part.
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		name = name[i+1:]
	}
	if !idName.MatchString(name) {
		return false
	}
	value := printablePrefix(string(window[m[2*v]:m[2*v+1]]))
	// A format verb starts a format string of a program ("%s>/etc/passwd").
	if strings.HasPrefix(value, "%") {
		return true
	}
	return !secretLikeValue(value)
}

// ruleShaped is the form of a rule name: lower case words with dashes.
var ruleShaped = regexp.MustCompile(`^[a-z]+(?:-[a-z0-9]+)+$`)
