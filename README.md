# switchboard

本机上的模型总机。账号在命令行里管，所有客户端只连一个地址。

```bash
switchboard init                                            # 建配置、数据库与密钥
switchboard provider ls                                     # 看已知厂商与各协议的路径
switchboard account add ds --provider deepseek --key sk-xxx  # 或用 --key - 从标准输入读
switchboard account set ds --key sk-yyy                     # 换 key：不删号，冷却与失败计数清零
switchboard model sync --account ds                         # 拉上游模型列表
switchboard model probe --account ds                        # 逐个探活，能用的才进选号
switchboard serve                                           # 起网关，127.0.0.1:4567
switchboard log tail                                        # 看请求记录
```

四条对话线协议能进，也能出（第五条 `systemone` 是决策协议，只与自己互通、不参与转换，见下面「决策请求」）。入站按路径分 anthropic、openai-chat、openai-responses 与 gemini，出站按厂商行上那家厂商的默认协议（探活发现某个模型该走别的协议时，结论记在 `account_models.protocol` 上优先用它），取值同为这四条；入站与出站相同则直通，不同则经中间表示转换，四条入站 × 四条出站共十六种组合，流式与非流式都可用，工具调用包含在内。中间表示、映射规则、选号规则、数据模型、里程碑见 [DESIGN.md](DESIGN.md)。

## 命令

| 命令 | 作用 |
| --- | --- |
| `init` | 建配置目录、数据库、加密密钥 |
| `serve` | 前台起网关 |
| `account add\|ls\|set\|rm\|test\|enable\|disable` | 加账号、列账号、换凭据、删账号、探活、启用、停用 |
| `provider ls\|add\|set\|unset\|rm` | 维护厂商默认地址、默认协议与各协议的路由；`add` 只新建，`set` 只设值，删除走 `unset` |
| `model ls` | 列出模型、可用账号与状态，可按厂商与账号筛选 |
| `model sync` | 从上游拉模型列表，写进本地，可按厂商与账号筛选 |
| `model probe` | 逐个模型探活，把能用的写进账号，可按厂商与账号筛选 |
| `model alias set\|unset\|ls` | 模型别名：把客户端写死的模型名指到真实模型名上，只影响请求方向 |
| `model price set\|unset\|ls` | 模型单价：目录里的厂商级标价与某账号的覆盖价，`usage` 与 `model ls` 都读它 |
| `usage` | 按账号、模型或日期汇总用量，含输入、输出与缓存读写 token |
| `log tail` | 最近的请求记录 |
| `call` | 手工打一条请求，打印选号过程；默认 anthropic 入站，`--protocol` 可切换四种入站，gemini 也支持 `--stream` |
| `decide` | 发一条 systemone 决策请求（`--state` 加一组问题），打印每个问题的答案与概率；`--json` 给脚本用 |
| `doctor` | 自检配置、目录、密钥、端口、数据库 |

每个命令都支持 `--json`，结果走 stdout，错误走 stderr。退出码：0 成功、1 运行失败、2 用法错误。

## 入站协议

路径决定入站协议，四条路径同时可用，一个网关可以同时接四种客户端（另有两条只读的模型清单接口 `GET /v1/models` 与 `GET /v1/models/{id}`，见下面「客户端怎么接」）：

| 入站路径 | 入站协议 | 常见客户端 |
| --- | --- | --- |
| `POST /v1/messages` | anthropic | Claude Code |
| `POST /v1/chat/completions` | openai-chat | 普通 OpenAI SDK 客户端 |
| `POST /v1/responses` | openai-responses | Codex |
| `POST /v1beta/models/{model}:generateContent`<br>`POST /v1beta/models/{model}:streamGenerateContent` | gemini | 直接用 Google SDK 或原生 `generateContent` 的客户端 |

gemini 的模型名在路径里（请求体里没有 `model` 字段）：`:` 之后的后缀不算名字的一部分，`{model}` 参与选号，名字里带 `/` 时按路径段转义成 `org%2Fgemini` 也能取到；流式由 `:streamGenerateContent` 后缀决定，不看请求体。两条路径都只收 POST：

