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

同行同列是“直通”，其余是“转换”。“直通”只需重写请求体里的 `model` 字段，其余字段与值原样带到上游，响应与流式帧按字节原样返回（上游中途断流时按同一协议补上错误帧与收尾帧，见 5.4），用量另由该协议的 scanner 读出。“转换”走中间表示：请求与响应都只搬映射表里定义过的字段（见 5.4）。gemini 既没有入站路径，也没有可在账号上选的出站协议。

## 3 用起来是这样

```bash
# 1 初始化。建配置目录与数据库，重复执行不会覆盖已有配置
$ switchboard init
配置 /home/someone/.config/switchboard/config.json
数据库 /home/someone/.local/share/switchboard/switchboard.db

# 2 加账号。key 加密后入库，不进配置文件。厂商的默认地址与默认协议来自厂商表
#   （默认协议显式存在厂商表里，deepseek 是 openai-chat；这里显式写一遍也一样）
$ switchboard account add deepseek-1 \
    --provider deepseek \
    --protocol openai-chat \
    --key sk-xxxxxxxx
已添加 deepseek-1（deepseek，openai-chat，凭据 sk-x…）

# 2.1 也可以自己登记一家厂商。每个协议一条 flag，值是相对 --base-url 的完整路径，
#     也可以直接写整条地址（同一厂商的不同协议因此可以落在不同域名上）；
#     至少给一条；路径写法由程序规整（前导斜杠、重复斜杠、末尾斜杠都不必纠结）。
#     默认协议必须落在已声明的路由上。改一家已有的厂商用 provider set，
#     删一条路由或清一项设置用 provider unset
$ switchboard provider add acme --base-url https://api.example.com \
    --openai /v1/chat/completions --anthropic /v1/messages
已添加厂商 acme（https://api.example.com），2 条路由，默认协议 openai-chat
$ switchboard provider unset acme anthropic
acme 已清掉 anthropic 路由
现有 1 条路由，默认协议 openai-chat

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

`account` 一组的子命令：`add`、`ls`、`set`、`rm`、`test`、`enable`、`disable`（`update` 作为 `set` 的同义词接受，帮助里只列 `set`）。`model` 一组的子命令：`ls`、`sync`、`probe`，后两个都支持 `--provider` 与 `--account` 筛选账号。`provider` 一组的子命令：`ls`、`add`、`set`、`unset`、`rm`，维护厂商默认地址、模型列表覆盖与它的路由（见 5.8）；`add` 只新建，改一家已有厂商用 `set`，删一条路由或清掉一项设置用 `unset`（删除不做成「传空串」：空值一律是用法错误）。`--models` 只是手填的初始值，模型列表的常规来源是 `model sync` 拉取加 `model probe` 探活（见 5.7）。

`account`、`model`、`provider`、`log` 四组支持 `-h`、`--help` 与 `help`：把该组的用途与子命令清单（每个子命令一行中文说明）打到 stdout，退出码 0，不读数据库。子命令自己的 `--help` 仍打印该子命令的参数清单。未知子命令退出码 2，提示里给出 `switchboard <组> --help`。

| 命令 | 参数 | 行为 | 输出 |
| --- | --- | --- | --- |
| `init` | `--force` | 建配置目录与数据目录，写默认配置；`--force` 覆盖 | 两个路径 |
| `serve` | `--addr`、`--log-level` | 前台起网关，注册三条入站路由与 `/healthz`；启动只统计账号与模型数，不发上游请求 | 启动摘要，之后按请求打日志 |
| `account add` | `<名称> --provider --key [--protocol] [--base-url] [--models] [--priority]` | 加密存 key；缺 `--base-url` 或 `--protocol` 时从厂商表补：地址取厂商的 `base_url`，协议取该厂商的有效默认协议（显式值落在已声明路由上就用它，否则按固定顺序 `openai-chat` → `openai-responses` → `anthropic` 取第一条，见 5.8），两个参数都仍能覆盖。厂商行在但一条路由都没有时算不出默认协议：没给 `--protocol` 也没给 `--base-url` 就报错，提示先用 `provider set` 补上路由，或改用这两个参数；给了 `--base-url` 则按自建地址加账号、协议回退 openai-chat。厂商不在表里且没给 `--base-url` 时报错，列出已知厂商、编辑距离不超过 2 时给出最接近的名字，并说明 `--provider <自定义名> --base-url <地址>` 仍可自建；厂商表为空时只认 `--base-url`。`--protocol` 只收 anthropic、openai-chat、openai-responses（见 5.8）。`--base-url` 与 `provider add` 同一份校验（`store.NormalizeBaseURL`，见 5.8），只有 scheme 没有主机名的写法是用法错误、退出码 2，落库的是规整后的形态 | 新账号名称 |
| `account ls` | `--json` | 列账号、模型数、是否处在冷却；`account_models` 有数据时模型列显示「N 可用 / M 同步」，协议列显示账号默认协议，存在与它不同的已发现协议时写成「openai-chat + 另 1 种」；`--json` 每行多了 `models_usable`、`models_synced` 两个计数与 `protocols`（默认协议在前，其余是模型行上发现到的协议） | 表格 |
| `account set` | `<名称> --key <key>` | 换掉已有账号的凭据，不删号重建：删号会丢掉 `account_models` 里的探活结果、协议发现与请求记录的账号归属。`--key` 的三种给法与 `account add` 一致（直接写值、`--key -` 从标准输入读、留空读 `SWITCHBOARD_KEY`），缺凭据是用法错误（退出码 2 并打出用法）。key 走同一条加密路径覆盖 `key_cipher`，明文不进日志、不进 `--json` 输出。旧凭据的结论一并作废：同一个事务里清掉 `cooldown_until`、`fail_streak`、`last_error` 并把账号重新启用（`enabled=1`），写入口是 `store.RotateKey`。不动 `account_models`、`accounts.models` 与协议发现结果，也不碰厂商表。账号不存在时报错并提示先 `switchboard account add` | 确认一行（新凭据的掩码、冷却与失败计数已清零、账号已启用）；`--json` 含 `name`、`key`（掩码）、`cooldown_cleared`、`fail_streak_cleared`、`enabled` |
| `account rm` | `<名称>` | 删账号，同时删掉它的 `account_models` 行；关联的请求记录保留 | 确认一行 |
| `account test` | `<名称>` | 分两步探活。先 `GET` 模型列表，地址与凭据头跟 `model sync` 同一套规则解析（厂商级覆盖 → 账号协议那条路由 → 同厂商按协议名排序的第一条非空 `models_path`，整条地址直接访问；凭据头跟取到地址的那条路由的协议，见 5.7），2xx 即判定凭据与连通性正常，同时报出上游模型数（按 `{data:[{id}]}` 解析，解析不了只说明「上游返回的不是模型列表」，不判失败）；列表返回 404 或 405（这家没有列表接口）、或 403（可能被网关拦）时退回一条 `max_tokens=1` 的请求，模型取 `accounts.models` 第一项、为空时用默认模型名 `default` 并在备注里说明。两阶段的状态处理一致：任一阶段 2xx → 启用账号并清掉冷却、`last_error` 与 `fail_streak`；401 → 停用账号并把错误写进 `last_error`；429/5xx/超时 → 按连续失败次数打冷却；其它 4xx → 只报原话、不动状态。它只判凭据与连通性，模型是否可用由 `model probe` 决定 | 表格（账号 / 状态 / HTTP / 耗时 / 上游模型数 / 备注），耗时取得出结论那一阶段的耗时，列表接口没通时模型数显示 `-`，备注写清结论出自列表接口还是模型调用；`--json` 含 `account`、`stage`（`models` 或 `call`）、`status`、`ok`、`models`（数量，未知为 0）、`latency`、`message` |
| `account enable` | `<名称>` | 重新启用账号并清冷却 | 确认一行 |
| `account disable` | `<名称>` | 停用账号，不再参与选号 | 确认一行 |
| `model ls` | `[--provider 厂商]` `[--account 账号]` `--json` | 按模型名列出可用账号与计价来源；`account_models` 有数据时每个账号带状态（可用 / 待探活 / 无权 / 不存在 / 凭据被拒 / 失败），没有任何模型行时回退到 `accounts.models` 并标「手填」；另有协议列，显示该模型在这个账号上的有效出站协议，取值只经 `store.EffectiveProtocol`，行不存在或协议列为空时由它回退到账号协议；`--json` 每个账号多了 `protocol` | 表格 |
| `provider ls` | `--json` | 列厂商、`base_url`、默认协议、模型列表、各协议的路由与备注；默认协议是该厂商的有效默认协议（显式值落在已声明路由上就用它，否则按固定顺序 `openai-chat` → `openai-responses` → `anthropic` 取第一条，见 5.8），一条路由都没有时显示 `-`，不算错误；模型列表列：厂商级 `models_path` 非空就显示它，否则显示各路由自己的 `models_path`（多条路由共用一个路径时只显示一次），都没有时显示 `-`；`--json` 每项含 `name`、`base_url`、`default_protocol`、`models_path`、`protocols`（每项 `protocol`、`path`、`models_path`）、`note`，没有路由时 `default_protocol` 为空串、`protocols` 为空数组 | 表格 |
| `provider add` | `<名称> --base-url <地址> [--anthropic <路径>] [--openai <路径>] [--responses <路径>] [--default-protocol <协议>] [--models-path <路径>] [--note <备注>]` | 只新建：名称已在厂商表里时报错（「厂商 X 已存在，改用 switchboard provider set X 修改」），绝不顺手改已有的行。`--base-url` 必填，校验与规整走 `store.NormalizeBaseURL`（见 5.8），cli 不另写一份 scheme/主机名预检：必须是带主机名的完整地址，`https://` 这种只有 scheme 的写法是用法错误（退出码 2，文案说明缺主机名并给正确例子）；scheme 大小写不敏感（`HTTPS://host/` 能用），落库时归一成小写、路径里重复斜杠合并、末尾斜杠去掉。至少要给一条协议路由，一条都不给是用法错误（不再需要这家厂商就用 `provider rm`）。三个协议 flag 各声明 `(厂商, 协议)` 的一条路由，值可以是相对 `base_url` 的完整路径，也可以是整条地址（含 `://`，见 5.8）：相对路径只规整不拒绝，`v1/chat/completions`、`//v1//chat/completions`、`/v1/chat/completions/` 都落成同一个地址，不报错，只有规整之后为空才算缺值；整条地址校验 scheme 与主机名（`https://`、`https:///x`、`ftp://host/x` 都是用法错误，退出码 2，文案指出缺的是主机名还是 scheme 不对），路径部分照旧规整、查询串与 `#` 之后原样保留，使用时不再拼 `--base-url`。同一个 `base_url` 上不同协议落在不同域名时，协议 flag 直接写整条地址：`switchboard provider add myco --base-url https://api.example.com --openai https://openai.example.com/v1/chat/completions --anthropic https://claude.example.com/v1/messages`。`--help` 写明目前支持 anthropic、openai、responses 三种协议，没有 `--gemini`：gemini 协议尚未实现，留一条永远用不上的路由只会误导。没给每条路由的模型列表路径时按规则自动填：`openai`（openai-chat）路由把尾部 `/chat/completions` 换成 `/models`（`/v1/chat/completions` → `/v1/models`、`/chat/completions` → `/models`），其余协议留空表示回退到同厂商其它协议的非空值（见 5.8）。`--models-path` 是厂商级覆盖，收同样两种写法（相对路径或整条地址），整条地址同样要带主机名。`--default-protocol` 是这家的默认协议，取值是命令行写法 `anthropic`、`openai`、`responses`（也认线协议全名 `openai-chat`、`openai-responses`，`openai` 就是 chat），必须落在这次声明的路由上，否则整笔拒绝、退出码 1、一条路由都不落库；缺省时按固定顺序 `openai` → `responses` → `anthropic` 取第一条已声明的路由。取值参数传给空串（或只有斜杠这类规整之后为空的写法）是用法错误，删除与清空见 `provider unset` | 确认一行（地址、默认协议、几条路由） |
| `provider set` | `<名称> [--base-url <地址>] [--anthropic <路径>] [--openai <路径>] [--responses <路径>] [--models-path <路径>] [--default-protocol <协议>] [--note <备注>]` | 只设值，不承担删除：只改传入的字段，其余一律保留。协议 flag 覆盖或新增那条路由的路径（写法与 `provider add` 同一套：相对路径只规整，整条地址校验后直接使用），`--default-protocol` 与 `--models-path` 换新值（`--models-path` 同样收两种写法），`--note` 换备注。任何取值参数传空串（例如 `--openai ""`、`--note ""`）都是用法错误、文案指向 `provider unset`，路径参数只有斜杠这类规整之后为空的写法同样算空值，不做「空串即删除」的约定：误敲一个空引号不该把已有路由或备注抹掉。改完之后默认协议必须仍指向一条存在的路由，否则整笔拒绝并提示先加路由或改默认协议。没给模型列表路径的路由按 `openai` 推导规则重算，手工填过的值保持不动。厂商不存在时报错并提示用 `provider add` 新建；一个参数都不给是用法错误 | 确认一行（改了几项、现有几条路由、默认协议） |
| `provider unset` | `<名称> <字段> [<字段>...]` | 删除必须显式声明：`anthropic`、`openai`、`responses` 删掉那条协议路由，`models-path` 清掉厂商级覆盖、回到按路由取，`default-protocol` 清掉显式值、回到 `openai` → `responses` → `anthropic` 固定顺序取第一条路由，`note` 清空备注。字段是位置参数，一次可以给多个，重复的只处理一次。要删的项本来就不存在时不报错（幂等），输出一行说明「X 本来就没有 …」。`base-url` 与厂商名不能 unset：前者是必填地址、清掉就无处转发，后者是主键、清不掉，两者都报错说明原因；未知字段、带 `--` 前缀、大小写不符报错并列出可用字段。删到一条路由都不剩时整笔拒绝并指向 `provider rm <名称>`；删掉的路由正好是默认协议时同样整笔拒绝，文案由 store 给，列出仍然声明的协议并给出 `provider set <名称> --default-protocol <协议>` 与 `provider unset <名称> default-protocol` 两条出路（cli 不另算一份回退规则），同一次调用里带上 `default-protocol` 即放行。`--json` 输出与 `provider ls` 的单项同形 | 逐项确认（清掉了什么 / 本来就没有）+ 现有几条路由与默认协议 |
| `provider rm` | `<名称> [--force]` | 删除厂商行与它的全部路由行；仍有账号指向该厂商时列出这些账号并拒绝，加 `--force` 才删，删完说明这些账号仍保留自己的 `base_url` | 确认一行，或待确认的账号清单 |
| `model sync` | `[--provider 厂商]` `[--account 账号]` `[--endpoint <路径>]` | 对选中的账号 GET 上游模型列表：地址按厂商的 `models_path` 覆盖、账号协议那条路由的 `models_path`、同厂商按协议名排序的第一条非空 `models_path` 的顺序取，都没有时按 `base_url` 后缀猜；凭据头跟「实际取到这条路径的那条路由」的协议走，不是账号协议（见 5.7）。`--endpoint` 只影响这一次调用，值是相对账号的 `base_url` 的完整路径，也可以是整条地址（见 5.8；相对路径只规整，`v1/models` 与 `//v1//models/` 都可用；整条地址直接访问、不再拼 `base_url`），给了就压过厂商覆盖与路由，作用于被选中的每个账号；传空值或只有斜杠是用法错误，整条地址缺主机名、scheme 不是 http(s) 同样是用法错误（退出码 2，文案指出缺什么），不需要临时覆盖就整个去掉这个参数；解析 `{data:[{id}]}`，按名字排序去重，空列表算该账号失败。结果写 `account_models`：新行状态 `synced`，已有行保留探活状态，上游已消失的行删除。只改 `account_models`，不动 `accounts.models`。某个账号失败时其余账号照常处理完 | 表格（账号 / 厂商 / 模型 / 新增 / 备注）与下一步 `model probe` 的提示；`--json` 是数组，每项 `account`、`provider`、`models`、`added`、`error`（只有失败才有） |
| `model probe` | `[--provider 厂商]` `[--account 账号]` `[--sync]` `[--model 模型]` `[--limit N]` `[--concurrency N]` `[--max-age 时长]` `[--refresh]` | 逐账号对每个模型发一条 `max_tokens=1` 的请求判定可用性，结果写 `account_models`；每次上游调用同时写一行 `requests`（见 5.5）。`status` 为 `usable`、`forbidden`、`notfound` 之一且 `checked_at` 在 `--max-age`（默认 `24h`）以内的模型跳过，不发上游请求、不写库、不出现在输出里；`synced`、`auth`、`error` 与 `checked_at` 为 0 的照常探；`--refresh` 或 `--max-age 0` 忽略新鲜度全部重探。该账号库里出现 `usable` 时，把库里**全部** `usable` 写成该账号的 `accounts.models`（不只是本次探到的），选号从此按该列表。没有模型行或带 `--sync` 时先同步；`--model` 只探一个模型名，`--limit` 截断的是跳过之后剩下的待探模型，`--concurrency` 默认 4 且不超过待探模型数；待探模型被全部跳过时仍按库里 `usable` 回写 `accounts.models`，该账号算作已满足；任一账号出现 401 即停止后续账号并返回错误；所有账号都没探出可用模型时退出码 1 且不改任何 `accounts.models`，错误信息带本次探活数与跳过数 | 表格（账号 / 模型 / 状态 / HTTP / 耗时 / 备注）与该账号的「本次探活 N 个 / 跳过 M 个」和可用 / 无权 / 不存在 / 失败计数；`--json` 是数组，每项 `account`、`model`、`status`、`http_status`、`latency_ms`、`message`，只含本次被探的模型，计数只出现在人读输出里。人读输出在 stderr 用单行原地刷新（`\r`）打实时进度，形如 `ds1 探活中 37/94  deepseek-v4.1-flash`：账号名、已完成 / 待探总数、最近完成的那个模型（并发大于 1 时显示刚回来的那个），被跳过的模型不计入总数；账号探完补一个换行，随后照旧打印汇总行与表格。进度只在 stderr 是终端时开，非终端（管道、CI、测试）与 `--json` 一个字节都不打，标准输出始终只有结果 |
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
2. 按模型名取候选账号：`enabled=1`、模型列表包含该模型（列表为空表示该账号不提供任何模型，不参与选号）、冷却已过。没有任何账号声明提供该模型时返回 404，错误消息说明「没有账号提供模型 X」并提示下一步：`switchboard model ls` 看现有清单、`switchboard model probe` 探活；错误类型按入站协议取：anthropic 入站是 `not_found_error`，两条 openai 入站是 `invalid_request_error`。有账号提供但全部在冷却或被停用时不算这种情况，仍按第 8 步返回 503。
3. 按 `priority` 升序，同一会话（见 5.3）优先沿用上次的账号。
4. 解密凭据，按该账号该模型的有效出站协议构造上游请求：入站协议与有效出站协议相同则直通，否则经中间表示转换（见 5.4）。上游路径、凭据头、请求体编码都跟有效出站协议走。
5. 发上游请求，读响应头。上游返回 429、5xx 或超时，且尚未向客户端写出任何字节时，把该账号打入冷却，换下一个候选。
6. 已开始向客户端输出后不再换号；上游中断则向客户端发一个错误事件并结束本次请求。直通与转换两条路径在这里行为一致，判据同为「有没有见到上游的收尾帧」：收尾帧没到（读到这里就干净 EOF 也算）就是断流，两条路径都补一个错误帧加该入站协议的合法结尾，并把这一次请求记成失败；收尾帧到齐的完整流不追加任何字节。收尾帧按帧里的 `type` 字段或 SSE 的 `event` 名判定，不按字节子串（见 5.4）。
7. 解析响应里的用量字段，写一行请求记录。
8. 所有候选账号都失败（上游报错、都在冷却、都被停用），返回 503，响应体带最后一次上游错误的摘要；这与第 2 步的 404 不是一回事：404 是清单里没有账号提供这个模型，503 是提供了但这一次全都打不通。

