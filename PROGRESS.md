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

## 第 14 节完成标准自检

(全部里程碑完成后逐项填写实际命令输出摘要。)
