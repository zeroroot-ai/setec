#!/usr/bin/env bash
#
# Fixture for check-crd-field-consumers.sh.
#
# Every rule gets a case that is RED for the stated reason, not merely non-zero:
# each red case asserts a substring of the message, so a rule that starts failing
# for a different reason is a test failure rather than a pass.
#
# Offline by construction. The analyzer is never run: FIELD_READS supplies a
# crafted output, and TYPES_DIR a crafted schema. That keeps the fixture a test
# of the DECISION, which is the half that holds the rules.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1

GATE=scripts/check-crd-field-consumers.sh
PASS=0 FAIL=0
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT

# A schema with one root object, one spec, one status, and one field that is not
# part of the served schema at all because it carries no json tag.
mkschema() {
  mkdir -p "$tmp/types"
  cat > "$tmp/types/fixture_types.go" <<'GO'
package v1alpha1

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Shown",type=string,JSONPath=`.status.shown`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Widget is the fixture root object.
type Widget struct {
	Spec   WidgetSpec   `json:"spec,omitempty"`
	Status WidgetStatus `json:"status,omitempty"`
}

type WidgetSpec struct {
	Size    string `json:"size,omitempty"`
	Ignored string `json:"ignored,omitempty"`
	Untagged string
}

type WidgetStatus struct {
	Shown string `json:"shown,omitempty"`
}

// +kubebuilder:object:root=true

// WidgetList carries no print columns, and must not be picked as the root.
type WidgetList struct {
	Items []Widget `json:"items"`
}
GO
}

# reads <field> <counts>  -> one analyzer output line
reads() { printf 'field\tv1alpha1.%s\t%s/types/fixture_types.go:10\t%s\n' "$1" "$tmp" "$2"; }

# run <exempt-file-contents> <analyzer-lines>
run() {
  printf '%s\n' "$1" > "$tmp/exempt.txt"
  TYPES_DIR="$tmp/types" EXEMPT_FILE="$tmp/exempt.txt" MIN_SERVED=1 \
    FIELD_READS="$2" bash "$GATE" 2>&1
}

# red <name> <exempt> <lines> <expected substring>
red() {
  local name="$1" out
  out="$(run "$2" "$3")"
  if [ $? -eq 0 ]; then
    FAIL=$((FAIL+1)); echo "  FAIL: $name was allowed"; return
  fi
  case "$out" in
    *"$4"*) PASS=$((PASS+1)) ;;
    *) FAIL=$((FAIL+1)); echo "  FAIL: $name failed for the wrong reason; wanted '$4', got: $out" ;;
  esac
}

# green <name> <exempt> <lines>
green() {
  local name="$1" out
  out="$(run "$2" "$3")"
  if [ $? -ne 0 ]; then
    FAIL=$((FAIL+1)); echo "  FAIL: $name was blocked: $out"
  else PASS=$((PASS+1)); fi
}

mkschema

# 1. THE CASE THIS GATE EXISTS FOR. A served field nobody reads.
red "an unread spec field" "" "$(reads WidgetSpec.Ignored 'reads=0')" \
  "v1alpha1.WidgetSpec.Ignored"

# 2. A write is not a read. setec#126 and SnapshotSpec.SHA256 are both this shape.
red "a field written many times and never read" "" "$(reads WidgetSpec.Ignored 'reads=0 writes=9')" \
  "reads=0 writes=9"

# 3. A read is a consumer.
green "a field with a Go read" "" "$(reads WidgetSpec.Ignored 'reads=3')"

# 4. A print column is a consumer. The field exists to be displayed and no Go
#    code will ever read it.
green "an unread field named by a print column" "" "$(reads WidgetStatus.Shown 'reads=0 writes=4')"

# 5. A field with no json tag is not in the served schema at all, and says so
#    rather than reading as an ordinary review item.
red "an unread field with no json tag" "" "$(reads WidgetSpec.Untagged 'reads=0')" \
  "no json tag"

# 6. Generated code is never a consumer, and never a candidate either: the gate
#    looks only at *_types.go, so a deepcopy line is not a field of the schema.
#    A real line travels with it, or the floor would fire and the case would pass
#    for the wrong reason.
green "an unread field reported from generated code is not judged" "" \
  "$(printf 'field\tv1alpha1.WidgetSpec.Ignored\t%s/types/zz_generated.deepcopy.go:88\treads=0\n' "$tmp")
