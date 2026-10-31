# lax 设计

## 1 这是什么

本机上的模型总机。手上有几家模型的账号，客户端只连 `http://127.0.0.1:4567`，由 lax 挑账号、转协议、记用量。

入站收五种线协议，按路径区分：`POST /v1/messages` 是 anthropic，`POST /v1/chat/completions` 是 openai-chat，`POST /v1/responses` 是 openai-responses，`POST /v1beta/models/{model}:generateContent` 与 `:streamGenerateContent` 是 gemini，`POST /v1/systemone` 是 systemone。前四种之间可以互相转换，出站协议归属在模型上（见 5.7）；systemone 不参与转换，只与它自己互通（见 5.14）。入站协议与有效出站协议相同则直通，不同则经中间表示转换，前四种入站 × 前四种出站共十六种组合，流式与非流式都可用（gemini 的原生形状见 5.9）。

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
| 入站协议 | anthropic、openai-chat、openai-responses 与 gemini 四个入口，按路径区分 | 客户端说什么协议就应当能接什么：Claude Code 说 anthropic，Codex 说 responses，普通 OpenAI SDK 说 chat，直接用 Google SDK 的客户端说 gemini 原生的 `generateContent`。只做一个入口等于把其余客户端挡在门外 |
| 出站协议 | anthropic、openai-chat、openai-responses 三条加上 gemini，归属在模型上（见 5.7） | 账号那头的协议不由账号名决定：同一家的 `claude-*` 与其余模型走不同路径。gemini 另有一套路径与凭据头，单独算一条协议比在 openai 兼容层里塞特例清楚（见 5.9） |
| 存储 | SQLite 单文件 | 不装数据库服务，事务与查询都够用 |
| 界面 | 纯命令行 | 用量由 `usage` 与 `log` 子命令查看 |
| 计费 | 不做 | 没有第三方付款方，只记录 token 与估算花费 |

协议矩阵。行列都按线协议算，四条入站 × 四条出站的十六种组合全部可用，非流式与流式都支持：

| 入站 ＼ 出站 | anthropic | openai-chat | openai-responses | gemini |
| --- | --- | --- | --- | --- |
| anthropic | 直通 | 转换 | 转换 | 转换 |
| openai-chat | 转换 | 直通 | 转换 | 转换 |
| openai-responses | 转换 | 转换 | 直通 | 转换 |
| gemini | 转换 | 转换 | 转换 | 直通 |

同行同列是“直通”，其余是“转换”。“直通”只需重写请求体里的 `model` 字段，其余字段与值原样带到上游，响应与流式帧按字节原样返回（上游中途断流时按同一协议补上错误帧与收尾帧，见 5.4），用量另由该协议的 scanner 读出；gemini 的报文里没有 `model` 字段（模型名在路径里），它的直通因此一个字节都不改。“转换”走中间表示：请求与响应都只搬映射表里定义过的字段（见 5.4 与 5.9）。gemini 的原生路径、凭据头与流式端点见 5.9。

## 3 用起来是这样

```bash
# 1 初始化。建配置目录与数据库，重复执行不会覆盖已有配置
$ lax init
配置 /home/someone/.config/lax/config.json
数据库 /home/someone/.local/share/lax/lax.db

# 2 加账号。key 加密后入库，不进配置文件。地址与默认协议都来自厂商行，账号只记厂商名
#   （deepseek 显式存了 openai-chat，所以这个账号的出站协议就是它，见 5.8）
$ lax account add deepseek-1 \
    --provider deepseek \
    --key sk-xxxxxxxx
已添加 deepseek-1（deepseek，openai-chat，凭据 sk-x…）

# 2.1 也可以自己登记一家厂商。每个协议一条 flag，值是相对 --base-url 的完整路径，
#     也可以直接写整条地址（同一厂商的不同协议因此可以落在不同域名上）；
#     至少给一条；路径写法由程序规整（前导斜杠、重复斜杠、末尾斜杠都不必纠结）。
#     默认协议必须落在已声明的路由上。改一家已有的厂商用 provider set，
#     删一条路由或清一项设置用 provider unset
$ lax provider add acme --base-url https://api.example.com \
    --openai /v1/chat/completions --anthropic /v1/messages
已添加厂商 acme（https://api.example.com），2 条路由，默认协议 openai-chat
$ lax provider unset acme anthropic
acme 已清掉 anthropic 路由
现有 1 条路由，默认协议 openai-chat

# 3 拉上游模型列表
$ lax model sync --account deepseek-1
账号        厂商      模型  新增  备注
deepseek-1  deepseek  47    47
下一步：lax model probe（每个模型发一条 max_tokens=1 的请求）

# 4 逐个探活，能用的才进选号。已经探过且结论还新鲜（默认 24h 内）的模型会跳过，不花钱
$ lax model probe --account deepseek-1
账号        模型              状态  HTTP  耗时   备注
deepseek-1  deepseek-v4-pro   可用  200   412ms
deepseek-1  deepseek-v4-fast  可用  200   388ms
deepseek-1  deepseek-r1       无权  403   121ms  套餐不含该模型
…（其余 44 行略）
deepseek-1：本次探活 47 个，跳过 0 个，可用 2，无权 45，不存在 0，失败 0；选号列表共 2 个
已把 2 个可用模型写进 deepseek-1，选号按这个列表走

# 5 看一眼
$ lax account ls
名称       厂商      协议         模型               优先级   状态
deepseek-1  deepseek  openai-chat  2 可用 / 47 同步  100      正常

# 6 起网关。前台运行，Ctrl-C 退出
$ lax serve
监听 127.0.0.1:4567
入站 /v1/messages（anthropic）
上游 1 个账号，2 个模型

# 7 在客户端里把地址指过来，例如 Claude Code：
#   ANTHROPIC_BASE_URL=http://127.0.0.1:4567
#   ANTHROPIC_AUTH_TOKEN=lax-local（值随意，本机不校验）
#   ANTHROPIC_MODEL=deepseek-v4-pro

# 8 观察。缓存读与缓存写是两列独立的数：命中通常更便宜、写入更贵，分开看才能对上账单
$ lax log tail -n 5
时间           协议                   模型              账号        状态   输入  输出  思考  缓存读  缓存写  耗时   错误
01-02 14:02:11 anthropic→anthropic    deepseek-v4-pro  deepseek-1  200    1831  402   0     15200   3210    1800ms
01-02 14:01:52 anthropic→openai-chat  deepseek-v4-pro  deepseek-1  429 ✗  0     0     0     0       0       200ms  冷却 60s
# 思考是独立的一列：上游把它报在输出之外时这里才有数，anthropic 自己把它算进输出 token，
# 所以第一行是 0。是否单独计价不按这一列显示的数字猜，判定见 5.5。

$ lax usage --since 7d --by account
账号        请求  输入 token  输出 token  思考 token  缓存读 token  缓存写 token  估算花费
deepseek-1  214   412330      98421      124800      1180000       240100        ¥1.87
缓存读写已按账号的缓存单价计入估算花费，未填缓存单价时按输入价算

# 9 排障。手工打一条，打印选号过程与上游原始响应
$ lax call "hi" --model deepseek-v4-pro --debug
选中 deepseek-1（priority 100，无冷却）
→ POST https://api.deepseek.com/v1/chat/completions
← 200 text/event-stream

# 10 三条入站路径并存，同一个账号都能打到。
#    这家厂商的默认协议是 openai-chat 时，第二条直通，第一、三条经中间表示转换。
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

`account` 一组的子命令：`add`、`ls`、`set`、`rm`、`test`、`enable`、`disable`（`update` 作为 `set` 的同义词接受，帮助里只列 `set`）。`model` 一组的子命令：`ls`、`sync`、`probe`，后两个都支持 `--provider` 与 `--account` 筛选账号；`alias` 一组（`set`、`unset`、`ls`）维护模型别名，把客户端写死的模型名指到真实模型名上（见 5.12）；`price` 一组（`set`、`unset`、`ls`）维护模型单价，写的是目录里的厂商级标价与某个账号的覆盖价（见 5.13）。`provider` 一组的子命令：`ls`、`add`、`set`、`unset`、`rm`，维护厂商默认地址、模型列表覆盖与它的路由（见 5.8）；`header set`、`header unset`、`header ls` 维护上游除凭据头之外还要的附加请求头（见 5.10）；`add` 只新建，改一家已有厂商用 `set`，删一条路由或清掉一项设置用 `unset`（删除不做成「传空串」：空值一律是用法错误）。`--models` 只是手填的初始值，模型列表的常规来源是 `model sync` 拉取加 `model probe` 探活（见 5.7）。

`account`、`model`、`provider`、`log` 四组支持 `-h`、`--help` 与 `help`：把该组的用途与子命令清单（每个子命令一行中文说明）打到 stdout，退出码 0，不读数据库。子命令自己的 `--help` 仍打印该子命令的参数清单。未知子命令退出码 2，提示里给出 `lax <组> --help`。`provider header` 这一组同样支持 `--help`，但它打印的是三条用法、一个示例与四种取值写法（见 5.10）：光列子命令说不清值怎么展开。`model alias` 这一组也一样：它的 `--help` 打印三条用法、一个示例与两段说明（别名只影响请求方向、客户端请求别名时 `log tail` 的模型列写成 `别名 → 真名`），光列子命令说不清别名改的是什么、不改的是什么。`model price` 同样按这个形状打印（三条用法、一个示例 `lax model price set glm-4.6 --in 0.6 --out 2.2`、两段说明：价格属于模型不属于账号；账号级只在套餐不同时覆盖，不影响其它账号）。

| 命令 | 参数 | 行为 | 输出 |
| --- | --- | --- | --- |
| `init` | `--force` | 建配置目录与数据目录，写默认配置；`--force` 覆盖 | 两个路径 |
| `serve` | `--addr`、`--log-level` | 前台起网关，注册四条入站路由、`/healthz` 与两个只读的模型清单接口（`GET /v1/models`、`GET /v1/models/{id}`，见 5.2.1）；启动只统计账号与模型数，不发上游请求 | 启动摘要，之后按请求打日志 |
| `account add` | `<名称> --provider --key [--models] [--priority]` | 加密存 key。价格不在账号上：模型单价属于供应商×模型，账号只在套餐不同时覆盖（`model price set`，见 5.13）。`--price-in`、`--price-out`、`--price-cache-read`、`--price-cache-write` 误敲仍是用法错误（退出码 2），文案说明价格已经只存在模型目录上并给出 `lax model price set` 的命令形状。地址与默认协议不落在账号上：转发时按账号的 `provider` 取厂商行的 `base_url` 与有效默认协议（见 5.8），因此不再有 `--base-url` 与 `--protocol` 两个参数；厂商不在表里时报错，列出已知厂商、编辑距离不超过 2 时给出最接近的名字，并提示先 `lax provider add <名称> --base-url <地址> --openai /v1/chat/completions` 再建账号。这两个参数删掉之后，误敲仍然是用法错误（退出码 2）：文案说清地址与默认协议已经只存在厂商行上，并给出 `provider add` 的命令（`--base-url`）或 `provider set --default-protocol`（`--protocol`）。`--models` 手工声明该账号可用的模型（写入按 5.13 的新结构：模型进 `provider_models`，账号侧记一条可用结论），留空则等 `model sync` 与 `model probe`。厂商行在、但一条路由都没声明不算错：此时算不出默认协议，账号照建，转发时按厂商默认协议推导，推导为空由转发层报错（见 5.8）；`--json` 里 `protocol` 与 `base_url` 报的是写完再读一次的派生值（厂商的默认协议与 `base_url`，算不出来时是空串），另有 `name`、`provider`、`models`（这个账号 `access='allowed'` 的名字集合，写完再读一次）、`key`（掩码）。 | 确认一行（名称、厂商、默认协议、脱敏凭据），协议算不出来时写 `-` |
| `account ls` | `--json` | 列账号、模型数、是否处在冷却；模型列按可达性统计，两种情况：`account_models` 有行时显示「N 可用 / M 已登记」（分子数 `access='allowed'` 的行，分母数这个账号在 `account_models` 里的全部行；分母写「已登记」而不是「已同步」，因为同步、手填与探活都会留行，其中同步与手填的行还没探过。零可用同样走这条分支，「0 可用 / M 已登记」说明这个账号眼下接不了任何模型），一行都没有时写「无模型（先 model sync）」——没有行表示该账号不提供任何模型、不参与候选，不写成「不限」这类看起来表示「什么模型都收」的文案；地址列与协议列都取厂商行：地址是该厂商的 `base_url`（厂商 `base_url` 为空时写 `-`，与 `provider ls` 同一种写法，见 5.8），协议是该厂商的有效默认协议（`store.Account.Protocol` 这个派生值，算不出来时写 `-`）；存在与默认协议不同的已发现协议时写成「openai-chat + 另 1 种」；`--json` 每行的 `base_url`、`protocol` 与两列同源（厂商行不在时都是空串），另多 `models_usable`、`models_synced` 两个计数与 `protocols`（默认协议在前，其余是模型行上发现到的协议；默认协议为空串时不把空串放进列表），两个计数与文本同源：`models_usable` 是 `access='allowed'` 的行数，`models_synced` 是 `account_models` 的全部行数，`models_synced` 为 0 就是文本的「无模型」，`models` 是这份 allowed 名字集合（与 `models_usable` 同源，不含 forbidden / missing / error 的行） | 表格 |
| `account set` | `<名称> [--key <key>]` | 换掉已有账号的凭据，不删号重建：删号会丢掉 `account_models` 里的探活结果、协议发现与请求记录的账号归属。`--key` 的三种给法与 `account add` 一致（直接写值、`--key -` 从标准输入读、留空读 `LAX_KEY`），解析不出凭据时是用法错误（退出码 2 并打出用法）——价格参数已经不在这条命令上，换凭据是它唯一的作用。key 走同一条加密路径覆盖 `key_cipher`，明文不进日志、不进 `--json` 输出。旧凭据的结论一并作废：同一个事务里清掉 `cooldown_until`、`fail_streak`、`last_error` 并把账号重新启用（`enabled=1`），写入口是 `store.RotateKey`。不动 `account_models` 与协议发现结果，也不碰厂商表与模型目录——账号的地址与默认协议是厂商行的派生值（见 5.8），换凭据不该影响它们，价格也不在账号上（见 5.13，改价用 `model price set`）。账号不存在时报错并提示先 `lax account add`。只接受 `--key` 一个参数；`--price-in` 一类误敲是用法错误（退出码 2）并指向 `model price set`。`--key` 给了就走 `store.RotateKey`（同事务清冷却、`fail_streak`、`last_error` 并重新启用），换凭据是这条子命令唯一的作用，凭据在动库之前就要能解析出来 | 确认一行（账号名、厂商、脱敏凭据、是否清了冷却）；`--json` 含 `name`、`key_rotated`、`key`（掩码）、`cooldown_cleared`、`fail_streak_cleared`、`enabled` |
| `account rm` | `<名称>` | 删账号，同时删掉它的 `account_models` 行；关联的请求记录保留 | 确认一行 |
| `account test` | `<名称>` | 分两步探活，先证明地址通、再证明凭据有效。第一步 `GET` 模型列表，地址与凭据头跟 `model sync` 同一套规则解析（厂商级覆盖 → 该账号的默认协议那条路由 → 同厂商按协议名排序的第一条非空 `models_path`，整条地址直接访问；凭据头跟取到地址的那条路由的协议，见 5.7），2xx 只说明地址通、上游连得上，同时报出上游模型数（与 `model sync` 同一套形状解析，解析不了只说明「上游返回的不是模型列表」，不判失败）；**列表 2xx 之后仍然**发一条 `max_tokens=1` 的请求证明凭据有效——列表接口是公开接口时任何 key 都能换回 200，前者不作数。列表返回 404 或 405（这家没有列表接口）、403（可能被网关拦）、取不到列表地址或连不上时同样走这条最小调用；模型名按这个顺序取：账号 `accounts.models` 的第一个非空名字 → 列表那一步拿到的第一个模型名（与 `model sync` 同一份解析、按名字排序去重后的第一项）→ 默认模型名 `default`，备注写清用的是哪一个与来源（账号清单 / 上游清单 / 兜底默认名）。两段都通过才判正常，状态与退出码按调用那段算；列表接口自己回 401/429/5xx/其它 4xx 时已经是凭据或上游的结论，不再花一次调用。两段的状态处理一致：2xx → 启用账号并清掉冷却、`last_error` 与 `fail_streak`；401 → 停用账号并把错误写进 `last_error`；429/5xx 与连接层失败（连不上、超时，一条 HTTP 响应都没拿到）→ 按连续失败次数与三档打冷却（见 5.3）；其它 4xx → 只报原话、不动状态。它只判凭据与连通性，模型是否可用由 `model probe` 决定 | 表格（账号 / 状态 / HTTP / 耗时 / 上游模型数 / 备注），状态与 HTTP 取调用那段，耗时取得出结论那一阶段的耗时，上游模型数取列表那段（列表接口没通时显示 `-`），备注写清两段各自的结果（列表接口可用、上游几个模型；模型调用可用还是失败）；`--json` 含 `account`、`stage`（`models` 或 `call`）、`status`、`ok`、`models`（数量，未知为 0）、`latency`、`message`，以及 `list` 对象（列表那一段的 `stage`、`status`、`ok`、`models`、`latency`、`message`） |
| `account enable` | `<名称>` | 重新启用账号并清冷却 | 确认一行 |
| `account disable` | `<名称>` | 停用账号，不再参与选号 | 确认一行 |
| `model ls` | `[--provider 厂商]` `[--account 账号]` `--json` | 按模型名列出提供它的账号、目录里的元数据与计价来源，一行一个模型名；账号列写成「账号(状态)」，状态取 `account_models.access`：`allowed` 且 `checked_at` 不为 0 是「可用」，`allowed` 但没探过是「待探活」，`forbidden` 是「无权」，`missing` 是「不存在」，`error` 是「失败」（凭据被拒在库里落成 `error`，模型层面无从区分）；协议列是该模型在这个账号上的有效出站协议，取值只经 `store.EffectiveModels`（账号覆盖 → 目录里的协议集合 → 厂商的有效默认协议，见 5.13），cli 不重算合并规则；另有三列目录元数据：上下文长度、最大输出（`store.EffectiveModels` 的合并结果，账号覆盖 → 目录）与支持的协议集合（只在 `provider_models.protocols` 里，账号侧没有协议集合的覆盖值），目录里没填的写 `-`；价格列是合并之后的模型单价（账号覆盖 → 厂商级标价，见 5.13），两项都是空时写「未定价」。一行模型行都没有时输出一行提示，下一步是 `model sync` 与 `model probe`，也可以用 `account add --models` 手填；`--json` 每项含 `model`、`accounts`（每项 `name`、`status`（`access` 原文）、`protocol`）、`context_window`、`max_output`、`protocols`（空集合是空数组，不是 null）、`price_in`、`price_out` | 表格（模型 / 账号 / 协议 / 上下文长度 / 最大输出 / 支持协议 / 输入价 / 输出价） |
| `provider ls` | `--json` | 列厂商、`base_url`、默认协议、模型列表、各协议的路由与备注；`base_url` 为空的厂商（它的路由与模型列表地址都是整条地址，见 5.8）地址列显示 `-`，`--json` 的 `base_url` 是空串；默认协议是该厂商的有效默认协议（显式值落在已声明路由上就用它，否则按固定顺序 `openai-chat` → `openai-responses` → `anthropic` 取第一条，见 5.8），一条路由都没有时显示 `-`，不算错误；模型列表列：厂商级 `models_path` 非空就显示它，否则显示各路由自己的 `models_path`（多条路由共用一个路径时只显示一次），都没有时显示 `-`；路由列与 `--json` 的 `protocols` 原样带出 gemini 路由，`path` 里的 `{model}` 占位符照库里存的写，命令行不展开它（见 5.9）；`--json` 每项含 `name`、`base_url`、`default_protocol`、`models_path`、`protocols`（每项 `protocol`、`path`、`models_path`）、`note`，没有路由时 `default_protocol` 为空串、`protocols` 为空数组 | 表格 |
| `provider add` | `<名称> [--base-url <地址>] [--anthropic <路径>] [--openai <路径>] [--responses <路径>] [--gemini <路径或整条地址>] [--default-protocol <协议>] [--models-path <路径>] [--note <备注>]` | 只新建：名称已在厂商表里时报错（「厂商 X 已存在，改用 lax provider set X 修改」），绝不顺手改已有的行。`--base-url` 可以省略，条件是这次声明的每一条路由与 `--models-path`（给了的话）都是整条地址：那时它拼不进任何地址，写一个只为过校验的地址只是无效配置（见 5.8）；有一条相对路径时缺它仍是用法错误，文案说清「这条路由是相对路径，需要 --base-url，或把它写成整条地址」，判定只有 `store` 一份（`requireBaseURLCoverage`），cli 把 `store.ErrBaseURLRequired` 包成用法错误。给了就校验并规整，走 `store.NormalizeBaseURL`（见 5.8），cli 不另写一份 scheme/主机名预检：必须是带主机名的完整地址，`https://` 这种只有 scheme 的写法是用法错误（退出码 2，文案说明缺主机名并给正确例子）；scheme 大小写不敏感（`HTTPS://host/` 能用），落库时归一成小写、路径里重复斜杠合并、末尾斜杠去掉。至少要给一条协议路由，一条都不给是用法错误（不再需要这家厂商就用 `provider rm`）。四个协议 flag 各声明 `(厂商, 协议)` 的一条路由，值可以是相对 `base_url` 的完整路径，也可以是整条地址（含 `://`，见 5.8）：相对路径只规整不拒绝，`v1/chat/completions`、`//v1//chat/completions`、`/v1/chat/completions/` 都落成同一个地址，不报错，只有规整之后为空才算缺值；整条地址校验 scheme 与主机名（`https://`、`https:///x`、`ftp://host/x` 都是用法错误，退出码 2，文案指出缺的是主机名还是 scheme 不对），路径部分照旧规整、查询串与 `#` 之后原样保留，使用时不再拼 `--base-url`。同一个 `base_url` 上不同协议落在不同域名时，协议 flag 直接写整条地址：`lax provider add myco --base-url https://api.example.com --openai https://openai.example.com/v1/chat/completions --anthropic https://claude.example.com/v1/messages`。`--gemini` 与另外三个同一套写法，声明的是 `(厂商, gemini)` 那条路由：gemini 的入站与出站都可用，非流式与流式都有（见 5.9），路径里可以写 `{model}` 占位符，cli 只规整斜杠，花括号与冒号原样落库，占位符在发请求之前才由网关换成模型名。`--help` 里四个协议 flag 都列出来，`--gemini` 的示例是真实形状：`lax provider add google-gemini --base-url https://generativelanguage.googleapis.com --gemini /v1beta/models/{model}:generateContent --default-protocol gemini`。没给每条路由的模型列表路径时按规则自动填：`openai`（openai-chat）路由把尾部 `/chat/completions` 换成 `/models`（`/v1/chat/completions` → `/v1/models`、`/chat/completions` → `/models`），其余协议留空表示回退到同厂商其它协议的非空值（见 5.8）。`--models-path` 是厂商级覆盖，收同样两种写法（相对路径或整条地址），整条地址同样要带主机名。`--default-protocol` 是这家的默认协议，取值是命令行写法 `anthropic`、`openai`、`responses`、`gemini`（也认线协议全名 `openai-chat`、`openai-responses`，`openai` 就是 chat，`gemini` 两种写法同形），必须落在这次声明的路由上，否则整笔拒绝、退出码 1、一条路由都不落库；缺省时按固定顺序 `openai` → `responses` → `anthropic` 取第一条已声明的路由。取值参数传给空串（或只有斜杠这类规整之后为空的写法）是用法错误，删除与清空见 `provider unset` | 确认一行（地址、默认协议、几条路由） |
| `provider set` | `<名称> [--base-url <地址>] [--anthropic <路径>] [--openai <路径>] [--responses <路径>] [--gemini <路径或整条地址>] [--models-path <路径>] [--default-protocol <协议>] [--note <备注>]` | 只设值，不承担删除：只改传入的字段，其余一律保留。协议 flag 覆盖或新增那条路由的路径（写法与 `provider add` 同一套：相对路径只规整，整条地址校验后直接使用；gemini 路由的 `{model}` 占位符同样原样保留），`--default-protocol` 与 `--models-path` 换新值（`--models-path` 同样收两种写法），`--note` 换备注。任何取值参数传空串（例如 `--openai ""`、`--note ""`）都是用法错误、文案指向 `provider unset`，路径参数只有斜杠这类规整之后为空的写法同样算空值，不做「空串即删除」的约定：误敲一个空引号不该把已有路由或备注抹掉。改完之后默认协议必须仍指向一条存在的路由，否则整笔拒绝并提示先加路由或改默认协议。把一条路由从整条地址改成相对路径、同时没给 `--base-url`、库里也没有地址时同样整笔拒绝（文案与 `provider add` 同一份，指向「给 --base-url 或写成整条地址」；要一起改就在同一次调用里带上 `--base-url`），退出码 2。没给模型列表路径的路由按 `openai` 推导规则重算，手工填过的值保持不动。厂商不存在时报错并提示用 `provider add` 新建；一个参数都不给是用法错误 | 确认一行（改了几项、现有几条路由、默认协议） |
| `provider unset` | `<名称> <字段> [<字段>...]` | 删除必须显式声明：`anthropic`、`openai`、`responses`、`gemini` 删掉那条协议路由，`models-path` 清掉厂商级覆盖、回到按路由取，`default-protocol` 清掉显式值、回到 `openai` → `responses` → `anthropic` 固定顺序取第一条路由，`note` 清空备注。字段是位置参数，一次可以给多个，重复的只处理一次。要删的项本来就不存在时不报错（幂等），输出一行说明「X 本来就没有 …」。`base-url` 与厂商名不能 unset：前者清掉之后相对路径的路由就拼不出完整地址（改地址用 `provider set --base-url`，本来就不需要地址的厂商应该从一开始就省略这个参数），后者是主键、清不掉，两者都报错说明原因；未知字段、带 `--` 前缀、大小写不符报错并列出可用字段。删到一条路由都不剩时整笔拒绝并指向 `provider rm <名称>`；删掉的路由正好是默认协议时同样整笔拒绝，文案由 store 给，列出仍然声明的协议并给出 `provider set <名称> --default-protocol <协议>` 与 `provider unset <名称> default-protocol` 两条出路（cli 不另算一份回退规则），同一次调用里带上 `default-protocol` 即放行。`--json` 输出与 `provider ls` 的单项同形 | 逐项确认（清掉了什么 / 本来就没有）+ 现有几条路由与默认协议 |
| `provider rm` | `<名称> [--force]` | 删除厂商行与它的全部路由行；仍有账号指向该厂商时列出这些账号并拒绝。加 `--force` 才删，走 `store.DeleteProviderForce`：同一个事务里把这些账号停用（`enabled=0` 并写明 `last_error`，见 5.8），因为它们的地址与路由随厂商一起没了，留在选号池里必然 502；账号行、凭据与模型清单都不删，重建厂商或改账号的厂商之后 `account enable` 就能接着用 | 确认一行（删了几条路由），`--force` 时另有一行列出被停用的账号；`--json` 含 `removed`、`routes`、`accounts`（指向它的账号名） |
| `provider header set` | `<厂商> <头名> <值> [--protocol <协议>]` | 声明上游除凭据头之外还要的附加请求头（见 5.10），覆盖语义：同一个 (厂商, 作用范围, 头名) 再写一次就是改值，不存在就新建，不报错也不留旧行。头名按 HTTP 头名的规矩校验（token 字符、大小写不敏感、落库归一成小写：写 `X-Opencode-Session` 与写 `x-opencode-session` 是同一行），值不能是空白串、也不能带 CR/LF/NUL，非法头名与空值都是用法错误（退出码 2，打出用法行）。`--protocol` 收命令行写法 `anthropic`、`openai`、`responses`、`gemini`（与 `--default-protocol` 同一套映射，落库的是线协议全名 `openai-chat` 这类），不给就是厂商级：这家厂商的每条路由都带上它；给了就是协议级：只加给这条协议，同名头覆盖厂商级。厂商不存在时报错并指向 `provider ls`（退出码 1） | 确认一行（厂商、头名、作用范围、脱敏后的值）与下一步 `provider header ls`；`--json` 含 `provider`、`protocol`（空串是厂商级）、`name`、`value`（脱敏）、`value_masked` |
| `provider header unset` | `<厂商> <头名> [--protocol <协议>]` | 删掉一条声明，定位用的是 `set` 给的同一个 (厂商, `--protocol`, 头名)。删不到时报错并列出这家厂商现有的声明（多半是头名或 `--protocol` 写错了），退出码 1：与 `provider unset` 删路由「删不到也报错」一致——头没删掉，转发照旧带它，静默当成功等于让人以为解决了。删的是协议级那行而厂商级还有同名头时，输出说明转发会回退到厂商级那条 | 确认一行（厂商、头名、作用范围）与现有几条声明；`--json` 含 `provider`、`protocol`、`name`、`removed`、`remaining`（这家还剩几条） |
| `provider header ls` | `[<厂商>]` `--json` | 列已声明的附加请求头：不给厂商就列全部厂商的，给了就只列这一家（厂商不在表里时报错、退出码 1，不把「写错厂商名」显示成「一条头都没有」）。文本四列：厂商、协议（厂商级显示 `-`）、头名、值（脱敏）。值一律走 `ProviderHeader.Masked`，只露前四位：值可能是密钥（会话 token、自定义鉴权头），完整值不打印，`--json` 也一样。厂商级与协议级各占一行，同一头名两边都有时两行都在，协议列就是区分；合并后的有效集合（协议级按同名覆盖厂商级，见 5.10）不在这里展开，转发时才算 | 表格；一条都没有时输出一行提示与一条示例命令；`--json` 是数组，每项 `provider`、`protocol`（空串是厂商级）、`name`、`value`（脱敏）、`value_masked`（恒 true，说明 `value` 不是明文） |
| `model sync` | `[--provider 厂商]` `[--account 账号]` `[--endpoint <路径>]` | 对选中的账号 GET 上游模型列表：地址按厂商的 `models_path` 覆盖、该账号的默认协议那条路由的 `models_path`、同厂商按协议名排序的第一条非空 `models_path` 的顺序取，都没有时按 `base_url` 后缀猜；凭据头跟「实际取到这条路径的那条路由」的协议走，不是账号的默认协议（见 5.7）。`--endpoint` 只影响这一次调用，值是相对账号的 `base_url` 的完整路径，也可以是整条地址（见 5.8；相对路径只规整，`v1/models` 与 `//v1//models/` 都可用；整条地址直接访问、不再拼 `base_url`），给了就压过厂商覆盖与路由，作用于被选中的每个账号；传空值或只有斜杠是用法错误，整条地址缺主机名、scheme 不是 http(s) 同样是用法错误（退出码 2，文案指出缺什么），不需要临时覆盖就整个去掉这个参数；解析 `{data:[{id}]}`、`{models:[{…}]}` 与顶层数组三种形状（见 5.7），按名字排序去重，空列表算该账号失败。结果写「目录 + 该账号可达性」（见 5.13）：名字进 `provider_models`（同步只说明这个名字存在，不写长度、价格与能力位），该账号在 `account_models` 里的这些名字标 `access='allowed'`；已有的行只改可达性，`http_status`、`error`、`checked_at` 与协议发现一律保留，这一轮清单里没出现的行也不删——可达性只由探活结论改动。抓取走 `gateway.SyncModelsWithEndpoint`，写库走 `store.ReplaceModels`，cli 不自己拼 SQL。某个账号失败时其余账号照常处理完 | 表格（账号 / 厂商 / 模型 / 新增 / 备注）与下一步 `model probe` 的提示；`--json` 是数组，每项 `account`、`provider`、`models`、`added`、`error`（只有失败才有） |
| `model probe` | `[--provider 厂商]` `[--account 账号]` `[--sync]` `[--model 模型]` `[--limit N]` `[--concurrency N]` `[--max-age 时长]` `[--refresh]` | 逐账号对每个模型发一条 `max_tokens=1` 的请求判定可用性，结果写 `account_models.access`（`allowed` / `forbidden` / `missing` / `error`，协议发现另写 `protocol`），每次上游调用同时写一行 `requests`（见 5.5）。选号本来就只读 `access='allowed'`（见 5.13），探活因此不再回写另一份模型清单：探完一条，这个账号能不能用这个模型当场就有答案。跳过规则不变：`allowed`、`forbidden`、`missing` 且 `checked_at` 在 `--max-age`（默认 `24h`）以内的模型跳过，不发上游请求、不写库、不出现在输出里；`error`（含凭据被拒）与 `checked_at` 为 0 的照常探；`--refresh` 或 `--max-age 0` 忽略新鲜度全部重探。没有模型行或带 `--sync` 时先同步；`--model` 只探一个模型名，`--limit` 截断的是跳过之后剩下的待探模型，`--concurrency` 默认 4 且不超过待探模型数；待探模型被全部跳过时一条上游请求都不发，库里已有的 `allowed` 行照旧算数，这个账号因此算作已满足；任一账号出现 401 即停止后续账号并返回错误（已经探完的行留在库里，凭据被拒的那条落成 `error`，下一轮重探）；所有账号库里都零 `allowed` 时退出码 1，错误信息带本次探活数与跳过数，并说明可达性已记录、`access` 为 `forbidden` / `missing` 的模型不会进候选 | 表格（账号 / 模型 / 状态 / HTTP / 耗时 / 备注）与该账号的「本次探活 N 个 / 跳过 M 个」、可用 / 无权 / 不存在 / 失败计数与「可达 N 个」（这个账号库里 `access='allowed'` 的行数）；零可用的账号改说一句「库里没有可用模型；可达性已记录，`access` 为 forbidden / missing 的模型不会进候选」，不再说「模型列表保持原样」——探活的结论现在直接就是选号依据；`--json` 是数组，每项 `account`、`model`、`status`、`http_status`、`latency_ms`、`message`，只含本次被探的模型，计数只出现在人读输出里。人读输出在 stderr 用单行原地刷新（`\r`）打实时进度，形如 `ds1 探活中 37/94  deepseek-v4.1-flash`：账号名、已完成 / 待探总数、最近完成的那个模型（并发大于 1 时显示刚回来的那个），被跳过的模型不计入总数；账号探完补一个换行，随后照旧打印汇总行与表格。进度只在 stderr 是终端时开，非终端（管道、CI、测试）与 `--json` 一个字节都不打，标准输出始终只有结果 |
| `model alias set` | `<别名> <目标>` | 覆盖语义：同一个别名再写一次就是改目标（`created_at` 不动），不存在就新建，不报错也不留旧行。参数个数不对、别名或目标为空（含只有空白）是用法错误（退出码 2，打出用法行）；名字里的控制字符、别名等于目标、别名与真实模型名同名，判定都在 `store.SetModelAlias` 这一处（见 5.12）。遮蔽真名是运行失败（退出码 1），文案由 store 给（写清命中的是哪个账号或哪家厂商的目录、下一步换个别名还是改哪一处），cli 原样带出、不另写一份规则。写完再解析一次：成环与超过 8 跳当场报出来，文案说明这一行已经进库并给出查看与撤掉两条命令，不等到请求时才炸。目标可以暂时没有任何账号提供：先把别名写好，之后加账号即生效 | 确认一行（`已设置别名 <别名> → <目标>`）与下一步 `lax model alias ls`；`--json` 含 `alias`、`target`、`resolved`（解析到的真实模型名）、`ok` |
| `model alias unset` | `<别名>` | 删不到时报错并列出当前有哪些别名（多半是别名写错了），退出码 1：没删掉而静默当成功，会让人以为客户端那个名字已经能解析。一条别名都没有时把下一步写成设置一条的命令。参数个数不对与空值是用法错误（退出码 2） | 确认一行（删掉的是哪个别名、还剩几条）；`--json` 含 `alias`、`removed`、`remaining` |
| `model alias ls` | `[<别名>]` `--json` | 不给别名就列全部（按别名升序），给了就只列这一条；别名不在表里时报错并列出已有的（退出码 1），不把写错一个字母显示成「一条别名都没有」。能不能解析问 `store.ResolveModelAlias`，cli 不自己走链：链断了（成环、超过 8 跳）时状态写「不可解析」，解析列写 store 的原文——它带着是哪几条串在一起，照着就能找到要改的那一条。别名链解析到真名才算可解析，「目标眼下没有账号提供」不影响这一列（那是请求到达时 404 的事，见 5.12）。一条别名都没有时输出一行提示与一条 `model alias set` 示例 | 表格（别名 / 目标 / 状态 / 解析）；`--json` 是数组，每项 `alias`、`target`、`resolved`（解析到的真实模型名，不可解析时为空串）、`ok` |
| `model price set` | `<模型> [--provider 供应商] [--account 账号] [--in N] [--out N] [--cache-read N] [--cache-write N]` | 写模型单价（每百万 token 的浮点，0 是合法值，负数是用法错误、退出码 2），价格归属见 5.13。两个作用域互斥：给 `--account` 写该账号在这个模型上的覆盖价（账号必须有这个模型的可达性行，否则报错并提示先 `model sync` 或 `model probe`），否则写厂商级标价（`--provider` 给定时用它，不给时按模型名唯一确定供应商，多于一家的时报错并列出候选）。只写命令行上给过的那几项，没给的留空（表示继承），不做「一次写四个」。模型不在目录里时报错并提示先同步或探活。同名模型出现在多家供应商的目录里、又没给 `--provider` 时是用法错误（退出码 2），文案列出候选供应商并给出带 `--provider` 的命令形状。 | 确认一行（作用域、模型、写入的几项价）与下一步 `model ls`；`--json` 含 `model`、`provider`、`account`（账号级才有值）、`price_in`、`price_out`、`price_cache_read`、`price_cache_write`（未写的为 null） |
| `model price unset` | `<模型> [--provider 供应商] [--account 账号] <字段> [<字段>...]` | 清掉某一项或几项：字段是 `in`、`out`、`cache-read`、`cache-write`，一次可给多个，重复的只处理一次。账号级清掉表示回到厂商级标价（不是「免费」），厂商级清掉表示这一项未知。要清的那一项本来为空时不报错（幂等），输出说明「本来就没有」。未知字段、带 `--` 前缀、大小写不符报错并列出可用字段。 | 逐项确认 + 剩余几项；`--json` 含 `model`、`provider`、`account`（账号级才有值）、`cleared`、`unchanged`（两者都用字段名）、`remaining`（这一行还剩几项有价） |
| `model price ls` | `[<模型>] [--provider 供应商] [--account 账号]` `--json` | 不给模型就列目录里所有有价格的模型行（按模型名升序），给了就只列这一个。文本五列：模型、供应商、输入价、输出价、缓存读 / 缓存写（空着写 `-`，两者都空时合成一列「回退输入价」）。`--account` 给定时额外显示该账号覆盖后的有效价，两列并排：目录价与有效价。`--json` 每项含 `model`、`provider`、`price_in`、`price_out`、`price_cache_read`、`price_cache_write`、`effective`（给了 `--account` 时才有，同四个字段的合并结果）。 | 表格；一项价格都没有时输出一行提示与一条 `model price set` 示例 |
| `usage` | `--since`、`--by account\|model\|day` | 汇总请求数、token（含思考与缓存读写）与估算花费；`--by account`、`--by model`、`--by day` 三种分组都给出思考、缓存读与缓存写的合计，文本表格另有一行合计，成本已按 5.5 的公式计入缓存读写与独立的思考 token（思考在输出之外时才另计，口径见 5.5），合计之后在缓存 token 不为 0 时用一句话写明这个口径（未填缓存单价按输入价算） | 表格；`--json` 是数组，每项 `key`、`requests`、`in_tokens`、`out_tokens`、`reasoning_tokens`、`cache_read`、`cache_write`、`cost` |
| `log tail` | `-n`、`--follow` | 最近的请求记录；`--follow` 跟随。思考与输出分两列：思考是上游单独报的那一项（anthropic 没有独立字段，显示 0），0 显示 `0`。模型列把 `requests` 的两列都摆出来：客户端给的名字与解析别名后的真实模型名不同时写成 `别名 → 真名`（`fast → glm-4.6`），相同时只写一个名字，不引入恒定的重复；两个名字各自按 24 个显示宽度截断（与单名字时的上限相同），一次别名命中因此一眼能看出实际打了哪个模型（见 5.12） | 表格（时间、协议、模型、账号、状态、输入、输出、思考、缓存读、缓存写、耗时、错误）；`--json` 是数组，每项 `ts`、`inbound`、`outbound`、`model`、`model_used`、`account`、`http_status`、`ok`、`in_tokens`、`out_tokens`、`reasoning_tokens`、`cache_read`、`cache_write`、`latency_ms`、`error`；`model` 是客户端给的名字（可能是别名），`model_used` 是解析后的真实模型名，两者相同时两个字段都有值、不做省略 |
| `decide` | `--model <模型> --state <文本或 JSON> [--questions <JSON>] [--noul 名=问句]... [--choice 名=问句 --choice 名:选项[,选项...]]... [--score 名=问句 --score 名:级别[,级别...]]... [--json]` | 直连本进程发一条 systemone 请求，打印每个问题的答案与概率（见 5.14）。`--state` 收纯文本或 JSON（去掉首尾空白后首字符是 `{` 、`[` 或 `"` 时按 JSON 解析，其余原样当一个字符串发出去）。`--questions` 直接给整块 JSON（必须是带至少一个问题的对象，原样转发）；写了它就不能再用 `--noul`/`--choice`/`--score`，两种来源同时给是用法错误。逐条拼的写法：`--noul 名=问句` 问一个是非题；`--choice 名=问句` 声明一个选择题、`--choice 名:选项[,选项...]` 给它追加选项（至少两个）；`--score 名=问句` 声明一个打分题、`--score 名:级别[,级别...]` 给它追加级别（至少两级，按从低到高写）；选项写 `key` 或 `key=说明`，说明落在 choice 的 `criteria` 对象里（没写说明的选项值是 `null`），score 的级别只写 `key`（它的 `criteria` 是有序字符串数组，没有说明的落点，写 `=` 是用法错误）；判据只能追加给已经声明过、且类型相同的问题。一个问题都没拼出来、`--choice` 少于两个选项、`--score` 少于两级、`--questions` 不是对象或空对象、`--state` 首字符像 JSON 却解析不了，都是用法错误（退出码 2）。模型的有效出站协议不是 systemone 时退出码 1 并说明它以哪条协议出站；入站协议固定 systemone。输出：每个问题一行（名字、类型、答案），`choice` 与 `score` 另给 `confidence`（`noul` 给概率），最后一行是用量；`--json` 原样输出上游 `answers` 与 `usage`，另加 `model`、`account`、`latency_ms`。非 2xx 时文案取响应体的 `detail.message`，退出码 1 |
| `call` | `[提示词] --model [--protocol] [--stream] [--max-tokens N] [--system 提示词] [--debug] [--json]` | 绕过客户端直连本进程，打印选号过程；默认发 anthropic 入站，`--protocol` 可在 anthropic、chat、responses、gemini 之间切换；`--protocol systemone` 与「有效出站协议是 systemone 的模型」都被拒绝（退出码 1）：决策请求没有提示词，改用 `lax decide`（见 5.14）。出站协议不取账号字段（账号已经不存协议，见 5.8）：按「该模型的 `account_models.protocol` → 厂商的有效默认协议」推导，入口是 `store.EffectiveProtocol(账号, 模型)`，cli 与转发层都不重算一份；模型还没探活过、协议列为空时就是厂商的默认协议。gemini 的模型名不在请求体里，由本命令按路径段转义后拼进入站路径：非流式 `/v1beta/models/{model}:generateContent`、`--stream` 时 `:streamGenerateContent`（见 5.9） | 响应，或 debug 轨迹；gemini 的流式按 SSE 逐帧解码后输出文本增量与工具调用，`--json` 时一个 `data` 帧一行 JSON 原文 |
| `doctor` | 无 | 检查配置文件、数据目录、数据库、端口 | 逐项 ok / fail |
| `version` | 无 | 版本号 | 一行 |

