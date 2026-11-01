# lax 插件系统设计

> 状态：**已合并到 `DESIGN.md`**。本文是插件系统的设计提案存档，不再单独维护；两者冲突时以 `DESIGN.md` 为准。
>
> 已合并进 `DESIGN.md` 的结论：
>
> - 第 2 节范围表：插件运行时从非目标改成分阶段目标，写清本版落地的是声明式插件、内置方言与 `moejs` 沙箱。
> - 第 4 节：`plugin` 命令组的子命令、参数、行为、输出列与 `--json` 字段（`install` 的 `--trust` 判定表，`update`/`rollback`/`verify`/`logs`/`reload`/`config` 六个生命周期子命令）；`model capability add|set` 的 `--dialect` 与 `model dialect ls|set|unset`。
> - 5.7：模型目录的插件来源与按操作的方言绑定。
> - 5.8：`providers.plugin_id`、插件路由的建立方式与冲突规则。
> - 5.16：语音改成「操作 + 方言」——绑定来源、同协议直通与 chat 方言两条路、MiMo 迁移、启动回填、两种 400、声明式映射的边界；十条内置方言（四条语音加六条宿主线协议方言），对话上游靠引用宿主方言就能装起来。
> - 5.17：插件运行时——`moejs` JS 沙箱的执行模型（每次 prepare/decode 新建 Runtime、同一段流复用、超时与中断、受限 ESM、注入的 `lax` API 与不注入的全局、字节闸门）、对话与流式方言（`Plan.Body`/`Plan.JSON`/`Plan.Path`、`ResultJSON` 是出站形状、`StreamDialect` 事件表）、生命周期（版本指针、`reload` 与 `SIGHUP`、pid 文件）与审计/日志。
> - 第 6 节：`plugins`、`plugin_versions`、`plugin_options`、`model_operations`、`plugin_audit`、`plugin_logs` 六张表与 `providers.plugin_id`；建表仍只走 `AutoMigrate`，没有迁移文件与版本号。
> - 第 7 节：M17 与 M18 里程碑，以及「已知未做」的边界清单（含沙箱没有内存硬上限、中断打不断在途原生调用）。
> - 第 8 节：`internal/plugin` 的职责与边界（不读数据库、不读宿主配置与环境变量，只读插件包自己的源文件；网络只经沙箱的 `lax.http.request` 白名单出口；不 import `gateway`）。
> - 第 9 节：插件运行时不再是非目标；`moejs` 沙箱已落地（见 5.17），`trusted-worker` 与导入写成「未做」，不是「不做」。
>
> 没有落地、也没有写进 `DESIGN.md` 的部分：`trusted-worker`（独立进程与文件、进程、环境变量权限）、Magpie/OpenCode 导入与 `loader.fetch`（含 `plugin import` 子命令）、OAuth 登录与刷新、插件市场与签名校验（装进来的包一律 `unverified`）、沙箱运行时池（现在每次 prepare/decode 新建 Runtime，同一段流复用）、指标。这些属于后续阶段。对话、向量化与决策操作的方言绑定可以指向宿主方言，也可以指向 `moejs` 插件的 hook 方言；绑定到声明式方言仍会被安装拒掉，本文 §17.2 里那种给对话操作写声明式映射的例子还没有实现路径。
>
> 本文里的 `RequestPlan`、运行时池、指标与部分审计字段描述的是设计目标，不是现在的实现；现在的方言接口是 `plugin.Dialect`（`ID` / `Operation` / `UpstreamWire` / `Prepare` / `Decode`）加可选的 `plugin.StreamDialect`，请求计划叫 `plugin.Plan`（`Body` / `JSON` / `Path`），`Path` 只在非语音入站的插件方言上参与选址且必须是相对路径，审计与日志落在 `plugin_audit` / `plugin_logs` 两张表。

## 1 为什么要有插件

lax 现在把厂商差异写在 Go 代码里。每加一个上游，通常要改路由、模型目录、请求构造、响应解码、用量读取，有时还要改语音适配层。问题不在某一次实现，而在变化频率：

- 上游接口会变，客户端和 CLI 不应该跟着发版。
- 同类上游的差异很小，却容易在核心代码里长出厂商分支。
- 语音、向量化、图像、视频、决策等操作族的生命周期不同，不能都塞进对话协议。
- 第三方插件有供应链风险，不能默认获得凭据、文件系统和任意网络。

插件系统要解决的是“新增上游和新增方言不再改核心”，不是“允许任意代码接管进程”。这两件事必须分开。

## 2 目标与非目标

### 2.1 目标

- 新增 OpenAI-compatible 上游只需要声明式配置，不重新编译 lax。
- 新增复杂上游只需要实现语义操作钩子，不改网关、选号、计费、日志和错误形状。
- 插件能声明模型、操作、能力、方言、认证方式和限制。
- 宿主始终掌握账号、凭据、网络、重试、冷却、请求记录、计费和客户端错误格式。
- 默认运行时能限制内存、执行时间、动态代码和网络目标。
- 方言可以独立演进，破坏性上游变化不需要改核心 ABI。
- Magpie/OpenCode 插件可以按明确的子集导入，而不是承诺任意 Bun 插件原样运行。
- 现有内置 provider、账号和请求记录保持兼容；内置能力可以逐步迁移成内置插件。

### 2.2 非目标

- 多租户、配额、充值、网页后台。
- 插件直接读写 lax 数据库。
- 插件注册任意 HTTP 路径或替换网关入口。
- 插件自行决定最终 HTTP 状态码、错误 JSON、计费和 fallback。
- 默认执行 npm 安装脚本、`child_process`、任意文件系统或任意环境变量。
- 承诺“所有 Magpie、OpenCode、New API 插件原样可用”。
- 用插件实现逆向接口或绕过上游授权。

## 3 核心决策

| 决策 | 结论 |
| --- | --- |
| 插件边界 | 语义操作 Operation，不是 HTTP 路径或厂商 JSON |
| 线协议 | Wire Protocol 是宿主属性，不是插件操作名 |
| 方言 | Dialect 是操作与上游线协议之间的版本化描述，不是第二套执行层 |
| 数据面 I/O | 插件返回 `RequestPlan`，由宿主发送；插件不直接发上游请求 |
| 控制面 I/O | 模型列表、探活、刷新等通过宿主受限的 `lax.http` |
| 默认运行时 | `moejs` 沙箱；纯声明式插件不执行代码 |
| 信任级别 | `declarative`、`sandboxed`、`trusted-worker`，逐级显式授权 |
| 能力 | `true`、`false`、`unknown` 三态，不把省略当成支持或拒绝 |
| 错误 | 插件返回语义错误，宿主映射 HTTP 形状和重试策略 |
| 用量 | 插件只给事实，宿主按价格表计费 |
| 存储 | 插件元数据与选项入库；密钥继续走 `secret`；不把可执行代码放进配置目录 |
| 更新 | 默认不自动更新；安装、更新、回滚都校验 checksum 和权限变化 |
| Magpie | 以导入器支持声明式/沙箱子集，不以运行时兼容为目标 |

## 4 术语

| 术语 | 含义 |
| --- | --- |
| Operation | 语义动作，例如 `chat.complete@1`、`speech.synthesize@1` |
| Wire Protocol | 客户端或上游使用的报文协议，例如 `openai-chat`、`anthropic-messages` |
| Dialect | 某个 provider 对某个 operation 的上游实现形状 |
| Provider | 厂商或服务，例如 `mimo`、`river` |
| Model | provider 下的模型条目，例如 `mimo/mimo-v2.5-tts` |
| Account | 宿主保存的凭据和选号状态 |
| Plugin | 可安装的声明或代码包 |
| Host | lax 进程，拥有 I/O、数据库、凭据和计费 |
| Hook | 插件暴露给宿主的语义函数 |
| RequestPlan | 插件描述的一次上游请求，由宿主执行 |
| MediaRef | 宿主持有的音频、图片或文件句柄，插件通过受限 API 访问 |

## 5 分层架构

```text
客户端
  │  wire protocol
  ▼
┌──────────────────────────────────────────────────────────────┐
│ lax gateway                                                  │
│                                                              │
│  ingress adapter                                             │
│    /v1/chat/completions, /v1/messages, /v1/responses,        │
│    /v1/audio/speech, /v1/audio/transcriptions, ...            │
│                           │                                  │
│                           ▼                                  │
│  canonical operation request                                 │
│    chat.complete@1, speech.synthesize@1, ...                  │
│                           │                                  │
│  router: model -> provider -> account -> capability           │
│                           │                                  │
│  plugin host: runtime pool                                   │
│    describe / models / prepare_request                       │
│    decode_response / decode_stream / probe                   │
│                           │                                  │
│  host transport                                              │
│    auth injection, proxy, TLS, DNS policy, redirects,        │
│    timeout, request/response limits, retry, fallback          │
│                           │                                  │
│  usage recorder / error mapper / request log                 │
└───────────────────────────┬──────────────────────────────────┘
                            │ upstream request
                            ▼
                上游厂商 API
```

关键点：

- 插件不解析客户端协议，宿主负责 ingress/egress 编解码。
- 插件不理解账号池，只收到当前这次尝试的账号上下文。
- 插件不发送数据面请求，只生成 `RequestPlan`。
- 插件不决定最终错误形状，只返回语义错误。
- 插件不计算价格，只返回用量事实。

## 6 信任模型

### 6.1 插件类型

| 类型 | 是否有代码 | 能力 | 适用场景 |
| --- | --- | --- | --- |
| `declarative` | 否 | 声明 provider、路由、模型、方言、鉴权模板 | OpenAI-compatible、固定 JSON 上游 |
| `provider` | 可选 | 静态声明，或实现 operation hooks | 声明式兼容上游、需要协议转换或签名的上游 |
| `transform` | 是 | 修改规范请求、结果、流事件、用量观察 | 参数覆盖、系统提示词、模型映射 |
| `auth` | 是 | 登录、刷新、探测 | OAuth、设备码、复杂 token 刷新 |
| `trusted-worker` | 是，独立进程 | 文件、进程、环境变量、任意网络 | 必须复用厂商 CLI 或桌面客户端凭据 |

### 6.2 信任级别

| 级别 | 运行时 | 默认 | 说明 |
| --- | --- | --- | --- |
| `declarative` | 无代码 | 最安全 | 只读 manifest，不执行 JS |
| `sandboxed` | `moejs` | 第三方插件默认 | 纯计算、宿主代理 HTTP、受限媒体访问 |
| `trusted-worker` | 独立进程或 Bun | 显式授权 | 可访问文件、进程、环境变量；必须单独提示风险 |
| `system` | lax 内置 | 不通过插件安装 | 与内置 provider 同级 |

规则：

- 安装 `sandboxed` 插件时显示它声明的网络目标、操作、账号能力。
- 安装 `trusted-worker` 时必须显式 `--trust trusted-worker`，并显示完整风险提示。
- 插件升级如果扩大权限，必须重新确认，不能静默继承旧授权。
- 插件不能通过 `describe()`、`models()` 或运行时返回值扩大 manifest 权限。
- 没有签名或 checksum 的包可以安装，但必须标记 `unverified`，不能自动更新。

## 7 插件包与 manifest

### 7.1 目录结构

```text
my-plugin/
  lax.plugin.json
  index.js
  models.json
  dialects.json
  README.md
  testdata/
    input/
    request/
    response/
    output/
```

`index.js` 可以是 `.mjs`、`.cjs`、`.ts`。`declarative` 插件不需要入口文件。

### 7.2 manifest

```json
{
  "schema": "lax.plugin/v1",
  "id": "com.example.mimo",
  "name": "Xiaomi MiMo",
  "version": "1.0.0",
  "kind": "provider",
  "runtime": "moejs",
  "entry": "./index.js",
  "abi": "lax.plugin/v1",
  "requires": [
    "operation:chat.complete@1",
    "operation:speech.synthesize@1",
    "operation:speech.transcribe@1"
  ],
  "permissions": {
    "network": {
      "mode": "host-proxy",
      "schemes": ["https"],
      "hosts": [
        "mimo-server-sgp.xiaomimimo.com",
        "platform.xiaomimimo.com"
      ],
      "redirects": "none",
      "max_control_calls": 8,
      "max_control_body_bytes": 1048576
    },
    "secrets": {
      "plugin": [],
      "account": ["api_key"],
      "mode": "host-inject"
    },
    "filesystem": "none",
    "process": "none",
    "env": "none",
    "media": {
      "read_bytes": 67108864,
      "write_bytes": 67108864
    }
  },
  "providers": [
    {
      "id": "mimo",
      "name": "Xiaomi MiMo",
      "catalog": "./models.json",
      "dialects": "./dialects.json"
    }
  ],
  "options": {
    "region": {
      "type": "string",
      "enum": ["SGP", "RU", "IN"],
      "default": "SGP"
    }
  }
}
```

### 7.3 manifest 规则

- `schema` 固定为 `lax.plugin/v1`。
- `id` 必须稳定且全局唯一，建议反向域名或 npm scope。
- `version` 是插件版本，不是上游模型版本。
- `entry` 必须是插件目录内的文件，不能是绝对路径，不能逃出插件根。
- `requires` 是宿主能力协商；宿主不支持时安装失败，不能等到请求时才报错。
- `providers[].id` 是 provider 命名空间；不能与已有内置 provider 静默冲突。
- `permissions.network.hosts` 必须是具体主机或受控通配符，不能只有 `*`。
- `permissions.network.redirects` 默认 `none`；允许重定向时必须限制同 host。
- `permissions.filesystem`、`process`、`env` 在 `sandboxed` 下必须是 `none`。
- `permissions.secrets.mode` 默认 `host-inject`；插件看不到原始账号 key。
- manifest 不能声明运行时才出现的 provider、operation 或 dialect 权限。
- manifest 不接受未知顶层字段；未知扩展放入 `extensions`，由宿主显式忽略或拒绝。

### 7.4 插件 ID、provider ID 与模型 ID

三者的职责不同：

| 名称 | 例子 | 唯一性 |
| --- | --- | --- |
| Plugin ID | `com.example.mimo` | 全局 |
| Provider ID | `mimo` | lax 内唯一 |
| Model ID | `mimo/mimo-v2.5-tts` | lax 内唯一 |
| Upstream Model ID | `mimo-v2.5-tts` | provider 内唯一 |
| Dialect ID | `com.example.mimo/mimo-chat-speech@1` | 全局 |

模型在客户端侧统一使用 `provider/model`。插件内部只处理 `model.upstream_id`，不拼接客户端模型名。

## 8 ABI 版本

### 8.1 插件 ABI

`lax.plugin/v1` 定义：

- manifest 字段。
- `describe`、`models`、`prepare_request`、`decode_response`、`decode_stream`、`probe`、`refresh_auth` 等钩子名。
- `RequestPlan`、`OperationResult`、`StreamEvent`、`Error`、`Usage` 的形状。
- `ctx` 的只读字段和宿主能力。

规则：

- 插件 manifest 的 `abi` 必须精确匹配宿主支持的主版本。
- 宿主可以接受同一主版本内的新增可选字段。
- 插件不能依赖未知字段；未知字段由宿主拒绝或放入 `extensions`。
- 删除字段、改变字段语义、改变二进制约定必须升主版本。
- 插件 ABI 与 operation ABI 独立升版。

### 8.2 Operation ABI

操作名格式：

```text
<namespace>.<verb>@<major>
```

第一版操作：

```text
chat.complete@1
embeddings.create@1
speech.synthesize@1
speech.transcribe@1
speech.translate@1
rerank@1
image.generate@1
image.edit@1
video.generate@1
video.status@1
video.content@1
system.decide@1
```

