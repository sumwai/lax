# lax

本机上的模型总机。账号在命令行里管，所有客户端只连一个地址。

```bash
lax init                                            # 建配置、数据库与密钥
lax provider ls                                     # 看已知厂商与各协议的路径
lax account add ds --provider deepseek --key sk-xxx  # 或用 --key - 从标准输入读
lax account set ds --key sk-yyy                     # 换 key：不删号，冷却与失败计数清零
lax model sync --account ds                         # 拉上游模型列表
lax model probe --account ds                        # 逐个探活，能用的才进选号
lax serve                                           # 起网关，127.0.0.1:4567
lax log tail                                        # 看请求记录
```

四条对话线协议能进也能出（第五条 `systemone` 是决策协议，只与自己互通、不参与转换；第六条 `openai-embeddings` 是向量化，第七条 `openai-speech` 与第八条 `openai-transcriptions` 是语音，出站按模型绑定的方言走同协议直通或 chat 方言，见下面「向量化」「决策请求」「语音」与「插件与方言」）。入站按路径分 anthropic、openai-chat、openai-responses 与 gemini，出站按厂商行上那家厂商的默认协议（探活发现某个模型该走别的协议时，结论记在 `account_models.protocol` 上优先用它），取值同为这四条；入站与出站相同则直通，不同则经中间表示转换，四条入站 × 四条出站共十六种组合，流式与非流式都可用，工具调用包含在内（这四条之外的 `systemone` 与 `openai-embeddings` 只做同协议直通，见「决策请求」与「向量化」）。中间表示、映射规则、选号规则、数据模型、里程碑见 [DESIGN.md](DESIGN.md)。

## 命令

| 命令 | 作用 |
| --- | --- |
| `init` | 建配置目录、数据库、加密密钥 |
| `serve` | 前台起网关 |
| `account add\|ls\|set\|rm\|test\|enable\|disable` | 加账号、列账号、换凭据、删账号、探活、启用、停用 |
| `provider ls\|add\|set\|unset\|rm` | 维护厂商默认地址、默认协议与各协议的路由；`add` 只新建，`set` 只设值，删除走 `unset` |
| `plugin install\|update\|rollback\|verify\|logs\|reload\|config\|ls\|show\|enable\|disable\|rm\|doctor` | 装本地目录里的插件（声明式或 `moejs` 沙箱）：登记它带来的厂商、模型与方言，升版本、回滚、校验、看日志、热加载、读写配置项、启停与自检；包哈希对不上的插件不加载 |
| `model ls` | 列出模型、可用账号与状态，可按厂商与账号筛选；`--json` 带每个账号的协议、HTTP 状态、探活时间与错误 |
| `model show` | 按账号看一个模型的可达性事实：协议、状态、HTTP 状态、探活时间与错误；只读库，不发上游请求 |
| `model add <模型> --account <账号> [--provider <厂商>] [--protocol <协议>]` | 手写一条「这个账号能用这个模型」，同事务写模型目录与账号可达性；可重复执行，决策模型与不在上游清单里的模型靠它声明 |
| `model sync` | 从上游拉模型列表，写进本地，可按厂商与账号筛选 |
| `model probe` | 逐个模型探活，把能用的写进账号，可按厂商与账号筛选；`--protocol` 只探指定的那一条协议 |
| `model alias set\|unset\|ls` | 模型别名：把客户端写死的模型名指到真实模型名上，只影响请求方向 |
| `model price set\|unset\|ls` | 模型单价：目录里的厂商级标价与某账号的覆盖价，`usage` 与 `model ls` 都读它 |
| `model dialect ls\|set\|unset` | 直接读写模型与操作的方言绑定：看哪个模型绑了哪条方言，改绑或删掉一条 |
| `model capability add\|set\|unset\|ls` | 模型能力声明：能力位（`speech` / `transcription`）、音色、音频格式与 `--dialect` 的按操作方言绑定；语音端点只打声明过对应能力、绑定过方言的模型，`/v1/models` 也输出这些声明 |
| `model call` | 手工发一条对话请求，打印选号过程；默认 anthropic 入站，`--protocol` 可切换四种入站，gemini 也支持 `--stream` |
| `model decide` | 手工发一条 systemone 决策请求（`--state` 加一组问题），打印每个问题的答案与概率；`--json` 给脚本用 |
| `model speak` | 手工发一条语音合成请求：`--model` 加 `--text`，音频写标准输出或 `--out <文件>`，`--voice` 与 `--format` 可选 |
| `model transcribe` | 手工发一条语音转写请求：`<音频文件或 ->` 加 `--model`，`--language`、`--prompt` 与 `--format` 可选，响应体原样输出 |
| `usage` | 按账号、模型或日期汇总用量，含输入、输出与缓存读写 token |
| `log tail` | 最近的请求记录 |
| `doctor` | 自检配置、目录、密钥、端口、数据库 |

每个命令都支持 `--json`，结果走 stdout，错误走 stderr。不带子命令等同 `--help`：`lax`、`lax model`、`lax plugin` 这类组都打印帮助到 stdout、退出码 0。退出码：0 成功、1 运行失败、2 用法错误。

## 入站协议

路径决定入站协议，八条路径同时可用，一个网关可以同时接多类客户端（另有两条只读的模型清单接口 `GET /v1/models` 与 `GET /v1/models/{id}`，见下面「客户端怎么接」）：

| 入站路径 | 入站协议 | 常见客户端 |
| --- | --- | --- |
| `POST /v1/messages` | anthropic | Claude Code |
| `POST /v1/chat/completions` | openai-chat | 普通 OpenAI SDK 客户端 |
| `POST /v1/responses` | openai-responses | Codex |
| `POST /v1/embeddings` | openai-embeddings | 需要向量化的 OpenAI SDK 客户端 |
| `POST /v1/audio/speech` | openai-speech | 用 `audio.speech.create` 的 OpenAI SDK 客户端 |
| `POST /v1/audio/transcriptions` | openai-transcriptions | 用 `audio.transcriptions.create` 的 OpenAI SDK 客户端 |
| `POST /v1beta/models/{model}:generateContent`<br>`POST /v1beta/models/{model}:streamGenerateContent`<br>`POST /v1beta/models/{model}:embedContent` | gemini | 直接用 Google SDK 或原生 `generateContent` / `embedContent` 的客户端 |

gemini 的模型名在路径里（请求体里没有 `model` 字段）：`:` 之后的后缀不算名字的一部分，`{model}` 参与选号，名字里带 `/` 时按路径段转义成 `org%2Fgemini` 也能取到；流式由 `:streamGenerateContent` 后缀决定，不看请求体。两条路径都只收 POST：

```bash
# 非流式；流式把路径换成 :streamGenerateContent，响应是 SSE（data 帧，末尾一帧带 finishReason）
curl -s http://127.0.0.1:4567/v1beta/models/deepseek-v4-pro:generateContent \
    -H 'Content-Type: application/json' \
    -d '{"contents":[{"role":"user","parts":[{"text":"用一句话介绍你自己"}]}]}'
```