退出码：`0` 成功；`1` 运行失败（含 `doctor` 有检查未通过、`call` 与 `decide` 非 2xx、`decide` 的模型不是 systemone 出站）；`2` 用法错误。
错误信息写 stderr，`--json` 时结果写 stdout，两者不混在一起。

`serve` 暴露五条入站路径：`/v1/messages`（anthropic）、`/v1/chat/completions`（openai-chat）、`/v1/responses`（openai-responses）、gemini 的 `/v1beta/models/{model}:generateContent` 与 `:streamGenerateContent`，以及 `/v1/systemone`（systemone，非流式、模型名在请求体里，见 5.14），路径决定入站协议（见 5.2），五条都只收 POST，其余方法 405 并带 `Allow: POST`；另有两条只读的模型清单接口 `GET /v1/models` 与 `GET /v1/models/{id}`，给客户端发现模型名用，它们不是入站协议入口，只收 GET（见 5.2.1）。`/healthz` 的响应里带 `inbound` 与 `outbound` 两份协议清单，都是这五条协议（见 5.2.1）。未知路径返回 404，这与清单接口的两条路径不冲突：它们各自注册了处理器，`/v1/models/` 之后一律当作模型名，不会落进「未知路径」；这条「之后全部算模型名」有一个例外，来自 `http.ServeMux` 的路径规整（见 5.2.1）。

## 5 程序内部做什么

### 5.1 进程与文件

| 路径 | 内容 |
| --- | --- |
| `$XDG_CONFIG_HOME/lax/config.json` | 监听地址、数据库路径，权限 0600 |
| `$XDG_DATA_HOME/lax/lax.db` | 账号、请求记录 |
| `$XDG_DATA_HOME/lax/secret.key` | 加密账号凭据的对称密钥，权限 0600 |

配置只有两项，其余状态都在数据库里。改状态一律走子命令，不手改库。

### 5.2 请求生命周期

1. 收到入站请求，按路径定入站协议：`/v1/messages` 是 anthropic，`/v1/chat/completions` 是 openai-chat，`/v1/responses` 是 openai-responses，`/v1beta/models/{model}:generateContent` 与 `/v1beta/models/{model}:streamGenerateContent` 是 gemini（模型名从路径取、`url.PathUnescape` 还原，`:` 之后的后缀不算模型名的一部分，流式标志也由后缀定），`/v1/systemone` 是 systemone（模型名在请求体里，见 5.14），其余路径返回 404。请求体解析失败返回 400，错误消息前缀协议名。定好协议后记下模型名与开始时间。
2. 按模型名取候选账号：`enabled=1`、模型列表包含该模型（列表为空表示该账号不提供任何模型，不参与选号）、冷却已过。没有任何账号声明提供该模型时返回 404，错误消息说明「没有账号提供模型 X」并提示下一步：`lax model ls` 看现有清单、`lax model probe` 探活；错误类型按入站协议取：anthropic 入站是 `not_found_error`，两条 openai 入站是 `invalid_request_error`。有账号提供但全部在冷却或被停用时不算这种情况，仍按第 8 步返回 503。
3. 按 `priority` 升序，同一会话（见 5.3）优先沿用上次的账号。
4. 解密凭据，按该账号该模型的有效出站协议构造上游请求：入站协议与有效出站协议相同则直通，否则经中间表示转换（见 5.4）。上游路径、凭据头、请求体编码都跟有效出站协议走。上游请求头不从零开始也不照抄入站：先放 `Content-Type` 与流式时的 `Accept`，再用 `setAuth` 按出站协议设凭据头，再透传客户端白名单头（见 5.11），最后叠厂商附加头（5.10）。
5. 发上游请求，读响应头。上游返回 429、5xx，或一条 HTTP 响应都没拿到（连接建立失败、请求发不出去、响应体没读完），且尚未向客户端写出任何字节时，把该账号打入冷却（档位见 5.3）并换下一个候选。
6. 已开始向客户端输出后不再换号；上游中断则向客户端发一个错误事件并结束本次请求。直通与转换两条路径在这里行为一致，判据同为「有没有见到上游的收尾帧」：收尾帧没到（读到这里就干净 EOF 也算）就是断流，两条路径都补一个错误帧加该入站协议的合法结尾，并把这一次请求记成失败；收尾帧到齐的完整流不追加任何字节。收尾帧按帧里的 `type` 字段或 SSE 的 `event` 名判定，不按字节子串（见 5.4）。
7. 解析响应里的用量字段，写一行请求记录。
8. 所有候选账号都失败（上游报错、都在冷却、都被停用），返回 503，响应体带最后一次上游错误的摘要；这与第 2 步的 404 不是一回事：404 是清单里没有账号提供这个模型，503 是提供了但这一次全都打不通。

#### 错误体按入站协议给形状

错误体的形状由入站协议决定，不由上游协议决定：同一句「没有账号提供模型 X」发给四条入站路径，字段要各自符合客户端读的那一套。第 1 步定下入站协议之后，四条入站路径上的错误一律走 `writeInboundError`（`internal/gateway`），按入站协议给形状：

| 入站协议 | 形状 | `type` 或 `status` 的取值 |
| --- | --- | --- |
| anthropic | `{"type":"error","error":{"type":"…","message":"…"}}` | `invalid_request_error`、`not_found_error`、`authentication_error`、`permission_error`、`api_error`、`rate_limit_error` |
| openai-chat | `{"error":{"message":"…","type":"…","param":null,"code":null}}` | OpenAI 的取值：`invalid_request_error`、`not_found_error`、`authentication_error`、`permission_error`、`server_error`、`rate_limit_exceeded` |
| openai-responses | 与 openai-chat 同形，同样是 `message`、`type`、`param`、`code` 四个字段 | 同上 |
| gemini | `{"error":{"code":<HTTP 状态码整数>,"message":"…","status":"<google 的状态名>"}}` | `status` 按状态码映射：400 `INVALID_ARGUMENT`、401 `UNAUTHENTICATED`、403 `PERMISSION_DENIED`、404 `NOT_FOUND`、405 `UNIMPLEMENTED`、429 `RESOURCE_EXHAUSTED`、500 `INTERNAL`、503 `UNAVAILABLE`，其余 `UNKNOWN` |
| systemone | `{"detail":{"error_type":"…","message":"…"}}` | `error_type` 取上游自己那套名字：`invalid_request_error`、`not_found_error`、`authentication_error`、`permission_error`、`rate_limit_error`、`api_error`；字段名是 `error_type` 而不是 `type`，与 TypeSafe API 实测一致（见 5.14） |

几条边界：

- 两条 openai 入站的 `param` 与 `code` 显式给 `null` 而不是省掉这两个键：SDK 会读它们存不存在。类型名按各协议的写法给（`rate_limit_exceeded`、`server_error`），不是把 anthropic 的 `rate_limit_error`、`api_error` 原样搬过去。
- gemini 的 `code` 是本次 HTTP 状态码的整数，`status` 由它映射，不取上游正文里的 `code` 与 `status`：上游那一套已经由 `errorMessageFromBody` 解成一句话（见 5.9）。
- 没有入站协议可依的入口仍用 anthropic 那条统一形状（`writeError`）：`/v1/models`、`/v1/models/{id}`、`/healthz` 之外的两条只读接口错误、405（方法用错）、未知路径与写库失败一类的内部错误。这些请求谈不上「客户端在等哪种形状」。
- 流内错误帧同样按入站协议给形状，见第 6 步与 5.4；四种形状的正文都过 `scrubSecret`（见 5.6）。

### 5.2.1 模型清单查询接口

客户端需要知道有哪些模型名能用，不该让它去翻 `provider ls`／`model ls` 的命令行输出。两个只读接口就是给客户端发现模型名用的：

- `GET /v1/models` → 200，`{"object":"list","data":[{"id":"deepseek-v4-pro","object":"model","created":0,"owned_by":"deepseek"}]}`。
- `GET /v1/models/{id}` → 命中返回 `data` 里的那一个对象（`id`、`object`、`created`、`owned_by` 四个字段，形状与列表里相同）；未命中 404，`error.type` 是 `not_found_error`，消息是「清单里没有模型 `<id>`（停用账号不计，冷却中的账号仍然计入）；用 `lax model ls` 看现有清单，或先运行 `model probe`」。`{id}` 是模型名原文：前缀之后的全部字符都算模型名，含 `/` 的名字（`org/model`）因此也取得到；只有前缀、没有名字时同样 404，文案说明缺的是模型名而不是模型不存在。`{id}` 是别名时返回别名那一条（`id` 仍是别名，`owned_by` 取提供解析后真名的厂商），解不到目标时同样 404，文案在那句话后面接着说「这是一个别名，目标是 Y，但没有启用账号提供它」（见 5.12）。
- 「前缀之后全部算模型名」有一个例外，来自 `http.ServeMux` 而不是这两个接口：mux 先把路径过一遍 `cleanPath` 再匹配，`/v1/models//x`、`/v1/models/a/./b` 与带 `..` 的写法（`/v1/models/x/../y`）匹配不到 `/v1/models/` 这个前缀，而是 307 到规整后的路径（mux 用 307 而不是 301，方法与非 GET 的请求体因此不会被改写），客户端跟过去查到的是规整后的名字。含 `//`、`.`、`..` 的模型名在真实上游里不存在，不为此绕开 mux 另写一套路由。
- 两个接口都只收 GET，其余方法（含 HEAD）405 并带 `Allow: GET`；四条入站路径反过来只收 POST，405 带 `Allow: POST`。405 说的是「这个地址不支持这个方法」，404 说的是「这个模型名不在清单里」，客户端据此能分开是方法用错了还是清单里真的没有。
- 读库失败（打不开、查询报错）返回 500，`error.type` 是 `api_error`，文案提示先跑 `lax doctor` 查数据库：清单答不出来要跟「清单是空的」分开，后者是 200 加空数组。
- `accounts.models` 里的空白名字（空串或只有空格）不算模型，不进清单：它匹配不上任何请求里的 `model`，列出来只会让客户端以为有这么个名字。

清单的来源与选号是同一份数据：`enabled=1` 账号的 `accounts.models`（见 5.3），不是 `account_models`——后者是同步与探活的原料，选号不读它（见 5.7）。四条规则：

