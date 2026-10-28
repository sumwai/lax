# switchboard 设计

## 1 这是什么

本机上的模型总机。手上有几家模型的账号，客户端只连 `http://127.0.0.1:4567`，由 switchboard 挑账号、转协议、记用量。

入站收三种线协议，按路径区分：`POST /v1/messages` 是 anthropic，`POST /v1/chat/completions` 是 openai-chat，`POST /v1/responses` 是 openai-responses。出站协议取值同样是这三种，归属在模型上：`account_models.protocol` 有值就用它，为空则继承账号的 `protocol`（见 5.7）。入站协议与有效出站协议相同则直通，不同则经中间表示转换，九种组合全部可用。

与同类产品的差别：

- 不是多租户服务。没有用户、充值、额度与网页后台。一个进程、一个 SQLite 文件、一份配置。
- 不是图形应用。所有操作都是子命令，输出能用 `--json` 解析，方便写脚本。
- 服务端只在回环地址上监听，默认不对外开端口。

## 2 第一版的范围

| 维度 | 决定 | 理由 |
| --- | --- | --- |
| 使用范围 | 单人单机 | 单进程加本地文件就够，省掉鉴权与租户隔离 |
| 账号类型 | 只支持粘贴 API key | 订阅账号要走浏览器登录与 token 刷新，受上游条款约束，风险和工作量都不适合放进第一版 |
| 客户端配置改写 | 不做 | 程序只管网关与账号，各客户端里把 base URL 与 key 填成本机地址即可；改写别人家的配置文件不属于这里的职责 |
| 入站协议 | anthropic、openai-chat、openai-responses 三个入口，按路径区分 | 客户端说什么协议就应当能接什么：Claude Code 说 anthropic，Codex 说 responses，普通 OpenAI SDK 说 chat。只做一个入口等于把其余客户端挡在门外 |
| 存储 | SQLite 单文件 | 不装数据库服务，事务与查询都够用 |
| 界面 | 纯命令行 | 用量由 `usage` 与 `log` 子命令查看 |
| 计费 | 不做 | 没有第三方付款方，只记录 token 与估算花费 |

协议矩阵。行列都按线协议算，当前实现的是三种协议之间的九种组合：

| 入站 ＼ 出站 | anthropic | openai-chat | openai-responses | gemini |
| --- | --- | --- | --- | --- |
| anthropic | 直通 | 转换 | 转换 | 未实现 |
| openai-chat | 转换 | 直通 | 转换 | 未实现 |
| openai-responses | 转换 | 转换 | 直通 | 未实现 |
| gemini | 未实现 | 未实现 | 未实现 | 未实现 |

同行同列是“直通”，其余是“转换”。“直通”只需重写请求体里的 `model` 字段，其余字段与值原样带到上游，响应与流式帧按字节原样返回，用量另由该协议的 scanner 读出。“转换”走中间表示：请求与响应都只搬映射表里定义过的字段（见 5.4）。gemini 既没有入站路径，也没有可在账号上选的出站协议。

## 3 用起来是这样

```bash
# 1 初始化。建配置目录与数据库，重复执行不会覆盖已有配置
$ switchboard init
配置 /home/someone/.config/switchboard/config.json
数据库 /home/someone/.local/share/switchboard/switchboard.db

# 2 加账号。key 加密后入库，不进配置文件
$ switchboard account add deepseek-1 \
    --provider deepseek \
    --key sk-xxxxxxxx
已添加 deepseek-1，模型列表待同步

# 3 拉上游模型列表
$ switchboard model sync --account deepseek-1
账号        厂商      模型  新增  备注
deepseek-1  deepseek  47    47
下一步：switchboard model probe（每个模型发一条 max_tokens=1 的请求）

# 4 逐个探活，能用的才进选号。已经探过且结论还新鲜（默认 24h 内）的模型会跳过，不花钱
$ switchboard model probe --account deepseek-1
账号        模型              状态  HTTP  耗时   备注
deepseek-1  deepseek-v4-pro   可用  200   412ms
deepseek-1  deepseek-v4-fast  可用  200   388ms
deepseek-1  deepseek-r1       无权  403   121ms  套餐不含该模型
…（其余 44 行略）
deepseek-1：本次探活 47 个，跳过 0 个，可用 2，无权 45，不存在 0，失败 0；选号列表共 2 个
已把 2 个可用模型写进 deepseek-1，选号按这个列表走

# 5 看一眼
$ switchboard account ls
名称       厂商      协议         模型               优先级   状态
deepseek-1  deepseek  openai-chat  2 可用 / 47 同步  100      正常

# 6 起网关。前台运行，Ctrl-C 退出
$ switchboard serve
监听 127.0.0.1:4567
入站 /v1/messages（anthropic）
上游 1 个账号，2 个模型

# 7 在客户端里把地址指过来，例如 Claude Code：
#   ANTHROPIC_BASE_URL=http://127.0.0.1:4567
#   ANTHROPIC_AUTH_TOKEN=switchboard-local（值随意，本机不校验）
#   ANTHROPIC_MODEL=deepseek-v4-pro

# 8 观察
$ switchboard log tail -n 5
时间           协议                    模型              账号        状态   输入  输出  耗时   错误
01-02 14:02:11 anthropic→anthropic     deepseek-v4-pro  deepseek-1  200    1831  402   1800ms
01-02 14:01:52 anthropic→openai-chat  deepseek-v4-pro  deepseek-1  429 ✗  0     0     200ms  冷却 60s

$ switchboard usage --since 7d --by account
账号        请求   输入 token  输出 token  估算花费
deepseek-1  214    412330      98421       ¥1.87

# 9 排障。手工打一条，打印选号过程与上游原始响应
$ switchboard call "hi" --model deepseek-v4-pro --debug
选中 deepseek-1（priority 100，无冷却）
→ POST https://api.deepseek.com/v1/chat/completions
← 200 text/event-stream

# 10 三条入站路径并存，同一个账号都能打到。
#    账号协议是 openai-chat 时，第二条直通，第一、三条经中间表示转换。
$ curl -s http://127.0.0.1:4567/v1/messages \
    -H 'content-type: application/json' \
    -d '{"model":"deepseek-v4-pro","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}'

$ curl -s http://127.0.0.1:4567/v1/chat/completions \
    -H 'content-type: application/json' \
    -d '{"model":"deepseek-v4-pro","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}'

$ curl -s http://127.0.0.1:4567/v1/responses \
    -H 'content-type: application/json' \
    -d '{"model":"deepseek-v4-pro","max_output_tokens":64,"input":"hi"}'

# 三条都返回 200，响应各自是入站协议的形状；未知路径返回 404，请求体解析失败返回 400。
```

