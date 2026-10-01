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

## 第 14 节完成标准自检

(全部里程碑完成后逐项填写实际命令输出摘要。)
