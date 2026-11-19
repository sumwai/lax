# lax

本机上的模型总机。账号与路由在命令行统一管理，所有客户端只需连接一个本地地址。

[文档站](https://docs.larkax.com) · [架构设计 (DESIGN.md)](DESIGN.md) · [快速开始](#快速开始) · [已知边界](#已知边界)

---

## 核心特性

- **单一入口，多端接入**：Claude Code 使用 `anthropic`，Codex 使用 `openai-responses`，常规 OpenAI SDK 使用 `openai-chat`。客户端无需修改代码，仅需指向本机网关地址。
- **八条协议，按需转换**：支持四条对话协议（`anthropic`、`openai-chat`、`openai-responses`、`gemini`）双向互转（16 种组合，流式与非流式均可用）；决策、向量化与两条语音协议做同协议直通。
- **状态本地化，可观测**：模型清单从上游同步并逐个探活；选号策略、故障冷却、额度快照与用量统计全部持久化在本地数据库中。
- **轻量单文件与插件扩展**：核心为单一 Go 二进制文件，不依赖外部服务；支持声明式方言与 `moejs` 沙箱插件扩展上游适配。

## 目录

| 主题 | 说明 | 跳转 |
| --- | --- | --- |
| 快速开始 | 初始化、登记供应商与账号、探活并启动网关 | [快速开始](#快速开始) |
| 客户端接入 | 配置 Claude Code、Codex / OpenAI SDK 与模型发现接口 | [客户端接入](#客户端接入) |
| 协议与转换 | 8 条入站路径、直通/转换机制与错误约定 | [协议与转换](#协议与转换) |
| 核心配置 | 供应商、模型同步与定价、选号策略、账号额度 | [核心配置](#核心配置) |
| 扩展能力与插件 | 语音、向量化、决策请求（`systemone`）与插件体系 | [扩展能力与插件](#扩展能力与插件) |
| 命令速览 | 主命令、子命令分组与 `--json` / 退出码规范 | [命令速览](#命令速览) |
| 数据与存储 | XDG 路径、SQLite / PG / MySQL 与凭据加密 | [数据与存储](#数据与存储) |
| 已知边界 | 各协议转换、计费、插件沙箱与额度接口的限制 | [已知边界](#已知边界) |
| 构建与发版 | 本地编译、版本号规则与平台支持范围 | [构建与发版](#构建与发版) |

---

## 快速开始

```bash
lax init                                            # 初始化配置目录、数据库与加密密钥
lax provider ls                                     # 查看厂商来源与协议总览
lax provider add acme --base-url https://api.acme.example \
    --openai /v1/chat/completions                   # 登记供应商（内置预设可直接选用）
lax account add acct1 --provider acme --key sk-xxx  # 添加账号（支持 --key - 从标准输入读取）
lax model sync --account acct1                      # 从上游拉取模型列表
lax model probe --account acct1                     # 逐个探活，可用模型才会进入选号池
lax serve                                           # 前台启动网关（默认监听 127.0.0.1:4567）
lax log tail                                        # 查看近期请求记录
```

初始化流程需按固定顺序执行：**建库 → 登记供应商与账号 → 同步模型 → 探活 → 启动网关**。

> [!TIP]
> 更换上游密钥时使用 `lax account set`，无需删号重建。探活结果、协议发现与历史请求记录的账号归属均会保留，同时冷却状态与失败计数清零。

完整安装步骤与首次配置请参考：[安装指南](https://docs.larkax.com/docs/self-hosted/install) · [快速开始](https://docs.larkax.com/docs/self-hosted/quickstart)。

---

## 客户端接入

将客户端的 API 地址指向本机网关即可，程序逻辑无需改动。

### Claude Code

```ini
ANTHROPIC_BASE_URL=http://127.0.0.1:4567
ANTHROPIC_AUTH_TOKEN=lax-local            # 本机网关不校验凭据值，填写任意非空字符串即可
ANTHROPIC_MODEL=model-1                   # 须为探活通过的可用模型名
```

### OpenAI SDK 与 Codex

使用 `openai-chat` 与 `openai-responses` 协议的客户端（含 Codex），将 Base URL 设为 `http://127.0.0.1:4567/v1`，API Key 填写任意非空值即可。

### 模型清单查询

不确定当前有哪些可用模型时，可直接查询网关只读接口：

```bash
curl -s http://127.0.0.1:4567/v1/models
curl -s http://127.0.0.1:4567/v1/models/model-1   # 查询单个模型，未命中返回 404
```

- **数据来源**：汇总已启用账号的模型列表，按名称去重并升序排列，`owned_by` 字段为供应商名称。
- **状态可见性**：处于冷却中的模型照常列出，已停用账号的模型不会出现。
- **只读语义**：两个接口仅接受 `GET` 请求（其他方法返回 `405` 并携带 `Allow: GET`），不写入请求记录，也不影响冷却与选号。
- **运维探针**：`/healthz` 返回运维视角的账号与模型计数，模型名称清单请以上述 `/v1/models` 接口为准。

---

## 协议与转换

`lax` 识别八条报文协议，通过入站 HTTP 路径自动区分。所有入站路径同时开放，单台网关可并发服务多类客户端：

| 入站路径 | 入站协议 | 典型客户端 | 转发模式 |
| --- | --- | --- | --- |
| `POST /v1/messages` | `anthropic` | Claude Code | 四协议互转 / 直通 |
| `POST /v1/chat/completions` | `openai-chat` | 常规 OpenAI SDK | 四协议互转 / 直通 |
| `POST /v1/responses` | `openai-responses` | Codex | 四协议互转 / 直通 |
| `POST /v1beta/models/{model}:generateContent` 等 | `gemini` | Google SDK 或原生客户端 | 四协议互转 / 直通 |
| `POST /v1/systemone` | `systemone` | 决策客户端（见[决策请求](#决策请求)） | 仅同协议直通 |
| `POST /v1/embeddings` | `openai-embeddings` | 向量化 OpenAI SDK | 仅同协议直通 |
| `POST /v1/audio/speech` | `openai-speech` | `audio.speech.create` 客户端 | 仅同协议直通 |
| `POST /v1/audio/transcriptions` | `openai-transcriptions` | `audio.transcriptions.create` 客户端 | 仅同协议直通 |

- **`gemini` 路径规则**：模型名位于 URL 路径中，请求体不含 `model` 字段。`:` 后的后缀不计入模型名，是否启用流式由 `:streamGenerateContent` 后缀决定。
- **直通模式**：入站与出站协议相同时触发。请求体仅替换 `model` 字段，响应按字节原样透传。
- **转换模式**：入站与出站协议不同时，经中间表示进行转换。仅映射转换表内定义的字段，表外字段直接丢弃；四条对话协议两两可转（共 16 种组合），流式与非流式均支持。
- **错误处理**：未知路径返回 `404`；请求体解析失败返回 `400`，错误消息带有对应协议名前缀。

深入阅读：[协议说明](https://docs.larkax.com/docs/self-hosted/gateway/protocols) · [协议转换](https://docs.larkax.com/docs/self-hosted/gateway/conversion)。

---

## 核心配置

供应商、模型、选号策略与账号额度均通过命令行配置。以下各节保留核心结论与最常用示例。

### 供应商

供应商（命令输出中称为「厂商」）是上游服务的配置实体，集中维护 Base URL、默认协议、各协议路由路径、凭据头与附加请求头。账号仅关联供应商名称，因此 `account add` 不提供 `--base-url` 参数——需先登记供应商，再创建账号。

```bash
lax provider add acme --base-url https://api.acme.example \
    --openai /v1/chat/completions \
    --anthropic /anthropic/v1/messages \
    --default-protocol openai
```

- **协议路由 Flag**：四条对话协议对应 `--anthropic`、`--openai`、`--responses`、`--gemini`；其余四条对应 `--systemone`、`--embeddings`、`--speech`、`--transcriptions`。
- **路径格式**：值可写相对路径或完整 URL（用于同厂商不同协议分域名的场景）。路径斜杠由程序自动规整（`v1/chat/completions` 与 `//v1//chat/completions/` 等效）；仅当所有路由均填写完整 URL 时可省略 `--base-url`。
- **单一数据源**：修改供应商地址或默认协议会立即生效于所有关联账号。
- **请求头与凭据**：凭据头默认按报文协议自动推导，非标准上游可在供应商上显式声明；附加请求头通过 `provider header set` 配置，支持四种取值：字面量、`{env:NAME}`（环境变量）、`{session}`（当前会话键）、`{model}`（当前请求模型名）。

详情参见：[供应商与路由](https://docs.larkax.com/docs/self-hosted/configure/providers) · [请求头配置](https://docs.larkax.com/docs/self-hosted/gateway/headers)。

### 模型管理

模型列表无需手动录入，通过 `model sync` 拉取上游清单后，再由 `model probe` 逐个验证可用性。

```bash
lax model sync --account acct1                      # 拉取上游模型列表
lax model probe --account acct1                     # 逐个探活
lax model probe --account acct1 --limit 5           # 仅探活该账号的前 5 个模型
lax model alias set fast model-2                    # 将客户端别名映射至上游真实模型名
lax model price set model-2 --in 0.6 --out 2.2      # 设置每百万 token 输入/输出单价
```

- **清单解析**：自动兼容 `{data:[…]}`、`{models:[…]}` 及顶层数组三种常见响应格式。
- **探活行为**：真实向目标上游发送探测请求；遇到 `401` 将直接停用该账号，遇到 `403`、`404` 或明确提示模型不存在的 `400` 标记为不可用。若某账号未探出任何可用模型，仅写入探活结果，保留其原有模型列表不变。
- **别名与定价**：别名仅作用于请求方向，响应中的模型名仍保持上游真名；单价默认挂在模型上，仅当账号间套餐费率确实存在差异时才需配置单账号覆盖价。

详情参见：[模型管理](https://docs.larkax.com/docs/self-hosted/configure/models) · [模型定价](https://docs.larkax.com/docs/self-hosted/configure/pricing)。

### 路由与选号

未显式配置策略时，默认按账号 `priority` 从小到大依次尝试；仅在遇到 `429`、`408`、`5xx` 或连接失败时切换下一个候选账号。

| 策略 | 调度逻辑 |
| --- | --- |
| `priority` | 按账号 `priority` 升序排列，适合主备切换 |
| `rotate` | 每次请求轮换起始账号，平均分摊流量 |
| `weighted` | 按配置权重比例轮转分配 |
| `success` | 按统计窗口内成功率降序排列 |
| `softmax` | 综合优先级、成功率与延迟评分后按概率采样 |

```bash
lax route set model-2 --strategy softmax --temperature 0.1 --explore 2
lax route explain model-2 --session demo --seed 42   # 预演选号过程，不实际发请求
```

- **故障冷却**：按「账号 + 模型」粒度独立隔离。首次触发冷却时长：`429` 为 `500 ms`，`5xx` 为 `10 s`，连接失败为 `2 s`；连续失败将延长冷却时间，上限 `30 分钟`。
- **快速失败**：所有候选账号尝试完毕仍失败时返回 `503`，将重试决策交还客户端，网关不在单次请求内阻塞等待冷却。
- **只读预演与额度**：`route explain` 仅读取数据库，不写入请求记录、不改变冷却状态；如需让账号额度参与选号排序，可通过 `route set --quota-order` 开启（默认关闭）。

详情参见：[选号机制](https://docs.larkax.com/docs/self-hosted/route) · [路由策略](https://docs.larkax.com/docs/self-hosted/route/policies) · [失败处理](https://docs.larkax.com/docs/self-hosted/route/failure)。

### 账号额度

用于只读查询上游余额与到期时间并保存本地快照，默认不自动刷新，也不参与选号排序。

```bash
lax quota refresh acct1               # 手动刷新指定账号额度
lax quota ls                          # 查看所有账号额度快照
lax quota show acct1                  # 查看明细（余额、本期已用、周期结束时间）
lax quota set acct1 --fx-rate 0.14    # 为非 USD 币种设置折算 USD 的手工汇率
```

- 非 USD 计价的账号须手动设置汇率；未设置时，折合 USD 金额、剩余比例与排序均按未知处理。
- 额度接口与刷新限制见[已知边界](#已知边界)及 [账号额度文档](https://docs.larkax.com/docs/self-hosted/route/quota)。

---

## 扩展能力与插件

### 语音

提供两条 OpenAI 形态协议：`POST /v1/audio/speech`（TTS）与 `POST /v1/audio/transcriptions`（ASR）。使用前需完成两步配置：为模型声明能力，并绑定一条出站方言。

```bash
lax provider set acme openai-speech=/v1/audio/speech
lax model capability set model-tts --capability speech \
    --voices voice-a,voice-b --audio-formats mp3,wav --dialect lax/openai-speech@1
```

- 未声明语音能力的模型将在网关本地直接返回 `400`，不向上游发起任何请求。
- 出站请求形态由绑定的方言决定：`lax/` 前缀为宿主内置方言，自定义格式可通过插件扩展。
- 不支持音频转码与流式语音传输；TTS 的 `stream_format: "sse"` 与 ASR 的 `stream: true` 均返回 `400`。

详情参见：[语音能力](https://docs.larkax.com/docs/self-hosted/gateway/speech)。

### 向量化

接收文本输入、输出向量表示，无角色、无工具调用、无流式模式，与对话协议不互通，仅做同协议直通。支持两条入站路径：`POST /v1/embeddings` 与 `POST /v1beta/models/{model}:embedContent`。

```bash
lax provider set acme openai-embeddings=/v1/embeddings
```

若未声明 `openai-embeddings` 路由，请求 `/v1/embeddings` 将返回 `400` 并提示执行上述配置命令。详情参见：[向量化文档](https://docs.larkax.com/docs/self-hosted/gateway/embeddings)。

### 决策请求

`systemone` 是第五条报文协议（非对话协议）：单条请求包含一个 `state` 与一组问题，返回结构化答案与概率分布，不生成自由文本。

```bash
lax model decide --model model-decision \
    --state "客户说：这个月的账单被扣了两次" \
    --noul "present=Is this text present?" \
    --choice "mood=这是哪一类问题?" \
    --choice "mood:billing=账单与付款,other"
```

- 入站与出站均为 `POST /v1/systemone`，仅同协议互通。
- 决策模型不会出现在上游清单中，需通过 `lax model add --protocol systemone` 手动声明；有效出站协议非 `systemone` 的模型将被拒绝。

详情参见：[决策请求文档](https://docs.larkax.com/docs/self-hosted/gateway/decision)。

### 插件

插件以目录包形式分发，由三个核心清单文件组成：

- `lax.plugin.json`：定义插件 ID、版本、运行时类型、权限声明及内置供应商。
- `dialects.json`：定义上游报文形态。
- `models.json`：定义模型与方言的绑定关系。

支持两种运行时：`declarative`（纯数据驱动映射，不执行代码，仅承载语音两条操作）与 `moejs`（进程内 JS 沙箱，可为对话、向量化与决策请求扩展报文）。

```bash
lax plugin install example-tts --trust sandboxed --yes
lax plugin ls
lax plugin doctor
```

插件来源支持本地目录、GitHub 仓库中的插件子路径，或官方 `lax-plugins` 仓库中的裸插件名。沙箱权限与运行时约束详见[已知边界](#已知边界)及文档：[插件概览](https://docs.larkax.com/docs/self-hosted/plugins) · [安装指南](https://docs.larkax.com/docs/self-hosted/plugins/install) · [沙箱安全](https://docs.larkax.com/docs/self-hosted/plugins/security)。

---

## 命令速览

| 命令 | 职责 |
| --- | --- |
| `init` | 初始化配置目录、数据库与加密密钥 |
| `serve` | 前台启动本地网关服务 |
| `account` | 账号的增删、启停、测试与凭据更新 |
| `provider` | 供应商地址、协议路由与请求头配置 |
| `model` | 模型目录、同步、探活、别名、定价与手工调用 |
| `route` | 选号策略、权重设置与路由预演 |
| `quota` | 账号额度快照查询、刷新与汇率设置 |
| `plugin` | 插件安装、升级、校验、配置与启停 |
| `usage` | 按账号、模型或日期汇总 Token 与花费 |
| `log tail` | 查看近期网关请求记录 |
| `doctor` | 自检配置、目录权限、密钥、端口与数据库状态 |
| `version` | 输出版本号（`--json` 输出 `name` 与 `version`） |

**子命令分组**：

- `account`：`add`、`ls`、`set`、`rm`、`test`、`enable`、`disable`
- `provider`：`ls`、`add`、`set`、`unset`、`rm`、`header`
- `model`：查看与同步（`ls`、`show`、`add`、`sync`、`probe`）；定价与别名（`alias`、`price`）；能力与手工请求（`capability`、`dialect`、`call`、`decide`、`speak`、`transcribe`）
- `route`：`ls`、`show`、`set`、`unset`、`account`、`explain`、`policy`
- `quota`：`ls`、`show`、`refresh`、`set`、`unset`
- `plugin`：安装与升级（`install`、`update`、`rollback`、`verify`）；查看与诊断（`ls`、`show`、`logs`、`doctor`）；配置与启停（`config`、`reload`、`enable`、`disable`、`rm`）

**CLI 约定**：所有命令均支持 `--json` 输出；正常结果输出至 `stdout`，错误信息输出至 `stderr`，二者严格分离。不带子命令执行（如 `lax`、`lax model`、`lax plugin`）等同于 `--help`，输出帮助至 `stdout` 并以 `0` 退出。退出码：`0` 成功、`1` 运行失败、`2` 用法错误。完整参数说明见 `lax <命令> --help` 或[命令行参考](https://docs.larkax.com/docs/self-hosted/cli)。

---

## 数据与存储

| 路径 | 用途 |
| --- | --- |
| `$XDG_CONFIG_HOME/lax/config.json` | 监听地址、数据库路径、驱动后端与连接串 |
| `$XDG_DATA_HOME/lax/lax.db` | 账号、模型状态与请求记录（使用默认 SQLite 时） |
| `$XDG_DATA_HOME/lax/secret.key` | 凭据加密主密钥 |
| `$XDG_DATA_HOME/lax/lax.pid` | `serve` 运行时进程号文件，退出时自动清理 |
| `$XDG_DATA_HOME/lax/plugins/` | 已安装的插件包目录 |

- **数据库后端**：默认使用 SQLite 单文件；如需切换至 PostgreSQL 或 MySQL，修改配置文件中的 `driver` 与 `dsn` 两项即可，表结构通过 `AutoMigrate` 自动迁移，无需手工建表。
- **凭据安全**：上游密钥使用 `AES-256-GCM` 加密后落盘，旨在防范备份文件或日志导出导致的凭据泄漏，不防同一台主机上以相同用户身份运行的本地进程读取。

详情参见：[数据与备份](https://docs.larkax.com/docs/self-hosted/operate/data)。

---

## 已知边界

以下按模块列出当前版本的协议、运行时与功能边界，点击即可展开查看详情。

<details>
<summary><strong>Gemini 协议支持边界</strong></summary>

- 入站与出站均支持流式与非流式。
- 模型列表支持同步，按 `nextPageToken` 自动翻页。
- 模型名位于 URL 路径中，供应商路由配置支持 `{model}` 占位符。
- 非正常的 `finishReason`（如 `SAFETY`）会按正常结束状态转换给客户端。
- 暂不转换内置工具与思考（thinking）内容。

</details>

<details>
<summary><strong>跨协议转换边界</strong></summary>

- 仅映射五类内容块：文本、图片、思考内容、工具调用、工具结果。
- 思考内容仅在响应方向转换，请求方向不回填（回填会导致上游返回 `400` 拒绝整条请求）。
- 思考签名仅 `anthropic` 协议具备对应字段，因此 `openai-chat` 与 `openai-responses` 的思考内容转换至 `anthropic` 时会被丢弃。
- 工具结果后紧跟的追问会合并进同一条 `user` 消息，以适配 `anthropic` 不允许两条相邻 `user` 消息的约束。
- 暂不转换思考配置参数、`encrypted_content`、`tools[].strict`、音频与文件类内容。
- 流式转换在工具参数输出中途断开时，已下发至客户端的片段无法撤回，该次请求计为失败。

</details>

<details>
<summary><strong>请求头、冷却与计费边界</strong></summary>

- 网关不会修改任何外部客户端的配置文件。
- 暂不支持需要按请求动态计算签名的上游鉴权方式。
- `model probe` 探活调用不计入用量汇总。
- 估算花费需先配置模型定价；缓存读写单价未填时，回退使用输入单价计算。
- `usage` 与 `log tail` 中仍分别展示缓存读取与缓存写入的 token 数量。

</details>

<details>
<summary><strong>语音能力边界</strong></summary>

- 不做音频转码，也不解析音频容器以计算时长。
- 直通路径按入参字符数与音频秒数计费，`chat` 方言按 token 计费。
- 响应中的 `model` 字段保持上游原值，不改写为客户端请求别名。

</details>

<details>
<summary><strong>插件与方言运行时边界</strong></summary>

- 当前版本支持声明式插件、内置方言与 `moejs` 沙箱；暂不支持 `trusted-worker`、插件市场与签名校验、OAuth 登录。
- 沙箱无 `fetch`、`fs`、`process` 等全局对象，联网仅限使用 `lax.http.request` 访问白名单 HTTPS 地址，无媒体传输通道。
- 沙箱未设内存硬上限，受源码大小、返回值大小、注入数据与 `cache` 四项字节上限约束。
- 逐 hook 超时机制无法中断已在途的 `lax.http.request` 网络请求。
- ESM 模块仅支持静态 `export`、`export default` 与相对路径 `import`。

</details>

<details>
<summary><strong>账号额度监控边界</strong></summary>

- 目前仅支持内置的两种额度接口，具体取值详见 `lax quota set --help`。
- 其中一种额度接口依赖未公开路径（`/alpha/*`），路径与响应字段存在随上游变更的风险。
- 人民币等非 USD 币种折算 USD 无可靠默认汇率，必须手动配置 `--fx-rate`。
- 额度刷新失败仅更新快照状态，不会停用账号，也不写入请求记录。
- 额度刷新属于管理面查询而非转发请求，不计入 `usage` 与 `log`。

</details>

---

## 构建与发版

```bash
make check        # 执行 go vet + go test + go build
make build        # 编译生成带版本号的 ./lax 二进制文件
./lax version
```

- **构建依赖**：仅需 Go 工具链，无需 `cgo`，编译产物为独立单文件，不依赖任何外部运行时服务。
- **版本号推导**：`make build` 自动从最近的 `v*` Git Tag 推导版本号（例如 `0.1.0-3-gabc1234`）；直接执行 `go build` 得到的版本号为 `0.1.0-dev`。
- **预编译发布**：发布包位于 GitHub Releases，文件名形如 `lax_0.1.0_linux_amd64.tar.gz`，同页附带 `checksums.txt`。
- **平台限制**：Windows 不在发布支持范围内（`serve` 与 `plugin reload` 依赖 Unix 信号机制）。
- **发版规范**：发版流程与自动部署规则请参见 [AGENTS.md](AGENTS.md)。

---

## 延伸阅读

| 关注主题 | 对应文档 |
| --- | --- |
| 安装、配置与故障排查 | [自部署指南](https://docs.larkax.com/docs/self-hosted) |
| 使用 API Key 接入公开入口 | [公开入口](https://docs.larkax.com/docs/public) |
| Claude Code、Codex 与各语言 SDK 配置 | [客户端接入](https://docs.larkax.com/docs/public/clients) |
| 命令参数与退出码明细 | [命令行参考](https://docs.larkax.com/docs/self-hosted/cli) |
| 核心概念与术语速查 | [术语表](https://docs.larkax.com/docs/reference/glossary) |
| 常见报错与使用疑问 | [常见问题](https://docs.larkax.com/docs/self-hosted/faq) |
| 架构取舍与设计背景 | [DESIGN.md](DESIGN.md) |