不是所有操作都必须在第一版实现。先固定命名和版本，避免以后把厂商路径写进 ABI。

规则：

- operation 名称不包含 `openai`、`anthropic`、`gemini` 这类线协议名。
- `speech.synthesize@1` 与 `openai-speech` 不是同义词。
- 新增可选输入字段是次版本兼容；改变必填字段、结果类型或语义必须升主版本。
- 同一次请求只属于一个 operation。
- 一个模型可以声明多个 operation。

## 9 运行时

### 9.1 默认：moejs 沙箱

`sandboxed` 插件运行在进程内 `moejs`：

- 模块只编译一次。
- 每个并发请求使用独立 `Runtime`。
- 共享模块不可变，运行时通过池复用。
- 运行时用后清理调用数据，不能把请求对象留在全局。
- 默认冻结内置对象，关闭可变 intrinsics。
- 关闭 `eval`、`Function` 构造器和动态代码。
- 静态 import 只允许插件目录内的相对文件；npm 依赖必须预先打包。
- 不注入 `fetch`、`process`、`require`、`fs`、`Bun`、`child_process`。
- 只注入 `lax`、`console`、`URL`、`TextEncoder` 等宿主允许的全局。
- `console.log` 进入宿主日志，不写 stdout。
- `async`/`await` 允许；等待宿主异步 API 时由事件循环驱动。

### 9.2 资源限制

| 项目 | 默认值 | 上限 | 说明 |
| --- | --- | --- | --- |
| 插件 JS 堆 | 16 MiB | 256 MiB | 媒体走 MediaRef，不默认把大文件放进 JS 堆 |
| `prepare_request` | 100 ms | 2 s | 纯转换 |
| `decode_response` | 2 s | 30 s | 大 JSON 可配置 |
| `decode_stream` 单块 | 100 ms | 2 s | 每块独立计时 |
| `models` | 10 s | 30 s | 控制面 |
| `probe` | 10 s | 30 s | 控制面 |
| `refresh_auth` | 30 s | 60 s | 控制面 |
| 每插件并发 Runtime | 8 | 64 | 与上游并发分开 |
| 单次控制面 HTTP | 15 s | 30 s | 默认最多 4 次调用 |
| 控制面响应体 | 1 MiB | 16 MiB | 模型列表可单独配置 |
| 数据面响应体 | 8 MiB | 64 MiB | 语音/图像可单独配置 |

超时、内存超限、动态代码违规都会中断 Runtime。中断后的实例丢弃，不回到池里。

### 9.3 运行时池

`moejs` 的 Runtime 不是并发安全的，不能多个 goroutine 同时调用。插件宿主使用池：

```text
acquire -> call hook -> release/releaseCallData -> pool
                      \-> error/timeout/memory -> close
```

规则：

- 同一个 operation 的 hook 调用串行。
- 不同 operation 可以使用不同 Runtime 并发。
- 插件模块级变量只能在单次 Runtime 内共享，不能假设跨请求持久。
- 需要缓存的插件使用宿主 `lax.cache`，不能把请求数据留在模块全局。
- 插件崩溃或连续失败达到阈值后标记 `unhealthy`，停止路由并需要 `lax plugin doctor`。

### 9.4 trusted-worker

只有以下情况才允许 `trusted-worker`：

- 需要复用厂商 CLI。
- 需要读取桌面客户端保存的凭据。
- 需要 `child_process`、`node:http2`、`node:crypto` 等沙箱没有的模块。
- 需要浏览器或本地回调服务器完成 OAuth。

约束：

- 独立进程、独立工作目录、最小环境变量。
- 只注入当前账号的凭据，不注入整个数据库。
- 仍然经过宿主 HTTP 代理和日志策略。
- 每插件单独开关，默认关闭。
- 安装时必须显示“可访问文件系统、进程和任意网络”的风险提示。

## 10 插件钩子

### 10.1 provider 钩子

| 钩子 | 必需 | 作用 |
| --- | --- | --- |
| `describe(ctx)` | 否 | 动态声明 provider、模型、操作、方言、能力和选项；静态 manifest/catalog 已提供时不需要 |
| `models(ctx)` | 否 | 拉取账号可用模型 |
| `prepare_request(ctx)` | 是 | 语义输入转 `RequestPlan` |
| `decode_response(ctx)` | 是 | 非流式上游响应转 `OperationResult` |
| `decode_stream(ctx)` | 流式必需 | 上游块转 `StreamEvent[]` |
| `probe(ctx)` | 否 | 按 operation 做可达性和凭据探活 |
| `refresh_auth(ctx)` | OAuth 必需 | 刷新 token，宿主保存 |
| `headers(ctx)` | 否 | 生成动态附加头 |
| `sign_request(ctx)` | 否 | 对 `RequestPlan` 做签名，需显式 secret 权限 |
| `map_error(ctx)` | 否 | 上游错误转语义错误 |
| `map_usage(ctx)` | 否 | 上游用量转规范用量事实 |

### 10.2 transform 钩子

| 钩子 | 作用 |
| --- | --- |
| `on_request(ctx)` | 修改规范请求 |
| `on_response(ctx)` | 修改规范结果 |
| `on_event(ctx)` | 修改流式事件 |
| `on_usage(ctx)` | 只读观察用量行 |

`transform` 只能处理语义层，不能直接改上游线协议，也不能改模型、账号和计费口径。

### 10.3 调用顺序

一次非流式请求：

```text
resolve model
  -> select provider
  -> select account
  -> select dialect
  -> transform.on_request
  -> plugin.prepare_request
  -> host.send(RequestPlan)
  -> plugin.decode_response
  -> transform.on_response
  -> host.record_usage
  -> host.encode_response
```

一次流式请求：

```text
resolve model
  -> select account
  -> select dialect
  -> transform.on_request
  -> plugin.prepare_request
  -> host.open_stream(RequestPlan)
  -> for each chunk:
       plugin.decode_stream
       -> transform.on_event
       -> host.encode_event
  -> host.finish_stream
```

账号 fallback 时，`prepare_request` 会以新账号重新调用；`decode_response` 不会跨账号复用。

### 10.4 `ctx` 形状

```json
{
  "abi": "lax.plugin/v1",
  "operation": "speech.synthesize@1",
  "request_id": "01J...",
  "provider": {
    "id": "mimo",
    "plugin_id": "com.example.mimo"
  },
  "account": {
    "id": 12,
    "name": "mimo-1",
    "auth_kind": "api-key",
    "expires_at": null
  },
  "model": {
    "id": "mimo/mimo-v2.5-tts",
    "upstream_id": "mimo-v2.5-tts",
    "operations": {},
    "limits": {}
  },
  "dialect": {
    "id": "com.example.mimo/mimo-chat-speech@1",
    "transport": "json",
    "streaming": false
  },
  "input": {
    "text": "老大! 我们完成了 mimo 的语音模型接入啦!",
    "voice": "冰糖",
    "format": "mp3",
    "speed": 1.2,
    "instructions": "轻快上扬，语速稍快",
    "language": "zh-CN"
  },
  "options": {
    "region": "SGP"
  },
  "limits": {
    "deadline_ms": 30000,
    "max_input_bytes": 33554432,
    "max_output_bytes": 33554432,
    "max_media_bytes": 67108864
  },
  "metadata": {
    "locale": "zh-CN",
    "client": "lax-cli",
    "stream": false
  },
  "state": {}
}
```

规则：

- `ctx` 是只读输入；插件修改只影响当前调用。
- `ctx.account` 不含 `key`、`refresh_token`、`cookie` 等秘密。
- `ctx.model.operations` 已经过宿主校验，插件不能声明当前模型不支持的 operation。
- `ctx.state` 只用于同一请求的多个 hook 之间传递 JSON，不跨请求。
- `ctx.extensions` 可带宿主提供的非敏感扩展，插件不能依赖未知扩展。

### 10.5 `lax` 宿主对象

`sandboxed` 插件可用：

```text
lax.log(level, message)
lax.error(code, message, options?)
lax.now()
lax.randomUUID()
lax.base64.encode(value)
lax.base64.decode(value)
lax.sha256(value)
lax.hmacSHA256(value, secret)
lax.http.request(descriptor)
lax.media.stat(ref)
lax.media.readBase64(ref, options)
lax.media.readText(ref, options)
lax.media.create(bytes_base64, mime)
lax.cache.get(namespace, key)
lax.cache.set(namespace, key, value, ttl_ms)
lax.cache.delete(namespace, key)
```

不提供：

```text
fetch
process
require
module
fs
Bun
child_process
WebSocket
XMLHttpRequest
```

`lax.http.request` 是控制面 API，不是数据面。它：

- 只允许 manifest 中声明的主机和 scheme。
- 默认不跟随重定向。
- 每次调用最多 15 秒，单次响应最多 1 MiB。
- 每次 hook 最多 4 次调用，总计最多 30 秒。
- 不发送宿主账号凭据，除非 payload 显式包含且该 secret 被授权。
- 返回 `{status, headers, body}`，不抛 HTTP 错误状态。

`lax.media` 用于避免大音频在 JS 堆里反复复制。读取必须声明上限，超过上限直接报错。

`lax.error` 返回一个带语义错误码的异常，宿主会按 §15 校验并映射。插件也可以从 hook 返回 `{error: ...}`；直接抛出普通 `Error` 时宿主只能记为 `plugin_error`，不会猜测错误码。

### 10.6 事件与日志

- `lax.log` 只接受 `debug`、`info`、`warn`、`error`。
- 每条日志截断到合理长度，默认每分钟限流。
- 日志不记录密钥、cookie、完整音频、完整请求体。
- 插件异常由宿主记录 `plugin_id`、`hook`、`operation`、`request_id` 和安全摘要。
- 插件不能写 stdout；写 stdout 会被重定向到日志，避免破坏宿主协议。

## 11 RequestPlan

### 11.1 形状

`prepare_request` 返回一次上游请求的描述，不直接发送：

```json
{
  "method": "POST",
  "path": "/api/route",
  "headers": {
    "content-type": "application/json"
  },
  "body": {
    "kind": "json",
    "value": {
      "model": "mimo-v2.5-tts",
      "messages": [
        {"role": "user", "content": "轻快上扬，语速稍快"},
        {"role": "assistant", "content": "老大! 我们完成了 mimo 的语音模型接入啦!"}
      ],
      "audio": {
        "voice": "冰糖",
        "format": "mp3"
      }
    }
  },
  "auth": {
    "kind": "host"
  },
  "dialect": "com.example.mimo/mimo-chat-speech@1",
  "response": {
    "mode": "buffered",
    "max_bytes": 33554432
  },
  "timeout_ms": 30000,
  "idempotent": false,
  "extensions": {}
}
```

### 11.2 字段

| 字段 | 必需 | 说明 |
| --- | --- | --- |
| `method` | 是 | `GET`、`POST`、`PUT`、`PATCH`、`DELETE`、`HEAD` |
| `path` | 二选一 | 相对 provider 路由的路径，可以带 query |
| `url` | 二选一 | 完整地址，必须命中 `permissions.network.hosts` |
| `headers` | 否 | 非敏感附加头；宿主会覆盖禁止头 |
| `body` | 否 | 请求体，见下表 |
| `auth` | 是 | 宿主如何注入凭据 |
| `dialect` | 是 | 当前请求使用的方言 ID |
| `response` | 否 | 宿主如何读取响应 |
| `timeout_ms` | 否 | 建议超时；宿主会夹到上限 |
| `idempotent` | 否 | 是否允许宿主在安全条件下重试 |
| `extensions` | 否 | 宿主保留的诊断字段，不发给上游、不返回客户端 |

已落地的 ABI（`lax.plugin/v1`）只实现了其中一部分：`method` 没有对应字段（给了只打一行警告），`headers` 生效但有上限与保留头（键小写、最多 32 条、单值 4096 字节、总计 16 KiB；`authorization`、`x-api-key`、`x-goog-api-key`、`host`、`content-length`、`cookie`、`x-lax-session` 与 `proxy-*` 直接拒），`url`、`auth`、`response`、`timeout_ms`、`idempotent`、`extensions` 均不支持：地址与凭据只由厂商路由与 `providers[].auth` 决定，见 `DESIGN.md` 5.8 与 5.17。

### 11.3 body 类型

| `kind` | 形状 | 用途 |
| --- | --- | --- |
| `json` | `{value: object}` | 大多数 JSON API |
| `text` | `{value: string, content_type?: string}` | 文本请求体 |
| `form` | `{value: object}` | `application/x-www-form-urlencoded` |
| `multipart` | `{parts: Part[]}` | ASR 文件上传、混合字段 |
| `bytes` | `{base64: string, content_type: string}` | 二进制请求体 |
| `media` | `{ref: string}` | 直接引用宿主 MediaRef，避免复制 |
| `none` | 无 | GET/HEAD 或无请求体 |

`multipart` 的 `Part`：

```json
{
  "name": "file",
  "kind": "media",
  "ref": "media_123",
  "filename": "audio.wav",
  "mime": "audio/wav"
}
```

也支持 `kind: "text"` 和 `kind: "json"`。宿主负责生成 boundary，插件不能自己拼 multipart 字节。

### 11.4 auth 类型

| `kind` | 行为 |
| --- | --- |
| `host` | 宿主按 provider/account 配置注入 |
| `none` | 不注入凭据 |
| `bearer` | 宿主注入 `Authorization: Bearer <account key>` |
| `header` | 宿主注入指定头，`name` 必填 |
| `query` | 宿主注入指定 query 参数，`name` 必填 |
| `plugin` | 插件已经提供完整凭据头；只允许 `trusted-worker` 或显式 secret 权限 |

默认使用 `host`。插件不应知道账号 key 的原文。

### 11.5 宿主校验

宿主在执行 `RequestPlan` 前必须做：

- `path` 与 `url` 只能有一个。
- `path` 必须以 `/` 开头，不能包含 `..` 逃逸。
- `url` 必须是绝对 `https` 地址；只有显式允许的 loopback 可以用 `http`。
- `url` 不能有 userinfo、fragment。
- host 必须精确命中 manifest 白名单。
- 解析后的 IP 不能是环回、私有、链路本地、组播或未指定地址，除非用户显式允许。
- 禁止 `Host`、`Content-Length`、`Transfer-Encoding`、`Connection`、`Proxy-Connection`、`Keep-Alive`、`TE`、`Trailer`、`Upgrade`。
- 头名和头值不能含 CR/LF/NUL。
- 请求体不超过 operation 限制。
- `response.max_bytes` 不超过宿主上限。
- 重定向默认关闭；允许时只能同 host、最多一次、不能降级到私网。
- 代理、TLS 校验、DNS 固定、超时和取消全部由宿主控制。

不满足校验时返回 `plugin_plan_invalid`，不发送任何上游请求。

### 11.6 provider 路由

`path` 的 base 按以下顺序选择：

1. `provider_routes` 中 `(provider, operation, dialect)` 的地址。
2. provider 的 `base_url`。
3. 插件 manifest 中该 provider 的 `base_url`。

如果三者都没有，`path` 无效，返回配置错误。插件不能通过 `path` 改变 provider base，除非 manifest 明确允许。

## 12 认证与账号

### 12.1 第一版认证

| 类型 | 说明 |
| --- | --- |
| `api-key` | 宿主保存 key，按 `RequestPlan.auth` 注入 |
| `bearer` | 等价于 API key + `Authorization: Bearer` |
| `custom-header` | 宿主保存 key，按指定头注入 |
| `basic` | 宿主保存用户名/密码，按 Basic 注入 |
| `none` | 无凭据，仅本地或公开端点 |