$(reads WidgetSpec.Size 'reads=2')"

# 7. A recorded verdict silences the finding.
green "a verdict with a reference and a reason" \
  "v1alpha1.WidgetSpec.Ignored | #121 | build the consumer, tracked" \
  "$(reads WidgetSpec.Ignored 'reads=0')"

# 8. A verdict with no issue or ADR reference is an opinion.
red "a verdict with no reference" \
  "v1alpha1.WidgetSpec.Ignored | later | build the consumer" \
  "$(reads WidgetSpec.Ignored 'reads=0')" \
  "has no issue or ADR reference"

# 9. A malformed entry must not silently exempt anything.
red "a verdict with two columns" \
  "v1alpha1.WidgetSpec.Ignored | #121" \
  "$(reads WidgetSpec.Ignored 'reads=0')" \
  'expected "<pkg>.<Type>.<Field>'

# 10. An exemption outliving its field records a decision about nothing.
red "a verdict naming a field that no longer exists" \
  "v1alpha1.WidgetSpec.Removed | #121 | kept after the field was deleted" \
  "$(reads WidgetSpec.Ignored 'reads=3')" \
  "no longer exist"

# 11. THE FLOOR. An analyzer output that reaches nothing must not read as clean.
#     Every guard in this repo that could not fail looked exactly like this.
out="$(TYPES_DIR="$tmp/types" EXEMPT_FILE=/dev/null MIN_SERVED=5 FIELD_READS="$(reads WidgetSpec.Size 'reads=1')" bash "$GATE" 2>&1)"
if [ $? -eq 0 ]; then
  FAIL=$((FAIL+1)); echo "  FAIL: a run that resolved 1 field below a floor of 5 reported ok"
else
  case "$out" in
    *"below the floor"*) PASS=$((PASS+1)) ;;
    *) FAIL=$((FAIL+1)); echo "  FAIL: the floor failed for the wrong reason: $out" ;;
  esac
fi

# 12. The List type must not be chosen as the root. If it were, no JSONPath would
#     resolve and the print-column rule would silently stop crediting anything —
#     a rule that cannot fire, which is the defect class this repo keeps hitting.
out="$(run "" "$(reads WidgetStatus.Shown 'reads=0')")"
case "$out" in
  *"print column(s) resolved"*) PASS=$((PASS+1)) ;;
  *) FAIL=$((FAIL+1)); echo "  FAIL: the ok line does not report resolved print columns: $out" ;;
esac
case "$out" in
  *"1 print column(s) resolved"*) PASS=$((PASS+1)) ;;
  *) FAIL=$((FAIL+1)); echo "  FAIL: expected exactly 1 resolved column (.metadata.* resolves to nothing): $out" ;;
esac

# 13. A schema with print columns and no root object cannot resolve them, and
#     must say so rather than crediting nothing in silence.
mkdir -p "$tmp/noroot"
sed '/object:root=true/d' "$tmp/types/fixture_types.go" > "$tmp/noroot/fixture_types.go"
out="$(TYPES_DIR="$tmp/noroot" EXEMPT_FILE=/dev/null MIN_SERVED=1 FIELD_READS="$(reads WidgetSpec.Size 'reads=1')" bash "$GATE" 2>&1)"
if [ $? -eq 0 ]; then
  FAIL=$((FAIL+1)); echo "  FAIL: a schema with unresolvable print columns reported ok"
else
  case "$out" in
    *"no +kubebuilder:object:root type"*) PASS=$((PASS+1)) ;;
    *) FAIL=$((FAIL+1)); echo "  FAIL: wrong reason for a rootless schema: $out" ;;
  esac
fi

echo
echo "passed=$PASS failed=$FAIL"
# A count floor. Cases appended below a summary block is how six of them once
# silently never ran in .github's fixture suite.
if [ "$PASS" -lt 14 ] && [ "$FAIL" -eq 0 ]; then
  echo "FAIL: only $PASS case(s) ran; cases were added below the summary block" >&2
  exit 1
fi
[ "$FAIL" -eq 0 ]