```bash
# 非流式；流式把路径换成 :streamGenerateContent，响应是 SSE（data 帧，末尾一帧带 finishReason）
curl -s http://127.0.0.1:4567/v1beta/models/deepseek-v4-pro:generateContent \
    -H 'Content-Type: application/json' \
    -d '{"contents":[{"role":"user","parts":[{"text":"用一句话介绍你自己"}]}]}'
```

出站协议由厂商决定，账号只存厂商名：地址与默认协议都是那家厂商行上的值（迁移 `0014` 之后账号不再有 `base_url`、`protocol` 两列），所以 `account add` 没有 `--base-url` 与 `--protocol` 两个参数——先 `provider add` 把厂商登记进去，再建账号。默认协议的取值是 `anthropic`、`openai-chat`、`openai-responses`、`gemini` 与 `systemone`（gemini 的原生路径是 `{base}/v1beta/models/{model}:generateContent`，流式换成 `:streamGenerateContent?alt=sse`，凭据头是 `x-goog-api-key`；systemone 是决策协议，入站与出站都是 `POST /v1/systemone`，只与自己互通，见下面「决策请求」）：`provider add --default-protocol` 显式存的优先，没显式存则按 `openai-chat` → `openai-responses` → `anthropic` 取第一条路由。改一家厂商的地址或默认协议立刻影响所有用它的账号（这是单一来源应有的行为）。厂商与各协议的路径在数据库里，`switchboard provider ls` 能看到，`provider add` / `set` / `unset` 可以增删改，不必重编译。入站协议与出站协议相同时直通，请求体只改 `model` 字段，其余字段与值原样带到上游，响应按字节原样返回（gemini 的报文里没有 `model` 字段，它直通时请求体一个字节都不改）；不同时经中间表示转换，请求与响应都只搬映射表里定义的字段，表外字段丢弃。未知路径返回 404，请求体解析失败返回 400，错误消息带协议名前缀。

自己加一家厂商时，每个协议一条 flag（`--anthropic`、`--openai`、`--responses`、`--gemini`、`--systemone`），值有两种写法：相对 `--base-url` 的完整路径，或者整条地址。整条地址给同一厂商不同协议分域名的情况用，写进去直接使用、不再拼 `--base-url`，例如 `--openai https://openai.example.com/v1/chat/completions --anthropic https://claude.example.com/v1/messages`；整条地址要带主机名，`https://` 这种只有 scheme、`https:///x` 缺主机名与 `ftp://host/x` 这类别的 scheme 都是用法错误（退出码 2）。`--base-url` 可以省略，条件是每个要声明的路由（以及给了的话 `--models-path`）都是整条地址：那时它拼不进任何地址，省掉它账号的地址也留空，转发与模型同步全走整条地址。bigmodel 就是这种形状——三套协议分属不同 host 与路径，模型列表在 chat 路径的同级：

```bash
switchboard provider add bigmodel \
    --anthropic https://open.bigmodel.cn/api/anthropic/v1/messages \
    --openai https://open.bigmodel.cn/api/coding/paas/v4/chat/completions \
    --responses https://open.bigmodel.cn/api/v1/responses
```

有一条路由是相对路径时缺 `--base-url` 仍是用法错误（退出码 2），文案指出是哪条路由，并给出两条出路：给 `--base-url`，或把这条路由写成整条地址。给了地址就必须是带主机名的完整地址，`https://` 这种只有 scheme 的写法会被拒；scheme 大小写不敏感，`HTTPS://api.example.com/` 能用，落库时归一成小写并去掉末尾斜杠。路径写法不用纠结：`v1/chat/completions`、`//v1//chat/completions`、`/v1/chat/completions/` 由程序规整成同一个地址，不报错；`--models-path` 与 `model sync --endpoint` 同理：`v1/models`、`//v1//models/` 都落到 `/v1/models`，也可以写整条地址、直接访问那个地址。只有规整之后为空的取值（`--openai ""`，或只有斜杠）算用法错误，删一条路由、清一项设置用 `provider unset`。`--default-protocol` 定下这家的默认协议，取值写 `anthropic`、`openai`、`responses`、`gemini`（也认线协议全名 `openai-chat`、`openai-responses`，`gemini` 两种写法同形），必须落在已声明的路由上：

