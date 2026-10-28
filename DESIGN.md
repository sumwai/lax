# switchboard 设计

## 1 这是什么

本机上的模型总机。手上有几家模型的账号，客户端只连 `http://127.0.0.1:3425`，由 switchboard 挑账号、翻协议、记用量。

与同类产品的差别：

- 不是多租户服务。没有用户、充值、额度与网页后台。一个进程、一个 SQLite 文件、一份配置。
- 不是图形应用。所有操作都是子命令，输出能用 `--json` 解析，方便写脚本。
- 服务端只在回环地址上监听，默认不对外开端口。

## 2 第一版的范围

| 维度 | 决定 | 理由 |
| --- | --- | --- |
| 使用范围 | 单人单机 | 单进程加本地文件就够，省掉鉴权与租户隔离 |
| 账号类型 | 只支持粘贴 API key | 订阅账号要走浏览器登录与 token 刷新，受上游条款约束，风险和工作量都不适合放进第一版 |
| 客户端配置改写 | 支持，一条命令改，留备份 | 手工改各客户端配置是最常见的出错点 |
| 入站协议 | Anthropic Messages 一个入口 | agent 类客户端主要说这个协议；其余协议在架构上留位置，不在第一版实现 |
| 存储 | SQLite 单文件 | 不装数据库服务，事务与查询都够用 |
| 界面 | 纯命令行 | 用量由 `usage` 与 `log` 子命令查看 |
| 计费 | 不做 | 没有第三方付款方，只记录 token 与估算花费 |

协议矩阵。行列都按线协议算，第一版只填一条入站：

| 入站 | 出站 anthropic | 出站 openai-chat | 出站 openai-responses | 出站 gemini |
| --- | --- | --- | --- | --- |
| anthropic | 直通 | 翻译（M2–M3） | 未实现 | 未实现 |
| openai-chat | 未实现 | 未实现 | 未实现 | 未实现 |
| openai-responses | 未实现 | 未实现 | 未实现 | 未实现 |
| gemini | 未实现 | 未实现 | 未实现 | 未实现 |

“直通”指不改报文，只换上游地址与凭据；仍要解析用量字段。

## 3 用起来是这样

```bash
# 1 初始化。建配置目录与数据库，重复执行不会覆盖已有配置
$ switchboard init
配置 /home/someone/.config/switchboard/config.json
数据库 /home/someone/.local/share/switchboard/switchboard.db

# 2 加账号。key 加密后入库，不进配置文件
$ switchboard account add deepseek-1 \
    --provider deepseek \
    --key sk-xxxxxxxx \
    --models deepseek-v4-pro,deepseek-v4-fast
已添加 deepseek-1

# 3 看一眼
$ switchboard account ls
名称        厂商       协议          模型                      状态
deepseek-1  deepseek   openai-chat   deepseek-v4-pro,…（2）    正常

# 4 起网关。前台运行，Ctrl-C 退出
$ switchboard serve
监听 127.0.0.1:3425
入站 /v1/messages（anthropic）
上游 1 个账号，3 个模型

# 5 让客户端指过来。第一条只打印要改的内容，--apply 才写文件
$ switchboard agent use claude-code --model deepseek-v4-pro
将修改 /home/someone/.claude/settings.json
  env.ANTHROPIC_BASE_URL=http://127.0.0.1:3425
  env.ANTHROPIC_AUTH_TOKEN=switchboard-local
  env.ANTHROPIC_MODEL=deepseek-v4-pro
加 --apply 写入，原文件备份为 settings.json.switchboard.bak

$ switchboard agent use claude-code --model deepseek-v4-pro --apply
已写入，备份 settings.json.switchboard.bak

# 6 观察
$ switchboard log tail -n 5
时间     入站        模型              账号        状态  输入  输出  耗时
14:02:11 anthropic  deepseek-v4-pro  deepseek-1  200   1831   402   1.8s
14:01:52 anthropic  deepseek-v4-pro  deepseek-1  429   0      0     0.2s  冷却 60s

$ switchboard usage --since 7d --by account
账号        请求   输入 token  输出 token  估算花费
deepseek-1  214    412330      98421       ¥1.87

# 7 排障。手工打一条，打印选号过程与上游原始响应
$ switchboard call "hi" --model deepseek-v4-pro --debug
选中 deepseek-1（priority 100，无冷却）
→ POST https://api.deepseek.com/chat/completions
← 200 text/event-stream
```