## 4 命令表

全局参数：`--json` 可以写在子命令前后任意位置；`--config <路径>` 与 `--db <路径>` 必须写在子命令之前。

`account` 一组的子命令：`add`、`ls`、`rm`、`test`、`enable`、`disable`。`model` 一组的子命令：`ls`、`sync`、`probe`，后两个都支持 `--provider` 与 `--account` 筛选账号。`--models` 只是手填的初始值，模型列表的常规来源是 `model sync` 拉取加 `model probe` 探活（见 5.7）。

`account`、`model`、`log` 三组支持 `-h`、`--help` 与 `help`：把该组的用途与子命令清单（每个子命令一行中文说明）打到 stdout，退出码 0，不读数据库。子命令自己的 `--help` 仍打印该子命令的参数清单。未知子命令退出码 2，提示里给出 `switchboard <组> --help`。

| 命令 | 参数 | 行为 | 输出 |
| --- | --- | --- | --- |
| `init` | `--force` | 建配置目录与数据目录，写默认配置；`--force` 覆盖 | 两个路径 |
| `serve` | `--addr`、`--log-level` | 前台起网关，注册三条入站路由与 `/healthz`；启动只统计账号与模型数，不发上游请求 | 启动摘要，之后按请求打日志 |
| `account add` | `<名称> --provider --key [--protocol] [--base-url] [--models] [--priority]` | 加密存 key，按厂商补默认 base-url 与协议 | 新账号名称 |
| `account ls` | `--json` | 列账号、模型数、是否处在冷却；`account_models` 有数据时模型列显示「N 可用 / M 同步」，`--json` 每行多了 `models_usable` 与 `models_synced` 两个计数 | 表格 |
| `account rm` | `<名称>` | 删账号，同时删掉它的 `account_models` 行；关联的请求记录保留 | 确认一行 |
| `account test` | `<名称>` | 用最小请求探活，更新冷却状态 | 状态码与耗时 |
| `account enable` | `<名称>` | 重新启用账号并清冷却 | 确认一行 |
| `account disable` | `<名称>` | 停用账号，不再参与选号 | 确认一行 |
| `model ls` | `[--provider 厂商]` `[--account 账号]` `--json` | 按模型名列出可用账号与计价来源；`account_models` 有数据时每个账号带状态（可用 / 待探活 / 无权 / 不存在 / 凭据被拒 / 失败），没有任何模型行时回退到 `accounts.models` 并标「手填」 | 表格 |
| `model sync` | `[--provider 厂商]` `[--account 账号]` | 对选中的账号 GET 上游模型列表：地址由 `base_url` 推出，末尾是 `/v1` 就补 `/models`，否则补 `/v1/models`；解析 `{data:[{id}]}`，按名字排序去重，空列表算该账号失败。结果写 `account_models`：新行状态 `synced`，已有行保留探活状态，上游已消失的行删除。只改 `account_models`，不动 `accounts.models`。某个账号失败时其余账号照常处理完 | 表格（账号 / 厂商 / 模型 / 新增 / 备注）与下一步 `model probe` 的提示；`--json` 是数组，每项 `account`、`provider`、`models`、`added`、`error`（只有失败才有） |
| `model probe` | `[--provider 厂商]` `[--account 账号]` `[--sync]` `[--model 模型]` `[--limit N]` `[--concurrency N]` `[--max-age 时长]` `[--refresh]` | 逐账号对每个模型发一条 `max_tokens=1` 的请求判定可用性，结果写 `account_models`；每次上游调用同时写一行 `requests`（见 5.5）。`status` 为 `usable`、`forbidden`、`notfound` 之一且 `checked_at` 在 `--max-age`（默认 `24h`）以内的模型跳过，不发上游请求、不写库、不出现在输出里；`synced`、`auth`、`error` 与 `checked_at` 为 0 的照常探；`--refresh` 或 `--max-age 0` 忽略新鲜度全部重探。该账号库里出现 `usable` 时，把库里**全部** `usable` 写成该账号的 `accounts.models`（不只是本次探到的），选号从此按该列表。没有模型行或带 `--sync` 时先同步；`--model` 只探一个模型名，`--limit` 截断的是跳过之后剩下的待探模型，`--concurrency` 默认 4 且不超过待探模型数；待探模型被全部跳过时仍按库里 `usable` 回写 `accounts.models`，该账号算作已满足；任一账号出现 401 即停止后续账号并返回错误；所有账号都没探出可用模型时退出码 1 且不改任何 `accounts.models`，错误信息带本次探活数与跳过数 | 表格（账号 / 模型 / 状态 / HTTP / 耗时 / 备注）与该账号的「本次探活 N 个 / 跳过 M 个」和可用 / 无权 / 不存在 / 失败计数；`--json` 是数组，每项 `account`、`model`、`status`、`http_status`、`latency_ms`、`message`，只含本次被探的模型，计数只出现在人读输出里 |
| `usage` | `--since`、`--by account\|model\|day` | 汇总请求数、token、估算花费 | 表格 |
| `log tail` | `-n`、`--follow` | 最近的请求记录；`--follow` 跟随 | 表格 |
| `call` | `[提示词] --model [--protocol] [--debug] [--json]` | 绕过客户端直连本进程，打印选号过程；默认发 anthropic 入站，`--protocol` 可在 anthropic、chat、responses 之间切换 | 响应，或 debug 轨迹 |
| `doctor` | 无 | 检查配置文件、数据目录、数据库、端口 | 逐项 ok / fail |
| `version` | 无 | 版本号 | 一行 |

