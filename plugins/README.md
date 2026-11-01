# 本机插件包

这个目录存放本机的 lax 插件包。插件内容不进入版本库：本目录的 `.gitignore`
忽略全部子项，只保留本文件与 `.gitignore` 自身。这样插件代码、测试与本机专用配置可以放在这里，
而不会被误提交。

## 目录结构

每个插件是一个独立子目录，至少包含：

```text
plugins/<插件名>/
  lax.plugin.json   # 插件 ID、版本、运行时、厂商、路由、权限
  dialects.json     # 方言声明
  models.json       # 模型目录与操作绑定
  index.js          # runtime: "moejs" 时的入口
  README.md         # 该插件的安装与使用说明
```

新增或修改插件后先跑 `lax plugin doctor`，再重新安装。同一版本不能覆盖，
改动后要把 `lax.plugin.json` 里的 `version` 升一号。

## 安装

```bash
lax plugin install ./plugins/<插件名> --trust sandboxed --yes
lax plugin reload
```

已经发布到 GitHub 的插件可以直接从插件目录地址安装，不必先克隆仓库：

```bash
lax plugin install https://github.com/<owner>/<repo>/tree/<ref>/plugins/<插件名> \
    --trust sandboxed --yes
```

`<ref>` 可以是分支、tag 或提交号。也可以写 `github.com/<owner>/<repo>/<path>` 或
`github:<owner>/<repo>/<path>[@<ref>]`；省略 ref 时取仓库默认分支。远程安装只支持公开仓库。

官方 `lax-plugins` 仓库里的插件还可以只写目录名：

```bash
lax plugin install doubao-seed-tts --trust sandboxed --yes
lax plugin install doubao-seed-tts@1.2.1 --trust sandboxed --yes
```

不带 `@` 时取最新版；带 `@1.2.1` 时固定到该插件的发布 tag
`doubao-seed-tts-v1.2.1`。当前目录有同名目录且不带 `@` 时按本地目录装；
没有才映射到 `github:sumwai/lax-plugins/plugins/doubao-seed-tts`。本地有同名目录
又想强制取远程最新版时写 `doubao-seed-tts@main`。

`runtime: "declarative"` 的插件用 `--trust declarative`；`moejs` 插件用
`--trust sandboxed`。安装前可以用 `--dry-run` 只看计划，不复制、不写库：

```bash
lax plugin install ./plugins/<插件名> --trust sandboxed --dry-run
```

## 测试

插件目录里的 Go 测试只在当前本机存在，克隆仓库后不会带上；从 GitHub 安装时测试文件会随包复制到数据目录，但不参与运行：

```bash
go test ./plugins/<插件名>
```

核心包（`internal/...`）不依赖任何具体厂商或插件；厂商协议映射的测试应放在
插件自己的目录里。网关的通用回归测试只使用假方言，不加载这里的插件包。

## 注意

- 不要在本目录保存上游凭据；凭据由 `lax account add --key -` 加密写入数据库。
- 插件包不进 Git，换机器或重装前请单独备份；需要共享时再发布成独立仓库或压缩包。
- 删除本机插件用 `lax plugin rm <插件 ID>`，同时清掉它带来的厂商、模型与方言绑定。