后续增加：

- `oauth-device`
- `oauth-browser`
- `refresh-token`
- `plugin-signature`

### 12.2 账号模型

```text
account
  id
  name
  provider_id
  auth_kind
  auth_cipher
  priority
  enabled
  cooldown_until
  fail_streak
  last_error
  created_at
```

`auth_cipher` 由 `internal/secret` 加密。`plugin` 不直接读数据库，也不持有解密后的全局对象。

### 12.3 OAuth 与刷新

- 宿主保存 `refresh_token`、`access_token`、`expires_at`，插件看不到完整存储。
- 到期前宿主调用 `refresh_auth(ctx)`，传入当前 token 的非敏感元数据和需要刷新的 secret。
- 插件返回新的 `auth` 对象，宿主加密保存。
- 刷新失败分为 `auth_expired`、`auth_invalid`、`auth_unavailable`。
- 刷新不能在 `prepare_request` 里偷偷做；必须走独立 hook，便于加锁和审计。
- 同一账号的刷新串行化，避免并发覆盖。

### 12.4 自定义签名

需要签名的插件有两种方式：

1. **宿主签名模板**：manifest 声明 header/query 模板，宿主用账号 key 计算。适用于简单 HMAC、Bearer、固定头。
2. **`sign_request(ctx)`**：插件拿到 `RequestPlan` 和受限 secret，返回新的 headers/query。只允许 `trusted-worker` 或 `permissions.secrets.mode = "plugin"` 的 `sandboxed` 插件。

`sign_request` 不能改 URL host、method 和 body，只能改签名相关字段。宿主仍然做最终校验。

## 13 结果与媒体

### 13.1 OperationResult

```json
{
  "result": {
    "kind": "media",
    "mime": "audio/mpeg",
    "body_base64": "SUQz..."
  },
  "usage": {
    "input_tokens": 125,
    "output_tokens": 7,
    "cached_input_tokens": 121,
    "tts_characters": 23,
    "source": "upstream"
  },
  "finish_reason": "stop",
  "extensions": {}
}
```

`result.kind`：

| 类型 | 用途 |
| --- | --- |
| `text` | 纯文本结果 |
| `json` | 结构化结果 |
| `media` | 音频、图片、视频、文件 |
| `embedding` | 向量 |
| `rerank` | 重排结果 |
| `decision` | 决策结果 |
| `error` | 插件明确返回的语义错误 |
| `none` | 无结果，只有 usage 或状态 |

### 13.2 media

媒体结果有两种：

```json
{
  "kind": "media",
  "mime": "audio/mpeg",
  "ref": "media_456"
}
```

```json
{
  "kind": "media",
  "mime": "audio/mpeg",
  "body_base64": "SUQz..."
}
```

优先使用 `ref`：

- 上游二进制响应由宿主直接持有 `ctx.response.media.ref`，插件可以原样返回。
- 插件生成大媒体时先 `lax.media.create`，再返回 `ref`。
- base64 只用于小的、无法避免的 JSON 内嵌媒体。
- 宿主对 media 做大小、MIME、生命周期和清理管理。
- media 默认只在当前请求内有效，响应写完后删除或按 TTL 清理。

### 13.3 非流式响应上下文

`decode_response` 拿到：

```json
{
  "status": 200,
  "headers": {
    "content-type": "application/json"
  },
  "body": {},
  "text": "",
  "media": null,
  "request": {
    "method": "POST",
    "url": "https://api.example.com/v1/audio/speech"
  }
}
```

规则：

- `body` 只在响应是 JSON 且未超过上限时提供。
- `text` 只在响应是文本且未超过上限时提供。
- `media` 在响应是二进制时提供 `{ref, mime, size}`。
- 插件不能用 `ctx.response` 直接写客户端。
- 插件不能修改 `status` 或 `headers` 来伪造最终响应；只能映射成语义结果或错误。

## 14 流式 ABI

### 14.1 输入

宿主逐块调用 `decode_stream`：

```json
{
  "seq": 12,
  "encoding": "utf8",
  "data": "data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n",
  "final": false,
  "state": {
    "buffer": ""
  }
}
```

二进制块用 `encoding: "base64"` 和 `data` 传 base64。

规则：

- 块边界是任意的，插件必须自己处理半行、半帧、多帧。
- `state` 是 JSON，宿主保存并在下一块原样传回。
- `state` 大小默认 64 KiB，深度默认 32。
- `final: true` 表示上游读取结束，插件仍可返回最后的事件。
- 单块超时或内存超限时宿主中断并结束流。
- 同一流不会并发调用 `decode_stream`。

### 14.2 输出

```json
{
  "events": [
    {"type": "text.delta", "text": "你"},
    {"type": "usage", "usage": {"input_tokens": 10, "output_tokens": 1}}
  ],
  "state": {
    "buffer": ""
  },
  "done": false
}
```

事件类型：

| 类型 | 字段 | 用途 |
| --- | --- | --- |
| `text.delta` | `text` | 正文增量 |
| `reasoning.delta` | `text` | 思考增量 |
| `tool_call.start` | `id`、`name` | 工具调用开始 |
| `tool_call.delta` | `id`、`arguments_delta` | 工具参数增量 |
| `tool_call.end` | `id` | 工具调用结束 |
| `audio.delta` | `data_base64`、`mime` | 音频增量 |
| `transcript.segment` | `text`、`start_ms`、`end_ms`、`speaker?` | 转写分段 |
| `usage` | `usage` | 用量事实 |
| `finish` | `reason` | 结束原因 |
| `error` | `error` | 语义错误 |
| `extension` | `name`、`data` | 插件扩展事件 |

规则：

- `events` 默认每次最多 32 条，总大小最多 64 KiB。
- 事件中的二进制只允许 `audio.delta`，且受 `max_media_bytes` 限制。
- `done: true` 表示插件认为流已经完整结束，宿主停止读取上游并取消剩余读取。
- 插件不能只发 `done` 而不发结束事件；宿主会按协议补结束帧。
- 客户端断开时宿主取消上游并中断当前 hook；插件不能继续写状态。

### 14.3 宿主编码

宿主把语义事件编码成客户端入站协议的 SSE、JSONL 或字节流：

- `openai-chat`：`chat.completion.chunk`。
- `openai-responses`：`response.*` 事件。
- `anthropic-messages`：`message_start`、`content_block_delta`、`message_stop`。
- `gemini`：`candidates[].content.parts[]`。
- `openai-speech`：音频字节或 `speech.audio.delta`/`speech.audio.done`。
- `openai-transcriptions`：文本、JSON 或 SSE 转写事件。

插件不生成这些线协议帧，只生成语义事件。

## 15 错误模型

### 15.1 形状

```json
{
  "error": {
    "code": "upstream_rate_limited",
    "message": "上游限流，稍后重试",
    "retryable": true,
    "retry_after_ms": 3000,
    "origin": "upstream",
    "details": {}
  }
}
```

### 15.2 错误码

| code | 默认 HTTP | 可重试 | 说明 |
| --- | --- | --- | --- |
| `invalid_input` | 400 | 否 | 客户端参数错 |
| `unsupported_operation` | 400 | 否 | 模型不支持该 operation |
| `unsupported_dialect` | 400 | 否 | 插件不支持该方言 |
| `model_not_found` | 404 | 否 | 模型不存在 |
| `model_forbidden` | 403 | 否 | 账号无权使用该模型 |
| `auth_invalid` | 401 | 否 | 凭据无效 |
| `auth_expired` | 401 | 否 | 凭据过期，可刷新 |
| `rate_limited` | 429 | 是 | 上游限流 |
| `quota_exhausted` | 429 或 402 | 否 | 配额耗尽 |
| `upstream_unavailable` | 502 或 503 | 是 | 上游不可用 |
| `upstream_timeout` | 504 | 是 | 上游超时 |
| `upstream_protocol_error` | 502 | 否 | 上游返回无法解析的报文 |
| `plugin_error` | 502 | 否 | 插件异常或非法返回 |
| `plugin_timeout` | 504 | 否 | 插件 hook 超时 |
| `plugin_plan_invalid` | 500 | 否 | RequestPlan 未通过宿主校验 |
| `cancelled` | 499 | 否 | 客户端取消 |
| `internal` | 500 | 否 | 宿主内部错误 |

### 15.3 规则

- 插件不能直接指定最终 HTTP 状态码，只能返回语义错误。
- 插件用 `lax.error(code, message, options)` 或返回 `{error}` 表达语义错误；未知或缺失的 code 归一为 `plugin_error`。
- 插件可以给 `retryable` 和 `retry_after_ms` 作为提示，宿主决定是否重试。
- `auth_invalid`/`auth_expired` 可以触发账号标记或刷新，但由宿主决定。
- `plugin_error`、`plugin_timeout`、`plugin_plan_invalid` 默认不 fallback 到其他账号，避免把插件 bug 放大。
- `upstream_*`、`rate_limited` 可以按现有选号策略 fallback。
- 错误消息截断到 512 字符，不能包含密钥、cookie、完整响应体或音频内容。
- `details` 只进入受控日志，不直接返回客户端。

## 16 用量事实

### 16.1 规范字段

```json
{
  "input_tokens": 125,
  "uncached_input_tokens": 4,
  "cached_input_tokens": 121,
  "cache_write_tokens": 0,
  "output_tokens": 7,
  "reasoning_tokens": 0,
  "tts_characters": 23,
  "audio_seconds": 3.4,
  "image_count": 0,
  "video_seconds": 0,
  "source": "upstream"
}
```

规则：

- 所有数值必须有限、非负。
- `source` 是 `upstream`、`estimated`、`partial`、`none`。
- `uncached_input_tokens` 未给时，宿主按 `input_tokens - cached_input_tokens` 推导；结果不能为负。
- 插件不能给价格、金额或折扣。
- 插件不能覆盖宿主的计费口径；价格以 `model_prices`/provider 配置为准。
- TTS 字符数由宿主从输入推导；插件只在上游报告不同计数时覆盖。
- ASR 音频秒数由插件报告；宿主不解析音频容器。
- 用量事实超过边界时宿主拒绝或截断，并记录 `usage_invalid`。

### 16.2 来源与展示

- `upstream`：上游明确报告。
- `estimated`：插件按字符、时长或图片数估算。
- `partial`：流式提前结束，只有部分用量。
- `none`：没有可用用量。

请求记录保留原始来源，`usage` 汇总时按现有口径合并，不把估算值伪装成上游值。

## 17 方言设计

### 17.1 四个概念分开

插件系统里最容易混在一起的是 Operation、Wire Protocol、Dialect 和语言变体。它们回答的问题不同：

| 概念 | 回答的问题 | 例子 | 谁拥有 |
| --- | --- | --- | --- |
| Operation | 用户要做什么语义动作 | `speech.synthesize@1` | 宿主 ABI |
| Wire Protocol | 客户端或上游的报文长什么样 | `openai-chat`、`anthropic-messages` | 宿主编解码器 |
| Dialect | 某 provider 如何把某个 operation 映射到上游 | `com.example.mimo/mimo-chat-speech@1` | 插件 |
| 语言变体 | 这次请求使用哪种自然语言或地区变体 | `yue`、`zh-CN`、`en-US` | 模型能力 + 请求参数 |

因此：

- 不能把 `sound`、`tts`、`audio` 写成 operation；它们说的是报文或资源类型。
- 不能把 `zh-yue`、`粤语` 写成 dialect；它们是语言能力，不是协议形状。
- 不能因为 MiMo 的 TTS 走 `openai-chat`，就把 `speech.synthesize@1` 改名为 `chat.complete@1`。
- 不能因为某个语音上游换了 JSON 字段，就改核心 `speech_adapter.go`；应当发布新的 dialect 版本。

### 17.2 Dialect 的形状

Dialect 是 manifest 引用的声明文件，不是第二套运行时。它只描述：

- 这个 operation 使用哪种上游线协议和传输方式。
- 请求和响应由宿主原生处理，还是由插件 hook 处理。
- 该 dialect 的默认值、限制和兼容性要求。
- 声明式 dialect 的有限字段映射。

代码 dialect：

```json
{
  "schema": "lax.dialect/v1",
  "id": "com.example.mimo/mimo-chat-speech@1",
  "operation": "speech.synthesize@1",
  "display_name": "MiMo TTS over chat completions",
  "upstream_wire": "openai-chat",
  "transport": {
    "request": "json",
    "response": "json",
    "streaming": "none"
  },
  "implementation": {
    "prepare": "hook",
    "decode": "hook"
  },
  "defaults": {
    "voice": "冰糖"
  },
  "limits": {
    "max_input_chars": 8192,
    "max_response_bytes": 33554432
  },
  "extensions": {}
}
```

声明式 dialect：

```json
{
  "schema": "lax.dialect/v1",
  "id": "com.example.openai/speech@1",
  "operation": "speech.synthesize@1",
  "upstream_wire": "openai-speech",
  "transport": {
    "request": "json",
    "response": "binary",
    "streaming": "bytes"
  },
  "implementation": {
    "prepare": "declarative",
    "decode": "declarative"
  },
  "request": {
    "method": "POST",
    "path": "/v1/audio/speech",
    "body": {
      "model": {"model": "upstream_id"},
      "input": {"input": "text"},
      "voice": {"input": "voice", "default": "alloy"},
      "response_format": {"input": "format", "default": "mp3"},
      "speed": {"input": "speed", "omit_if_empty": true},
      "instructions": {"input": "instructions", "omit_if_empty": true}
    }
  },
  "response": {
    "mode": "passthrough",
    "mime": {"input": "format", "map": {"mp3": "audio/mpeg", "wav": "audio/wav", "pcm16": "audio/pcm"}}
  }
}
```

字段规则：

- `id` 全局唯一，建议 `<反向域名>/<名称>@<主版本>`。
- `operation` 必须精确匹配 Operation ABI。
- `upstream_wire` 只能取宿主认识的线协议；它用于默认头、流式解码和诊断，不决定客户端协议。
- `transport.streaming` 为 `none`、`bytes`、`sse` 或 `jsonl`。
- `implementation.prepare` 与 `implementation.decode` 为 `hook` 或 `declarative`；一个 dialect 可以请求走 hook、响应走声明式，反过来也可以。
- 同一个 dialect 只能属于一个 operation；同一个模型同一 operation 只能绑定一个 dialect。
- `extensions` 不参与执行；宿主不认识时只记录，不静默解释成安全能力。

### 17.3 声明式映射的边界

声明式映射是数据，不是一门脚本语言。只允许以下节点：

| 节点 | 语义 |
| --- | --- |
| `const` | 固定 JSON 值 |
| `input` | 从规范输入读取字段 |
| `model` | 读取 `model.upstream_id`、`model.id` 等只读字段 |
| `option` | 读取插件配置项 |
| `default` | 字段为空时用默认值 |
| `omit_if_empty` | 空字符串、空数组或 `null` 时省略该字段 |
| `map` | 固定枚举映射，例如 `mp3` → `audio/mpeg` |
| `array` / `object` | 固定结构里放上述节点 |

不允许：

- 任意表达式、正则替换、循环、递归、脚本片段。
- 读取环境变量、文件、网络或秘密原文。
- 根据账号 key、请求头或响应正文做分支。
- 在声明式映射里构造新的 URL host 或绕过 `RequestPlan` 校验。

超过这个边界的转换写成 `sandboxed` 插件的 hook。这样声明式插件始终可以静态审计，不需要为了“简单逻辑”引入不可控执行面。

响应侧声明式规则：

