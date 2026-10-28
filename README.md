# switchboard

本机上的模型总机。账号在命令行里管，所有客户端只连一个地址。

```bash
switchboard init                                            # 建配置、数据库与密钥
switchboard account add ds --provider deepseek --key sk-xxx  # 或用 --key - 从标准输入读
switchboard model sync --account ds                         # 拉上游模型列表
switchboard model probe --account ds                        # 逐个探活，能用的才进选号
switchboard serve                                           # 起网关，127.0.0.1:4567
switchboard log tail                                        # 看请求记录
```

入站 `/v1/messages`（Anthropic Messages），出站支持同协议直通与 OpenAI Chat 互转，含流式与工具调用。字段映射、选号规则、数据模型、里程碑见 [DESIGN.md](DESIGN.md)。

## 命令

| 命令 | 作用 |
| --- | --- |
| `init` | 建配置目录、数据库、加密密钥 |
| `serve` | 前台起网关 |
| `account add\|ls\|rm\|test\|enable\|disable` | 加账号、列账号、删账号、探活、启用、停用 |
| `model ls` | 列出模型、可用账号与状态，可按厂商与账号筛选 |
| `model sync` | 从上游拉模型列表，写进本地，可按厂商与账号筛选 |
| `model probe` | 逐个模型探活，把能用的写进账号，可按厂商与账号筛选 |
| `usage` | 按账号、模型或日期汇总用量 |
| `log tail` | 最近的请求记录 |
| `call` | 手工打一条请求，打印选号过程 |
| `doctor` | 自检配置、目录、密钥、端口、数据库 |

每个命令都支持 `--json`，结果走 stdout，错误走 stderr。退出码：0 成功、1 运行失败、2 用法错误。

## 模型从上游拉，再探活

模型列表不用手填。`model sync` 拉上游的模型清单，`model probe` 逐个试一遍，能用的才进入选号。两条命令都接受 `--provider` 与 `--account`，不给就处理全部账号：

```bash
switchboard account add ds --provider deepseek --key sk-xxx
switchboard model sync                                     # 全部账号各拉一次模型列表
switchboard model sync --provider deepseek --account ds   # 只处理这一个账号
switchboard model probe --account ds --limit 5            # 只探这个账号的前 5 个模型
switchboard serve
```

探活会真实调用上游：`--limit 5` 限制每个账号这一次最多探几个，`--model deepseek-v4-pro` 只试一个。某个账号没探出可用模型时只写探活结果，它原有的模型列表保持不变；全部账号都没探出可用模型时退出码 1。碰到 401 时命令停下并停用该账号：凭据被拒，后面的结论不作数。同步失败的账号同样不影响其余账号，只是最终退出码为 1。`account ls` 与 `model ls` 会带上每个账号的模型状态（可用 / 待探活 / 无权 / 不存在 / 凭据被拒 / 失败）。

## 客户端怎么接

客户端里把三样填成本机地址即可，程序不改任何客户端配置：

```
ANTHROPIC_BASE_URL=http://127.0.0.1:4567
ANTHROPIC_AUTH_TOKEN=switchboard-local    # 本机网关不校验，填个非空值即可
ANTHROPIC_MODEL=deepseek-v4-pro           # 必须是探活出来的可用模型名
```

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

只支持 Anthropic Messages 入站；不改写任何客户端配置文件；冷却时长按单次失败计算；探活调用不计入用量汇总；估算花费需要给账号填 `--price-in` 与 `--price-out`。
