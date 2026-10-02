#!/usr/bin/env bash
#
# check-crd-field-consumers.sh — a served CRD field must have a consumer.
#
# ADR-0094 layer 6, the setec half (#121; the gibson half is gibson#503).
#
# A spec or status field in a served schema is a promise printed in
# `kubectl explain`. When no controller reads it, the operator writes it, the
# object is admitted, and the cluster does something else. setec#126 is the
# measured instance that prompted this: a SandboxClass kernel and rootfs image
# were accepted and ignored, so the class booted the operator default and
# nothing reported the substitution.
#
# WHAT COUNTS AS A CONSUMER
#
#   1. A Go read of the field, resolved by the type checker.
#   2. A `+kubebuilder:printcolumn` marker whose JSONPath names the field. Such
#      a field exists to be displayed; `kubectl get` is its consumer and no Go
#      code will ever read it.
#
# A WRITE IS NOT A READ. `SnapshotSpec.SHA256` is written on every snapshot and
# read nowhere, which is exactly why the digest is never verified on restore.
# Counting writes would have reported it as live.
#
# GENERATED CODE IS NEVER A CONSUMER. zz_generated.deepcopy.go touches every
# field, so counting it would make every field read as live. The analyzer skips
# generated files by default; this script never passes -generated.
#
# HOW THE READS ARE COUNTED
#
# By `ast-checks/cmd/unwired`, the same analyzer `make lint-unwired` uses, at
# the version go.mod pins. Reimplementing the type-checker half here would give
# this repo two answers to "is this read", which is how a measurement starts
# disagreeing with itself.
#
# WHY THIS EXISTS BESIDE lint-unwired
#
# lint-unwired keeps a whole-repo baseline of 89 tolerated declarations that
# only has to shrink. A CRD field is a published promise rather than an internal
# loose end, so it gets a separate list, a verdict per entry, and a reason that
# has to name where the decision lives.
#
# Usage:
#   check-crd-field-consumers.sh            run the gate
#   check-crd-field-consumers.sh --selftest prove every rule can fail
#
# Exit 0 = every served field has a consumer or a recorded verdict.
# Exit 1 = at least one does not.
set -euo pipefail

cd "$(dirname "$0")/.." || exit 1

TYPES_DIR="${TYPES_DIR:-api/v1alpha1}"
EXEMPT_FILE="${EXEMPT_FILE:-scripts/crd-field-consumers-exempt.txt}"
# MIN_SERVED is the plausibility floor: a run that resolves fewer fields than
# this measured nothing and must not report ok. The three CRDs carry 108 served
# fields today. Only the fixture lowers it, and one fixture case proves the floor
# itself can fail.
MIN_SERVED="${MIN_SERVED:-50}"

# The analyzer version is read from go.mod, never written here. Two pins for one
# tool drift silently; #138 removed the other copy of this mistake.
UNWIRED_VERSION="$(awk '$1=="github.com/zeroroot-ai/ast-checks"{print $2; exit}' go.mod)"
case "$UNWIRED_VERSION" in
  v*) ;;
  *) echo "::error::no ast-checks version in go.mod, so field reads have no pinned measurer" >&2; exit 1 ;;
esac

if [ "${1:-}" = "--selftest" ]; then
  exec bash scripts/__tests__/check-crd-field-consumers.test.sh
fi

# FIELD_READS lets the selftest supply a crafted analyzer output instead of
# running the analyzer. The gate itself never sets it.
if [ -n "${FIELD_READS:-}" ]; then
  reads_out="$FIELD_READS"
else
  reads_out="$(go run "github.com/zeroroot-ai/ast-checks/cmd/unwired@${UNWIRED_VERSION}" \
    -dir . -kinds field -all 2>/dev/null)"
fi

# The analyzer output goes to a file, not down the pipe: python3 - already takes
# its program on stdin, so a heredoc and a pipe cannot both be stdin. The first
# draft of this script did exactly that and read an empty field list, which the
# floor at the bottom caught.
reads_file="$(mktemp)"
trap 'rm -f "$reads_file"' EXIT
printf '%s\n' "$reads_out" > "$reads_file"

python3 - "$TYPES_DIR" "$EXEMPT_FILE" "$reads_file" "$MIN_SERVED" <<'PY'
import os, re, sys