出站协议由厂商决定，账号只存厂商名：地址与默认协议都是那家厂商行上的值（迁移 `0014` 之后账号不再有 `base_url`、`protocol` 两列），所以 `account add` 没有 `--base-url` 与 `--protocol` 两个参数——先 `provider add` 把厂商登记进去，再建账号。默认协议的取值是 `anthropic`、`openai-chat`、`openai-responses`、`gemini`、`systemone`、`openai-embeddings`、`openai-speech` 与 `openai-transcriptions`（gemini 的原生路径是 `{base}/v1beta/models/{model}:generateContent`，流式换成 `:streamGenerateContent?alt=sse`，凭据头是 `x-goog-api-key`；systemone 是决策协议，入站与出站都是 `POST /v1/systemone`，只与自己互通，见下面「决策请求」；openai-embeddings 是向量化，openai-speech 与 openai-transcriptions 是语音，都见下面「向量化」与「语音」）：`provider add --default-protocol` 显式存的优先，没显式存则按 `openai-chat` → `openai-responses` → `anthropic` 取第一条路由。改一家厂商的地址或默认协议立刻影响所有用它的账号（这是单一来源应有的行为）。厂商与各协议的路径在数据库里，`lax provider ls` 能看到，`provider add` / `set` / `unset` 可以增删改，不必重编译。入站协议与出站协议相同时直通，请求体只改 `model` 字段，其余字段与值原样带到上游，响应按字节原样返回（gemini 的报文里没有 `model` 字段，它直通时请求体一个字节都不改）；不同时经中间表示转换，请求与响应都只搬映射表里定义的字段，表外字段丢弃。未知路径返回 404，请求体解析失败返回 400，错误消息带协议名前缀。

自己加一家厂商时，每个协议一条 flag（`--anthropic`、`--openai`、`--responses`、`--gemini`、`--systemone`、`--embeddings`、`--speech`、`--transcriptions`），值有两种写法：相对 `--base-url` 的完整路径，或者整条地址。整条地址给同一厂商不同协议分域名的情况用，写进去直接使用、不再拼 `--base-url`，例如 `--openai https://openai.example.com/v1/chat/completions --anthropic https://claude.example.com/v1/messages`；整条地址要带主机名，`https://` 这种只有 scheme、`https:///x` 缺主机名与 `ftp://host/x` 这类别的 scheme 都是用法错误（退出码 2）。`--base-url` 可以省略，条件是每个要声明的路由（以及给了的话 `--models-path`）都是整条地址：那时它拼不进任何地址，省掉它账号的地址也留空，转发与模型同步全走整条地址。bigmodel 就是这种形状——三套协议分属不同 host 与路径，模型列表在 chat 路径的同级：

```bash
lax provider add bigmodel \
    --anthropic https://open.bigmodel.cn/api/anthropic/v1/messages \
    --openai https://open.bigmodel.cn/api/coding/paas/v4/chat/completions \
    --responses https://open.bigmodel.cn/api/v1/responses
```

有一条路由是相对路径时缺 `--base-url` 仍是用法错误（退出码 2），文案指出是哪条路由，并给出两条出路：给 `--base-url`，或把这条路由写成整条地址。给了地址就必须是带主机名的完整地址，`https://` 这种只有 scheme 的写法会被拒；scheme 大小写不敏感，`HTTPS://api.example.com/` 能用，落库时归一成小写并去掉末尾斜杠。路径写法不用纠结：`v1/chat/completions`、`//v1//chat/completions`、`/v1/chat/completions/` 由程序规整成同一个地址，不报错；`--models-path` 与 `model sync --endpoint` 同理：`v1/models`、`//v1//models/` 都落到 `/v1/models`，也可以写整条地址、直接访问那个地址。只有规整之后为空的取值（`--openai ""`，或只有斜杠）算用法错误，删一条路由、清一项设置用 `provider unset`。`--default-protocol` 定下这家的默认协议，取值写 `anthropic`、`openai`、`responses`、`gemini`（也认线协议全名 `openai-chat`、`openai-responses`，`gemini` 两种写法同形），必须落在已声明的路由上：

```bash
# 新建厂商：至少一条协议路由；--base-url 只在路由是相对路径时才需要
lax provider add myco --base-url https://api.deepseek.com \
    --openai /v1/chat/completions \
    --anthropic /anthropic/v1/messages \
    --default-protocol openai
已添加厂商 myco（https://api.deepseek.com），2 条路由，默认协议 openai-chat
```

gemini 的入站与出站都可用，原生接口把模型名放在路径里，所以它的路由可以写 `{model}` 占位符：占位符原样落库，发请求之前才换成模型名（按路径段转义），相对路径与整条地址两种写法都行。`google-gemini` 这家（`https://generativelanguage.googleapis.com`）已经内置，模型列表在 `/v1beta/models`、转发在 `/v1beta/models/{model}:generateContent`，`provider ls` 能看到；要指向自己搭的代理就照同一个形状另加一家：

```bash
# 只声明 gemini 一条路由，默认协议跟着写成 gemini
lax provider add gem-proxy --base-url https://gemini-proxy.example.com \
    --gemini /v1beta/models/{model}:generateContent \
    --default-protocol gemini
已添加厂商 gem-proxy（https://gemini-proxy.example.com），1 条路由，默认协议 gemini
```

删掉这条路由是 `provider unset gem-proxy gemini`；这家只有这一条路由时会被拒绝，那是不想要这家厂商了，用 `provider rm`。账号一侧用 `account add --provider google-gemini`（这家的默认协议已经是 gemini；自建代理就把 `--provider` 换成那家的名字），gemini 的入站是 `POST /v1beta/models/{model}:generateContent` 与 `:streamGenerateContent`，非流式与流式都能用。

`provider set` 只设值：补一条路由、改默认协议或地址，没传的字段一律保留。传空值是用法错误，删除要显式声明：

```bash
# 补一条 responses 路由，并把默认协议换掉
lax provider set myco --responses /v1/responses --default-protocol responses

# 删一条路由；也可以一次删多个（models-path、default-protocol、note 同样可用）
lax provider unset myco anthropic
```

`provider unset` 幂等：要删的项本来就不存在时不报错，只说明一句「myco 本来就没有 anthropic 路由」。删到一条路由都不剩会被拒绝，那是不想要这家厂商了，用 `provider rm`。`--models-path <路径>` 是厂商级的模型列表覆盖，`provider unset myco models-path` 清掉它、回到按各路由取。

凭据头默认按线协议推导：anthropic 用 `x-api-key`，gemini 用 `x-goog-api-key`，两条 openai 协议与 systemone 用 `Authorization: Bearer`。有的上游不是这个写法（豆包要 `X-Api-Key`），这时按厂商声明两项：