### 5.3 选号与失败转移

- 候选集合由模型名决定。模型名用请求里的 `model` 字段原文匹配，不做别名归一（第一版）。`accounts.models` 为空表示该账号不提供任何模型，不参与选号：刚建号、或 `model probe` 零可用的账号在探出可用模型之前不会被选中，否则它会接下任何模型名，把请求打到一家根本不提供该模型的上游；`account test` 与 `model probe` 不受此影响，仍可单独探活。`model sync` 只刷新 `account_models`，不改变选号依据；`model probe` 探出可用模型后才把列表写进 `accounts.models`，选号此后按新列表走。
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

流式在转换路径上走增量：上游帧先解码成 `text`、`tool_start`、`tool_args`、`stop`、`error` 增量，再由入站协议的编码器编成客户端事件。同协议直通时逐行转发原始字节，只用 scanner 记账。

两条路径判定断流用的是同一个判据：有没有见到上游的收尾帧。收尾帧按帧里的 `type` 字段或 SSE 的 `event` 名解析判定，不按字节子串——`{"type": "message_stop"}` 这种带空格、或者缩进成多行 `data` 行的合法帧同样是收尾帧。anthropic 认 `message_stop`，openai-chat 认 `[DONE]`，openai-responses 认 `response.completed`、`response.incomplete`、`response.failed`。