- 只算 `enabled=1` 的账号：停用账号的模型不出现，客户端因此不会把请求发到一个必然 503 的名字上。冷却中的账号照样算：冷却影响的是「这一次能不能接」，不是「有没有这个模型名」，把它藏起来只会让客户端在冷却的几十秒里以为模型没了。
- 按模型名去重、升序。选号本来也按名字匹配，一个名字列多行只会让客户端以为要挑账号。
- `owned_by` 取厂商名（`accounts.provider`）；同名模型被多家支持时取厂商名字典序第一个，不看 `priority` 与账号顺序：同一份库必须给出同一份响应，否则客户端缓存与用例断言都会抖。
- `created` 固定 0，不编时间戳：这里没有上游的创建时间可用，假时间只会被客户端当成真事实。

别名条目（见 5.12）叠在同一份真名清单上：

- `id` 是别名，`owned_by` 取提供「解析后的真名」的厂商（多个时照上面那条取厂商名字典序第一个），`created` 仍是 0。别名不属于哪家厂商，客户端看到的仍是真相。
- 真名与别名按名字合成一份 `data`、一起升序排列：两个名字都能用，同一个库两次请求给出同一份响应。别名与某个真名同名时只留真名那一条——写入时已经拒绝遮蔽真名，库被手改过或事后新加账号仍可能撞上，真名优先。
- 解析不到目标的别名不出现（没有启用账号提供那个真名），链成环或超过 8 跳的别名也不出现：清单反映「现在能接的名字」，接不了的名字列出来只会让客户端把请求发到一个必然 404 的名字上。

没有任何模型时返回 200 与 `{"object":"list","data":[]}`，不是 404：「清单是空的」是清单的答案，「这个接口不存在」是路径的答案，两者要能分开。错误体与 anthropic 入站同形（顶层 `type` 是 `error`，`error.type`、`error.message` 给类型与文案，见 `writeError`；这两个接口没有入站协议可依，见 5.2 的错误体一节），文案照 5.2 的口吻写清发生了什么、下一步运行什么命令。

两个接口只读：不写 `requests`、不动冷却与 `fail_streak`、不影响选号与 `accounts.models`。它们每次 GET 都全表扫描 `accounts`，第一版不加缓存（边界见第 7 节「已知未做」）。与 `/healthz` 的分工是视角不同：`/healthz` 是运维视角的计数（几个账号、几个启用、几个模型、哪些协议进出），给监控与 `doctor` 看，它计的是全部账号的模型并集（含停用账号）；`/v1/models` 是客户端视角的清单（有哪些名字、归谁），只算启用账号，字段按上游模型列表的形状给，客户端拿它就能填自己的模型下拉框。

清单与转发对同一个名字可以给出不同答案，分界是一句话：清单只反映「现在能接的账号声明了哪些名字」，转发时的 503 是「账号存在但当前不可用」。所以只由停用账号提供的名字在清单里 404、走转发却是 503；冷却中的账号两头都在：它在清单里出现，转发时若正在冷却则计入 503 那一侧。两个方向的判据分别落在 `modelCatalog` 的 `enabled=1` 与候选集合为空后 `account.Providers` 的判定上（见 5.2 第 2 步与第 8 步）。

### 5.3 选号与失败转移

- 候选集合由模型名决定。模型名用请求里的 `model` 字段原文匹配，不做别名归一（第一版）。`accounts.models` 为空表示该账号不提供任何模型，不参与选号：刚建号、或 `model probe` 零可用的账号在探出可用模型之前不会被选中，否则它会接下任何模型名，把请求打到一家根本不提供该模型的上游；`account test` 与 `model probe` 不受此影响，仍可单独探活。`model sync` 只刷新 `account_models`，不改变选号依据；`model probe` 探出可用模型后才把列表写进 `accounts.models`，选号此后按新列表走。
- 协议不进选号。候选量与排序只看 `accounts.models`、`enabled`、冷却与 `priority`，协议决定的是「怎么打」而不是「能不能接」：同一账号的两个模型可以各走各的协议，不必为了协议不同拆成两个账号、重复同步与重复探活。有效出站协议按 `account_models.protocol` 优先、账号 `protocol` 兜底算出（见 5.7），换号时按候选账号各自的有效协议重新算。
- 协议只影响路径、凭据头与请求体编码。冷却、失败转移、会话粘性、401 停用与 403 原话返回都在这层之上，不因协议不同而变。
- 冷却分三档，按失败的来源定，不看状态码本身：429 起始 60 秒；上游明确的 5xx 起始 10 秒；**连接层失败**（DNS 解析失败、连接被拒、TLS 握手失败、读超时、`unexpected EOF` 一类没有拿到任何 HTTP 响应的失败）起始 2 秒。三档都按连续失败翻倍、上限 30 分钟，连续失败次数记在 `fail_streak` 列、跨请求累计。连接层失败单独一档，因为这类失败通常来自本机网络或中转抖动而不是上游在拒服务：按 5xx 那档算，单账号网关在断网恢复后仍会被锁住十几秒到几分钟，实测一个不稳的中转连续六次 EOF 就能把首个冷却推到分钟级。`requests.http_status` 对这类失败仍记 502（表示「网关没能拿到上游响应」），冷却档位由内部的失败来源决定，不改状态码的含义。定档只有两份代码：`account.KindFromStatus(status)` 给拿到状态的失败定档（429 → `KindRateLimit`，其余 → `KindServer`），没拿到状态的失败由网关显式给 `KindTransport`；`account.Cooldown(kind, streak)` 按档位与连续失败次数算时长，三档的翻倍与上限都只有这一处。已经向客户端写出字节之后的断流（流式 `wrote` 为真）不进冷却、也不换号，因此也不算连接层失败：这一次的结局已经写在客户端那边，重试只会把半截响应再打一遍。任意一次成功、`account test` 成功或 `account enable` 都会把冷却、`last_error` 与 `fail_streak` 一起清零。
- 会话粘性：请求头带 `x-lax-session` 或 `metadata.user_id` 时，同一个值在一小时内粘同一个账号；否则按 `priority` 轮转。
- 401 说明凭据本身不行：把账号标为 `enabled=0` 并把错误返回客户端，避免同一条错误把整个候选池刷成不可用。`account test` 成功或 `account enable` 可恢复，两者都会清冷却。
- 403 多为套餐或模型权限不对，凭据还能用：不停用账号，也不再换号，直接把上游原话返回。commandcode 对不在套餐内的模型就是返回 403。

### 5.4 中间表示与转换规则

入站协议四条，出站协议四条（两边同一份名单，gemini 的原生形状见 5.9），两边相同时直通，不同时经中间表示互转。中间表示不绑任何一家上游的字段名，协议差异全部在编解码器里吸收。

中间表示在 `internal/translate/ir.go`：

| 类型 | 字段 |
| --- | --- |
| `Request` | `Model`、`System`、`Messages`、`Tools`、`ToolChoice`、`Thinking`、`ParallelToolCalls`、`MaxTokens`、`Temperature`、`TopP`、`Stop`、`Stream` |
| `Message` | `Role` + `Blocks` |
| `Block` | `text`、`image`、`tool_use`、`tool_result`、`thinking` |
| `Thinking` | `Text`、`Signature`、`Redacted` |
| `Response` | `ID`、`Model`、`Blocks`、`StopReason`、`StopNote`、`Usage` |
| `Usage` | `In`、`Out`、`CacheRead`、`CacheWrite` |

`StopNote` 是结束原因里四种取值装不下的那一档（目前只有 gemini 的 `SAFETY`、`RECITATION` 一类会填）：编给客户端时丢弃，由网关写一行日志当备注（见 5.9）。

按协议名分发的入口都在 `ir.go`：`DecodeRequest`、`EncodeRequest`、`DecodeResponse`、`EncodeResponse`、`NewStreamDecoder`、`NewStreamEncoder`、`NewUsageScanner`、`PathProtocol`。四套编解码器分别在 `anthropic.go`、`chat.go`、`responses.go` 与 `gemini.go`；`gemini.go` 同时管入站与出站、非流式与流式，四个入口都接（`PathProtocol` 认它的两条入站路径，见 5.9）。

同一个概念在三条协议里落的位置不同：

| 概念 | anthropic | openai-chat | openai-responses |
| --- | --- | --- | --- |
| system | 顶层 `system` 字段 | 首条 `role=system` 消息 | 顶层 `instructions` |
| 对话中间的 system 消息 | `role=system` 消息 | `role=system` 消息 | 折进 `instructions`，位置丢失 |
| tool_use | assistant 消息里的 `tool_use` 块 | `tool_calls[]` | `function_call` item |
| tool_result | user 消息里的 `tool_result` 块 | 独立 `role=tool` 消息 | `function_call_output` item |
| 思考内容 | assistant 消息里的 `thinking` 块（带 `signature`）与 `redacted_thinking` 块（带 opaque `data`） | assistant 消息的 `reasoning_content`（少数实现把同一段正文写在 `reasoning`） | output 里的 `reasoning` item，正文在 `content` 或 `summary` 的文本里 |
| 结束原因 | `end_turn` / `max_tokens` / `tool_use` / `stop_sequence` | `stop` / `length` / `tool_calls` | `completed` / `incomplete`（`max_output_tokens`） |
| 流式事件 | 具名事件 `message_start`、`content_block_delta`、`message_stop` | 只有 `data` 帧，末尾 `[DONE]` | 具名事件 `response.created`、`response.output_text.delta`、`response.completed` |
| 用量字段 | `input_tokens`、`output_tokens`、`cache_read_input_tokens`、`cache_creation_input_tokens` | `prompt_tokens`、`completion_tokens`、`prompt_tokens_details.cached_tokens` | `input_tokens`、`output_tokens`、`input_tokens_details.cached_tokens` |

结束原因在中间表示里只留四种：`end_turn`、`max_tokens`、`tool_use`、`stop_sequence`；编码时再翻回各协议自己的说法。

上面那张表是从概念找坐标，反方向编出去还有这些不在这张表里的规则：

- 对话中间的 system 消息：anthropic 入站允许 `role=system` 的对话中间消息，按位置保留成中间表示的 `RoleSystem` 消息。Claude Code 的 mid-conv system 就发这个形状，模型名不在它的能力表里时默认开启，因此指向任何非 Anthropic 模型名的会话都会走进这条默认，拒收会让整个客户端停在 `400 anthropic: 不支持的消息角色 "system"` 上（见 `LESSONS.md`）。编回 anthropic 原样写 `role=system`（上游认不认看同一个 beta，`anthropic-beta` 走 5.11 白名单）；编成 openai-chat 写一条 `role=system` 消息；openai-responses 与 gemini 只有顶层的 `instructions` 与 `systemInstruction`，分别折进去，落到这两条协议时这条消息失去原来的位置。块只留文本，与顶层 `system` 的处理一致，其它块丢弃。

- 工具结果进 anthropic：`RoleTool` 消息折叠成一条 `role=user` 消息里的 `tool_result` 块，同一个 assistant 消息之后的多条结果按原顺序并进同一条 user 消息。anthropic 没有 `role=tool`，chat 与 responses 解出来的工具结果原样写过去会被上游以 400 拒绝。
- 连续的 user 侧内容并成一条：工具结果之后紧跟的追问（普通 user 消息）也并进同一条 user 消息，块顺序是 `tool_result` 在前、普通文本与图片按原顺序跟在后面。anthropic 不接受两条相邻的 user 消息，分成两条写过去同样会被 400 拒绝。
- 工具序号进 chat：流式的 `tool_calls[].index` 由编码器自己从 0 连续编号，不沿用上游块号。上游 anthropic 有文本块时首个工具是 1、并行工具是 1 与 2；按稠密下标建数组的客户端会撞上空位。
- 流式工具调用的收尾进 responses：编码器缓存每个工具 item 的名称与参数，收尾前补 `response.function_call_arguments.done` 与 `response.output_item.done`，并把完整的 `function_call` 回填进 `response.completed.response.output`。只发 added 与 delta 的流会让客户端拿不到收尾；`output` 按 `output_index` 列工具 item、思考 item 与文本 item。
- 流式文本 item 进 responses：每个文本块一个 `message` item，开出来时发 `response.output_item.added`（`status: in_progress`、`content: []`），正文照旧按 `response.output_text.delta` 逐段下发；收尾补 `response.output_item.done`，并把 `content: [{"type":"output_text","text": <全文>}]` 回填进 `response.completed.response.output`，与工具 item、思考 item 一起按 `output_index` 排序。文本块的边界取上游的 `output_index`：上游块号变了、或者中间插进了工具与思考 item，就另起一个 `message` item——chat 与 gemini 的文本增量没有块号，靠后者分块。按客户端要的结果回填正文是有代价的：单条响应攒的正文总长上限 1MB，越线之后停止继续攒，攒不全的文本 item 收尾时不发 `output_item.done`、也不进 `output`（其余 item 与 `completed` 的 `status`、`usage` 照旧给出），同时记一行日志说明客户端仍可从增量拼出全文。断流走 `response.failed` 时 `output` 恒为空数组，半截正文不回填。
- 编码到 anthropic 时丢掉空内容消息：一块内容都没剩下的消息整条不写出去。丢弃之后相邻的同角色消息按块顺序并成一条，assistant 侧直接接在后面，user 侧仍是 `tool_result` 在前、普通内容跟随。anthropic 拒绝空的 `content` 数组，也拒绝相邻的同角色消息，写出去就是整条请求 400。代价是历史里只剩思考的一轮整条消失（例如一条只有 `reasoning_content`、没有签名的 assistant 消息），客户端下一轮看到的上下文里不再有那一轮的空位；所有消息都被丢弃时编码直接返回错误，不写出空的 `messages`。
- 首条消息不补：丢弃之后第一条仍是 assistant 时原样保留，不预置一条客户端没发过的 user 消息。anthropic 的 Messages 文档只描述交替的轮次、连续轮次的合并与 prefill（末条是 assistant），没有「首条必须是 user」这一条；补一条伪造的 user 文本等于往上下文里塞客户端没说过的话，比让上游用它自己的理由拒绝更贵。
- 工具定义跨协议：anthropic 的服务端内置工具（`tools[].type` 不是 `custom`，例如 `web_search_20250305`）在中间表示里保留 `type`，编到 chat 与 responses 时整条丢弃——这两条协议的工具定义只认 function，伪造一个出来会让语义错位；编回 anthropic 时 `type` 原样保留，缺 `input_schema` 的内置工具不补空对象。

两条分界与各自的代价：

- 直通（入站协议 = 出站协议）：请求体只改 `model` 字段，其余字段与值原样保留，包括文档里没列出的陌生字段；响应体与流式帧按字节原样转发。读用量不改变转发内容，由该协议的 usage scanner 从响应体或流式帧里读，缓存字段也能读到。gemini 是这条规则的例外：它的报文里没有 `model` 字段（模型名在路径里），所以直通时请求体一个字节都不改，往上写一个 `model` 只会让上游以「未知字段」拒掉整条请求。代价是只对同协议账号成立，好处是上游新加的字段与参数可以直接用。
- 转换（入站协议 ≠ 出站协议）：解码成中间表示，再按目标协议编码，请求与响应都只搬表里定义过的字段。表里没有的字段丢弃，这是有意的：陌生字段跨协议搬过去只会变成语义错位。代价是改协议就意味着表外字段消失，新增映射必须同时补用例。

流式在转换路径上走增量：上游帧先解码成 `thinking`、`text`、`tool_start`、`tool_args`、`stop`、`error` 增量，再由入站协议的编码器编成客户端事件。同协议直通时逐行转发原始字节，只用 scanner 记账。

解码器认 `response.output_item.added` 与 `response.function_call_arguments.delta` 两种工具增量；只发 `response.output_item.done` 的上游在 done 里补齐 `tool_start` 与 `tool_args`，已经发过增量的 item 按 output 序号跳过，不重复发。缺了这条，只发 done 的上游会让整条工具调用消失，而结束原因又从 completed 里的 output 反推，客户端会看到 `stop_reason=tool_use` 却没有 `tool_use` 块。

两条路径判定断流用的是同一个判据：有没有见到上游的收尾帧。收尾帧按帧里的 `type` 字段或 SSE 的 `event` 名解析判定，不按字节子串——`{"type": "message_stop"}` 这种带空格、或者缩进成多行 `data` 行的合法帧同样是收尾帧。anthropic 认 `message_stop`，openai-chat 认 `[DONE]`，openai-responses 认 `response.completed`、`response.incomplete`、`response.failed`，gemini 认带 `candidates[].finishReason` 的帧（它没有单独的结束事件；只有 `promptFeedback.blockReason` 的帧不算收尾，那是上游拒了提示词）。

收尾帧到齐的完整流不追加任何字节，直通的字节仍与上游逐字节相同。收尾帧没到（包括帧边界上干净 EOF）就是断流，已写出的字节一个不动，只在末尾追加该入站协议的合法结尾：anthropic 补 `event: error` 再补 `message_delta`（带已读到的 usage）与 `message_stop`，openai-chat 补一帧 `data: {"error":…}` 再补 `data: [DONE]`，openai-responses 补 `error` 与 `response.failed`，gemini 补一帧 `data: {"error":…}` 再补一帧带 `finishReason` 与 `usageMetadata` 的候选（它没有单独的结束事件）。转换路径的错误帧走编码器，形状跟着入站协议走：入站是 responses 时终态是 `response.failed`（`status=failed`），不是 `response.completed`——上游出错却告诉客户端「已完成」会与 `requests` 里的 `ok=0` 自相矛盾；正常结束仍发 `response.completed`。这一次请求在两条路径上都记成失败，`error` 写明收尾帧没到或解码出错；上游自己已经发过错误帧时，转换路径不再重复补一条，`error` 写上游的原话。追加之前判断帧边界时两种行尾都认（`\n\n` 与 CRLF 流的 `\r\n\r\n`），已经结束了一帧就不再补空行。

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
| assistant 消息里的 `thinking` 块 | 并进 assistant 消息的 `reasoning_content`，`signature` 丢弃（chat 没有放签名的字段）；只在响应方向搬，请求方向不回填 |
| assistant 消息里的 `redacted_thinking` 块 | 丢弃（opaque `data` 只有 anthropic 认） |
| `tool_choice` | `tool_choice`，`any` 映射为 `required` |
| `max_tokens` | `max_tokens` |
| `stop_sequences` | `stop` |
| `temperature`、`top_p` | 同名 |
| 响应 `stop_reason=end_turn` | `finish_reason=stop` |
| `stop_reason=tool_use` | `finish_reason=tool_calls` |
| `stop_reason=max_tokens` | `finish_reason=length` |
| `usage.input_tokens` / `output_tokens` | `usage.prompt_tokens` / `completion_tokens` |

思考内容的四条规则与它们的代价：

- 思考内容只在响应方向搬运，请求方向不回填。chat 的 `reasoning_content`（以及少数实现的 `reasoning`）与 responses 的 `reasoning` item 都是上游自己产生的字段；客户端把上一轮的思考内容写回请求，主流推理模型（DeepSeek 一类）会以 400 拒掉整条请求，比丢掉一段思考贵得多。所以解码得到的思考块编成 chat 与 responses 的**请求**时整块丢弃，响应方向照旧搬运。anthropic 是唯一要求客户端回填思考块的协议，带 `signature` 的块仍原样写回请求（见下一条）。
- 思考块编到 anthropic 时必须有 `signature`，没有就整块不写（不是报错）。不伪造签名：anthropic 会以 400 拒绝整条请求，一次 400 比丢一段思考更贵。代价是 chat 与 responses 转进 anthropic 的非流式请求与响应里，思考内容整块消失。
- `signature` 与 `redacted_thinking` 的 opaque `data` 只有 anthropic 有落点，编到另外两条协议时丢弃；反方向 chat、responses 的思考内容没有签名，转到 anthropic 时同样丢弃（见上一条）。中间表示里思考块只有一个种类（`BlockKind` 是 `thinking`），`Signature` 非空表示能写回 anthropic 的普通思考块，`Redacted` 非空表示只能写回 anthropic 的 `redacted_thinking`。
- 流式是例外：增量一到就已经写给了客户端，撤不回来，所以 chat / responses → anthropic 的流里照发 `content_block_start(type=thinking)` 与 `thinking_delta`，只是不发 `signature_delta`。反过来 anthropic 的 `thinking_delta` 与 `signature_delta` 编成 chat 的 `delta.reasoning_content`（签名丢弃）、responses 的 `response.reasoning_summary_text.delta`。

anthropic 的流式块序列是 start → delta* → stop，同一个块收过 `content_block_stop` 之后不能再收 `content_block_delta`。思考增量与工具参数交替到达时（并行工具、上游把 reasoning 插在两次 arguments 之间），编码器按块记开关状态：需要写进已关闭的块时先把新块开出来，重新启用那个上游工具块号。代价是同一个 `tool_use` 的 `id` 会出现在两个块里；两者取其一都是残的（上半段参数本身已经是半截 JSON），这里选的是让每个 `content_block_delta` 都有对应未关闭的块（见第 7 节）。

responses 的 `reasoning` item 只在响应方向编：非流式把正文写进 `summary` 的 `summary_text`（`content` 里的 `reasoning_text` 要与 item 的 `id` 跟 `encrypted_content` 配套上游才认，这三样不搬，写 `summary` 是不伪造上游事实的写法）；解码时两处的文本都读，`content` 在前、`summary` 在后，用换行拼起来。请求方向整块丢弃（见上面第一条）。流式下发给 responses 客户端时思考 item 有完整生命周期：`response.output_item.added`、逐段 `response.reasoning_summary_text.delta`，收尾补 `response.output_item.done` 并把 item（`status=completed`、正文在 `summary` 里）回填进 `completed.response.output`——只发 added 与 delta 会让客户端等一个永远不来的收尾。

同一对协议间的流式映射（SSE）：

| 上游 chat.completion.chunk | 下发给客户端 anthropic 事件 |
| --- | --- |
| 首块 | `message_start`（含空 content） |
| `delta.reasoning_content` | `content_block_start(thinking)` 与 `content_block_delta(thinking_delta)` |
| `delta.content` 首个非空 | `content_block_start(index=0, text)` |
| 后续 `delta.content` | `content_block_delta(text_delta)` |
| `delta.tool_calls[i]` 首个 | `content_block_start(tool_use)` 并分配 index |
| `delta.tool_calls[i].function.arguments` | `content_block_delta(input_json_delta)` |
| `finish_reason` | 关闭所有 block，发 `message_delta`（含 usage），再发 `message_stop` |
| 上游错误帧 | `event: error`，带映射后的 type 与 message |

客户端是 responses 时，`finish_reason` 之前还会给文本 item 与思考 item 补 `response.output_item.done`、按工具 item 逐个补 `response.function_call_arguments.done` 与 `response.output_item.done`，再把完整的 item 按 `output_index` 回填进 `response.completed.response.output`。

#### 思考配置参数

请求侧决定「要不要想、想多深」的字段，在四种协议里名字与单位都不同，进中间表示的 `ThinkingConfig`（`Request.Thinking`）。它与消息里的思考内容块不是一件事：那边是 `Thinking` 类型（`Text`、`Signature`、`Redacted`），装的是上游在响应里给出的正文；这里是请求参数。两者刻意不共用一个类型名，`Thinking` 只用于内容块。

| 协议 | 字段 | 中间表示 |
| --- | --- | --- |
| anthropic | 顶层 `thinking{type:"enabled",budget_tokens:N}` | `ThinkingConfig{Enabled:true, Budget:N}` |
| anthropic | `thinking{type:"disabled"}` | `ThinkingConfig{Enabled:false}` |
| openai-chat | `reasoning_effort: "minimal"\|"low"\|"medium"\|"high"`（少数实现包成 `reasoning.effort`，解码时两处都读、顶层优先） | `ThinkingConfig{Enabled:true, Effort:…}` |
| openai-responses | `reasoning.effort`（同名取值） | 同上 |
| gemini | `generationConfig.thinkingConfig.thinkingBudget`（token 数） | `ThinkingConfig{Enabled:true, Budget:N}`（0 视为关闭；负数按开着算，预算原样保留） |

跨协议编出去时的映射（同协议直通不改字段）：

| 目标 | 由预算算 | 由档位算 |
| --- | --- | --- |
| anthropic | `budget_tokens` 直传 | `minimal` 1024、`low` 2048、`medium` 4096、`high` 8192 |
| openai-chat、openai-responses | 预算 ≥ 8192 → `high`，≥ 2048 → `medium`，否则 `low` | `reasoning_effort` / `reasoning.effort` 直传 |
| gemini | `thinkingBudget` 直传，`Enabled=false` 写 0 | 按上一行 anthropic 的档位→预算表折成预算再传 |

- 只在客户端显式给了这类字段时才写出去，不凭协议默认值加：上游对不认识的参数会直接 400。`Thinking` 为 nil 时四种编码器一个字段都不写。
- `Enabled=false` 时 anthropic 写 `thinking{type:"disabled"}`，两条 openai 协议不写 `reasoning_effort` / `reasoning`（没有「不思考」的档位），gemini 写 `thinkingBudget:0`。
- `Budget` 与 `Effort` 同时有值时按目标协议能直接表达的那一个写：anthropic 与 gemini 用 `Budget`，两条 openai 协议用 `Effort`。一条协议解出来只会有一个（预算或档位），这种组合只在手工构造的中间表示里出现。
- `Enabled=true` 但既没有预算也没有认得出的档位时不写：anthropic 的 `enabled` 必须带 `budget_tokens`，gemini 不写 `thinkingConfig` 就是它自己的默认值，替客户端编一个深度等于替它下决定。gemini 的负数预算（`-1` 表示动态思考）同理：anthropic 与两条 openai 协议都表达不了，编到它们时丢掉。
- 认不出的档位（例如上游新加的取值，或表示「不思考」的取值）在两条 openai 协议上原样直传，编到 anthropic 与 gemini 时不写。
- 预算与档位的换算是有损的（预算换成三档后精度丢失），取舍是宁可少一点精度也要让客户端设的值起作用。
- `ThinkingConfig` 与消息里的思考内容无关：前者是请求侧参数，后者是响应里的增量（见 5.4 的思考内容一行）。

#### 工具定义与选择

| 字段 | 行为 |
| --- | --- |
| `tools[].strict`（chat 与 responses 共有；chat 写在 `function` 里，responses 写在工具对象顶层） | 两种 openai 协议之间原样保留（进 `Tool.Strict`，`nil` 表示客户端没给，与给了 `false` 不混）；编到 anthropic 与 gemini 时丢弃（两者都没有对应字段），编回 openai 时按中间表示重新写 |
| `tool_choice.type=allowed_tools`（anthropic） | 编到 responses 时写成 `tool_choice{type:"allowed_tools",mode:"auto",tools:[{type:"function",name:…}]}`（同一个名字与白名单）；编到 chat 时 chat 没有白名单形状，降级为 `auto`；编到 gemini 时用 `mode=ANY` 加 `allowedFunctionNames` |
| `tool_choice.type=allowed_tools`（responses 来源） | 与上一行对称：编到 anthropic 保留白名单，编到 chat 降级为 `auto`，编到 gemini 同上 |
| `toolConfig.functionCallingConfig{ANY, allowedFunctionNames}` 有多个名字（gemini 来源） | 解成 `allowed_tools`，名字全搬；一个名字仍按指定工具处理，没有名字的 `ANY` 退成 `required` |
| 指定单个工具 | anthropic 的 `{type:"tool",name}`、chat 的 `{type:"function",function:{name}}`、responses 的 `{type:"function",name}`、gemini 的 `allowedFunctionNames` 四者互转 |