```bash
# 头名与写法一起声明：头名的落库形态是小写
lax provider add doubao-voice --base-url https://openspeech.bytedance.com \
    --auth-header X-Api-Key --auth-scheme raw \
    --speech /api/v3/tts/unidirectional --transcriptions /api/v3/auc/bigmodel/recognize/flash

# 只换写法（头不改）：raw 直接写 key，bearer 写 "Bearer <key>"，bearer-semicolon 写 "Bearer;<key>"
lax provider set doubao-voice --auth-scheme bearer

# 清掉声明，回到按线协议推导
lax provider unset doubao-voice auth-header
```

声明之后上游只收这一个头，协议默认的凭据头一个不发：两个头同时出现会被上游当成两种凭据。只给 `--auth-scheme` 而不给 `--auth-header` 是用法错误（没有头就没有地方写值）；`provider ls --json` 的 `auth_header` 与 `auth_scheme` 是头名与写法，不是秘密（key 在账号行上加密存）。规则见 DESIGN 5.8。

有些上游除凭据头之外还要额外的头才路由（opencode 的 Go 端点缺了 `x-opencode-session` 直接 400）。这类头按厂商声明，写进数据库、不用重编译：`--protocol` 不给就是厂商级，这家厂商的每条路由都带上它；给了就只加给那条协议，同名头协议级覆盖厂商级。值四种写法：字面量原样发出，`{env:NAME}` 取环境变量（适合不该落库的会话 token），`{session}` 取本次请求的会话键（一个对话一个稳定值），`{model}` 取本次请求的模型名，都是转发前才展开。值可能是密钥，所以 `ls` 与 `--json` 只输出脱敏后的前四位：

```bash
# 只加给 openai-chat 那条路由，值随对话稳定
lax provider header set opencode x-opencode-session '{session}' --protocol openai-chat

# 看这家声明了什么：厂商 / 协议（厂商级显示 -）/ 头名 / 值（脱敏）
lax provider header ls opencode

# 删一条：删不到会报错并列出这家现有的声明
lax provider header unset opencode x-opencode-session --protocol openai-chat
```

完整规则（四种写法、与凭据头冲突时声明优先、不进请求记录）见 DESIGN 5.10。

换 key 不删号重建：`account add` 时那份 key 失效了，用 `account set` 覆盖掉就行，探活结果、协议发现与请求记录的账号归属都留着。旧凭据留下的冷却与失败计数在同一个事务里清零，账号重新启用：

```bash
lax account set ds --key sk-yyy          # 直接写值
lax account set ds --key - < key.txt     # 从标准输入读
LAX_KEY=sk-yyy lax account set ds         # 留空则读环境变量 LAX_KEY
```

`update` 是 `set` 的同义词。`--json` 只回显掩码，明文不进输出；换完 key 不用再跑 `account add`，也不影响已经探出来的模型状态。

`account test` 判的是凭据与连通性，不是某个模型能不能用，所以它分两段。先 `GET` 模型列表证明地址通：地址与凭据头跟 `model sync` 同一套规则（厂商覆盖 → 该账号的默认协议那条路由的 `models_path`，整条地址直接访问，凭据头跟取到地址的那条路由的协议），2xx 说明上游连得上，顺带报出上游模型数，这一条不花 token。列表 2xx 之后**仍然**再发一条 `max_tokens=1` 的请求证明凭据有效：有的上游列表接口是公开的，任何 key 都回 200（commandcode 就是），光看列表判不出 key 好不好。这家没有列表接口（404、405）、列表被网关拦住（403）或连不上时也走这条请求，模型取账号模型列表第一项，为空时用默认模型名 `default` 并在备注里说明。两段都通过才判正常，状态与退出码按调用那段算；备注与 `--json` 写清两段各自的结果（`list` 对象是第一段的）。某个模型到底能不能用由 `model probe` 决定。

```bash
lax account test ds
账号  状态  HTTP  耗时   上游模型数  备注
ds    正常  200   412ms  47          模型调用可用（模型 deepseek-chat）；列表接口可用（GET https://api.deepseek.com/v1/models，上游 47 个模型）
```

## 模型从上游拉，再探活

模型列表不用手填。`model sync` 拉上游的模型清单，`model probe` 逐个试一遍，能用的才进入选号。两条命令都接受 `--provider` 与 `--account`，不给就处理全部账号：

```bash
lax account add ds --provider deepseek --key sk-xxx
lax model sync                                     # 全部账号各拉一次模型列表
lax model sync --provider deepseek --account ds   # 只处理这一个账号
lax model sync --account ds --endpoint /v1/models # 临时指定列表路径，只影响这一次调用
lax model probe --account ds --limit 5            # 只探这个账号的前 5 个模型
lax serve
```

模型列表的形状不用管：`{data:[…]}`、`{models:[…]}`、顶层数组三种都认，gemini 的 `{models:[{"name":"models/…"}]}` 走第二种，清单按 `supportedGenerationMethods` 过滤、名字去掉 `models/` 前缀；分页带 `pageSize=1000`，有 `nextPageToken` 就接着取，最多 10 页。

探活会真实调用上游：`--limit 5` 限制每个账号这一次最多探几个，`--model deepseek-v4-pro` 只试一个。某个账号没探出可用模型时只写探活结果，它原有的模型列表保持不变；全部账号都没探出可用模型时退出码 1。终端里探活时 stderr 上有一行原地刷新的进度（账号、已完成 / 待探数、最近探完的模型），账号探完补一个换行再打汇总与表格；stderr 不是终端或加了 `--json` 时没有这一行，管道、CI 与日志里不会多出噪声。碰到 401 时命令停下并停用该账号：凭据被拒，后面的结论不作数。同步失败的账号同样不影响其余账号，只是最终退出码为 1。`account ls` 与 `model ls` 会带上每个账号的模型状态（可用 / 待探活 / 无权 / 不存在 / 凭据被拒 / 失败）。空模型列表的账号不参与选号，所以 `account ls` 的模型列不会写「不限」：只有手填的名字时直接列出模型名，同步过但没探出可用模型时写「0 可用 / N 同步」，完全没有模型信息时写「无模型（先 model sync）」；`model ls` 一条模型都没有时同样把下一步指到 `model sync` 与 `model probe`。

模型名还可以起别名。客户端里的模型名往往写死（`claude-3-5-sonnet`、`gpt-4o`），而账号声明的是上游的真实名字；`model alias` 把那个写死的名字指到一个真实模型名上，网关按真名选号、改请求体与路径，客户端不必改：

```bash
lax model alias set fast glm-4.6   # 客户端请求 fast 时按 glm-4.6 走；同名再写一次就是改目标
lax model alias ls                 # 别名 / 目标 / 状态 / 解析
lax model alias ls fast            # 只看一条
lax model alias unset fast
```

