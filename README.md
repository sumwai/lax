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

四条线协议能进，也能出。入站按路径分 anthropic、openai-chat、openai-responses 与 gemini，出站按账号的 `protocol` 字段，取值同为这四条；入站与出站相同则直通，不同则经中间表示转换，四条入站 × 四条出站共十六种组合，流式与非流式都可用，工具调用包含在内。中间表示、映射规则、选号规则、数据模型、里程碑见 [DESIGN.md](DESIGN.md)。

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
| `usage` | 按账号、模型或日期汇总用量 |
| `log tail` | 最近的请求记录 |
| `call` | 手工打一条请求，打印选号过程；默认 anthropic 入站，`--protocol` 可切换三种入站 |
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

出站协议由账号决定：`account add --protocol` 取值 `anthropic`、`openai-chat`、`openai-responses` 与 `gemini`（gemini 的原生路径是 `{base}/v1beta/models/{model}:generateContent`，流式换成 `:streamGenerateContent?alt=sse`，凭据头是 `x-goog-api-key`），留空则取该厂商的默认协议（`provider add --default-protocol` 显式存的优先，没显式存则按 `openai-chat` → `openai-responses` → `anthropic` 取第一条路由）。厂商与各协议的路径在数据库里，`switchboard provider ls` 能看到，`provider add` / `set` / `unset` 可以增删改，不必重编译。入站协议与出站协议相同时直通，请求体只改 `model` 字段，其余字段与值原样带到上游，响应按字节原样返回（gemini 的报文里没有 `model` 字段，它直通时请求体一个字节都不改）；不同时经中间表示转换，请求与响应都只搬映射表里定义的字段，表外字段丢弃。未知路径返回 404，请求体解析失败返回 400，错误消息带协议名前缀。

自己加一家厂商时，每个协议一条 flag（`--anthropic`、`--openai`、`--responses`、`--gemini`），值有两种写法：相对 `--base-url` 的完整路径，或者整条地址。整条地址给同一厂商不同协议分域名的情况用，写进去直接使用、不再拼 `--base-url`，例如 `--openai https://openai.example.com/v1/chat/completions --anthropic https://claude.example.com/v1/messages`；整条地址要带主机名，`https://` 这种只有 scheme、`https:///x` 缺主机名与 `ftp://host/x` 这类别的 scheme 都是用法错误（退出码 2）。`--base-url` 必须是带主机名的完整地址，`https://` 这种只有 scheme 的写法会被拒；scheme 大小写不敏感，`HTTPS://api.example.com/` 能用，落库时归一成小写并去掉末尾斜杠。路径写法不用纠结：`v1/chat/completions`、`//v1//chat/completions`、`/v1/chat/completions/` 由程序规整成同一个地址，不报错；`--models-path` 与 `model sync --endpoint` 同理：`v1/models`、`//v1//models/` 都落到 `/v1/models`，也可以写整条地址、直接访问那个地址。只有规整之后为空的取值（`--openai ""`，或只有斜杠）算用法错误，删一条路由、清一项设置用 `provider unset`。`--default-protocol` 定下这家的默认协议，取值写 `anthropic`、`openai`、`responses`、`gemini`（也认线协议全名 `openai-chat`、`openai-responses`，`gemini` 两种写法同形），必须落在已声明的路由上：

```bash
# 新建厂商：--base-url 必填，至少一条协议路由
switchboard provider add myco --base-url https://api.deepseek.com \
    --openai /v1/chat/completions \
    --anthropic /anthropic/v1/messages \
    --default-protocol openai
已添加厂商 myco（https://api.deepseek.com），2 条路由，默认协议 openai-chat
```

gemini 只作出站，原生接口把模型名放在路径里，所以它的路由可以写 `{model}` 占位符：占位符原样落库，发请求之前才换成模型名（按路径段转义），相对路径与整条地址两种写法都行。只加 gemini 一家时写成这样：

```bash
# 只声明 gemini 一条路由，默认协议跟着写成 gemini
switchboard provider add google-gemini --base-url https://generativelanguage.googleapis.com \
    --gemini /v1beta/models/{model}:generateContent \
    --default-protocol gemini
已添加厂商 google-gemini（https://generativelanguage.googleapis.com），1 条路由，默认协议 gemini
```

删掉这条路由是 `provider unset google-gemini gemini`；这家只有这一条路由时会被拒绝，那是不想要这家厂商了，用 `provider rm`。账号一侧用 `account add --provider google-gemini --protocol gemini`，gemini 只作出站，客户端的入站路径仍是那三条。

`provider set` 只设值：补一条路由、改默认协议或地址，没传的字段一律保留。传空值是用法错误，删除要显式声明：

```bash
# 补一条 responses 路由，并把默认协议换掉
switchboard provider set myco --responses /v1/responses --default-protocol responses

# 删一条路由；也可以一次删多个（models-path、default-protocol、note 同样可用）
switchboard provider unset myco anthropic
```

`provider unset` 幂等：要删的项本来就不存在时不报错，只说明一句「myco 本来就没有 anthropic 路由」。删到一条路由都不剩会被拒绝，那是不想要这家厂商了，用 `provider rm`。`--models-path <路径>` 是厂商级的模型列表覆盖，`provider unset myco models-path` 清掉它、回到按各路由取。

换 key 不删号重建：`account add` 时那份 key 失效了，用 `account set` 覆盖掉就行，探活结果、协议发现与请求记录的账号归属都留着。旧凭据留下的冷却与失败计数在同一个事务里清零，账号重新启用：