```bash
# 新建厂商：至少一条协议路由；--base-url 只在路由是相对路径时才需要
switchboard provider add myco --base-url https://api.deepseek.com \
    --openai /v1/chat/completions \
    --anthropic /anthropic/v1/messages \
    --default-protocol openai
已添加厂商 myco（https://api.deepseek.com），2 条路由，默认协议 openai-chat
```

gemini 的入站与出站都可用，原生接口把模型名放在路径里，所以它的路由可以写 `{model}` 占位符：占位符原样落库，发请求之前才换成模型名（按路径段转义），相对路径与整条地址两种写法都行。`google-gemini` 这家（`https://generativelanguage.googleapis.com`）已经内置，模型列表在 `/v1beta/models`、转发在 `/v1beta/models/{model}:generateContent`，`provider ls` 能看到；要指向自己搭的代理就照同一个形状另加一家：

```bash
# 只声明 gemini 一条路由，默认协议跟着写成 gemini
switchboard provider add gem-proxy --base-url https://gemini-proxy.example.com \
    --gemini /v1beta/models/{model}:generateContent \
    --default-protocol gemini
已添加厂商 gem-proxy（https://gemini-proxy.example.com），1 条路由，默认协议 gemini
```

删掉这条路由是 `provider unset gem-proxy gemini`；这家只有这一条路由时会被拒绝，那是不想要这家厂商了，用 `provider rm`。账号一侧用 `account add --provider google-gemini`（这家的默认协议已经是 gemini；自建代理就把 `--provider` 换成那家的名字），gemini 的入站是 `POST /v1beta/models/{model}:generateContent` 与 `:streamGenerateContent`，非流式与流式都能用。

`provider set` 只设值：补一条路由、改默认协议或地址，没传的字段一律保留。传空值是用法错误，删除要显式声明：

```bash
# 补一条 responses 路由，并把默认协议换掉
switchboard provider set myco --responses /v1/responses --default-protocol responses

# 删一条路由；也可以一次删多个（models-path、default-protocol、note 同样可用）
switchboard provider unset myco anthropic
```

`provider unset` 幂等：要删的项本来就不存在时不报错，只说明一句「myco 本来就没有 anthropic 路由」。删到一条路由都不剩会被拒绝，那是不想要这家厂商了，用 `provider rm`。`--models-path <路径>` 是厂商级的模型列表覆盖，`provider unset myco models-path` 清掉它、回到按各路由取。

有些上游除凭据头之外还要额外的头才路由（opencode 的 Go 端点缺了 `x-opencode-session` 直接 400）。这类头按厂商声明，写进数据库、不用重编译：`--protocol` 不给就是厂商级，这家厂商的每条路由都带上它；给了就只加给那条协议，同名头协议级覆盖厂商级。值四种写法：字面量原样发出，`{env:NAME}` 取环境变量（适合不该落库的会话 token），`{session}` 取本次请求的会话键（一个对话一个稳定值），`{model}` 取本次请求的模型名，都是转发前才展开。值可能是密钥，所以 `ls` 与 `--json` 只输出脱敏后的前四位：

```bash
# 只加给 openai-chat 那条路由，值随对话稳定
switchboard provider header set opencode x-opencode-session '{session}' --protocol openai-chat

# 看这家声明了什么：厂商 / 协议（厂商级显示 -）/ 头名 / 值（脱敏）
switchboard provider header ls opencode

# 删一条：删不到会报错并列出这家现有的声明
switchboard provider header unset opencode x-opencode-session --protocol openai-chat
```

完整规则（四种写法、与凭据头冲突时声明优先、不进请求记录）见 DESIGN 5.10。

换 key 不删号重建：`account add` 时那份 key 失效了，用 `account set` 覆盖掉就行，探活结果、协议发现与请求记录的账号归属都留着。旧凭据留下的冷却与失败计数在同一个事务里清零，账号重新启用：

```bash
switchboard account set ds --key sk-yyy          # 直接写值
switchboard account set ds --key - < key.txt     # 从标准输入读
SWITCHBOARD_KEY=sk-yyy switchboard account set ds # 留空则读环境变量 SWITCHBOARD_KEY
```