退出码：`0` 成功；`1` 运行失败（含 `doctor` 有检查未通过、`call` 非 2xx）；`2` 用法错误。
错误信息写 stderr，`--json` 时结果写 stdout，两者不混在一起。

`serve` 同时暴露三条入站路径：`/v1/messages`（anthropic）、`/v1/chat/completions`（openai-chat）、`/v1/responses`（openai-responses），路径决定入站协议（见 5.2）；未知路径返回 404。`/healthz` 的响应里带 `inbound` 与 `outbound` 两份协议清单。

## 5 程序内部做什么

### 5.1 进程与文件

| 路径 | 内容 |
| --- | --- |
| `$XDG_CONFIG_HOME/switchboard/config.json` | 监听地址、数据库路径，权限 0600 |
| `$XDG_DATA_HOME/switchboard/switchboard.db` | 账号、请求记录、客户端配置状态 |
| `$XDG_DATA_HOME/switchboard/secret.key` | 加密账号凭据的对称密钥，权限 0600 |

配置只有两项，其余状态都在数据库里。改状态一律走子命令，不手改库。

### 5.2 请求生命周期

1. 收到入站请求，按路径定入站协议：`/v1/messages` 是 anthropic，`/v1/chat/completions` 是 openai-chat，`/v1/responses` 是 openai-responses，其余路径返回 404。请求体解析失败返回 400，错误消息前缀协议名。定好协议后记下模型名与开始时间。
2. 按模型名取候选账号：`enabled=1`、模型列表包含该模型、冷却已过。
3. 按 `priority` 升序，同一会话（见 5.3）优先沿用上次的账号。
4. 解密凭据，按该账号该模型的有效出站协议构造上游请求：入站协议与有效出站协议相同则直通，否则经中间表示转换（见 5.4）。上游路径、凭据头、请求体编码都跟有效出站协议走。
5. 发上游请求，读响应头。上游返回 429、5xx 或超时，且尚未向客户端写出任何字节时，把该账号打入冷却，换下一个候选。
6. 已开始向客户端输出后不再换号；上游中断则向客户端发一个错误事件并结束本次请求。
7. 解析响应里的用量字段，写一行请求记录。
8. 所有候选都失败，返回 503，响应体带最后一次上游错误的摘要。

### 5.3 选号与失败转移

