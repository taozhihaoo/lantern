#!/usr/bin/env bash
# scripts/run_bench.sh — 规格第 12 节基准:10 万篇/约 500MB 语料。
# 输出真实数字,写入 BENCH.md 时人工誊录(不美化)。
set -euo pipefail
REPO="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO"
GO=${GO:-go}
B=${BENCH_DIR:-$REPO/.bench}
mkdir -p "$B"

echo "== 机器配置 =="
uname -a
$GO version
echo "cores=$NUMBER_OF_PROCESSORS"

echo "== 1. 生成语料(10 万篇,目标 500MB)=="
$GO run ./scripts/gen_corpus -dir "$B/corpus" -n 100000 -target-mb 500

$GO build -o "$B/lantern.exe" ./cmd/lantern

echo "== 2. 索引吞吐 workers=8 =="
( cd "$B/corpus" && "$B/lantern.exe" index . --workers 8 --index "$B/idx8" )

echo "== 3. 索引体积(含正文 vs 原文)=="
du -sk "$B/idx8" "$B/corpus"

echo "== 4. 索引吞吐 workers=1 =="
( cd "$B/corpus" && "$B/lantern.exe" index . --workers 1 --index "$B/idx1" )

echo "== 5. 索引体积(不含正文)=="
( cd "$B/corpus" && "$B/lantern.exe" index . --workers 8 --no-store-body --index "$B/idxnb" )
du -sk "$B/idxnb"

echo "== 6. 查询延迟(空闲,索引含正文,200 次/模式)=="
for m in and phrase prefix fuzzy; do
  $GO run ./scripts/bench_search -index "$B/idx8" -mode "$m" -n 200
done

echo "== 7. 搜索与合并/索引并发:后台重索引(no-store,触发提交与合并),同时测延迟 =="
( cd "$B/corpus" && "$B/lantern.exe" index . --workers 8 --no-store-body --index "$B/idx8" >/dev/null ) &
BG=$!
sleep 3
$GO run ./scripts/bench_search -index "$B/idx8" -mode and -n 200
wait $BG || true

echo "BENCH_DONE"