收尾帧到齐的完整流不追加任何字节，直通的字节仍与上游逐字节相同。收尾帧没到（包括帧边界上干净 EOF）就是断流，已写出的字节一个不动，只在末尾追加该入站协议的合法结尾：anthropic 补 `event: error` 再补 `message_delta`（带已读到的 usage）与 `message_stop`，openai-chat 补一帧 `data: {"error":…}` 再补 `data: [DONE]`，openai-responses 补 `error` 与 `response.failed`。转换路径的错误帧走编码器，形状跟着入站协议走：入站是 responses 时终态是 `response.failed`（`status=failed`），不是 `response.completed`——上游出错却告诉客户端「已完成」会与 `requests` 里的 `ok=0` 自相矛盾；正常结束仍发 `response.completed`。这一次请求在两条路径上都记成失败，`error` 写明收尾帧没到或解码出错；上游自己已经发过错误帧时，转换路径不再重复补一条，`error` 写上游的原话。追加之前判断帧边界时两种行尾都认（`\n\n` 与 CRLF 流的 `\r\n\r\n`），已经结束了一帧就不再补空行。

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

`model probe` 的 `max_tokens=1` 调用同样记行：每次上游调用写一行，探活也不例外。换协议重试的中间尝试也各记一行，它们的 `ok` 为 0，`error` 以「换协议前的尝试，未作结论」开头并带上那次的上游错误摘要，与判定行区分开；一次 403 触发三次调用就是三行，账实一致比行数好看重要。这类行的取值：`ts` 是这次调用的开始时间；`inbound` 与 `outbound` 都取这一行实际使用的协议（每一行各自是那次试的协议），探活就是「用这个协议直连上游」；`model_requested` 与 `model_used` 都取被探的模型名；`account_id` 取账号 id；`http_status` 与 `ok` 取上游响应，`ok` 表示 2xx，网络错误时 `http_status` 记 0；`in_tokens`、`out_tokens`、`cache_read`、`cache_write` 都由这次所用协议的 usage scanner 从响应体读出：anthropic 的 scanner 四项齐全（`cache_read_input_tokens` 与 `cache_creation_input_tokens`），chat 与 responses 只读得到缓存读（`cached_tokens`），`cache_write` 记 0；非 2xx 与网络错误记 0；`latency_ms` 是这次请求的耗时；`error` 是错误摘要，与探活结果一样过 `scrubSecret`，凭据不进库。这类行因此也进 `usage` 的汇总，探活的花费不会被漏掉一部分。