- `mode: "passthrough"`：二进制响应原样交给宿主，只校验 MIME 与大小。
- `mode: "json_pointer"`：按 RFC 6901 读取一个字段，配合 `encoding: "base64"`、`encoding: "text"` 或 `encoding: "json"`。
- `mode: "sse_json_pointer"`：只适用于简单 SSE，每帧取一个字段；复杂分帧、工具调用和多事件转换必须走 hook。
- `usage` 只能映射规范字段，不能映射价格。

### 17.4 模型与 operation 的绑定

模型目录里的每个 operation 显式绑定 dialect：

```json
{
  "provider": "mimo",
  "model": "mimo-v2.5-tts",
  "upstream_id": "mimo-v2.5-tts",
  "operations": {
    "speech.synthesize@1": {
      "dialect": "com.example.mimo/mimo-chat-speech@1",
      "capabilities": {
        "supported": true,
        "streaming": false,
        "formats": ["mp3", "wav", "pcm16"],
        "voices": ["冰糖", "茉莉", "苏打"]
      }
    }
  }
}
```

规则：

- 客户端模型名仍是 `provider/model`，插件只拿 `upstream_id`。
- operation 的必填输入由宿主按 Operation ABI 校验，插件不能重新定义一套请求 JSON。
- 一个模型可以同时绑定 `chat.complete@1`、`speech.synthesize@1` 和 `speech.transcribe@1`，但不能把三个 operation 混成一次请求。
- 同一模型同一 operation 出现两个绑定是安装错误；不允许“最后一个生效”。
- 模型别名只改变 `provider/model` 的解析结果，不改变 dialect 绑定。

### 17.5 内置 dialect 与插件 dialect

- `lax/` 前缀保留给宿主内置 dialect。
- 插件 dialect 使用自己的反向域名前缀，不能冒用 `lax/`。
- 安装时发现 dialect ID 冲突，整包拒绝；不能用插件覆盖内置 dialect。
- 模型没有显式绑定时，宿主只能使用内置的 OpenAI-compatible 默认 dialect，并且要求该 operation 的能力声明为 `true`。
- 模型显式绑定插件 dialect 时，插件绑定优先；`plugin inspect` 与 `model show` 必须显示最终选中的 dialect。
- 插件被停用或删除时，绑定它的模型变为不可用，不能静默回退到内置 dialect。静默回退会把一次配置删除变成一次上游形状变化，故障很难定位。

### 17.6 方言升级

上游发生破坏性变化时：

1. 新建 `...@2` dialect，保留 `...@1` 的旧行为。
2. 模型可以按账号或版本逐步切到 `@2`；切换前后都可回滚。
3. 只有确认所有绑定都不再使用 `@1` 后，才在插件的新版本里删除它。
4. 核心不需要为新上游发版，也不需要理解 `@2` 的字段名。

以下情况不需要升主版本：

- 新增可选字段。
- 新增默认值。
- 新增不改变语义的响应字段。
- 修复解析器 bug。

以下情况必须升主版本：

- 请求或响应的必填字段改变。
- 同一字段含义改变，例如 `input` 从正文变成指令。
- 流式事件边界或结束语义改变。
- 认证方式改变导致账号不能透明复用。

### 17.7 Dialect 不是第二套 hook 系统

Dialect 不注册新的 Go 接口，不引入独立生命周期，也不拥有自己的 secret 权限。它只是：

- 选择一次 operation 的上游形状。
- 告诉宿主该调用哪些现有 hook 或声明式映射。
- 给日志、探活、测试和 UI 一个稳定 ID。

因此同一个 provider 插件可以有多个 dialect，但它们共用 manifest 权限、运行时、日志和审计。若一个方言需要额外网络目标或额外秘密，那属于插件权限变化，需要重新确认，不是 dialect 自己能扩权的事。

## 18 模型能力

### 18.1 三态规则

能力值只有三种：

| 值 | 含义 | 宿主行为 |
| --- | --- | --- |
| `true` | 声明支持，且有依据 | 可进入该 operation 的候选、可出现在 `/v1/models` |
| `false` | 明确不支持 | 本地拒绝，不调用上游 |
| `"unknown"` | 没有可靠结论 | 不宣称支持；默认本地拒绝，导入器或用户显式允许时才尝试 |

省略字段表示“本次更新不修改已有结论”，不是 `false`，也不是 `true`。需要把已有结论重置为未知时，显式写 `"unknown"`。

三态必须覆盖所有布尔能力和枚举能力：

- `supported`、`streaming`、`tools`、`reasoning`、`json_mode`、`vision`、`timestamps`、`diarization`、`translation` 可以是 `true`、`false` 或 `"unknown"`。
- `formats`、`voices`、`languages` 可以是数组或 `"unknown"`。
- 未知不是空数组；空数组表示“明确一个都没有”，会拒绝所有值。

默认拒绝未知能力是故意的。把未知当成支持会把一次上游 400/403 变成网关的“模型不可用”，而用户看不到真正原因是插件没声明。`plugin import` 可以为已知的导入子集填入 `true`，但必须在 `plugin inspect` 里标出来源。

### 18.2 能力按 operation 声明

能力属于 `(provider, model, operation)`，不属于整个模型：

```json
{
  "operations": {
    "chat.complete@1": {
      "supported": true,
      "streaming": true,
      "tools": true,
      "reasoning": true,
      "modalities": {
        "input": ["text", "image"],
        "output": ["text"]
      },
      "formats": "unknown"
    },
    "speech.synthesize@1": {
      "supported": true,
      "streaming": false,
      "formats": ["mp3", "wav", "pcm16"],
      "voices": ["冰糖", "茉莉"],
      "languages": {
        "output": {
          "mode": "list",
          "tags": ["zh-CN", "yue"]
        }
      },
      "limits": {
        "max_input_chars": 8192
      }
    },
    "speech.transcribe@1": {
      "supported": true,
      "streaming": "unknown",
      "formats": ["wav", "mp3"],
      "languages": {
        "input": {
          "mode": "list",
          "tags": ["zh-CN", "yue"]
        },
        "auto_detect": true,
        "timestamps": "unknown",
        "diarization": "unknown"
      }
    }
  }
}
```

不能用一个全局 `supports_tools: true` 推断 TTS 或向量化也支持工具调用，也不能拿 chat 的格式列表校验音频。

### 18.3 声明、探活与推断分开

每个能力结论都要能回答“从哪里知道”：

| 来源 | 含义 | 是否可直接对外宣称 |
| --- | --- | --- |
| `declared` | 插件 manifest 或模型文件声明 | 可以，但标为声明 |
| `configured` | 用户手工配置 | 可以，标为配置 |
| `probed` | 宿主按 operation 探活得到 | 可以，带探活时间 |
| `inferred` | 从上游模型列表、协议或导入器推断 | 不默认宣称；必须显式启用 |
| `unknown` | 没有可靠结论 | 不可以 |

同一字段可能有多个来源。合并规则：

1. `false` 是硬拒绝，任何探活成功都不能自动翻成 `true`；必须由用户或插件更新声明。
2. `true` + 新鲜探活失败时，有效值为 `false`，状态记为 `conflict`，并保留两边证据。
3. `"unknown"` + 探活成功时可以变为 `true`，来源记为 `probed`。
4. 探活结论过期后回到声明值；声明也是未知时才回到 `"unknown"`。
5. 上游模型列表只能证明“模型名存在”，不能证明支持 TTS、ASR、工具或推理。

探活结果与声明值分表保存，不能把一次探活失败写成永久能力否定，也不能把一次成功写成永久承诺。

### 18.4 语言、地区与方言

自然语言能力必须独立于 wire dialect 建模。统一使用 BCP 47 标签：

- `zh-CN`：普通话，中国大陆习惯。
- `zh-TW`：普通话，台湾地区习惯。
- `yue`：粤语；兼容输入别名 `zh-yue`、`cantonese`、`粤语`，落库前归一成 `yue`。
- `wuu`：吴语；`nan`：闽南语；`hak`：客家话。
- `en-US`、`en-GB`、`ja-JP` 等按标准标签处理。

多语言字段：

```json
{
  "languages": {
    "input": {
      "mode": "list",
      "tags": ["zh-CN", "yue", "en-US"]
    },
    "output": {
      "mode": "list",
      "tags": ["zh-CN", "yue"]
    },
    "auto_detect": true,
    "translation": {
      "supported": true,
      "directions": [["zh-CN", "en-US"], ["yue", "zh-CN"]]
    },
    "aliases": {
      "zh-yue": "yue",
      "cantonese": "yue",
      "粤语": "yue"
    }
  }
}
```

规则：

- `input` 描述 ASR/翻译的输入语言，`output` 描述 TTS/翻译的输出语言；两者不能互相代替。
- `mode` 为 `list`、`any`、`auto` 或 `"unknown"`。`list` 必须给非空 `tags`；`any` 明确表示不限制，不能用空数组暗示。
- `auto_detect` 只表示上游能自动识别，不表示识别结果一定准确；探活要单独验。
- `translation.directions` 是可验证的方向对，不能用 `supported: true` 暗示任意语言对都可翻译。
- 音色可以带语言标签；同一音色名在不同地区可能是不同声音，插件必须声明映射。
- 客户端传 `language` 时，宿主先归一化标签，再匹配 `aliases` 与模型能力；不匹配就本地拒绝并列出可用标签。
- 不把“方言”强行塞进 `wire` 字段。未来出现脚本、口音、性别、年龄、情绪等维度时，放进 operation 的 `extensions` 或新能力字段，并升对应 ABI，不要在核心写厂商特例。

### 18.5 `/v1/models` 输出

模型清单必须包含非对话模型，并且不能把未知能力写成支持：

```json
{
  "id": "mimo/mimo-v2.5-tts",
  "object": "model",
  "owned_by": "mimo",
  "lax": {
    "operations": {
      "speech.synthesize@1": {
        "dialect": "com.example.mimo/mimo-chat-speech@1",
        "supported": true,
        "streaming": false,
        "formats": ["mp3", "wav", "pcm16"],
        "voices": ["冰糖", "茉莉"],
        "languages": {
          "output": {"mode": "list", "tags": ["zh-CN"]}
        },
        "source": "declared",
        "probed_at": null
      }
    }
  }
}
```

规则：

- `/v1/models` 包含 chat、embedding、rerank、TTS、ASR、图像和视频模型；是否可用按 operation 与账号可达性计算。
- 不因模型不是 chat 就隐藏；不能要求客户端猜 TTS 模型名。
- `supported: false` 的模型不进入自动路由；是否仍出现在清单里由配置决定，默认不出现。
- `"unknown"` 不进入自动路由，也不在清单里显示成可用；可以在 `plugin inspect` 与 `model show` 里看到原因。
- `source` 与 `probed_at` 必须可见，便于用户判断是声明还是实测。

### 18.6 探活按 operation 分派

探活不再默认发 chat 请求。宿主按 operation 选择探针：

| Operation | 探活方式 | 判定 |
| --- | --- | --- |
| `chat.complete@1` | 最小 chat 请求或已知的零费用错误请求 | 2xx、认证失败、模型不存在、限流分别记录 |
| `embeddings.create@1` | 最小向量化请求 | 维度与响应形状 |
| `speech.synthesize@1` | 错误结构或最小合成，按插件声明 | 能区分“模型不是 TTS”与“参数错” |
| `speech.transcribe@1` | 缺文件或最小音频 | 能区分“模型不是 ASR”与“文件格式错” |
| `image.generate@1` / `video.generate@1` | 插件提供的探测请求 | 只记录可达性，不把生成物当成功证明 |

探活结果：

- `supported`：operation 可用。
- `unsupported`：上游明确说该模型不支持这个 operation。
- `forbidden`：账号无权使用该模型。
- `auth_invalid` / `auth_expired`：凭据问题。
- `rate_limited`：暂时不可用，不改变长期能力。
- `transient`：网络或上游临时故障。
- `unknown`：无法判定，不能当成可用。

探活请求也要写入请求记录，并标记 `probe: true`。能避免计费就避免；无法避免时使用最小输入，并在 `plugin inspect` 里显示可能费用。

## 19 存储

### 19.1 设计原则

- 插件元数据入库；可执行包放在数据目录，不放配置目录。
- 插件不能直接连数据库，也不能定义表、迁移或 SQL。
- 账号凭据仍由 `internal/secret` 加解密；插件只拿宿主注入的凭据或受限签名能力。
- 插件配置、缓存、审计都只存 JSON 值，不存明文秘密。
- 数据库是活动版本的唯一依据；lockfile 只用于完整性校验和复现，不参与运行期决策。
- 具体迁移编号不在本文确定。落地时按合并后的 `DESIGN.md` 第 6 节统一处理；若项目保留 `AutoMigrate`，就只改 row 定义，不凭空增加迁移文件编号。

### 19.2 数据目录

```text
$XDG_DATA_HOME/lax/
  lax.db
  plugins/
    installed/
      com.example.mimo/
        1.0.0/
          lax.plugin.json
          index.js
          models.json
          dialects.json
    staging/
    quarantine/
    lock.json
```

规则：

- `installed/<plugin-id>/<version>/` 是不可变目录；安装完成后不再原地修改。
- 新版本先写入 `staging/`，校验、静态扫描、试运行通过后再原子移动。
- 解包必须拒绝绝对路径、`..`、符号链接逃逸、硬链接和超出大小上限的条目。
- `lock.json` 记录插件 ID、版本、包哈希、manifest 哈希、权限哈希、来源、信任级别和安装时间。
- 启动时校验活动版本与 lockfile；哈希不匹配就把插件标记为 `tampered`，不加载。
- 删除插件后包可以清理，但审计记录保留；`--purge` 才删除该插件的缓存、配置和独立 secret。

### 19.3 新表

以下为提案形状，字段名可在实现时按现有命名风格微调，但语义不应改变。

| 表 | 主键 | 关键字段 | 用途 |
| --- | --- | --- | --- |
| `plugins` | `id` | `active_version`、`kind`、`trust`、`enabled`、`state`、`last_loaded_at`、`last_error` | 插件当前状态与活动版本 |
| `plugin_versions` | `plugin_id, version` | `abi`、`runtime`、`manifest_json`、`permissions_json`、`package_sha256`、`manifest_sha256`、`source`、`signature_status`、`installed_at` | 每个已安装版本不可变元数据 |
| `plugin_options` | `plugin_id, key` | `value_json`、`updated_at` | 非敏感配置 |
| `plugin_audit` | `id` | `plugin_id`、`version`、`action`、`actor`、`at`、`permissions_before_json`、`permissions_after_json`、`result`、`error` | 安装、更新、授权、启停与回滚审计 |
| `plugin_cache` | `plugin_id, namespace, key` | `value_json`、`expires_at`、`updated_at` | 宿主托管的可选缓存，不能存秘密 |
| `plugin_secrets` | `plugin_id, name` | `cipher`、`created_at`、`updated_at` | 仅当插件需要独立 secret；密文由 `internal/secret` 处理 |
| `model_operations` | `provider, model, operation` | `dialect_id`、`capabilities_json`、`limits_json`、`source`、`updated_at` | 模型按 operation 的方言与能力 |
| `model_probes` | `account_id, model, operation` | `dialect_id`、`access`、`http_status`、`latency_ms`、`error`、`checked_at`、`evidence_json` | 按 operation 的探活事实 |

约束：

- `plugin_versions` 不允许更新已存在行的哈希与 manifest；更新只插入新版本。
- `plugin_options` 的 key 必须是 manifest 声明过的 option；未知 key 拒绝。
- `plugin_cache` 写入前检查命名空间归属、TTL、大小与 JSON 深度；插件不能读别的插件命名空间。
- `plugin_secrets` 的明文只在宿主内存中出现；日志、审计、JSON 输出都不能带出。
- 审计表只增不改；清理必须按保留策略批量进行，不提供插件 API。

