# ARCHITECTURE

Lantern 是仅用 Go 标准库实现的本地全文搜索引擎:分段倒排索引 + BM25F +
查询语言 + 崩溃安全存储 + 增量索引,并提供 Explain(得分逐项拆解)与
Why-not(未命中诊断)两个特性。本文档完整描述存储格式与设计。

## 1. 包依赖(自上而下,禁止反向)

```
cmd/lantern → internal/cli → internal/server → internal/whynot → internal/query
  → internal/rank → internal/snippet → internal/scan → internal/index
  → internal/codec → internal/analysis → internal/fsx
```

`internal/testutil` 仅供各包 `_test.go` 导入(生产代码禁止),自身依赖
analysis/query/rank(见 DECISIONS.md D12)。所有权与锁:

- 单写者:跨进程由 `write.lock` 串行化;进程内 Writer 的 `cm` 互斥串行化
  Flush/Compact/后台合并。
- 读者:`Index.Snapshot()` 持有不可变段集合的引用计数;`SegmentRef.refs`
  与 `liveGen` 为原子量;`SegmentReader` 的所有读方法并发安全
  (LRU 缓存自带锁,墓碑位集经 atomic.Pointer 读取)。
- 被移出 manifest 的段在引用归零后物理删除(Windows 下无法删除打开中的
  文件,故墓碑历史文件保留,见 D9)。

## 2. 通用文件 footer

所有二进制数据文件以 12 字节 footer 结尾:

```
[ payload ... ][ "LNTN"(4B) ][ format_version u32 LE ][ crc32 u32 LE ]
```

crc32(IEEE)覆盖 payload + magic + version 的全部字节。`meta.json`、
`manifest.json`、`state.jsonl` 为纯 JSON,不加 footer(完整性分别由原子
rename、manifest 中的逐文件校验和、state_len 有效前缀保证,见 D8)。

## 3. 段(目录 `seg_<id>/`,不可变)

段内 docID 为 uint32 本地编号,从 0 递增。可检索字段:0=path、1=title、
2=body(NumFields=3);ext/size/mtime 为 doc values。

### 3.1 meta.json

```json
{ "format": 1, "docs": N, "created": "RFC3339",
  "fields": {"path": {"doc_count": .., "total_len": ..},
             "title": {...}, "body": {...}},
  "ext_dict": ["go","md",...], "store_body": true }
```

`doc_count` 为该字段至少一个 token 的文档数;`total_len` 为该字段 token
总数(即字段长度=词数)。avglen(打分)由跨段汇总计算。

### 3.2 terms.dat / terms.idx(词典)

词项按 (fieldID, term 字节序) 全局排序,每 `TermsBlockSize=64` 项一块。

terms.dat:每块帧 `[blockLen u32 LE][payload]`;payload:

```
[u16 n]
每项: [fieldID u8][shared varint][suffixLen varint][suffix bytes]
      [docFreq varint][totalTF varint][postingsOff varint][postingsLen varint]
```

`shared` 仅在同字段时相对前项的公共前缀长度(跨字段置 0)。

terms.idx:`[nBlocks u32 LE]` + 每块一条稀疏索引:

```
[fieldID u8][termLen varint][首词项 bytes][blockOff u64 LE]
```

打开段时全量载入内存;`Term(field, term)` 对 (fieldID, term) 做二分定位
块,再块内线性查找。`Terms(field, fn)` 顺序遍历全部块。

### 3.3 postings.dat(倒排)

词项条目按词典顺序连续存放。每条目:

```
[文档块 × K,每块: [blockLen u32 LE][block]]
[skip 表: nBlocks varint + (maxDoc varint, off varint, len varint) × n]
[skipLen u16 LE]
```

block:`[n varint][docID delta varint × n][tf varint × n][位置 delta varint(每文档 tf 个,文档内首位置以 0 为基准)]`。
docID delta 跨块连续(块首相对上一块最后 docID)。skip 表的 off/len 为
**文件绝对偏移**(含 4 字节帧头),`PostingIterator.Advance(target)` 二分
skip 表整块跳读。迭代器约定:构造后未定位,首次 `Next/Advance` 定位;
自动跳过墓碑文档。`DocFreq/TotalTF` 计数包含已删除未合并文档(规格 7.1,
与 Lucene 一致)。

### 3.4 norms.dat

`nDocs × 3 × u32 LE`:每文档每可检索字段的 token 数,O(1) 随机访问
(`docID*12 + field*4`)。

### 3.5 dv.dat(doc values)