估算花费需要价格表；价格表第一版放在账号上（`--price-in / --price-out`，单位每百万 token），缺省则花费记为 null，不猜价格。

### 5.6 凭据

AES-256-GCM 加密后入库，密钥在 `secret.key`。这个方案防的是「备份文件、误贴日志、共享磁盘快照」这类泄漏，不防同一台机器上以同一用户身份运行的程序。文档里明确写出这一点，不暗示更强保护。

换 key 走 `account set --key`，不删账号：删号会连带删掉 `account_models` 里的探活结果与协议发现，请求记录也会失去账号归属。换 key 是同一个事务里的三件事：覆盖 `key_cipher`、清掉旧凭据留下的冷却与失败计数、把账号重新启用——那些失败是上一个凭据的结论，留着会让新 key 一上来就排在候选之外。

### 5.7 模型同步与探活

手填 `--models` 的问题在于上游一改模型名就失效：账号看上去正常，请求却全打在 404 或 403 上，只能对着上游文档猜。模型列表改成从上游拉取，再逐个探活，只保留真正能用的名字。

两个命令分工明确，一个管「上游有哪些模型」，一个管「哪几个真能用」。两者都按账号处理，并支持同样的筛选：`--provider` 精确匹配账号的 `provider` 字段，`--account` 精确匹配账号名，两个都给时账号必须属于该厂商，否则报错；不带筛选参数时处理全部账号，按账号逐个进行。

`model sync [--provider 厂商] [--account 账号] [--endpoint <路径>]` 只解决前者：

- 模型列表地址按这个顺序取，纯规则在 `store.ResolveModelsPath`：`--endpoint` 给的地址（只作用于这一次调用，压过下面全部）→ 厂商级 `providers.models_path` 覆盖 → 账号协议那条路由的 `models_path` → 同厂商按协议名排序的第一条非空 `models_path`；每一项都可能是相对路径或整条地址（见 5.8），整条地址直接访问、不再拼账号的 `base_url`；厂商行存在且声明了路由、但一个非空 `models_path` 都没有时报错，提示用 `--models` 手填或 `provider set --models-path <地址>` 补上；厂商行不存在或一条路由都没声明时按 `base_url` 后缀猜（见 5.8）。
- 凭据头与地址一起定：地址来自哪条路由，就用那条路由的协议选凭据头（anthropic 用 `x-api-key`，另两家用 `Bearer`）；厂商级覆盖与 `--endpoint` 没有自己的协议，按账号协议发。deepseek 的 anthropic 账号取到的是 chat 路由上的 `/v1/models`，所以必须发 `Bearer`——按账号协议发 `x-api-key` 只会换回 401/403，还把错误指到「凭据无效」上。
- `model sync --endpoint <地址>` 只影响这一次调用：值是相对账号的 `base_url` 的完整路径，也可以是整条地址（账号行固化地址，见 5.8），相对路径按 5.8 的规则规整（`v1/models`、`//v1//models/` 都落成 `/v1/models`）、整条地址校验后直接访问、不再拼 `base_url`；给了就忽略厂商覆盖、路由的 `models_path` 与 `base_url` 兜底，作用于被选中的每个账号。
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

- 账号的 `protocol` 一定要试一次，那一次的结论先存着，它就是「按账号配置打」的真相：`401` 判定与候选用完后的结论都取它。它不一定排第一个（见下面「顺序」那条），所以循环按协议名认住这一行，而不是按位置。
- 账号协议那一次失败时，只有可能只是路径不对的才换协议再试：`404`（路径不存在）、`405`（方法不允许）、`403`（有的网关按路径拒，commandcode 对非 `/messages` 的 `claude-*` 就是 403），以及 `400` 且错误文本指向端点、路径或「不支持该协议」。其余失败（例如明确说模型不存在、`5xx`、网络错误）直接按上表判定，不再花调用。
- 换协议之后的那几次失败都不作数：不管是什么原因，都接着试剩下的候选，直到候选用完，结论仍回账号协议那次。中途停手会让排在后面的候选永远试不到，而换协议后的 `401` 本来就是常态（凭据头形状不对）。
- 顺序：库里已经发现过这个模型的协议排最前，否则账号协议最前；其余按候选列表的顺序。库里已有的发现结果排前面是为了让 `--refresh` 重探已知模型时不再白打一次 `404`。顺序固定，同一模型多次探活得到同一个结论，不随运行顺序变化。
- 候选协议由 `gateway.Options.RouteProtocols`（resolver）给出：入参是账号的厂商与模型名，默认实现取该厂商在 `provider_routes` 里声明过的协议（见 5.8）；没注入、或该厂商一个路由都没声明时它们就是三种线协议全试。resolver 负责去重与校验，只认三种线协议，出现别的值直接报错——静默落进 chat 分支会把一条配置错误变成对错误地址的上游调用。循环本身不认识厂商，也不会对着不存在的端点白打一次。
- 第一个 `2xx` 就是结论：状态记 `usable`，把这次用的协议写进 `account_models.protocol`，剩下的协议不再试。
- `401` 一律不换协议：凭据问题与路径无关，照旧停用账号并立即停止整次探活。这一条只判账号协议那一次；换协议之后的 `401` 多半是凭据头形状不对（anthropic 用 `x-api-key`，另两家用 `Bearer`），既不停用账号也不算凭据被拒。
- 三种协议都没试出 `2xx` 时，结论取账号协议那一次的判定（状态、HTTP 状态与错误文本），`protocol` 保持原值不动。协议列只在发现成功时写，一次失败不会把之前测出的协议抹掉，也不会凭空写入一个。

花费：先用账号协议试，成功时只花一次调用，只有失败才会多花一到两次。同一模型重复探活的结果稳定，代价最多是每次 `--refresh` 多一两次 `max_tokens=1` 的调用，跟 `--max-age` 省下的调用相比是小头。每次上游调用都在 `requests` 里各留一行（见 5.5），打出几次就记几行。

`account test` 只判凭据与连通性，模型是否可用由 `model probe` 决定，所以它先 `GET` 模型列表：地址按 `model sync` 同一套规则解析（见 5.8），凭据头跟取到地址的那条路由的协议；2xx 就是「凭据能用、上游连得上」，顺带报出上游模型数，这一条不花 token，也不依赖某个模型名能不能用。列表接口不可用时（404、405、403，或这家声明了路由却一个 `models_path` 都没有）才退回一条 `max_tokens=1` 的请求：按账号的 `protocol` 打，不看模型上的协议发现结果，模型取 `accounts.models` 第一项；模型列表为空的账号（刚建好、还没同步过）取不到 `accounts.models[0]`，回退到默认模型名 `default` 并在备注里说明这一句，既不 panic 也不要求先同步：凭据好不好用得先能试出来。两阶段的状态处理一致（见 5.3），备注与 `--json` 的 `stage` 写清结论出自哪一步，因此「列表 404 之后模型 403」与「列表接口自己 403」不会读成同一个结论。

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

厂商与路由。转发地址同样按 `(账号 provider, 协议)` 查路由（见 5.8），不再由 `base_url` 的后缀猜：同一家上游的 chat 与 anthropic 路径可以完全不同，猜后缀必然在其中一种协议上猜错。