### 19.4 现有表扩展

| 表 | 新增字段 | 说明 |
| --- | --- | --- |
| `providers` | `plugin_id`、`origin`、`permissions_hash` | `plugin_id` 为空表示内置或手工厂商；`origin` 为 `builtin`、`plugin`、`manual` |
| `provider_routes` | `operation`、`dialect_id` | 现有协议路由默认补 `chat.complete@1`；语音路由补对应 operation |
| `provider_models` | `plugin_id`、`catalog_source` | 目录来源与归属；能力仍以 `model_operations` 为准 |
| `accounts` | `plugin_id`、`auth_kind`、`auth_metadata_json` | `plugin_id` 为空表示内置；`auth_metadata_json` 只放非敏感元数据 |
| `account_models` | 不改主键 | 作为账号×模型的旧可达性视图保留；新逻辑按 `model_probes` 查 operation |
| `requests` | `plugin_id`、`plugin_version`、`operation`、`dialect_id`、`capability_source`、`hook_ms`、`upstream_ms`、`retry_count`、`fallback_count`、`plugin_error_code`、`plan_hash` | 每次上游尝试仍一行；插件维度可查询 |

兼容规则：

- 现有账号、厂商、路由与请求记录不做语义重写；新增列给空值即可。
- 现有协议到 operation 的默认映射：`openai-chat`、`openai-responses`、`anthropic`、`gemini` → `chat.complete@1`；`openai-embeddings` → `embeddings.create@1`；`openai-speech` → `speech.synthesize@1`；`openai-transcriptions` → `speech.transcribe@1`；`systemone` → `system.decide@1`。
- 现有 `account_models.access` 继续用于模型级可达性；operation 级探活结果不能覆盖另一 operation 的结论。
- 旧请求记录允许 `operation` 为空；新记录必须写 operation。
- 任何插件表都不能成为转发路径的唯一依据；启动时仍能只用内置表工作。

### 19.5 请求记录与缓存边界

请求记录是事实，不是插件日志的复制品：

- 只记录插件 ID、版本、operation、dialect、耗时、错误码和哈希，不记录提示词正文、音频、完整响应。
- `plan_hash` 是规范化 `RequestPlan` 的哈希，用于对比同一请求在重试或升级前后的变化。
- 插件缓存必须有 TTL 和大小上限；过期数据不能当作能力证明。
- 插件不能读取全局请求记录，也不能按账号查询历史；需要统计的插件由宿主注入聚合后的只读数据。

## 20 安装、生命周期与命令

### 20.1 来源与信任

第一版支持：

- 本地目录：必须有 `lax.plugin.json`。
- 本地 `.tar.gz` / `.zip`：解包后必须有唯一插件根。
- HTTPS 归档 URL：带 SHA-256 时必须校验；不带时标记 `unverified`。
- 本地 Magpie 包目录：通过 `plugin import magpie` 转换成 lax 插件或生成导入报告。

第一版不支持：

- 自动执行 npm `postinstall`、`prepare` 或任意包脚本。
- 从裸 Git 仓库直接执行；可以下载归档，但仍按归档校验。
- 插件自行创建监听端口或注册任意 HTTP 路径。
- 默认自动更新。

### 20.2 命令面

`plugin` 组统一支持 `--json`。结果写 stdout，错误写 stderr；退出码沿用仓库约定：`0` 成功，`1` 运行或校验失败，`2` 用法错误。

| 命令 | 参数 | 行为 |
| --- | --- | --- |
| `plugin install <source>` | `--trust declarative\|sandboxed\|trusted-worker`、`--dry-run`、`--allow-unverified`、`--no-enable`、`--yes` | 解析、校验、扫描、试运行，再原子安装 |
| `plugin list` | `--enabled`、`--kind`、`--trust` | 列出 ID、版本、类型、信任、状态、provider 与 operation |
| `plugin inspect <id>` | 无 | 显示 manifest、权限、来源、哈希、能力、冲突与最近审计 |
| `plugin enable <id>` | 无 | 启用并加载；失败时不改变原状态 |
| `plugin disable <id>` | 无 | 停止新请求路由；在途请求按 deadline 结束 |
| `plugin update <id>` | `--version`、`--source`、`--dry-run`、`--yes` | 安装新版本并原子切换；权限扩大必须重新确认 |
| `plugin rollback <id>` | `--version` | 切回已安装的旧版本；不重新下载 |
| `plugin remove <id>` | `--purge` | 停用、卸载包；默认保留配置、缓存与审计 |
| `plugin verify [<id>]` | `--all` | 校验 manifest、ABI、哈希、包完整性与 lockfile |
| `plugin doctor [<id>]` | 无 | 检查加载、权限、运行时、模型绑定与探活配置 |
| `plugin test <id>` | `--fixture` | 用 testdata 契约用例跑 hook；默认不连真实上游 |
| `plugin reload [<id>]` | 无 | 重载当前活动版本；用于配置变化，不改变版本 |
| `plugin logs <id>` | `--follow`、`--level`、`--since` | 查看宿主记录的插件日志，默认脱敏 |
| `plugin config get <id> [key]` | 无 | 读取非敏感配置；secret 只显示是否已设置 |
| `plugin config set <id> <key> <value>` | `--stdin` | 写入 manifest 声明的 option |
| `plugin config unset <id> <key>` | 无 | 删除 option 并恢复默认值 |
| `plugin secret set <id> <name>` | `--stdin` | 从 stdin 读入独立 secret，不通过命令行参数 |
| `plugin import magpie <source>` | `--mode declarative\|sandboxed\|manual`、`--dry-run` | 分析并导入兼容子集；默认只生成报告 |

凡是声明了 `--dry-run` 的写命令，在 dry-run 下不得写数据库、数据目录或 secret；只输出将要发生的变更与权限差异。`plugin secret set` 不支持 dry-run，它从 stdin 读入秘密，读取即视为一次显式设置。

### 20.3 安装流水线

1. **解析来源**：限制下载大小、重定向次数和协议；HTTP 只允许显式 loopback 开发模式。
2. **落到 staging**：权限收紧到当前用户；拒绝路径逃逸与特殊文件。
3. **校验 manifest**：schema、ABI、ID、版本、未知字段、entry 是否在根目录内。
4. **校验依赖**：`requires` 中的 operation、runtime、permission 宿主必须支持；缺失即安装失败。
5. **静态扫描**：动态代码、`eval`、`Function`、`process`、`fs`、`Bun`、`child_process`、非相对 import、可疑 URL、超大文件、嵌套压缩包。
6. **计算哈希**：包哈希、manifest 哈希、权限哈希；与来源提供的 checksum 比对。
7. **加载试运行**：在一次性 runtime 调用可选的 `describe`、`models` 的静态部分和 `plugin test` 的 smoke fixture；静态 manifest/catalog 已完成声明时跳过 `describe`；不得发真实上游请求。
8. **检查冲突**：plugin ID、provider ID、model ID、dialect ID、operation 绑定不得与现有内容静默冲突。
9. **展示权限差异**：新增网络 host、secret scope、账号能力、媒体读写、文件和进程权限逐项列出。
10. **确认并提交**：生成 `plugin_versions`、`plugins` 与审计记录；包原子移动到 `installed/`；失败回滚 staging。
11. **启用**：默认加载并通过健康检查后启用；`--no-enable` 只安装不加载。

任何一步失败都返回可执行的下一步提示，例如“缺少 host 白名单，请在 manifest 的 `permissions.network.hosts` 增加 `api.example.com`”，而不是只打印底层异常。

### 20.4 更新与回滚

更新不原地覆盖：

```text
download -> staging -> verify -> permission diff -> confirm
  -> insert new plugin_version -> switch active_version
  -> load new runtime -> health check
  -> drain old runtime -> keep old version for rollback
```

规则：

- 切换前的新请求继续用旧版本完成；切换后的新请求用新版本。
- 新版本加载失败或健康检查失败时，自动切回旧版本，并把新版本标为 `failed`。
- 权限扩大、ABI 主版本变化、信任级别变化都不能静默通过；必须重新确认。
- 不自动更新。`plugin update` 只更新显式指定的插件；即使来源提供了新版本，也要用户触发。
- 默认保留最近两个可用版本；更旧版本可按保留策略清理，`plugin rollback` 只能回到仍存在的版本。
- 更新失败不能删除旧版本，也不能改写旧版本的 `plugin_versions` 行。

### 20.5 输出与状态

人类可读输出示例：

```text
ID                    VERSION  KIND      TRUST       STATE    OPERATIONS
com.example.mimo      1.0.0    provider  sandboxed   ok       chat.complete@1, speech.synthesize@1
com.example.river     0.4.2    provider  declarative ok       chat.complete@1
```

`--json` 至少包含：

```json
{
  "id": "com.example.mimo",
  "version": "1.0.0",
  "kind": "provider",
  "trust": "sandboxed",
  "enabled": true,
  "state": "ok",
  "operations": ["chat.complete@1", "speech.synthesize@1"],
  "providers": ["mimo"],
  "permissions": {},
  "package_sha256": "…",
  "last_error": null
}
```

插件状态固定为：

- `ok`：已加载，最近调用正常。
- `degraded`：连续失败或探活失败，但仍允许有限流量。
- `unhealthy`：熔断，停止新请求。
- `disabled`：用户停用。
- `incompatible`：ABI、权限或依赖不满足。
- `failed`：加载失败或更新失败。
- `tampered`：包或 manifest 哈希不匹配。

状态与权限只由宿主写；插件返回值不能改变自己的 `enabled`、`trust` 或权限。

## 21 Magpie 兼容

### 21.1 调研结论

Magpie 的 provider 插件与网关中间件是两套运行时：

- Provider 插件运行在 Bun 宿主里，拥有文件系统、`child_process`、任意 `fetch` 和宿主进程权限。
- 网关中间件运行在 `moejs` 里，使用池化 Runtime、逐 hook 超时和受限 import。
- Magpie 的 `/v1/audio/speech` 与 `/v1/audio/transcriptions` 最终走 provider 的 `loader.fetch`，没有独立的 TTS/ASR 语义 hook。
- `magpie-community/plugins` 里没有现成的 TTS/ASR provider；`packages/mimo/index.mjs` 是 chat-only。
- 用真实 River 插件探测 `moejs`：默认 Runtime 能加载 `config` 与 `loader` 的静态部分，但没有 `fetch`、`process`、`fs`、`require`；`moejs/webapi` 提供受限 fetch/stream，`moejs/node` 只提供沙箱化文件与 buffer 子集。
- 静态扫描社区 provider：35 个里有 28 个直接依赖 Node/Bun 能力。因此“所有 Magpie 插件都能在 lax 里跑”不成立。

所以 lax 的承诺是：**导入一个可静态证明相容的子集**，不是兼容 Bun 运行时。

### 21.2 导入模式

| 模式 | 适用条件 | 产物 |
| --- | --- | --- |
| `declarative` | `config`、`loader`、`auth.methods`、模型表都是静态或可静态求值；不需要 `loader.fetch` | 原生 lax manifest + dialect + models，执行零 JS |
| `sandboxed` | 有代码，但只使用宿主允许的全局与受限 `loader.fetch`；没有 Node/Bun API | `sandboxed` lax 插件 + 兼容 transport adapter |
| `manual` | 依赖 OAuth、`Bun.$`、`process.env`、文件、进程、任意网络或复杂流式 | 只生成迁移报告，不安装 |

导入命令默认 `--dry-run`，输出三份结果：

```text
compatible    可以自动导入，列出权限与 operation
needs-review  需要人工确认，说明原因与改动点
unsupported   不兼容，列出具体 API 与替代方案
```

### 21.3 入口解析

导入器按以下顺序找入口：

1. 有 `lax.plugin.json`：直接按 lax 插件校验，不再按 Magpie 猜。
2. 有 `package.json` 且声明 `magpie.middleware` 或 `magpie.gateway`：作为 transform 候选；只映射明确支持的钩子。
3. Provider 包：读 `package.json` 的 `exports["."]`，否则 `module`，否则 `main`；必须落在包根内。
4. 入口文件必须能被静态解析；入口导出的插件函数必须唯一可确定。多个导出无法判断时进入 `manual`。
5. 没有入口、只有声明文件时按 `declarative` 处理。

使用 `exports` 时必须处理条件导出；只接受 `import`/`default` 指向的字符串路径，其他条件先报告再人工选择。

### 21.4 字段映射

| Magpie/OpenCode 字段 | lax 对应 | 说明 |
| --- | --- | --- |
| `config.provider[id].name` | provider 名称 | 只作展示 |
| `config.provider[id].npm` | `upstream_wire` | `@ai-sdk/openai-compatible` → `openai-chat`，其他只在已知映射表内 |
| `config.provider[id].api` | provider `base_url` | 必须是 HTTPS；导入时进网络白名单 |
| `config.provider[id].models` | models + `model_operations` | 每个模型要补 operation 与能力；缺失即 `"unknown"` |
| `auth.provider` | provider ID | 必须与 `config.provider` 一致 |
| `auth.methods[].type = "api"` | `auth_kind = "api-key"` | `label` 只作展示，`placeholder` 不决定秘密 |
| `loader.baseURL` | 路由 base | 必须命中白名单，不能运行时改到私网 |
| `loader.apiKey` | 宿主注入的账号 key | 导入器不保存这个值；真实值来自 `account add --key` |
| `loader.headers` | `provider_headers` 或动态 `headers` | 静态头可声明；动态头需要 hook |
| `provider.models()` | `models` hook | 返回值只作模型目录，不授予能力 |
| `chat.headers` | `headers` hook | 只能生成请求头，不能改 host、账号或计费 |
| `loader.fetch()` | 兼容 transport 适配层 | 见下节；不是任意网络通行证 |

模型能力不能从 `npm` 或模型名自动推断为 `true`。导入器只能：

- 把明确写出的 `reasoning`、`tool_call`、`modalities`、`limit` 转成对应字段。
- 把无法确认的能力写成 `"unknown"`。
- 在报告里列出“未声明、因此默认拒绝”的 operation。

### 21.5 `loader.fetch` 的边界

Magpie 插件经常把完整请求构造放在 `loader.fetch` 里，甚至直接返回自定义 `Response`。lax 不把这个函数暴露成裸 `fetch`，而是提供兼容桥：

```text
plugin loader.fetch(url, init)
  -> 解析 method / url / headers / body
  -> 命中 manifest host / method / size 白名单
  -> 转成受限 RequestPlan
  -> 宿主发送，必要时注入账号凭据
  -> 返回受限 Response 视图给插件
```

规则：

- URL 必须命中插件 manifest 的网络白名单；不能因为 Magpie 插件里写了 `fetchImpl` 就放开。
- 兼容桥只在导入器生成的 `compat_fetch` 阶段可用；在 `describe`、`models`、`prepare_request` 等阶段调用 `loader.fetch` 会返回 `plugin_error`，不能在“准备请求”时偷偷发数据面请求。
- `loader.fetch` 不是控制面 API；它只能承载当前 operation 的一次上游请求或该 provider 声明的模型列表请求。
- 每次请求最多一个上游调用；插件想重试或 fallback，由宿主策略决定。
- 请求体、响应体、流块、头数量和总时长都有硬上限。
- 响应对象只提供 `status`、`headers`、`text()`、`json()`、`body` 的受限视图；不提供 `Response` 构造器、文件系统或 socket。
- `loader.fetch` 不能把响应直接写给客户端；仍要经过 `decode_response` 或 `decode_stream` 转成语义结果。
- 插件不能通过 `loader.fetch` 读取任意网页、调用 webhook 或访问元数据服务；这些不是 provider 数据面。
- 如果插件依赖多个 host、动态 host、任意重定向或 WebSocket，导入模式降为 `manual`。

