# 仓库约定

## 结构

`DESIGN.md` 是唯一的设计依据。实现与文档冲突时，先改 `DESIGN.md` 再改代码，不要只在代码里悄悄偏离。

| 目录 | 职责 | 边界 |
| --- | --- | --- |
| `cmd/lax` | 进程入口 | 只调 `cli.Run` |
| `internal/cli` | 子命令、参数、输出 | 不直接连数据库、不发上游请求 |
| `internal/config` | 路径与配置文件 | 不含业务状态 |
| `internal/store` | SQLite、迁移、读写 | 上游凭据只在 `secret` 解密后进入这里 |
| `internal/secret` | 凭据加解密 | 不碰数据库连接 |
| `internal/account` | 账号池、冷却、选号 | 选号是纯逻辑，输入是候选集与时间 |
| `internal/gateway` | 入站 HTTP、上游转发 | 唯一发上游厂商请求的地方 |
| `internal/pluginfetch` | 从 GitHub 下载并解出插件目录 | 不校验 manifest、不读数据库与配置、不安装 |
| `internal/translate` | 协议编解码 | 纯函数，不读环境、不发网络 |
| `internal/timerange` | 命令行时间范围解析 | 不读配置、不连数据库 |
| `internal/usage` | 用量金额格式化 | 不读环境、不写请求记录 |

## 代码

- 注释与命令行文案用中文，标识符与提交信息用英文。
- 依赖，只有在标准库确实做不了时才加，并说明理由写进 `DESIGN.md`。
- 错误信息写清楚发生了什么、下一步做什么，不要只抛上游原文。
- 命令行输出默认给人看，`--json` 给脚本；两种输出不能混在一起。

## 测试

- `translate` 的每条映射都要有对拍用例，输入与期望输出放在 `testdata/` 里。
- 上游一律用 `httptest` 假服务，测试不连真实厂商。
- 改数据库结构必须新增迁移文件，不改已提交的迁移。
- 交付前跑 `make check`。

## 发版

远程 `origin` 是公开仓库 `https://github.com/sumwai/lax`。发版信息的事实来源是 `v*` tag，详细规则见 `DESIGN.md` 5.18 与 `README.md`「构建」。

- 版本用 SemVer，tag 写成 annotated tag `vMAJOR.MINOR.PATCH`（例如 `v0.1.0`）。1.0 之前破坏性改动升 MINOR，其余升 PATCH。
- 只有 `v*` tag 参与版本推导。`backup-*` 是本地备份标记，不是发布版本，不要推送，也不要拿来发版。
- 版本号在构建时由 `-X lax/internal/cli.version=<版本>` 注入，源码里的 `0.1.0-dev` 只是默认值；不要手工改这个默认值来发版。`make build` 用 `git describe --tags --match 'v*' --always --dirty` 推导版本。
- 发版前确认工作区干净并跑过 `make check`。本地先用 `make dist VERSION=vX.Y.Z` 验证四个发布包与 `dist/checksums.txt`，再打 tag：`git tag -a vX.Y.Z -m "lax vX.Y.Z"`，推送 `git push origin master vX.Y.Z`。
- `.github/workflows/ci.yml` 在 `master` 与 pull request 上跑 `make check`；推送 `v*` tag 会触发 `.github/workflows/release.yml`，由 `make dist` 生成 Linux/macOS 的 amd64、arm64 包并创建 GitHub Release，随后把正式 tag 的 Linux amd64 包自动部署到 `aliyun-hongkong`（带 `-` 的预发布 tag 不部署）。发布不覆盖 Windows：`serve` 与 `plugin reload` 依赖 Unix 信号。
- 已推送的 tag 不移动、不删除、不重打。发错了就发新的 PATCH 版本。Release 完成后下载产物，核对 `checksums.txt` 与 `lax version` 输出。

## 提交

Conventional Commits：`feat(gateway): …`、`fix(translate): …`、`docs: …`、`test: …`。

提交正文写三件事：改了什么、为什么这样改（实测数据或约束）、怎么验证的。不写过程记录。

提交信息与文档正文只写代码、配置与行为的事实：不写里程碑或批次编号（`M7`、`M8` 这类）、子代理名、审查意见，也不指代对话里的轮次与过程。要指代别的改动，用提交哈希、文件路径或符号名。`DESIGN.md` 第 7 节的里程碑表格是设计内容，照旧保留。

主语英文、一行、不超过 72 字。正文一段一行，不手工折行，段间空行隔开。

## 记录踩坑

审查或排查中发现的、以后不容易复原原由的问题，写进 `LESSONS.md` 的索引与 `lessons/<yyyy-mm-dd>-<topic>.md`，正文用英文。

每条要写清：发现日期、发现方式（哪个提交或哪次审查、哪次线上故障）、当时的症状、为什么此前没被发现、最终决定与代价、怎么验证、修复提交哈希。修复还没落地时状态写 `fix in progress`，落地后补齐哈希。

`LESSONS.md` 只在表格前留一段说明，其余内容全部放进 `lessons/` 的正文，不要在两处重复。

## 文档

两份文档面向不同读者，不互相代替：

| 位置 | 读者 | 内容 |
| --- | --- | --- |
| `DESIGN.md` | 维护者 | 设计与取舍的唯一依据；实现与它冲突时先改它 |
| `README.md` | 仓库访客 | 装、跑、常用命令、已知边界 |

### 什么时候改哪份

改代码时同一次提交里把对应文档改掉，不要留到以后补：

| 改动 | 要改的文档 |
| --- | --- |
| 命令、参数、输出列、`--json` 字段、退出码 | `DESIGN.md` 第 4 节、`README.md` 对应段 |
| 协议、转发、错误形状、已知边界 | `DESIGN.md` 第 5、7 节、`README.md`「已知边界」 |
| 厂商、路由、附加头、模型同步与探活 | `DESIGN.md` 5.7、5.8、5.10 与第 6 节、`README.md` |
| 数据库结构或新增迁移 | `DESIGN.md` 第 6 节（含迁移编号） |
| 客户端接入方式（路径、鉴权头、SDK 示例） | `README.md` |
| 非显然的坑 | `LESSONS.md` 索引 + `lessons/<yyyy-mm-dd>-<topic>.md` |

## 不做

多租户、配额、充值、网页后台、桌面菜单栏、逆向接口。这些是 `DESIGN.md` 第 9 节列的非目标，实现时不顺手加。