白名单只搬名字、不搬工具定义：被白名单引用却没有声明在 `tools` 里的名字照样写出去，上游会自己拒，换协议时替它挑一个子集只会把客户端的意图改掉。

`Mode` 是 `allowed_tools` 但 `Allowed` 为空时，整条 `tool_choice`（gemini 是整块 `toolConfig`）不写：这一档说的是「只准调这些」，一个名字都没有时写成自动、`none` 或一个空数组都是替客户端说话，不写时上游按自己的默认值走。anthropic 与 responses 那一边解出空白名单时同样整条丢弃。

anthropic 的 `allowed_tools` 另有一个可选的 `mode`（`auto` 或 `any`），中间表示不搬它：编到 responses 一律写 `mode:"auto"`，编回 anthropic 时不写（它的默认值就是 `auto`），因此 `any` 这一档在跨协议之后退成 `auto`。

### 5.5 请求记录

每次尝试写一行，包括失败与换号前的尝试。字段：时间、入站协议、出站协议、请求模型名、实际模型名、账号 id、HTTP 状态、是否成功、输入 token、未命中缓存的输入 token（`in_tokens_uncached`）、输出 token、思考 token（`reasoning_tokens` 与 `reasoning_tokens_extra`）、缓存读、缓存写、耗时、错误摘要。

`http_status` 记的是上游响应的状态码；转发路径一条 HTTP 响应都没拿到时记 502（网关自己填的，表示「没能拿到上游响应」），此时冷却按连接层那一档算，档位不从这个 502 反推（见 5.3）。`model probe` 的上游调用行是另一套：那里的网络错误记 0（见本节下面那段）。

`inbound` 与 `outbound` 两列记的是这一行实际用到的协议对，转换时两列不同，直通时两列相同。除输入与输出 token，`cache_read` 与 `cache_write` 也会写入：anthropic 的缓存读与缓存写分别取自 `cache_read_input_tokens` 与 `cache_creation_input_tokens`；chat 与 responses 只带缓存读（`cached_tokens`），这两家的缓存写记为 0。直通与转换两条路径都会填这几个字段。缓存读与缓存写这两列在 `log tail` 与 `usage` 里都能看到：`log tail` 逐行显示（文本两列加 `--json` 的 `cache_read`、`cache_write`），`usage` 按 `--by` 的三种分组合计，文本与 `--json` 都带。思考 token 同样按这个办法展示：`log tail` 一行一列「思考」（`--json` 的 `reasoning_tokens`），`usage` 三种分组各合计一列（`--json` 的 `reasoning_tokens`），显示的始终是原值，计不计价由下一段判定。展示用的输入 token 是 `in_tokens`（上游原值，归一化只写进新列），花费按下一段的公式折。

`model probe` 的 `max_tokens=1` 调用同样记行：每次上游调用写一行，探活也不例外。换协议重试的中间尝试也各记一行，它们的 `ok` 为 0，`error` 以「换协议前的尝试，未作结论」开头并带上那次的上游错误摘要，与判定行区分开；一次 403 触发三次调用就是三行，账实一致比行数好看重要。这类行的取值：`ts` 是这次调用的开始时间；`inbound` 与 `outbound` 都取这一行实际使用的协议（每一行各自是那次试的协议），探活就是「用这个协议直连上游」；`model_requested` 与 `model_used` 都取被探的模型名；`account_id` 取账号 id；`http_status` 与 `ok` 取上游响应，`ok` 表示 2xx，网络错误时 `http_status` 记 0；`in_tokens`、`out_tokens`、`cache_read`、`cache_write` 与思考两列都由这次所用协议的 usage scanner 从响应体读出：anthropic 的 scanner 缓存读写两项齐全（`cache_read_input_tokens` 与 `cache_creation_input_tokens`），chat 与 responses 只读得到缓存读（`cached_tokens`），`cache_write` 记 0；思考数与总 token 也走同一条读取路径（scanner 返回的 `translate.Usage` 带 `Reasoning` 与 `Total`），所以探活行与转发行用的是同一套判定；非 2xx 与网络错误记 0；`latency_ms` 是这次请求的耗时；`error` 是错误摘要，与探活结果一样过 `scrubSecret`，凭据不进库。这类行因此也进 `usage` 的汇总，探活的花费不会被漏掉一部分。

reasoning token 的口径：各家对「思考算不算输出」做法不同，入账时按上报的数字判定，不按协议猜。上游报在哪：chat 的 `completion_tokens_details.reasoning_tokens`、responses 的 `output_tokens_details.reasoning_tokens`（部分中转照 chat 的写法放在 `completion_tokens_details` 里，两处都读，官方形状优先）、gemini 的 `usageMetadata.thoughtsTokenCount`；anthropic 没有独立字段，它的 `output_tokens` 本来就含思考，`reasoning_tokens` 记 0。

| 场景 | 判定 | `reasoning_tokens` | `reasoning_tokens_extra` |
| --- | --- | --- | --- |
| `total > prompt + completion`（只报 `completion_tokens` 与 `reasoning_tokens` 两项、total 把两者相加的中转，实测一个 prompt=7、completion=1、total=132、reasoning=124 的上游） | 思考是输出之外的一项 | 原值，展示 | 原值，按输出价再计一次 |
| `total <= prompt + completion`（OpenAI 官方把 `reasoning_tokens` 当作 `completion_tokens` 的子集） | 思考已在输出里 | 原值，展示 | 0，不重复计 |
| 没报 `total` | 按子集处理，宁可少算一次也不重复计价 | 原值，展示 | 0 |

判定只写在一处，纯函数 `store.ReasoningExtra(total, in, out, reasoning) int64`，参数是上游报的总 token、输入、输出与思考数，转发行与探活行都调它，调用点不按协议分情况。计价的公式在下面那张表里多一行 `reasoning_tokens_extra × --price-out`。

估算花费的口径（价格归属见 5.13：模型单价属于供应商×模型，账号只在套餐不同时覆盖；单位每百万 token，四项都缺时花费记为 null，不猜价格）：

| 项 | 取值 |
| --- | --- |
| 未命中缓存的输入 | `in_tokens_uncached × price_in` |
| 缓存读 | `cache_read × price_cache_read`，未填时回退到 `price_in` |
| 缓存写 | `cache_write × price_cache_write`，未填时回退到 `price_in` |
| 输出 | `out_tokens × price_out` |
| 独立的 reasoning | `reasoning_tokens_extra × price_out`（判定见上表） |

单价按「这一行请求的账号 + 这一行请求的模型」现算，取值顺序只有一处：`account_models` 里该 (账号, 模型) 行的覆盖值 → `provider_models` 里该 (供应商, 模型) 行的厂商级标价 → 都没有就记 null。不把单价快照进 `requests`：改价之后历史行按新价重算，这与账号级价格时的行为一致，也让「一次改价」只改一处。请求行上的 `model_used` 是解析别名后的真实模型名，正是用来 join 目录的那一列。

某个单价缺项时那一项不计（缓存读写缺项照旧回退输入价，输入价也缺就没有回退），但同组里其它有价的项照算，不因为缺一项就把整组记成 null。**一组里所有请求的所有项都缺价时，花费记 null 而不是 0**：0 表示「这些请求确实不花钱」，缺价表示「不知道多少钱」，混成一个数就没法判断该不该去补价。文本里 null 写「未定价」，合计行按同样的规则判。

花费的展示精度按数值大小自适应，固定两位小数不够用：决策请求的一次调用常见价是万分之几，固定两位会把 `0.0021` 显示成 `0.00`，看起来和免费、和缺价都没区别。规则：0 写 `0.00`；小于 0.01 写四位小数；其余写两位小数。

`in_tokens_uncached` 是入账时算好的一列，因为三种协议对「输入 token」的含不含缓存做法不一致：anthropic 的 `input_tokens` 已排除缓存读写，直接用它；chat 的 `prompt_tokens` 与 gemini 的 `promptTokenCount` 都包含命中缓存的部分，要减掉 `cache_read`（减到 0 为止）。记在 `requests.in_tokens` 里的仍是从上游读到的原值，展示不变；只有花费用归一化后的那一列。不归一化时两个方向都错：anthropic 的缓存读完全不算钱，chat 与 gemini 的缓存读按满价算。

### 5.6 凭据

AES-256-GCM 加密后入库，密钥在 `secret.key`。这个方案防的是「备份文件、误贴日志、共享磁盘快照」这类泄漏，不防同一台机器上以同一用户身份运行的程序。文档里明确写出这一点，不暗示更强保护。

换 key 走 `account set --key`，不删账号：删号会连带删掉 `account_models` 里的探活结果与协议发现，请求记录也会失去账号归属。换 key 是同一个事务里的三件事：覆盖 `key_cipher`、清掉旧凭据留下的冷却与失败计数、把账号重新启用——那些失败是上一个凭据的结论，留着会让新 key 一上来就排在候选之外。

### 5.7 模型同步与探活

手填 `--models` 的问题在于上游一改模型名就失效：账号看上去正常，请求却全打在 404 或 403 上，只能对着上游文档猜。模型列表改成从上游拉取，再逐个探活，只保留真正能用的名字。

两个命令分工明确，一个管「上游有哪些模型」，一个管「哪几个真能用」。两者都按账号处理，并支持同样的筛选：`--provider` 精确匹配账号的 `provider` 字段，`--account` 精确匹配账号名，两个都给时账号必须属于该厂商，否则报错；不带筛选参数时处理全部账号，按账号逐个进行。

`model sync [--provider 厂商] [--account 账号] [--endpoint <路径>]` 只解决前者：

- 模型列表地址按这个顺序取，纯规则在 `store.ResolveModelsPath`：`--endpoint` 给的地址（只作用于这一次调用，压过下面全部）→ 厂商级 `providers.models_path` 覆盖 → 厂商默认协议那条路由的 `models_path` → 同厂商按协议名排序的第一条非空 `models_path`；每一项都可能是相对路径或整条地址（见 5.8），整条地址直接访问、不再拼厂商的 `base_url`（账号行不再存地址）；厂商行存在且声明了路由、但一个非空 `models_path` 都没有时报错，提示用 `--models` 手填或 `provider set --models-path <地址>` 补上；厂商行不存在或一条路由都没声明时按 `base_url` 后缀猜（见 5.8）。
- 凭据头与地址一起定：地址来自哪条路由，就用那条路由的协议选凭据头（anthropic 用 `x-api-key`，gemini 用 `x-goog-api-key`，两条 openai 协议用 `Bearer`）；厂商级覆盖与 `--endpoint` 没有自己的协议，按厂商的有效默认协议发（账号行不再存协议，见 5.8）。deepseek 的 anthropic 账号取到的是 chat 路由上的 `/v1/models`，所以必须发 `Bearer`——按厂商默认协议发 `x-api-key` 只会换回 401/403，还把错误指到「凭据无效」上。模型列表请求也带厂商附加头：`setAuth` 设完凭据头再叠声明，`{model}` 在这里没有模型名，所以那条头不发（见 5.10）。
- `model sync --endpoint <地址>` 只影响这一次调用：值是相对厂商 `base_url` 的完整路径，也可以是整条地址（地址只在厂商行上，见 5.8），相对路径按 5.8 的规则规整（`v1/models`、`//v1//models/` 都落成 `/v1/models`）、整条地址校验后直接访问、不再拼 `base_url`；给了就忽略厂商覆盖、路由的 `models_path` 与 `base_url` 兜底，作用于被选中的每个账号。
- 响应形状自动识别，不加配置、不加 flag，按这个顺序试，第一个命中生效：`{data:[…]}`（OpenAI 兼容，主流）→ `{models:[…]}`（ollama 的 `/api/tags` 这类，gemini 也用它）→ 顶层数组（`["m1"]` 或 `[{"id":…}]`）。每个条目按 `id` → `name` → `model` 取第一个非空字符串，去空白、去重、排序，缺这些字段的条目跳过。判定只看字段在不在：只有字段不存在时才试下一种形状，字段存在但为空（`{"data":[]}`、`{"models":[]}`，或该字段是 `null`）就是「上游返回空列表」，算该账号失败，不拿后面的形状兜底——空列表与形状不对是两件事。三种形状都不匹配时报错，文案列出这三种形状并提示检查 `base-url`（也可以用 `--models` 手填）。
- gemini 的清单是第二种形状：`{"models":[{"name":"models/gemini-2.5-pro","supportedGenerationMethods":["generateContent","countTokens"]}],"nextPageToken":"…"}`。形状判定与上面完全一样，`{"models":[…]}` 就是 `{"models":[…]}`；另加两条后处理，只在这一次解析里生效（纯函数 `parseModelListPage` 与 `modelEntries`，见 `internal/gateway/models.go`）：
  - 方法过滤：响应里真的出现 `supportedGenerationMethods` 字段时，只保留该方法列表含 `generateContent` 的条目，embedding 一类没有这个方法的条目（以及过滤生效时压根没写这个字段的条目）不进清单——上游没说它支持生成，宁少不错。一个条目都没有这个字段时（ollama 的 `/api/tags`）不过滤，否则没有方法列表的上游会被过滤成空列表。
  - 去前缀：条目名以 `models/` 开头时去掉一个前缀再入库，只去一个（`models/models/x` 落成 `models/x`）。这条与形状无关、对所有形状生效：路由的 `{model}` 占位符在请求时只把裸名放回路径，库里存带前缀的名字会让上游收到 `models/models/…`。
- 分页：每个列表请求都带 `pageSize=1000`（不认识的厂商忽略这个参数），响应顶层有非空 `nextPageToken` 时带上 `pageToken` 取下一页，各页结果合并、去重、排序，最多 10 页。超过 10 页还在翻页时报错（文案说明清单过大、指出 `pageSize` 与地址），不静默截断：截断会让同步看起来成功，少掉的那几个模型永远选不到号，症状是「上游明明有这个模型却一直 404」。`nextPageToken` 是空串、只有空白或不是字符串时当作没有下一页。
- 元数据来源：条目里除了名字还带几列模型事实，认得出就写进目录，认不出就留空——字段缺失、0、负数都算「上游没给」，不猜默认值。端点名到线协议的映射表（`internal/gateway` 的 `endpointProtocols`）放在 gateway 而不是 `translate`：它服务的是模型同步与探活这两件上游侧的事，`translate` 仍然只做协议编解码，不认识厂商清单的字段名。

  | 上游 | 上下文 | 最大输出 | 支持的协议 | 其它（不解析） |
  | --- | --- | --- | --- | --- |
  | commandcode | `context_length` | - | `supported_endpoints`：`/messages` → `anthropic`、`/chat/completions` → `openai-chat`、`/responses` → `openai-responses`、`/systemone` → `systemone` | `owned_by`、`name`（名字已按 `id` → `name` → `model` 取过） |
  | openrouter | `context_length` | `top_provider.max_completion_tokens` | - | `supported_parameters`（能力位暂不解析，见 5.13） |
  | gemini | `inputTokenLimit` | `outputTokenLimit` | `supportedGenerationMethods`：只有 `generateContent` 折成 `gemini` 协议，`countTokens`、`embedContent` 是接口动作而不是协议 | `displayName` |
  | 其它 OpenAI 兼容 | - | - | - | 只有名字 |

  三种形状（`{data:[…]}`、`{models:[…]}`、顶层数组）读的是同一批键，条目是字符串时只有名字、元数据留空。协议集合落库前按固定顺序 `anthropic` → `openai-chat` → `openai-responses` → `gemini` → `systemone` 排好、去重，空集合存空串（见 5.13）。未识别的端点名不计：猜一个相近的名字等于给探活安排一条这家未必声明过的路径。同名的条目只留第一次出现的那一条，元数据不跨页合并。
- 结果分两处写：清单里的名字按 `(厂商, 模型)` upsert 进目录 `provider_models`（`source='sync'`，只写上游给出的字段，见 5.13）。目录由 gateway 在拉清单这一层写——路径、凭据头与解析都在这里，再让调用方读一遍 JSON 就是第二份解析规则；写目录失败整次同步失败，不静默丢元数据。可达性由命令层写：该账号对这些名字标 `access='allowed'`，但只给**还没有行**的名字插，已有行一个字段都不动。
- 同步不推翻探活结论：可达性是账号侧的结论，探活说了 `forbidden` / `missing` 就不再被一次清单翻回 `allowed`——上游清单会列出这个 key 用不了的模型（实测 bigmodel 的清单里有 `glm-4.6-air`），翻回去就是把一个已知不可用的名字重新放进候选池。写入因此只有 `INSERT … ON CONFLICT(account_id, model) DO NOTHING` 这一种形状，已有行的 `access`、`http_status`、`error`、`checked_at` 与 `protocol` 原样保留，只有新出现的名字拿默认值。
- 上游清单里消失的名字不删行：一次少给了几个名字不等于这些模型不可用，可达性只由探活结论改动（见 5.13）。
- 这条命令同时改目录与可达性，所以新出现的名字同步完就能选号；已有行的可达性不变。
- 某个账号失败不打断其余账号：全部处理完才返回，退出码 1，失败原因写在那一个账号的备注列。

`model probe [--provider 厂商] [--account 账号] [--sync] [--model 模型] [--limit N] [--concurrency N] [--max-age 时长] [--refresh]` 只解决后者：逐账号处理，对该账号的每个模型发一条 `max_tokens=1` 的请求，按响应判定可用性。结论写进可达性：`store.SetAccountModelAccess` 一次写 `access`、`http_status`、`error` 与 `checked_at`，发现到的协议由 `store.SetAccountModelProtocol` 写，目录里没有的模型补一行 `source='probe'`（见 5.13）。下一轮按 `checked_at` 决定哪些模型不用再花钱。

| 上游响应 | 可达性 | 含义与处理 |
| --- | --- | --- |
| 2xx | `allowed` | 可用，进入候选池 |
| 401 | 不写 | 凭据被拒，同时把账号停用；这是账号的结论而不是这个模型的结论，可达性那一栏留空（`gateway.ModelProbe.Access` 为空），整次探活到此停下 |
| 403 | `forbidden` | 套餐或权限不含该模型，凭据本身还能用 |
| 404 | `missing` | 上游不认识这个模型名 |
| 400 且错误文本明确说模型不存在 | `missing` | 按模型名不存在处理 |
| 其它 | `error` | 临时错误，结论为不可用 |

协议发现。协议真正的归属是 `(base_url, model)` 而不是账号：同一家上游的 `claude-*` 只接受 `/messages`，其余模型走 `/v1/chat/completions`，用的是同一个 key。所以探活除了判定可用性，还顺手把「这个模型该用哪种协议」测出来，结论写进 `account_models.protocol`（见 6）。

- 账号所属厂商的默认协议一定要试一次，那一次的结论先存着，它就是「按厂商默认配置打」的真相：`401` 判定与候选用完后的结论都取它。它不一定排第一个（见下面「顺序」那条），所以循环按协议名认住这一行，而不是按位置。
- 厂商默认协议那一次失败时，只有可能只是路径不对的才换协议再试：`404`（路径不存在）、`405`（方法不允许）、`403`（有的网关按路径拒，commandcode 对非 `/messages` 的 `claude-*` 就是 403），以及 `400` 且错误文本指向端点、路径或「不支持该协议」。其余失败（例如明确说模型不存在、`5xx`、网络错误）直接按上表判定，不再花调用。
- 换协议之后的那几次失败都不作数：不管是什么原因，都接着试剩下的候选，直到候选用完，结论仍回厂商默认协议那次。中途停手会让排在后面的候选永远试不到，而换协议后的 `401` 本来就是常态（凭据头形状不对）。
- 顺序：库里已经发现过这个模型的协议排最前，接着是目录里这个模型声明的协议（`provider_models.protocols`，见上面的「元数据来源」），然后厂商的有效默认协议，其余按候选列表的顺序。清单声明的协议排在厂商默认协议前面是为了少打错协议的请求：commandcode 的 `claude-*` 只接受 `/messages`，按厂商默认的 chat 先打只会先换回一个 403。已发现的结果仍压过清单声明——前者意味着这个账号上真的用这条协议打通了，比上游清单更贴近事实。清单声明只在候选范围之内生效：这家没有声明那条路由时不打，否则就是去打一条没声明过的路径（见 5.8）。账号行不存协议，所以候选里没有「账号协议」这一档。库里已有的发现结果排前面是为了让 `--refresh` 重探已知模型时不再白打一次 `404`。顺序固定，同一模型多次探活得到同一个结论，不随运行顺序变化。
- 候选协议由 `gateway.Options.RouteProtocols`（resolver）给出：入参是账号的厂商与模型名，默认实现取该厂商在 `provider_routes` 里声明过的协议（见 5.8）；没注入、或该厂商一个路由都没声明时它们就是三条入站协议全试（gemini 不在兜底里：只有声明过 gemini 路由时才进候选）。resolver 负责去重与校验，只认 `store.IsWireProtocol` 认的线协议，出现别的值直接报错——静默落进 chat 分支会把一条配置错误变成对错误地址的上游调用。循环本身不认识厂商，也不会对着不存在的端点白打一次。
- 第一个 `2xx` 就是结论：可达性记 `allowed`，把这次用的协议写进 `account_models.protocol`，剩下的协议不再试。
- `401` 一律不换协议：凭据问题与路径无关，照旧停用账号并立即停止整次探活。这一条只判厂商默认协议那一次；换协议之后的 `401` 多半是凭据头形状不对（anthropic 用 `x-api-key`，gemini 用 `x-goog-api-key`，两条 openai 协议用 `Bearer`），既不停用账号也不算凭据被拒。
- 候选协议都没试出 `2xx` 时，结论取厂商默认协议那一次的判定（状态、HTTP 状态与错误文本），`protocol` 保持原值不动。协议列只在发现成功时写，一次失败不会把之前测出的协议抹掉，也不会凭空写入一个。

花费：先用厂商默认协议试，成功时只花一次调用，只有失败才会多花一到两次。同一模型重复探活的结果稳定，代价最多是每次 `--refresh` 多一两次 `max_tokens=1` 的调用，跟 `--max-age` 省下的调用相比是小头。每次上游调用都在 `requests` 里各留一行（见 5.5），打出几次就记几行。

`account test` 只判凭据与连通性，模型是否可用由 `model probe` 决定，所以它分两段验：先 `GET` 模型列表证明地址通，再发一条 `max_tokens=1` 的请求证明凭据有效。第一段：地址按 `model sync` 同一套规则解析（见 5.8），凭据头跟取到地址的那条路由的协议；2xx 说明地址通、上游连得上，顺带报出上游模型数，这一条不花 token，也不依赖某个模型名能不能用。列表那一段到此为止——列表接口可能是公开接口，commandcode 的 `GET /provider/v1/models` 对任何 key（甚至没有 key）都回 200 与整份清单，`POST /provider/v1/chat/completions` 才回 `401 Invalid 'Authorization' header or token`：光凭 2xx 判不出凭据好不好，所以 2xx 之后**仍然**发第二段。列表接口不可用时（404、405、403，或这家声明了路由却一个 `models_path` 都没有，或连不上）也走这条最小调用，只是备注里写清第一段为什么没成。第二段：按账号所属厂商的有效默认协议打（账号行不再存协议，见 5.8），不看模型上的协议发现结果；算不出协议时（厂商没有路由、默认协议为空）这一步直接报错，文案给出下一步命令 `lax provider set <厂商> --openai /v1/chat/completions`，不落到 chat 分支去打一个猜出来的地址。模型名按三档取：该账号 `access='allowed'` 的第一行（按名字升序，也就是 `store.AllowedModels` 的第一项）→ 第一段列表那一步拿到的第一个模型名（按名字排序去重后的第一项）→ 默认模型名 `default`。前两档都是上游真实存在的名字，第三档只是没有名字可用时的兜底：刚建好、还没探过模型的账号走第二档——列表已经报出了一个模型名，再拿固定名字去打只会把「模型名不对」误报成账号失败（只提供 `gemini-3.8-flash` 的中转对 `default` 回 503「无法从令牌路由为模型 default 选择渠道」）；列表接口不可用或清单为空时才回退到 `default`，既不 panic 也不要求先同步：凭据好不好用得先能试出来。备注写清这次用的是哪一个、来自哪一档（账号清单 / 上游清单 / 兜底默认名），否则「模型调用返回 404」看不出是名字不对还是模型真的不能用。两段都通过才判正常，状态与退出码按第二段算；例外是第一段自己就给出了结论（401 是凭据问题，429/5xx 与连接层失败要按 5.3 的三档打冷却，其它 4xx 只报原话），这时不再花一次调用。两段的状态处理一致（见 5.3），备注与 `--json` 写清两段各自的结果（`--json` 的 `list` 对象是第一段自己的结论，`stage` 是判定成败的那一步），因此「列表 404 之后模型 403」与「列表接口自己 403」不会读成同一个结论。

参数与处理顺序：

- 该账号没有模型行，或给了 `--sync` 时，先同步一次，不需要手工跑两遍。自动同步失败的账号跳过不探，其余账号照常处理，最终退出码 1。
- 增量跳过：模型同时满足「可达性有定论」与「`checked_at` 距当前不超过 `--max-age`」时跳过，不发上游请求。有定论的意思是 `access` 是 `forbidden` 或 `missing`（本身就是一个结论），或者是 `allowed` 且 `checked_at` 不为 0（探过、可用）；`checked_at` 为 0 的 `allowed` 只是同步或 `--models` 手填进来的名字，没探过，照常探活。有定论的行重探一遍结论不会变，跳过省下的是真实计费调用；`error` 与没探过的行照常探。`checked_at` 为 0 视为不新鲜。
- `--refresh` 忽略新鲜度全部重探，`--max-age 0` 与它等价。`--max-age` 默认 `24h`，新鲜度对 `--model` 指定的单模型同样生效，`--refresh` 可以强制重探。
- 跳过的模型不写库、不产生输出行，`--json` 里也就没有它们。
- `--model <模型>` 只探指定的一个模型名。
- `--limit N` 截断的是跳过之后剩下的待探模型，不是库里的模型总数：多账号时每个账号各有 N 个额度，跳过的模型不占额度。
- `--concurrency N` 改并发，默认 4，且不超过待探的模型数。
- 一个账号探完立刻写回结果并打印该账号的「本次探活 / 跳过 / 可用 / 无权 / 不存在 / 失败」计数，再处理下一个账号。

探活是要花钱的。每个模型的一次判定都是一次真实的上游调用，`max_tokens=1` 也照样计入上游账单与速率限制，所以模型多的账号用 `--limit` 控制这一次的花费，重复跑同一批账号靠 `--max-age` 与 `--refresh` 控制。得出判定的那次调用在 `requests` 里留一行，`usage` 与 `log tail` 里都能看到（见 5.5）；换协议多打的调用没有 token，不单独记行。

任何一个账号碰到 401，整次探活就停下并返回错误：凭据都不行了，继续探下去只是重复同一条错误。此时该账号已被停用，后续账号不再处理，其余结论不作数，也不会写回可达性——即使同批里已经探出了可用模型，也不写，避免把一次凭据故障当成新的事实。