## 4 命令表

全局参数：`--json` 可以写在子命令前后任意位置；`--config <路径>` 与 `--db <路径>` 必须写在子命令之前。

| 命令 | 参数 | 行为 | 输出 |
| --- | --- | --- | --- |
| `init` | `--force` | 建配置目录与数据目录，写默认配置；`--force` 覆盖 | 两个路径 |
| `serve` | `--addr`、`--log-level` | 前台起网关，注册入站路由，启动时探活所有账号 | 启动摘要，之后按请求打日志 |
| `account add` | `<名称> --provider --key [--protocol] [--base-url] [--models] [--priority]` | 加密存 key，按厂商补默认 base-url 与协议 | 新账号名称 |
| `account ls` | `--json` | 列账号、模型数、是否处在冷却 | 表格 |
| `account rm` | `<名称>` | 删账号，关联的请求记录保留 | 确认一行 |
| `account test` | `<名称>` | 用最小请求探活，更新冷却状态 | 状态码与耗时 |
| `model ls` | `--json` | 按模型名汇总可用账号与计价来源 | 表格 |
| `agent ls` | `--json` | 支持的客户端、各自的配置文件、是否已指向本网关 | 表格 |
| `agent use` | `<客户端> [--model] [--apply] [--print]` | 默认只打印差异；`--apply` 写文件并备份 | 差异或确认 |
| `agent revert` | `<客户端>` | 从备份还原 | 确认一行 |
| `usage` | `--since`、`--by account\|model\|day` | 汇总请求数、token、估算花费 | 表格 |
| `log tail` | `-n`、`--follow` | 最近的请求记录；`--follow` 跟随 | 表格 |
| `call` | `[提示词] --model [--protocol] [--debug] [--json]` | 绕过客户端直连本进程，打印选号过程 | 响应，或 debug 轨迹 |
| `doctor` | 无 | 检查配置文件、数据目录、数据库、端口 | 逐项 ok / fail |
| `version` | 无 | 版本号 | 一行 |

退出码：`0` 成功；`1` 运行失败（含 `doctor` 有检查未通过、`call` 非 2xx）；`2` 用法错误。
错误信息写 stderr，`--json` 时结果写 stdout，两者不混在一起。

## 5 程序内部做什么

### 5.1 进程与文件

| 路径 | 内容 |
| --- | --- |
| `$XDG_CONFIG_HOME/switchboard/config.json` | 监听地址、数据库路径，权限 0600 |
| `$XDG_DATA_HOME/switchboard/switchboard.db` | 账号、请求记录、客户端配置状态 |
| `$XDG_DATA_HOME/switchboard/secret.key` | 加密账号凭据的对称密钥，权限 0600 |

配置只有两项，其余状态都在数据库里。改状态一律走子命令，不手改库。

### 5.2 请求生命周期

1. 收到入站请求，记下协议、模型名、开始时间。
2. 按模型名取候选账号：`enabled=1`、模型列表包含该模型、冷却已过。
3. 按 `priority` 升序，同一会话（见 5.3）优先沿用上次的账号。
4. 解密凭据，按出站协议构造上游请求。同协议直通，跨协议走翻译层。
5. 发上游请求，读响应头。上游返回 429、5xx 或超时，且尚未向客户端写出任何字节时，把该账号打入冷却，换下一个候选。
6. 已开始向客户端输出后不再换号；上游中断则向客户端发一个错误事件并结束本次请求。
7. 解析响应里的用量字段，写一行请求记录。
8. 所有候选都失败，返回 503，响应体带最后一次上游错误的摘要。

### 5.3 选号与失败转移

- 候选集合由模型名决定。模型名用请求里的 `model` 字段原文匹配，不做别名归一（第一版）。
- 冷却：429 起始 60 秒，5xx 起始 10 秒，连续失败翻倍，上限 30 分钟；`account test` 成功即清除。
- 会话粘性：请求头带 `x-switchboard-session` 或 `metadata.user_id` 时，同一个值在一小时内粘同一个账号；否则按 `priority` 轮转。
- 凭据错误（401、403）不算临时故障，直接返回客户端，并把账号标记为 `enabled=0` 提示人工处理，避免整池被打空。

### 5.4 翻译规则：anthropic 入 → openai-chat 出