- 候选集合由模型名决定。模型名用请求里的 `model` 字段原文匹配，不做别名归一（第一版）。`model sync` 只刷新 `account_models`，不改变选号依据；`model probe` 探出可用模型后才把列表写进 `accounts.models`，选号此后按新列表走。
- 协议不进选号。候选量与排序只看 `accounts.models`、`enabled`、冷却与 `priority`，协议决定的是「怎么打」而不是「能不能接」：同一账号的两个模型可以各走各的协议，不必为了协议不同拆成两个账号、重复同步与重复探活。有效出站协议按 `account_models.protocol` 优先、账号 `protocol` 兜底算出（见 5.7），换号时按候选账号各自的有效协议重新算。
- 协议只影响路径、凭据头与请求体编码。冷却、失败转移、会话粘性、401 停用与 403 原话返回都在这层之上，不因协议不同而变。
- 冷却：429 起始 60 秒，5xx 起始 10 秒，连续失败翻倍，上限 30 分钟；连续失败次数记在账号的 `fail_streak` 列，跨请求累计。任意一次成功、`account test` 成功或 `account enable` 都会把冷却、`last_error` 与 `fail_streak` 一起清零。
- 会话粘性：请求头带 `x-switchboard-session` 或 `metadata.user_id` 时，同一个值在一小时内粘同一个账号；否则按 `priority` 轮转。
- 401 说明凭据本身不行：把账号标为 `enabled=0` 并把错误返回客户端，避免同一条错误把整个候选池刷成不可用。`account test` 成功或 `account enable` 可恢复，两者都会清冷却。
- 403 多为套餐或模型权限不对，凭据还能用：不停用账号，也不再换号，直接把上游原话返回。commandcode 对不在套餐内的模型就是返回 403。

### 5.4 中间表示与转换规则

入站协议三条，出站协议三条，两边相同时直通，不同时经中间表示互转。中间表示不绑任何一家上游的字段名，协议差异全部在编解码器里吸收。

中间表示在 `internal/translate/ir.go`：

| 类型 | 字段 |
| --- | --- |
| `Request` | `Model`、`System`、`Messages`、`Tools`、`ToolChoice`、`ParallelToolCalls`、`MaxTokens`、`Temperature`、`TopP`、`Stop`、`Stream` |
| `Message` | `Role` + `Blocks` |
| `Block` | `text`、`image`、`tool_use`、`tool_result` |
| `Response` | `ID`、`Model`、`Blocks`、`StopReason`、`Usage` |
| `Usage` | `In`、`Out`、`CacheRead`、`CacheWrite` |

按协议名分发的入口都在 `ir.go`：`DecodeRequest`、`EncodeRequest`、`DecodeResponse`、`EncodeResponse`、`NewStreamDecoder`、`NewStreamEncoder`、`NewUsageScanner`、`PathProtocol`。三套编解码器分别在 `anthropic.go`、`chat.go`、`responses.go`。

同一个概念在三条协议里落的位置不同：

| 概念 | anthropic | openai-chat | openai-responses |
| --- | --- | --- | --- |
| system | 顶层 `system` 字段 | 首条 `role=system` 消息 | 顶层 `instructions` |
| tool_use | assistant 消息里的 `tool_use` 块 | `tool_calls[]` | `function_call` item |
| tool_result | user 消息里的 `tool_result` 块 | 独立 `role=tool` 消息 | `function_call_output` item |
| 结束原因 | `end_turn` / `max_tokens` / `tool_use` / `stop_sequence` | `stop` / `length` / `tool_calls` | `completed` / `incomplete`（`max_output_tokens`） |
| 流式事件 | 具名事件 `message_start`、`content_block_delta`、`message_stop` | 只有 `data` 帧，末尾 `[DONE]` | 具名事件 `response.created`、`response.output_text.delta`、`response.completed` |
| 用量字段 | `input_tokens`、`output_tokens`、`cache_read_input_tokens`、`cache_creation_input_tokens` | `prompt_tokens`、`completion_tokens`、`prompt_tokens_details.cached_tokens` | `input_tokens`、`output_tokens`、`input_tokens_details.cached_tokens` |

结束原因在中间表示里只留四种：`end_turn`、`max_tokens`、`tool_use`、`stop_sequence`；编码时再翻回各协议自己的说法。

两条分界与各自的代价：

- 直通（入站协议 = 出站协议）：请求体只改 `model` 字段，其余字段与值原样保留，包括文档里没列出的陌生字段；响应体与流式帧按字节原样转发。读用量不改变转发内容，由该协议的 usage scanner 从响应体或流式帧里读，缓存字段也能读到。代价是只对同协议账号成立，好处是上游新加的字段与参数可以直接用。
- 转换（入站协议 ≠ 出站协议）：解码成中间表示，再按目标协议编码，请求与响应都只搬表里定义过的字段。表里没有的字段丢弃，这是有意的：陌生字段跨协议搬过去只会变成语义错位。代价是改协议就意味着表外字段消失，新增映射必须同时补用例。

流式在转换路径上走增量：上游帧先解码成 `text`、`tool_start`、`tool_args`、`stop`、`error` 增量，再由入站协议的编码器编成客户端事件。上游中途中断时先发一个错误事件，再补齐收尾事件，客户端总能拿到合法结尾。同协议直通时逐行转发原始字节，只用 scanner 记账。

两个协议之间的常见映射示例（anthropic 入 → openai-chat 出）。完整规则在编解码器与 `internal/translate` 的用例里，这里只列常看到的几行：

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