一个账号的待探模型被全部跳过时，这次一条上游请求都不发，但仍按库里全部 `access='allowed'` 的行报可用数与退出码。报错与否只看终态：库里至少一个 `allowed` 的账号算已满足，退出码 0；全部账号库里都零可用时退出码仍是 1，错误信息带上本次的探活数与跳过数（例如「本次探活 0 个、跳过 7 个」），跳过一个以上时再提示结论仍在 `--max-age` 窗口内、可用 `--refresh` 强制重探。「全被跳过」与「探了都不行」因此能从文案上分开，而不是一律落成同一句。

厂商与路由。转发地址同样按 `(账号 provider, 协议)` 查路由（见 5.8），不再由 `base_url` 的后缀猜：同一家上游的 chat 与 anthropic 路径可以完全不同，猜后缀必然在其中一种协议上猜错。

探活结果反过来成为选号依据：一个账号库里至少一个模型是 `allowed`，5.2 与 5.3 的候选集合就按这些行算，不再有第二份账号级清单要同步。取全库而非本次结果，是因为 `--limit` 与 `--model` 都可能只探一部分：只探两个却把另外三个之前探通的模型从选号里除掉，属于退化。某账号零个可用模型时它自然不进候选，目录行与可达性行（`forbidden` / `missing` / `error`）都留着，等下一轮探活或手工改可达性时不用重建；一次网络抖动因此不会把账号清空成「什么都不提供」，也不会让它接下任何模型名。所有账号都没探出可用模型时退出码 1。

### 5.8 厂商与路由

出站地址不由代码里的厂商表决定，也不靠 `base_url` 末尾有没有 `/v1` 猜后缀：同一个 `base_url` 上不同协议的路径并不一样，猜出来的地址对其中一种协议一定是错的。deepseek 的 chat 在 `/v1/chat/completions`，anthropic 在 `/anthropic/v1/messages`；commandcode 的 base 是 `https://api.commandcode.ai/provider/v1`，chat 在 `/chat/completions`、anthropic 在 `/messages`，模型列表在 `/models`。

- `providers` 一行提供 `base_url`、备注、一列默认协议与一列模型列表路径覆盖；协议的路径细节一律在 `provider_routes` 上，厂商行不存转发路径。
- `provider_routes` 按 `(厂商, 协议)` 存完整路径 `path` 与模型列表路径 `models_path`。`path` 是相对路径时，完整地址是 `base_url` 去掉末尾斜杠后直接拼它；`path` 是整条地址时直接用它，不做任何猜测与补全（两种写法见下一条）。`path` 里可以出现 `{model}` 占位符，两种写法都支持：拼好的完整地址在发请求之前把 `{model}` 换成转义后的模型名（见 5.9）。规整只合并斜杠，花括号与冒号原样落库。
- 路由的 `path`、路由与厂商的 `models_path`、`model sync --endpoint` 三处收两种写法，判定与解释只有一份（`store.IsFullAddress` 与 `store.NormalizeEndpoint`）：含 `://` 的是整条地址，用 `url.Parse` 校验 scheme 为 `http` 或 `https`、主机名非空，规整时 scheme 归一成小写、路径合并重复斜杠去尾斜杠，授权部分、查询串与 `#` 之后原文保留，使用时不再拼 `base_url`；不含 `://` 的是相对路径，走 `NormalizePath` 拼厂商的 `base_url`（账号行不再存地址）。整条地址让同一厂商的不同协议落在不同域名上：openai 兼容端点在 `https://openai.example.com/v1/chat/completions`、anthropic 端点在 `https://claude.example.com/v1/messages`，此时 `base_url` 可以不写（见下一段的「整条地址路由不需要 base_url」）。`https://`、`https:///x` 这类只有 scheme 没有主机名的写法报错，`ftp://host/x` 这类其它 scheme 同样报错，错误文案用中文指出缺什么；写入侧（`CreateProvider`、`SetProvider`、`AddProvider`）碰到坏整条地址整笔拒绝、一个字段都不落库，读取侧（`joinEndpoint`、`modelsEndpoint`）把存量的脏值原样交给 `http.NewRequest` 报错，不改写用户数据。
- 相对路径只规整、不拒绝输入，规则是 `store.NormalizePath`（由 `store.NormalizeEndpoint` 在取值不含 `://` 时调用）：去掉首尾空白、把连续斜杠合并成一个、补上恰好一个前导斜杠、去掉末尾斜杠；空串、只有斜杠、只有空白的输入一律视为空。`v1/chat/completions`、`//v1//chat//completions`、`/v1/chat/completions/` 因此是同一个地址，`--openai`、`--models-path`、`model sync --endpoint` 都不再要求「必须以 `/` 开头」。写入侧（`CreateProvider`、`SetProvider`、`AddProvider` 的 `path` 与 `models_path`）与读取侧（拼转发地址、拼模型列表地址、`--endpoint`）走同一个入口：库里存的永远是规整形态，读取侧拼地址时再过一遍，历史库里的不规整写法因此也拼得出正确地址。理由：路径是照着上游文档抄的，多一个或少一个斜杠是写法差异，不是用法错误；为此报错只会让人先改命令再重敲一遍，得到的仍是同一个地址。真正的用法错误是非空字段规整之后为空（删除语义归 `provider unset`），以及整条地址校验不过，那两条仍然报错。
- 整条地址路由不需要 `base_url`：每一条声明的路由与要落库的 `models_path` 都是整条地址时，`base_url` 拼不进任何地址，写一个「为了过校验」的地址只是无效配置。`provider add` 与 `provider set` 因此允许省略 `--base-url`（库里的 `providers.base_url` 存空串）；账号行没有地址这一列，`account add` 也不需要占位地址；读取侧（`joinEndpoint`、`modelsEndpoint`）对这种行只走整条地址，不拼 `base_url`，因此不会拼出 `//` 开头的畸形地址。真实例子是 bigmodel 的三套协议分属不同 host 与路径：anthropic 在 `https://open.bigmodel.cn/api/anthropic/v1/messages`、chat 在 `https://open.bigmodel.cn/api/coding/paas/v4/chat/completions`、responses 在 `https://open.bigmodel.cn/api/v1/responses`，模型列表是 chat 那条推导出的 `https://open.bigmodel.cn/api/coding/paas/v4/models`，没有哪条地址用得上 `base_url`。`provider ls` 的地址列对空值显示 `-`。
- 缺 `base_url` 与相对路径互斥，这一条只有一份判定（`store.requireBaseURLCoverage`，三个写入口 `CreateProvider`、`SetProvider`、`AddProvider` 都调它），cli 只把它的错误包成用法错误（`store.ErrBaseURLRequired` → 退出码 2），不另算一份：有一条路由（或厂商级 `models_path`）是相对路径又没给 `base_url` 时整笔拒绝、一个字段都不落库，文案说清是哪条路由、并给出两条出路——「需要 --base-url」或「把它写成整条地址」。`provider set` 把一条路由从整条地址改成相对路径、同时没给 `--base-url`、库里也没有地址时同样整笔拒绝（要连地址一起给就在同一次调用里带上 `--base-url`）。空的 `base_url` 加一条路由都不给仍是错误（`provider add` 至少要一条路由），行为不变。`base_url` 不在 `provider unset` 的字段里：它只能由 `provider set` 换成另一个地址。
- `base_url` 同理只规整路径、不拒绝输入（`store.NormalizeBaseURL`），校验用 `url.Parse` 做：scheme 只认 `http` 与 `https`，大小写不敏感，落库时归一成小写；缺主机名是错误——`https://` 里没有地址，规整出来的 `https:` 拼不出可用的请求地址，不能当成一条 `base_url` 落库；缺 scheme 与别的 scheme 同样报错。规整只作用在路径部分：合并重复斜杠、补一个前导斜杠、去掉末尾斜杠，`https://api.example.com//api//paas//v4/` 落成 `https://api.example.com/api/paas/v4`——真实存在的前缀原样保留。授权部分（含端口与用户信息）、查询串与 `#` 之后的内容一个字不动：那里的连写斜杠是值的一部分，合并会改掉语义；`..` 与 `.` 段也原样保留，静默改写等于把地址换成另一个地址。拼地址时两侧（`base_url` 的路径与路由的 `path`）都过这套规则，最终地址的路径里不出现 `//`；`http://`、`https://` 里那一对斜杠不属于要合并的范围。
- 协议由四个 flag 声明：`--anthropic`、`--openai`（openai-chat）、`--responses`（openai-responses）、`--gemini`（gemini 的入站与出站都可用，非流式与流式都有，见 5.9）。gemini 路由的 `path` 里可以写 `{model}` 占位符，写入侧只合并斜杠，花括号与冒号原样落库；没有 gemini 路由的 gemini 账号走 `base_url` 后缀兜底。`provider add` 用它们新建，`provider set` 用它们覆盖或新增一条路由，删掉一条路由是 `provider unset <名称> <协议>`：传空值是用法错误，删除只在 `unset` 里发生。`--default-protocol` 认命令行写法 `anthropic`、`openai`、`responses`、`gemini`，也认线协议全名 `openai-chat`、`openai-responses`，两种写法落到同一个线协议，输入 `openai` 就是 chat，`gemini` 两种写法同形；错误文案用命令行写法举例。
- `models_path` 的推导只发生在写入侧，读取侧不拿它重算。写入时没给模型列表地址的路由按规则补默认值（`store.DeriveModelsPath`）：openai-chat 路由把尾部 `/chat/completions` 换成 `/models`（`/v1/chat/completions` → `/v1/models`、`/chat/completions` → `/models`；整条地址则在同一个 scheme + host 上换尾段，`https://openai.example.com/v1/chat/completions` → `https://openai.example.com/v1/models`，查询串与 `#` 之后跟着原样带过去，不换域名），其它协议留空，表示这条协议没有自己的列表接口、读取时回退到同厂商其它协议的非空值。推导值跟着路径走：改路径时只有「空」或「原本就是旧路径的推导值」才换成新推导值，手工填过的值不动。内置 `anthropic` 厂商只有 anthropic 一条路由，没有 openai 路由可以推导，它的 `/v1/models` 因此不会因为重写那条路由而消失——读取侧推导会让 anthropic 型账号彻底失去模型列表来源。
- `providers.models_path` 是厂商级覆盖：非空时 `model sync` 直接用它，不看路由；空串表示不覆盖、按路由取。它同样收相对路径与整条地址两种写法。要改一家上游的列表地址又不想逐条写 `models_path` 时用它。
- 模型列表的凭据头跟取到路径的那条路由的协议走（见 5.7），不是厂商的默认协议：deepseek 的 anthropic 账号用的是 chat 路由上的 `/v1/models`，就得发 `Bearer`。
- 地址与默认协议只有一份，在厂商行上：账号只存 `provider` 名字，不存 `base_url`、不存 `protocol`——两列由迁移 `0014` 从 `accounts` 表上删掉，转发时按账号的 `provider` 取 `providers.base_url` 与它的有效默认协议，逐模型的协议覆盖仍在 `account_models.protocol`（见 5.7）。代价是改厂商地址会立刻影响所有引用它的账号（这是单一来源应有的行为），而且用 `--force` 删厂商会让指向它的账号彻底失去地址——`provider rm --force` 因此在同一个事务里把这些账号停用（`enabled=0` 并写明 `last_error`，文案「厂商已删除，请 provider add 重建或修改账号的厂商」），免得它们留在选号池里必然 502。停用只动状态，账号行、凭据、模型清单与探活结果都不删，用 `provider add` 重建同名厂商或改账号的厂商之后 `account enable` 就能接着用。
- 两列删掉之后，账号的地址与默认协议是**只读派生值**，规则只有一份：`store.Account` 的 `BaseURL` 取 `providers.base_url`，`Protocol` 取这家厂商的有效默认协议（`ResolveDefaultProtocol(default_protocol, routes)` 的结果，显式值落不到已声明的路由上就按固定顺序回退）；厂商行不存在时两者都是空串，厂商行在但一条路由都没声明时 `Protocol` 是空串。`ListAccounts`、`GetAccount`、`ListAccountsByProvider` 三个读入口共用 `queryAccounts` 一份实现，写入侧（`CreateAccount`）不看这两个字段，也不回写。
- 删厂商两个入口：`store.DeleteProvider` 只删厂商行与它的路由，账号状态原样保留（`provider rm` 不带 `--force` 时调用方已经先拒绝）；`store.DeleteProviderForce` 是 `provider rm --force` 的入口，在上面那一步之外把指向它的账号逐个停用，与 `SetEnabled` 走同一条写路径 `setEnabledTx`。
- 厂商行存在但没有该协议的路由时，退回原来的后缀猜测：末尾是 `/v1` 就补 `/chat/completions`、`/messages` 或 `/responses`，否则补 `/v1` 再接同样的后缀；模型列表同理补 `/models` 或 `/v1/models`。自建地址因此仍然可用——模型上的发现结果压过厂商默认协议、而这家没声明那条路由时走的就是这一条。猜后缀要用 `base_url`，厂商用的是整条地址路由（`base_url` 为空）时无后缀可猜，此时报错并提示用 `provider set <厂商> --<协议> <路径>` 声明这条路由。
- 账号的 `provider` 在表里查不到（历史库里的孤儿行）是另一种情况：地址与默认协议都只从厂商行取，孤儿行两样都是空串，拼不出任何可用地址。转发以 503 结束（错误体按入站协议给形状，见 5.2），文案提示先 `lax provider add <厂商> --base-url <地址> --openai /v1/chat/completions` 重建厂商、或用 `lax account set <账号> --provider <厂商>` 把账号指到已有厂商；`model sync` 与 `account test` 同样在取地址这一步就报这条错。`provider rm --force` 之后的账号不落到这里：存储层已经在同一个事务里把它们停用（见上一条），选号不会选中。
- 算不出有效出站协议时（一条路由都没声明，或默认协议落不到已声明的路由上——例如只声明了 gemini 一条路由却没给 `--default-protocol`，而模型上也没有发现结果），转发同样是 503 而不是编码器报「不支持的出站协议 ""」：错误体按入站协议给形状，文案给出下一步命令，例如 `lax provider set <厂商> --openai /v1/chat/completions` 声明一条路由，或 `lax model probe --account <账号>` 探出这个模型的协议。这一类失败不动账号的冷却与 `fail_streak`：它是一次配置缺口，不是这家上游打不通。
- 探活的候选协议跟着路由收紧：只用该厂商在 `provider_routes` 里声明过的协议（含 gemini），不去打这家没有声明的路径；没有厂商行的账号仍三条入站协议全试。顺序（已发现的协议、目录里声明的协议、厂商默认协议在前，其余按协议名排序）见 5.7。读库失败时报错，不放宽成全部协议全试——放宽等于去打这家没声明的路径，把一次读库故障变成对错误地址的上游调用。
- 默认协议：显式值优先，但必须落在这家已声明的路由上；否则按固定顺序 `openai-chat` → `openai-responses` → `anthropic` 取第一条已声明的路由，一条路由都没有时为空。有效出站协议在账号没有逐模型覆盖时就用它（见 5.7），`provider ls` 也显示它，两处同一个规则。规则是纯函数 `store.ResolveDefaultProtocol`，读库入口是 `store.EffectiveProviderProtocol`：厂商不存在返回 `ErrProviderNotFound`，厂商在但一条路由都没有时返回空串与 `ErrProviderNoRoutes`，提示文案由调用方写，`cli` 不再自己算这条回退。**不能靠协议名字典序兜底**：deepseek 与 commandcode 都同时声明了 `openai-chat` 与 `anthropic` 两条路由，字典序把 `anthropic` 排在前面，默认协议就会从升级前的 `openai-chat` 变成 `anthropic`，这两个厂商的账号在升级前后默认协议不一致。升级前的默认值因此由迁移 `0007` 逐家显式种进 `default_protocol`：`anthropic` 自己是 `anthropic`，其余 7 家是 `openai-chat`。
- 默认协议的写入侧与读取侧分工：`provider add` 的 `--default-protocol` 必须落在这次声明的路由上，`provider set` 改完之后必须仍然指向一条存在的路由，`provider unset <协议>` 删掉的正好是默认协议时也整笔拒绝（提示先改默认协议再删），两处都是同一个事务，指不到就一条都不落库。读取侧对历史脏数据（旧版本删路由时没清默认协议）按固定顺序回退到仍然存在的路由，不静默交给后缀猜测。
- `base_url` 的 `http://`、`https://` 前缀在 `store` 的三个写入口（`CreateProvider`、`SetProvider`、`AddProvider`）里校验并规整（合并重复斜杠、去掉末尾斜杠），整条地址的 `path` 与 `models_path` 用的是同一套校验（`store.NormalizeEndpoint`），`cli` 那层不再是唯一防线：脏地址一旦落库，转发与模型同步都会拿它拼路径。给了地址就必须是带主机名的完整地址；空地址的合法性由 `requireBaseURLCoverage` 单独判（每一条路由与 `models_path` 都是整条地址，见上一条）。读侧拼地址用的也是同一个 `store.NormalizeBaseURL`。
- `provider add` 只新建，`provider set` 只改传入的字段；两者都在一个事务里写厂商行与路由，失败时不留半条记录。`provider rm` 删厂商行与它的全部路由，仍有账号指过来时先列出这些账号并拒绝，`--force` 才删；账号行只存 `provider` 字符串，地址与默认协议都从厂商行取，所以删厂商之后指向它的账号既没有地址、也没有路由（路由随厂商一起消失），`--force` 因此把它们停用，重建厂商或改账号的厂商之后才谈得上转发。`base_url` 为空的厂商（它的路由与模型列表地址都是整条地址）删掉之后同理。
- 凭据头仍按协议而不是按厂商：anthropic 用 `x-api-key` 加 `anthropic-version`，gemini 用 `x-goog-api-key`，两条 openai 协议用 `Bearer`（见 5.9 与 gateway 的 `setAuth`）。

`base_url` 与 `path` 拼出来的完整地址（`path` 是整条地址时直接用，不拼 `base_url`），几行示例：

| 厂商 | 默认协议 | `path` 的写法 | 完整地址 |
| --- | --- | --- | --- |
| deepseek | openai-chat | `/v1/chat/completions` | `https://api.deepseek.com/v1/chat/completions` |
| deepseek | anthropic | `/anthropic/v1/messages` | `https://api.deepseek.com/anthropic/v1/messages` |
| commandcode | openai-chat | `/chat/completions` | `https://api.commandcode.ai/provider/v1/chat/completions` |
| commandcode | anthropic | `/messages` | `https://api.commandcode.ai/provider/v1/messages` |
| split | openai-chat | `https://openai.example.com/v1/chat/completions` | `https://openai.example.com/v1/chat/completions` |
| split | anthropic | `/v1/messages` | `https://claude.example.com/v1/messages`（`base_url` 是这一家） |
| google-gemini | gemini | `/v1beta/models/{model}:generateContent` | `https://generativelanguage.googleapis.com/v1beta/models/{model}:generateContent`（`{model}` 在发请求前换成裸模型名） |

种子数据在迁移 `0006` 里，默认协议在迁移 `0007` 里，厂商级模型列表覆盖那一列在迁移 `0008` 里，gemini 的厂商行与路由在迁移 `0009` 里，都不在代码里。`providers` 表为空不算错误：用户可以把种子厂商删光，此时所有账号走兜底。维护用 `provider` 子命令（见 4）。

### 5.9 gemini 入站与出站

gemini 的原生接口（Generative Language API 的 `generateContent`）与另外三条协议有几处结构差异，入站与出站两边都要处理：

- 模型名在 URL 路径里：非流式是 `POST {base}/v1beta/models/{model}:generateContent`，流式是 `POST {base}/v1beta/models/{model}:streamGenerateContent?alt=sse`。请求体里没有 `model` 字段，跨协议转换时也没有地方写它，所以模型名只能靠路径：出站靠路由的 `{model}` 占位符，入站靠解析客户端给的路径。
- 流式也由路径决定：`:streamGenerateContent` 后缀加 `alt=sse`，请求体里没有 `stream` 字段，非流式与流式的请求体形状完全一样。
- 凭据不走 `Authorization` 也不走 `x-api-key`：`x-goog-api-key: <key>`。这条按协议决定，与厂商无关——同一家代理可能同时提供 gemini 原生端点与 openai 兼容端点，地址不同、头也不同，按厂商选头必然在其中一条上错。
- 错误体是 `{"error":{"code":…,"message":…,"status":…}}`，流里的错误也是这个形状。入站 gemini 的回包按同一个形状写（见 5.2）：`code` 是本次 HTTP 状态码，`status` 由它映射。
- 结束原因是 `finishReason`（`STOP`、`MAX_TOKENS`、`SAFETY` 等），没有「因为工具调用而停」这一档。

#### 入站路径

- `POST /v1beta/models/{model}:generateContent`（非流式）与 `POST /v1beta/models/{model}:streamGenerateContent`（流式，SSE）是入站 gemini 的两条路径。模型名是前缀与动作后缀之间的那一段，按 `url.PathUnescape` 还原，`:` 之后的后缀不算模型名的一部分；模型名里带 `/` 的名字写成 `org%2Fgemini` 也能取到（`http.ServeMux` 按前缀注册路由，取值用转义后的路径）。两条路径都只收 POST，其余方法 405 带 `Allow: POST`；后缀不是这两个动作时 404。
- 报文里没有模型名，路径里的名字就是这一次请求的模型名：它参与选号与有效协议解析，与另外三条入站从请求体 `model` 字段取名字的效果一样。请求体里就算带了 `model` 字段也不看，路径优先。
- 流式由 `:streamGenerateContent` 后缀决定，不看请求体：同一份请求体发给非流式路径就是非流式响应。

#### 出站地址与占位符

- 路由的 `path` 里可以出现 `{model}`，出现一次或多次都行，只替换这一个字面量。替换发生在地址拼好之后、发请求之前（`gateway.modelEndpoint` 对完整地址做 `strings.ReplaceAll`），替换值是 `url.PathEscape(模型名)`：模型名按路径段转义，`org/gemini` 这样的名字落成 `org%2Fgemini`，不会被拆成两段路径；路径里的 `:`（`:generateContent`）与查询串一个字不动。
- 两种写法与占位符共存：相对路径 `/v1beta/models/{model}:generateContent` 由 `base_url` 拼上；整条地址 `https://generativelanguage.googleapis.com/v1beta/models/{model}:generateContent` 直接使用、不再拼 `base_url`。`store.NormalizePath` 与 `store.NormalizeEndpoint` 只合并斜杠，花括号与冒号原样落库，读取侧拼地址时再过一遍同样的规则。
- 不含占位符的路由行为不变：地址里没有 `{model}` 就一个字节都不改，另外三条协议因此不受影响（它们的路径里本来就没有模型名）。
- 没有厂商行、或厂商没有 gemini 路由时退到后缀兜底，兜底值同样带占位符：`{base}/v1beta/models/{model}:generateContent`（`base_url` 末尾写了 `/v1beta` 也不会拼出两层）。只给 `--base-url` 与 `--protocol gemini` 就能用，不必先声明路由。
- 路由与非流式写法一样只写 `:generateContent`：流式请求在拼好地址、替换完占位符之后把末尾的 `:generateContent` 换成 `:streamGenerateContent` 并补一个 `alt=sse`（`gateway.geminiStreamEndpoint`，纯字符串拼接，不经 `url.Parse`，模型名的转义形式原样保留）。路由本来就写 `:streamGenerateContent` 时只补 `alt=sse`；没有这个后缀的自定义路由路径不动、只加 `alt=sse`。不加 `alt=sse` 的话上游返回的是一段 JSON 数组，不是 SSE，客户端的流式解析会直接断在半路。

#### 请求映射（中间表示 → gemini）

| 中间表示 | gemini 请求 |
| --- | --- |
| `Request.System`，以及 `role=system` 消息 | 顶层 `systemInstruction.parts[]`（多段按换行拼成一段文本） |
| `RoleUser` 消息 | `contents[]` 里 `role=user` 的一条 |
| `RoleAssistant` 消息 | `contents[]` 里 `role=model` 的一条（gemini 只有 `user` 与 `model` 两种角色） |
| `RoleTool` 消息（工具结果） | 折进 `role=user` 的 `functionResponse` part；之后紧跟的 user 内容并进同一条，`functionResponse` 在前 |
| `text` 块 | `parts[].text` |
| `image` 块（data URI） | `parts[].inlineData`（`mimeType` + `data`） |
| `image` 块（http(s) 地址） | `parts[].fileData.fileUri` |
| `tool_use` 块 | `parts[].functionCall`（`name` + `args`） |
| `tool_result` 块 | `parts[].functionResponse`（`name` 由 `tool_use_id` 查历史里的 `tool_use` 得到，正文写成 `{"result": <文本>}`） |
| `tools[].name` / `description` / `input_schema` | `tools[].functionDeclarations[]` 的 `name` / `description` / `parameters` |
| `max_tokens` / `temperature` / `top_p` / `stop_sequences` | `generationConfig` 的 `maxOutputTokens` / `temperature` / `topP` / `stopSequences` |
| `tool_choice` = `auto` / `required` / `none` | `toolConfig.functionCallingConfig.mode` = `AUTO` / `ANY` / `NONE` |
| `tool_choice` = 指定工具 | `mode=ANY` 加 `allowedFunctionNames: [name]` |
| `tool_choice` = `allowed_tools` | `mode=ANY` 加完整的 `allowedFunctionNames`（名字全部搬，不挑子集） |
| `Thinking.Budget` / `Thinking.Effort` | `generationConfig.thinkingConfig.thinkingBudget`（档位按 5.4 的档位→预算表折；`Enabled=false` 写 0） |

表里看不出来的几条取舍：

- 工具定义只搬客户端函数：anthropic 的服务端内置工具（`type` 不是 `custom`）整条丢弃；gemini 自己的内置工具（`googleSearch`、`codeExecution` 一类）不在映射表里，也不伪造——客户端在 gemini 报文里写了 `googleSearch`，它不进中间表示，转给别的协议时自然消失（见第 7 节已知边界）。
- 工具调用的 id 出站时丢弃：gemini 的 `functionCall` 没有 id，`functionResponse` 按函数名对回调用。编码时先按历史里的 `tool_use` 建一张 `id → 函数名` 的表，查不到名字的 `tool_result` 整块丢弃——把 id 当成函数名写出去，上游会认为它对应一个没声明过的函数，换来的是整条请求 400。
- `args` 与 `response` 必须是 JSON 对象（protobuf `Struct`）：非对象的工具参数（`null`、字符串、数组）整段省略，不让一条参数把整条请求换成 400。
- `tool_choice` 没有「只准调某一个函数」这一档，用 `ANY` 加 `allowedFunctionNames` 表达，这是最接近的写法；上游不认白名单时会退化成 `ANY` 的语义（任何已声明函数都可能被调），比丢掉限制更接近客户端的本意。
- `ParallelToolCalls`、`tool_result.is_error` 在 gemini 里没有落点，丢弃；`stream` 为真的请求与非流式同一份报文（gemini 的流式由路径表示，见上）。
- 入站方向反着搬：`contents[].parts[].text` / `inlineData` / `fileData` / `functionCall` / `functionResponse` 五类 part 折成中间表示的文本、图片、工具调用与工具结果块，`systemInstruction` 折成 `Request.System`，`tools[].functionDeclarations[]` 折成工具定义，`toolConfig` 与 `generationConfig` 折回 `ToolChoice` 与采样参数。gemini 的工具调用与结果都只有函数名、没有 id，而入站转给 anthropic 与 chat 时那一头要求 `tool_result.tool_use_id` / `tool_call_id` 引用同一轮的 `tool_use` id：因此解码按整段对话给 `functionCall` 补合成 id（`gemini_call_<n>`，跨消息连续编号），再按函数名维护一个待对回队列，`functionResponse` 依次取队列里的 id。同名函数被并行调用两次时两个结果因此各自对上自己那次调用（对拍用例 `gemini_parallel_tool_calls_request_to_anthropic` 钉住这一条）；队列在出现新一轮 `functionCall` 时清空，历史里没有对应调用的结果退回函数名，那种请求上游可能拒收，但比丢掉整条结果好。

