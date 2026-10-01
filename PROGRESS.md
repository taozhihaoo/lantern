# PROGRESS

按 GOAL.md 第 13 节里程碑推进;每个里程碑通过质量门禁(gofmt / vet / test -race / 交叉编译)后提交。
本文件只记录真实进度,随里程碑推进增量更新。

## M0 脚手架 — ✅ 完成

- [x] go.mod(仅 module+go 指令,零 require)
- [x] Makefile(build/test/race/fuzz-short/bench/demo/check/cross)
- [x] internal/fsx:FS 接口 + osFS + FaultFS(Fail/Drop/Truncate 三种注入模式)
- [x] fsx 单元测试
- [x] 文档骨架:README / docs/ARCHITECTURE / DECISIONS / BENCH / PROGRESS

门禁:gofmt 无输出;go vet 干净;go test -race 通过;windows/darwin 交叉编译通过。

## M1 analysis — ✅ 完成

- [x] 规范化:小写化 + 全角 ASCII 折叠(FF01–FF5E → 21–7E,U+3000 → 空格)
- [x] Token{Text, Pos, StartByte, EndByte, Kind};偏移单调、rune 对齐
- [x] 标识符感知:camelCase/PascalCase/连续大写缩写/snake/kebab/点分
- [x] CJK bigram + 单字 token;整体 token 与首子 token 共享 Pos
- [x] 表驱动测试(含 "Go语言并发编程"、parseHTTPRequest、snake_case_name、v2.1.3)
- [x] FuzzAnalyze:不 panic、偏移合法、Pos 单调不减(20s ≈ 280 万次执行)

实测:`go test -cover ./internal/analysis` → **100.0%** statements。
已知问题:无效 UTF-8 输入下"rune 对齐"无定义,fuzz 不变量仅在合法 UTF-8 输入上检查对齐。

## M2 codec — ✅ 完成

- [x] varint(u32/u64)、delta 编解码
- [x] 位集 Set/Get/Rank1/Iterate + 序列化
- [x] 前缀压缩字符串块(terms.dat 每 64 词项一块)
- [x] CRC32 footer 封装:magic "LNTN" + version(u32 LE) + crc(u32 LE),
      CRC 覆盖 payload+magic+version
- [x] 往返测试、截断/翻转损坏检测测试 + FuzzCodecRoundtrip(20s ≈ 60 万次执行)

实测:`go test -cover ./internal/codec` → **91.2%** statements。
修复记录:初版 ParseFooter 的 CRC 覆盖范围与写入端不一致,测试立即暴露并修复。

## M3 段 — ✅ 完成

- [x] memtable(文档数/字节双阈值)、段写入器、段读取器
- [x] terms.dat/idx 前缀压缩块 + 稀疏索引打开时载入内存二分定位;
      postings.dat 128 篇/块 delta+varint + skip 表(Advance 跳块)
- [x] norms.dat 固定宽 uint32;dv.dat 固定宽 doc values
- [x] docs.dat/idx 存储字段 + flate 可选正文;live_<gen>.bits 墓碑位集
- [x] footer magic/version/CRC;损坏检测测试(截断/翻转必须报错)
- [x] posting 迭代器 Next/Advance(跳块+跳墓碑)/TF/Positions

实测:`go test -cover ./internal/index` → **83.3%**(M3 时点;M4 后继续上升)。
修复记录:skip 表偏移误用条目内相对偏移(应为文件绝对偏移)、位置差分
基准不一致(编码 -1 vs 解码 0),均由 M3 测试暴露并修复。

## M4 提交与恢复 — ✅ 完成

- [x] manifest.json(generation/段列表+文件校验和/各段 liveGen/state_len/next_seg_id)
      + state.jsonl 追加日志(有效前缀由 manifest.state_len 标记)
- [x] 原子提交协议(规格 5.6 严格顺序);提交点后失败时按磁盘 manifest
      实况收敛(目录 fsync 失败场景)
- [x] 启动恢复:清理孤儿 manifest tmp/tmp 段/未引用段目录,截断 state.jsonl
- [x] 单写者锁 write.lock(O_EXCL,pid+主机名+心跳定长记录,5s 刷新,30s 陈旧警告接管)
- [x] 快照引用计数(MVCC):refs/liveGen 原子量;引用归零且已移出 manifest 才物理删除
- [x] 分层合并(8 为底数量级分层,层内 ≥8 段合并,墓碑丢弃,后台 goroutine+cm 互斥);
      compact 全量合并;合并按路径重放分析保证 docID 确定
