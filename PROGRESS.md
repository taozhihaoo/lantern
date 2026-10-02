# PROGRESS

按 GOAL.md 第 13 节里程碑推进;每个里程碑通过质量门禁(gofmt / vet /
test -race / 交叉编译)后提交。本文件只记录真实进度与实测数据。

## M0 脚手架 — ✅ 完成

- [x] go.mod(仅 module+go 指令,零 require)
- [x] Makefile(build/test/race/fuzz-short/bench/demo/check/cross/corpus)
- [x] internal/fsx:FS 接口 + osFS + FaultFS(Fail/Drop/Truncate 三种注入模式)
- [x] fsx 单元测试;文档骨架

## M1 analysis — ✅ 完成

- [x] 全角折叠(FF01–FF5E/U+3000)、小写化;Token 五元组;偏移单调 rune 对齐
- [x] 标识符感知(camel/Pascal/缩写;整体与首子词共享 Pos)
- [x] CJK bigram + 单字;表驱动测试(含规格全部用例)
- [x] FuzzAnalyze:不 panic、偏移合法、Pos 单调
- 实测:`go test -cover ./internal/analysis` → **100.0%**

## M2 codec — ✅ 完成

- [x] varint/delta/位集(Rank1/Iterate)/前缀压缩块/CRC footer
- [x] 往返 + 截断/翻转损坏检测 + FuzzCodecRoundtrip
- 实测:覆盖率 **91.2%**;修复记录:ParseFooter 的 CRC 覆盖范围与写入端不一致(测试暴露)

## M3 段 — ✅ 完成

- [x] memtable(5000 篇/64MB 双阈值)、段写入器/读取器
- [x] terms 前缀压缩块 + 稀疏索引;postings 128 篇/块 delta+varint + skip 表
- [x] norms/dv 固定宽;docs 存储字段 + flate 正文;live_<gen>.bits 墓碑
- [x] footer 校验、损坏检测、迭代器 Next/Advance(跳块+跳墓碑)
- 实测:覆盖率 ≥80%(M4 后持续上升);修复:skip 偏移误用相对偏移、位置差分基准不一致

## M4 提交与恢复 — ✅ 完成

- [x] manifest(generation/文件校验和/liveGen/state_len/next_seg_id)+ state.jsonl 追加日志
- [x] 原子提交协议(5.6 严格顺序);失败时按磁盘 manifest 实况收敛
- [x] 启动恢复(孤儿 tmp/段清理、state 截断)
- [x] write.lock(O_EXCL + 5s 心跳 + 30s 陈旧警告接管)
- [x] 快照引用计数(MVCC,refs/liveGen 原子量);分层合并(层内 ≥8 段)+ compact
- [x] LRU 块缓存(64MB);FaultFS 崩溃注入(提交协议每一步 × Fail/Drop/截断)
- [x] 并发搜索 + 合并 race 测试;Stats 与 CheckIndex
- 实测:`go test -race ./...` 通过;修复:commit 漏 unlock 死锁、refs++ 竞争、
  内存状态表漏应用自动 put 条目

## M5 查询 — ✅ 完成

- [x] lexer/parser/AST 全语法;错误三行格式带列号与指示符;退出码语义
- [x] planner + 迭代器树(AND leapfrog/OR 归并/NOT/短语邻近位置/过滤惰性/Top-K)
- [x] CJK:单字包含展开、多字 bigram 短语、混排 AND;模糊剪枝 DP;前缀展开
- [x] 差分测试:testutil 固定种子语料 + 独立暴力 oracle;5 种子 × 3 分段配置 ×
      40 查询结果集与 Total 完全一致;Top-K 分数误差 < 1e-9
- [x] FuzzLexer
- 修复记录(全部由差分暴露,详见 git log M5):termIter 字段 OR 误写 AND、
  迭代器预定位丢首文档、逐字段饱和、OR 得分语义(改递归求值)、
  decodeStoredDoc 截断正文、CJK raw 重分析引错误 bigram、操作符列号、生成器字节切分

## M6 排序与高级匹配 — ✅ 完成