#### 响应映射（gemini → 中间表示）

| gemini 响应 | 中间表示 |
| --- | --- |
| `candidates[].content.parts[].text` | `text` 块 |
| `parts[].functionCall` | `tool_use` 块，`input` 取 `args` |
| `parts[].inlineData` / `parts[].fileData` | `image` 块（data URI / 地址） |
| `finishReason=STOP` | `end_turn` |
| `finishReason=MAX_TOKENS` | `max_tokens` |
| `finishReason` 为 `STOP` 且响应里有 `functionCall` | `tool_use` |
| `finishReason=SAFETY` / `RECITATION` / `BLOCKLIST` / 其它 | `end_turn`，原值进 `StopNote`（见下） |
| `usageMetadata.promptTokenCount` / `candidatesTokenCount` / `cachedContentTokenCount` | `Usage.In` / `Out` / `CacheRead` |
| `responseId` / `modelVersion` | `Response.ID` / 缺 `requestedModel` 时的 `Model` |

- 只取 `candidates[0]`，第一版不做多候选。`candidates` 为空但给了 `promptFeedback.blockReason` 时报错并带上这个原因：提示词被安全策略拦下与「响应形状不对」要分开说，用户看到 `SAFETY` 才知道该改的是提示词而不是请求格式。
- gemini 没有「因为工具调用而停」这一档：有 `functionCall` 时 `finishReason` 仍是 `STOP`。所以响应里出现 `functionCall` 就按 `tool_use` 编出去，客户端才会走工具调用分支（chat 是 `finish_reason=tool_calls`，anthropic 是 `stop_reason=tool_use`）；`SAFETY` 一类仍按 `end_turn`，不冒充 `tool_use`。
- 非正常 `finishReason` 统一按 `end_turn` 处理，不给客户端错误事件：中间表示只有四种结束原因，编一个 `content_filter` 出来另外三条协议都不认，客户端会因为一个自己编的类型走错分支；上游其实已经把这一轮说完了（常常还带着一段「我不能回答」的正文），把它报成错误会与 `requests` 里的 `ok=1` 自相矛盾。原值放进中间表示的 `StopNote`，由网关写一行日志当备注（非流式在解码之后，流式在 `stop` 增量上），不进客户端响应、不进 `requests` 的 `error` 列。
- 工具调用没有 id，而 anthropic 的 `tool_use` 与 chat 的 `tool_calls` 都必须有：解码时按出现顺序补一个合成 id `gemini_call_<n>`（同一份响应多次解码得到同一个值）。它只活在我们发给客户端的响应里，下一轮客户端回填时反正会被丢掉。
- `cachedContentTokenCount` 是 `promptTokenCount` 的子集（gemini 的 prompt 计数里已经包含命中缓存的部分），与 chat 的 `prompt_tokens_details.cached_tokens` 同一套记法，因此直接落到 `Usage.CacheRead`，`CacheWrite` 恒为 0。
- 编回 gemini 客户端时按同一张表反着写：文本、图片、`tool_use` 三类块进 `parts`，`finishReason` 由中间表示的结束原因给出（`max_tokens` 是 `MAX_TOKENS`，其余一律 `STOP`——gemini 没有别的正常取值），用量写成 `usageMetadata`（`totalTokenCount` 是输入加输出）。思考块与工具结果块在 gemini 的响应里没有落点，丢弃。
- 思考内容（part 上的 `thought` 标记与 `thoughtSignature`）不搬：思考内容在 5.4 里本来就是只往响应方向搬，gemini 这一档还没有落点。请求侧的 `thinkingConfig` 是另一件事，它按 5.4 的思考配置参数搬（`includeThoughts` 不在映射表里，不搬）。

#### 流式

上游是 gemini（`NewStreamDecoder` 认它）：

| gemini 流式帧 | 增量 |
| --- | --- |
| `data: {...}`（没有事件名） | 一帧一个 `GenerateContentResponse`，逐帧解码 |
| `parts[].text` | `text` |
| `parts[].functionCall` | 同一帧里给出 `tool_start` 与一整段 `tool_args`，`id` 补 `gemini_call_<n>` |
| `finishReason` | `stop`（带 `usageMetadata` 的用量） |
| 只有 `usageMetadata` 的帧 | 收尾补一个只带用量的 `stop` |
| `promptFeedback.blockReason` | `error` |
| `{"error":{…}}` | `error`，带上游原话 |

下游是 gemini 客户端（`NewStreamEncoder` 认它）：

| 增量 | gemini 帧 |
| --- | --- |
| 流开始 | 一帧只带 `role=model` 的候选（gemini 没有「消息开始」事件，先给一帧让客户端立刻拿到东西）；`parts` 为空时整个字段不写（`[]` 与字段缺失的读法不一样，空数组会让严一些的客户端以为这是一段空内容） |
| `text` | 一帧 `parts[].text` |
| `thinking` | 丢弃（gemini 的报文里没有落点） |
| `tool_start` / `tool_args` | 攒起来，收尾时一次编成 `parts[].functionCall`；空参数照发不带 `args` 的调用，断在中间的参数整块丢弃 |
| `stop` | 先发攒下的工具帧（若有）；带 `finishReason` 与 `usageMetadata` 的结束帧留到流结束（`Finish`），用量晚到也带得上；没有内容的收尾帧同样不写 `parts` |
| `error` | 一帧 `{"error":{"code":500,"message":…,"status":"INTERNAL"}}` |

- gemini 的流式在两个方向上都不是真增量：它当上游时 `functionCall` 一帧给全参数（不像 chat 那样逐帧给片段），它当客户端时工具调用要攒到收尾才发。代价是 gemini 客户端在最后一帧才看到 `functionCall`，好处是不伪造「半截参数」这种 gemini 表达不了的形状；文本仍然逐帧下发，不受影响。断流时攒下来的参数如果只到一半（不是合法 JSON 对象），这一次调用整块丢掉、只发错误帧：带一个空 `args` 发出去等于让客户端拿空参数调一次函数，比少一次调用更糟。
- 流式里不搬图片（`inlineData` / `fileData`）：增量体系只有文本、思考、工具三样，图片按块出现，没有对应的流式事件。
- 收尾帧的判定与另外三条协议同一条规则：`candidates[].finishReason` 非空就是收尾帧，只有 `promptFeedback.blockReason` 的帧不算（那是上游拒了提示词，这一次记失败）。尾帧没到时按入站协议补一个 gemini 形状的错误帧，再补一帧带 `finishReason` 与已读到的用量的候选（见 5.4）。

#### 错误体

- 上游错误体由 `internal/gateway` 的 `errorMessageFromBody` 解包：它认 `error.message`（并最多解开三层套在 `message` 里的 JSON），gemini 的 `{"error":{"code":…,"message":…,"status":…}}` 因此直接取出上游原话。`code` 与 HTTP 状态码重复、`status`（`INVALID_ARGUMENT` 一类）没有各自的落点，两者都丢弃；这条规则四个出站协议共用，不为 gemini 单开一份。
- 下发给 gemini 客户端的错误是 google 的形状：`{"error":{"code":<HTTP 状态码>,"message":…,"status":…}}`（`gateway.writeInboundError`），流内错误帧同形且没有事件名。`code` 与 `status` 由本次 HTTP 状态码算出，不搬上游正文里的同名字段——上游的 `code`、`status` 只用来解出那句话，解完就丢（见上一条）。

### 5.10 厂商附加请求头

有些上游除了标准的凭据头之外还要求别的头才能路由：opencode 的 Go 端点要求 `x-opencode-session: <每个对话一个稳定值>`（官方文档的原话是「send a stable session ID … so we can optimize routing」，缺了直接 400），Azure 一类要 `api-version`，还有厂商要求自定义的鉴权头。这些头的名字与值由厂商约定，不是我们要翻译的协议差异，因此不进 `setAuth`，也不为它们写代码分支：厂商行上存「附加头」的声明，转发时按声明加上去。

形状：迁移 `0010` 的 `provider_headers(provider, protocol, name, value)`，`protocol` 为空串表示整个厂商的每条路由都加，非空表示只加给这条协议（同一名字按协议覆盖厂商级）。`name` 按 HTTP 头名的规矩校验（`token` 字符，大小写不敏感，落库时归一成小写），`value` 不能是空白串、也不能带 CR/LF/NUL，`protocol` 必须在 `store.IsWireProtocol` 认的四种里（空串除外）。写入时在同一个事务里确认厂商行还在，不在就报 `ErrProviderNotFound`：外键虽然也拦得住，但报数据库的错误说不清下一步该干什么。厂商行删掉时它的头跟着删（`ON DELETE CASCADE`，连接上 `foreign_keys` 开着），不会留孤儿行。读侧两个入口：`store.ProviderHeaders` 出这家厂商的全部声明，`store.EffectiveProviderHeaders(厂商, 协议)` 出「厂商级 + 这条协议」合并后的有效集合（协议级按同名覆盖厂商级，按头名排序），转发只读后者。

头名在库里只有小写一种写法，因此覆盖、删除与「这个头有没有声明」都按头名比，写 `X-Opencode-Session` 与写 `x-opencode-session` 是同一行。`ProviderHeader` 的 `String` 只出「头名=前四位加省略号」，`slog.Any`、`%v` 与整片 `%v` 都走它，所以把一行或一片附加头交给日志不会带出值；要看原值直接读 `Value` 字段。

值的四种写法（转发前展开，展开后为空则该头不发，并在网关日志里记一行）：占位符按整值匹配，值就是下面四种之一时才展开；掺了别的文字（例如 `Bearer {env:TOKEN}`）一律当字面量原样发出去，不做局部替换。

| 写法 | 展开成 | 用途 |
| --- | --- | --- |
| 字面量 | 原样 | `api-version: 2024-10-21` 这类固定值 |
| `{env:NAME}` | 进程环境变量的值；变量没设置、或设置了但是空串时这条头不发，日志写明是哪个变量 | 会话 token、自定义鉴权头这类不该落库的东西 |
| `{session}` | 本次请求的会话键（见 5.3：`x-lax-session` 或 `metadata.user_id`），与选号粘性取同一个值；都没有时每次请求现生成一个 UUID | opencode 的 `x-opencode-session`：同一对话一个稳定值 |
| `{model}` | 本次请求的模型名（裸名，不转义）；模型列表请求没有模型名，这条头在那里不发 | 少数按模型路由的网关 |

- 注入范围是所有发往上游的请求：转发、`model sync` 的模型列表请求、`account test` 的两段（列表与最小调用）、`model probe` 的单模型请求。顺序固定为「先 `setAuth` 设默认凭据头，再叠声明」。`/v1/models` 这类入站只读接口不带附加头。
- 展开后为空串时不发这个头，也不发一个空值头；展开后含回车、换行或 NUL 时同样不发（环境变量的值可能带折行，原样拼进请求头等于拼出第二条头）。
- 读声明失败（读库失败）时转发报错，不静默跳过：少一条附加头上游就回 400，静默跳过等于把一次配置故障变成一句看不懂的协议错误。文案提示先跑 `lax doctor`。
- 只影响上游请求，不影响入站响应与 `/v1/models` 这类只读接口；也不进 `requests` 记录（头里可能是密钥，落库等于把它们写进请求记录）。日志只在不发送时记一行（头名与没发的原因），正常发送不记：每条转发都打一行会让日志被正常路径淹掉，而那正是排查时最不需要的东西。任何情况下都不打印值。
- 与协议凭据头冲突时按「声明优先」：显式写的头覆盖 `setAuth` 给的默认值（比如某些网关要 `Authorization: Bearer` 换成自定义 scheme）。冲突在写入时不管，只由转发时的「先设默认、再套声明」顺序决定，避免两层都做判断。
- 命名：`provider header set <名称> <头名> <值> [--protocol <协议>]`、`provider header unset <名称> <头名> [--protocol <协议>]`、`provider header ls [<名称>]`（`--json` 输出；`set` 是覆盖语义，不存在时新建）。

不做插件运行时：`magpie-community/plugins` 那套（一个文件、不许 import、跑在网关的 `onRequest`/`onResponse` 钩子上）解决的是「任意改写」，代价是一个 JS 引擎依赖与一段不受控的执行面，而它目录里的主体是订阅登录与逆向接口，正是第 9 节的非目标。已知需要注入的头全部能用上表四种写法表达；确实出现「按条件改请求体字段」的需求时再引入钩子，那时先在本节写清钩子的输入输出，再谈运行时。

### 5.11 客户端头透传

上游请求头不全量照抄：照抄会把客户端自己的凭据送出去，也会把 `Host`、`Content-Length` 这类逐跳头拼坏。但一个头都不给也不行：客户端靠头开关上游特性，`anthropic-beta` 决定 1M 上下文、extended thinking 与 prompt caching 能不能用，`anthropic-version` 决定走哪一版报文，OpenAI 的两条协议用 `openai-organization` 与 `openai-project` 决定算到哪个组织。这些头全部丢掉时，客户端看到的是「特性没生效」而没有任何错误。

规则是白名单 + 只在同协议时透传：

| 入站 = 出站 | 透传的头 |
| --- | --- |
| anthropic | `anthropic-beta`、`anthropic-version` |
| openai-chat、openai-responses | `openai-beta`、`openai-organization`、`openai-project` |
| gemini | 无：gemini 的特性开关在查询串与请求体里，不在头里 |

- 同一头名有多个值时按出现顺序全部带上（`anthropic-beta` 重复出现表示同时开多个 beta）；客户端没给或给的是空值时一个都不设，`anthropic-version` 因此保持 `setAuth` 写的默认值。
- 入站协议与有效出站协议不同时一个都不透传：`anthropic-beta` 给 chat 上游只会换回 400，这些头只在同一协议内有意义。
- 永远不透传：凭据类（`Authorization`、`x-api-key`、`x-goog-api-key`）与 `Cookie`；逐跳头（`Host`、`Connection`、`Content-Length`、`Transfer-Encoding`、`Expect`、`Upgrade`）；本网关自己的 `x-lax-session`（它只用于会话粘性，见 5.3）。白名单本身已经把这张名单挡在外面，名单仍单列一份当第二道闸：以后往白名单里加名字时，误加同名头不会漏出去。
- 只作用于转发路径（`buildRequest`）：`model sync`、`account test` 与 `model probe` 是网关自己发起的，没有客户端请求可依，行为不变。
- 优先级从低到高：`setAuth` 的默认值 → 透传的客户端头 → 厂商附加头声明（5.10）。声明是站点配置，要能压住客户端带来的值。同名时只由顺序决定：客户端这层先删同名头再按顺序追加，`anthropic-version` 给了就替换掉默认值而不是与它并存；附加头那层用覆盖写入，连客户端带来的多个值一起压掉。
- 单个头值超过 8KB 时丢弃整个头并记一行日志（头名与长度，不记值）：异常客户端把一个头写爆不应该变成上游的 431。
- 透传的头不进 `requests` 记录：它们对排查没有价值，落库只会把客户端带的东西搬进本机数据库。

### 5.12 模型别名

客户端的模型名往往写死（`claude-3-5-sonnet`、`gpt-4o`），而账号声明的是上游的真实名字（`glm-4.6`、`gemini-3.8-flash`）。别名表让客户端继续用自己认识的名字，不必改客户端、也不必给账号编假模型名。

- 存储：迁移 `0013` 的 `model_aliases(alias TEXT PRIMARY KEY, target TEXT NOT NULL, created_at INTEGER NOT NULL)`，全局一张表，不按账号分。按账号分会让同一个别名在不同厂商下指向不同模型，客户端拿到的名字就不再稳定；要按厂商区分就写两个别名（`fast-openai`、`fast-glm`）。
- 匹配：精确匹配，区分大小写，不做通配、不做前缀归一。别名可以指向另一个别名，解析沿链走，最长 8 跳：8 跳是「别名 → … → 真实名字」的边数，走满 8 跳落到真实名字算解析成功，第 9 跳落到仍是别名的名字上报错。成环与超长是两种错误，都带上是哪几条串在一起；解析逻辑只有一份，调用方只问一次，不自己走链。解析结果就是「真实模型名」。
- 写入校验：别名与目标都去首尾空白后不能为空、不能含控制字符（它们会原样进请求体与路径，也能进日志与表格），落库的就是去空白之后的值；别名不能等于目标（含去空白之后相等）；别名不能与任何真实模型名同名，否则会遮蔽真名，客户端请求那个名字时行为会随账号增减而变。真名按两处判，都是区分大小写的精确匹配：`provider_models` 里存在同名模型——目录是真名的事实来源（见 5.13），目录里有、眼下没有账号能用的名字也算，账号一恢复可用别名的行为就会突然改变，文案因此写出是哪几家厂商的目录（最多三个）；以及某个账号 `account_models.access='allowed'` 的同名行，也就是眼下真的能用。两处都命中时合成一句话报出（厂商与账号各列最多三个），不报两次；判定与落库在同一个事务里，中间插不进一条新账号或一条新目录行。目标可以暂时没有任何账号提供，之后加账号即可生效。
- 请求处理：拿到入站模型名（gemini 的是从路径取的那个）后先解析别名一次，此后的选号、有效协议、路由占位符、请求体里的 `model`、路径里的模型名全部用解析后的真实名字；响应方向不解析。`requests` 两列正好分工：`model_requested` 记客户端给的名字（别名），`model_used` 记解析后的真实名字；`log tail` 两列不同时能看出一次别名命中。
- 解析失败：链成环或超过 8 跳时这个名字解不出真实名字，换哪个账号都发不出去，按入站协议报 400（`writeInboundError`，`error.type` 是 `invalid_request_error`），消息带 store 给的原文（整条链）并给出下一步 `lax model alias ls`；这类请求不写 `requests`。它不是 404：404 说的是「清单里没有这个模型」，400 说的是这个名字本身解不开。
- 404 文案：别名解析出的目标没有账号提供时，消息写明「别名 X 指向 Y，但没有账号提供 Y；用 `lax model ls` 看现有清单」，不要只说「没有账号提供 X」，否则用户会去清单里找一个永远不存在的名字。错误类型与非别名场景一样按入站协议取（anthropic 是 `not_found_error`，两条 openai 是 `invalid_request_error`）。
- 响应里的模型名不改：别名只影响请求方向。直通路径现在把上游响应按字节转发（有用例逐字节断言，流式尤其），为一个字段重编码整帧的代价与风险都高于收益；响应里的 `model` 因此是上游的真实名字。跨协议转换时同理，转换后的响应里也写真实名字。
- `/v1/models` 与 `/v1/models/{id}`：清单里同时列出真实名字与别名（具体规则见 5.2.1）。别名条目的 `id` 是别名，`owned_by` 取提供一个目标的厂商（多个时按现有规则取厂商名字典序第一个），`created` 仍是 0；两条都出现时客户端两个名字都能用。别名解析不到目标时不在清单里出现：清单反映「现在能接的名字」，解析不到的名字接不了。`/v1/models/{id}` 用别名查得到同一条目；查不到时按现有的 404 处理，文案在那句话后面接着说「这是一个别名，目标是 Y」，链坏了的别名则带 store 的原文与 `lax model alias ls`。
- 命令行：`model alias set <别名> <目标>`（覆盖语义）、`model alias unset <别名>`、`model alias ls`（`--json` 输出别名与目标）。三条都在参数层做上面的校验，退出码与非空校验沿用现有约定。

### 5.13 模型目录与账号可达性

模型的事实（上下文长度、最大输出、支持哪些协议、能力位、厂商级价格）属于上游，与哪个账号去访问无关；账号只该回答「我能不能用这个模型」以及在个别模型上「我这里的能力/价格与目录不同」。把这两件事混在一张表里，会出现「同一个模型在三个账号下有三份重复元数据」，改一次要改三处。

- `provider_models(provider, model)` 存模型事实：`source`（`sync` / `manual` / `probe`）、`context_window`、`max_output`、`protocols`（该模型支持的协议集合，逗号分隔）、`capabilities`（能力位，逗号分隔）、四个价格列（厂商级默认价）、`updated_at`。主键 `(provider, model)`。
- `protocols` 的取值口径：只存四条线协议全名（`anthropic`、`openai-chat`、`openai-responses`、`gemini`，即 `store.IsWireProtocol` 认的那些），按这个固定顺序排列、去重，**空串表示上游没声明**。来源只有 `model sync` 从清单条目里认出来的那几个端点名与方法名（见 5.7 的「元数据来源」），未识别的写法一律不计。这一列只用来给探活排候选的顺序（见 5.7），不改变「这条路由存不存在」：候选范围仍由厂商的 `provider_routes` 决定。
- `capabilities` 暂时一律留空，**空表示未知，不表示不支持**：上游的 `supported_parameters` 这类字段各家形状不同，还没有消费者，先不为了填满一列而临时定规则。同理，`context_window`、`max_output` 为空是「上游没给」，不是 0 也不是「没有限制」。
- `account_models(account_id, model)` 存账号侧的两件事：**可达性**（`access` 取 `allowed` / `forbidden` / `missing` / `error`，另有 `http_status`、`error`、`checked_at`）与**覆盖值**（`protocol`、`context_window`、`max_output`、四个价格列，NULL 表示继承目录）。主键 `(account_id, model)`。
- `accounts.models` 那一列删除：它等价于「该账号 `access='allowed'` 的行集合」，是派生数据。

取值优先级：

| 维度 | 顺序 |
| --- | --- |
| 能否进候选池 | `account_models.access='allowed'` |
| 有效协议 | `account_models.protocol` → `provider_models.protocols` 里按厂商默认顺序 → `providers.default_protocol` |
| 上下文长度、最大输出 | `account_models` 覆盖值 → `provider_models` |
| 价格 | `account_models` 覆盖值 → `provider_models` 厂商级默认 |

写入分工：

| 动作 | `provider_models` | `account_models` |
| --- | --- | --- |
| `model sync --account A` | 按 `(provider, model)` upsert，`source='sync'`，只更新上游给出的字段 | 清单里出现的名字标 `allowed`，但只给没有行的名字插；不出现的与已有的行一律不动 |
| `model probe --account A` | 目录里没有的模型补一行 `source='probe'` | 写 `access`、`http_status`、`error`、`checked_at` 与发现到的 `protocol`；`401` 不写可达性（那是账号的结论） |
| `account add --models` | 目录里没有的补一行 `source='manual'` | 该账号 `access='allowed'` |

- 目录是**同厂商所有账号的并集**：同一厂商不同账号能看到的模型可以不同（实测 bigmodel 的清单里有 `glm-4.6-air`，某个 key 无权访问），差异由 `account_models.access` 表达，不靠各自复制一份目录。
- `/v1/models` 只列 `access='allowed'` 的行（另见 5.12 的别名）：客户端不该看到自己用不了的名字。
- 元数据大部分只存不用：上下文长度、最大输出与能力位还没有消费者，等做「按能力过滤/超长提示」时再接，先不为了有消费者而临时定规则。`protocols` 已经有消费者——探活候选的顺序（见 5.7）与有效协议的定序（见下面 `EffectiveModels`）。价格是已有消费者的那一列：`store.Usage` 按「账号 + 模型」现算单价，取 `account_models` 覆盖值 → `provider_models` 厂商级标价（见 5.5）。

价格不落在账号上：`accounts` 的四个价格列已删除（迁移 `0016`），账号级价格是模型事实还没拆出账号时的过渡形态。同一家供应商的同一个模型只该有一份标价，账号只在套餐不同时覆盖——否则改一次价要改 N 个账号，`usage` 还会给同一件事算出 N 个单价。写入口两个：`store.SetProviderModelPrices`（厂商级标价，`(供应商, 模型)` 必须已在目录里）与 `store.SetAccountModelPrices`（账号覆盖，该账号必须有这个模型的可达性行）；清值走 `ClearProviderModelPrices` 与 `ClearAccountModelPrices`，字段名只有 `in`、`out`、`cache-read`、`cache-write` 四个，取值链与字段解析都只在 `internal/store/model_price.go` 一处。

目录的写入口只有 `store.UpsertProviderModels(厂商, 行)`，按 `(provider, model)` upsert，一批行在同一个事务里写：

- **来源不降级**：`manual` 压过 `sync` 与 `probe`，`sync` 压过 `probe`。传来来源的等级低于库里那一行时整行不写——不只是 `source` 那一列不写，长度、价格也不写，否则一次同步能把用户手填的元数据改回上游那份；等级够时 `source` 落成传来的值，同等级重复写等于刷新这一行。等级判定只有一份，在 `UpsertProviderModels` 的 SQL 里。
- 其余列只写非零/非空的那几个：传 nil 的长度与价格、空的协议与能力位都表示「这一项不知道」，保留库里已有的值。一条只带名字的清单因此不会把探活补上的元数据抹掉。
- `updated_at` 每次写都刷新成当前秒；迁移回填的行是 0，旧表没有时间可用。

`ReplaceModels(账号, 名字)` 是 `model sync` 的写入口，语义是「把这些名字标 `allowed`，其余不动」：

- 只做加法：不删行、**已有行的可达性一个字节不改**（探活说了 `forbidden` / `missing` 的行不会被一次清单翻回 `allowed`），`http_status`、`error`、`checked_at` 与 `protocol` 也不动。旧语义里的「上游清单里消失的行删除」随 `accounts.models` 一起没了——可达性只由探活结论改动，一次少给了几个名字不等于这些模型不可用。
- 目录里没有的名字补一行 `source='manual'`：这个入口拿到的只有名字，不提供长度、价格与协议集合，不该冒充 `sync` 那份元数据。**已有的目录行一个字段都不动，连 `source` 也不动**：把同步写进来的行改成 `manual` 会让它此后等级更高，下一次同步再也写不进去，上游给的上下文长度就永远停在第一次同步的值上（`providerModelInsertOnly` 只插新行）。
- 空清单等于「这次没有新名字」，不删任何行。账号不存在时读不到它的厂商，写不了目录，报错而不是只写可达性。
- `account add --models` 是同一个形状的一次性版本：名字落成 `source='manual'` 的目录行与该账号 `access='allowed'` 的可达性行，与账号本身一个事务。目录里已有的行同样一个字段都不动（含 `source`），理由同上。

`store.EffectiveModels(账号)` 是合并之后的读入口，一次算出每个模型行的可达性与有效元数据：