这层桥的代价是：Magpie 插件看到的不是原生 Bun `fetch`，某些依赖 `Response` 实现细节、`ReadableStream` 特性或 Bun 网络行为的插件仍会失败。报告必须说明，而不是在运行期才暴露。

### 21.6 明确不支持

以下情况直接判为 `manual` 或 `unsupported`：

- 直接 import Node 内置模块：`node:fs`、`node:path`、`node:child_process`、`node:http2`、`node:crypto` 等。
- 使用 `Bun.*`、`Bun.$`、`Bun.file`、`Bun.spawn`、`Bun.serve`。
- 读取 `process.env`、`process.cwd()`、`__dirname` 之外的文件，或执行 shell。
- 需要 `child_process` 调用厂商 CLI 才能登录。
- 依赖 npm 安装脚本、原生插件、二进制辅助程序。
- OAuth 浏览器回调、设备码轮询、需要本地监听端口的登录。
- 任意注册 HTTP 路径、页面、菜单、agent 或非 provider 的 Magpie 扩展。
- 依赖 Magpie 的全局配置、Bun 版本、`MAGPIE_ADDR` 或其他宿主环境变量。
- `loader.fetch` 需要访问未声明 host、私网地址、WebSocket 或无法限长的流。

支持的 Magpie transform 钩子子集：

| Magpie | lax | 差异 |
| --- | --- | --- |
| `onRequest` | `transform.on_request` | lax 默认失败关闭，不沿用 Magpie 的 fail-open |
| `onProvider` | `transform.on_request` 的 provider 阶段 | 不能改账号选择 |
| `onResponse` | `transform.on_response` | 只能改语义结果 |
| `onEvent` | `transform.on_event` | 不能伪造结束或用量 |
| `onUsage` | `transform.on_usage` | 只读观察，不能改金额 |
| `strategies` | 不导入 | 选号策略属于宿主 |

Magpie 中间件失败时保留原请求；lax 的 `on_request` 默认失败关闭，因为一次静默跳过可能把未脱敏或未映射的请求发给上游。导入时必须显示这条行为差异；确实需要 fail-open 的，只能用于只读观察钩子，不能用于请求变换。

### 21.7 多语言与语音插件

Magpie 没有为 TTS/ASR 定义独立能力协议，音频端点只是 `loader.fetch` 的另一种响应。导入器因此不能把 Magpie 插件自动当成“支持 `speech.synthesize@1`”的证据。要成为 lax 语音插件，必须额外提供或由导入器生成：

- operation 与 dialect 绑定。
- `formats`、`voices`、`languages` 三态能力。
- TTS/ASR 的请求与响应解码规则。
- 按 operation 的探活方式。

如果 Magpie 插件本身只实现 chat，即使它内部能返回音频，也只导入 chat operation。若它通过 `loader.fetch` 显式处理 `/v1/audio/speech`，导入器可以生成一个语音 transport 候选，但默认把 `formats`、`voices`、`languages` 标成 `"unknown"`，要求用户补声明或跑探活。

## 22 New API 的可借鉴点

New API 的 Task Plugin API 提供了几个成熟做法：

### 22.1 Manifest 驱动

- 插件声明 `models`、`routes`、`protocols`、`apiVersion` 和 usage schema。
- 宿主在加载时校验版本与字段，不把未知声明当成可用能力。
- 插件元数据可以驱动列表、图标、表单和用量展示。

lax 借鉴 manifest 驱动与严格校验，但不借鉴“插件注册原生 HTTP 路由”。lax 的入口固定为语义 operation；客户端路径、鉴权头、错误形状和重试仍由宿主拥有。否则一个插件就能改变网关的公开 API，破坏多客户端兼容。

### 22.2 宿主托管的 `utils.fetch`

New API 的 `utils.fetch` 由宿主执行，插件不直接拿到任意网络：

- host allowlist。
- 每个 hook 最多 4 次调用。
- 单次 15 秒、总 30 秒。
- 单次响应最大 1 MiB。
- 超限、未知 host、错误重定向直接拒绝。

lax 的 `lax.http.request` 采用同一思路，但区分控制面与数据面：

- 控制面：模型列表、探活、刷新，走 `lax.http.request`。
- 数据面：只允许 `RequestPlan` 或 Magpie `loader.fetch` 兼容桥，由宿主发送。
- 两者都不能让插件看到账号 key 原文。

### 22.3 Runtime 池与调用预算

New API 对 hook 调用计数、并发和超时做统一管理。lax 应借鉴：

- 每插件、每 operation、每 Runtime 分别计数。
- 超时中断后销毁 Runtime，不回到池里。
- 连续失败触发熔断，不让一个坏插件拖垮所有请求。
- 诊断输出调用次数、失败数、平均耗时和最近错误，而不是只给一个布尔状态。

### 22.4 严格未知字段

New API 对插件声明做严格 property validation；旧宿主会拒绝新字段。lax 采用类似规则：

- manifest、dialect、hook 返回值都拒绝未知字段。
- 需要扩展时使用版本化 `extensions`，并明确宿主是否忽略。
- 同一主版本只允许新增可选字段；删除、改语义升主版本。

### 22.5 Usage schema 与异步任务

New API 的 usage schema 对视频、音频等非 token 计费很有价值。lax 可以借鉴“插件声明用量字段”，但不能让插件声明价格或结算规则。异步任务也有价值，但第一版不引入任务队列：

- `video.generate@1` 先返回操作结果或明确的 `pending` 语义。
- `video.status@1`、`video.content@1` 后续再实现。
- 不在第一版复制 `/v1/tasks` 这类公开路径；任务 ID 和查询路由属于宿主设计。

### 22.6 不借鉴的部分

- 不复制 Task Plugin 的原生 route 注册。
- 不让插件声明并占用公开协议路径。
- 不让插件决定最终错误形状、计费或账号 fallback。
- 不把异步任务、配额、充值和后台纳入插件系统第一版。
- 不因为 New API 有某个字段就把它塞进 lax manifest；只保留当前 operation ABI 需要的字段。

## 23 安全模型

### 23.1 威胁与对策

| 威胁 | 影响 | 对策 | 残余风险 |
| --- | --- | --- | --- |
| 插件窃取账号 key | 上游账号被滥用 | 默认 `host-inject`，`ctx` 不含秘密；只有显式 secret 权限才给受限值 | 同进程内存读取、宿主漏洞 |
| 插件读取其他账号 | 跨租户数据泄漏 | 每次调用只注入当前账号；缓存按插件命名空间隔离；不提供账号列表 API | 宿主 bug |
| SSRF 访问私网/元数据 | 读取云元数据、内网服务 | host allowlist、解析后 IP 校验、DNS 固定、禁止重定向到私网、禁止 userinfo | DNS 重绑定防御依赖解析实现 |
| 重定向绕过白名单 | 请求到未授权 host | 默认 `redirects: none`；允许时同 host、最多一次、重新校验 IP | 上游自身行为 |
| 恶意响应体 | 内存耗尽、解析崩溃 | 响应体硬上限、流式读取、深度与字段数限制、panic 恢复 | 大流仍占带宽 |
| 无限循环/CPU 占用 | Runtime 卡死 | 逐 hook 超时、Runtime 隔离、并发上限、熔断 | 宿主调度开销 |
| 内存炸弹 | 进程 OOM | 堆上限、媒体走 MediaRef、事件与缓存上限、输入大小限制 | Go 进程共享内存 |
| 动态代码与逃逸 | 任意代码执行 | 沙箱关闭 `eval`/`Function`，只允许相对 import；不注入 Node/Bun | 引擎漏洞 |
| 原型污染 | 改宿主对象或越权字段 | 深拷贝、拒绝 `__proto__`/`constructor`/`prototype`、冻结 intrinsics、严格 JSON | 引擎实现差异 |
| 供应链投毒 | 安装即执行恶意代码 | 校验 checksum、无安装脚本、静态扫描、权限差异确认、unverified 不自动更新 | 用户手动装未验证包 |
| 日志/错误泄漏 | 密钥、提示词、音频外泄 | 日志脱敏、错误截断、`details` 只进受控日志、默认不回显请求体 | 用户主动导出日志 |
| 计费篡改 | 少记/多记用量 | 插件只报事实，宿主校验并计费；价格不来自插件 | 上游少报 |
| Header 注入 | 请求走私、伪造头 | 禁止 CR/LF/NUL，覆盖 `Host`、`Content-Length` 等，头名白名单校验 | 上游代理行为 |
| 路径逃逸 | 读到插件目录外文件 | `entry` 与 import 路径必须落在插件根；拒绝绝对路径、`..`、symlink 逃逸 | 文件系统竞态 |
| 恶意的 Magpie `loader.fetch` | 任意网络访问 | 兼容桥只允许声明 host 与当前 operation；禁止任意 `Response`、WebSocket | 导入器分析不全 |
| 更新降级/回滚攻击 | 运行旧漏洞版本 | 保留版本哈希、审计、权限差异确认；回滚必须显式 | 用户主动回滚 |
| 插件状态污染 | 跨请求串数据 | 每请求独立 Runtime，模块状态只在调用内；缓存显式命名空间 | 池复用 bug |

### 23.2 信任提示

安装时按信任级别显示：

- `declarative`：无代码执行；显示 provider、host、模型与 operation。
- `sandboxed`：可执行 JS；显示 host、媒体读写、账号 secret 范围、hook 预算与可调用的宿主 API。
- `trusted-worker`：额外显示“可访问文件系统、进程、环境变量和任意网络”，并要求显式 `--trust trusted-worker` 与风险确认。

权限扩大必须重新确认，即使插件版本号只是补丁版本。权限缩小可以自动通过，但仍写审计。

### 23.3 默认失败关闭

以下情况拒绝请求或停止插件，不能静默放行：

- manifest / ABI / operation 不认识。
- 包哈希或权限哈希不匹配。
- dialect 绑定的 operation 与请求不一致。
- `RequestPlan` 未通过 host、scheme、path、header、body 校验。
- 能力为 `false` 或 `"unknown"` 且没有显式允许。
- 重定向目标、DNS 解析结果或私网检查失败。
- 插件返回未知字段、超限结果或非法媒体引用。
- Runtime 超时、崩溃、连续失败达到熔断阈值。
- `trusted-worker` 未确认风险或进程未启动。

例外只有明确声明的只读观察钩子：`on_usage` 失败可以记录并继续，因为它不改变请求；`on_request`、`decode_response`、`decode_stream` 默认不能 fail-open。

### 23.4 Secret 处理

- 账号 secret 只由 `internal/secret` 解密，宿主负责注入。
- 插件只能通过 `ctx.account.id` 引用账号，不拿 key、refresh token、cookie 或完整认证头。
- `RequestPlan.auth` 只描述注入方式，不包含明文。
- 需要自定义签名时，优先使用宿主模板；`sign_request` 只拿最小 secret，并禁止改 host、method、body。
- 插件独立 secret 只存密文；`plugin config` 不读写 secret，`plugin secret set` 从 stdin 读。
- 日志、错误、审计、`--json` 一律脱敏；只显示“已设置/未设置”。
- Go 进程无法保证内存清零；文档不能宣称“秘密绝不会留在内存”。

### 23.5 trusted-worker

`trusted-worker` 不是普通沙箱，必须作为高风险模式：

- 独立进程、独立工作目录、最小环境变量。
- 只注入当前账号所需凭据；不挂载数据库。
- 限制 CPU、内存、打开文件数、子进程与运行时间。
- stdout/stderr 进入宿主日志，限制单行大小，避免管道阻塞。
- 退出、崩溃或超时由宿主回收，不能残留孤儿进程。
- 仅按插件单独开启，默认关闭；安装与每次启动都显示风险。

如果某个上游只需要 HTTP 和字段转换，不应使用 `trusted-worker`；优先写 `sandboxed` 插件。

## 24 可观测性

### 24.1 请求记录

每次上游尝试至少可查询：

| 字段 | 说明 |
| --- | --- |
| `plugin_id`、`plugin_version` | 哪个插件处理 |
| `operation` | 语义动作 |
| `dialect_id` | 上游形状 |
| `capability_source` | 本次放行依据：`declared`、`configured`、`probed` |
| `hook_ms` | 本次所有插件 hook 的总耗时 |
| `upstream_ms` | 宿主发上游请求的耗时 |
| `retry_count` | 当前尝试内的重试次数 |
| `fallback_count` | 跨账号 fallback 次数 |
| `plugin_error_code` | 插件语义错误码；宿主错误可为空 |
| `plan_hash` | 规范化 RequestPlan 哈希 |

不记录：

- 完整提示词、音频、图片、视频或响应正文。
- 账号 key、Authorization、cookie、refresh token。
- 可由 `request_id` 关联的完整插件日志正文。

### 24.2 插件日志

格式：

```text
plugin[com.example.mimo@1.0.0] level=info op=speech.synthesize@1 request=01J... msg="prepared request"
```

规则：

- 每个插件有固定前缀，便于 `plugin logs` 过滤。
- 默认限流，例如每插件每分钟 60 行；超限丢弃并计数，不阻塞请求。
- 单行最大 8 KiB，超长截断并标记 `truncated=true`。
- 不记录秘密、完整 URL query、完整请求体或媒体内容。
- 插件不能写 stdout；stdout 被重定向到日志并标记来源，避免破坏宿主协议。
- panic、超时、内存超限单独记录，包含 hook 名与安全摘要，不包含堆内存内容。

### 24.3 指标

建议暴露以下指标，标签基数要受控：

- `lax_plugin_hook_total{plugin,operation,hook,result}`。
- `lax_plugin_hook_duration_seconds` 直方图。
- `lax_plugin_failures_total{plugin,reason}`。
- `lax_plugin_runtime_pool{plugin,state}`。
- `lax_plugin_capability_probe_total{plugin,operation,result}`。
- `lax_plugin_cache_bytes{plugin}`。
- `lax_plugin_state{plugin,state}`。

不要把请求 ID、模型名、账号名、完整 dialect 或错误消息放进指标标签；它们放在请求记录或日志里。

### 24.4 健康状态与熔断

状态机：

```text
ok -> degraded -> unhealthy -> disabled
 ^        |          |
 +--------+----------+
      reload / doctor / 时间窗口恢复
```

规则：

- `ok`：最近 N 次调用无插件错误；或错误率低于阈值。
- `degraded`：错误率升高、探活失败或控制面超时，但仍允许有限流量。
- `unhealthy`：连续失败或熔断打开，停止新请求；在途请求按 deadline 结束。
- `disabled`：用户停用，不自动恢复。
- `incompatible`、`failed`、`tampered`：不会因为时间自动恢复，必须 `verify`、重装或重新启用。

熔断只影响该插件或该 operation，不把整个网关打挂。一个 provider 插件失败时，宿主仍可按现有账号与 fallback 策略选择其他 provider；插件自身错误默认不 fallback 到同一插件的其他账号，避免放大 bug。

### 24.5 审计

以下动作必须写 `plugin_audit`：

- install、update、rollback、remove、enable、disable、reload。
- 权限扩大与信任级别变化。
- secret 设置/删除（只记名字与时间，不记值）。
- 插件被标记 `tampered`、`incompatible` 或 `failed`。
- 手工覆盖 dialect/模型绑定。

审计记录只增不改，保留策略独立于插件包。`plugin inspect` 展示最近若干条，方便回答“谁在什么时候把哪个权限打开了”。

## 25 测试与验收

### 25.1 测试分层