| anthropic | openai-chat |
| --- | --- |
| `system` 字符串或块数组 | 前置 `role=system` 消息，块数组按顺序拼成一个字符串 |
| `messages[].role=user` + `text` 块 | `role=user`，`content` 为字符串 |
| `messages[].role=assistant` + `text` 块 | `role=assistant` |
| `tool_use` 块 | `tool_calls[]`，`arguments` 由 `input` 序列化 |
| `tool_result` 块 | 独立 `role=tool` 消息，`tool_call_id` 取 `tool_use_id` |
| `image` 块（base64） | `image_url` 的 data URI |
| `tools[].input_schema` | `tools[].function.parameters` |
| `tool_choice` | `tool_choice`，`any` 映射为 `required` |
| `max_tokens` | `max_tokens` |
| `stop_sequences` | `stop` |
| `temperature`、`top_p` | 同名 |
| 响应 `stop_reason=end_turn` | `finish_reason=stop` |
| `stop_reason=tool_use` | `finish_reason=tool_calls` |
| `stop_reason=max_tokens` | `finish_reason=length` |
| `usage.input_tokens` / `output_tokens` | `usage.prompt_tokens` / `completion_tokens` |

流式映射（SSE）：

| 上游 chat.completion.chunk | 下发给客户端 anthropic 事件 |
| --- | --- |
| 首块 | `message_start`（含空 content） |
| `delta.content` 首个非空 | `content_block_start(index=0, text)` |
| 后续 `delta.content` | `content_block_delta(text_delta)` |
| `delta.tool_calls[i]` 首个 | `content_block_start(tool_use)` 并分配 index |
| `delta.tool_calls[i].function.arguments` | `content_block_delta(input_json_delta)` |
| `finish_reason` | 关闭所有 block，发 `message_delta`（含 usage），再发 `message_stop` |
| 上游错误帧 | `event: error`，带映射后的 type 与 message |

未在表内的字段一律丢弃，不猜。新增映射必须同时补一条对拍用例。

### 5.5 请求记录

每次尝试写一行，包括失败与换号前的尝试。字段：时间、入站协议、出站协议、请求模型名、实际模型名、账号 id、HTTP 状态、是否成功、输入 token、输出 token、缓存读、缓存写、耗时、错误摘要。

估算花费需要价格表；价格表第一版放在账号上（`--price-in / --price-out`，单位每百万 token），缺省则花费记为 null，不猜价格。

### 5.6 客户端配置改写

第一版只处理 Claude Code，写 `~/.claude/settings.json` 的 `env` 下三个键：

| 键 | 值 |
| --- | --- |
| `ANTHROPIC_BASE_URL` | 配置里的监听地址 |
| `ANTHROPIC_AUTH_TOKEN` | `switchboard-local`（本机网关不校验，客户端要求非空） |
| `ANTHROPIC_MODEL` | 指定模型 |

规则：默认只打印差异，`--apply` 才写；写前把原文件复制为 `<文件名>.switchboard.bak`；只动上表三个键，其它键原样保留；`agent revert` 从备份还原，若原来没有这个配置文件，则把它删掉。其它客户端按同一份结构在后续里程碑加表项，改法与备份规则不变。

### 5.7 凭据

AES-256-GCM 加密后入库，密钥在 `secret.key`。这个方案防的是「备份文件、误贴日志、共享磁盘快照」这类泄漏，不防同一台机器上以同一用户身份运行的程序。文档里明确写出这一点，不暗示更强保护。

## 6 数据模型

