# Lantern

零依赖、可解释的本地全文搜索引擎(Go 标准库实现,Go ≥ 1.22)。

- **分段倒排索引**:不可变段 + manifest 原子提交 + 崩溃安全恢复 + MVCC 快照 + 分层合并
- **BM25F 排序**:跨段统计、字段加权、可选 recency 加成
- **查询语言**:`AND/OR/NOT`、`"短语"`、`"邻近"~n`、`字段:`、`前缀*`、`模糊~`、`ext:/size:/mtime:` 过滤
- **Explain**:每条结果的得分逐项拆解,Σ贡献与总分之差 < 1e-9(测试强制)
- **Why-not**:解释某文档为何未命中(忽略规则/编码/词项缺失的最近词/短语顺序与间隔/NOT 触发/过滤失败…),并给出"移除哪个子句可命中 + 排名估计"
- **中文与代码友好**:CJK 二元组 + 单字包含匹配、camelCase/snake_case 标识符子词、全角折叠
- **HTTP 界面**:内嵌单文件 UI(120ms 即搜、得分条形图、why-not 面板),零外部请求

## 安装

```bash
go build -o lantern ./cmd/lantern      # 或 make build
```

无任何第三方依赖;`go.mod` 仅有 module 与 go 指令。

## 快速上手

```bash
lantern index docs                  # 扫描并增量索引(--workers N --max-size 2MB --no-store-body)
lantern search "搜索引擎"           # 中文二元组命中
lantern search "\"http request\""   # 短语命中 parseHTTPRequest(标识符子词)
lantern search "title:alpha pre* fuzzy~ ext:md size:<1mb mtime:>2025-01-01"
lantern search "release" --explain  # 输出得分逐项拆解
lantern why-not "release" docs/a.md # 解释 a.md 为何未命中
lantern stats                       # 文档数/词项数/段数/磁盘/平均字段长度
lantern check                       # 校验段 CRC 与 manifest 一致性
lantern compact                     # 强制合并为单段
lantern serve --addr 127.0.0.1:7700 # HTTP API + 内嵌网页界面
lantern watch docs --interval 2s    # 轮询增量更新(Ctrl-C 优雅退出)
```

索引目录默认 `./.lantern`,可用 `--index <dir>` 或 `LANTERN_DIR` 覆盖;
文档路径相对索引目录的父目录(基目录)。退出码:0 成功、1 运行错误、
2 用法/查询语法错误(带列号指示)。

## HTTP API

```
GET /api/search?q=&n=&explain=1
GET /api/why-not?q=&path=
GET /api/doc?path=        # 仅返回索引内已有路径
GET /api/stats
GET /healthz
```

## 设计取舍(详见 docs/ARCHITECTURE.md 与 DECISIONS.md)

- **段不可变 + manifest 原子提交**:rename manifest 即提交点;崩溃后重开
  必为旧版本或新版本之一(每步注入故障测试覆盖)。
- **打分与匹配分离**:迭代器树负责高效匹配(leapfrog/跳块),得分对命中
  文档按 plan 树递归求值——保证 OR 分支得分语义与差分 oracle 严格一致。
- **合并 = 存储文档重放分析**:避免跨段 docID 重映射;分析是确定性纯函数。
- **跨段统计后打分**:同一语料在不同分段配置下得分逐位一致(差分测试)。
- **JSON 元数据不加二进制 footer**:完整性由原子 rename + manifest 校验和
  + state_len 有效前缀分别保证。

## 差分测试与质量门禁

- 差分(oracle)测试:固定种子随机语料(齐夫词表 + 中文 + 标识符),
  5 种子 × 3 分段配置(单段/多段/合并后)× 40 随机查询,结果集与暴力
  扫描完全一致;Top-K 分数与暴力 BM25F 误差 < 1e-9。
- 崩溃安全:FaultFS 在提交协议每一步注入 Fail/Drop/断电截断,重开必须
  一致且 check 通过。
- fuzz:analysis/codec/query 各有 fuzz 目标。
- `make race` / `make cross`(windows/darwin/linux)/ `make fuzz-short` / `make bench`。

## 限制(非目标)

PDF 解析、向量/语义检索、分布式、多用户权限、正则搜索、GBK 等非
UTF-8 编码解码(计数跳过)、云服务、任何第三方库。
Windows 下 ANSI 高亮自动降级;目录 fsync 为 no-op(NTFS 元数据日志,
见 DECISIONS.md D2)。