types_dir, exempt_file, reads_file = sys.argv[1], sys.argv[2], sys.argv[3]
min_served = int(sys.argv[4])

# ---------------------------------------------------------------------------
# 1. The served schema: every struct field in the *_types.go files.
# ---------------------------------------------------------------------------
# These files are gofmt'd, so a struct body's fields sit at exactly one tab.
# That is a stronger handle than a general-purpose regex over the file.
FIELD = re.compile(
    r'^\t(?P<name>[A-Z]\w*)\s+(?P<type>[^\s].*?)(?:\s+`(?P<tags>[^`]*)`)?\s*$')
JSONTAG = re.compile(r'json:"(?P<v>[^"]*)"')
PRINTCOL = re.compile(r'\+kubebuilder:printcolumn:.*?JSONPath=[`"](?P<path>[^`"]+)[`"]')
ROOT = '+kubebuilder:object:root=true'

# fields[Type][FieldName] = {"json": name or None, "type": bare type name}
fields = {}
# printcolumns[i] = (root type of the file, JSONPath)
printcolumns = []

def bare(t):
    """Strip pointer, slice and map decoration down to a type name."""
    t = t.strip()
    t = re.sub(r'^\[\]', '', t)
    t = re.sub(r'^map\[[^\]]*\]', '', t)
    return t.lstrip('*').strip()

for fname in sorted(os.listdir(types_dir)):
    if not fname.endswith('_types.go'):
        continue
    lines = open(os.path.join(types_dir, fname)).read().split('\n')

    # Markers are collected PER FILE and attached to the file's root object,
    # rather than tracked by adjacency to the next `type`. A gofmt'd types file
    # puts a blank line between the marker block and the type's doc comment, so
    # adjacency tracking silently lost every marker. Each *_types.go declares one
    # root object plus its List; the List carries no print columns.
    file_roots, file_paths = [], []
    cur = None
    for i, line in enumerate(lines):
        if ROOT in line:
            nxt = next((l for l in lines[i:] if re.match(r'^type \w+ struct \{', l)), None)
            if nxt:
                file_roots.append(re.match(r'^type (\w+) struct \{', nxt).group(1))
        pc = PRINTCOL.search(line)
        if pc:
            file_paths.append(pc.group('path'))

        m = re.match(r'^type (?P<n>\w+) struct \{', line)
        if m:
            cur = m.group('n')
            fields.setdefault(cur, {})
            continue
        if line == '}' or line.startswith('type '):
            cur = None
            continue
        if cur is None:
            continue
        fm = FIELD.match(line)
        if not fm:
            continue
        tags = fm.group('tags') or ''
        jt = JSONTAG.search(tags)
        jname = jt.group('v').split(',')[0] if jt else None
        fields[cur][fm.group('name')] = {
            'json': jname or None, 'type': bare(fm.group('type'))}

    objs = [r for r in file_roots if not r.endswith('List')]
    if file_paths and not objs:
        print('::error::%s declares print columns but no +kubebuilder:object:root type; '
              'the columns cannot be resolved to a field' % fname)
        sys.exit(1)
    for path in file_paths:
        printcolumns.append((objs[0], path))

# ---------------------------------------------------------------------------
# 2. Resolve each print-column JSONPath to the field it displays.
# ---------------------------------------------------------------------------
# A path that leaves this package (.metadata.creationTimestamp) resolves to
# nothing, which is correct: there is no field of ours to credit.
displayed = set()
for root, path in printcolumns:
    cur, ok = root, True
    for seg in [s for s in path.split('.') if s]:
        if cur not in fields:
            ok = False
            break
        hit = next((n for n, f in fields[cur].items() if f['json'] == seg), None)
        if hit is None:
            ok = False
            break
        last, cur = (cur, hit), fields[cur][hit]['type']
    if ok:
        displayed.add('%s.%s' % last)