别名只影响请求方向：响应不重写，里面的模型名仍是上游的真实名字。客户端请求别名时，请求记录里的请求模型名与使用模型名会不同，`log tail` 的模型列打的是客户端请求的那个名字，一次别名命中一眼可见。别名精确匹配、区分大小写，可以指向另一个别名，链最长 8 跳；名字已经被某个账号或模型目录当成真实模型时写入会被拒绝，否则别名会遮蔽真名。目标眼下没有账号提供也能先写，之后加账号即生效；`model alias ls` 的解析列写的是链走到的真实名字，链断了（成环、超过 8 跳）时显示成不可解析并给出原因。

模型单价也挂在模型目录上，不在账号上：同一家上游的同一个模型只有一份标价，否则改一次价要改 N 个账号，`usage` 还会给同一件事算出 N 个单价。管理员先用 `model price set` 定厂商级标价，套餐真的不同时才对某个账号加一份覆盖价：

```bash
lax model price set glm-4.6 --in 0.6 --out 2.2           # 厂商级标价，模型名只在一家目录里时不用 --provider
lax model price set glm-4.6 --in 0.6 --out 2.2 --provider bigmodel  # 同名模型出现在多家时用 --provider 指定
lax model price set glm-4.6 --account ds --out 2.5       # 只给这个账号的覆盖价，不影响别的账号
lax model price ls                                       # 模型 / 供应商 / 输入价 / 输出价 / 缓存读 / 缓存写
lax model price ls glm-4.6 --account ds                  # 目录价与这个账号的有效价并排
lax model price unset glm-4.6 cache-read                 # 清掉某一项；清第二次不报错，只说「本来就没有」
```

价格单位是每百万 token，0 合法（免费或打包），负数是用法错误。只写命令行上给过的那几项，没给的留空表示继承：账号级的「留空」就是回到厂商级标价（`model price unset` 同理，账号级清掉不是「免费」），缓存读写没定价时按输入价算（`model price ls` 的缓存那格写「回退输入价」）。`account add` 与 `account set` 因此不再收 `--price-*`：误敲会得到一个指向 `model price set` 的用法错误。

## 向量化

向量化没有消息序列：输入一段文本或一组文本，输出一组向量，没有角色、没有工具、没有流式。它与对话协议不相通，因此不进中间表示，只做同协议直通。入站两条：`POST /v1/embeddings`（OpenAI 报文，入站协议 `openai-embeddings`）与 `POST /v1beta/models/{model}:embedContent`（gemini 报文）。

向量化端点是否存在由供应商决定，因此在厂商上声明一条路由（`openai` 与 `ollama` 已内置）：

```bash
lax provider set openai openai-embeddings=/v1/embeddings
```

没有声明这条路由时 `/v1/embeddings` 返回 400 并给出上面这条命令，不会猜一个地址去打。gemini 的 `:embedContent` 用 gemini 协议本身：路由里存的是对话动作，发请求前把后缀换成 `:embedContent`，内置的 `google-gemini` 不需要额外配置。

| 项 | 行为 |
| --- | --- |
| 请求体 | `/v1/embeddings` 只换 `model`（别名照旧解析），`input`、`encoding_format`、`dimensions` 原样；gemini 一个字节都不改 |
| 校验 | `model` 非空字符串、`input` 存在；带 `"stream": true` 返回 400（向量化没有流式） |
| 响应 | 按字节原样返回 |
| 用量 | `usage.prompt_tokens` 记输入，输出与思考记 0；gemini 的 `:embedContent` 不报用量，记 0/0 |
| 模型清单 | 只能做向量化的条目不再被丢掉：gemini 的 `embedContent` 与清单里的 `/embeddings` 端点都会让条目进目录，并记上 `embedding` 能力位 |
| 探活 | 供应商声明了向量化路由时，对话那条失败后再拿 `{"model":…,"input":"lax probe"}` 试一次；gemini 的能力位只有 `embedding` 时直接打 `:embedContent` |

## 语音

语音有两条线协议，报文都按 OpenAI 的形状：`openai-speech`（文本转语音，`POST /v1/audio/speech`）与 `openai-transcriptions`（语音转文本，`POST /v1/audio/transcriptions`）。出站形状由模型绑定的**方言**决定：绑到宿主直通方言 `lax/openai-speech@1` / `lax/openai-transcriptions@1` 才同协议直通（只换 `model`，音频或 JSON 字节原样）；插件方言即使 `upstream_wire` 与入站协议相同，也由它的 `Prepare` / `Decode` 从零编解码，所以豆包这类“同线协议、不同报文”的厂商仍能接入。绑到 `lax/mimo-chat-speech@1` / `lax/mimo-chat-asr@1` 时由内置 chat 方言把语音请求编成 chat completions、再把响应解回音频字节或转写结果。

**先声明能力，再绑方言，再调接口。** 能力位决定一个模型能不能用于语音端点：没声明的模型（例如一个普通对话模型）在本地就回 400，一条上游调用都不会发。音色与音频格式同样要声明，`/v1/models` 会把它们带出去。方言绑定决定上游形状：没有绑定也是 400，文案给出带 `--dialect` 的下一步命令；绑定的方言没注册（插件被停用或删掉）同样 400，且不回落。

```bash
# 真正的 OpenAI audio 厂商：声明路由，绑直通方言
lax provider set openai openai-speech=/v1/audio/speech
lax provider set openai openai-transcriptions=/v1/audio/transcriptions
lax model add gpt-4o-mini-tts --account openai1
lax model capability set gpt-4o-mini-tts --capability speech --voices alloy,nova --audio-formats mp3,wav \
    --dialect lax/openai-speech@1

# MiMo：只有 chat 路由，绑 chat 方言
lax provider add mimo --base-url https://api.xiaomimimo.com \
    --openai /v1/chat/completions --models-path /v1/models --default-protocol openai-chat
lax account add mimo1 --provider mimo --key -
lax model capability set mimo-v2.5-tts --provider mimo --capability speech \
    --voices "mimo_default,冰糖,茉莉,苏打,白桦,Mia,Chloe,Milo,Dean" --audio-formats mp3,wav,pcm16 \
    --dialect lax/mimo-chat-speech@1
lax model capability set mimo-v2.5-asr --provider mimo --capability transcription --audio-formats wav,mp3 \
    --dialect lax/mimo-chat-asr@1
```

`lax serve` 启动时会把已有的语音能力声明补成绑定（`store.BackfillSpeechDialects`，幂等）：厂商声明了对应的语音路由就绑直通方言，否则绑 MiMo 的 chat 方言。上面三条 `--dialect` 因此可以省掉；形状不是 MiMo 的 chat 上游才必须手写。

```python
from openai import OpenAI
c = OpenAI(base_url="http://127.0.0.1:4567/v1", api_key="lax-local")

c.audio.speech.create(model="mimo-v2.5-tts", voice="Mia", input="今天风很大", response_format="mp3") \
    .stream_to_file("hi.mp3")
print(c.audio.transcriptions.create(model="mimo-v2.5-asr", file=open("hi.mp3", "rb"), language="zh").text)
```

