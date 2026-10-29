# 仓库约定

## 结构

`DESIGN.md` 是唯一的设计依据。实现与文档冲突时，先改 `DESIGN.md` 再改代码，不要只在代码里悄悄偏离。

| 目录 | 职责 | 边界 |
| --- | --- | --- |
| `cmd/switchboard` | 进程入口 | 只调 `cli.Run` |
| `internal/cli` | 子命令、参数、输出 | 不直接连数据库、不发上游请求 |
| `internal/config` | 路径与配置文件 | 不含业务状态 |
| `internal/store` | SQLite、迁移、读写 | 上游凭据只在 `secret` 解密后进入这里 |
| `internal/secret` | 凭据加解密 | 不碰数据库连接 |
| `internal/account` | 账号池、冷却、选号 | 选号是纯逻辑，输入是候选集与时间 |
| `internal/gateway` | 入站 HTTP、上游转发 | 唯一发上游请求的地方 |
| `internal/translate` | 协议编解码 | 纯函数，不读环境、不发网络 |
| `internal/usage` | 汇总与花费估算 | 不写请求记录 |

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

三份文档面向不同读者，不互相代替：

| 位置 | 读者 | 内容 |
| --- | --- | --- |
| `DESIGN.md` | 维护者 | 设计与取舍的唯一依据；实现与它冲突时先改它 |
| `README.md` | 仓库访客 | 装、跑、常用命令、已知边界 |
| `docs-site/` | 使用者 | 站点文档：安装、配置、接入客户端、排障 |

### 什么时候改哪份

改代码时同一次提交里把对应文档改掉，不要留到以后补：

| 改动 | 要改的文档 |
| --- | --- |
| 命令、参数、输出列、`--json` 字段、退出码 | `DESIGN.md` 第 4 节、`README.md` 对应段、`docs-site/content/docs/reference/cli.mdx` |
| 协议、转发、错误形状、已知边界 | `DESIGN.md` 第 5、7 节、`README.md`「已知边界」、`docs-site/content/docs/using/gateway.mdx` |
| 厂商、路由、附加头、模型同步与探活 | `DESIGN.md` 5.7、5.8、5.10 与第 6 节、`docs-site/content/docs/configure/*`、`README.md` |
| 数据库结构或新增迁移 | `DESIGN.md` 第 6 节（含迁移编号） |
| 客户端接入方式（路径、鉴权头、SDK 示例） | `docs-site/content/docs/using/clients.mdx`、`README.md` |
| 需要使用者动手的变化（换 key、重新探活、迁移） | `docs-site/content/docs/reference/faq.mdx` 或 `docs-site/content/docs/getting-started/*` |
| 非显然的坑 | `LESSONS.md` 索引 + `lessons/<yyyy-mm-dd>-<topic>.md` |

### 文档站

- Fumadocs + Next.js，只在本机跑：`make docs`（127.0.0.1:3000，改文件热更新）、`make docs-build`（生产构建，顺带类型检查）、`make docs-install`。Node 依赖只在 `docs-site/` 内，`make check` 不包含站点。
- 页面手写在 `docs-site/content/docs/`，侧边栏顺序在各级 `meta.json`。新增页面要同时加 `meta.json` 条目，否则页面存在但侧边栏里进不去。
- 站点面向使用者：`DESIGN.md`、`LESSONS.md` 与 `lessons/` 的内容不进站点，站点也不写开发过程与设计取舍。
- 示例输出要脱敏（token、key、账号名用占位），不写本机绝对路径与用户名；`--json` 示例保持真实键名。
- 能用表格或列表表达的不写段落：参数、取值、优先级、字段来源、边界条件一律上表或列项，段落只用于讲“为什么”。
- 改完跑 `make docs-build`：它同时做类型检查与生产构建。`node_modules`、`.next`、`.next-build` 已在 `.gitignore` 里，不入库。
- `/llms.txt` 与 `/llms-full.txt` 由 `content/docs` 自动生成（`docs-site/app/llms.txt/route.ts`），页面改了它们就跟着变，不要手写这两个地址下的内容。

## 不做

多租户、配额、充值、网页后台、桌面菜单栏、逆向接口。这些是 `DESIGN.md` 第 9 节列的非目标，实现时不顺手加。