探活结果反过来成为选号依据：一个账号只要库里至少一个模型是 `usable`，就把库里全部 `usable` 写成该账号的 `accounts.models`，5.2 与 5.3 的候选集合从此按这份列表算。取全库而非本次结果，是因为 `--limit` 与 `--model` 都可能只探一部分：只探两个却把另外三个之前探通的模型从选号里除掉，属于退化。某账号零个可用模型时只写 `account_models`，不改它的 `accounts.models`，账号原有列表保持不变，避免一次网络抖动就把账号清空。所有账号都没探出可用模型时退出码 1，且不改任何 `accounts.models`。

### 5.8 厂商与路由

出站地址不由代码里的厂商表决定，也不靠 `base_url` 末尾有没有 `/v1` 猜后缀：同一个 `base_url` 上不同协议的路径并不一样，猜出来的地址对其中一种协议一定是错的。deepseek 的 chat 在 `/v1/chat/completions`，anthropic 在 `/anthropic/v1/messages`；commandcode 的 base 是 `https://api.commandcode.ai/provider/v1`，chat 在 `/chat/completions`、anthropic 在 `/messages`，模型列表在 `/models`。

- `providers` 一行提供 `base_url`、备注、一列默认协议与一列模型列表路径覆盖；协议的路径细节一律在 `provider_routes` 上，厂商行不存转发路径。
- `provider_routes` 按 `(厂商, 协议)` 存完整路径 `path` 与模型列表路径 `models_path`。`path` 是相对路径时，完整地址是 `base_url` 去掉末尾斜杠后直接拼它；`path` 是整条地址时直接用它，不做任何猜测与补全（两种写法见下一条）。
- 路由的 `path`、路由与厂商的 `models_path`、`model sync --endpoint` 三处收两种写法，判定与解释只有一份（`store.IsFullAddress` 与 `store.NormalizeEndpoint`）：含 `://` 的是整条地址，用 `url.Parse` 校验 scheme 为 `http` 或 `https`、主机名非空，规整时 scheme 归一成小写、路径合并重复斜杠去尾斜杠，授权部分、查询串与 `#` 之后原文保留，使用时不再拼 `base_url`；不含 `://` 的是相对路径，走 `NormalizePath` 拼账号的 `base_url`。整条地址让同一厂商的不同协议落在不同域名上：openai 兼容端点在 `https://openai.example.com/v1/chat/completions`、anthropic 端点在 `https://claude.example.com/v1/messages`，此时 `base_url` 只是其中一家的地址（仍必填，账号行要用它兜底）。`https://`、`https:///x` 这类只有 scheme 没有主机名的写法报错，`ftp://host/x` 这类其它 scheme 同样报错，错误文案用中文指出缺什么；写入侧（`CreateProvider`、`SetProvider`、`AddProvider`）碰到坏整条地址整笔拒绝、一个字段都不落库，读取侧（`joinEndpoint`、`modelsEndpoint`）把存量的脏值原样交给 `http.NewRequest` 报错，不改写用户数据。
- 相对路径只规整、不拒绝输入，规则是 `store.NormalizePath`（由 `store.NormalizeEndpoint` 在取值不含 `://` 时调用）：去掉首尾空白、把连续斜杠合并成一个、补上恰好一个前导斜杠、去掉末尾斜杠；空串、只有斜杠、只有空白的输入一律视为空。`v1/chat/completions`、`//v1//chat//completions`、`/v1/chat/completions/` 因此是同一个地址，`--openai`、`--models-path`、`model sync --endpoint` 都不再要求「必须以 `/` 开头」。写入侧（`CreateProvider`、`SetProvider`、`AddProvider` 的 `path` 与 `models_path`）与读取侧（拼转发地址、拼模型列表地址、`--endpoint`）走同一个入口：库里存的永远是规整形态，读取侧拼地址时再过一遍，历史库里的不规整写法因此也拼得出正确地址。理由：路径是照着上游文档抄的，多一个或少一个斜杠是写法差异，不是用法错误；为此报错只会让人先改命令再重敲一遍，得到的仍是同一个地址。真正的用法错误是非空字段规整之后为空（删除语义归 `provider unset`），以及整条地址校验不过，那两条仍然报错。
- `base_url` 同理只规整路径、不拒绝输入（`store.NormalizeBaseURL`），校验用 `url.Parse` 做：scheme 只认 `http` 与 `https`，大小写不敏感，落库时归一成小写；缺主机名是错误——`https://` 里没有地址，规整出来的 `https:` 拼不出可用的请求地址，不能当成一条 `base_url` 落库；缺 scheme 与别的 scheme 同样报错。规整只作用在路径部分：合并重复斜杠、补一个前导斜杠、去掉末尾斜杠，`https://api.example.com//api//paas//v4/` 落成 `https://api.example.com/api/paas/v4`——真实存在的前缀原样保留。授权部分（含端口与用户信息）、查询串与 `#` 之后的内容一个字不动：那里的连写斜杠是值的一部分，合并会改掉语义；`..` 与 `.` 段也原样保留，静默改写等于把地址换成另一个地址。拼地址时两侧（`base_url` 的路径与路由的 `path`）都过这套规则，最终地址的路径里不出现 `//`；`http://`、`https://` 里那一对斜杠不属于要合并的范围。
- 协议由三个 flag 声明：`--anthropic`、`--openai`（openai-chat）、`--responses`（openai-responses）。没有 `--gemini`：gemini 协议进出都没实现，留一条永远用不上的路由只会误导。`provider add` 用它们新建，`provider set` 用它们覆盖或新增一条路由，删掉一条路由是 `provider unset <名称> <协议>`：传空值是用法错误，删除只在 `unset` 里发生。`--default-protocol` 认命令行写法 `anthropic`、`openai`、`responses`，也认线协议全名 `openai-chat`、`openai-responses`，两种写法落到同一个线协议，输入 `openai` 就是 chat；错误文案用命令行写法举例。
- `models_path` 的推导只发生在写入侧，读取侧不拿它重算。写入时没给模型列表地址的路由按规则补默认值（`store.DeriveModelsPath`）：openai-chat 路由把尾部 `/chat/completions` 换成 `/models`（`/v1/chat/completions` → `/v1/models`、`/chat/completions` → `/models`；整条地址则在同一个 scheme + host 上换尾段，`https://openai.example.com/v1/chat/completions` → `https://openai.example.com/v1/models`，查询串与 `#` 之后跟着原样带过去，不换域名），其它协议留空，表示这条协议没有自己的列表接口、读取时回退到同厂商其它协议的非空值。推导值跟着路径走：改路径时只有「空」或「原本就是旧路径的推导值」才换成新推导值，手工填过的值不动。内置 `anthropic` 厂商只有 anthropic 一条路由，没有 openai 路由可以推导，它的 `/v1/models` 因此不会因为重写那条路由而消失——读取侧推导会让 anthropic 型账号彻底失去模型列表来源。
- `providers.models_path` 是厂商级覆盖：非空时 `model sync` 直接用它，不看路由；空串表示不覆盖、按路由取。它同样收相对路径与整条地址两种写法。要改一家上游的列表地址又不想逐条写 `models_path` 时用它。
- 模型列表的凭据头跟取到路径的那条路由的协议走（见 5.7），不是账号协议：deepseek 的 anthropic 账号用的是 chat 路由上的 `/v1/models`，就得发 `Bearer`。
- 账号行仍固化自己的 `base_url`（`account add` 时写入厂商默认值）。改厂商表只影响之后新加的账号，已有账号的地址不变。
- 厂商行存在但没有该协议的路由，以及账号的 `provider` 在表里根本不存在（用户显式 `--base-url` 的自建地址），都退回原来的后缀猜测：末尾是 `/v1` 就补 `/chat/completions`、`/messages` 或 `/responses`，否则补 `/v1` 再接同样的后缀；模型列表同理补 `/models` 或 `/v1/models`。自建地址因此仍然可用。
- 探活的候选协议跟着路由收紧：只用该厂商在 `provider_routes` 里声明过的协议，不去打这家没有声明的路径；没有厂商行的账号仍三种全试。顺序（已发现的协议、账号协议在前，其余按协议名排序）见 5.7。读库失败时报错，不放宽成三种全试——放宽等于去打这家没声明的路径，把一次读库故障变成对错误地址的上游调用。
- 默认协议：显式值优先，但必须落在这家已声明的路由上；否则按固定顺序 `openai-chat` → `openai-responses` → `anthropic` 取第一条已声明的路由，一条路由都没有时为空。`account add` 用它补账号的 `protocol`，`provider ls` 也显示它，两处同一个规则。规则是纯函数 `store.ResolveDefaultProtocol`，读库入口是 `store.EffectiveProviderProtocol`：厂商不存在返回 `ErrProviderNotFound`，厂商在但一条路由都没有时返回空串与 `ErrProviderNoRoutes`，提示文案由调用方写，`cli` 不再自己算这条回退。**不能靠协议名字典序兜底**：deepseek 与 commandcode 都同时声明了 `openai-chat` 与 `anthropic` 两条路由，字典序把 `anthropic` 排在前面，默认协议就会从升级前的 `openai-chat` 变成 `anthropic`，这两个厂商的账号在升级前后默认协议不一致。升级前的默认值因此由迁移 `0007` 逐家显式种进 `default_protocol`：`anthropic` 自己是 `anthropic`，其余 7 家是 `openai-chat`。
- 默认协议的写入侧与读取侧分工：`provider add` 的 `--default-protocol` 必须落在这次声明的路由上，`provider set` 改完之后必须仍然指向一条存在的路由，`provider unset <协议>` 删掉的正好是默认协议时也整笔拒绝（提示先改默认协议再删），两处都是同一个事务，指不到就一条都不落库。读取侧对历史脏数据（旧版本删路由时没清默认协议）按固定顺序回退到仍然存在的路由，不静默交给后缀猜测。
- `base_url` 的 `http://`、`https://` 前缀在 `store` 的三个写入口（`CreateProvider`、`SetProvider`、`AddProvider`）里校验并规整（合并重复斜杠、去掉末尾斜杠），整条地址的 `path` 与 `models_path` 用的是同一套校验（`store.NormalizeEndpoint`），`cli` 那层不再是唯一防线：脏地址一旦落库，转发与模型同步都会拿它拼路径。读侧拼地址用的也是同一个 `store.NormalizeBaseURL`。
- `provider add` 只新建，`provider set` 只改传入的字段；两者都在一个事务里写厂商行与路由，失败时不留半条记录。`provider rm` 删厂商行与它的全部路由，仍有账号指过来时先列出这些账号并拒绝，`--force` 才删；账号行只存 `provider` 字符串，删厂商不影响已有账号的 `base_url`。
- 凭据头仍按协议而不是按厂商：anthropic 用 `x-api-key` 加 `anthropic-version`，另两种用 `Bearer`。

