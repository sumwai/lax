# switchboard

本机上的模型总机。账号在命令行里管，所有客户端只连一个地址。

```bash
switchboard init                      # 建配置与数据库
switchboard account add ds --provider deepseek --key sk-xxx --models deepseek-v4-pro
switchboard serve                     # 起网关，127.0.0.1:3425
switchboard agent use claude-code --model deepseek-v4-pro --apply
switchboard log tail                  # 看请求记录
```

支持的入站协议与出站协议、字段映射、里程碑，见 [DESIGN.md](DESIGN.md)。

## 状态

第一版按里程碑推进，当前在 M0。`init`、`doctor`、`version` 可用，其余子命令已定义并返回「尚未实现」。

## 构建

```bash
make check        # go vet + test + build
./switchboard --help
```

只需要 Go 工具链，生成的 `switchboard` 是单文件，不依赖外部服务。

## 数据在哪

| 路径 | 内容 |
| --- | --- |
| `$XDG_CONFIG_HOME/switchboard/config.json` | 监听地址、数据库路径 |
| `$XDG_DATA_HOME/switchboard/switchboard.db` | 账号与请求记录 |
| `$XDG_DATA_HOME/switchboard/secret.key` | 加密凭据的密钥 |

凭据用 AES-256-GCM 加密后入库。这防的是备份文件与日志泄漏，不防同一台机器上以同一用户身份运行的程序。
