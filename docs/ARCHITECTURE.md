# ARCHITECTURE(骨架,随里程碑补全)

Lantern 是仅用 Go 标准库实现的本地全文搜索引擎:分段倒排索引 + BM25F + 查询语言 +
崩溃安全存储 + 增量索引,并提供 Explain(得分逐项拆解)与 Why-not(未命中诊断)两个特性。

## 包依赖(自上而下,禁止反向)

cmd/lantern → internal/cli → internal/server → internal/whynot → internal/query →
internal/rank → internal/snippet → internal/scan → internal/index → internal/codec →
internal/analysis → internal/fsx;internal/testutil 仅供测试导入。

## 存储格式

(各文件字节布局在 M3/M4 完成时在此完整记录。)

所有数据文件以 footer 结束:magic "LNTN"(4B)+ format version(u32 LE)+ 此前全部内容 CRC32(u32 LE)。

(待补:terms.dat/terms.idx/postings.dat/norms.dat/dv.dat/docs.dat/docs.idx/live_<gen>.bits/
manifest.json/state.jsonl 的精确布局。)