MiMo 的两条 chat 方言（`lax/mimo-chat-speech@1` 与 `lax/mimo-chat-asr@1`）的映射：

| 入站 | 出站 |
| --- | --- |
| TTS `input` | `messages` 里 assistant 的正文（数组按换行拼接） |
| TTS `instructions` | `messages` 里 user 的风格指令（没有就省略） |
| TTS `voice` | `audio.voice` 原样；空则省略 |
| TTS `response_format` | `audio.format`：mp3→mp3、wav→wav、pcm→pcm16 |
| TTS `speed` | 丢弃（上游没有这个参数） |
| ASR `file` | `messages[0].content` 里唯一的 `input_audio`（data URL + base64） |
| ASR `language` | `asr_options.language`（auto / zh / en） |
| ASR `prompt`、`temperature` | 丢弃（上游拒绝 text part） |
| ASR `response_format` | 只影响本地包装：json → `{"text":…}`、text → 纯文本、verbose_json → 带 `task`、`language`、`duration` |

本版不做转码、不做流式：ogg/opus、SILK 输入与 `opus`/`aac`/`flac` 输出都回 400，`stream_format: "sse"` 与 ASR 的 `stream: true` 也回 400，`srt`/`vtt` 与 `auto`/`zh`/`en` 以外的语言同样拒。这些都在本地拒，不会变成一次上游调用。能力声明里的 `pcm16` 与入站的 `pcm` 是同一个格式的两种叫法，本地按同一项比较。

计费分两条：直通按字符（`tts_chars`）× `--tts-char` 与音频秒（`audio_seconds`）× `--audio-second`；chat 方言按上游报的 token 计（`in_tokens` / `out_tokens` / 缓存读），`tts_chars` 与 `audio_seconds` 只记录展示、不进花费。`/v1/models` 的条目会带 `capabilities`、`voices` 与 `audio_formats`，不确定能传什么时先查它。

语音模型的探活仍按对话请求打，也没接进方言分发：声明了能力的模型用 `model add` 手写可达性（`model ls` 显示「待探活」，不影响选号）；对这类模型跑 `model probe` 会因为对话请求返回 400 把 `allowed` 改写成 `forbidden`。直通路由上的应错探活规则见 DESIGN 5.16：`400` / `422` 判可达、`404` 判不存在、`401` 停用账号，而 `400` 只证明端点存在、不证明凭据有效。

## 插件与方言

插件是本地目录里的包：`lax.plugin.json` 说明插件 ID、版本、ABI、运行时、权限与它带来的厂商，`dialects.json` 说明这些厂商的上游形状，`models.json` 说明模型与「哪个操作绑哪条方言」。运行时有两种：

- `declarative`：宿主按数据执行映射，插件不跑代码；只能承载语音两条操作。
- `moejs`：每次 hook 在进程内 JS 沙箱里执行插件代码，可以给对话、向量化与决策请求补报文，并为对话与决策请求解响应、逐块解流。

```bash
lax plugin install ./my-plugin --dry-run       # 只打印将写入的厂商、路由、模型、方言、别名与权限
lax plugin install ./my-plugin                 # 解析校验 → 包哈希 → 版本与方言冲突 → 复制到数据目录 → 写厂商目录与绑定
lax plugin install ./my-js-plugin --trust sandboxed  # 带代码的包；--trust 与 runtime 必须一致
lax plugin update com.example.echo --source ./echo-v2  # 升到新版本（不给 --yes 只打印计划并退出 1）
lax plugin rollback com.example.echo --version 1.0.0   # 把活动版本切回装过的旧版本
lax plugin verify com.example.echo             # 只读校验：哈希、运行时与方言声明，不改库、不开沙箱
lax plugin doctor                              # 完整自检：包、哈希、沙箱、导出钩子、方言登记与状态
lax plugin logs com.example.echo --follow      # 插件日志（每个插件保留最近 500 条）
lax plugin reload                              # 给正在跑的 serve 发 SIGHUP，整表换掉方言注册表
lax plugin config set com.example.echo region cn   # 读写 manifest 声明的配置项
lax plugin ls                                  # ID / 版本 / 类型 / 运行时 / 信任 / 状态 / 厂商 / 方言数
lax plugin show com.example.echo               # manifest 原文、权限、版本、厂商、方言、冲突、最近审计与日志
lax plugin disable com.example.echo            # 重启或 reload 之后不再加载它的方言
lax plugin rm com.example.echo --purge         # 删库记录、它带来的厂商与包目录
lax model dialect ls mimo-v2.5-tts             # 看模型绑了哪条方言
lax model dialect set mimo-v2.5-tts chat.complete@1 com.example.echo/chat@1
```

本机插件包放在 `plugins/<插件名>/`，默认不进版本库（见 `plugins/README.md`）。下面以本机的 `doubao-seed-tts` 为例，它同时提供豆包 Seed-TTS 2.0 与 Seed-ASR 极速版：

```bash
lax plugin install ./plugins/doubao-seed-tts --trust sandboxed --yes
printf '%s' "$DOUBAO_API_KEY" \
    | lax account add doubao --provider doubao-seed --models seed-tts-2.0,bigmodel --key -
lax model speak --model doubao-seed-tts --voice zh_female_xiaohe_uranus_bigtts \
    --format mp3 --text '你好' --out hello.mp3
lax model transcribe hello.mp3 --model doubao-seed-asr --language zh --format verbose_json

curl -s http://127.0.0.1:6543/v1/audio/speech \
    -H 'Content-Type: application/json' \
    -d '{"model":"doubao-seed-tts","input":"你好","voice":"zh_female_xiaohe_uranus_bigtts","instructions":"请用轻快、开心的语气"}' \
    --output happy.mp3
curl -s http://127.0.0.1:6543/v1/audio/transcriptions \
    -F model=doubao-seed-asr -F language=zh -F response_format=json \
    -F file=@hello.mp3
```

客户端模型名分别是 `doubao-seed-tts` 与 `doubao-seed-asr`，插件把它们解析成上游 `seed-tts-2.0` 与 `bigmodel`；路由、`X-Api-Key` 与逐请求的 `X-Api-Resource-Id` / `X-Api-Request-Id` 都由插件声明。TTS 支持 mp3 / wav / pcm，`speed` 会换算成豆包的 `speech_rate`，`instructions` 会作为 `additions.context_texts` 传给上游，用来控制语气、节奏与音量。ASR 只收 wav / mp3，返回 `json`、`text` 或 `verbose_json`，`language` 只认 `auto` / `zh` / `en`；沙箱把音频内联进请求，单文件上限 8 MiB。两条都只做非流式。插件包的目录结构与安装方式见 `plugins/README.md`，具体限制见插件包自己的 README。