```bash
switchboard account set ds --key sk-yyy          # 直接写值
switchboard account set ds --key - < key.txt     # 从标准输入读
SWITCHBOARD_KEY=sk-yyy switchboard account set ds # 留空则读环境变量 SWITCHBOARD_KEY
```

`update` 是 `set` 的同义词。`--json` 只回显掩码，明文不进输出；换完 key 不用再跑 `account add`，也不影响已经探出来的模型状态。

`account test` 判的是凭据与连通性，不是某个模型能不能用。它先 `GET` 模型列表：地址与凭据头跟 `model sync` 同一套规则（厂商覆盖 → 账号协议那条路由的 `models_path`，整条地址直接访问，凭据头跟取到地址的那条路由的协议），2xx 就说明 key 没问题、上游连得上，顺带报出上游模型数，这一条不花 token。这家没有列表接口（404、405）或列表被网关拦住（403）时，退回一条 `max_tokens=1` 的请求，模型取账号模型列表第一项，为空时用默认模型名 `default` 并在备注里说明。备注与 `--json` 的 `stage` 写清结论出自列表接口还是模型调用；某个模型到底能不能用由 `model probe` 决定。

```bash
switchboard account test ds
账号  状态  HTTP  耗时   上游模型数  备注
ds    正常  200   412ms  47          列表接口可用
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

探活会真实调用上游：`--limit 5` 限制每个账号这一次最多探几个，`--model deepseek-v4-pro` 只试一个。某个账号没探出可用模型时只写探活结果，它原有的模型列表保持不变；全部账号都没探出可用模型时退出码 1。终端里探活时 stderr 上有一行原地刷新的进度（账号、已完成 / 待探数、最近探完的模型），账号探完补一个换行再打汇总与表格；stderr 不是终端或加了 `--json` 时没有这一行，管道、CI 与日志里不会多出噪声。碰到 401 时命令停下并停用该账号：凭据被拒，后面的结论不作数。同步失败的账号同样不影响其余账号，只是最终退出码为 1。`account ls` 与 `model ls` 会带上每个账号的模型状态（可用 / 待探活 / 无权 / 不存在 / 凭据被拒 / 失败）。空模型列表的账号不参与选号，所以 `account ls` 的模型列不会写「不限」：只有手填的名字时直接列出模型名，同步过但没探出可用模型时写「0 可用 / N 同步」，完全没有模型信息时写「无模型（先 model sync）」；`model ls` 一条模型都没有时同样把下一步指到 `model sync` 与 `model probe`。

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

## 数据在哪

| 路径 | 内容 |
| --- | --- |
| `$XDG_CONFIG_HOME/switchboard/config.json` | 监听地址、数据库路径 |
| `$XDG_DATA_HOME/switchboard/switchboard.db` | 账号、模型状态、请求记录 |
| `$XDG_DATA_HOME/switchboard/secret.key` | 加密凭据的密钥 |

凭据用 AES-256-GCM 加密后入库。这防的是备份文件与日志泄漏，不防同一台机器上以同一用户身份运行的程序。

## 已知边界

gemini 的入站与出站都支持非流式与流式；模型列表同步还没做（`GET /v1beta/models` 的响应形状与已知三种都不同，名字还带 `models/` 前缀），gemini 账号的模型名暂时用 `--models` 手填。gemini 的模型名在 URL 路径里，路由的 `path` 因此可以写 `{model}` 占位符（相对路径与整条地址都行，替换时按路径段转义）；入站 gemini 的错误体沿用统一形状，不带 google 的 `error.code` 与 `error.status`；非正常的 `finishReason`（`SAFETY` 一类）按正常结束转给客户端，原值只进网关日志；gemini 的内置工具（`googleSearch` 等）与思考内容（`thought` 标记、`thinkingConfig`）不搬。转换时搬运文本、图片、思考内容、工具调用与工具结果五类块：思考内容只在响应方向搬运，请求方向不回填——chat 的 `reasoning_content` 与 responses 的 `reasoning` item 是上游自己产生的字段，回填给推理模型会被以 400 拒掉整条请求；响应方向仍照旧搬运。签名只有 anthropic 有落点，没有签名的思考块不写进 anthropic 的请求与响应（不伪造签名，伪造会被上游以 400 拒绝），所以 chat 与 responses 的思考内容转到 anthropic 时丢弃；流式转换下发给 anthropic 客户端的思考块没有签名，客户端若把这些块回填进下一轮请求，上游会报 400。工具结果之后紧跟的追问并进同一条 user 消息，因为 anthropic 不接受两条相邻的 user 消息。思考配置参数（anthropic 的 `thinking`、chat 的 `reasoning_effort`）、`redacted_thinking` 的不透明 `data` 与 responses 的 `encrypted_content` 跨协议不搬，reasoning token 不单独记账（算进输出 token）。音频与文件类内容丢弃；anthropic 的服务端内置工具（`web_search` 等）跨协议时整条丢弃，chat 与 responses 的工具定义只认 function，编回 anthropic 时才原样保留 `type`；`tools[].strict` 不搬运，`is_error` 只有 anthropic 有落点，`allowed_tools` 降级为 `auto`。流式转换下断在工具参数中间时，已下发的参数片段撤不回来，收尾的错误帧会写明「工具调用参数在上游中断，内容不完整」，这一次记失败，程序不伪造闭合的 JSON。跨协议只搬运图片地址，没有其它多模态变换。不改写任何客户端配置文件；冷却时长按单次失败计算；探活调用不计入用量汇总；估算花费需要给账号填 `--price-in` 与 `--price-out`。
