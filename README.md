# lax

本机上的模型总机。账号在命令行里管，所有客户端只连一个地址。

- **一个地址，多种客户端。** Claude Code 说 anthropic，Codex 说 openai-responses，普通 OpenAI SDK 说 openai-chat。
- **网关按各自形状收下，** 再转成上游认的形状。
- **八条报文协议。** 四条对话协议可以互相转换；决策、向量化与两条语音只做同协议直通。
- **东西都留在本机。** 模型从上游拉、逐个探活；选号、冷却、额度与用量记在本地数据库。
- **两份文档。** 怎么用看[文档站](https://docs.larkax.com)，为什么这样设计看 [DESIGN.md](DESIGN.md)。

## 目录

| 我想…… | 看这里 |
| --- | --- |
| 装起来跑通第一条请求 | [快速开始](#快速开始) |
| 把 Claude Code 或 SDK 接上来 | [客户端怎么接](#客户端怎么接) |
| 查某条命令怎么用 | [命令一览](#命令一览) |
| 配供应商、拉模型、调选号、看额度 | [配置](#配置) |
| 接语音、向量化、决策、插件 | [其他能力](#其他能力) |
| 知道哪些做不到 | [已知边界](#已知边界) |
| 改代码、发版 | [构建与发版](#构建与发版) |

## 快速开始

```bash
lax init                                            # 建配置、数据库与密钥
lax provider ls                                     # 看厂商来源与协议总览
lax account add ds --provider deepseek --key sk-xxx  # 或用 --key - 从标准输入读
lax model sync --account ds                         # 拉上游模型列表
lax model probe --account ds                        # 逐个探活，能用的才进选号
lax serve                                           # 起网关，127.0.0.1:4567
lax log tail                                        # 看请求记录
```

顺序是固定的：先建库，再登记厂商与账号，然后同步模型、探活，最后启动网关。

`account set` 用来换 key，不用删号重建。探活结果、协议发现与请求记录的账号归属都留着，冷却与失败计数清零。

完整的安装与首次配置：

- [安装](https://docs.larkax.com/docs/self-hosted/install)
- [快速开始](https://docs.larkax.com/docs/self-hosted/quickstart)

## 客户端怎么接

客户端里把地址填成本机网关即可，程序不用改。

Claude Code：

```
ANTHROPIC_BASE_URL=http://127.0.0.1:4567
ANTHROPIC_AUTH_TOKEN=lax-local            # 本机网关不校验，填个非空值即可
ANTHROPIC_MODEL=deepseek-v4-pro           # 必须是探活出来的可用模型名
```

说 openai-chat 与 openai-responses 的客户端（含 Codex）把 base URL 填成 `http://127.0.0.1:4567/v1`。

凭据值本机不校验，填一个非空值即可。

不知道有哪些模型名可用时直接问网关：

```bash
curl -s http://127.0.0.1:4567/v1/models
curl -s http://127.0.0.1:4567/v1/models/deepseek-v4-pro   # 单个模型，未命中返回 404
```

- 清单来自启用账号的模型列表，按名字去重升序，`owned_by` 是厂商名。
- 两个接口只读、只收 GET，其它方法返回 405 并带 `Allow: GET`。
- 不写请求记录，也不影响冷却与选号。
- 冷却中的模型照样列出，停用账号的模型不列。

`/healthz` 是运维视角的账号与模型计数；模型名清单看上面两个接口。

## 命令一览

| 命令 | 作用 |
| --- | --- |
| `init` | 建配置目录、数据库、加密密钥 |
| `serve` | 前台起网关 |
| `account` | 账号的增删、启停与凭据 |
| `provider` | 供应商的地址、协议路由与请求头 |
| `model` | 模型目录、探活、别名与定价 |
| `route` | 选号的策略、权重与预演 |
| `quota` | 账号额度的快照与监控设置 |
| `plugin` | 插件的安装、升级、校验与启停 |
| `usage` | 按账号、模型或日期汇总用量 |
| `log tail` | 最近的请求记录 |
| `doctor` | 自检配置、目录、密钥、端口、数据库 |
| `version` | 打印版本号；`--json` 给出 `name` 与 `version` |

每组的子命令：

- `account`：`add`、`ls`、`set`、`rm`、`test`、`enable`、`disable`
- `provider`：`ls`、`add`、`set`、`unset`、`rm`、`header`
- `model` 查看与同步：`ls`、`show`、`add`、`sync`、`probe`
- `model` 定价与别名：`alias`、`price`
- `model` 能力与手工请求：`capability`、`dialect`、`call`、`decide`、`speak`、`transcribe`
- `route`：`ls`、`show`、`set`、`unset`、`account`、`explain`、`policy`
- `quota`：`ls`、`show`、`refresh`、`set`、`unset`
- `plugin` 安装与升级：`install`、`update`、`rollback`、`verify`
- `plugin` 查看与诊断：`ls`、`show`、`logs`、`doctor`
- `plugin` 配置与启停：`config`、`reload`、`enable`、`disable`、`rm`

每个命令都支持 `--json`，结果走 stdout，错误走 stderr，两种输出不混在同一条流里。

不带子命令等同 `--help`：`lax`、`lax model`、`lax plugin` 这类组都打印帮助到 stdout、退出码 0。

退出码：0 成功，1 运行失败，2 用法错误。

参数细节用 `lax <命令> --help` 查，或看[命令行参考](https://docs.larkax.com/docs/reference/cli)。

## 协议与转换

lax 认八条报文协议，按入站路径区分。

- **四条对话协议能进也能出：** anthropic、openai-chat、openai-responses、gemini。
- **另外四条只做同协议直通：** systemone 是决策，openai-embeddings 是向量化。
- openai-speech 与 openai-transcriptions 是两条语音协议，同样只做直通。

八条入站路径同时可用，一个网关可以同时接多类客户端：

| 入站路径 | 入站协议 | 常见客户端 |
| --- | --- | --- |
| `POST /v1/messages` | anthropic | Claude Code |
| `POST /v1/chat/completions` | openai-chat | 普通 OpenAI SDK 客户端 |
| `POST /v1/responses` | openai-responses | Codex |
| `POST /v1/systemone` | systemone | 决策客户端，见[决策请求](#决策请求) |
| `POST /v1/embeddings` | openai-embeddings | 需要向量化的 OpenAI SDK 客户端 |
| `POST /v1/audio/speech` | openai-speech | 用 `audio.speech.create` 的客户端 |
| `POST /v1/audio/transcriptions` | openai-transcriptions | 用 `audio.transcriptions.create` 的客户端 |
| `POST /v1beta/models/{model}:generateContent` 等 | gemini | Google SDK 或原生客户端 |

gemini 的模型名在路径里，请求体中没有 `model` 字段。

`:` 之后的后缀不算名字的一部分，流式由 `:streamGenerateContent` 后缀决定。

入站与出站相同时直通，不同时经中间表示转换：

- **直通：** 请求体只改 `model` 字段，响应按字节原样返回。
- **转换：** 只搬映射表里定义的字段，表外字段丢弃。
- **四条对话协议两两可转，** 共十六种组合，流式与非流式都可用。

未知路径返回 404，请求体解析失败返回 400，错误消息带协议名前缀。

细节见：

- [协议](https://docs.larkax.com/docs/self-hosted/gateway/protocols)
- [转换](https://docs.larkax.com/docs/self-hosted/gateway/conversion)

## 配置

供应商、模型、选号与额度都在命令里配。每节只留结论与最常用的一条命令。

### 供应商

供应商（命令输出里写「厂商」）是上游服务的登记项。地址、默认协议、各协议的路径、凭据头与附加头都记在这一行上。

账号只存厂商名，地址与协议都在那家厂商那一行。所以 `account add` 没有 `--base-url`：先登记厂商，再建账号。

```bash
lax provider add myco --base-url https://api.deepseek.com \
    --openai /v1/chat/completions \
    --anthropic /anthropic/v1/messages \
    --default-protocol openai
```

- 每个协议一条 flag。四条对话协议是 `--anthropic`、`--openai`、`--responses`、`--gemini`。
- 另外四条是 `--systemone`、`--embeddings`、`--speech`、`--transcriptions`。
- 值可以写相对路径，也可以写整条地址；整条地址用于同一厂商不同协议分域名。
- 路径的斜杠写法由程序规整，`v1/chat/completions` 与 `//v1//chat/completions/` 是同一个地址。
- `--base-url` 只在所有路由都是整条地址时可以省略。
- 改地址或默认协议立刻影响所有用它的账号，这是单一来源应有的行为。

凭据头默认按报文协议推导，不是这个写法的上游按厂商声明。

附加请求头用 `provider header set` 声明，值是下面四种之一：

- 字面量。
- `{env:NAME}`，取环境变量。
- `{session}`，本次对话的会话键。
- `{model}`，本次请求的模型名。

细节见：

- [供应商与路由](https://docs.larkax.com/docs/self-hosted/configure/providers)
- [请求头](https://docs.larkax.com/docs/self-hosted/gateway/headers)

### 模型

模型列表不用手填。`model sync` 拉上游清单，`model probe` 逐个试，能用的才进选号。

```bash
lax model sync --account ds                      # 拉上游模型列表
lax model probe --account ds                     # 逐个探活
lax model probe --account ds --limit 5           # 只探这个账号的前 5 个
lax model alias set fast glm-4.6                 # 客户端写死的名字指到真名
lax model price set glm-4.6 --in 0.6 --out 2.2   # 每百万 token 单价
```

- 清单三种形状都认：`{data:[…]}`、`{models:[…]}`、顶层数组。
- 探活会真实打上游；401 停用账号，403、404 与明说模型不存在的 400 记不可用。
- 某个账号没探出可用模型时只写探活结果，它原有的模型列表保持不变。
- 别名只影响请求方向，响应里的模型名仍是上游真名。
- 单价挂在模型上，不挂在账号上；套餐真的不同时才给单个账号加覆盖价。

细节见：

- [模型管理](https://docs.larkax.com/docs/self-hosted/configure/models)
- [定价](https://docs.larkax.com/docs/self-hosted/configure/pricing)

### 选号

不配置时按 `priority` 从小到大试。429、408、5xx 或连接失败才换下一个。

| 策略 | 怎么排 |
| --- | --- |
| `priority` | 账号 `priority` 升序，主备 |
| `rotate` | 每次请求换起点，平均分摊 |
| `weighted` | 按权重比例轮转 |
| `success` | 窗口内成功率降序 |
| `softmax` | 优先级、成功率、延迟评分后按概率抽 |

```bash
lax route set glm-4.6 --strategy softmax --temperature 0.1 --explore 2
lax route explain glm-4.6 --session demo --seed 42   # 不发请求，预演这次怎么选
```

- 冷却按「账号 + 模型」隔离：429 首次 500 毫秒，5xx 首次 10 秒，连接失败首次 2 秒。
- 失败次数越多冷却越久，上限 30 分钟。
- 候选都试完返回 503，把重试交给客户端；不在一次请求内等冷却。
- `route explain` 只读库，不写请求记录、不改冷却。
- 额度可以参与选号，默认关闭。

细节见：

- [选号](https://docs.larkax.com/docs/self-hosted/route/index)
- [策略](https://docs.larkax.com/docs/self-hosted/route/policies)
- [失败处理](https://docs.larkax.com/docs/self-hosted/route/failure)

### 账号额度

只读查上游余额与到期，存一份快照给命令看。默认不自动刷新，也不参与选号。目前只支持 CommandCode 与 DeepSeek。

```bash
lax quota refresh cc1              # 手动查一次
lax quota ls                       # 所有账号的快照
lax quota show cc1                 # 明细：余额、本期已用、周期结束
lax quota set cc1 --fx-rate 0.14   # 非 USD 币种折 USD 的手工汇率
```

- 余额不是 USD 时必须手填汇率；没给时折合 USD、剩余比例与排序都按未知处理。
- 额度刷新失败只更新快照状态，不停用账号、不写请求记录。
- 要不要让额度参与选号，在 `route set --quota-order` 上配置，默认关闭。

细节见[账号额度](https://docs.larkax.com/docs/self-hosted/route/quota)。

## 其他能力

### 语音

两条报文协议，报文按 OpenAI 的形状：`POST /v1/audio/speech` 与 `POST /v1/audio/transcriptions`。

用之前要做两件事：给模型声明能力，再绑一条方言。

```bash
lax provider set openai openai-speech=/v1/audio/speech
lax model capability set gpt-4o-mini-tts --capability speech \
    --voices alloy,nova --audio-formats mp3,wav --dialect lax/openai-speech@1
```

- 没声明能力的模型在本地就回 400，一条上游调用都不发。
- 出站形状由绑定的方言决定；`lax/` 前缀是宿主内置，插件可以带来别的。
- 不做转码、不做流式；`stream_format: "sse"` 与 ASR 的 `stream: true` 都回 400。

细节见[语音](https://docs.larkax.com/docs/self-hosted/gateway/speech)。

### 向量化

输入文本、输出向量，没有角色、没有工具、没有流式。它与对话协议不相通，只做同协议直通。

入站两条：`POST /v1/embeddings` 与 `POST /v1beta/models/{model}:embedContent`。

```bash
lax provider set openai openai-embeddings=/v1/embeddings
```

没声明这条路由时 `/v1/embeddings` 回 400，并给出上面这条命令。

细节见[向量化](https://docs.larkax.com/docs/self-hosted/gateway/embeddings)。

### 决策请求

systemone 是第五条报文协议，不是对话：一条请求由一个 `state` 加一组问题组成，回的是答案与概率，不产文本。

```bash
lax model decide --model typesafe/jev --state "客户说：这个月的账单被扣了两次" \
    --noul "present=Is this text present?" \
    --choice "mood=这是哪一类问题?" --choice "mood:billing=账单与付款,other"
```

- 入站与出站都是 `POST /v1/systemone`，只与自己互通。
- 决策模型不在上游清单里，用 `model add --protocol systemone` 手写声明。
- 有效出站协议不是 systemone 的模型会被拒绝。

细节见[决策请求](https://docs.larkax.com/docs/self-hosted/gateway/decision)。

### 插件

插件是目录里的包，三个文件各管一件事：

- `lax.plugin.json` 说 ID、版本、运行时、权限与它带来的厂商。
- `dialects.json` 说上游形状。
- `models.json` 说模型与方言绑定。

运行时有两种：

- `declarative` 按数据执行映射，插件不跑代码，只承载语音两条操作。
- `moejs` 在进程内 JS 沙箱里跑代码，可以给对话、向量化与决策请求补报文。

```bash
lax plugin install doubao-seed-tts --trust sandboxed --yes
lax plugin ls
lax plugin doctor
```

- 来源可以是本地目录、GitHub 仓库里的插件路径，或官方 `lax-plugins` 仓库里的裸插件名。
- 沙箱没有 `fetch`、`fs`、`process`，联网只能走 `lax.http.request` 的白名单 https。
- 沙箱没有内存硬上限，只有几道字节闸门；逐 hook 超时打不断在途的 HTTP 请求。

细节见：

- [插件](https://docs.larkax.com/docs/self-hosted/plugins/index)
- [安装](https://docs.larkax.com/docs/self-hosted/plugins/install)
- [沙箱](https://docs.larkax.com/docs/self-hosted/plugins/security)

## 数据在哪

| 路径 | 内容 |
| --- | --- |
| `$XDG_CONFIG_HOME/lax/config.json` | 监听地址、数据库路径、后端与连接串 |
| `$XDG_DATA_HOME/lax/lax.db` | 账号、模型状态、请求记录（只用 SQLite 时） |
| `$XDG_DATA_HOME/lax/secret.key` | 加密凭据的密钥 |
| `$XDG_DATA_HOME/lax/lax.pid` | `serve` 运行时的进程号，退出时删掉 |
| `$XDG_DATA_HOME/lax/plugins/` | 已安装的插件包 |

数据库缺省是 SQLite 单文件。

要用 PostgreSQL 或 MySQL，在配置里改 `driver` 与 `dsn` 两项。表结构走 `AutoMigrate`，不需要手工建库。

凭据用 AES-256-GCM 加密后入库。这防的是备份文件与日志泄漏，不防同一台机器上以同一用户身份运行的程序。

细节见[数据与备份](https://docs.larkax.com/docs/self-hosted/operate/data)。

## 已知边界

### gemini

- 入站、出站都支持流式与非流式。
- 模型列表可同步，按 `nextPageToken` 翻页。
- 模型名在 URL 路径里，路由可以写 `{model}` 占位符。
- 非正常的 `finishReason`（`SAFETY` 一类）按正常结束转给客户端。
- 内置工具与思考内容不搬。

### 跨协议转换

- 搬五类块：文本、图片、思考内容、工具调用、工具结果。
- 思考内容只在响应方向搬，请求方向不回填；回填会被上游以 400 拒掉整条请求。
- 签名只有 anthropic 有落点，所以 chat 与 responses 的思考内容转到 anthropic 时丢弃。
- 工具结果之后紧跟的追问并进同一条 user 消息，因为 anthropic 不接受两条相邻的 user 消息。
- 不搬：思考配置参数、`encrypted_content`、`tools[].strict`、音频与文件类内容。
- 流式转换下断在工具参数中间时，已下发的片段撤不回来，这一次记失败。

### 请求头、冷却与计费

- 不改写任何客户端配置文件。
- 要求按请求计算签名的上游仍然接不了。
- 探活调用不计入用量汇总。
- 估算花费需要先定价，缓存读写未填时回退到输入价。
- `usage` 与 `log tail` 里仍分别显示缓存读写的 token。

### 语音

- 不做音频转码，也不解析音频容器算时长。
- 直通路径按入参字符与音频秒计费，chat 方言按 token 计。
- 响应里的 `model` 原样，不改写成客户端用的名字。

### 插件与方言

- 本版落地声明式插件、内置方言与 `moejs` 沙箱。
- 仍没有：`trusted-worker`、插件市场与签名、OAuth 登录。
- 沙箱没有内存硬上限，只有源码、返回值、注入数据与 cache 四道字节闸门。
- 逐 hook 超时打不断在途的 `lax.http.request`。
- ESM 只支持静态 `export`、`export default` 与相对 import。
- 联网只走白名单 https，没有媒体通道。

### 账号额度监控

- 只支持 CommandCode 与 DeepSeek。
- CommandCode 的 `/alpha/*` 是未公开接口，路径与字段都可能变。
- 人民币折 USD 没有可靠默认汇率，必须手填。
- 额度刷新失败只写快照状态，不停用账号、不写请求记录。
- 额度刷新本身不是转发请求，不计入 `usage` 与 `log`。

## 构建与发版

```bash
make check        # go vet + go test + go build
make build        # 生成带版本号的 ./lax
./lax version
```

只需要 Go 工具链，生成的 `lax` 是单文件，不依赖外部服务，也不需要 cgo。

`make build` 从最近的 `v*` tag 推导版本号，例如 `0.1.0-3-gabc1234`；直接 `go build` 得到 `0.1.0-dev`。

发布包从 GitHub Releases 下载，文件名形如 `lax_0.1.0_linux_amd64.tar.gz`，同一页有 `checksums.txt`。

Windows 不在发布范围：`serve` 与 `plugin reload` 依赖 Unix 信号。

发版流程与自动部署规则见 [AGENTS.md](AGENTS.md) 的发版段。

## 更多

| 想了解 | 位置 |
| --- | --- |
| 怎么装、怎么配、怎么排障 | [自部署](https://docs.larkax.com/docs/self-hosted) |
| 用 API Key 接公开入口 | [公开入口](https://docs.larkax.com/docs/public) |
| Claude Code、Codex、各家 SDK | [客户端接入](https://docs.larkax.com/docs/clients) |
| 命令参数与退出码 | [命令行参考](https://docs.larkax.com/docs/reference/cli) |
| 术语速查 | [术语](https://docs.larkax.com/docs/reference/glossary) |
| 常见问题 | [常见问题](https://docs.larkax.com/docs/reference/faq) |
| 为什么这样设计 | [DESIGN.md](DESIGN.md) |