同一对协议间的流式映射（SSE）：

| 上游 chat.completion.chunk | 下发给客户端 anthropic 事件 |
| --- | --- |
| 首块 | `message_start`（含空 content） |
| `delta.content` 首个非空 | `content_block_start(index=0, text)` |
| 后续 `delta.content` | `content_block_delta(text_delta)` |
| `delta.tool_calls[i]` 首个 | `content_block_start(tool_use)` 并分配 index |
| `delta.tool_calls[i].function.arguments` | `content_block_delta(input_json_delta)` |
| `finish_reason` | 关闭所有 block，发 `message_delta`（含 usage），再发 `message_stop` |
| 上游错误帧 | `event: error`，带映射后的 type 与 message |

### 5.5 请求记录

每次尝试写一行，包括失败与换号前的尝试。字段：时间、入站协议、出站协议、请求模型名、实际模型名、账号 id、HTTP 状态、是否成功、输入 token、输出 token、缓存读、缓存写、耗时、错误摘要。

`inbound` 与 `outbound` 两列记的是这一行实际用到的协议对，转换时两列不同，直通时两列相同。除输入与输出 token，`cache_read` 与 `cache_write` 也会写入：anthropic 的缓存读与缓存写分别取自 `cache_read_input_tokens` 与 `cache_creation_input_tokens`；chat 与 responses 只带缓存读（`cached_tokens`），这两家的缓存写记为 0。直通与转换两条路径都会填这几个字段。

`model probe` 的 `max_tokens=1` 调用同样写一行，一个模型这次探活写一行，记的是得出判定结果的那一次调用（发现成功的 2xx、账号协议那次的 401，或者候选都试完时的账号协议那次）。协议发现换协议重试的中间调用不单独记行：它们都没得出判定，也没产生 token。这类行的取值：`ts` 是这次请求的开始时间；`inbound` 与 `outbound` 都取这次实际使用的协议（发现成功的那次是发现到的协议，其余是账号协议），探活就是「用这个协议直连上游」；`model_requested` 与 `model_used` 都取被探的模型名；`account_id` 取账号 id；`http_status` 与 `ok` 取上游响应，`ok` 表示 2xx，网络错误时 `http_status` 记 0；`in_tokens`、`out_tokens`、`cache_read`、`cache_write` 都由这次所用协议的 usage scanner 从响应体读出：anthropic 的 scanner 四项齐全（`cache_read_input_tokens` 与 `cache_creation_input_tokens`），chat 与 responses 只读得到缓存读（`cached_tokens`），`cache_write` 记 0；非 2xx 与网络错误记 0；`latency_ms` 是这次请求的耗时；`error` 是错误摘要，与探活结果一样过 `scrubSecret`，凭据不进库。这类行因此也进 `usage` 的汇总。

估算花费需要价格表；价格表第一版放在账号上（`--price-in / --price-out`，单位每百万 token），缺省则花费记为 null，不猜价格。

### 5.6 凭据

AES-256-GCM 加密后入库，密钥在 `secret.key`。这个方案防的是「备份文件、误贴日志、共享磁盘快照」这类泄漏，不防同一台机器上以同一用户身份运行的程序。文档里明确写出这一点，不暗示更强保护。

### 5.7 模型同步与探活

手填 `--models` 的问题在于上游一改模型名就失效：账号看上去正常，请求却全打在 404 或 403 上，只能对着上游文档猜。模型列表改成从上游拉取，再逐个探活，只保留真正能用的名字。

两个命令分工明确，一个管「上游有哪些模型」，一个管「哪几个真能用」。两者都按账号处理，并支持同样的筛选：`--provider` 精确匹配账号的 `provider` 字段，`--account` 精确匹配账号名，两个都给时账号必须属于该厂商，否则报错；不带筛选参数时处理全部账号，按账号逐个进行。

`model sync [--provider 厂商] [--account 账号]` 只解决前者：

- 模型列表地址由 `base_url` 推出，末尾是 `/v1` 就补 `/models`，否则补 `/v1/models`。
- 响应按 `{data:[{id}]}` 解析，按名字排序去重。上游返回空列表算该账号失败。
- 结果写进 `account_models`：新出现的模型行状态为 `synced`，已存在的行保留既有探活状态，上游已消失的行删除。
- 协议发现结果不被同步清掉：写入走 `INSERT … ON CONFLICT(account_id, model) DO NOTHING`，已存在的行一个字段都不动，`status`、`checked_at` 与 `protocol` 原样保留，只有新出现的行拿默认值。一次同步不会把探活与协议发现的结论推倒重来。
- 这条命令只改 `account_models`，不动 `accounts.models`，所以同步本身不改变选号结果。
- 某个账号失败不打断其余账号：全部处理完才返回，退出码 1，失败原因写在那一个账号的备注列。

