# PROGRESS

按 GOAL.md 第 13 节里程碑推进;每个里程碑通过质量门禁(gofmt / vet / test -race / 交叉编译)后提交。
本文件只记录真实进度,随里程碑推进增量更新。

## M0 脚手架 — 进行中

- [x] go.mod(仅 module+go 指令,零 require)
- [x] Makefile(build/test/race/fuzz-short/bench/demo/check/cross)
- [x] internal/fsx:FS 接口 + osFS + FaultFS(Fail/Drop/Truncate 三种注入模式)
- [x] fsx 单元测试
- [x] 文档骨架:README / docs/ARCHITECTURE / DECISIONS / BENCH / PROGRESS

门禁记录:(待首次运行)

## 第 14 节完成标准自检

(全部里程碑完成后逐项填写实际命令输出摘要。)