`base_url` 与 `path` 拼出来的完整地址（`path` 是整条地址时直接用，不拼 `base_url`），几行示例：

| 厂商 | 账号协议 | `path` 的写法 | 完整地址 |
| --- | --- | --- | --- |
| deepseek | openai-chat | `/v1/chat/completions` | `https://api.deepseek.com/v1/chat/completions` |
| deepseek | anthropic | `/anthropic/v1/messages` | `https://api.deepseek.com/anthropic/v1/messages` |
| commandcode | openai-chat | `/chat/completions` | `https://api.commandcode.ai/provider/v1/chat/completions` |
| commandcode | anthropic | `/messages` | `https://api.commandcode.ai/provider/v1/messages` |
| split | openai-chat | `https://openai.example.com/v1/chat/completions` | `https://openai.example.com/v1/chat/completions` |
| split | anthropic | `/v1/messages` | `https://claude.example.com/v1/messages`（`base_url` 是这一家） |

种子数据在迁移 `0006` 里，默认协议在迁移 `0007` 里，厂商级模型列表覆盖那一列在迁移 `0008` 里，都不在代码里。`providers` 表为空不算错误：用户可以把种子厂商删光，此时所有账号走兜底。维护用 `provider` 子命令（见 4）。

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

CREATE TABLE providers (
  name             TEXT PRIMARY KEY,
  base_url         TEXT NOT NULL,           -- 账号兜底的地址，路由写相对路径时靠它拼
  note             TEXT NOT NULL DEFAULT '',
  default_protocol TEXT NOT NULL DEFAULT '',
  models_path      TEXT NOT NULL DEFAULT '' -- 相对路径或整条地址，空串表示不覆盖
);

CREATE TABLE provider_routes (
  provider    TEXT NOT NULL,
  protocol    TEXT NOT NULL,
  path        TEXT NOT NULL,                -- 相对 base_url 的路径，或整条地址（含 ://）
  models_path TEXT NOT NULL DEFAULT '',     -- 同上，空串表示回退到同厂商其它协议
  PRIMARY KEY (provider, protocol)
);
```

`account_models` 由迁移 `0003` 建立，`status` 取值就是 `synced`、`usable`、`forbidden`、`notfound`、`auth`、`error`（见 5.7）。`accounts.fail_streak` 由迁移 `0004` 加入，只用于冷却翻倍，写入口只有 `RecordFailure`（加一）、`ClearFailure`（清零）与 `RotateKey`（换凭据时清零，见 5.6）。`accounts.models` 仍是选号实际读的那份列表。删除账号时 `DeleteAccount` 在同一个事务里删掉它的 `account_models` 行：SQLite 的 rowid 会被复用，遗留子行会让新账号继承旧账号的探活状态。

`account_models.protocol` 由迁移 `0005` 加入，记录「这个模型该用哪种协议」，取值同样是 anthropic、openai-chat、openai-responses。**空字符串表示继承账号的 `protocol`**：有效出站协议是「列非空就用列，否则用账号的 `protocol`」，纯规则在 `store.ResolveProtocol`，读库入口是 `store.EffectiveProtocol(accountID, accountProtocol, model)`，行不存在与列为空都当成继承，不报错。转发与展示只读这个有效协议，不在调用方拼规则；`cli` 要显示协议时也只调它。

这个列只有一个写入口 `SetModelProtocol`，由探活发现协议后调用（见 5.7）。`SetModelStatus` 只改 `status`、`http_status`、`error`、`checked_at`，不动 `protocol`；`ReplaceModels` 的 `ON CONFLICT DO NOTHING` 也只给新行写默认值，同步不会覆盖已发现的协议。旧库升上来时整列默认空串，也就是全部回退到账号协议，行为与 M7 一致。

`providers` 与 `provider_routes` 由迁移 `0006` 建立并种入已知厂商，`providers.default_protocol` 由迁移 `0007` 加入并显式种入升级前的默认协议，`providers.models_path` 由迁移 `0008` 加入（老行一律空串，表示不覆盖），行里没有凭据，只有地址与路径。`providers` 一行只提供 `base_url`、备注、默认协议与模型列表覆盖，协议细节全在 `provider_routes` 的 `(厂商, 协议)` 上；两者之间没有外键，所以删厂商行时必须连带删它的路由行，`DeleteProvider` 在一个事务里一起做。`providers` 表为空不算错误：种子行可以被用户删光，此时所有账号回退到后缀猜测。账号行仍固化自己的 `base_url`，改厂商表只影响之后新加的账号。写入口是 `CreateProvider`（`provider add`，名称已存在时报 `ErrProviderExists`，不隐式更新）与 `SetProvider`（`provider set` 与 `provider unset` 的共同入口：patch 里为 nil 的字段不动，`Routes` 里传空串表示删掉那条路由——`provider unset <名称> <协议>` 就是这么调的；命令面上空值仍是用法错误）：两者都在一个事务里写厂商行与路由，`CreateProvider` 至少要一条路由，`SetProvider` 删到一条不剩时报错，另有一个 `AddProvider`（upsert，允许零路由），它是第二条 upsert 入口：`cli` 不走它，生产代码里也没有其它调用者，现在只有 `store` 自己的用例在用；新建与改仍分别走 `CreateProvider`、`SetProvider`。读路由只有 `LookupProviderRoute` 一个入口，转发、模型同步与探活都经过它。

`provider_routes.models_path` 是写入侧的默认值，不是读取侧的唯一来源：`CreateProvider`/`SetProvider` 写一条没有模型列表地址的路由时按 `DeriveModelsPath` 补（只给 openai-chat 补，尾部 `/chat/completions` 换成 `/models`），其它协议留空表示「这条协议没有自己的列表接口」。空串在读取时回退到同厂商其它协议的非空值，纯规则在 `store.ResolveModelsPath`：厂商级覆盖 → 账号协议那条路由 → 协议名排序的第一条非空值，返回路径时一并返回这条路径所属的协议，模型同步按它选凭据头。推导不放在读取侧，因为内置 `anthropic` 厂商只有 anthropic 一条路由，读时推导会让 anthropic 型账号彻底失去模型列表来源。

`providers.default_protocol` **空字符串表示这家没有显式默认协议**：有效默认协议按固定顺序 `openai-chat` → `openai-responses` → `anthropic` 回退到第一条已声明的路由（用户自建的厂商通常不填）。纯规则在 `store.ResolveDefaultProtocol`：显式值必须落在已声明的路由上才用它，指不到时同样按固定顺序回退——历史脏数据因此不会静默交给后缀猜测。读库入口是 `store.EffectiveProviderProtocol(provider)`：厂商不存在返回 `ErrProviderNotFound`，厂商在但一条路由都没有时返回空串与 `ErrProviderNoRoutes`，文案留给调用方。写入侧保证不产生悬空值：`CreateProvider` 的默认协议必须落在这次声明的路由上，`SetProvider` 改完之后必须仍指向一条存在的路由，否则整笔事务拒绝；`SetProvider` 收到空的 `DefaultProtocol` 表示清掉显式值、回到固定顺序，这一条只由 `provider unset <名称> default-protocol` 触发。旧库升上来时 8 家种子厂商由 `0007` 拿到显式默认协议，用户自建的厂商保持空串。

`providers` 的三个写入口（`CreateProvider`、`SetProvider`、`AddProvider`）都在 `store` 里校验 `base_url` 的 scheme 与主机名并规整（只合并路径里的重复斜杠、去尾斜杠，查询串与 `#` 之后的内容不动，`store.NormalizeBaseURL`），`cli` 那层不再是唯一防线。`path` 与 `models_path` 三列同样在写入侧经 `store.NormalizeEndpoint` 解释后落库：相对路径只规整（路径写法不拒绝，`v1/chat/completions` 与 `/v1/chat/completions/` 落成同一个值），整条地址（含 `://`）校验 scheme 与主机名后按整条地址落库，读取侧拼地址时再用同一个入口过一遍，历史库里的不规整写法因此也拼得出正确地址。两列因此可能存整条地址，也可能存相对路径，判定只看有没有 `://`（`store.IsFullAddress`）。