- [x] BM25F(规格参数)+ 跨段统计 + df=max 字段近似 + N 含墓碑
- [x] Explain(JSON + 文本树,Σ贡献 = 总分 <1e-9 测试强制,含 recency 开启)
- [x] recency 加成(默认关,排位变化有测试);snippet 密度窗口 + rune 对齐高亮
- [x] 差分扩展至全部语法与打分比对

## M7 扫描与 CLI — ✅ 完成

- [x] 忽略规则子集(* ** ? / ! 锚定;规则带来源行号;默认忽略 .git/node_modules/.lantern)
- [x] 嗅探(NUL/控制字符 → binary;非法 UTF-8 → non_utf8 计数;UTF-16 BOM 转码)
- [x] 提取器:md(front matter/标题)/html(剥离 script/style + 实体)/txt/json/csv/code/docx(zip+xml)
- [x] 增量:size+mtime 短路 → sha256;删除墓碑;mtime-only 分类;结果按路径排序与 worker 数无关
- [x] watch 轮询 + 去抖 + 优雅退出;索引汇总(新增/更新/删除/跳过分类/耗时/吞吐)
- [x] CLI:index/search/why-not/stats/check/compact/serve;--index/LANTERN_DIR;
      flag 任意位置;退出码 0/1/2
- 冒烟:端到端(索引→中文/标识符/短语搜索→stats/check→compact→watch 提交→删除→陈旧锁接管)

## M8 Why-not — ✅ 完成

- [x] 文档层:ignored(规则+来源:行号)/too_large/binary/non_utf8/outside/
      not_scanned/missing_on_disk/stale(两侧 size/mtime/sha 对照)
- [x] 子句层:存储正文重分析;词项缺失最近词(编辑距离 ≤2)+ 位置;字段不匹配
      指出字段;短语/邻近实际间隔与顺序颠倒/间隔过大;NOT 触发位置;过滤实际值
- [x] 建议层:移除单子句可命中 + RankOf 排名估计(1-based/总数)
- [x] 14 类场景 golden 文件测试(固定 mtime 保证确定性)+ JSON 输出

## M9 服务与 UI — ✅ 完成

- [x] /api/search /api/why-not /api/doc /api/stats /healthz + / (embed UI)
- [x] 每请求 MVCC 快照;只读;CSP/nosniff/X-Frame-Options;doc 仅索引内路径
- [x] UI:120ms 去抖、<mark> 高亮、得分条形拆解、why-not 面板、键盘导航、
      明暗自适应、零外部请求
- [x] httptest 接口测试(搜索/语法错误/why-not/doc 穿越 404/安全头/healthz)

## M10 收尾 — ✅ 完成

- [x] 语料生成器(scripts/gen_corpus,10 万篇/587MB)+ run_bench.sh 一键复现
- [x] go test -bench + scripts/bench_search(p50/p95 四模式 + 并发场景)
- [x] BENCH.md 实测(机器配置/吞吐/体积比/延迟/并发/瓶颈分析)
- [x] ARCHITECTURE.md 完整字节布局与设计;README;DECISIONS D1-D19
- [x] scripts/demo.sh 一键演示(语料→索引→搜索/Explain→Why-not→stats/check/compact)
- [x] 全量门禁(gofmt/vet/test -race/交叉编译 windows/darwin/linux)+ 三处 fuzz 短目标
- [x] 性能优化(基准驱动):segScorer 预建游标、norms/dv 预载、布尔跳过位置解码、
      StoredDocLite、TermPrefix 范围扫描、CJK 二元组首尾索引缓存

---

## 覆盖率(go test -race -cover ./... 实测,最终门禁)

| 包 | 覆盖率 |
| --- | --- |
| internal/analysis | **100.0%** |
| internal/codec | **91.2%** |
| internal/index | **81.5%** |
| internal/query | **82.6%** |
| internal/scan | 86.0% |
| internal/snippet | 98.4% |
| internal/server | 73.9% |
| internal/whynot | 68.1% |
| internal/fsx | 50.5% |

(internal/index、internal/query、internal/analysis 均 ≥80%,满足 11.6。)

## 已知问题