`update` 是 `set` 的同义词。`--json` 只回显掩码，明文不进输出；换完 key 不用再跑 `account add`，也不影响已经探出来的模型状态。

`account test` 判的是凭据与连通性，不是某个模型能不能用，所以它分两段。先 `GET` 模型列表证明地址通：地址与凭据头跟 `model sync` 同一套规则（厂商覆盖 → 该账号的默认协议那条路由的 `models_path`，整条地址直接访问，凭据头跟取到地址的那条路由的协议），2xx 说明上游连得上，顺带报出上游模型数，这一条不花 token。列表 2xx 之后**仍然**再发一条 `max_tokens=1` 的请求证明凭据有效：有的上游列表接口是公开的，任何 key 都回 200（commandcode 就是），光看列表判不出 key 好不好。这家没有列表接口（404、405）、列表被网关拦住（403）或连不上时也走这条请求，模型取账号模型列表第一项，为空时用默认模型名 `default` 并在备注里说明。两段都通过才判正常，状态与退出码按调用那段算；备注与 `--json` 写清两段各自的结果（`list` 对象是第一段的）。某个模型到底能不能用由 `model probe` 决定。

```bash
switchboard account test ds
账号  状态  HTTP  耗时   上游模型数  备注
ds    正常  200   412ms  47          模型调用可用（模型 deepseek-chat）；列表接口可用（GET https://api.deepseek.com/v1/models，上游 47 个模型）
```

## 模型从上游拉，再探活

模型列表不用手填。`model sync` 拉上游的模型清单，`model probe` 逐个试一遍，能用的才进入选号。两条命令都接受 `--provider` 与 `--account`，不给就处理全部账号：

```bash
switchboard account add ds --provider deepseek --key sk-xxx
switchboard model sync                                     # 全部账号各拉一次模型列表
switchboard model sync --provider deepseek --account ds   # 只处理这一个账号
switchboard model sync --account ds --endpoint /v1/models # 临时指定列表路径，只影响这一次调用
switchboard model probe --account ds --limit 5            # 只探这个账号的前 5 个模型
switchboard serve
```

模型列表的形状不用管：`{data:[…]}`、`{models:[…]}`、顶层数组三种都认，gemini 的 `{models:[{"name":"models/…"}]}` 走第二种，清单按 `supportedGenerationMethods` 过滤、名字去掉 `models/` 前缀；分页带 `pageSize=1000`，有 `nextPageToken` 就接着取，最多 10 页。

探活会真实调用上游：`--limit 5` 限制每个账号这一次最多探几个，`--model deepseek-v4-pro` 只试一个。某个账号没探出可用模型时只写探活结果，它原有的模型列表保持不变；全部账号都没探出可用模型时退出码 1。终端里探活时 stderr 上有一行原地刷新的进度（账号、已完成 / 待探数、最近探完的模型），账号探完补一个换行再打汇总与表格；stderr 不是终端或加了 `--json` 时没有这一行，管道、CI 与日志里不会多出噪声。碰到 401 时命令停下并停用该账号：凭据被拒，后面的结论不作数。同步失败的账号同样不影响其余账号，只是最终退出码为 1。`account ls` 与 `model ls` 会带上每个账号的模型状态（可用 / 待探活 / 无权 / 不存在 / 凭据被拒 / 失败）。空模型列表的账号不参与选号，所以 `account ls` 的模型列不会写「不限」：只有手填的名字时直接列出模型名，同步过但没探出可用模型时写「0 可用 / N 同步」，完全没有模型信息时写「无模型（先 model sync）」；`model ls` 一条模型都没有时同样把下一步指到 `model sync` 与 `model probe`。

模型名还可以起别名。客户端里的模型名往往写死（`claude-3-5-sonnet`、`gpt-4o`），而账号声明的是上游的真实名字；`model alias` 把那个写死的名字指到一个真实模型名上，网关按真名选号、改请求体与路径，客户端不必改：

```bash
switchboard model alias set fast glm-4.6   # 客户端请求 fast 时按 glm-4.6 走；同名再写一次就是改目标
switchboard model alias ls                 # 别名 / 目标 / 状态 / 解析
switchboard model alias ls fast            # 只看一条
switchboard model alias unset fast
```

