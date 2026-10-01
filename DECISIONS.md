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

## D8. JSON 文档不附加二进制 footer
"所有数据文件以 footer 结束"应用于二进制数据文件(terms/terms.idx/postings/norms/dv/
docs/docs.idx/live_*.bits,共 8 类)。meta.json、manifest.json、state.jsonl 保持纯 JSON
(可读、可增量追加),完整性分别由:原子 rename 提交(manifest/meta)、manifest 内的
逐文件 CRC 校验和(段文件)、state_len 有效前缀(state)覆盖。`lantern check` 校验全部。

## D9. 墓碑历史文件(live_<gen>.bits)永久保留
删除提交写入新代位集后,旧代文件不再删除:文件仅 nDocs/8 字节量级,而 Windows 禁止
删除仍被句柄打开的文件(会阻塞读者打开期间的恢复与段目录操作)。段目录的完整文件
清单与校验和记录在 manifest,多余的历史文件不构成不一致。

## D10. FaultFS 截断注入语义 = 撕裂写 + 进程死亡
真实掉电发生在 manifest 提交点之前(进程死亡)。若截断注入后进程继续运行并提交了
引用损坏文件的 manifest,重开必然失败,那是注入模型不真实而非协议缺陷。因此
FaultFS 的 Truncate 注入在截断后令所有后续操作失败(模拟进程死亡),保证:
提交点之前的撕裂由"旧 manifest + 孤儿清理"吸收。Drop 注入仅对 rename/remove/
mkdirall/syncdir/syncfile/close 等可幂等跳过的操作开放,其余自动降级为 Fail。

## D11. merge 以"存储文档重放分析"实现
合并时只读取存活文档的存储字段(path/title/body/ext/size/mtime/sha256)并按路径
排序后重放 MemTable.Add(重新分析),而非转写倒排。理由:省去跨段 docID 重映射与
位置搬移逻辑;分析是确定性纯函数,重建结果与原索引逐 token 一致;CPU 可接受。
合并段的 storeBody = 任一来源段存储正文。

## D12. internal/testutil 的依赖方向
testutil 供各包 _test.go 导入(生产代码禁止导入),自身可依赖 analysis/query/rank
以实现随机语料与暴力 oracle。这不违反"包间依赖单向无环":引擎依赖链不变,
testutil 位于测试侧。

## D13. 短语与邻近语义
严格短语(无 ~):词项在同一字段内按顺序、位置严格相邻(pos 差 1)。
邻近 "a b"~S:各词项在单一字段内存在一组位置(每词一个、任意顺序),
使 max(pos)-min(pos) ≤ S + n - 1(n 为词数)。S=0 即"相邻但任意顺序"。
短语与邻近只作为匹配条件,贡献 0 分(规格 7.2)。

## D14. 模糊编辑距离按 UTF-8 字节计算
与词典字节序排序一致,前缀剪枝(共享 DP 行)才能正确工作;权重
1 - dist/len 的分母同样取字节长度。拉丁词为主时与 rune 语义一致。

## D15. 迭代器约定:构造不定位
所有 DocIterator 构造后处于"未定位"状态,首次 Next() 定位到第一个
匹配文档(Advance(target) 定位到第一个 ≥ target)。该约定避免了
"构造即预定位 + 调用方再 Next"造成的首文档丢失。

## D16. 匹配与打分分离
规格要求 DocIterator 带 Score(),但 OR 查询中"游标不在当前文档的
分支"无法安全求值(前向游标不可回退)。因此:迭代器树只负责匹配;
得分对每个命中文档按 plan 树递归求值(与差分 oracle 同构):
AND=Σ命中子树,OR=Σ命中分支,NOT/短语/过滤=0,词项=weight·idf·tfw/(k1+tfw)
(tfw 先跨字段求和再饱和)。Score() 仍实现于迭代器(AND 与单词项场景正确),
供规格接口与单元测试使用。

(后续决定按 D 编号追加。)