- 返回该账号的全部模型行，不按可达性过滤：调用方自己看 `Access`（`/v1/models` 只列 `allowed`）。
- 长度与价格按「账号覆盖 → 目录」取值，覆盖值是 NULL 就继承目录；两边都没有就是空，不拿 0 冒充。
- 有效协议在这一处定序：账号覆盖非空就用它；否则看目录里这个模型支持的协议集合，厂商的有效默认协议在集合里就用它，不在就按 `openai-chat` → `openai-responses` → `anthropic` → `gemini` 取第一个，再没有就取集合里字典序最小的那个；集合为空才回退到厂商的有效默认协议。集合不按库里的行序解释，同一份数据多跑几次结论一致。
- 单模型的 `store.EffectiveProtocol(账号, 模型)` 走同一条规则，转发与展示用它。

迁移 `0015`：`account_models` 的每一行按账号的 `provider` 搬进 `provider_models`（`source='probe'`，`protocol` 列落到 `protocols`，其余列留空、`updated_at` 为 0；同一厂商的多个账号各探到不同协议时取非空的那个，结果与行序无关），`status` 映射成 `access`（`usable`→`allowed`，`forbidden`→`forbidden`，`notfound`→`missing`，其余→`error`：`synced` 与 `auth` 在旧库里本来就不是选号依据）；`accounts.models` 里出现但 `account_models` 没有的名字补一行 `source='manual'` 与 `access='allowed'`，已有的行一个字段都不动；最后删掉 `accounts.models`。五步在同一个事务里，`json_valid` 挡住历史脏值——一条坏 JSON 不该让整次升级失败。

### 5.14 systemone 决策协议

TypeSafe 的 System One（Jev）不是对话：输入一个 `state` 加一组带名字的 `questions`，输出每个问题的答案与概率，不产文本。commandcode 的 Provider API 在 `/provider/v1/systemone` 上提供它，TypeSafe 本家是 `POST https://api.typesafe.ai/v1/systemone`。它算第五种线协议 `systemone`：入站 `POST /v1/systemone`，出站同一个路径，厂商路由的 flag 是 `--systemone`、默认协议取值是 `systemone`。

| 项 | 规则 |
| --- | --- |
| 入站路径 | `POST /v1/systemone`，只收 POST，其余方法 405 带 `Allow: POST`；模型名在请求体 `model` 里，不在路径上 |
| 协议互通 | 只与自己互通：入站是 systemone 且有效出站协议也是 systemone 才转发；有效出站协议是另外四种时 400，文案说明这个模型走的是哪条协议；反过来入站是那四条而入站模型的有效协议是 systemone 时同样 400。不做转换：对话消息与决策请求之间没有可定义的映射，5.4 的中间表示服务的是其余四条 |
| 请求体 | 直通，只把 `model` 换成解析别名后的真名，其余键（`state`、`questions`、`assets`）原样转发；请求体上限 8 MiB，超过给 413 |
| 最少校验 | `model` 非空且是字符串、`questions` 是非空对象；其余字段交给上游（32 MiB、64 层嵌套、有限数这些规则在上游，不在本机复制一份） |
| 流式 | 不支持。请求体带 `"stream": true` 时 400，文案说明这条协议没有流式、把请求体改回非流式：静默当同步会让等 SSE 的客户端挂到超时 |
| 响应 | 直通，不改 `model`（5.12 的响应不改名同样适用）；`usage.input_tokens` 与 `usage.output_tokens` 写进请求记录，没有缓存与思考字段，输入 token 按不含缓存处理（`store.UncachedInTokens` 的默认分支，见 5.5） |
| 错误体 | `{"detail":{"error_type":"…","message":"…"}}`，形状照 TypeSafe 实测：这条协议真实的客户端是 TypeSafe 的 SDK，它读 `detail`。字段名是 `error_type` 不是 `type`；`detail` 用对象而不是纯字符串，好在留下类型的同时不改 SDK 的读法。取值：`invalid_request_error`（400、413）、`authentication_error`（401）、`permission_error`（403）、`not_found_error`（404）、`rate_limit_error`（429）、`api_error`（5xx）。上游自己的 `422` 与校验错误正文（`detail` 是数组）原样透传，不重新包装 |
| 上游会把模型名解析成版本 | 请求 `model` 写 `jev-latest` 时，上游返回的 `model` 是 `jev-1.14.0`。响应体字节原样透传，我们不改写，也不把上游的这次解析写进 `model_used`：那一列记的是我们送去上游的名字 |
| 探活 | systemone 模型的探活不是 `max_tokens=1` 的对话请求，而是一条最小决策请求：`state` 为 `ping`、一个问题 `{"ok":{"type":"noul","instructions":"Is this text present?"}}`；2xx 即 `allowed`，其余判定与 5.7 相同。一种模型的有效出站协议是 systemone 时，探活只能走这条路——对它发对话请求只会拿到 404 |
| 同步 | `supported_endpoints` 里的 `/systemone` 折成 `systemone`（映射表见 5.7）；但 commandcode 的清单不列决策模型，`typesafe/jev` 一类的名字只能手工声明（`account add --models`） |
| 别名与选号 | 与其余四条一致：别名解析在请求入口做一次（见 5.12），选号只读 `access='allowed'`（见 5.13） |
| CLI | `provider add/set --systemone <路径或整条地址>`、`--default-protocol systemone`、`lax decide`（把 `state` 与问题组发出去并打印答案与概率，见第 4 节）；`call` 不接 systemone：它发的是对话，决策请求没有提示词 |

第 7 节记已知未做：`assets`（图片）原样转发但不校验、压缩请求体（gzip 与 zstd）不支持（客户端自己解压后发）、`box` 答案原样返回不解释。

## 6 数据模型

```sql
CREATE TABLE accounts (
  id             INTEGER PRIMARY KEY,
  name           TEXT    NOT NULL UNIQUE,
  provider       TEXT    NOT NULL,           -- 只存名字：地址、默认协议、路由都在 providers 上
  key_cipher     BLOB    NOT NULL,
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
  in_tokens_uncached INTEGER NOT NULL DEFAULT 0,
  out_tokens      INTEGER NOT NULL DEFAULT 0,
  cache_read      INTEGER NOT NULL DEFAULT 0,
  cache_write     INTEGER NOT NULL DEFAULT 0,
  reasoning_tokens       INTEGER NOT NULL DEFAULT 0,
  reasoning_tokens_extra INTEGER NOT NULL DEFAULT 0,
  latency_ms      INTEGER NOT NULL DEFAULT 0,
  error           TEXT
);

CREATE INDEX requests_ts ON requests(ts DESC);

CREATE TABLE provider_models (
  provider        TEXT    NOT NULL,
  model           TEXT    NOT NULL,
  source          TEXT    NOT NULL,           -- sync / manual / probe
  context_window  INTEGER,
  max_output      INTEGER,
  protocols       TEXT    NOT NULL DEFAULT '', -- 该模型支持的协议，逗号分隔
  capabilities    TEXT    NOT NULL DEFAULT '',
  price_in        REAL,
  price_out       REAL,
  price_cache_read  REAL,
  price_cache_write REAL,
  updated_at      INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (provider, model)
);

CREATE TABLE account_models (
  account_id      INTEGER NOT NULL,
  model           TEXT    NOT NULL,
  access          TEXT    NOT NULL,           -- allowed / forbidden / missing / error
  http_status     INTEGER NOT NULL DEFAULT 0,
  error           TEXT    NOT NULL DEFAULT '',
  checked_at      INTEGER NOT NULL DEFAULT 0,
  protocol        TEXT    NOT NULL DEFAULT '', -- 账号级的协议覆盖（探活发现）
  context_window  INTEGER,                     -- NULL 表示继承目录
  max_output      INTEGER,
  price_in        REAL,
  price_out       REAL,
  price_cache_read  REAL,
  price_cache_write REAL,
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
  path        TEXT NOT NULL,                -- 相对 base_url 的路径，或整条地址（含 ://），可含 {model} 占位符
  models_path TEXT NOT NULL DEFAULT '',     -- 同上，空串表示回退到同厂商其它协议
  PRIMARY KEY (provider, protocol)
);

CREATE TABLE provider_headers (
  provider TEXT NOT NULL REFERENCES providers(name) ON DELETE CASCADE,  -- 删厂商时它的头一起走
  protocol TEXT NOT NULL,                   -- 空串表示整个厂商，非空只加给这条协议
  name     TEXT NOT NULL,                   -- HTTP 头名，落库归一成小写
  value    TEXT NOT NULL,                   -- 字面量，或 {env:…}、{session}、{model} 占位符（见 5.10）
  PRIMARY KEY (provider, protocol, name)
);
CREATE TABLE model_aliases (
  alias          TEXT    PRIMARY KEY,
  target         TEXT    NOT NULL,
  created_at     INTEGER NOT NULL
);
```

`account_models` 由迁移 `0003` 建立，迁移 `0015` 把它改成上表的形状：`status` 那一列换成了 `access`（取值 `allowed`、`forbidden`、`missing`、`error`，见 5.13），并加上账号侧的覆盖值列。`accounts.fail_streak` 由迁移 `0004` 加入，只用于冷却翻倍，写入口只有 `RecordFailure`（加一）、`ClearFailure`（清零）与 `RotateKey`（换凭据时清零，见 5.6）。删除账号时 `DeleteAccount` 在同一个事务里删掉它的 `account_models` 行：SQLite 的 rowid 会被复用，遗留子行会让新账号继承旧账号的探活状态。目录 `provider_models` 是按厂商共享的，删账号不动它：另一个账号可能还在用同一批模型事实。

三个写入口各管一行的一部分，谁都不顺手改别人的列：`SetAccountModelAccess` 写 `access`、`http_status`、`error`、`checked_at`（探活结论，时间是调用方给的）；`SetAccountModelProtocol` 只写 `protocol`；`SetAccountModelOverride` 整体写六个覆盖值列，nil 就是 NULL（继承目录），不是「不动」。后两个在模型行还不存在时先落一行 `allowed` 占位：能测出协议、能给这个模型写覆盖值，就说明这个账号要用它。`ReplaceModels` 只把名字标成 `allowed`，不碰这三组列（见 5.13）。

旧的状态词表（`synced`、`usable`、`forbidden`、`notfound`、`auth`、`error`）库里已经不再存，只在 `store` 里留了一层过渡视图：`store.ModelUsable` 一类的常量、`AccountModel.Status` 与写入口 `SetModelStatus` 还在，读出来的 `Status` 由 `access` 与 `checked_at` 反推（`allowed` 且探过是 `usable`、没探过是 `synced`），写进去的旧词表由 `accessFromStatus` 映射回来。`auth` 与新词表没有对应，两个方向都落到 `error`。这层只为尚未改完的 `internal/gateway` 与 `internal/cli` 保留，两边改完就删掉。

`accounts.base_url` 与 `accounts.protocol` 由迁移 `0014` 删掉（见 5.8）：地址、默认协议与路由都只留在厂商行上。迁移先给「`provider` 不在 `providers` 表里」的账号补一行厂商——名字取账号的 `provider`，`base_url` 取该账号的地址（同名账号有多个地址时取最小的那个，结果与行序、与重跑都无关），`note` 写「由迁移 0014 从账号地址补建」，`default_protocol` 取账号的 `protocol` 但只认四条线协议（别的值留空，回到固定顺序回退）；已有厂商一律 `INSERT OR IGNORE` 跳过，账号上的历史协议不会覆盖 `0007` 种好的默认协议。删列用的是 `ALTER TABLE … DROP COLUMN`：本仓库依赖的 `modernc.org/sqlite` 内置 SQLite 3.54.1 支持它（自 3.35 起），因此不走「建新表 + 搬数据 + 重建索引 + 改引用」那一套；两列不在任何索引、外键、视图与触发器里，索引不必重建。迁移之后 `Account.BaseURL` 与 `Account.Protocol` 是只读派生值：读账号的 `ListAccounts`、`GetAccount`、`ListAccountsByProvider` 共用 `queryAccounts`，写入的 `CreateAccount` 不再碰这两列。

`accounts.price_cache_read`、`accounts.price_cache_write` 与 `requests.in_tokens_uncached` 由迁移 `0011` 加入。两列缓存单价可空，空表示没填：估算花费按输入价回退，老账号升上来两列都是 NULL，金额与只有输入输出价时一致。`in_tokens_uncached` 是入账时按出站协议归一化之后的输入 token（纯规则 `store.UncachedInTokens`，见 5.5），`in_tokens` 保持上游原值；历史行没有区分能力，迁移按当时的 `in_tokens` 回填，不写 0——写 0 会让历史花费凭空变成只算输出。花费的公式只有一份，在 `store.Usage` 的 SQL 里：`in_tokens_uncached × price_in + cache_read × COALESCE(price_cache_read, price_in, 0) + cache_write × COALESCE(price_cache_write, price_in, 0) + out_tokens × price_out + reasoning_tokens_extra × price_out`，五项之和再除以 1e6。

`requests.reasoning_tokens` 与 `requests.reasoning_tokens_extra` 由迁移 `0012` 加入（见 5.5）。前者是上游上报的思考 token 原值，后者是其中被判成「输出之外的一项」的那部分，只给后者按输出价再计一次；判定只有一处，纯函数 `store.ReasoningExtra`。两列都没给历史行回填：当时上游报了什么已经无从复原，留默认值 0 让升级前后的金额完全一致。anthropic 没有独立的思考字段，它的行这两列都是 0，`out_tokens` 本来就含思考。

`account_models.protocol` 由迁移 `0005` 加入，记录「这个模型该用哪种协议」，取值是 anthropic、openai-chat、openai-responses 与 gemini（见 5.9）。**空字符串表示没有账号级覆盖**：有效出站协议按「账号覆盖 → 目录里这个模型支持的协议 → 厂商的有效默认协议」算（5.13 给了定序），纯规则在 `store.resolveModelProtocol`（`store.ResolveProtocol(providerProtocol, modelProtocol)` 是它最后两步的旧写法），读库入口是 `store.EffectiveProtocol(accountID, model)`——账号行不再存协议，所以第二个参数就是模型名；行不存在、列为空、厂商行不存在或这家一条路由都没声明时都回退到空串，不报错。转发与展示只读这个有效协议，不在调用方拼规则；`cli` 要显示协议时也只调它。旧的三参数写法 `EffectiveProtocol(accountID, accountProtocol, model)` 还留着作兼容包装（第二个参数是已经无用的账号协议、被忽略），只为 `internal/gateway` 与 `internal/cli` 里尚未改完的调用点继续能编译，两边走的是同一条规则。

这个列只有一个写入口 `SetAccountModelProtocol`（旧名 `SetModelProtocol` 是兼容包装），由探活发现协议后调用（见 5.7）。`SetAccountModelAccess` 只改可达性与探活那几列，不动 `protocol` 与覆盖值；`ReplaceModels` 也不动它，同步不会覆盖已发现的协议。旧库升上来时整列默认空串，也就是全部由目录与厂商默认协议算。

`providers` 与 `provider_routes` 由迁移 `0006` 建立并种入已知厂商，`providers.default_protocol` 由迁移 `0007` 加入并显式种入升级前的默认协议，`providers.models_path` 由迁移 `0008` 加入（老行一律空串，表示不覆盖），行里没有凭据，只有地址与路径。`providers` 一行只提供 `base_url`、备注、默认协议与模型列表覆盖，协议细节全在 `provider_routes` 的 `(厂商, 协议)` 上；两者之间没有外键，所以删厂商行时必须连带删它的路由行，`DeleteProvider` 在一个事务里一起做（`provider rm --force` 走 `DeleteProviderForce`，同一个事务里还要停用指向它的账号，见 5.8）。`providers` 表为空不算错误：种子行可以被用户删光，此时所有账号回退到后缀猜测。账号行不存地址，`Account.BaseURL` 与 `Account.Protocol` 都是读的时候从厂商行派生出来的只读值（见 5.8），改厂商表立刻影响所有引用它的账号。写入口是 `CreateProvider`（`provider add`，名称已存在时报 `ErrProviderExists`，不隐式更新）与 `SetProvider`（`provider set` 与 `provider unset` 的共同入口：patch 里为 nil 的字段不动，`Routes` 里传空串表示删掉那条路由——`provider unset <名称> <协议>` 就是这么调的；命令面上空值仍是用法错误）：两者都在一个事务里写厂商行与路由，`CreateProvider` 至少要一条路由，`SetProvider` 删到一条不剩时报错，另有一个 `AddProvider`（upsert，允许零路由），它是第二条 upsert 入口：`cli` 不走它，生产代码里也没有其它调用者，现在只有 `store` 自己的用例在用；新建与改仍分别走 `CreateProvider`、`SetProvider`。读路由只有 `LookupProviderRoute` 一个入口，转发、模型同步与探活都经过它。

`provider_routes.models_path` 是写入侧的默认值，不是读取侧的唯一来源：`CreateProvider`/`SetProvider` 写一条没有模型列表地址的路由时按 `DeriveModelsPath` 补（只给 openai-chat 补，尾部 `/chat/completions` 换成 `/models`），其它协议留空表示「这条协议没有自己的列表接口」。空串在读取时回退到同厂商其它协议的非空值，纯规则在 `store.ResolveModelsPath`：厂商级覆盖 → 账号协议那条路由 → 协议名排序的第一条非空值，返回路径时一并返回这条路径所属的协议，模型同步按它选凭据头。推导不放在读取侧，因为内置 `anthropic` 厂商只有 anthropic 一条路由，读时推导会让 anthropic 型账号彻底失去模型列表来源。

`providers.default_protocol` **空字符串表示这家没有显式默认协议**：有效默认协议按固定顺序 `openai-chat` → `openai-responses` → `anthropic` 回退到第一条已声明的路由（用户自建的厂商通常不填）。纯规则在 `store.ResolveDefaultProtocol`：显式值必须落在已声明的路由上才用它，指不到时同样按固定顺序回退——历史脏数据因此不会静默交给后缀猜测。读库入口是 `store.EffectiveProviderProtocol(provider)`：厂商不存在返回 `ErrProviderNotFound`，厂商在但一条路由都没有时返回空串与 `ErrProviderNoRoutes`，文案留给调用方。写入侧保证不产生悬空值：`CreateProvider` 的默认协议必须落在这次声明的路由上，`SetProvider` 改完之后必须仍指向一条存在的路由，否则整笔事务拒绝；`SetProvider` 收到空的 `DefaultProtocol` 表示清掉显式值、回到固定顺序，这一条只由 `provider unset <名称> default-protocol` 触发。旧库升上来时 8 家种子厂商由 `0007` 拿到显式默认协议，用户自建的厂商保持空串。

gemini 的两行由迁移 `0009` 种入，逐字段照下表：

| 表 | 键 | 值 |
| --- | --- | --- |
| `providers` | `google-gemini` | `base_url` = `https://generativelanguage.googleapis.com`，`default_protocol` = `gemini`，`models_path` 为空（不覆盖，按路由取） |
| `provider_routes` | `(google-gemini, gemini)` | `path` = `/v1beta/models/{model}:generateContent`，`models_path` = `/v1beta/models` |

gemini 路由的 `models_path` 必须显式给：写入侧的默认值只推导 openai-chat 一条（见 5.8），gemini 自己的列表路径没有推导规则。两条都用 `INSERT OR IGNORE`：升级时用户可能已经用 `provider add google-gemini` 自己建过这家（名字撞上、地址指向代理），那两行原样保留，迁移不覆盖用户的写法，也不因为主键冲突整笔失败。

`provider_routes.path` 里的 `{model}` 是出站模型名占位符（见 5.9）：规整只合并斜杠，花括号与冒号原样保留，替换发生在网关拼好地址之后。协议取值的白名单只有一份 `store.IsWireProtocol`（四条线协议），厂商路由、默认协议、账号的 `--protocol` 与探活候选都读它。

`model_aliases` 由迁移 `0013` 建立：`alias` 是主键，`target` 是真实模型名（或另一个别名），`created_at` 是 Unix 秒（覆盖改目标时不动它）。写入口只有两处：`SetModelAlias` 是覆盖语义（同一个别名再写一次就是改目标，UPSERT，不报错），校验全部在事务里做完再落库：别名与目标去空白后不能为空、不能含控制字符、别名不等于目标，别名也不能与任何账号可达的真实模型名同名（区分大小写，比的是该账号 `access='allowed'` 的行，也就是迁移 `0015` 之后 `accounts.models` 的等价物，见 5.13），否则报 `ErrModelAliasShadow` 并列出声明了那个名字的账号（最多三个）；`DeleteModelAlias` 删不到时按情况报 `ErrModelAliasNotFound`。读入口也只有两处：`ModelAliases` 按别名升序返回全表（SQLite 的 TEXT 默认二进制序，也就是区分大小写），`ResolveModelAlias` 把入站名字解析成真实名字——它先把全表一次读出来，再在内存里沿链走，不按跳查库，因此不会出现链走到一半表被改掉、前半段用旧目标后半段用新目标；名字不是别名时返回 `("", false, nil)`，成环报 `ErrModelAliasCycle`、超过 8 跳报 `ErrModelAliasTooDeep`，两种错误都把串起来的名字写进文案。别名与真实名字的冲突只在写入时判定，读取不做二次校验（见 5.12）。

