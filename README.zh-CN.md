# opendoc

[![CI](https://github.com/arcships/open-doc-cli/actions/workflows/ci.yml/badge.svg)](https://github.com/arcships/open-doc-cli/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.25-00ADD8.svg)](go.mod)

[English](README.md) | 简体中文

![opendoc — 把云端文档镜像成本地知识库](docs/assets/cloud-archive-banner.png)

**把授权范围内的 Notion 和飞书/Lark 文档单向镜像到本地，落成一棵只读的 Markdown 树，agent 直接读——查询时无 API、无凭证、无网络。**

## 为什么做 opendoc

你的知识在线上——个人笔记在 Notion，工作文档在飞书。而你的 agent 在本地机器上。它每次需要你写过的东西，都得跨过这道鸿沟走 API：管理凭证、翻页、扛限流，还要依赖平台搜索端点——而那些端点的召回方式从来不是为 agent 的检索习惯设计的。

与此同时，agent *最擅长*的检索界面早就存在：文件系统。grep、glob、读文件、顺链接、走目录——coding agent 天生就会，又快，还离线可用。

opendoc 从另一个方向填平这道鸿沟：**与其教 agent 操作各个平台，不如把文档搬到 agent 面前。**一条命令把授权范围内的全部文档镜像成本地 Markdown 树；从此"搜我的笔记"就只是一次 grep。

落到磁盘上的不只是一份缓存，而是你的**本地知识库**：多年积累的会议记录、设计文档、决策结论、读书笔记——原本散落在各个平台，如今汇成你自己磁盘上一座完整、可 grep 的图书馆。你的 agent 不再凭通用知识作答，而是基于*你的*知识作答。并且它是开放格式的纯 Markdown，以最彻底的方式属于你：任何工具都能读、离线可用、比任何平台都活得久。

opendoc 是"搬运工 + 图书管理员"，不是操作器：只负责把线上内容忠实地搬下来、组织好、保持新鲜；一切写操作仍发生在线上。交付物是磁盘上的一棵目录树，不是一个服务。

## 它凭什么好用

- **零集成消费**。无 SDK、无 MCP 往返、无常驻进程、无需要保温的索引。镜像就是 Grep/Glob/Read 天然能读的纯 Markdown——消费端什么都不用装，这本身就是特性。
- **查询时零成本**。同步之后，检索不需要网络、不需要凭证、不受限流。在飞机上也能用。
- **树本身就是检索信号**。路径由真实标题构成（`notion/2025 日本之旅/行程.md`），每个文件都带 frontmatter——稳定 ID、线上 URL、breadcrumb、时间戳——路径和文件头都是可 grep 的上下文，任何命中一步跳回线上原文。
- **忠实优先，有损留痕**。转换必然有损（画板、内嵌表格），但内容绝不静默丢失：每处降级都留下可读的占位内容、可下钻的 ID 和/或线上链接，并在同步报告里计数。
- **增量同步，无人值守**。日常 sync 只拉变更，千篇级文档库分钟内完成；`opendoc schedule` 挂上 launchd，镜像永远不超过几小时的新鲜度。
- **结构性安全**。镜像只读（本地改动下次 sync 被覆盖；要改内容，顺 frontmatter 的 URL 去线上改），opendoc 从不回写平台；删除检测带权限抖动保护——临时过期的 token 不会把你的镜像整库扫进回收站。
- **为 agent 调用而设计**。退出码确定、`--json` 结构化输出、绝不交互（`init` 除外）。doctor 的失败码（`F2-NOAUTH`、`N3-EMPTY`……）是稳定路由键，agent 可以据此自行修复环境。

## 架构

```
┌────────────────────────────────────────────────┐
│ 消费层：agent(Grep/Read) · 人(编辑器，次要)      │
│   引导物：SKILL.md · INDEX.md · frontmatter     │
├────────────────────────────────────────────────┤
│ 知识库：markdown 树 + assets 池                  │
├────────────────────────────────────────────────┤
│ 同步引擎：枚举→diff→拉取→素材→写盘→链接→索引     │
│ 状态：manifest.sqlite                           │
├────────────────────────────────────────────────┤
│ 平台适配器                                       │
│  notion: 官方 markdown 端点 + search 平铺枚举    │
│  feishu: 内嵌 lark 引擎 fetch + wiki/drive 枚举  │
└────────────────────────────────────────────────┘
```

核心不变量：**引擎对平台一无所知**，只认识 `adapter.Adapter` 接口。新增平台（语雀、Confluence…）= 新写一个实现该接口的包 + 在 CLI 装配处注册，引擎零改动。完整细节见 [docs/dev/architecture.md](docs/dev/architecture.md)。

## 镜像库布局

默认根 `~/.opendoc/`（可用 `--root` 或 `OPENDOC_ROOT` 改）：

```
~/.opendoc/
├── INDEX.md                   # 自动生成的全库目录树
├── assets/                    # 全局素材池，sha256 前 2 位分桶
├── notion/                    # 有子页面 = 目录 + README.md；叶子 = 单文件
│   └── …                      # database → 目录 + _index.md + 每行一个子目录
├── feishu/
│   ├── wiki-<空间名>/          # 每个 wiki space 一棵子树
│   └── drive-<空间名>/         # drive 文件夹树
└── .internal/                 # 内部状态（manifest.sqlite、config.toml、trash、logs）
                               # 检索与浏览工具应忽略
```

每个 `.md` 头部都有 YAML frontmatter：`id`（平台稳定 ID）、`source`、`type`、`url`（跳回线上）、`title`、`breadcrumb`（线上祖先路径）、`updated`、`synced`；database 行还带 `properties`。

## 快速开始

opendoc 以 agent plugin 的形式分发——同一个包，双 manifest 同时适配两家 agent——通过 [arcships/plugins](https://github.com/arcships/plugins) marketplace 目录仓安装。安装时只会稀疏拉取 plugin 包（`plugin/`），本仓库的源码不会落到用户机器上。装好后直接使用（例如让 agent 搜索你的笔记，首次使用它会引导你完成初始化）。引擎二进制不随仓库提交；skill 首次使用时会发现二进制缺失，在征得你同意后从 GitHub releases 下载对应平台的构建（`opendoc-<os>-<arch>`），并做 sha256 校验。

**Claude Code**——在 `claude` 会话内：

```
/plugin marketplace add arcships/plugins
/plugin install opendoc@arcships
```

**Codex**——在终端里：

```bash
codex plugin marketplace add arcships/plugins
codex plugin add opendoc@arcships
```

**开发者**——从源码构建引擎，并把本地工作树作为 marketplace 安装（本仓库自带一份名为 `arcships-dev` 的开发目录，不会与正式 marketplace 重名冲突）：

```bash
./scripts/build-skill.sh                      # 构建 plugin/bin/opendoc-dev
claude plugin marketplace add "$(pwd)"        # 或：codex plugin marketplace add "$(pwd)"
# 然后安装 opendoc@arcships-dev
```

装好后一切由 agent 驱动——CLI 也可以独立使用：

```bash
opendoc init                # 交互式初始化，写入 .internal/config.toml
opendoc sync                # 首轮全量镜像，之后增量
```

## 命令

| 命令 | 作用 |
|---|---|
| `opendoc init` | 交互式初始化，生成 `.internal/config.toml`。 |
| `opendoc sync` | 全量/增量同步（首轮全量，之后增量 + 对账轮）。 |
| `opendoc status` | 镜像库概况：上次 sync、文档数、pending 素材等。 |
| `opendoc doctor` | 环境体检：配置、凭证、平台可达性，输出结构化失败码（`--json`）。 |
| `opendoc resolve <id\|url\|path>` | 在稳定 ID / 线上 URL / 本地路径三者间互查。 |
| `opendoc schedule` | 管理无人值守的 launchd 任务（`com.arcships.opendoc.sync`），按计划运行 `opendoc sync`。 |

退出码确定、输出结构化、绝不交互，为 agent 调用而设计。未初始化时 `sync`/`status`/`resolve` 以退出码 3（`ExitNotInitialized`）指向 onboarding 文档。

## 非目标

- **不做双向同步**：opendoc 只往下拉，从不回写 Notion 或飞书。
- **不做实时**：按需或按计划轮询，没有实时订阅。
- **不镜像权限**：评论、版本历史、访问控制都不复刻。
- **不做向量索引**：几千篇文档以内，文件系统检索 + 引导文件就是完整的检索层；目录与 frontmatter 约定为将来加索引留了门。
- **落盘即明文**：谁能读这台机器就能读全部镜像内容。只在可信的个人机器上运行 opendoc。

## 文档

- [docs/dev/architecture.md](docs/dev/architecture.md) — 规范真相源：分层、一次 sync 从头到尾发生什么、Adapter 契约、每个包管什么。
- [docs/dev/README.md](docs/dev/README.md) — contributor 上手：构建、测试、从哪开始。
- [docs/dev/testing.md](docs/dev/testing.md) — 测试如何组织、mock 模式、fixture 红线。
- [docs/notion-properties-mapping.md](docs/notion-properties-mapping.md) — Notion properties → frontmatter 映射。
- [plugin/skills/opendoc/SKILL.md](plugin/skills/opendoc/SKILL.md) — plugin 内附带的 Agent Skill 引导。

## 许可证

Apache-2.0。版权归 arcships 所有。见 [LICENSE](LICENSE)。