安装流水线是固定的：解析并严格校验 manifest、方言文件、模型目录与三者之间的引用 → 定信任级别 → 拒掉声明式方言承载非语音操作 → 算包目录哈希 → 版本冲突（同一版本不能覆盖）→ 方言冲突（内置方言与其余已装插件一起试登记，`lax/` 前缀不许冒用）→ 复制到 `<数据目录>/plugins/installed/<插件 ID>/<版本>/`（先落 `staging/` 再整目录改名，符号链接一律拒）→ 逐家厂商一个事务写目录（厂商、路由、模型、方言绑定、别名，冲突整包回滚）→ 写 `plugin_versions`（记下安装时的 `runtime`）与 `plugins`。`--dry-run` 停在复制之前，`--no-enable` 只装不用，装第二个版本要 `--yes`。

`--trust` 只有两个取值：`declarative` 与 `sandboxed`。`runtime: "declarative"` 的包只能按声明式装，`runtime: "moejs"` 的包只能按沙箱装；对不上、或写 `trusted-worker`，都是用法错误（退出码 2）。`plugin update` 没有这个参数，按新包的 runtime 重新推断。

插件带来的厂商记在 `providers.plugin_id` 上：删除插件会连带删掉这些厂商的路由、模型目录、方言绑定与别名，并停用指向它们的账号（`last_error` 写明原因，重新安装之后要 `account enable` 才回到选号池）。同名厂商地址不一致、同名路由路径不一致、手写绑定已指向别的方言、别名已指向别的目标，四种冲突都整笔失败并给下一步命令。

对话与语音上游可以只写方言文件、引用宿主方言：`models.json` 里把操作绑到 `lax/openai-chat@1`（另外五条是 `lax/anthropic@1`、`lax/openai-responses@1`、`lax/gemini@1`、`lax/openai-embeddings@1`、`lax/systemone@1`），安装会按线协议建默认路由、按上游名落模型目录，并在客户端名字与上游名字不同时写一条别名，请求仍走宿主编解码。路由与凭据头可以在 manifest 里直接声明：`providers[].routes` 给每条线协议一个路径（写了就不再按方言推导，覆盖不到方言的 `upstream_wire` 会在安装时报错），`providers[].auth` 声明凭据头（`{"header":"X-Api-Key","scheme":"raw"}`，与 `provider add --auth-header/--auth-scheme` 同一套规则）。要改上游报文或解一种宿主不认识的响应，就写 hook 方言（`runtime: "moejs"`，`implementation.prepare` 与 `implementation.decode` 都写 `hook`）：宿主编好报文后交给它补字段，响应返回 `ResultJSON`（上游线协议形状），宿主再解成中间表示、按客户端协议编回去。插件方言可以声明任意一条语音线协议（不一定是 `openai-chat`）：它与入站同协议时仍由它自己编请求、解响应，只有宿主自己的两条语音直通方言才原样转发。声明式方言绑非语音操作仍会在安装时被拒。

方言决定语音请求的出站形状（见上一节）。绑定也可以手工写：`model capability add|set` 的 `--dialect` 写一条，`model dialect ls|set|unset` 直接读写 `model_operations`：
```bash
lax model capability add mimo-v2.5-tts --capability speech --dialect lax/mimo-chat-speech@1
lax model dialect set mimo-v2.5-tts chat.complete@1 com.example.echo/chat@1 --provider mimo
```

`--dialect` 写 `model_operations`：`speech` → `speech.synthesize@1`，`transcription` → `speech.transcribe@1`，两项能力都声明就各写一条；模型没声明语音能力时是用法错误。`model dialect set` 是覆盖语义，`unset` 删不到时报错，`--provider` 不给时按模型名唯一确定。非语音操作绑到插件方言时 `set` 会提醒「网关会拒绝」，但当前实现里 hook 方言可以承接这些操作（见下面「沙箱里能做什么」）。

### 沙箱里能做什么

`moejs` 插件的每个 hook 拿到的 `ctx`：`abi`、`operation`、`model`、`wire`、`stream`、`dialect`、归一后的 `input`（正文、音色、格式、音频 base64 等）、配置项 `options`、上限 `limits`、宿主编好的 `body`、`decode_response` 的 `response`、`decode_stream` 的 `chunk` 与 `state`。注入的 API 只有这些：

| API | 用途 |
| --- | --- |
| `lax.log([level], message)`、`console.log\|info\|warn\|error` | 写日志，落 `plugin_logs` 并打一行到 serve 输出 |
| `lax.now()`、`lax.randomUUID()` | 时间与 v4 UUID；逐请求的头（例如上游要求的请求 ID）靠它生成 |
| `lax.base64.encode\|decode`、`lax.sha256`、`lax.hmacSHA256` | 编解码与摘要 |
| `lax.error(code, message, {retryable?, details?})` | 造带错误码的对象，由插件 `throw` |
| `lax.http.request({method?, url, headers?, body?, timeout_ms?})` | 控制面请求，见下 |
| `lax.cache.get(ns, key)` / `set(ns, key, value[, ttl_ms])` / `delete(ns, key)` | 进程内缓存，按插件隔离，单插件 1 MiB |
| `URL` | 只解析与格式化，不发请求 |

不能做的：

- 没有 `fetch`、`process`、`require`、`fs`、`Bun`、`child_process`、`setTimeout`、`setInterval`；读不到文件、进程、环境变量与凭据原文（凭据由宿主注入请求头，插件看不到 key）。
- 联网只能走 `lax.http.request`：manifest 要声明 `network.mode = "host-proxy"` 与主机白名单，只放行 https、不跟随重定向，每次 hook 最多 4 次、单次上限 15 s、响应体默认 1 MiB。`lax.media.*` 本版不可用，音频走 `ctx.input.audio_base64` 与 `result.body_base64`。
- **没有内存硬上限**：goja 不提供堆上限 API，能限的只有四道字节闸门——源码单文件 4 MiB、整包 8 MiB（最多 64 个文件），hook 返回值 8 MiB，注入的音频与流块 8 MiB，`lax.cache` 1 MiB。写坏的插件仍能把进程内存吃高。
- 逐 hook 超时默认 100 ms（编请求与解流）与 2 s（解响应），上限 2 s / 2 s / 30 s。超时用中断实现，但中断只在 JS 代码里生效：`lax.http.request` 正在等上游响应时不会立刻返回，一个 100 ms 的 hook 可能实际占用几秒。
- 模块只支持受限 ESM：`export function\|const\|let\|var\|class`、`export default`（带 hook 方法的对象）、`import X from "./a.js"` 与 `import {a as b} from "./a.js"` 这类相对 import。没有 `import *`、裸模块名与 `node:`、动态 `import()`、`export *`、`export { … }`、顶层 `await`；路径必须在包根内、要带扩展名。

`plugin config` 读写 manifest 的 `options`：`set` 按声明的 `type`（`string` / `number` / `boolean`）与 `enum` 校验，`get` 没写过的键回落到 `default`，`unset` 幂等。取值只落库，运行中的网关要 `plugin reload` 才用新值；`secrets` 不走这里。

退出码沿用 0 成功、1 运行失败（含 `plugin doctor` / `plugin verify` 有检查未通过）、2 用法错误；每个子命令都支持 `--json`。