```sql
CREATE TABLE accounts (
  id             INTEGER PRIMARY KEY,
  name           TEXT    NOT NULL UNIQUE,
  provider       TEXT    NOT NULL,
  protocol       TEXT    NOT NULL,
  base_url       TEXT    NOT NULL,
  key_cipher     BLOB    NOT NULL,
  models         TEXT    NOT NULL DEFAULT '[]',
  price_in       REAL,
  price_out      REAL,
  priority       INTEGER NOT NULL DEFAULT 100,
  enabled        INTEGER NOT NULL DEFAULT 1,
  cooldown_until INTEGER,
  last_error     TEXT,
  created_at     INTEGER NOT NULL
);

CREATE TABLE requests (
  id              INTEGER PRIMARY KEY,
  ts              INTEGER NOT NULL,
  inbound         TEXT    NOT NULL,
  outbound        TEXT    NOT NULL,
  model_requested TEXT    NOT NULL,
  model_used      TEXT    NOT NULL,
  account_id      INTEGER,
  http_status     INTEGER NOT NULL,
  ok              INTEGER NOT NULL,
  in_tokens       INTEGER NOT NULL DEFAULT 0,
  out_tokens      INTEGER NOT NULL DEFAULT 0,
  cache_read      INTEGER NOT NULL DEFAULT 0,
  cache_write     INTEGER NOT NULL DEFAULT 0,
  latency_ms      INTEGER NOT NULL DEFAULT 0,
  error           TEXT
);

CREATE TABLE agents (
  name        TEXT PRIMARY KEY,
  config_path TEXT NOT NULL,
  backup_path TEXT,
  model       TEXT,
  applied_at  INTEGER
);

CREATE INDEX requests_ts ON requests(ts DESC);
```

迁移用 `internal/store/migrations/*.sql`，按文件顺序执行，`PRAGMA user_version` 记录已执行版本。

## 7 里程碑

| 里程碑 | 内容 | 验收 |
| --- | --- | --- |
| M0 | 骨架：`init`、SQLite 迁移、`serve` 起 `/healthz`、`doctor` 完整 | `make check` 通过，`doctor` 全 ok |
| M1 | 非流式直通：anthropic → anthropic，`account` 四个子命令、`call`、`log tail` | `call` 打真实账号拿到回复，`log tail` 有记录 |
| M2 | 翻译：anthropic → openai-chat，非流式，含工具调用 | 对拍用例覆盖 5.4 表里每一行 |
| M3 | 流式：SSE 双向翻译 | 客户端能流式拿到 token 与工具调用 |
| M4 | 多账号：冷却、失败转移、`usage`、`agent use/revert` | 两个账号时杀掉一个，请求自动落到另一个 |

M0 到 M4 已实现，`make check` 与 `go test -race` 通过，端到端由 `internal/cli` 的 `TestWalkingSkeleton` 用假上游验证：初始化、加账号、`call`、`log tail`、`usage`、`doctor` 全链路走通。上游是 `httptest` 假服务，真实厂商的连通性需要拿到 key 后自行 `account test` 验证。

已知未做，按已知行为处理：

- 冷却时长按单次失败计算，不跨请求累计连续失败次数。
- 估算花费依赖账号上的 `--price-in` 与 `--price-out`，没有价格时记为 0 并在 `usage` 末尾提示。
- 客户端配置改写只支持 Claude Code。
- 模型名不做别名归一，请求里的 `model` 必须与账号 `--models` 里的名字完全一致。

之后（不在第一版）：openai-chat / responses / gemini 入站，订阅账号登录，隐私遮蔽，配置档案（profiles），价格表内置。

## 8 代码结构

```
cmd/switchboard/        main，只做 os.Exit(cli.Run(...))
internal/cli/           子命令分发、参数解析、输出格式
internal/config/        路径解析与配置读写
internal/store/         SQLite 连接、迁移、账号与请求记录的读写
internal/secret/        凭据加解密
internal/account/       账号池、冷却、选号
internal/gateway/       HTTP 入站处理、上游转发、错误映射
internal/translate/     各协议编解码与互转（不碰网络，纯函数）
internal/usage/         用量汇总与花费估算
internal/agentcfg/      客户端配置读写与备份还原
```

约束：`translate` 只做结构体到结构体的转换，不读环境、不发请求，便于用对拍用例覆盖；`gateway` 是唯一发上游请求的地方（`account test` 的探活也走 `gateway.Probe`）；`cli` 不直接连数据库，只调 `store` 与 `account`。

依赖：只有 `modernc.org/sqlite`（纯 Go 实现，不需要 cgo，交叉编译不受影响），其余全部用标准库。命令行解析用 `flag`，不引入第三方 CLI 框架。

## 9 非目标

- 多租户、配额、充值、开票。
- 网页后台与桌面菜单栏。
- 逆向接口、把订阅网页版包装成 API。
- 跨机部署与集群。

## 10 需要外部信息才能定的点

- 各家上游的默认 base-url、模型名清单、是否兼容 `openai-chat`。
- 各家价格，用于估算花费。缺价格就记 null。
- Claude Code 之外的客户端配置文件格式（Codex、Gemini CLI、OpenCode），逐个里程碑补。