别名只影响请求方向：响应不重写，里面的模型名仍是上游的真实名字。客户端请求别名时，请求记录里的请求模型名与使用模型名会不同，`log tail` 的模型列打的是客户端请求的那个名字，一次别名命中一眼可见。别名精确匹配、区分大小写，可以指向另一个别名，链最长 8 跳；名字已经被某个账号或模型目录当成真实模型时写入会被拒绝，否则别名会遮蔽真名。目标眼下没有账号提供也能先写，之后加账号即生效；`model alias ls` 的解析列写的是链走到的真实名字，链断了（成环、超过 8 跳）时显示成不可解析并给出原因。

模型单价也挂在模型目录上，不在账号上：同一家上游的同一个模型只有一份标价，否则改一次价要改 N 个账号，`usage` 还会给同一件事算出 N 个单价。管理员先用 `model price set` 定厂商级标价，套餐真的不同时才对某个账号加一份覆盖价：

```bash
switchboard model price set glm-4.6 --in 0.6 --out 2.2           # 厂商级标价，模型名只在一家目录里时不用 --provider
switchboard model price set glm-4.6 --in 0.6 --out 2.2 --provider bigmodel  # 同名模型出现在多家时用 --provider 指定
switchboard model price set glm-4.6 --account ds --out 2.5       # 只给这个账号的覆盖价，不影响别的账号
switchboard model price ls                                       # 模型 / 供应商 / 输入价 / 输出价 / 缓存读 / 缓存写
switchboard model price ls glm-4.6 --account ds                  # 目录价与这个账号的有效价并排
switchboard model price unset glm-4.6 cache-read                 # 清掉某一项；清第二次不报错，只说「本来就没有」
```

价格单位是每百万 token，0 合法（免费或打包），负数是用法错误。只写命令行上给过的那几项，没给的留空表示继承：账号级的「留空」就是回到厂商级标价（`model price unset` 同理，账号级清掉不是「免费」），缓存读写没定价时按输入价算（`model price ls` 的缓存那格写「回退输入价」）。`account add` 与 `account set` 因此不再收 `--price-*`：误敲会得到一个指向 `model price set` 的用法错误。

## 决策请求：switchboard decide

systemone 是第五条线协议，不是对话：一条请求由一个 `state` 加一组带名字的问题组成，回的是每个问题的答案与概率（是非题给概率，选择题与打分题给置信度），不产文本。这类模型的入站与出站都是 `POST /v1/systemone`，只与自己互通，也不参与跨协议转换，所以发对话的 `call` 对它一律拒绝（退出码 1，文案指向 `decide`）。厂商一侧多了一条 `--systemone` 路由：

```bash
switchboard provider add typesafe --base-url https://api.typesafe.ai \
    --systemone /v1/systemone --default-protocol systemone
switchboard account add ts --provider typesafe --key sk-xxx --models typesafe/jev   # 决策模型不在上游清单里，只能手工声明
switchboard model probe --account ts
```

只声明 systemone 一条路由时要同时给 `--default-protocol systemone`：固定回退顺序只认 `openai-chat` → `openai-responses` → `anthropic`，不给的话这家厂商算不出默认协议。删这条路由是 `provider unset typesafe systemone`（删掉的正好是默认协议时会被拒绝，先把默认协议改成别的）。

```bash
switchboard decide --model typesafe/jev --state "客户说：这个月的账单被扣了两次" \
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
ANTHROPIC_AUTH_TOKEN=switchboard-local    # 本机网关不校验，填个非空值即可
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
./switchboard --help
```

只需要 Go 工具链，生成的 `switchboard` 是单文件，不依赖外部服务。

## 文档站

```bash
make docs         # 本地预览，127.0.0.1:3000，改文件自动刷新
make docs-build   # 生产构建，顺带跑一次类型检查
```

站点用 Fumadocs（Next.js）渲染，代码在 `docs-site/`。内容可以放在仓库里，也可以单独成仓：构建前 `docs-site/scripts/fetch-content.mjs` 按 `DOCS_CONTENT_DIR` 或 `DOCS_CONTENT_REPO` 把内容同步进 `docs-site/content/docs`，两个变量都不设时沿用现有目录。内容面向使用者：安装、快速开始、厂商与路由、账号、模型、网关与协议、客户端接入、用量与日志、命令行参考、常见问题。

