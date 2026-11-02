#!/usr/bin/env bash
# Runs every model check, proof and query under spec/ and compares each result with what
# spec/README.md says it is. Exits non-zero if any result differs, in either direction: a property
# that should hold fails, or a "before" instance that should still show its bug stops failing (so
# the model no longer finds the bug it was written for).
#
# Needs: quint (npm @informalsystems/quint), TLA2TOOLS_JAR (tla2tools.jar), Java, lake (Lean
# 4.35.0) and go. CODEQL, when set, also runs the S10 CodeQL queries; SPECIFY, when set (the
# gm2211/specify CLI), lints spec/grove.spec and runs its linked formal checks.
set -euo pipefail
cd "$(dirname "$0")"
spec=$PWD
QUINT=${QUINT:-quint}
: "${TLA2TOOLS_JAR:?set TLA2TOOLS_JAR to tla2tools.jar}"
export TLA2TOOLS_JAR

fail=0
row() { printf '%-4s %-22s %-34s %-6s %s\n' "$@"; }
row ID MODEL PROPERTY WANT GOT

# check <id> <file> <main module> invariant|temporal <property> holds|fails
check() {
	local id=$1 file=$2 main=$3 kind=$4 prop=$5 want=$6 out got
	out=$(cd "$spec/quint" && "$QUINT" verify "$file" --main="$main" --backend=tlc --"$kind"="$prop" 2>&1) || true
	if grep -q '\[ok\]' <<<"$out"; then
		got=holds
	elif grep -q '\[violation\]' <<<"$out"; then
		got=fails
	else
		got=error
		echo "$out" >&2
	fi
	row "$id" "$main" "$prop" "$want" "$got"
	[ "$got" = "$want" ] || fail=1
}

check S1  snooze.qnt   snoozeAll                invariant snoozedMeansPaused           holds
check S1  snooze.qnt   snoozeAllBefore          invariant snoozedMeansPaused           fails
check L1  snooze.qnt   snoozeResume             temporal  eventuallyWorkingAfterResume holds
check L1  snooze.qnt   snoozeResumeBefore       temporal  eventuallyWorkingAfterResume fails
check L2  snooze.qnt   snoozeAll                temporal  eventuallyConsistent         holds
check L2  snooze.qnt   snoozeAllBefore          temporal  eventuallyConsistent         fails
check L2  snooze.qnt   snoozeExpiryFailsBefore  temporal  eventuallyConsistent         fails
check S1  snooze.qnt   snoozeMBT                invariant snoozedMeansPaused           holds
check L2  snooze.qnt   snoozeMBT                temporal  eventuallyConsistent         holds
check L2  snooze.qnt   snoozeMBTFresh           temporal  eventuallyConsistent         holds
check L3  dispatch.qnt dispatchFixed            temporal  terminates                   holds
check L4  dispatch.qnt dispatchFixed            invariant neverFailedForSnooze         holds
check L4  dispatch.qnt dispatchFixed            invariant neverLost                    holds
check L4  dispatch.qnt dispatchFixed            temporal  runsAfterSnooze              holds
check L4  dispatch.qnt dispatchBuggy            invariant neverFailedForSnooze         fails
check L4  dispatch.qnt dispatchBuggy            temporal  runsAfterSnooze              fails
check L4  dispatch.qnt dispatchOperator         temporal  operatorPauseFailsFast       holds
check S2  attempts.qnt attemptsRetry            invariant accounting                   holds
check S2  attempts.qnt attemptsNoRetry          invariant accounting                   holds
check L5  attempts.qnt attemptsRetry            temporal  terminates                   holds
check L5  attempts.qnt attemptsNoRetry          temporal  terminates                   holds
check L7  fleet.qnt    fleetTTL                 invariant noJobOnRecyclingVM           holds
check L7  fleet.qnt    fleetTTL                 invariant drainedBeforeRecycle         holds
check L7  fleet.qnt    fleetTTL                 temporal  alwaysComesBack              holds
check L7  fleet.qnt    fleetTTLDrainDown        invariant noJobOnRecyclingVM           fails
check L10 fleet.qnt    fleetTTL                 temporal  unhealthyIsRebuilt           holds
check S3  enrol.qnt    identityFixed            invariant noForeignIdentity            holds
check S3  enrol.qnt    identityFixed            invariant noAttackerInProd             holds
check S3  enrol.qnt    identityBefore           invariant noForeignIdentity            fails
check S3  enrol.qnt    identityBefore           invariant noAttackerInProd             fails
check S4  enrol.qnt    identityFixed            invariant rollCallLeavesOnlyAccounted  holds
check S4  enrol.qnt    identityBefore           invariant rollCallLeavesOnlyAccounted  holds
check L8  enrol.qnt    certsTolerant            invariant neverExpires                 holds
check L8  enrol.qnt    certsTooManyFailures     invariant neverExpires                 fails
check L9  enrol.qnt    join                     temporal  joins                        holds

# S5-S7, S9 and the proved half of S8: Lean checks every proof (a `sorry` would be an error here).
if grep -rn 'sorry' "$spec/lean/Grove" >/dev/null; then
	echo "spec/lean contains sorry" >&2
	fail=1