`model probe [--provider 厂商] [--account 账号] [--sync] [--model 模型] [--limit N] [--concurrency N] [--max-age 时长] [--refresh]` 只解决后者：逐账号处理，对该账号的每个模型发一条 `max_tokens=1` 的请求，按响应判定可用性。判定结果连同时间写进 `account_models.checked_at`，下一轮按它决定哪些模型不用再花钱。

| 上游响应 | 状态 | 含义与处理 |
| --- | --- | --- |
| 2xx | `usable` | 可用，进入可用列表 |
| 401 | `auth` | 凭据被拒，同时把账号停用 |
| 403 | `forbidden` | 套餐或权限不含该模型，凭据本身还能用 |
| 404 | `notfound` | 上游不认识这个模型名 |
| 400 且错误文本明确说模型不存在 | `notfound` | 按模型名不存在处理 |
| 其它 | `error` | 临时错误，结论为不可用 |

协议发现。协议真正的归属是 `(base_url, model)` 而不是账号：同一家上游的 `claude-*` 只接受 `/messages`，其余模型走 `/v1/chat/completions`，用的是同一个 key。所以探活除了判定可用性，还顺手把「这个模型该用哪种协议」测出来，结论写进 `account_models.protocol`（见 6）。

- 先用账号的 `protocol` 试一次。这一次的结论先存着，它就是「按账号配置打」的真相。
- 这一次失败时，只有可能只是路径不对的才换协议再试：`404`（路径不存在）、`405`（方法不允许）、`403`（有的网关按路径拒，commandcode 对非 `/messages` 的 `claude-*` 就是 403），以及 `400` 且错误文本指向端点、路径或「不支持该协议」。其余失败（例如明确说模型不存在、`5xx`、网络错误）直接按上表判定，不换协议。
- 换协议时依次试另外两种，顺序固定：账号协议优先，其余按 anthropic、openai-chat、openai-responses 补齐。顺序固定是为了同一模型多次探活得到同一个结论，不随运行顺序变化。
- 候选协议列表是探活的入参，默认就是三种线协议全试。以后按厂商声明收紧（例如某家只有 anthropic 与 chat 两条）时只传该厂商声明过的那些，循环本身不改，也不会对着不存在的端点白打一次。
- 第一个 `2xx` 就是结论：状态记 `usable`，把这次用的协议写进 `account_models.protocol`，剩下的协议不再试。
- `401` 一律不换协议：凭据问题与路径无关，照旧停用账号并立即停止整次探活。这一条只判账号协议那一次；换协议之后的 `401` 多半是凭据头形状不对（anthropic 用 `x-api-key`，另两家用 `Bearer`），既不停用账号也不算凭据被拒。
- 三种协议都没试出 `2xx` 时，结论取账号协议那一次的判定（状态、HTTP 状态与错误文本），`protocol` 保持原值不动。协议列只在发现成功时写，一次失败不会把之前测出的协议抹掉，也不会凭空写入一个。

花费：先用账号协议试，成功时只花一次调用，只有失败才会多花一到两次。同一模型重复探活的结果稳定，代价最多是每次 `--refresh` 多一两次 `max_tokens=1` 的调用，跟 `--max-age` 省下的调用相比是小头。得出判定的那次在 `requests` 里留一行（见 5.5）。

`account test` 与具体模型无关，仍按账号的 `protocol` 打一次，不看模型上的协议发现结果：它检验的是凭据与账号整体可用性。

参数与处理顺序：

- 该账号没有模型行，或给了 `--sync` 时，先同步一次，不需要手工跑两遍。自动同步失败的账号跳过不探，其余账号照常处理，最终退出码 1。
- 增量跳过：模型同时满足「`account_models.status` 是 `usable`、`forbidden`、`notfound` 之一」与「`checked_at` 距当前不超过 `--max-age`」时跳过，不发上游请求。这三种状态都有定论，重探一遍结论不会变，跳过省下的是真实计费调用。`synced`（从未探过）、`auth`、`error` 没有定论，照常探活；`checked_at` 为 0 视为不新鲜。
- `--refresh` 忽略新鲜度全部重探，`--max-age 0` 与它等价。`--max-age` 默认 `24h`，新鲜度对 `--model` 指定的单模型同样生效，`--refresh` 可以强制重探。
- 跳过的模型不写库、不产生输出行，`--json` 里也就没有它们。
- `--model <模型>` 只探指定的一个模型名。
- `--limit N` 截断的是跳过之后剩下的待探模型，不是库里的模型总数：多账号时每个账号各有 N 个额度，跳过的模型不占额度。
- `--concurrency N` 改并发，默认 4，且不超过待探的模型数。
- 一个账号探完立刻写回结果并打印该账号的「本次探活 / 跳过 / 可用 / 无权 / 不存在 / 失败」计数，再处理下一个账号。

探活是要花钱的。每个模型的一次判定都是一次真实的上游调用，`max_tokens=1` 也照样计入上游账单与速率限制，所以模型多的账号用 `--limit` 控制这一次的花费，重复跑同一批账号靠 `--max-age` 与 `--refresh` 控制。得出判定的那次调用在 `requests` 里留一行，`usage` 与 `log tail` 里都能看到（见 5.5）；换协议多打的调用没有 token，不单独记行。