一次尝试的账与它的结论在同一个事务里落库：`RecordAttempt` 先写 `requests` 一行，再按 `AttemptState` 记冷却（`fail_streak` 加一）、清冷却（清零）或停用账号，中途失败整笔回滚。冷却、清零与停用的 SQL 只在 `recordFailureTx`、`clearFailureTx`、`setEnabledTx` 里，`RecordFailure`、`ClearFailure`、`SetEnabled` 与 `RecordAttempt` 都调它们，写入口仍然只有一处。

迁移用 `internal/store/migrations/*.sql`，按文件名前缀解析出的版本号升序执行，`PRAGMA user_version` 记录已执行版本；版本号来自文件名而不是文件在排序结果里的下标，改名或删文件不会让老库的升级整体错位。

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
| M9 | 厂商与路由进库：`providers` 与 `provider_routes`（迁移 `0006`），出站地址按 `(厂商, 协议)` 的 `path` 拼，模型列表按 `models_path` 取，探活候选协议按厂商声明过的路由收紧；没有厂商行的账号保留后缀猜测 | 空库跑到最新版本后种子逐行对上，11 条路由拼出的完整地址与 `models_path` 全部正确；deepseek 账号下 anthropic 模型打到 `/anthropic/v1/messages` 并带 `x-api-key`，chat 模型打到 `/v1/chat/completions` 并带 `Bearer`；只有一条路由的厂商探活失败时上游只收到一次请求，deepseek 试两条；`models_path` 为空时回退到同厂商第一条非空路径，全空时报错文案带「用 --models 手填」；自定义 `base_url` 且 `provider` 不在表里时仍按后缀猜测；空库跑到最新版本与从 `0006` 升级两条路径下，8 家的 `default_protocol` 逐行对上，显式默认优先于固定顺序兜底，deepseek 与 commandcode 的默认协议仍是 `openai-chat`（单靠协议名字典序会给 `anthropic`） |
| M10 | 厂商命令面与模型列表路径：`provider add` 只新建、`provider set` 增量改（空值是用法错误，删除只在 `provider unset`）、默认协议按固定顺序 `openai-chat` → `openai-responses` → `anthropic` 兜底，`models_path` 的写入默认值与厂商级覆盖（迁移 `0008`），模型列表的凭据头跟着取到路径的那条路由走，`model sync --endpoint`，`base_url` 校验下沉到 store，探活读库失败报错而不放宽候选协议 | 空库与从 `0007` 升级两条路径都到最新版本：升级后 `provider_routes.models_path` 还在、路由数据一个字段不动、`providers.models_path` 全场空串；`provider add` 重名报 `ErrProviderExists` 并提示 `provider set`，一条路由都不给报错；`provider set` 只改传入的字段，协议 flag 传空串是用法错误、删除走 `provider unset`，删到一条不剩报错，默认协议悬空整笔拒绝（同一次里一起改则放行）；`/v1/chat/completions` → `/v1/models`、`/chat/completions` → `/models`、anthropic 路由留空，重写内置 `anthropic` 厂商的 `/v1/messages` 之后它的 `/v1/models` 仍在；deepseek 的 anthropic 账号同步模型列表打到 `/v1/models` 并带 `Bearer`（不是 `x-api-key`）；`--endpoint /v1/model` 压过厂商覆盖与路由，`v1/model`、`//v1//model/` 这类写法被规整成同一个地址、不报错；默认协议显式优先、缺省按固定顺序、指不到路由拒绝、悬空的历史值按固定顺序回退 |
| M11 | 端点两种写法的命令面与凭据轮换：`--anthropic`、`--openai`、`--responses`、`--models-path` 与 `model sync --endpoint` 一律收相对路径或整条地址，校验只有 `store.NormalizeEndpoint` 一份，cli 把它的错误包成用法错误；`account set --key`（`update` 同义）换凭据并在同一事务里清掉旧凭据的冷却与失败计数、重新启用账号 | `provider add` 用整条地址声明 openai 路由后，`provider ls --json` 的 `path` 就是整条地址，转发与 `model sync` 都打到那个 host、`base_url` 那台一次请求都不收；`https://`、`https:///x`、`ftp://host/x` 与空值在 add、set、`--models-path`、`--endpoint` 四处都退出码 2 且不落库；`model sync --endpoint <整条地址>` 打到该地址，下一轮回到路由的 `models_path`；`account set --key` 之后 `key_cipher` 变化、旧密文不再存在，`cooldown_until`、`fail_streak`、`last_error` 清零且 `enabled=1`，`accounts.models` 与 `account_models` 一个字段不动，新 key 能转发、`--json` 不含明文、`update` 与 `set` 输出一致；`account test` 对模型列表为空的账号退出码 0，用默认模型名探活且不 panic |
M0 到 M11 的代码已落地，端到端由 `internal/cli` 的 `TestWalkingSkeleton` 用假上游验证：初始化、加账号、`call`、`log tail`、`usage`、`doctor` 全链路走通。协议矩阵由 `internal/gateway/matrix_test.go` 与 `matrix_stream_test.go` 验证：9 种组合的非流式与流式各跑一遍，同协议直通的「不改字节」「保留陌生字段」与跨协议的「丢弃陌生字段」单独有用例。协议降级到模型的那部分由 `internal/gateway` 的 `TestProbeModel*` 与 `TestForwardUsesModelProtocol` 验证，迁移兼容由 `internal/store` 的 `TestMigration0005*`、`TestMigration0006*`、`TestMigration0007*` 与 `TestMigrationsRunToLatestFromEmptyDB` 验证；厂商路由的解析、探活收紧与模型列表回退由 `internal/store` 的 `TestProvider*` 与 `internal/gateway` 的 `TestForwardUsesProviderRoute`、`TestEndpointFallsBackWithoutProviderRoute`、`TestProbeModelNarrowsCandidatesByProvider`、`TestProbeModelStartsFromDiscoveredProtocol`、`TestSyncModelsUsesProviderModelsPath`、`TestSyncModelsWithoutModelsPath` 验证。厂商写入口与模型列表路径由 `internal/store` 的 `TestCreateProvider*`、`TestSetProvider*`、`TestDeriveModelsPath`、`TestResolveModelsPath`、`TestRouteModelsPathDerivedOnWrite`、`TestStoreWritePathsValidateBaseURL`、`TestMigration0008*` 与 `internal/gateway` 的 `TestSyncModelsAuthFollowsModelsPathRoute`、`TestSyncModelsProviderModelsPathOverride`、`TestSyncModelsWithEndpointOverride`、`TestSyncModelsNoModelsPathAnywhere` 验证；路径与地址的规整由 `internal/store` 的 `TestNormalizePath`、`TestNormalizeBaseURL`、`TestCreateProviderNormalizesPaths`、`TestSetProviderNormalizesPaths` 与 `internal/gateway` 的 `TestJoinEndpoint` 验证；端点取值的两种写法（相对路径与整条地址）由 `internal/store` 的 `TestIsFullAddress`、`TestNormalizeEndpoint`、`TestDeriveModelsPathFullAddress`、`TestCreateProviderStoresFullAddressRoutes`、`TestSetProviderFullAddressRoutesAreIndependent`、`TestStoreWritePathsRejectBadFullAddress` 与 `internal/gateway` 的 `TestForwardUsesFullAddressRoute`、`TestForwardMixesFullAddressAndRelativeRoutes`、`TestSyncModelsFullAddressModelsPath`、`TestSyncModelsWithEndpointFullAddress`、`TestSyncModelsWithEndpointRejectsBadFullAddress` 验证；一次尝试的账与结论一起提交由 `internal/store` 的 `TestRecordAttemptWritesRowAndStateTogether` 验证；直通路径的断流结尾由 `internal/gateway` 的 `TestPassthroughStreamDisconnectKeepsLegalEnding`、`TestPassthroughCRLFTailStartsAtFrameBoundary` 逐协议验证，转换路径的干净截断与 responses 入站的 `response.failed` 由 `TestConversionStreamCleanTruncationIsFailure`、`TestConversionStreamResponsesInboundEndsFailed` 验证，收尾帧按 `type` 或 `event` 名判定（带空格、多行缩进、只写事件名）由 `TestPassthroughEndFrameShapeDoesNotMatter` 验证，完整流不被追加字节由 `TestProtocolMatrixStreaming` 的逐字节断言保证；端点两种写法在命令面上的行为与 `account set --key` 由 `internal/cli` 的 `TestProviderPathsAcceptFullAddress`、`TestProviderPathsRejectBadFullAddress`、`TestModelSyncEndpointFullAddress`、`TestModelSyncEndpointBadValues`、`TestAccountSetRotatesKey`、`TestAccountSetKeyFromStdin`、`TestAccountSetErrors`、`TestAccountSetJSONMasksKey`、`TestAccountUpdateIsSynonymForSet`、`TestAccountTestWithoutModels` 与 `internal/gateway` 的 `TestProbeEmptyModelsUsesFallbackModel`、`TestProbeEmptyModelsKeepsNoteOnFailure` 验证。上游是 `httptest` 假服务，M5、M6、M7、M8、M9 与 M10 里涉及真实厂商的那部分需要拿到 key 后手工验收。