fi
(cd "$spec/lean" && lake build) || fail=1
row S5-9 lean "Grove.Auth/ArtifactPath/Broker" proved "$([ $fail = 0 ] && echo proved || echo FAILED)"

# The Lean transcriptions against the Go code, and L6, L4, S1, L2 against the real packages.
(cd "$spec/.." && GROVE_SPEC_BIN="$spec/lean/.lake/build/bin/grove-spec" go test -count=1 \
	-run 'TestSpec_|TestL6_' ./internal/server ./internal/fleet) || fail=1
# Model-based conformance: sample traces of the fixed snooze model and replay each against the real
# code (TestSnoozeConformsToModel); then put each old bug back and require that the tests catch it.
traces=$(mktemp -d)
(cd "$spec/quint" &&
	"$QUINT" run snooze.qnt --backend=typescript --main=snoozeMBT --max-samples=500 --max-steps=40 \
		--n-traces=500 --seed=1 --out-itf="$traces/op_{seq}.itf.json" >/dev/null &&
	"$QUINT" run snooze.qnt --backend=typescript --main=snoozeMBTFresh --max-samples=500 --max-steps=40 \
		--n-traces=500 --seed=2 --out-itf="$traces/fresh_{seq}.itf.json" >/dev/null) || fail=1
n=$(find "$traces" -name '*.itf.json' | wc -l | tr -d ' ')
row L1-2 snooze.qnt "traces sampled for replay" 1000 "$n"
[ "$n" = 1000 ] || fail=1
(cd "$spec/.." && GROVE_SNOOZE_TRACES="$traces" go test -count=1 -race ./internal/conformance) || fail=1
mut=$(python3 "$spec/mutants.py" "$spec/.." "$traces") || fail=1
echo "$mut"
killed=$(grep -c '^killed' <<<"$mut" || true)
total=$(python3 -c 'import importlib.util,sys; s=importlib.util.spec_from_file_location("m",sys.argv[1]); m=importlib.util.module_from_spec(s); s.loader.exec_module(m); print(len(m.MUTANTS))' "$spec/mutants.py")
row S1 mutants "old bugs caught by conformance" "$total/$total" "$killed/$total"
[ "$killed" = "$total" ] || fail=1

# grove.spec: the 20 properties as Specify behaviors. Its formal links (Quint invariants by seeded
# simulation, Lean theorems) must all pass; temporal properties stay with the TLC rows above.
if [ -n "${SPECIFY:-}" ]; then
	out=$(cd "$spec/.." && LEAN_PATH="$spec/lean/.lake/build/lib/lean" "$SPECIFY" formal check \
		--spec spec/grove.spec --timeout-ms 300000 2>/dev/null) || true
	got=$(python3 -c 'import json,sys; r=json.load(sys.stdin); n=len(r["results"]); p=sum(x["outcome"]["status"]=="passed" for x in r["results"]); print(f"{p}/{n}" if r["valid"] else f"FAILED {p}/{n}")' <<<"$out" 2>/dev/null || echo error)
	row S1-9 specify "grove.spec formal check" 18/18 "$got"
	[ "$got" = 18/18 ] || fail=1
fi

# grove.spec: the 20 properties as Specify behaviors. Its formal links (Quint invariants by seeded
# simulation, Lean theorems) must all pass; temporal properties stay with the TLC rows above.
if [ -n "${SPECIFY:-}" ]; then
	out=$(cd "$spec/.." && LEAN_PATH="$spec/lean/.lake/build/lib/lean" "$SPECIFY" formal check \
		--spec spec/grove.spec --timeout-ms 300000 2>/dev/null) || true
	got=$(python3 -c 'import json,sys; r=json.load(sys.stdin); n=len(r["results"]); p=sum(x["outcome"]["status"]=="passed" for x in r["results"]); print(f"{p}/{n}" if r["valid"] else f"FAILED {p}/{n}")' <<<"$out" 2>/dev/null || echo error)
	row S1-9 specify "grove.spec formal check" 18/18 "$got"
	[ "$got" = 18/18 ] || fail=1
fi

# S10: CodeQL taint queries. Each leak query must find nothing; the coverage query must find each
# of the four secrets at its one allowed destination (else the leak queries would be vacuous).
if [ -n "${CODEQL:-}" ]; then
	db=$(mktemp -d)/db
	(cd "$spec/.." && "$CODEQL" database create "$db" --language=go --source-root=. --command="go build ./..." >/dev/null)
	rows() {
		"$CODEQL" query run --database="$db" --output="$db/$1.bqrs" "$spec/codeql/$1.ql" >/dev/null
		"$CODEQL" bqrs decode --format=csv --no-titles "$db/$1.bqrs" | grep -c . || true
	}
	for q in JobSecretLeak RepoTokenLeak BrokerCredentialLeak NodeSecretLeak; do
		n=$(rows $q)
		row S10 codeql "$q" 0 "$n"
		[ "$n" = 0 ] || fail=1
	done
	n=$(rows Coverage)
	row S10 codeql Coverage 4 "$n"
	[ "$n" = 4 ] || fail=1
fi

exit $fail