任何一个账号碰到 401，整次探活就停下并返回错误：凭据都不行了，继续探下去只是重复同一条错误。此时该账号已被停用，后续账号不再处理，其余结论不作数，也不会写回 `accounts.models`——即使同批里已经探出了可用模型，也不写，避免把一次凭据故障当成新的事实。

一个账号的待探模型被全部跳过时，这次一条上游请求都不发，但仍按库里全部 `usable` 回写该账号的 `accounts.models`。报错与否只看终态：库里至少一个 `usable` 的账号算已满足，退出码 0；全部账号库里都零可用时退出码仍是 1，错误信息带上本次的探活数与跳过数（例如「本次探活 0 个、跳过 7 个」），跳过一个以上时再提示结论仍在 `--max-age` 窗口内、可用 `--refresh` 强制重探。「全被跳过」与「探了都不行」因此能从文案上分开，而不是一律落成同一句。

探活结果反过来成为选号依据：一个账号只要库里至少一个模型是 `usable`，就把库里全部 `usable` 写成该账号的 `accounts.models`，5.2 与 5.3 的候选集合从此按这份列表算。取全库而非本次结果，是因为 `--limit` 与 `--model` 都可能只探一部分：只探两个却把另外三个之前探通的模型从选号里除掉，属于退化。某账号零个可用模型时只写 `account_models`，不改它的 `accounts.models`，账号原有列表保持不变，避免一次网络抖动就把账号清空。所有账号都没探出可用模型时退出码 1，且不改任何 `accounts.models`。

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
  fail_streak    INTEGER NOT NULL DEFAULT 0,
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

CREATE INDEX requests_ts ON requests(ts DESC);

