# switchboard

本机上的模型总机。账号在命令行里管，所有客户端只连一个地址。

```bash
switchboard init                      # 建配置、数据库与密钥
switchboard account add ds --provider deepseek --key sk-xxx --models deepseek-v4-pro
switchboard serve                     # 起网关，127.0.0.1:3425
switchboard agent use claude-code --model deepseek-v4-pro --apply
switchboard log tail                  # 看请求记录
```

入站 `/v1/messages`（Anthropic Messages），出站支持同协议直通与 OpenAI Chat 互转，含流式与工具调用。字段映射、选号规则、数据模型、里程碑见 [DESIGN.md](DESIGN.md)。

## 命令

| 命令 | 作用 |
| --- | --- |
| `init` | 建配置目录、数据库、加密密钥 |
| `serve` | 前台起网关 |
| `account add\|ls\|rm\|test` | 加账号、列账号、删账号、探活 |
| `model ls` | 列出可提供的模型与定价 |
| `agent ls\|use\|revert` | 把客户端指过来 / 还原 |
| `usage` | 按账号、模型或日期汇总用量 |
| `log tail` | 最近的请求记录 |
| `call` | 手工打一条请求，打印选号过程 |
| `doctor` | 自检配置、目录、密钥、端口、数据库 |

每个命令都支持 `--json`，结果走 stdout，错误走 stderr。退出码：0 成功、1 运行失败、2 用法错误。

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
| `$XDG_DATA_HOME/switchboard/switchboard.db` | 账号、请求记录、客户端改写状态 |
| `$XDG_DATA_HOME/switchboard/secret.key` | 加密凭据的密钥 |

凭据用 AES-256-GCM 加密后入库。这防的是备份文件与日志泄漏，不防同一台机器上以同一用户身份运行的程序。

## 已知边界

只支持 Anthropic Messages 入站；客户端配置改写只做 Claude Code；冷却时长按单次失败计算；估算花费需要给账号填 `--price-in` 与 `--price-out`。