| 层 | 测试对象 | 是否联网 | 入口 |
| --- | --- | --- | --- |
| 纯函数 | manifest、dialect、能力合并、权限差异、请求映射 | 否 | `go test` |
| 契约 | 插件 hook 输入输出、错误映射、用量事实 | 否 | `plugin test` + testdata |
| 网关 | RequestPlan 执行、流式分发、fallback、错误形状 | 否，`httptest` | `go test ./internal/gateway` |
| CLI | 安装、更新、回滚、JSON 输出、退出码 | 否 | `go test ./internal/cli` |
| 存储 | 事务、约束、旧库兼容、并发更新 | 否，临时 SQLite/外部测试库 | `go test ./internal/store` |
| 安全 | SSRF、重定向、超限、逃逸、原型污染 | 否，`httptest` 与本地假 DNS | `go test` |
| 导入 | Magpie 包解析、静态扫描、兼容报告 | 否 | `go test` + fixture 包 |
| 端到端 | 起 `serve`，客户端打三条入站与语音端点 | 否，假上游 | `go test` |

真实厂商调用只能作为人工验收，不进 `make check`。任何测试都不得写死用户 key，也不得把真实音频、提示词或响应体提交到仓库。

### 25.2 Fixture 布局

```text
testdata/plugins/
  com.example.mimo/
    manifest.json
    models.json
    dialects.json
    cases/
      tts-basic/
        input.json
        request.json
        response.json
        expected.json
      asr-basic/
        input.json
        request.json
        response.json
        expected.json
      tts-upstream-400/
        input.json
        request.json
        response.json
        expected.json
```

规则：

- 每个 case 只验证一个行为，失败时能指出哪个字段不同。
- `request.json` 是期望的 `RequestPlan`；`expected.json` 是 `OperationResult` 或语义错误。
- 二进制音频用小的确定性 fixture，不用真实人声；大文件用生成器，并在测试后删除。
- 错误 case 必须包含上游状态码、错误体和期望的 lax 错误码。
- `plugin test` 默认只跑 fixture；`--live` 若未来加入，必须显式开启并提示可能计费。

### 25.3 必测清单

Manifest 与 ABI：

- 缺失、未知、重复字段被拒。
- 未知主版本、缺失 operation、非法 provider ID 被拒。
- `entry` 绝对路径、`..`、symlink 逃逸被拒。
- ABI 次版本新增可选字段仍可加载；删除字段被拒。
- manifest 声明的权限与实际调用不一致时，安装或运行被拒。

Dialect：

- 同一模型同一 operation 的两个绑定冲突。
- `@1` 与 `@2` 并存，模型可分别绑定。
- 内置 `lax/` dialect 不能被覆盖。
- 声明式映射只接受白名单节点；出现脚本片段被拒。
- 路径模板中的模型名被正确转义，不能注入 query 或 path。

RequestPlan：

- `path` 与 `url` 只能有一个。
- host、scheme、userinfo、fragment、私网 IP、重定向违规被拒。
- 禁止头覆盖、CR/LF/NUL 注入被拒。
- JSON、text、form、multipart、bytes、media、none 七种 body 都能正确执行。
- multipart boundary 由宿主生成，插件拼的 boundary 无效。
- 响应大小、超时、取消都按上限截断或终止。

结果与错误：

- 每个结果类型都能编码回对应入站协议。
- 插件错误不能指定最终 HTTP 状态。
- `retryable` 只是提示，最终重试由宿主决定。
- 错误消息、details、日志不泄漏秘密。
- 未知错误码、未知结果类型、超大 usage 被拒。

流式：

- 任意字节边界：半行、半帧、多帧、跨 UTF-8 字符、空块、末尾无换行。
- SSE 的 `data:`、注释行、事件名、`[DONE]` 分别处理。
- 客户端取消后上游被取消，Runtime 被回收。
- `done: true` 后不再读取上游。
- 事件数、事件大小、状态大小超限被拒。
- 同一流不并发调用 `decode_stream`。

存储与生命周期：

- 安装失败不留下半条 `plugins`/`plugin_versions`。
- 更新失败自动回滚，旧版本仍可服务。
- 在途请求固定使用启动时的版本。
- 删除插件后模型不可用，不会静默回退。
- lockfile 哈希、manifest 哈希、权限哈希不匹配时拒绝加载。
- `--purge` 清理缓存与 secret，但保留审计。

安全：

- `127.0.0.1`、`::1`、`10.0.0.0/8`、`172.16.0.0/12`、`192.168.0.0/16`、`169.254.169.254`、IPv4-mapped IPv6、未指定地址都被拒。
- DNS 返回多个地址时，任一私网地址都拒绝；连接使用已校验并固定的 IP。
- 重定向到不同 host、HTTPS → HTTP、私网地址都拒绝。
- `__proto__`、`constructor`、`prototype` 注入不能污染宿主对象。
- `eval`、`Function`、`process`、`fs`、`Bun`、`child_process` 在 sandbox 中不可见。
- 无限循环、递归 JSON、巨大数组、超大字符串触发超时或内存错误，进程不崩溃。
- 单插件日志与错误不会刷爆磁盘。
- 恶意 `loader.fetch` 不能访问白名单外 host。

导入：

- River 这类静态 OpenAI-compatible 包导入为 `declarative`。
- 使用 `loader.fetch` 的包只有在 host 与 operation 可静态确定时才进入 `sandboxed`。
- 28/35 依赖 Node/Bun 的样本至少被识别并归入 `manual`，不能误判为可运行。
- `package.json` 的 `exports`/`main`/`magpie.middleware` 入口解析符合规则。
- 导入报告对未知能力写 `"unknown"`，不写成 `true`。

端到端：

- 起 `serve`，用 `httptest` 假上游完成 chat、TTS、ASR 各一条非流式与流式请求。
- 同一个 provider 下两个账号，杀掉一个后 fallback 到另一个。
- 插件返回 `plugin_error` 时不 fallback 到同插件其他账号。
- `/v1/models` 包含 TTS/ASR 模型、正确 operation 与能力来源。
- 停用插件后 `/v1/models` 不再宣称其模型可用。

### 25.4 验收门槛

- `make check` 通过。
- `go test -race ./...` 在支持 race 的平台上通过。
- 新增或修改 `translate`/映射规则时，`testdata` 对拍齐全。
- 所有安全用例在 CI 中默认运行，不依赖网络。
- 安装、更新、回滚、权限扩大、secret 设置的 CLI 快照稳定。
- 任何真实厂商验收都单独记录，不进入自动测试。
- 已知边界写进 `README.md`，容易复发的坑写进 `LESSONS.md` 与 `lessons/`。

## 26 分阶段落地

### 26.1 阶段划分

| 阶段 | 范围 | 不做什么 | 验收 |
| --- | --- | --- | --- |
| 0 设计合并 | 把本文结论写进 `DESIGN.md`，列出 ABI 与 schema | 不写实现 | `DESIGN.md` 内部无矛盾；本文标记为已合并或删除 |
| 1 声明式 provider | manifest、dialect、models、`plugin install/list/inspect/test`、声明式请求映射 | 不执行第三方 JS；不做 Magpie | 新增一个 OpenAI-compatible provider 不改 Go、不发版 |
| 2 内置语音迁移 | 用 `system` 插件或同一 ABI 的 operation hook 承接 MiMo TTS/ASR | 不删除现有兼容路径，直到对拍通过 | 去掉“speech + chat 就是 MiMo”的核心分支；TTS/ASR 对拍与现状一致 |
| 3 沙箱 provider | `moejs`、Runtime 池、`RequestPlan`、控制面 `lax.http`、媒体引用、流式 ABI | 不放宽到 Node/Bun | 一个需要动态请求的 provider 在 sandbox 中稳定运行；安全用例通过 |
| 4 Magpie 子集 | 导入器、`loader.fetch` 桥、transform 子集、OAuth 评估 | 不承诺全部 Magpie 插件 | 至少一个静态包导入为声明式；至少一个沙箱包导入并跑通 fixture；不支持项有报告 |
| 5 trusted-worker | 独立进程、文件/进程权限、显式授权、资源回收 | 不默认开启 | 厂商 CLI 场景可运行；越权与孤儿进程测试通过 |

阶段 2 的关键不是“把 MiMo 代码搬到另一个文件”，而是改变所有权：

- 核心不再判断“这个模型是语音，所以按 MiMo 的 chat 形状发”。
- MiMo 的请求构造与响应解码由插件/dialect 完成。
- 网关只负责选号、`RequestPlan` 执行、错误形状、计费和客户端编码。
- 现有未提交的 MiMo 语音实现保留为行为参考与 fixture 来源，不在迁移完成前删除。

### 26.2 文档更新点

合并进 `DESIGN.md` 时必须同时改：

| 位置 | 改动 |
| --- | --- |
| 第 2 节 | 把“插件运行时”从明确非目标改为分阶段目标，写清边界 |
| 第 4 节 | 增加 `plugin` 命令组、参数、输出列与 `--json` 字段；退出码沿用 0/1/2 |
| 5.7 | 模型同步增加插件目录、能力来源、按 operation 的探活 |
| 5.8 | 厂商与路由增加 `plugin_id`、operation、dialect；内置路由兼容 |
| 5.16 | 语音从核心适配改为 operation + dialect；写清 MiMo 迁移 |
| 第 6 节 | 新表、现有表新列、索引、事务与旧库兼容；具体迁移方式按当时仓库约定 |
| 第 7 节 | 增加插件里程碑与每阶段验收，不把本文当第二份设计 |
| 第 8 节 | `internal/plugin`、`internal/plugin/runtime`、`internal/plugin/import` 等职责边界 |
| 第 9 节 | 从非目标移除“插件运行时”，保留“不做多租户、配额、网页后台”等 |

同时更新：

- `README.md`：安装、启用、排障、常见 provider 导入示例、已知边界。
- `LESSONS.md` 与 `lessons/`：记录 Magpie 全权限宿主、`loader.fetch`、能力未知、PCM 名称不一致等不易复原原因的问题。
- 本文：合并完成后改为“已合并到 `DESIGN.md`”，或删除，避免长期维护两份事实。

### 26.3 兼容与回滚

- 现有配置、数据库、账号、请求记录、三条入站协议与语音端点保持原行为。
- 新列默认空；旧客户端与旧 CLI 不受影响。
- 内置 provider 继续可用；插件使用新 provider ID，不能静默替换。
- 阶段 2 的 MiMo 迁移保留旧路径作为 fallback，直到对拍用例全部通过。
- 每个阶段都可单独停用：声明式插件可禁用；沙箱插件可卸载；trusted-worker 可关闭。
- 插件系统故障不能阻止 `serve` 启动；启动时应降级为“只使用内置 provider”，并把插件状态标为 `failed`/`incompatible`。

## 27 示例

### 27.1 MiMo TTS 与 ASR 插件

下面是一个完整的 sandboxed provider 示例。示例只使用宿主 API，不使用 `fetch`、`process`、`fs`、`Bun` 或 Node 内置模块。

`lax.plugin.json`：

```json
{
  "schema": "lax.plugin/v1",
  "id": "com.example.mimo",
  "name": "Xiaomi MiMo",
  "version": "1.0.0",
  "kind": "provider",
  "runtime": "moejs",
  "entry": "./index.js",
  "abi": "lax.plugin/v1",
  "requires": [
    "operation:speech.synthesize@1",
    "operation:speech.transcribe@1"
  ],
  "permissions": {
    "network": {
      "mode": "host-proxy",
      "schemes": ["https"],
      "hosts": ["mimo-server-sgp.xiaomimimo.com"],
      "redirects": "none",
      "max_control_calls": 4,
      "max_control_body_bytes": 1048576
    },
    "secrets": {
      "plugin": [],
      "account": ["api_key"],
      "mode": "host-inject"
    },
    "filesystem": "none",
    "process": "none",
    "env": "none",
    "media": {
      "read_bytes": 67108864,
      "write_bytes": 67108864
    }
  },
  "providers": [
    {
      "id": "mimo",
      "name": "Xiaomi MiMo",
      "catalog": "./models.json",
      "dialects": "./dialects.json"
    }
  ],
  "options": {
    "region": {
      "type": "string",
      "enum": ["SGP", "RU", "IN"],
      "default": "SGP"
    }
  }
}
```

`dialects.json`：

```json
{
  "schema": "lax.dialect/v1",
  "dialects": [
    {
      "id": "com.example.mimo/mimo-chat-speech@1",
      "operation": "speech.synthesize@1",
      "upstream_wire": "openai-chat",
      "transport": {"request": "json", "response": "json", "streaming": "none"},
      "implementation": {"prepare": "hook", "decode": "hook"},
      "defaults": {"voice": "冰糖"},
      "limits": {"max_input_chars": 8192, "max_response_bytes": 33554432}
    },
    {
      "id": "com.example.mimo/mimo-chat-asr@1",
      "operation": "speech.transcribe@1",
      "upstream_wire": "openai-chat",
      "transport": {"request": "json", "response": "json", "streaming": "none"},
      "implementation": {"prepare": "hook", "decode": "hook"},
      "limits": {"max_input_bytes": 25165824, "max_response_bytes": 8388608}
    }
  ]
}
```

`models.json`：

```json
{
  "models": [
    {
      "model": "mimo-v2.5-tts",
      "upstream_id": "mimo-v2.5-tts",
      "operations": {
        "speech.synthesize@1": {
          "dialect": "com.example.mimo/mimo-chat-speech@1",
          "capabilities": {
            "supported": true,
            "streaming": false,
            "formats": ["mp3", "wav", "pcm16"],
            "voices": ["冰糖", "茉莉", "苏打", "白桦"],
            "languages": {
              "output": {"mode": "list", "tags": ["zh-CN"]}
            },
            "limits": {"max_input_chars": 8192}
          }
        }
      }
    },
    {
      "model": "mimo-v2.5-asr",
      "upstream_id": "mimo-v2.5-asr",
      "operations": {
        "speech.transcribe@1": {
          "dialect": "com.example.mimo/mimo-chat-asr@1",
          "capabilities": {
            "supported": true,
            "streaming": false,
            "formats": ["wav", "mp3"],
            "languages": {
              "input": {"mode": "list", "tags": ["zh-CN", "yue", "en-US"]},
              "auto_detect": true,
              "timestamps": "unknown",
              "diarization": "unknown"
            },
            "limits": {"max_input_bytes": 25165824}
          }
        }
      }
    }
  ]
}
```

`index.js`：