1. Windows 下目录 fsync 为 no-op(NTFS 元数据日志;DECISIONS.md D2)。
2. 合成语料(随机词表)下"不含正文"索引体积比 41.3%,略超 40% 参考
   (随机文本熵高,flate 无效;真实文本更优,分析见 BENCH.md)。
3. 三词 AND p95(163ms)未达 20ms 参考:基准查询集为高频词最坏偏置
   (交集数千~两万篇),单命中 ~8µs 主要为同分并列裁决所需的存储路径
   随机读;典型低频查询为毫秒级。完整分析见 BENCH.md。

---

## 第 14 节完成标准自检(逐项附实际命令输出摘要)

**[x] 1. go.mod 无 require;go vet、gofmt、go test -race 全部通过;交叉编译通过**

```
$ cat go.mod
module lantern
go 1.22                      ← 无任何 require
$ gofmt -l .                 ← 无输出
$ go vet ./...               ← 无输出(通过)
$ go test -race -cover ./... -count=1
ok  lantern/internal/analysis  100.0%
ok  lantern/internal/codec      91.2%
ok  lantern/internal/index      81.5%
ok  lantern/internal/query      82.6%
ok  lantern/internal/scan       86.0%
ok  lantern/internal/server     73.9%
ok  lantern/internal/snippet    98.4%
ok  lantern/internal/whynot     68.1%
ok  lantern/internal/fsx        50.5%
$ CGO_ENABLED=0 GOOS=windows go build ./...   ← 通过
$ CGO_ENABLED=0 GOOS=darwin  go build ./...   ← 通过
$ CGO_ENABLED=0 GOOS=linux   go build ./...   ← 通过
```

**[x] 2. 差分测试(≥5 种子、含合并前后)全部通过;Top-K 打分误差 < 1e-9**

`go test ./internal/query -run TestDifferential`:
5 个种子(seed1–5) × 3 种分段配置(single-seg / multi-seg / merged 合并后)
× 每种子 40 条随机查询(布尔/短语/邻近/前缀/模糊/字段/过滤),结果集与
Total 与暴力扫描完全一致 → `ok lantern/internal/query`。
`TestDifferentialScoring`:3 种子 × 30 查询,Top-K 分数与暴力 BM25F 逐条
比对,断言 |diff| < 1e-9 → 通过。

**[x] 3. 崩溃注入覆盖提交协议每一步**

`go test ./internal/index -run 'TestCrashInjectionAtEveryStep|TestCrashTruncate'`:
以干净提交的全部 FS 操作数为准,对第 k 步(k=1..N)分别注入 Fail(操作前
崩溃)与 Drop(操作后崩溃)两模式,每步重开索引断言:可打开、内容等于
旧版本或新版本之一、CheckIndex 通过、无孤儿文件;另对每个段文件与
manifest tmp/state 追加注入断电截断(撕裂写 + 进程死亡)。全部通过。

**[x] 4. 中文用例**

单测 `TestCriteriaCJKAndIdentifiers` + CLI 实测:
```
$ lantern search "搜索"   → docs/engine.md(全文搜索引擎设计)命中
$ lantern search "引擎"   → 命中
$ lantern search "擎"     → 命中(单字包含匹配二元组词典)
$ lantern search "Go语言并发" → 命中(中英混排)
```

**[x] 5. 标识符用例**

```
$ lantern search '"http request"'  → docs/code.go(parseHTTPRequest)命中(短语命中子词序列)
$ lantern search "case"            → docs/snake.txt(snake_case_name)命中(子词单独检索)
$ lantern search "snake_case_name" → 命中(多原子 AND)
```

**[x] 6. 全部查询语法可用;语法错误带列号指示;模糊/前缀/过滤正确**

