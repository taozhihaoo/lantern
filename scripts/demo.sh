#!/usr/bin/env bash
# scripts/demo.sh — 一键演示(规格 13/M10):
# 生成示例语料 → 增量索引 → 搜索/Explain → Why-not → stats/check。
set -euo pipefail
cd "$(dirname "$0")/.."

GO=${GO:-go}
WORK=${LANTERN_DEMO_DIR:-$(mktemp -d)/lantern-demo}
mkdir -p "$WORK/docs"
trap 'rm -rf "$WORK"' EXIT

echo "==> 1. 构建可执行"
$GO build -o "$WORK/lantern" ./cmd/lantern

echo "==> 2. 生成示例语料($WORK/docs)"
mkdir -p "$WORK/docs/sub"
cat > "$WORK/docs/.gitignore" <<'EOF'
*.log
EOF
cat > "$WORK/docs/engine.md" <<'EOF'
---
draft: true
---
# 全文搜索引擎设计

倒排索引与 BM25F 排序是搜索引擎的核心技术。
分段倒排索引支持增量索引与崩溃安全恢复。
EOF
cat > "$WORK/docs/sub/code.go" <<'EOF'
package demo

// parseHTTPRequest 解析 HTTP 请求并调用 doWork。
func parseHTTPRequest() { doWork() }
EOF
cat > "$WORK/docs/notes.txt" <<'EOF'
plain notes mentioning lantern and release notes
EOF
echo "ignored log line" > "$WORK/docs/noise.log"

run() { (cd "$WORK" && "./lantern" "$@"); }

echo "==> 3. 增量索引"
run index docs

echo "==> 4. 搜索:中文二元组 + Explain"
run search "搜索引擎" --explain --no-color

echo "==> 5. 搜索:标识符短语 http request"
run search '"http request"' --no-color

echo "==> 6. 搜索:单字'擎'(二元组包含匹配)"
run search "擎" --no-color

echo "==> 7. Why-not:notes.txt 为何未命中 '搜索引擎'"
run why-not "搜索引擎" docs/notes.txt

echo "==> 8. Why-not:JSON 输出"
run why-not "搜索引擎 missingword" docs/engine.md --json

echo "==> 9. stats / check / compact"
run stats
run check
run compact
run check

echo "==> 演示完成(工作目录 $WORK 已清理)"