# ---------------------------------------------------------------------------
# 3. Exemptions. Keyed <pkg>.<Type>.<Field>, reason and reference required.
# ---------------------------------------------------------------------------
exempt = {}
if os.path.exists(exempt_file):
    for n, raw in enumerate(open(exempt_file), 1):
        # A comment is a line that STARTS with #. There are no inline comments,
        # because an issue reference is "#121" and stripping from the first # ate
        # every reference column in the first draft of this file.
        line = raw.strip()
        if not line or line.startswith('#'):
            continue
        parts = [p.strip() for p in line.split('|')]
        if len(parts) != 3 or not all(parts):
            print('::error::%s:%d: expected "<pkg>.<Type>.<Field> | <reference> | <reason>"'
                  % (exempt_file, n))
            sys.exit(1)
        key, ref, reason = parts
        if not re.search(r'(#\d+|ADR-\d+|https?://)', ref):
            print('::error::%s:%d: %s has no issue or ADR reference; a verdict with no '
                  'place to read it is an opinion' % (exempt_file, n, key))
            sys.exit(1)
        exempt[key] = (ref, reason)

# ---------------------------------------------------------------------------
# 4. Judge.
# ---------------------------------------------------------------------------
rc = 0
served, unread, stale, satisfied = set(), [], [], []
for line in open(reads_file):
    p = line.rstrip('\n').split('\t')
    if len(p) < 4 or p[0] != 'field':
        continue
    key, loc, counts = p[1], p[2], ' '.join(p[3:])
    path = loc.split(':')[0]
    # normpath on both sides: the analyzer reports a repo-relative path and the
    # fixture an absolute one, and a .strip('/') here ate the leading slash of
    # the second, which made every fixture case resolve zero fields.
    if os.path.normpath(os.path.dirname(path)) != os.path.normpath(types_dir) \
            or not path.endswith('_types.go'):
        continue
    served.add(key)
    reads = int(re.search(r'reads=(\d+)', counts).group(1))
    if reads:
        continue
    short = key.split('.', 1)[1] if '.' in key else key
    if short in displayed:
        continue
    unread.append((key, loc, counts, short))

# Two ways an entry rots, and both read as a live decision:
#   * the field no longer exists, so the verdict is about nothing;
#   * the field now HAS a consumer, so the verdict is not only unnecessary but
#     would mask the consumer being removed again later.
unread_keys = {u[0] for u in unread}
for key in exempt:
    if key not in served:
        stale.append(key)
    elif key not in unread_keys:
        satisfied.append(key)

if stale:
    rc = 1
    print('::error::%s names %d field(s) that no longer exist:' % (exempt_file, len(stale)))
    for k in sorted(stale):
        print('::error::  %s' % k)
    print('::error::Delete the entry. An exemption outliving its field records a '
          'decision about nothing.')

if satisfied:
    rc = 1
    print('::error::%s records a verdict for %d field(s) that now HAVE a consumer:'
          % (exempt_file, len(satisfied)))
    for k in sorted(satisfied):
        print('::error::  %s' % k)
    print('::error::Delete the entry. The promise is kept, and an exemption left in '
          'place would hide the consumer being removed again.')

blocking = [u for u in unread if u[0] not in exempt]
if blocking:
    rc = 1
    print('')
    print('BLOCKED: %d served CRD field(s) with no consumer.' % len(blocking))
    print('')
    for key, loc, counts, short in sorted(blocking):
        typ, fld = short.rsplit('.', 1)
        tagged = fields.get(typ, {}).get(fld, {}).get('json')
        kind = 'review' if tagged else 'FAILURE: no json tag, so it is not in the served schema at all'
        print('  %-52s %s  %s' % (key, counts, loc))
        print('       %s' % kind)
    print('')
    print('Each one is a promise kubectl explain prints and the cluster does not keep.')
    print('ADR-0094 rule 5: build the consumer, or delete the producer. Never default')
    print('to deletion. A print column counts as a consumer when the field exists only')
    print('to be displayed.')
    print('')
    print('To record a verdict instead, add a line to %s:' % exempt_file)
    print('')
    print('    <pkg>.<Type>.<Field> | <#issue or ADR> | <why this is the right state>')
    print('')

if rc == 0:
    print('ok  %d served field(s) checked, %d print column(s) resolved, '
          '%d verdict(s) recorded' % (len(served), len(displayed), len(exempt)))
    # A floor. A walk that resolves nothing would otherwise read as a clean run,
    # which is the shape of every guard in this repo that could not fail.
    if len(served) < min_served:
        print('::error::only %d served field(s) were found, below the floor of %d; the '
              'analyzer output is not reaching %s' % (len(served), min_served, types_dir))
        rc = 1
sys.exit(rc)
PY