## 决策请求：lax model decide

systemone 是第五条线协议，不是对话：一条请求由一个 `state` 加一组带名字的问题组成，回的是每个问题的答案与概率（是非题给概率，选择题与打分题给置信度），不产文本。这类模型的入站与出站都是 `POST /v1/systemone`，只与自己互通，也不参与跨协议转换，所以发对话的 `model call` 对它一律拒绝（退出码 1，文案指向 `model decide`）。厂商一侧多了一条 `--systemone` 路由：

```bash
lax provider add typesafe --base-url https://api.typesafe.ai \
    --systemone /v1/systemone --default-protocol systemone
lax account add ts --provider typesafe --key sk-xxx
lax model add typesafe/jev --account ts --protocol systemone   # 决策模型不在上游清单里，只能手写声明
lax model probe --account ts --model typesafe/jev --protocol systemone   # 只探 systemone 这一条路
```

`model add` 一次写两张表：模型目录（来源 `manual`，协议集合加上 `systemone`）与账号可达性（`allowed`，账号级协议 `systemone`）。可重复执行，同名再写一次结果相同；从 `forbidden` / `missing` / `error` 翻回 `allowed` 时清掉旧的 HTTP 状态、错误与探活时间，下一轮照常重探。不给 `--account` 无法写可达性，只给模型名是用法错误。只声明 systemone 一条路由时要同时给 `--default-protocol systemone`：固定回退顺序只认 `openai-chat` → `openai-responses` → `anthropic`，不给的话这家厂商算不出默认协议。删这条路由是 `provider unset typesafe systemone`（删掉的正好是默认协议时会被拒绝，先把默认协议改成别的）。

探活按模型声明的协议优先：`model add --protocol systemone` 写下的账号级协议会排在候选最前面，所以 `model probe --account ts` 不会先去打 anthropic 与 openai-chat；`--protocol` 则把候选收窄到这一条。连接层失败（连不上、TLS 超时）会先重试两次，重试完仍失败时不改可达性、保留上一次的 `allowed`；只有 `403`、`404` 与明说模型不存在的 `400` 才改可达性。`model show typesafe/jev` 能直接看到协议、HTTP 状态、探活时间与错误，不用再查库。`model decide` 遇到模型不在任何账号清单里时，下一步文案也会指向 `lax model add` 而不是重新建账号。

```bash
lax model decide --model typesafe/jev --state "客户说：这个月的账单被扣了两次" \
    --noul "present=Is this text present?" \
    --choice "mood=这是哪一类问题?" --choice "mood:billing=账单与付款,other" \
    --score "anger=有多生气?" --score "anger:calm,mild,angry"
模型 typesafe/jev（账号 ts，systemone，耗时 132ms）
名称     类型    答案     概率  置信度
present  noul    yes      0.99  -
mood     choice  billing  -     0.87
anger    score   mild     -     0.61
用量 输入 11 / 输出 4
```

`--state` 收纯文本或 JSON：去掉首尾空白后首字符是 `{`、`[` 或 `"` 时按 JSON 解析，其余原样当一个字符串发出去。问题有两种给法，互斥：`--questions` 直接给整块 questions 对象（JSON，原样转发），或者用 `--noul 名=问句`、`--choice 名=问句 --choice 名:选项[,选项...]`、`--score 名=问句 --score 名:级别[,级别...]` 逐条拼（选项写 `key` 或 `key=说明`，选择题至少两个选项、打分题至少两级级别且按从低到高写，级别只写 key）。`--json` 原样输出上游的 `answers` 与 `usage`，另加 `model`、`account`、`latency_ms`。有效出站协议不是 systemone 的模型会被拒绝（退出码 1）并说明它以哪条协议出站；非 2xx 时文案取上游错误体的 `detail.message`。

## 客户端怎么接

客户端里把地址填成本机网关即可，程序不改任何客户端配置。Claude Code 一例：

```
ANTHROPIC_BASE_URL=http://127.0.0.1:4567
ANTHROPIC_AUTH_TOKEN=lax-local            # 本机网关不校验，填个非空值即可
ANTHROPIC_MODEL=deepseek-v4-pro           # 必须是探活出来的可用模型名
```

说 openai-chat 与 openai-responses 的客户端把 base URL 填成 `http://127.0.0.1:4567/v1`，模型名同样取探活出来的可用值；凭据值本机不校验，填一个非空值即可。

不知道有哪些模型名可用时直接问网关，清单来自启用账号的模型列表，按名字去重升序，`owned_by` 是厂商名：

```bash
curl -s http://127.0.0.1:4567/v1/models
{"object":"list","data":[{"id":"deepseek-v4-fast","object":"model","created":0,"owned_by":"deepseek"},{"id":"deepseek-v4-pro","object":"model","created":0,"owned_by":"deepseek"}]}

curl -s http://127.0.0.1:4567/v1/models/deepseek-v4-pro   # 单个模型，未命中返回 404
```

这两个接口只读、只收 GET（其它方法 405 并带 `Allow: GET`），不写请求记录，也不影响冷却与选号；冷却中的账号的模型照样列出来，停用账号的模型不列。`/healthz` 是运维视角的账号与模型计数，模型名清单看这两个接口。

## 构建

```bash
make check        # go vet + go test + go build
./lax --help
```

只需要 Go 工具链，生成的 `lax` 是单文件，不依赖外部服务，也不需要 cgo。

## 数据在哪

| 路径 | 内容 |
| --- | --- |
| `$XDG_CONFIG_HOME/lax/config.json` | 监听地址、数据库路径、后端与连接串 |
| `$XDG_DATA_HOME/lax/lax.db` | 账号、模型状态、请求记录（只用 SQLite 时） |
| `$XDG_DATA_HOME/lax/secret.key` | 加密凭据的密钥 |
| `$XDG_DATA_HOME/lax/lax.pid` | `serve` 运行时写下的进程号（权限 0600，退出时删掉），`plugin reload` 凭它发 `SIGHUP` |
| `$XDG_DATA_HOME/lax/plugins/` | 已安装的插件包：`installed/<插件 ID>/<版本>/` 不可变，`staging/` 是安装时的临时落点；挂在库文件旁边，随数据目录搬家 |

数据库缺省是 SQLite 单文件。要用 PostgreSQL 或 MySQL，在配置里改两项：

```json
{
  "addr": "127.0.0.1:4567",
  "db": "/home/someone/.local/share/lax/lax.db",
  "driver": "postgres",
  "dsn": "postgres://user:pass@127.0.0.1:5432/lax?sslmode=disable"
}
```

`driver` 取 `sqlite`、`postgres` 或 `mysql`；`dsn` 非空时用它连库，此时 `db` 被忽略。只有 SQLite 的 `dsn` 由程序拼。表结构与建表走 `AutoMigrate`，不需要手工建库；旧版单文件库（18 个 `.sql` 迁移那个时代的库）在首次打开时自动搬迁，原文件留一份 `.bak`。

