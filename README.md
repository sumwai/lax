# switchboard

本机上的模型总机。账号在命令行里管，所有客户端只连一个地址。

```bash
switchboard init                                            # 建配置、数据库与密钥
switchboard provider ls                                     # 看已知厂商与各协议的路径
switchboard account add ds --provider deepseek --key sk-xxx  # 或用 --key - 从标准输入读
switchboard model sync --account ds                         # 拉上游模型列表
switchboard model probe --account ds                        # 逐个探活，能用的才进选号
switchboard serve                                           # 起网关，127.0.0.1:4567
switchboard log tail                                        # 看请求记录
```

三种线协议都能进也能出。入站按路径分 anthropic、openai-chat、openai-responses，出站按账号的 `protocol` 字段；入站与出站相同则直通，不同则经中间表示转换，九种组合都可用，流式与工具调用包含在内。中间表示、映射规则、选号规则、数据模型、里程碑见 [DESIGN.md](DESIGN.md)。

## 命令

| 命令 | 作用 |
| --- | --- |
| `init` | 建配置目录、数据库、加密密钥 |
| `serve` | 前台起网关 |
| `account add\|ls\|rm\|test\|enable\|disable` | 加账号、列账号、删账号、探活、启用、停用 |
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

路径决定入站协议，三条路径同时可用，一个网关可以同时接三种客户端：

| 入站路径 | 入站协议 | 常见客户端 |
| --- | --- | --- |
| `POST /v1/messages` | anthropic | Claude Code |
| `POST /v1/chat/completions` | openai-chat | 普通 OpenAI SDK 客户端 |
| `POST /v1/responses` | openai-responses | Codex |

出站协议由账号决定：`account add --protocol` 取值 `anthropic`、`openai-chat`、`openai-responses`，留空则取该厂商的默认协议（`provider add --default-protocol` 显式存的优先，没显式存则按 `openai-chat` → `openai-responses` → `anthropic` 取第一条路由）。厂商与各协议的路径在数据库里，`switchboard provider ls` 能看到，`provider add` / `set` / `unset` 可以增删改，不必重编译。入站协议与出站协议相同时直通，请求体只改 `model` 字段，其余字段与值原样带到上游，响应按字节原样返回；不同时经中间表示转换，请求与响应都只搬映射表里定义的字段，表外字段丢弃。未知路径返回 404，请求体解析失败返回 400，错误消息带协议名前缀。

自己加一家厂商时，每个协议一条 flag（`--anthropic`、`--openai`、`--responses`，目前支持这三种），值是相对 `--base-url` 的完整路径、必须以 `/` 开头。`--default-protocol` 定下这家的默认协议，取值写 `anthropic`、`openai`、`responses`（也认线协议全名 `openai-chat`、`openai-responses`），必须落在已声明的路由上：

```bash
# 新建厂商：--base-url 必填，至少一条协议路由
switchboard provider add myco --base-url https://api.deepseek.com \
    --openai /v1/chat/completions \
    --anthropic /anthropic/v1/messages \
    --default-protocol openai
已添加厂商 myco（https://api.deepseek.com），2 条路由，默认协议 openai-chat
```

`provider set` 只设值：补一条路由、改默认协议或地址，没传的字段一律保留。传空值是用法错误，删除要显式声明：

```bash
# 补一条 responses 路由，并把默认协议换掉
switchboard provider set myco --responses /v1/responses --default-protocol responses

# 删一条路由；也可以一次删多个（models-path、default-protocol、note 同样可用）
switchboard provider unset myco anthropic
```

`provider unset` 幂等：要删的项本来就不存在时不报错，只说明一句「myco 本来就没有 anthropic 路由」。删到一条路由都不剩会被拒绝，那是不想要这家厂商了，用 `provider rm`。`--models-path <路径>` 是厂商级的模型列表覆盖，`provider unset myco models-path` 清掉它、回到按各路由取。

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

探活会真实调用上游：`--limit 5` 限制每个账号这一次最多探几个，`--model deepseek-v4-pro` 只试一个。某个账号没探出可用模型时只写探活结果，它原有的模型列表保持不变；全部账号都没探出可用模型时退出码 1。碰到 401 时命令停下并停用该账号：凭据被拒，后面的结论不作数。同步失败的账号同样不影响其余账号，只是最终退出码为 1。`account ls` 与 `model ls` 会带上每个账号的模型状态（可用 / 待探活 / 无权 / 不存在 / 凭据被拒 / 失败）。

## 客户端怎么接

客户端里把地址填成本机网关即可，程序不改任何客户端配置。Claude Code 一例：

```
ANTHROPIC_BASE_URL=http://127.0.0.1:4567
ANTHROPIC_AUTH_TOKEN=switchboard-local    # 本机网关不校验，填个非空值即可
ANTHROPIC_MODEL=deepseek-v4-pro           # 必须是探活出来的可用模型名
```

说 openai-chat 与 openai-responses 的客户端把 base URL 填成 `http://127.0.0.1:4567/v1`，模型名同样取探活出来的可用值；凭据值本机不校验，填一个非空值即可。

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

gemini 协议进出都没有实现。转换时丢弃思考与推理内容（anthropic 的 `thinking` 块、chat 的 `reasoning_content`、responses 的 `reasoning` item）、音频与文件类内容、responses 的内置工具（`web_search` 等）；跨协议只搬运图片地址，没有其它多模态变换。不改写任何客户端配置文件；冷却时长按单次失败计算；探活调用不计入用量汇总；估算花费需要给账号填 `--price-in` 与 `--price-out`。