已知未做，按已知行为处理：

- 估算花费依赖账号上的 `--price-in` 与 `--price-out`，没有价格时记为 0 并在 `usage` 末尾提示。
- 模型名不做别名归一，请求里的 `model` 必须与账号 `--models` 里的名字完全一致。
- 不带任何客户端配置改写：客户端里的 base URL、token、模型名都靠自己填。
- 探活并发默认 4，`--concurrency` 可以改，只是默认值，不会根据上游限速自适应。
- gemini 协议进出都没有实现：既没有入站路径，也不在账号的 `protocol` 可选值里，`provider add/set` 也没有 `--gemini`——留一条永远用不上的路由只会误导。
- 思考与推理内容在转换时丢弃：anthropic 的 `thinking` 块、chat 的 `reasoning_content`、responses 的 `reasoning` item 都不进中间表示。
- 音频与文件类内容丢弃，只搬运文本、图片、工具调用与工具结果四类块。
- responses 的内置工具（`web_search` 等）丢弃，工具定义只认 function。
- 跨协议只做图片地址的搬运，没有图片转写以外的多模态变换。
- 路径里 `//` 有真实含义的上游（少数 CDN 风格端点）目前无法表达：`store.NormalizePath` 会把连续斜杠折叠成一条，`provider add/set` 写进去的 `//` 落库就只剩一个 `/`；整条地址的路径部分（`https://host//a//b`）也过同一套折叠，同样表达不了这种路径（查询串与 `#` 之后不在折叠范围内）。
- 模型列表的响应形状只按 `{data:[{id}]}` 解析（也认 `data[].name`）。形状不同的厂商，例如 ollama 的 `/api/tags` 返回 `{models:[{name}]}`，尚未支持，这类厂商只能用 `--models` 手填。

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

包之间的依赖：`store` 为协议常量（`translate.ProtocolAnthropic`、`translate.ProtocolChat`、`translate.ProtocolResponses`）import `internal/translate`，只取常量，不调用编解码；`gateway`、`cli`、`account` 都经 `store` 与 `translate` 拿协议取值，不各自复制一份字符串。

外部依赖：只有 `modernc.org/sqlite`（纯 Go 实现，不需要 cgo，交叉编译不受影响），其余全部用标准库。命令行解析用 `flag`，不引入第三方 CLI 框架。

## 9 非目标

- 多租户、配额、充值、开票。
- 网页后台与桌面菜单栏。
- 逆向接口、把订阅网页版包装成 API。
- 改写其它客户端的配置文件，不论带不带备份。
- 跨机部署与集群。

## 10 需要外部信息才能定的点

- 各家上游的默认 base-url 与各协议的路径不再需要外部信息：迁移 `0006` 把已知厂商种进 `providers` 与 `provider_routes`，以后用 `provider ls|add|set|rm` 维护，新增厂商不必改代码重编译。新厂商的 `path` 要以它的文档为准：它是完整路径，程序只在 `base_url` 去掉末尾斜杠后直接拼接，不做补全；也可以写整条地址（含 `://`），此时直接用它、不再拼 `base_url`，同一厂商的不同协议因此可以落在不同域名上；模型列表路径没给时只给 openai-chat 路由按 `/chat/completions` → `/models` 推导；写法不同的上游用 `provider set --models-path` 给厂商级覆盖，路由级的值只由写入时推导产生，命令面不提供手工写入口。
- 各家模型名清单，以及某家是否兼容 `openai-chat`。
- 各家价格，用于估算花费。缺价格就记 null。