`provider_headers` 由迁移 `0010` 建立（见 5.10），一行是一条 `(厂商, 协议, 头名)` 的附加请求头声明：`protocol` 空串表示整个厂商的每条路由都加，非空表示只加给这条协议。`provider` 外键指向 `providers(name)` 并带 `ON DELETE CASCADE`，`store.Open` 的连接串带 `_pragma=foreign_keys(1)`，所以删厂商行时它的头由 SQLite 一起删掉；`provider_routes` 建于 `0006`、没有外键，才要在 `DeleteProvider` 里手删。写入口只有两处：`SetProviderHeader` 是覆盖语义（同一个 `(厂商, 协议, 头名)` 再写一次就是改值，UPSERT，不报错），`DeleteProviderHeader` 删不到时按情况报 `ErrProviderHeaderNotFound`（厂商在、头不在）或 `ErrProviderNotFound`（厂商不在）。两个写入口都先校验再进事务，并在事务里确认厂商行还在：外键虽然会拦住指向不存在厂商的写入，但报数据库的外键错误说不清下一步该干什么。头名按 HTTP `token` 校验（大小写不敏感，落库归一成小写），值不能是空白串、也不能带 CR/LF/NUL（那样拼进请求头会把一条头拆成两条），`protocol` 只能是空串或 `IsWireProtocol` 认的四种。读有两个入口：`ProviderHeaders` 返回这家厂商的全部声明（按 `protocol`、`name` 升序，厂商级在最前），`EffectiveProviderHeaders(厂商, 协议)` 返回那次请求真正要加的那一份——先铺厂商级，再用这个协议自己的声明按同名覆盖，返回时也是按 `name` 升序。转发侧只读后者，不在调用方拼覆盖规则。附加头不进 `requests`，日志只在没发时记头名与原因，正常发送不记；`ProviderHeader.String`（值截断到前四位）只给 `provider header ls` 与错误文案用。

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
| M6 | 协议矩阵：三入站 × 三出站 + 同协议直通，中间表示与三个编解码器，流式增量（gemini 出站另见 M12） | 9 种组合的非流式与流式用例通过；对真实上游三种入站各打一条能拿到回复 |
| M7 | 模型探活跳过与记账：`model probe` 按 `--max-age` 跳过有定论且新鲜的结论、`--refresh` 强制重探，每次探活的上游调用写一行 `requests` 并带缓存读写 | `TestModelProbe*` 覆盖跳过、刷新、`--limit`、记账、零可用的跳过计数，`TestProbeModel*` 覆盖各状态判定、401 停用与探活请求行；`log tail` 能看到探活行，零可用时 `accounts.models` 不变 |
| M8 | 协议归属从账号降到模型：`account_models.protocol`（迁移 `0005`）覆盖账号的 `protocol`，探活顺手发现协议，选号与转发按有效协议算，同步不覆盖发现结果 | 同一账号下两个模型可以分别走 anthropic 与 openai-chat：各自打到正确的路径与凭据头，「入站 == 出站」按模型协议算；协议发现用例覆盖 `404`、`403`、`400 指向端点` 三种失败换协议、账号协议直接成功只花一次调用、`401` 不换协议并停用账号；`EffectiveProtocol` 在行不存在或列为空时回退账号；旧库升上来 `protocol` 默认为空串，`ReplaceModels` 不动已发现的协议 |
| M9 | 厂商与路由进库：`providers` 与 `provider_routes`（迁移 `0006`），出站地址按 `(厂商, 协议)` 的 `path` 拼，模型列表按 `models_path` 取，探活候选协议按厂商声明过的路由收紧；没有厂商行的账号保留后缀猜测 | 空库跑到最新版本后种子逐行对上，11 条路由拼出的完整地址与 `models_path` 全部正确；deepseek 账号下 anthropic 模型打到 `/anthropic/v1/messages` 并带 `x-api-key`，chat 模型打到 `/v1/chat/completions` 并带 `Bearer`；只有一条路由的厂商探活失败时上游只收到一次请求，deepseek 试两条；`models_path` 为空时回退到同厂商第一条非空路径，全空时报错文案带「用 --models 手填」；自定义 `base_url` 且 `provider` 不在表里时仍按后缀猜测；空库跑到最新版本与从 `0006` 升级两条路径下，8 家的 `default_protocol` 逐行对上，显式默认优先于固定顺序兜底，deepseek 与 commandcode 的默认协议仍是 `openai-chat`（单靠协议名字典序会给 `anthropic`） |
| M10 | 厂商命令面与模型列表路径：`provider add` 只新建、`provider set` 增量改（空值是用法错误，删除只在 `provider unset`）、默认协议按固定顺序 `openai-chat` → `openai-responses` → `anthropic` 兜底，`models_path` 的写入默认值与厂商级覆盖（迁移 `0008`），模型列表的凭据头跟着取到路径的那条路由走，`model sync --endpoint`，`base_url` 校验下沉到 store，探活读库失败报错而不放宽候选协议 | 空库与从 `0007` 升级两条路径都到最新版本：升级后 `provider_routes.models_path` 还在、路由数据一个字段不动、`providers.models_path` 全场空串；`provider add` 重名报 `ErrProviderExists` 并提示 `provider set`，一条路由都不给报错；`provider set` 只改传入的字段，协议 flag 传空串是用法错误、删除走 `provider unset`，删到一条不剩报错，默认协议悬空整笔拒绝（同一次里一起改则放行）；`/v1/chat/completions` → `/v1/models`、`/chat/completions` → `/models`、anthropic 路由留空，重写内置 `anthropic` 厂商的 `/v1/messages` 之后它的 `/v1/models` 仍在；deepseek 的 anthropic 账号同步模型列表打到 `/v1/models` 并带 `Bearer`（不是 `x-api-key`）；`--endpoint /v1/model` 压过厂商覆盖与路由，`v1/model`、`//v1//model/` 这类写法被规整成同一个地址、不报错；默认协议显式优先、缺省按固定顺序、指不到路由拒绝、悬空的历史值按固定顺序回退 |
| M11 | 端点两种写法的命令面与凭据轮换：`--anthropic`、`--openai`、`--responses`、`--models-path` 与 `model sync --endpoint` 一律收相对路径或整条地址，校验只有 `store.NormalizeEndpoint` 一份，cli 把它的错误包成用法错误；`account set --key`（`update` 同义）换凭据并在同一事务里清掉旧凭据的冷却与失败计数、重新启用账号；`serve` 增加两个只读的模型清单接口 `GET /v1/models` 与 `GET /v1/models/{id}`（见 5.2.1），清单取 `enabled=1` 账号的 `accounts.models`，按名字去重升序，`owned_by` 取厂商名 | `provider add` 用整条地址声明 openai 路由后，`provider ls --json` 的 `path` 就是整条地址，转发与 `model sync` 都打到那个 host、`base_url` 那台一次请求都不收；`https://`、`https:///x`、`ftp://host/x` 与空值在 add、set、`--models-path`、`--endpoint` 四处都退出码 2 且不落库；`model sync --endpoint <整条地址>` 打到该地址，下一轮回到路由的 `models_path`；`account set --key` 之后 `key_cipher` 变化、旧密文不再存在，`cooldown_until`、`fail_streak`、`last_error` 清零且 `enabled=1`，`accounts.models` 与 `account_models` 一个字段不动，新 key 能转发、`--json` 不含明文、`update` 与 `set` 输出一致；`account test` 对模型列表为空的账号退出码 0，用默认模型名探活且不 panic；`GET /v1/models` 对两个账号各自的模型去重后升序给出、同名模型取厂商名字典序第一个，停用账号的模型不出现、冷却中账号的模型出现，没有任何模型时是 200 加 `data:[]`；`GET /v1/models/{id}` 命中返回单个对象、未命中 404 且类型是 `not_found_error`；两个接口的非 GET 方法 405 并带 `Allow: GET`，不写 `requests` 行、不改账号状态，`/healthz` 与三条入站路径行为不变 |
| M12 | gemini 出站（第一步，非流式）：`translate.ProtocolGemini` 与 `gemini.go` 的请求/响应编解码、用量读取，路由 `path` 的 `{model}` 占位符（相对路径与整条地址都支持），凭据改走 `x-goog-api-key`，`store.IsWireProtocol` 成为唯一协议白名单（账号的 `--protocol` 接受 gemini） | chat 与 anthropic 入站各打一条到 gemini 账号：上游路径含模型名、头是 `x-goog-api-key`、请求体没有 `model` 字段、响应转回入站协议且 `usageMetadata` 落到用量；`{model}` 在相对路径与整条地址两种写法里都被替换且模型名按路径段转义，不含占位符的路由字节不变；gemini 出站的流式请求把上游端点从 `:generateContent` 改写成 `:streamGenerateContent?alt=sse`（见 5.9），客户端拿到流式响应与用量；对拍用例覆盖 system、图片、工具定义、工具调用与结果、`generationConfig`、`tool_choice`、`finishReason`、`usageMetadata` 的映射 |
| M13 | gemini 入站与出站流式：两条入站路径（`/v1beta/models/{model}:generateContent` 与 `:streamGenerateContent`，模型名与流式标志从路径注入），gemini 的响应编码与流式编码器，gemini 出站流式解码器（只有 `data:` 帧，末尾 `usageMetadata`），出站流式端点换成 `:streamGenerateContent?alt=sse`，入站直通不改请求体，非正常 `finishReason` 进 `StopNote` | gemini 入站的非流式与流式各打一条（上游分别是 chat 与 anthropic 假上游）：模型名从路径取、响应是 gemini 形状、用量从上游协议转成 `usageMetadata`；anthropic 客户端打 gemini 流式账号时上游 URI 是 `:streamGenerateContent?alt=sse`、头是 `x-goog-api-key`、客户端拿到 anthropic 事件与用量；gemini 出站流式的增量序列（文本逐帧、`functionCall` 一帧给全参数、`finishReason` 收尾）与反向（工具调用攒到收尾）由对拍用例钉住；`SAFETY` 一类映射成 `end_turn` 且原值只进日志 |
M0 到 M13 的代码已落地，端到端由 `internal/cli` 的 `TestWalkingSkeleton` 用假上游验证：初始化、加账号、`call`、`log tail`、`usage`、`doctor` 全链路走通。协议矩阵由 `internal/gateway/matrix_test.go` 与 `matrix_stream_test.go` 验证：三条入站 × 三条出站共 9 种组合的非流式与流式各跑一遍（出站 gemini 的三条另由下面 gemini 那批用例覆盖），同协议直通的「不改字节」「保留陌生字段」与跨协议的「丢弃陌生字段」单独有用例。思考内容的映射与它的代价由 `internal/translate` 的 `TestGolden` 按 `testdata/in` 与 `testdata/out` 对拍：四条协议两两之间的请求、响应与流式都有用例（gemini 那几对见本段末尾），含带签名的往返、无签名时丢弃与请求方向不回填（`anthropic_thinking_request_to_chat_dropped`、`responses_reasoning_request_to_chat_dropped` 等）。工具调用的五条规则同样各有用例：`chat_tool_results_to_anthropic_user_message` 与 `responses_tool_results_to_anthropic_user_message` 钉住工具结果折叠进 user 消息，`chat_tool_result_then_user_followup_to_anthropic_merged_user` 与 `responses_tool_result_then_user_followup_to_anthropic_merged_user` 钉住工具结果之后的追问并进同一条 user 消息，`anthropic_parallel_tools_stream_to_chat_dense_index` 钉住 chat 的稠密工具序号，`chat_tool_stream_to_responses_done_events` 与 `responses_output_item_done_stream_to_anthropic` 钉住 responses 的 done 事件与 output 回填、只发 done 的上游，`chat_text_stream_to_responses_message_output`、`chat_text_tool_text_stream_to_responses_output_order` 与 `anthropic_two_text_blocks_stream_to_responses_message_items` 钉住文本 item 的 added 与 done、`completed.response.output` 的正文与顺序，1MB 上限与断流时的空 `output` 由 `TestResponsesStreamEncoderTextCap` 与 `TestResponsesStreamEncoderErrorKeepsTextOutOfOutput` 钉住，`anthropic_builtin_tool_to_chat_dropped` 钉住内置工具丢弃，请求侧的思考配置参数与工具白名单由 `anthropic_thinking_budget_to_chat_reasoning_effort`、`chat_reasoning_effort_to_anthropic_thinking_budget`、`chat_reasoning_object_to_responses_reasoning`、`gemini_thinking_budget_to_anthropic_thinking`、`anthropic_thinking_budget_to_gemini_thinking_config`、`anthropic_thinking_disabled_to_chat_dropped`、`anthropic_thinking_disabled_to_responses_dropped`、`anthropic_thinking_disabled_to_gemini_zero_budget`、`chat_tool_strict_to_responses_kept`、`responses_tool_strict_to_chat_kept`、`anthropic_allowed_tools_to_responses_allowed_tools`、`responses_allowed_tools_to_anthropic_allowed_tools`、`anthropic_allowed_tools_to_chat_auto`、`anthropic_allowed_tools_to_gemini_allowed_function_names`、`gemini_allowed_function_names_to_anthropic_allowed_tools` 钉住（规则见 5.4 的两小节）；思考与工具参数交替时的块开关由 `chat_reasoning_between_tool_args_stream_to_anthropic_reopen_block` 与 `TestAnthropicStreamEncoderKeepsDeltasOnOpenBlocks` 验证，缺 `parameters` 的工具补空 schema 由 `responses_tool_without_parameters_to_anthropic_empty_input_schema` 验证；断流时工具参数不完整的文案与 `ok=0` 由 `internal/gateway` 的 `TestConversionStreamTruncatedToolArgsNote` 与三个编码器的单元用例验证。协议降级到模型的那部分由 `internal/gateway` 的 `TestProbeModel*` 与 `TestForwardUsesModelProtocol` 验证，迁移兼容由 `internal/store` 的 `TestMigration0005*`、`TestMigration0006*`、`TestMigration0007*` 与 `TestMigrationsRunToLatestFromEmptyDB` 验证；厂商路由的解析、探活收紧与模型列表回退由 `internal/store` 的 `TestProvider*` 与 `internal/gateway` 的 `TestForwardUsesProviderRoute`、`TestEndpointFallsBackWithoutProviderRoute`、`TestProbeModelNarrowsCandidatesByProvider`、`TestProbeModelStartsFromDiscoveredProtocol`、`TestSyncModelsUsesProviderModelsPath`、`TestSyncModelsWithoutModelsPath` 验证。厂商写入口与模型列表路径由 `internal/store` 的 `TestCreateProvider*`、`TestSetProvider*`、`TestDeriveModelsPath`、`TestResolveModelsPath`、`TestRouteModelsPathDerivedOnWrite`、`TestStoreWritePathsValidateBaseURL`、`TestMigration0008*` 与 `internal/gateway` 的 `TestSyncModelsAuthFollowsModelsPathRoute`、`TestSyncModelsProviderModelsPathOverride`、`TestSyncModelsWithEndpointOverride`、`TestSyncModelsNoModelsPathAnywhere` 验证；路径与地址的规整由 `internal/store` 的 `TestNormalizePath`、`TestNormalizeBaseURL`、`TestCreateProviderNormalizesPaths`、`TestSetProviderNormalizesPaths` 与 `internal/gateway` 的 `TestJoinEndpoint` 验证；端点取值的两种写法（相对路径与整条地址）由 `internal/store` 的 `TestIsFullAddress`、`TestNormalizeEndpoint`、`TestDeriveModelsPathFullAddress`、`TestCreateProviderStoresFullAddressRoutes`、`TestSetProviderFullAddressRoutesAreIndependent`、`TestStoreWritePathsRejectBadFullAddress` 与 `internal/gateway` 的 `TestForwardUsesFullAddressRoute`、`TestForwardMixesFullAddressAndRelativeRoutes`、`TestSyncModelsFullAddressModelsPath`、`TestSyncModelsWithEndpointFullAddress`、`TestSyncModelsWithEndpointRejectsBadFullAddress` 验证；一次尝试的账与结论一起提交由 `internal/store` 的 `TestRecordAttemptWritesRowAndStateTogether` 验证；直通路径的断流结尾由 `internal/gateway` 的 `TestPassthroughStreamDisconnectKeepsLegalEnding`、`TestPassthroughCRLFTailStartsAtFrameBoundary` 逐协议验证，转换路径的干净截断与 responses 入站的 `response.failed` 由 `TestConversionStreamCleanTruncationIsFailure`、`TestConversionStreamResponsesInboundEndsFailed` 验证，收尾帧按 `type` 或 `event` 名判定（带空格、多行缩进、只写事件名）由 `TestPassthroughEndFrameShapeDoesNotMatter` 验证，完整流不被追加字节由 `TestProtocolMatrixStreaming` 的逐字节断言保证；端点两种写法在命令面上的行为与 `account set --key` 由 `internal/cli` 的 `TestProviderPathsAcceptFullAddress`、`TestProviderPathsRejectBadFullAddress`、`TestModelSyncEndpointFullAddress`、`TestModelSyncEndpointBadValues`、`TestAccountSetRotatesKey`、`TestAccountSetKeyFromStdin`、`TestAccountSetErrors`、`TestAccountSetJSONMasksKey`、`TestAccountUpdateIsSynonymForSet`、`TestAccountTestWithoutModels` 与 `internal/gateway` 的 `TestProbeEmptyModelsUsesFallbackModel`、`TestProbeEmptyModelsKeepsNoteOnFailure` 验证；模型清单接口由 `internal/gateway` 的 `TestModelsListDedupesSortsAndPicksOwner`、`TestModelsListSkipsDisabledKeepsCooling`、`TestModelsListEmpty`、`TestModelByIDHitAndMiss`、`TestModelByIDPathIsCleanedBeforeMatching`、`TestModelByIDDisabledOnlyModelIs404`、`TestModelsRejectNonGet`、`TestModelsEndpointsAreReadOnly`、`TestModelsEndpointsLeaveExistingRoutesAlone` 验证，四条入站路径 405 的 `Allow: POST` 由 `TestInboundRejectsOtherMethodsWithAllowPost` 与 `TestGeminiInboundUnknownPathAndMethod` 验证；错误体按入站协议给形状由 `internal/gateway` 的 `TestInboundErrorShapeWhenAllAccountsFail`、`TestInboundErrorShapeWhenModelMissing`、`TestInboundErrorShapeScrubsSecret`、`TestInboundStreamErrorFrameShapePerProtocol`、`TestInboundStreamErrorFrameScrubsSecretPerProtocol`、`TestReadOnlyAndUnknownPathsKeepUnifiedErrorShape` 按四条入站逐字段核对（形状、`type`/`status`/`code`/`param`、脱敏与没有入站协议的入口不变）。gemini 的映射由 `internal/translate` 的对拍用例 `chat_tools_to_gemini_function_declarations`、`anthropic_image_request_to_gemini_inline_data`、`gemini_tool_call_response_to_chat_tool_calls`、`gemini_tool_call_response_to_anthropic_tool_use`、`gemini_max_tokens_response_to_anthropic`、`gemini_contents_request_to_chat`、`gemini_request_to_chat_image_and_named_tool`、`chat_response_to_gemini_candidate`、`gemini_safety_response_to_chat_end_turn`、`gemini_stream_to_anthropic_text_and_tool`、`anthropic_stream_to_gemini_tool_call` 钉住，边界（孤立工具结果丢弃、非对象 `args` 省略、`promptFeedback.blockReason`、用量读取、非正常 `finishReason` 的 `StopNote`、入站路径解析、两端的流式增量序列）由 `internal/translate` 的 `TestGemini*` 与 `TestEncodeGemini*` 钉住，转发路径（路径里的模型名、`x-goog-api-key`、两种路由写法与占位符共存、模型名转义、不含占位符的路由不变、直通不改请求体、流式端点改写、gemini 入站的非流式与流式、错误体解包、断流收尾、探活）由 `internal/gateway` 的 `TestForwardChatToGemini`、`TestForwardAnthropicToGemini`、`TestGeminiRoutePlaceholderInFullAddress`、`TestGeminiModelNameIsPathEscaped`、`TestRouteWithoutPlaceholderUnchanged`、`TestGeminiOutboundStreamEndpoint`、`TestForwardGeminiInboundToChat`、`TestForwardGeminiInboundStreamToAnthropic`、`TestForwardGeminiOutboundStream`、`TestGeminiPassthroughKeepsBytes`、`TestGeminiInboundStreamToGeminiPassthrough`、`TestGeminiInboundErrorBodyUnwrapped`、`TestGeminiInboundModelNameIsPathUnescaped`、`TestGeminiInboundUnknownModel`、`TestGeminiStreamTruncatedEndsWithErrorAndFinish`、`TestGeminiStopNoteLogged`、`TestProbeGeminiRoute`、`TestModelEndpoint` 与 `internal/store` 的 `TestGeminiRouteKeepsModelPlaceholder` 验证。gemini 的模型列表同步由 `internal/gateway` 的 `TestSyncModelsGeminiShape`、`TestSyncModelsGeminiAuthHeader`、`TestSyncModelsGeminiPaginates`、`TestSyncModelsGeminiPageLimit`、`TestSyncModelsNoNextPageTokenStops`、`TestSyncModelsWithoutGenerationMethodsUnfiltered` 验证（方法过滤、去前缀、两页拼接、超过 10 页报错、游标为空即停、ollama 风格不受影响），种子行与升级路径由 `internal/store` 的 `TestMigration0009SeedsGeminiProvider`、`TestMigration0009UpgradesOldDatabase`、`TestMigration0009KeepsUserDefinedGeminiProvider` 与 `TestProvidersSeededByMigration` 验证。上游是 `httptest` 假服务，M5、M6、M7、M8、M9 与 M10 里涉及真实厂商的那部分需要拿到 key 后手工验收。

已知未做，按已知行为处理：

- 估算花费依赖账号上手工填的 `--price-in`、`--price-out`、`--price-cache-read` 与 `--price-cache-write`（单位每百万 token），没有内建价格表；一个价都没填时记为 0 并在 `usage` 末尾提示，缓存单价未填时回退到输入价（见 5.5 的公式）。
- `in_tokens_uncached` 只在入账那一刻算一次：之后改归一口径不会重算历史行（迁移按当时的 `in_tokens` 回填）。三种协议的输入 token 含义不同，anthropic 的 `input_tokens` 已排除缓存，chat 的 `prompt_tokens` 与 gemini 的 `promptTokenCount` 含缓存命中，不按出站协议归一化时 anthropic 的缓存读等于白用、chat 与 gemini 的缓存读按满价多算；归一化由 `store.UncachedInTokens` 在写 `requests` 时做掉（转发与探活两处入账都过它），上游自己报出自相矛盾的数（缓存比输入还多）时只能夹到 0，不记负数。
- 别名只做精确匹配（见 5.12）：不做大小写归一、不做通配与前缀替换，链最长 8 跳；响应里的模型名保持上游的真实名字。
- 探活并发默认 4，`--concurrency` 可以改，只是默认值，不会根据上游限速自适应。
- gemini 只取 `candidates[0]`，`candidateCount` 与多候选不搬；流式里不搬图片（`inlineData` / `fileData`）。
- gemini 的思考内容（`thought` 标记与 `thoughtSignature`）不搬，请求侧的思考配置参数按 5.4 落成 `thinkingConfig`；`ParallelToolCalls` 与 `tool_result.is_error` 丢弃；工具结果的 `response` 固定写成 `{"result": <文本>}`，非对象的 `args` 整段省略；gemini 自己的内置工具（`googleSearch`、`codeExecution` 一类，`tools[]` 里只有对应键、没有 `functionDeclarations`）整条丢弃，既不给上游伪造、也不转成别的协议的工具定义（见 5.9）。
- gemini 的多模态只到「图片按 inlineData / fileData 搬运」为止：音频、视频与文件上传（Files API）不搬，`fileData.fileUri` 只接受 Files API 给出的 URI，把普通 http(s) 图片地址写进去很可能被上游拒（见第 10 节）。
- 思考内容只在响应方向搬运：跨协议时请求里的思考内容（chat 的 `reasoning_content`、responses 的 `reasoning` item）整块丢弃，因为主流推理上游对回填这两种字段会直接回 400。代价是多轮对话里客户端带着的那段思考不再进入上下文；anthropic 带 `signature` 的思考块例外，回填是它要求的写法（见 5.4）。
- 流式转换下发给 anthropic 客户端的 `thinking` 块没有 `signature`：上游不是 anthropic 时给不出签名。客户端若把这些块回填到下一轮请求，anthropic 上游会以 400 拒绝；非流式不写无签名的思考块（见 5.4），两条路径的代价不一样，因为流式的增量写出后撤不回来。
- reasoning token 单独记账（见 5.5）：chat 的 `completion_tokens_details.reasoning_tokens`、responses 的 `output_tokens_details.reasoning_tokens` 与 gemini 的 `thoughtsTokenCount` 记进 `requests.reasoning_tokens`（anthropic 没有独立字段，记 0），是否重复计入输出由 `total` 判定（`total > prompt + completion` 才算独立的一项，判定在 `store.ReasoningExtra`）。
- responses 的 `encrypted_content` 与 reasoning item 的 `id` 不搬：它们只有原厂商的上游认，搬过去只会让上游拒收。
- 音频与文件类内容丢弃，只搬运文本、图片、思考内容、工具调用与工具结果五类块。
- 工具定义的边界按已知行为处理：anthropic 的服务端内置工具（`tools[].type` 不是 `custom`，例如 `web_search_20250305`）跨协议时整条丢弃，chat 与 responses 的工具定义只认 function，编回 anthropic 时才原样保留 `type`；`tool_result.is_error` 只有 anthropic 有落点，从 chat 与 responses 进 anthropic 时恒为 false，反方向同样丢弃；工具调用的 `arguments` 只校验是不是合法 JSON，不校验是不是对象（`null`、`"x"` 都原样搬过去）。`tools[].strict` 与 `allowed_tools` 的搬运规则见 5.4 的工具定义与选择。
- 流式转换下断在工具参数中间时，已经下发的 `partial_json` 撤不回来，客户端可能拿到 `{"city":"北` 这种无法解析的 `input`：收尾的错误帧文案会写明「工具调用参数在上游中断，内容不完整」，这一次请求记 `ok=0`，程序不伪造一段闭合的 JSON。
- responses 入站流式的 `response.completed.response.output` 按 `output_index` 列文本 item（`message`，正文在 `content` 的 `output_text` 里）、工具 item（`function_call`）与思考 item（`reasoning`），与事件里的 `output_index` 一一对上，客户端按它重建答案不必自己拼增量。回填正文的代价是内存：单条响应攒的正文总长上限 1MB，超过之后停止继续攒，攒不全的文本 item 不发 `output_item.done`、也不进 `output`（其余 item 与 `status`、`usage` 照常给出），并记一行日志；超限的长回答要拿全文仍然只剩拼增量这一条路。断流走 `response.failed` 时 `output` 恒为 `[]`，不回填半截正文与半截参数。
- 思考增量把 anthropic 的工具块关掉、之后又来了同一个工具的参数时，编码器只能重开一个新块，于是同一个 `tool_use` 的 `id` 出现在两个块里，前一块的 `partial_json` 也是半截。上游把 reasoning 插在两次 `arguments` 之间才会走到这里；优先保证事件序列合法（每个 `content_block_delta` 都对应一个未关闭的块），不伪造一个把两段拼起来的块。
- 跨协议只做图片地址的搬运，没有图片转写以外的多模态变换。
- 路径里 `//` 有真实含义的上游（少数 CDN 风格端点）目前无法表达：`store.NormalizePath` 会把连续斜杠折叠成一条，`provider add/set` 写进去的 `//` 落库就只剩一个 `/`；整条地址的路径部分（`https://host//a//b`）也过同一套折叠，同样表达不了这种路径（查询串与 `#` 之后不在折叠范围内）。
- `account test` 报的上游模型数与 `model sync` 用同一套解析（三种形状都认，见 5.7），但两者共用同一个读取上限（8MB）：超过上限的清单两边都读不出来，`account test` 的模型数列是 `-`、备注写「上游返回的不是模型列表」，同步则直接报错。凭据与连通性的结论不受影响。
- 模型列表的响应形状只认三种已知形状（见 5.7）：`{data:[…]}`、`{models:[…]}` 与顶层数组。三者都不匹配时报错并列出这三种形状，包了一层的响应（例如 `{"result":{"data":[…]}}`）同样算不匹配，这类上游仍只能用 `--models` 手填。
- 两个模型清单接口每次 GET 都重新全表扫描 `accounts`（`ListAccounts` 之后逐行取 `accounts.models`），没有缓存。库只有一条连接，客户端若在每个请求之前拉一次清单，这次读会与转发写 `requests` 的写入排队，高并发时清单请求会拖慢对话请求。第一版不加缓存：缓存要一套失效规则（账号启停、`model probe` 回写 `accounts.models`、用户直接改库），错一次的代价比多一次读高。
- 需要固定附加头的上游靠 5.10 的声明接：opencode 的 Go 端点要求带 `x-opencode-session` 才路由，缺了这个头直接回 `400 Request is missing x-opencode-session`（实测）；`provider header set opencode x-opencode-session '{session}'` 之后再打同一个地址，错误变成该 key 没有这个模型的 400，说明路由这一关已经过了。客户端自己带的同名头不算数：转发只透传 5.11 白名单里的头，白名单之外一律丢弃，要加头只能声明。仍然接不了的是**按请求算出来的头**：要求按时间戳、请求体或密钥算一个签名的厂商（签名算法不在声明能表达的范围内），以及需要多步换取会话 token 才能调用的端点。

之后（不在第一版）：订阅账号登录，隐私遮蔽，配置档案（profiles），价格表内置。

## 8 代码结构

```
cmd/lax/                main，只做 os.Exit(cli.Run(...))
internal/cli/           子命令分发、参数解析、输出格式
internal/config/        路径解析与配置读写
internal/store/         SQLite 连接、迁移、账号与请求记录的读写
internal/secret/        凭据加解密
internal/account/       账号池、冷却、选号
internal/gateway/       HTTP 入站处理、上游转发、错误映射
internal/translate/     中间表示（ir.go）、四套协议编解码（anthropic.go、chat.go、responses.go、gemini.go；gemini 的入站与出站都在 gemini.go）
internal/usage/         用量汇总与花费估算
```

约束：`translate` 只做结构体到结构体的转换，不读环境、不发请求，便于用对拍用例覆盖；`gateway` 是唯一发上游请求的地方（`account test` 的探活也走 `gateway.Probe`）；`cli` 不直接连数据库，只调 `store` 与 `account`。

包之间的依赖：`store` 为协议常量（`translate.ProtocolAnthropic`、`translate.ProtocolChat`、`translate.ProtocolResponses`、`translate.ProtocolGemini`）import `internal/translate`，只取常量，不调用编解码；`gateway`、`cli`、`account` 都经 `store` 与 `translate` 拿协议取值，不各自复制一份字符串。

外部依赖：只有 `modernc.org/sqlite`（纯 Go 实现，不需要 cgo，交叉编译不受影响），其余全部用标准库。命令行解析用 `flag`，不引入第三方 CLI 框架。

## 9 非目标

- 多租户、配额、充值、开票。
- 网页后台与桌面菜单栏。
- 逆向接口、把订阅网页版包装成 API。
- 改写其它客户端的配置文件，不论带不带备份。
- 跨机部署与集群。
- 插件运行时（内置 JS 引擎、任意请求改写与执行第三方插件）：已知需要注入的厂商头用 5.10 的声明式附加头表达。
- systemone 与对话协议之间的转换：决策请求没有消息序列，也产不出文本，5.14 只做同协议直通。

## 10 需要外部信息才能定的点

- 各家上游的默认 base-url 与各协议的路径不再需要外部信息：迁移 `0006` 把已知厂商种进 `providers` 与 `provider_routes`，以后用 `provider ls|add|set|rm` 维护，新增厂商不必改代码重编译。新厂商的 `path` 要以它的文档为准：它是完整路径，程序只在 `base_url` 去掉末尾斜杠后直接拼接，不做补全；也可以写整条地址（含 `://`），此时直接用它、不再拼 `base_url`，同一厂商的不同协议因此可以落在不同域名上；模型列表路径没给时只给 openai-chat 路由按 `/chat/completions` → `/models` 推导；写法不同的上游用 `provider set --models-path` 给厂商级覆盖，路由级的值只由写入时推导产生，命令面不提供手工写入口。
- 各家模型名清单，以及某家是否兼容 `openai-chat`。
- gemini 的两处形状要在拿到真实 key 后确认：`functionResponse.response` 我们固定写成 `{"result": <文本>}`（5.9 的取舍），真实上游是否按这个键名理解语义还没实测；`fileData.fileUri` 只接受 Files API 给出的 URI，把普通 http(s) 图片地址写进去很可能被拒。这两条都不影响已覆盖的文本与工具调用映射，改起来只动 `gemini.go`。
- 各家价格，用于估算花费。缺价格就记 null。
- 各家推理上游是否接受请求里回填的思考内容：现在一律不回填（见 5.4），所以这条不影响转发。若以后要放宽，需要拿带推理的模型的真实 key 分别打 `/v1/chat/completions` 与 `/v1/responses`，试 `reasoning_content` 与 `reasoning` item（带 `summary`、带 `content`、两者都带）哪些返回 200。