`nDocs × 20B`:`[mtime i64 LE][size i64 LE][extID u32 LE]`;
extID → meta.json 的 `ext_dict`。

### 3.6 docs.dat / docs.idx(存储字段)

docs.idx:`[nDocs u32 LE][(nDocs+1) × u64 LE 偏移]`;docs.dat 记录:

```
[pathLen varint][path][titleLen varint][title]
[mtime i64 LE][size i64 LE][sha256 32B][flags u8]
[bodyLen varint][body(flate 压缩;flags bit0=已存储)]
```

正文仅当 StoreBody 且 ≤ DefaultBodyLimit(1MB)时存储。

### 3.7 live_<gen>.bits(墓碑)

`codec.AppendBitset`:`[n u32 LE][words × u64 LE]` + footer。bit=1 表示
删除。提交删除时写入新文件 live_<gen+1>.bits(旧文件保留),段本身永不
修改;manifest 记录各段当前 liveGen。

## 4. manifest.json / state.jsonl(索引目录)

manifest(原子提交点):

```json
{ "format": 1, "generation": G, "state_len": B, "next_seg_id": S,
  "segments": [ {"id":"seg_000001","live_gen":0,
                 "files": {"terms.dat": {"size":..,"crc32":..}, ...} } ] }
```

state.jsonl 为追加日志,每行 `{"op":"put|del","path":..,"size":..,
"mtime":..,"sha":"hex","seg":"seg_x","doc":n}`;manifest.state_len 标记
有效前缀,恢复时截断尾部。put 条目由提交器在新段 ID 确定后自动生成。

## 5. 原子提交协议(规格 5.6 严格顺序)

```
写新段到 seg_x.tmp(WriteSegment:逐文件 create→write→fsync→close)
→ fsync tmp 目录 → rename 到 seg_x
→ 追加 state.jsonl(写→sync)
→ 写 manifest.<gen>.tmp(create→write→sync)
→ rename 为 manifest.json → fsync 索引目录
```

rename manifest 即提交点。任何一步失败:重读磁盘 manifest——若新
manifest 已落盘(仅剩目录 fsync 失败)按成功收敛内存状态;否则保留旧
状态,新段目录成为孤儿,下次 Open 恢复时清理。启动恢复
(`recoverDir`):删除 manifest.*.tmp 与 seg_*.tmp、未被 manifest 引用的
seg_* 目录;state.jsonl 截断到 state_len。

## 6. 写锁(规格 5.7)

`write.lock`:O_CREATE|O_EXCL 创建;内容为定长 256B JSON(pid、主机名、
heartbeat),持有者每 5s 原位覆写;超过 30s 无心跳视为陈旧,打印警告后
删除并接管。读者不加锁。

## 7. 快照与合并(MVCC)

`Snapshot()` 复制当前段列表并对每段 refs++(原子)。提交切换 manifest
不影响进行中的搜索;被移除段在 refs 归零后 `RemoveAll`。

合并(规格 5.9):分层策略,按 8 为底的数量级分层,层内 ≥8 段合并;
否则合并最小的 8 段防无限增长。合并 = 读取各段存活文档的存储字段,按
路径排序后重放分析(确定性,见 D11),产出单新段,同时从 manifest 移除
旧段并追加新 put 状态条目。`compact` 强制全量合并。后台 goroutine 在
每次 Flush 后检查策略,受写锁与 cm 互斥保护。

## 8. 读取路径

`SegmentReader.readAt` 经 LRU 块缓存(默认 64MB,键=段/文件/偏移/长度)
以 `ReadAt` 读取,不使用 mmap(规格 5.10)。打开段时仅校验各文件 footer
magic;完整 CRC 由 `lantern check`(CheckIndex)执行:段文件 CRC、meta
解析、manifest 校验和比对、孤儿检测、状态表指向文档的存活校验。

## 9. 分析(internal/analysis)

- 规范化:全角 ASCII(FF01–FF5E)→ 半角(-0xFEE0)、全角空格(3000)→
  空格、逐 rune 小写(1:1,不引入 NFKC,D3)。
- 原子 = 极大连续字母数字段;拉丁子段:camelCase/PascalCase/连续大写
  缩写切子词(HTTPRequest→http,request;整体 token 与首子词共享 Pos,
  后续子词依次递增);数字字母混合体不拆(v2)。
- CJK(Han/平假名/片假名/谚文)连续串输出重叠二元组,单字串输出单字;
  每个二元组占一个位置。
