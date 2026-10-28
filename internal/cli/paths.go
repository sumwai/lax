package cli

import (
	"strings"

	"switchboard/internal/store"
)

// checkBaseURL 是 --base-url 唯一的校验入口：provider add、provider set 与 account add 三处都走它。
// 判定交给 store.NormalizeBaseURL（见 DESIGN 5.8），cli 不另写一份 scheme 或主机名预检——
// 两份判定迟早对不上，`https://` 这种只有 scheme 的写法会被误报成「要以 http:// 或 https:// 开头」。
//
// 返回的地址就是 store 写入口落库的形态：scheme 归一成小写、路径里的重复斜杠合并、末尾斜杠去掉，
// 因此 `HTTPS://api.example.com/` 也能用。出错时包成用法错误（退出码 2）：
// 文案保留 store 的原话（它已经写明缺什么、正确地址长什么样），再附这个子命令的用法行，
// 用户不用先去猜语法。
func checkBaseURL(value, usageLine string) (string, error) {
	normalized, err := store.NormalizeBaseURL(value)
	if err != nil {
		return "", usagef("%v\n%s", err, usageLine)
	}
	return normalized, nil
}

// hasFullAddress 判断路径类取值里是不是写了整条地址。
// 相对 base_url 的路径不含 `://`，只有把完整地址抄进路径参数才会出现它。
func hasFullAddress(value string) bool {
	return strings.Contains(value, "://")
}

// pathGotFullAddress 是路径类 flag 收到整条地址时的统一文案（见 DESIGN 4、5.8）。
// `--openai https://api.x/v1/chat/completions` 会被 store.NormalizePath 规整成
// `/https:/api.x/v1/chat/completions`：一个谁也打不通的地址，静默落库比当场报错难查得多。
// where 说明整条地址该写在哪个参数上（中文开头时不加空格，参数名开头时加一个），
// example 给出这个子命令的正确写法，usageLine 是用法行。
func pathGotFullAddress(flagName, value, where, example, usageLine string) error {
	return usagef("--%s 收到的是整条地址 %q：这里是相对 base_url 的路径，不是完整地址，写进来会被规整成 /https:/… 这种打不通的地址。%s，路径只写地址后面的部分，例如 %s\n%s",
		flagName, value, joinCN("完整地址写在", where), example, usageLine)
}