`DESIGN.md`、`LESSONS.md` 与 `lessons/` 是仓库内部的开发文档，不进站点。

需要 Node.js 与 npm，首次 `make docs` 会装依赖。`make check` 不包含文档站，Go 工具链仍可单独跑通。

部署方式（Docker 镜像、systemd、反向代理）与内容分仓的环境变量、更新触发方式见 [`docs-site/README.md`](docs-site/README.md)。

## 数据在哪

| 路径 | 内容 |
| --- | --- |
| `$XDG_CONFIG_HOME/switchboard/config.json` | 监听地址、数据库路径 |
| `$XDG_DATA_HOME/switchboard/switchboard.db` | 账号、模型状态、请求记录 |
| `$XDG_DATA_HOME/switchboard/secret.key` | 加密凭据的密钥 |

凭据用 AES-256-GCM 加密后入库。这防的是备份文件与日志泄漏，不防同一台机器上以同一用户身份运行的程序。

## 已知边界

gemini 的入站与出站都支持非流式与流式，模型列表可同步（`google-gemini` 内置，`GET /v1beta/models` 的清单按 `supportedGenerationMethods` 只留 `generateContent`、名字去掉 `models/` 前缀、按 `nextPageToken` 翻页）。gemini 的模型名在 URL 路径里，路由的 `path` 因此可以写 `{model}` 占位符（相对路径与整条地址都行，替换时按路径段转义）；入站 gemini 的错误体是 google 的形状（`error.code` 是 HTTP 状态码、`error.status` 是状态名），四条入站路径各自的形状见 DESIGN 5.2；非正常的 `finishReason`（`SAFETY` 一类）按正常结束转给客户端，原值只进网关日志；gemini 的内置工具（`googleSearch` 等）与思考内容（`thought` 标记、`thinkingConfig`）不搬。转换时搬运文本、图片、思考内容、工具调用与工具结果五类块：思考内容只在响应方向搬运，请求方向不回填——chat 的 `reasoning_content` 与 responses 的 `reasoning` item 是上游自己产生的字段，回填给推理模型会被以 400 拒掉整条请求；响应方向仍照旧搬运。签名只有 anthropic 有落点，没有签名的思考块不写进 anthropic 的请求与响应（不伪造签名，伪造会被上游以 400 拒绝），所以 chat 与 responses 的思考内容转到 anthropic 时丢弃；流式转换下发给 anthropic 客户端的思考块没有签名，客户端若把这些块回填进下一轮请求，上游会报 400。工具结果之后紧跟的追问并进同一条 user 消息，因为 anthropic 不接受两条相邻的 user 消息。思考配置参数（anthropic 的 `thinking`、chat 的 `reasoning_effort`）、`redacted_thinking` 的不透明 `data` 与 responses 的 `encrypted_content` 跨协议不搬，reasoning token 不单独记账（算进输出 token）。音频与文件类内容丢弃；anthropic 的服务端内置工具（`web_search` 等）跨协议时整条丢弃，chat 与 responses 的工具定义只认 function，编回 anthropic 时才原样保留 `type`；`tools[].strict` 不搬运，`is_error` 只有 anthropic 有落点，`allowed_tools` 降级为 `auto`。流式转换下断在工具参数中间时，已下发的参数片段撤不回来，收尾的错误帧会写明「工具调用参数在上游中断，内容不完整」，这一次记失败，程序不伪造闭合的 JSON。跨协议只搬运图片地址，没有其它多模态变换。不改写任何客户端配置文件；需要非标准请求头的上游通过 `provider header` 声明（见 DESIGN 5.10），例如 opencode 的 Go 端点要求 `x-opencode-session`，声明 `{session}` 即可路由，要求按请求计算的签名的上游仍然接不了；冷却时长按单次失败计算；探活调用不计入用量汇总；估算花费需要给模型定价（`model price set` 的 `--in` 与 `--out`，账号只在套餐不同时用 `--account` 覆盖），缓存读写按 `--cache-read` 与 `--cache-write` 折价（未填时回退到输入价，单价单位都是每百万 token）；`usage` 与 `log tail` 里仍分别显示缓存读写的 token。