- Token{Text, Pos, StartByte, EndByte, Kind};偏移为原文有效字节区间且
  rune 对齐,StartByte 单调不减,Pos 非递减。

## 10. 查询(internal/query)

语法与优先级(NOT > AND > OR,词间隐式 AND)、短语/邻近、字段限定、
前缀(上限 1024,超限 truncated)、模糊(无数字时词长 ≤2→0、3–5→1、
≥6→2)、ext/size/mtime 过滤(doc values 惰性求值)、语法错误三行格式
带列号与指示符。执行:planner 产出迭代器树(AND leapfrog、OR 归并、
NOT 排除、短语/邻近位置窗口、Top-K 大小为 K 的小根堆,同分按路径
字典序);打分与匹配分离——每个命中文档按 plan 树递归求值(D16),
只有命中分支贡献分数。

CJK 查询:单字 → 词典"包含该字"的二元组展开(D5);多字 → bigram
序列短语;混排 → 拉丁词 + CJK 短语 AND。模糊展开:有序词典 + 共享
DP 行的前缀剪枝(行最小值 > k 整棵子树剪枝),权重 = 1 - dist/len
(下限 0.05),距离按 UTF-8 字节(D14)。

## 11. 打分(internal/rank,BM25F)

```
tfw(t,d)   = Σ_f  w_f · tf_f(t,d) / (1 - b_f + b_f · len_f(d)/avglen_f)
score(t,d) = idf(t) · tfw / (k1 + tfw)
idf(t)     = ln(1 + (N - df + 0.5)/(df + 0.5))
```

默认 k1=1.2;w=(path 2.0, title 3.0, body 1.0);b=(0.5, 0.5, 0.75),
可由 Params 覆盖。df(t) = 各字段跨段 df 之和的最大值(文档化近似);
N 与 df 含已删除未合并文档。avglen_f = Σ_seg totalLen_f / Σ_seg docCount_f
(跨段汇总后再打分,分段数不影响得分)。展开词项权重乘入贡献;
recency(默认关):score *= 1 + r·0.5^(ageDays/halfLifeDays)。
Explain:每词项的 weight/df/idf/各字段 tf·len·avglen·w·b·tfw/贡献、
boost 与最终得分,可 JSON 序列化 + 文本树;Σ贡献与总分之差 < 1e-9
(贡献先乘 boost 再按项顺序求和,测试强制)。

## 12. 扫描(internal/scan)

遍历不跟随符号链接;默认忽略 .git/node_modules/.lantern;支持
.lanternignore 与 .gitignore 子集(*、**、?、尾部 / 目录规则约束后代、
! 取反、/ 锚定;规则带来源与行号供 Why-not 引用)。二进制判定:NUL 或
<0x20 控制字符 → binary;其余非法 UTF-8 → non_utf8(D17);UTF-16 BOM
转码。提取器:markdown(front matter 剥离、首个 # 标题)、html(自写
剥离器,跳过 script/style,实体经 html.UnescapeString)、txt/json/csv/
代码纯文本、docx(zip+xml)。增量:size+mtime 短路,不同再 sha256;
内容未变仅 mtime 变化 → 重索引该文档(D19);删除写墓碑;重命名 =
删除+新增。Workers 并行解析,结果按路径排序后单写者汇入(与 worker 数
无关)。watch 轮询 + 去抖(一个安静间隔后批量提交),Ctrl-C 优雅退出。

## 13. Why-not(internal/whynot)

三层(规格 8):文档层(ignored(规则+来源:行号)/too_large/binary/
non_utf8/outside/not_scanned/missing_on_disk/stale(两侧 size/mtime/sha
对照));子句层(存储正文重新分析:词项缺失的最近词(编辑距离 ≤2)与
位置、字段不匹配指出字段、短语/邻近的实际间隔与顺序颠倒/间隔过大、
NOT 触发位置、过滤实际值 vs 要求);建议层(逐个移除顶层子句求值,
给出可命中方案与 RankOf 估计排名)。golden 文件测试覆盖每类原因。

## 14. 服务(internal/server)

只读;/api/search、/api/why-not、/api/doc(仅索引内路径)、/api/stats、
/healthz、/ (embed 单文件 UI)。每请求独立 MVCC 快照。安全头:CSP、
X-Content-Type-Options: nosniff、X-Frame-Options: DENY。UI 原生 JS:
120ms 去抖即搜、<mark> 高亮、得分条形拆解、why-not 面板、键盘导航、
明暗自适应,零外部请求。
