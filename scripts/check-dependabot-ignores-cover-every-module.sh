#!/usr/bin/env bash
#
# check-dependabot-ignores-cover-every-module.sh — an advisory-driven Dependabot
# ignore must cover every module that requires the dependency directly.
#
# WHY
#
# GO-2026-6443 affects google.golang.org/grpc in a range that REOPENS above a
# fixed one: 0–1.82.2, then 1.83.0–1.83.2, then 1.84.0-dev onward. So v1.84.0 is
# newer than the clean v1.83.2 and is affected again, and Dependabot's "bump to
# the latest" walks back into the advisory.
#
# The ignore for it was added to ONE of the four gomod entries. The root module
# kept being offered v1.84.0, `direct-require-advisories` kept refusing it, and
# the whole 17-package group PR sat blocked by one member (setec#125).
#
# A per-module ignore that covers some modules is the same shape as a guard
# keyed to one call site: correct where it was written, silent everywhere else.
#
# WHAT IT ASSERTS
#
# For every `ignore` entry under a gomod update: every OTHER gomod directory
# whose go.mod lists that dependency in a `require` block, NOT marked
# `// indirect`, carries the same entry.
#
# Indirect requires are excluded on purpose, and for the same reason
# check-direct-requires-have-no-advisory.sh excludes them: Dependabot does not
# offer a version bump for a dependency the module does not name.
#
# Usage:
#   check-dependabot-ignores-cover-every-module.sh            run the check
#   check-dependabot-ignores-cover-every-module.sh --selftest  prove it can fail
#
# Exit 0 = every ignore covers every module that could be offered the bump.
set -euo pipefail

cd "$(dirname "$0")/.." || exit 1

CONFIG="${CONFIG:-.github/dependabot.yml}"
ROOT="${ROOT:-.}"

if [ "${1:-}" = "--selftest" ]; then
  exec bash scripts/__tests__/check-dependabot-ignores-cover-every-module.test.sh
fi

python3 - "$CONFIG" "$ROOT" <<'PY'
import os, re, sys, yaml

config, root = sys.argv[1], sys.argv[2]

with open(config) as fh:
    doc = yaml.safe_load(fh)

gomod = [u for u in (doc.get('updates') or []) if u.get('package-ecosystem') == 'gomod']
if not gomod:
    print('::error::%s declares no gomod update; this check has nothing to compare '
          'and must not report ok' % config)
    sys.exit(1)

REQUIRE = re.compile(r'^\s+(?P<mod>[^\s]+)\s+v[^\s]+(?P<indirect>\s*//\s*indirect)?\s*$')

def direct_requires(directory):
    """Module paths this go.mod requires directly."""
    path = os.path.join(root, directory.lstrip('/'), 'go.mod')
    if not os.path.exists(path):
        return None
    out, in_block = set(), False
    for line in open(path):
        stripped = line.strip()
        if stripped.startswith('require ('):
            in_block = True
            continue
        if in_block and stripped == ')':
            in_block = False
            continue
        if stripped.startswith('require ') and '(' not in stripped:
            parts = stripped.split()
            if len(parts) >= 3 and '// indirect' not in line:
                out.add(parts[1])
            continue
        if in_block:
            m = REQUIRE.match(line.rstrip('\n'))
            if m and not m.group('indirect'):
                out.add(m.group('mod'))
    return out

requires, missing_mod = {}, []
for u in gomod:
    d = u['directory']
    r = direct_requires(d)
    if r is None:
        missing_mod.append(d)
    else:
        requires[d] = r

rc = 0
if missing_mod:
    rc = 1
    for d in missing_mod:
        print('::error::%s names gomod directory %s, which has no go.mod. The ignore '
              'coverage below cannot be computed for it.' % (config, d))

# ignores[dep] = {directory: sorted(versions)}
ignores = {}
for u in gomod:
    for entry in (u.get('ignore') or []):
        dep = entry.get('dependency-name')
        if not dep:
            continue
        versions = tuple(sorted(str(v) for v in (entry.get('versions') or [])))
        ignores.setdefault(dep, {})[u['directory']] = versions

checked = 0
for dep, byDir in sorted(ignores.items()):
    needs = {d for d, reqs in requires.items() if dep in reqs}
    for d in sorted(needs):
        checked += 1
        if d not in byDir:
            rc = 1
            have = ', '.join(sorted(byDir))
            print('::error::%s requires %s directly but %s has no ignore for it; '
                  '%s does. Dependabot will keep offering the ignored version there.'
                  % (os.path.join(d.lstrip('/') or '.', 'go.mod'), dep, d, have))
        elif len(set(byDir.values())) > 1:
            rc = 1
            print('::error::the ignore for %s lists different versions per module: %s. '
                  'One advisory, one version list.'
                  % (dep, {k: list(v) for k, v in sorted(byDir.items())}))

if rc == 0:
    print('ok  %d gomod module(s), %d ignore(d dependencies), %d (module, ignore) pair(s) verified'
          % (len(requires), len(ignores), checked))
    # A floor. With no ignores there is nothing to verify, and that is a real
    # state — but a run that found no MODULES measured nothing.
    if len(requires) < 2:
        print('::error::only %d module(s) were parsed; go.mod discovery is not working'
              % len(requires))
        rc = 1
sys.exit(rc)
PY
