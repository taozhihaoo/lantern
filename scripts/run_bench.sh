#!/usr/bin/env bash
# scripts/run_bench.sh - reproducible benchmark: 100k docs / ~500MB corpus.
# Prints real numbers; copy them into BENCH.md verbatim.
set -euo pipefail
REPO="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO"
GO=${GO:-go}
B=${BENCH_DIR:-$REPO/.bench}
mkdir -p "$B"

echo "== Machine =="
uname -a
$GO version
# NUMBER_OF_PROCESSORS only exists on Windows; keep set -u happy everywhere.
if command -v nproc >/dev/null 2>&1; then
  echo "cores=$(nproc)"
elif command -v getconf >/dev/null 2>&1; then
  echo "cores=$(getconf _NPROCESSORS_ONLN)"
else
  echo "cores=${NUMBER_OF_PROCESSORS:-unknown}"
fi

echo "== 1. Generate corpus (100k docs, target 500MB) =="
$GO run ./scripts/gen_corpus -dir "$B/corpus" -n 100000 -target-mb 500

$GO build -o "$B/lantern" ./cmd/lantern

# The index base directory is the parent of --index, and targets are resolved
# relative to that base. Running from $B with target "corpus" therefore scans
# exactly the corpus; `cd corpus && index . --index $B/idx8` would make the
# base $B and silently index the whole .bench directory (other indexes too).
echo "== 2. Index throughput workers=8 =="
"$B/lantern" index corpus --workers 8 --index "$B/idx8"

echo "== 3. Index size (with stored body vs raw corpus) =="
du -sk "$B/idx8" "$B/corpus"

echo "== 4. Index throughput workers=1 =="
"$B/lantern" index corpus --workers 1 --index "$B/idx1"

echo "== 5. Index size (without stored body) =="
"$B/lantern" index corpus --workers 8 --no-store-body --index "$B/idxnb"
du -sk "$B/idxnb"

echo "== 6. Query latency (idle, 200 runs / mode) =="
for m in and phrase prefix fuzzy; do
  $GO run ./scripts/bench_search -index "$B/idx8" -mode "$m" -n 200
done

echo "== 7. Search latency while a re-index/merge runs in background =="
"$B/lantern" index corpus --workers 8 --no-store-body --index "$B/idx8" >/dev/null &
BG=$!
sleep 3
$GO run ./scripts/bench_search -index "$B/idx8" -mode and -n 200
wait $BG || true

echo "BENCH_DONE"