- [x] LRU 块缓存(64MB 默认,ReadAt)
- [x] FaultFS 崩溃注入测试:提交协议每一步(Fail/Drop × ~90 步)+
      断电截断(逐文件);重开后可打开、内容=旧或新版本、check 通过、无孤儿
- [x] 并发测试:4 读 goroutine + 3 轮 compact;-race 干净
- [x] Stats 与 CheckIndex(manifest/段 CRC/校验和/孤儿/状态表存活一致性)

实测:`go test -race ./...` 通过(含 90+ 崩溃注入子用例)。
修复记录:commit 漏 unlock 导致死锁;refs++ 读锁下竞争(改原子量);
内存状态表未应用自动 put 条目导致 compact 后路径丢失;均被测试捕获。

## M5 查询 — ✅ 完成

- [x] lexer/parser/AST:NOT>AND>OR、隐式 AND、括号、短语 "…"、~slop 邻近、
      字段限定(title/path/body/ext/size/mtime)、前缀 *、模糊 ~/~/N
- [x] 语法错误带列号与 ^ 指示符(query:1:C: 三行格式,操作符列号取起始)
- [x] planner + 迭代器树:AND leapfrog、OR 归并、短语/邻近位置交集、NOT 排除、
      过滤惰性求值(doc values)、Top-K 小根堆(同分按路径字典序)
- [x] 匹配与打分分离:迭代器树负责匹配;得分由 plan 树递归求值
      (只有命中分支贡献分数,与暴力 oracle 语义一致)
- [x] CJK 查询:单字 → 二元组包含展开;多字 → bigram 序列短语;
      混排(Go语言并发)→ 拉丁词 + CJK 短语 AND
- [x] 模糊展开:有序词典 + 共享 DP 行前缀剪枝(行最小值>k 整树剪枝);
      展开权重 = 1 - dist/len(下限 0.05);上限 1024 超限标记 truncated
- [x] 前缀展开(同一上限);单字 CJK 展开(包含匹配,见 D5)
- [x] 差分测试(规格 11.2):testutil 固定种子语料(齐夫词表+中文+标识符)
      + 独立暴力 oracle;5 种子 × 3 分段配置(单段/多段/合并后)× 40 随机查询,
      结果集与 Total 完全一致;Top-K 分数误差 < 1e-9(TestDifferentialScoring)
- [x] 标准用例:搜索/引擎/擎 命中"全文搜索引擎";Go语言并发 混排命中;
      "http request" 短语命中 parseHTTPRequest;snake 子词单独检索(TestCriteria)
- [x] FuzzLexer:任意输入不 panic,错误类型必须为 SyntaxError

实测:`go test -race ./...` 全部通过;FuzzLexer 15s 无 crash。
修复记录(全部由差分测试暴露):
1. termIter 误把字段间 OR 写成 AND(仅 56/150 文档命中);
2. 迭代器构造时预定位导致每段首文档被跳过(统一惰性初始化约定);
3. 得分逐字段饱和(BM25F 应先跨字段求和 tfw 再一次饱和);
4. OR 查询得分语义(非命中分支不得贡献分数)→ 递归求值重构;
5. decodeStoredDoc 用压缩长度当解压长度截断了存储正文;
6. 多段查询 CJK 部分经 raw 重分析引入错误 bigram(改用 part.terms);
7. 操作符列号记在推进之后;
8. 查询生成器按字节切 CJK 前缀产生非法 UTF-8(改 rune 切)。

## M6 排序与高级匹配 — ✅ 完成

- [x] BM25F(默认参数与规格一致,Params 可配置覆盖);df=max(字段 df) 文档化近似,
      N/df 含已删除未合并文档(Lucene 语义)
- [x] 跨段汇总统计后打分;同语料不同分段得分一致(差分测试三种分段配置强制)
- [x] Explain:JSON 序列化 + RenderText 文本树;包含 N/df/idf/各字段 tf·len·avglen·
      w·b·tfw/权重/boost/最终得分;Σ贡献 = 总分(含 recency 开启,测试强制 <1e-9)
- [x] 前缀展开(上限 1024,truncated 标记)、模糊(共享 DP 行剪枝)、recency 加成
      (默认关闭,开启后排位变化有测试)
- [x] snippet 包:命中密度最高窗口 + rune 对齐高亮(拉丁/中文/标识符均有测试)
- [x] 差分测试覆盖全部语法(布尔/短语/邻近/前缀/模糊/字段/过滤)与打分比对(1e-9)

实测:`go test -race ./...` 通过;TestExplainSumEqualsFinal 覆盖 8 类查询 ×
recency 开启场景。

## 第 14 节完成标准自检

(全部里程碑完成后逐项填写实际命令输出摘要。)