CREATE TABLE account_models (
  account_id  INTEGER NOT NULL,
  model       TEXT    NOT NULL,
  status      TEXT    NOT NULL,
  http_status INTEGER NOT NULL DEFAULT 0,
  error       TEXT    NOT NULL DEFAULT '',
  checked_at  INTEGER NOT NULL DEFAULT 0,
  protocol    TEXT    NOT NULL DEFAULT '',
  PRIMARY KEY (account_id, model)
);
```

`account_models` 由迁移 `0003` 建立，`status` 取值就是 `synced`、`usable`、`forbidden`、`notfound`、`auth`、`error`（见 5.7）。`accounts.fail_streak` 由迁移 `0004` 加入，只用于冷却翻倍，写入口只有 `RecordFailure`（加一）与 `ClearFailure`（清零）。`accounts.models` 仍是选号实际读的那份列表。删除账号时 `DeleteAccount` 在同一个事务里删掉它的 `account_models` 行：SQLite 的 rowid 会被复用，遗留子行会让新账号继承旧账号的探活状态。

`account_models.protocol` 由迁移 `0005` 加入，记录「这个模型该用哪种协议」，取值同样是 anthropic、openai-chat、openai-responses。**空字符串表示继承账号的 `protocol`**：有效出站协议是「列非空就用列，否则用账号的 `protocol`」，纯规则在 `store.ResolveProtocol`，读库入口是 `store.EffectiveProtocol(accountID, accountProtocol, model)`，行不存在与列为空都当成继承，不报错。转发与展示只读这个有效协议，不在调用方拼规则；`cli` 要显示协议时也只调它。

这个列只有一个写入口 `SetModelProtocol`，由探活发现协议后调用（见 5.7）。`SetModelStatus` 只改 `status`、`http_status`、`error`、`checked_at`，不动 `protocol`；`ReplaceModels` 的 `ON CONFLICT DO NOTHING` 也只给新行写默认值，同步不会覆盖已发现的协议。旧库升上来时整列默认空串，也就是全部回退到账号协议，行为与 M7 一致。

迁移用 `internal/store/migrations/*.sql`，按文件顺序执行，`PRAGMA user_version` 记录已执行版本。

## 7 里程碑

| 里程碑 | 内容 | 验收 |
| --- | --- | --- |
| M0 | 骨架：`init`、SQLite 迁移、`serve` 起 `/healthz`、`doctor` 完整 | `make check` 通过，`doctor` 全 ok |
| M1 | 非流式直通：anthropic → anthropic，`account` 的增删查与探活、`call`、`log tail` | `call` 打真实账号拿到回复，`log tail` 有记录 |
| M2 | 转换：anthropic → openai-chat，非流式，含工具调用 | 对拍用例覆盖 5.4 的映射示例里每一行 |
| M3 | 流式：SSE 双向转换 | 客户端能流式拿到 token 与工具调用 |
| M4 | 多账号：冷却、失败转移、`usage` | 两个账号时杀掉一个，请求自动落到另一个 |
| M5 | 模型同步与探活：`model sync`、`model probe`、`model ls` 按模型列账号状态 | 对真实上游 `model sync` 能拉出模型数；`model probe` 能区分 `usable` 与 `forbidden`；零可用模型时不改 `accounts.models` |
| M6 | 协议矩阵：三入站 × 三出站 + 同协议直通，中间表示与三个编解码器，流式增量 | 9 种组合的非流式与流式用例通过；对真实上游三种入站各打一条能拿到回复 |
| M7 | 模型探活跳过与记账：`model probe` 按 `--max-age` 跳过有定论且新鲜的结论、`--refresh` 强制重探，每次探活的上游调用写一行 `requests` 并带缓存读写 | `TestModelProbe*` 覆盖跳过、刷新、`--limit`、记账、零可用的跳过计数，`TestProbeModel*` 覆盖各状态判定、401 停用与探活请求行；`log tail` 能看到探活行，零可用时 `accounts.models` 不变 |
| M8 | 协议归属从账号降到模型：`account_models.protocol`（迁移 `0005`）覆盖账号的 `protocol`，探活顺手发现协议，选号与转发按有效协议算，同步不覆盖发现结果 | 同一账号下两个模型可以分别走 anthropic 与 openai-chat：各自打到正确的路径与凭据头，「入站 == 出站」按模型协议算；协议发现用例覆盖 `404`、`403`、`400 指向端点` 三种失败换协议、账号协议直接成功只花一次调用、`401` 不换协议并停用账号；`EffectiveProtocol` 在行不存在或列为空时回退账号；旧库升上来 `protocol` 默认为空串，`ReplaceModels` 不动已发现的协议 |

M0 到 M8 的代码已落地，端到端由 `internal/cli` 的 `TestWalkingSkeleton` 用假上游验证：初始化、加账号、`call`、`log tail`、`usage`、`doctor` 全链路走通。协议矩阵由 `internal/gateway/matrix_test.go` 与 `matrix_stream_test.go` 验证：9 种组合的非流式与流式各跑一遍，同协议直通的「不改字节」「保留陌生字段」与跨协议的「丢弃陌生字段」单独有用例。协议降级到模型的那部分由 `internal/gateway` 的 `TestProbeModel*` 与 `TestForwardUsesModelProtocol` 验证，迁移兼容由 `internal/store` 的 `TestMigration0005*` 与 `TestMigrationsRunToLatestFromEmptyDB` 验证。上游是 `httptest` 假服务，M5、M6、M7 与 M8 里涉及真实厂商的那部分需要拿到 key 后手工验收。

已知未做，按已知行为处理：

- 估算花费依赖账号上的 `--price-in` 与 `--price-out`，没有价格时记为 0 并在 `usage` 末尾提示。
- 模型名不做别名归一，请求里的 `model` 必须与账号 `--models` 里的名字完全一致。
- 不带任何客户端配置改写：客户端里的 base URL、token、模型名都靠自己填。
- 探活并发默认 4，`--concurrency` 可以改，只是默认值，不会根据上游限速自适应。
- gemini 协议进出都没有实现：既没有入站路径，也不在账号的 `protocol` 可选值里。
- 思考与推理内容在转换时丢弃：anthropic 的 `thinking` 块、chat 的 `reasoning_content`、responses 的 `reasoning` item 都不进中间表示。
- 音频与文件类内容丢弃，只搬运文本、图片、工具调用与工具结果四类块。
- responses 的内置工具（`web_search` 等）丢弃，工具定义只认 function。
- 跨协议只做图片地址的搬运，没有图片转写以外的多模态变换。

之后（不在第一版）：gemini 入站与出站，订阅账号登录，隐私遮蔽，配置档案（profiles），价格表内置。

## 8 代码结构

```
cmd/switchboard/        main，只做 os.Exit(cli.Run(...))
internal/cli/           子命令分发、参数解析、输出格式
internal/config/        路径解析与配置读写
internal/store/         SQLite 连接、迁移、账号与请求记录的读写
internal/secret/        凭据加解密
internal/account/       账号池、冷却、选号
internal/gateway/       HTTP 入站处理、上游转发、错误映射
internal/translate/     中间表示（ir.go）与三个协议的编解码（anthropic.go、chat.go、responses.go）
internal/usage/         用量汇总与花费估算
```

约束：`translate` 只做结构体到结构体的转换，不读环境、不发请求，便于用对拍用例覆盖；`gateway` 是唯一发上游请求的地方（`account test` 的探活也走 `gateway.Probe`）；`cli` 不直接连数据库，只调 `store` 与 `account`。

依赖：只有 `modernc.org/sqlite`（纯 Go 实现，不需要 cgo，交叉编译不受影响），其余全部用标准库。命令行解析用 `flag`，不引入第三方 CLI 框架。

## 9 非目标

- 多租户、配额、充值、开票。
- 网页后台与桌面菜单栏。
- 逆向接口、把订阅网页版包装成 API。
- 改写其它客户端的配置文件，不论带不带备份。
- 跨机部署与集群。

## 10 需要外部信息才能定的点

- 各家上游的默认 base-url、模型名清单、是否兼容 `openai-chat`。
- 各家价格，用于估算花费。缺价格就记 null。