语法覆盖:`foo bar / OR / -foo / NOT foo / "短语" / "a b"~3 / title: body:
path: / pre* / foo~ ~/1 ~/2 / ext:go / size:>1mb / size:<=10kb /
mtime:>2025-01-01 / mtime:2025-06(TestParseValid 19 例 + 差分全部语法)。
```
$ lantern search "(unclosed"; echo exit=$?
query:1:1: 缺少右括号
  (unclosed
  ^
exit=2
$ lantern search "ok query"; echo exit=$?
exit=0
```

**[x] 7. --explain 逐项贡献之和等于总分(测试强制)**

`TestExplainSumEqualsFinal`:8 类查询 × recency 开启(0.6/45d),对每条
命中断言 `Σ贡献 - FinalScore` 与 `FinalScore - hit.Score` 均 < 1e-9 → 通过。
渲染见 `Explain.RenderText()`(score 行 + 每词项 contrib 行 + 各字段 tfw 行)。

**[x] 8. why-not 覆盖第 8 节每种原因,golden 测试通过**

`go test ./internal/whynot -run TestGolden`:14 类场景 golden 文件逐一比对
(ignored[规则+.gitignore:1]/too_large/binary/non_utf8/not_scanned/
outside/stale[两侧 size·mtime·sha]/term_missing_nearest[最近词+位置]/
field_mismatch[指出字段]/phrase_reversed[顺序颠倒]/phrase_gap[间隔过大]/
not_triggered[NOT 位置]/filter_fail[实际值 vs 要求]/suggest_remove[移除子句
后命中 + 排名 1/1]),并校验 JSON 输出合法 → 通过。

**[x] 9. 增量索引:修改/删除/重命名正确;未变不重索引;worker 数无关**

`TestScanIncremental`(修改→update、删除→deletes、新增→add、二次扫描
unchanged=3/Added=0)、`TestScanMTimeOnly`(内容未变仅 mtime → mtime-only
分类)、`TestScanWorkersDeterministic`(1 vs 8 workers 结果序列完全一致)、
CLI 冒烟(修改/删除文件后 search 结果随增量正确翻转;重命名=删除+新增)。

**[x] 10. serve 可用:UI 搜索/得分拆解/why-not;无外部网络请求**

`go test ./internal/server`:/api/search(中文+explain+snippet 高亮)、
语法错误 400、/api/why-not、/api/doc(索引内 200、穿越 404)、/healthz、
CSP/nosniff 头、UI 内容。实测冒烟:
```
$ lantern serve --addr 127.0.0.1:7788 &
$ curl -s localhost:7788/healthz  → {"status":"ok"}
$ curl -s "localhost:7788/api/search?q=搜索&explain=1" → hits+explain JSON
$ curl -sI localhost:7788/ → CSP/nosniff/X-Frame-Options: DENY
```
UI 为 embed 单文件(原生 JS):`grep -E 'https?://|cdn|unpkg|jsdelivr'
internal/server/ui/index.html` → 无外部请求;默认仅监听 127.0.0.1。

**[x] 11. BENCH.md 真实实测数据与机器配置;scripts/demo.sh 一键运行**

BENCH.md:AMD Ryzen 7 7700 / Win10 / go1.27.0;10 万篇 587MB 语料;
索引吞吐 523 MB/s(8 workers)/106 MB/s(1 worker);体积比(含正文
73.6%、不含正文 41.3%);四模式延迟 p50/p95;并发重索引下延迟不变;
AND p95 未达标项附瓶颈分析与已做优化清单。`bash scripts/demo.sh` 一键
演示(生成语料→增量索引→中文/标识符搜索+Explain→Why-not 文本与
JSON→stats/check/compact)全流程通过。

**[x] 12. README / ARCHITECTURE / DECISIONS / PROGRESS 完整且与代码一致**

- README:安装、全命令示例、HTTP API、设计取舍、限制。
- docs/ARCHITECTURE.md:包依赖与锁说明;footer;全部 8 类段文件逐字节
  布局;manifest/state;原子提交协议;写锁;MVCC/合并;读取路径与
  性能关键路径;分析/查询/打分/扫描/Why-not/服务设计。
- DECISIONS.md:D1–D19(规格歧义决策与理由)。
- PROGRESS.md:各里程碑完成项、实测数据、修复记录、已知问题、本自检。

---

结论:第 14 节 12 项验收标准全部满足(第 1 项的覆盖率、第 2/3/7/8 项由
测试强制,第 4/5/6/9/10 项有单测与 CLI 双重证据,第 11 项为实测数据,
第 12 项文档与代码同步)。
