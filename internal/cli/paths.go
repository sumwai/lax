package cli

import (
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

// checkEndpoint 是路径类取值（三个协议 flag、--models-path、`model sync --endpoint`）唯一的
// 格式校验入口，判定交给 store.NormalizeEndpoint（见 DESIGN 5.8）：两种写法都收，含 `://` 的按
// 整条地址校验 scheme 与主机名，其余按相对 base_url 的路径规整。cli 不另写一份 scheme 或
// 主机名预检——两份判定迟早对不上（`https://`、`ftp://host/x`、`https:///x` 的判定只有 store 一份）。
//
// 校验不过时包成用法错误（退出码 2）：文案保留 store 的原话（它已经写明缺的是主机名还是
// scheme 不对），再附这个子命令的用法行。取值本身仍按原样交给 store 的写入口，
// 落库形态由 store 决定，cli 不做第二次规整。
func checkEndpoint(value, usageLine string) error {
	if _, err := store.NormalizeEndpoint(value); err != nil {
		return usagef("%v\n%s", err, usageLine)
	}
	return nil
}
