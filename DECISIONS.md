# DECISIONS

记录规格歧义处的选择与理由(GOAL.md 执行规则:选"更简单且可测试"的方案)。

## D1. fsx.File 增加 Truncate;SyncFile 落在 File.Sync 上
规格列出的 FS 方法为 "Create/Open/Rename/Remove/SyncFile/SyncDir/ReadDir 等"。实现上常规写路径
持有打开句柄,`File.Sync()` 才是可靠的刷盘点;FS 仍提供 `SyncFile(path)` 便捷方法(打开-刷盘-关闭)。
`File` 额外暴露 `Truncate`,供 FaultFS 模拟"断电截断"(截断打开中的句柄)。

## D2. SyncDir 在 Windows 上为 no-op
Windows 无法对目录句柄 FlushFileBuffers(需要 GENERIC_WRITE 且语义不同)。`osFS.SyncDir` 在
`runtime.GOOS=="windows"` 时直接返回 nil;其他平台 open+fsync。NTFS 的元数据日志已保证目录项
持久性,崩溃安全测试在 Windows 上依然通过 FaultFS 的注入语义验证协议正确性。

## D3. 全角折叠采用最小折叠表
按 4.1 不引入 NFKC:仅折叠 U+FF01–U+FF5E → U+0021–U+007E(减 0xFEE0)与 U+3000 → U+0020。
覆盖全角 ASCII 与全角空格;其余全角字符(如全角片假名)不折叠。理由:可测试、可解释,
覆盖绝大多数中文输入法产生的全角英文/数字/标点。

## D4. 标识符"整体 token"只在 camelCase 类切分时输出
对 snake_case/kebab/dot 分隔的标识符,分词按非字母数字切分已自然产出子词且相邻(位置连续),
短语与单词条查询语义与"子 token"方案等价;不再额外输出拼接整体(如 "snakecasename"),
因为查询侧同样的分析器不会产生该整体,属不可查询的冗余词条。
camelCase/PascalCase/缩写(HTTPRequest→http,request)的切分不改变字节流,故整体 token
(raw 小写形式)与首子 token 共享 Pos,后续子 token 依次递增。

## D5. CJK 单字查询的"前缀匹配"实现为"包含匹配"
二元组词典中,目标字可能出现在二元组的首字或尾字(查"擎"命中"引擎")。若严格按词项前缀匹配
只能命中"擎X",遗漏"X擎"。因此单字 CJK 查询展开为词典中所有包含该字的二元组(上限与
前缀展开相同,1024 个,超限标记 truncated)。规格示例明确要求 擎→引擎,此为实现该示例的最简一致语义。

## D6. 语法错误的列号语义
错误位置定义为"导致错误的最小词法单元"的起始列(rune 列,1-based)。缺少右括号时报在最外层
未闭合的 `(` 处,并输出 `query:行:列: 消息` + 原文行 + `^` 指示符两行。规格示例为示意,本实现
保证格式一致(`query:1:C: ...`),列号指向需要补全的位置。

## D7. state.jsonl 为追加日志,manifest 记录有效前缀长度
规格要求 manifest.json + state.jsonl。为避免每次提交全量重写状态表,state.jsonl 采用追加写;
manifest.json 中记录 `state_len`(有效字节前缀)。启动恢复时截断无效尾部。条目为
JSON Lines(path → size/mtime/sha256/seg/docID 或 tombstone)。

(后续决定按 D 编号追加。)