```js
const FORMAT_TO_UPSTREAM = {
  mp3: "mp3",
  wav: "wav",
  pcm16: "pcm",
};

const UPSTREAM_TO_MIME = {
  mp3: "audio/mpeg",
  wav: "audio/wav",
  pcm: "audio/pcm",
};

function fail(code, message) {
  throw lax.error(code, message);
}

function ttsBody(ctx) {
  const format = ctx.input.format || "mp3";
  if (!FORMAT_TO_UPSTREAM[format]) {
    fail("invalid_input", `MiMo TTS 不支持格式 ${format}`);
  }

  const messages = [];
  if (ctx.input.instructions) {
    messages.push({ role: "user", content: ctx.input.instructions });
  }
  messages.push({ role: "assistant", content: ctx.input.text });

  return {
    model: ctx.model.upstream_id,
    messages,
    audio: {
      voice: ctx.input.voice || "冰糖",
      format: FORMAT_TO_UPSTREAM[format],
    },
  };
}

function asrBody(ctx) {
  const audio = ctx.input.audio;
  if (!audio || !audio.ref) {
    fail("invalid_input", "ASR 需要音频输入");
  }

  const format = (ctx.input.format || "wav").toLowerCase();
  const mime = audio.mime || "audio/wav";
  const base64 = lax.media.readBase64(audio.ref, {
    max_bytes: ctx.limits.max_input_bytes,
  });

  const body = {
    model: ctx.model.upstream_id,
    messages: [
      {
        role: "user",
        content: [
          {
            type: "input_audio",
            input_audio: {
              data: `data:${mime};base64,${base64}`,
              format,
            },
          },
        ],
      },
    ],
  };

  if (ctx.input.language) {
    body.asr_options = { language: ctx.input.language };
  }
  return body;
}

export async function prepare_request(ctx) {
  const isTTS = ctx.operation === "speech.synthesize@1";
  const body = isTTS ? ttsBody(ctx) : asrBody(ctx);

  return {
    method: "POST",
    path: "/v1/chat/completions",
    headers: { "content-type": "application/json" },
    auth: { kind: "host" },
    body: { kind: "json", value: body },
    dialect: ctx.dialect.id,
    response: {
      mode: "buffered",
      max_bytes: ctx.limits.max_output_bytes,
    },
    timeout_ms: ctx.limits.deadline_ms,
  };
}

export async function decode_response(ctx) {
  const choices = ctx.response.body && ctx.response.body.choices;
  const message = choices && choices[0] && choices[0].message;

  if (ctx.operation === "speech.synthesize@1") {
    const data = message && message.audio && message.audio.data;
    if (!data) {
      fail("upstream_protocol_error", "MiMo 响应缺少 audio.data");
    }
    const format = (ctx.input.format || "mp3") === "pcm16"
      ? "pcm"
      : (ctx.input.format || "mp3");
    return {
      result: {
        kind: "media",
        mime: UPSTREAM_TO_MIME[format] || "audio/mpeg",
        body_base64: data,
      },
      finish_reason: "stop",
    };
  }

  if (ctx.operation === "speech.transcribe@1") {
    if (!message || typeof message.content !== "string") {
      fail("upstream_protocol_error", "MiMo ASR 响应缺少 text");
    }
    const usage = ctx.response.body.usage || {};
    return {
      result: { kind: "text", text: message.content },
      usage: {
        audio_seconds: usage.seconds,
        source: "upstream",
      },
      finish_reason: "stop",
    };
  }

  fail("unsupported_operation", `不支持 ${ctx.operation}`);
}
```

说明：

- TTS 的字符数是宿主从 `ctx.input.text` 推导的，插件不重复上报，避免字符口径不一致。
- ASR 的音频秒数来自上游 `usage.seconds`；没有就留空，不猜。
- `lax.media.readBase64` 受 manifest 的 `read_bytes` 限制；更大的音频应改用 `multipart` + `MediaRef`，或在后续 ABI 中增加流式上传。
- 示例没有 `speed` 字段；语速要求通过 `instructions` 传给上游，避免插件声明一个上游不支持的参数。
- `fail` 只是把 `lax.error` 包了一层的示例；插件也可以从 hook 返回结构化错误，宿主都会重新校验错误码。

对应的 TTS fixture 输入可以就是这次需求里的句子：

```json
{
  "operation": "speech.synthesize@1",
  "model": "mimo/mimo-v2.5-tts",
  "input": {
    "text": "老大! 我们完成了 mimo 的语音模型接入啦!",
    "voice": "冰糖",
    "format": "mp3",
    "instructions": "轻快上扬，语速稍快，带着压抑不住的激动与小骄傲，声音明亮有活力",
    "language": "zh-CN"
  }
}
```

### 27.2 纯声明式 provider

一个 OpenAI-compatible 上游不需要 `index.js`：

`lax.plugin.json`：

```json
{
  "schema": "lax.plugin/v1",
  "id": "com.example.river",
  "name": "River",
  "version": "1.0.0",
  "kind": "provider",
  "runtime": "declarative",
  "abi": "lax.plugin/v1",
  "requires": ["operation:chat.complete@1"],
  "permissions": {
    "network": {
      "mode": "host-proxy",
      "schemes": ["https"],
      "hosts": ["api.river.ai"],
      "redirects": "none",
      "max_control_calls": 4
    },
    "secrets": {"plugin": [], "account": ["api_key"], "mode": "host-inject"},
    "filesystem": "none",
    "process": "none",
    "env": "none"
  },
  "providers": [
    {
      "id": "river",
      "name": "River",
      "base_url": "https://api.river.ai/v1",
      "catalog": "./models.json",
      "dialects": "./dialects.json"
    }
  ]
}
```

`dialects.json`：

```json
{
  "schema": "lax.dialect/v1",
  "dialects": [
    {
      "id": "com.example.river/openai-chat@1",
      "operation": "chat.complete@1",
      "upstream_wire": "openai-chat",
      "transport": {"request": "json", "response": "json", "streaming": "sse"},
      "implementation": {"prepare": "declarative", "decode": "declarative"},
      "request": {
        "method": "POST",
        "path": "/chat/completions",
        "body": {
          "model": {"model": "upstream_id"},
          "messages": {"input": "messages"},
          "stream": {"input": "stream"},
          "temperature": {"input": "temperature", "omit_if_empty": true},
          "max_tokens": {"input": "max_output_tokens", "omit_if_empty": true},
          "tools": {"input": "tools", "omit_if_empty": true},
          "tool_choice": {"input": "tool_choice", "omit_if_empty": true}
        }
      },
      "response": {
        "mode": "sse_json_pointer",
        "text_pointer": "/choices/0/delta/content",
        "finish_pointer": "/choices/0/finish_reason",
        "usage_pointer": "/usage"
      }
    }
  ]
}
```

`models.json`：

```json
{
  "models": [
    {
      "model": "river/deepseek-v4.1-flash",
      "upstream_id": "deepseek-ai/DeepSeek-V4.1-Flash",
      "operations": {
        "chat.complete@1": {
          "dialect": "com.example.river/openai-chat@1",
          "capabilities": {
            "supported": true,
            "streaming": true,
            "tools": true,
            "reasoning": true,
            "formats": "unknown",
            "limits": {"context": 1048576, "max_output": 384000}
          }
        }
      }
    }
  ]
}
```

这类定义可以执行，不加载 JS，也没有 `fetch`。复杂流式、动态 header、模型列表函数和 OAuth 仍需要 sandboxed 或 manual 导入。

### 27.3 Transform 中间件

一个只改请求参数、不碰网络的 transform：

`lax.plugin.json`：

```json
{
  "schema": "lax.plugin/v1",
  "id": "com.example.safe-defaults",
  "name": "Safe defaults",
  "version": "1.0.0",
  "kind": "transform",
  "runtime": "moejs",
  "entry": "./index.js",
  "abi": "lax.plugin/v1",
  "requires": ["operation:chat.complete@1"],
  "permissions": {
    "network": {"mode": "none"},
    "secrets": {"plugin": [], "account": [], "mode": "none"},
    "filesystem": "none",
    "process": "none",
    "env": "none"
  }
}
```

`index.js`：

```js
export async function on_request(ctx) {
  const request = ctx.request;
  const body = request.body.value;

  if (body.temperature === undefined) {
    body.temperature = 0.7;
  }
  if (body.max_tokens === undefined && body.max_output_tokens !== undefined) {
    body.max_tokens = body.max_output_tokens;
  }

  lax.log("debug", "applied safe defaults");
  return { request };
}
```

约束：

- transform 不能改 `model`、`account`、provider、dialect 或计费。
- 它返回的是规范请求，不是上游原始报文。
- 默认失败关闭；如果 transform 抛错，请求不会继续发给上游。
- 它没有网络和 secret 权限；需要读环境变量或调用外部的，不属于 transform。

## 28 取舍、待定问题与验收

### 28.1 关键取舍

| 取舍 | 选择 | 理由 | 代价 |
| --- | --- | --- | --- |
| 运行时 | 默认 `moejs`，`trusted-worker` 例外 | 进程内、可限资源、可池化；社区 provider 仍有大量 Node/Bun 依赖，不能假装全兼容 | 28/35 的 Magpie provider 需要人工迁移 |
| 数据面 I/O | `RequestPlan` 由宿主发送 | 凭据、SSRF、重定向、代理、TLS、重试、记账、错误形状都只有一份实现 | 插件不能再拿裸 `fetch` 为所欲为，某些 Magpie 插件需兼容桥 |
| Operation | 语义动作，不按 HTTP 路径 | 客户端协议、上游协议和业务语义可以独立演进 | 每类新能力要定义 ABI，不能临时塞字段 |
| Dialect | 声明/选择层，不是新运行时 | 上游破坏性变化可以 `@2` 平滑迁移，核心不发版 | manifest/dialect 文件更多，安装时要校验冲突 |
| 声明式映射 | 有限字段映射 | 可静态审计、零代码执行、适合长尾 OpenAI-compatible 上游 | 复杂转换必须写 sandbox hook |
| 能力 | `true`/`false`/`"unknown"` | 不把“没声明”伪装成“支持” | `"unknown"` 默认拒绝，导入时需要补声明或探活 |
| 插件状态 | 每请求 Runtime + 宿主 cache | 防止跨请求串数据，避免模块全局泄漏 | 插件不能依赖进程内长期状态，需显式缓存 |
| 更新 | 版本不可变、原子切换、手动手控 | 可回滚、可审计、权限扩大可拦截 | 运维步骤比“覆盖文件重启”多 |
| Magpie | 导入子集，不做 Bun 兼容 | 全权限宿主不适合作为 lax 默认执行面 | 不能宣称“所有 Magpie 插件开箱即用” |
| New API | 借鉴 manifest、host fetch、pool、校验 | 这些是经过实践的宿主边界 | 不复制原生 route、异步任务和配额模型 |

### 28.2 为什么不直接上 Bun

直接复用 Bun 的吸引力是“Magpie 插件立刻可跑”，但代价是：

- 插件默认能读文件、起进程、读环境变量、访问任意网络。
- 需要随 lax 分发或下载第二个运行时，跨平台、代理、musl、版本兼容问题会进入核心。
- 凭据保护、SSRF、审计和失败隔离都更难做。
- Magpie 自己的 provider 插件与 middleware 也不是同一运行时；把两者混为一谈会误判兼容性。

所以第一版先把可静态审计的 provider 与 `moejs` 沙箱做好。确实需要厂商 CLI 或文件凭据的，再放进显式授权的 `trusted-worker`。这是产品边界，不是实现偷懒。

### 28.3 为什么不直接暴露 `fetch`

`fetch` 是插件系统最难收回的权限：

```text
fetch(url)
  -> URL 由插件决定
  -> DNS 由插件间接触发
  -> 重定向由上游决定
  -> 响应体可能无限大
  -> 凭据可能被拼进 header
  -> 代理/TLS 配置出现第二份
```

`RequestPlan` 把“想请求什么”和“怎么安全地请求”分开。Magpie 的 `loader.fetch` 兼容桥也遵守同一原则：插件可以表达请求，宿主决定能否发出、发到哪里、带什么凭据、多久超时、多大响应。

### 28.4 待评审决策

以下问题需要在合并 `DESIGN.md` 前明确，不能留到实现时各写一套：

1. **`"unknown"` 的默认行为**：本文选择默认拒绝。是否允许模型级 `allow_unknown: true` 作为迁移开关？若允许，必须默认告警并在 `plugin inspect` 标红。
2. **内置 MiMo 的归属**：用随二进制发布的 `system` 插件，还是保留 Go 适配器但走同一 operation 接口？两者都能满足设计，但文档和代码结构只能选一个主路径。
3. **一个插件是否允许同时有多个活动版本**：本文选择“单活动版本 + 已安装旧版本可回滚”。若未来要按模型灰度，需要把版本选择下推到模型绑定，不能偷偷改全局指针。
4. **`loader.fetch` 是否进入第一阶段**：本文把它放到 Magpie 阶段。若提前支持，必须先有 host 白名单、流式上限和响应视图，不能先给裸 `fetch`。
5. **OAuth 是否算第一版能力**：本文把 OAuth 放到阶段 4 评估。它需要本地回调、刷新锁和 secret 生命周期，不能只加一个 `oauth: true` 字段。
6. **签名与来源**：是否强制要求发布者签名？本文允许未签名安装但标 `unverified`，且不自动更新。若项目要面向公共插件市场，需要再收紧。
7. **插件缓存的持久性**：本文只承诺宿主托管的 TTL 缓存，不承诺跨重启。若插件需要持久状态，必须显式申请并定义迁移，不能让插件自己写文件。
8. **数据库迁移方式**：`AGENTS.md` 提到迁移文件，当前 `DESIGN.md` 描述 `AutoMigrate`。合并时要统一，本文不写具体迁移编号。
9. **媒体上限**：64 MiB 是当前建议值；TTS 通常更小，ASR 可能更大。是否按 operation 设置不同上限，需要实测。
10. **多实例部署**：本文不解决多副本一致性。插件安装、活动版本和缓存都以单进程/单数据库为准；集群不在目标内。

### 28.5 评审验收问题

评审时用下列问题检验设计是否真的解耦：

- 新增一个 OpenAI-compatible TTS 上游，能否只安装声明式插件，不改 Go、不发 lax 版本？
- MiMo 的上游字段变化，能否只发 `...@2` dialect，不改网关、计费、错误形状和客户端？
- 去掉 MiMo 核心分支后，TTS/ASR 的请求、响应、用量和错误是否与现有实现逐条对拍？
- 插件能否在不知道账号 key 的情况下完成认证请求？
- 插件能否访问 `169.254.169.254`、`127.0.0.1`、未声明 host 或通过重定向绕过白名单？
- 插件崩溃、死循环、内存超限、返回非法 JSON 时，网关是否仍能服务其他 provider？
- `unknown` 能力是否会在 `/v1/models` 里被误报成可用？
- 插件更新扩大权限时，是否必须重新确认？
- 删除插件后，模型是否明确不可用，而不是静默换到另一个方言？
- Magpie 导入失败时，是否给出具体 API 和不兼容原因，而不是运行期 500？
- 所有测试是否只依赖 `httptest` 与 fixture，不连接真实厂商？
- `make check` 是否通过，文档是否已合并进 `DESIGN.md`？

### 28.6 建议的评审结论

如果上述问题没有阻塞项，建议按以下顺序推进：

1. 先把本文合并进 `DESIGN.md`，尤其是第 2、4、5.7、5.8、5.16、6、7、8、9 节。
2. 实现声明式 provider 与模型/dialect 目录，先解决“新增兼容上游不改代码”。
3. 把 MiMo TTS/ASR 迁移到同一 ABI，用现有行为做对拍，确认核心不再认识 MiMo。
4. 再引入 `moejs` 沙箱与 `RequestPlan`，以安全用例作为上线门槛。
5. Magpie 导入与 `trusted-worker` 分开推进，不以“兼容全部插件”为目标。

这样做的结果是：插件系统先解决长尾 provider 的配置问题，再解决复杂转换，再考虑第三方生态兼容。每一步都有可回滚的边界，也不会在第一次发布时把不受控的 Bun 运行时塞进核心。

## 附录：调研来源

- MiMo 语音文档：`https://mimo.mi.com/docs/zh-CN/quick-start/usage-guide/audio/Speech-Recognition`。
- Magpie 文档与插件说明：`https://usemagpie.ai/docs/plugins`。
- Magpie 社区插件：`https://github.com/magpie-community/plugins`。
- moejs：`https://github.com/MoeForge/moejs`。
- New API：`https://github.com/QuantumNous/new-api`，重点参考 Task Plugin API v1 的 manifest、`utils.fetch`、运行池与严格校验；不复制其原生 route 与任务模型。
