# switchboard

本机上的模型总机。账号在命令行里管，所有客户端只连一个地址。

```bash
switchboard init                                            # 建配置、数据库与密钥
switchboard account add ds --provider deepseek --key sk-xxx  # 或用 --key - 从标准输入读
switchboard account sync ds                                 # 拉上游模型列表
switchboard account probe ds                                # 逐个探活，能用的才进选号
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
| `account sync` | 从上游拉模型列表，写进本地 |
| `account probe` | 逐个模型探活，把能用的写进账号 |
| `model ls` | 列出模型、可用账号与状态 |
| `usage` | 按账号、模型或日期汇总用量 |
| `log tail` | 最近的请求记录 |
| `call` | 手工打一条请求，打印选号过程 |
| `doctor` | 自检配置、目录、密钥、端口、数据库 |

每个命令都支持 `--json`，结果走 stdout，错误走 stderr。退出码：0 成功、1 运行失败、2 用法错误。

## 模型从上游拉，再探活

模型列表不用手填。`account sync` 拉上游的模型清单，`account probe` 逐个试一遍，能用的才进入选号：

```bash
switchboard account add ds --provider deepseek --key sk-xxx
switchboard account sync ds     # 拉模型列表，地址由 base-url 推出
switchboard account probe ds    # 每个模型发一条 max_tokens=1 的请求
switchboard serve
```

探活会真实调用上游：模型多时用 `--limit 5` 控制这一次花多少，`--model deepseek-v4-pro` 只试一个。探不出可用模型时退出码 1，账号原有模型列表不变。`account ls` 与 `model ls` 会带上每个账号的模型状态（可用 / 待探活 / 无权 / 不存在 / 凭据被拒 / 失败）。

## 客户端怎么接

客户端里把三样填成本机地址即可，程序不改任何客户端配置：

```
ANTHROPIC_BASE_URL=http://127.0.0.1:4567
ANTHROPIC_AUTH_TOKEN=switchboard-local    # 本机网关不校验，填个非空值即可
ANTHROPIC_MODEL=deepseek-v4-pro           # 必须是 probe 出来的可用模型名
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