三个后端共用同一套代码：表结构由 Go 结构体推导，查询走 GORM 的链式调用，方言差异（占位符、UPSERT 语法、自增主键、列类型、日期函数）由 GORM 与 `store.dateExpr` 处理。SQLite 用 `github.com/glebarez/sqlite`（纯 Go，不需要 cgo）；PostgreSQL 与 MySQL 分别用 `gorm.io/driver/postgres` 与 `gorm.io/driver/mysql`。三个后端都过了集成用例 `internal/store/backend_integration_test.go`（`go test -tags integration`，靠 `LAX_TEST_POSTGRES_DSN` 与 `LAX_TEST_MYSQL_DSN` 开启，不给就只跑 SQLite）。

凭据用 AES-256-GCM 加密后入库。这防的是备份文件与日志泄漏，不防同一台机器上以同一用户身份运行的程序。

## 已知边界

gemini 的入站与出站都支持非流式与流式，模型列表可同步（`google-gemini` 内置，`GET /v1beta/models` 的清单去掉 `models/` 前缀、按 `nextPageToken` 翻页；只能做向量化的条目现在也进清单，能力位记 `embedding`，以前它们被 `supportedGenerationMethods` 过滤掉了）。gemini 的模型名在 URL 路径里，路由的 `path` 因此可以写 `{model}` 占位符（相对路径与整条地址都行，替换时按路径段转义）；入站 gemini 的错误体是 google 的形状（`error.code` 是 HTTP 状态码、`error.status` 是状态名），四条入站路径各自的形状见 DESIGN 5.2；非正常的 `finishReason`（`SAFETY` 一类）按正常结束转给客户端，原值只进网关日志；gemini 的内置工具（`googleSearch` 等）与思考内容（`thought` 标记、`thinkingConfig`）不搬。转换时搬运文本、图片、思考内容、工具调用与工具结果五类块：思考内容只在响应方向搬运，请求方向不回填——chat 的 `reasoning_content` 与 responses 的 `reasoning` item 是上游自己产生的字段，回填给推理模型会被以 400 拒掉整条请求；响应方向仍照旧搬运。签名只有 anthropic 有落点，没有签名的思考块不写进 anthropic 的请求与响应（不伪造签名，伪造会被上游以 400 拒绝），所以 chat 与 responses 的思考内容转到 anthropic 时丢弃；流式转换下发给 anthropic 客户端的思考块没有签名，客户端若把这些块回填进下一轮请求，上游会报 400。工具结果之后紧跟的追问并进同一条 user 消息，因为 anthropic 不接受两条相邻的 user 消息。思考配置参数（anthropic 的 `thinking`、chat 的 `reasoning_effort`）、`redacted_thinking` 的不透明 `data` 与 responses 的 `encrypted_content` 跨协议不搬，reasoning token 不单独记账（算进输出 token）。音频与文件类内容丢弃；anthropic 的服务端内置工具（`web_search` 等）跨协议时整条丢弃，chat 与 responses 的工具定义只认 function，编回 anthropic 时才原样保留 `type`；`tools[].strict` 不搬运，`is_error` 只有 anthropic 有落点，`allowed_tools` 降级为 `auto`。流式转换下断在工具参数中间时，已下发的参数片段撤不回来，收尾的错误帧会写明「工具调用参数在上游中断，内容不完整」，这一次记失败，程序不伪造闭合的 JSON。跨协议只搬运图片地址，没有其它多模态变换。不改写任何客户端配置文件；需要非标准请求头的上游通过 `provider header` 声明（见 DESIGN 5.10），例如 opencode 的 Go 端点要求 `x-opencode-session`，声明 `{session}` 即可路由，要求按请求计算的签名的上游仍然接不了；冷却时长按单次失败计算；探活调用不计入用量汇总；估算花费需要给模型定价（`model price set` 的 `--in` 与 `--out`，账号只在套餐不同时用 `--account` 覆盖），缓存读写按 `--cache-read` 与 `--cache-write` 折价（未填时回退到输入价，单价单位都是每百万 token）；`usage` 与 `log tail` 里仍分别显示缓存读写的 token。语音不做音频转码、不解析音频容器算时长（直通路径 TTS 按入参字符、ASR 按上游报的秒数，chat 方言按 token 记账），变化的模型名也不改写（响应里的 `model` 原样）；出站形状只由绑定的方言决定，同协议的方言不执行声明式映射，chat 形状的语音方言可以是内置的 Go 实现，也可以是插件用声明式映射或 hook 方言写的（见「插件与方言」）。

插件与方言：本版落地声明式插件、内置方言与 `moejs` 沙箱，仍没有 `trusted-worker`（独立进程，以及文件、进程、环境变量权限）、Magpie/OpenCode 导入与 `plugin import`、OAuth 登录、插件市场与签名（`plugin_versions.signature_status` 一律 `unverified`、`source` 一律 `dir`）。声明式映射只能表达 `const`、`input`、`model`、`option` 四种来源加 `map`、`default`、`omit_if_empty` 三个修饰键，没有表达式、正则、循环与脚本，`input` 白名单只有语音那八项，所以声明式方言只能给语音两条操作写映射；对话、向量化与决策请求可以绑宿主方言（`lax/` 前缀）走 Go 编解码，也可以绑 hook 方言。hook 方言的边界：`Input.Body` 是宿主编好的报文（`Input.Messages` 仍为空，读消息要解析 `ctx.body`）；向量化与决策入站的响应，方言返回的 `ResultJSON` 在入站与出站同协议时原样写回（这两个协议没有中间表示，跨协议改编不支持）；要流式就得实现 `decode_stream`，否则在发上游请求之前 400；`Plan.Path` 只能改相对路径，语音路径不看它；`Plan.Headers` 是逐请求的头（请求 ID、资源号这类），语音与非语音两条方言路径都生效，但凭据头与帧头是保留头、沙箱直接拒，凭据只由厂商的 `auth` 声明注入。沙箱没有内存硬上限（goja 不提供堆上限 API，只有源码 8 MiB、返回值 8 MiB、注入音频与流块 8 MiB、cache 1 MiB 四道字节闸门）；逐 hook 超时打不断在途的 `lax.http.request`，一个 100 ms 的 hook 可能占用几秒；ESM 只支持静态 `export`、`export default` 与相对 import，没有 `import *`、裸模块、动态 import 与顶层 `await`；联网只走 `lax.http.request` 的白名单 https，没有媒体通道。`plugin install` 逐家厂商一个事务，一个插件带多家厂商时第 N 家失败会让前 N-1 家留在库里（用 `lax plugin rm` 或 `lax provider rm` 清）。`plugin reload`（`SIGHUP`）只整表换方言注册表，不关旧沙箱，反复 reload 会积累旧实例；没有 `models` / `probe` 两个控制面钩子，模型清单与探活仍由 `model sync` / `model probe` 做。
