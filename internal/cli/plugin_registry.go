package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"lax/internal/config"
	"lax/internal/plugin"
	"lax/internal/store"
)

// 插件包在数据目录里的布局：installed/<插件 ID>/<版本>/ 是不可变目录，staging/ 是安装时的
// 临时落点。两者在同一个文件系统下，staging 里装好的目录 rename 过去就位，装到一半的包
// 不会出现在 installed/ 下。
const (
	pluginPackagesDir  = "plugins"
	pluginInstalledDir = "installed"
	pluginStagingDir   = "staging"
)

// pluginDataDirOf 是插件包所在的数据目录：与库文件同级。
func pluginDataDirOf(paths config.Paths) string {
	return filepath.Dir(paths.DB)
}

// pluginRoot 是插件包目录，挂在库文件旁边：包随数据目录搬家，配置目录里只放配置。
func pluginRoot(dataDir string) string {
	return filepath.Join(dataDir, pluginPackagesDir)
}

// pluginInstalledRoot 是已安装版本的父目录。
func pluginInstalledRoot(dataDir string) string {
	return filepath.Join(pluginRoot(dataDir), pluginInstalledDir)
}

// pluginStagingRoot 是安装时的临时目录。
func pluginStagingRoot(dataDir string) string {
	return filepath.Join(pluginRoot(dataDir), pluginStagingDir)
}

// pluginVersionDir 是一个已安装版本的目录。
func pluginVersionDir(dataDir, id, version string) string {
	return filepath.Join(pluginInstalledRoot(dataDir), id, version)
}

// loadRegistry 组装网关用的方言注册表：内置方言打底，再按活动版本登记库里启用的插件。
//
// 加载失败的插件只进 notes，不返回错误：一个装坏的插件不该让整个网关起不来，
// 用户还有别的厂商在用。notes 交给调用方打成警告，ls 与 doctor 负责把细节说清楚。
//
// 带代码（moejs）的插件要开沙箱才能登记 hook 方言，失败时另把原因写进插件状态：
// 它不像声明式插件那样能在请求时绕开，状态栏要能看出「装了但没起来」。
//
// human 是沙箱日志顺带打给人看的目标，通常就是标准输出；传 nil 时只落库。
func loadRegistry(st *store.Store, dataDir string, human io.Writer) (*plugin.Registry, []string, error) {
	reg := plugin.DefaultRegistry()
	plugins, err := st.ListPlugins()
	if err != nil {
		return nil, nil, err
	}
	var notes []string
	for _, p := range plugins {
		if !p.Enabled || p.State == pluginStateTampered {
			continue
		}
		if p.ActiveVersion == "" {
			notes = append(notes, fmt.Sprintf("插件 %s 没有活动版本，跳过加载：用 lax plugin show %s 看已装版本", p.ID, p.ID))
			continue
		}
		dir := pluginVersionDir(dataDir, p.ID, p.ActiveVersion)
		loaded, err := plugin.LoadDir(dir)
		if err != nil {
			notes = append(notes, fmt.Sprintf("插件 %s %s 加载失败，跳过：%v；用 lax plugin doctor %s 定位", p.ID, p.ActiveVersion, err, p.ID))
			continue
		}

		// 路径按安装时记下的 runtime 选：装的时候确认的信任级别决定这包能不能跑代码。
		// 两处对不上就不加载：包被改过时，按 manifest 自报的 runtime 走等于让它自己选信任级别。
		runtime := loaded.Manifest.Runtime
		if pv, ok, err := st.ActivePluginVersion(p.ID); err == nil && ok && pv.Runtime != "" {
			if pv.Runtime != loaded.Manifest.Runtime {
				reason := fmt.Sprintf("安装时记的运行时是 %s，包里的 manifest 写的是 %s", pv.Runtime, loaded.Manifest.Runtime)
				notes = append(notes, fmt.Sprintf("插件 %s %s 的运行时对不上，跳过：%s；用 lax plugin doctor %s 定位", p.ID, p.ActiveVersion, reason, p.ID))
				if err := st.SetPluginState(p.ID, pluginStateFailed, reason); err != nil {
					notes = append(notes, fmt.Sprintf("插件 %s 的状态没写进去：%v", p.ID, err))
				}
				continue
			}
			runtime = pv.Runtime
		}

		options, err := st.PluginOptions(p.ID)
		if err != nil {
			notes = append(notes, fmt.Sprintf("插件 %s 的配置项读取失败，按默认值继续：%v", p.ID, err))
			options = nil
		}
		opts := plugin.SandboxOptions{
			Manifest: loaded.Manifest,
			Root:     dir,
			Entry:    loaded.Manifest.Entry,
			Trust:    p.Trust,
			Options:  options,
			Logf:     pluginLogSink(st, p.ID, human),
		}
		if runtime == plugin.RuntimeMoejs {
			if err := loaded.Register(reg, opts); err != nil {
				_ = loaded.Close()
				reason := "带代码的插件没起来：" + err.Error()
				notes = append(notes, fmt.Sprintf("插件 %s %s %s；用 lax plugin doctor %s 定位", p.ID, p.ActiveVersion, reason, p.ID))
				if err := st.SetPluginState(p.ID, pluginStateFailed, reason); err != nil {
					notes = append(notes, fmt.Sprintf("插件 %s 的状态没写进去：%v", p.ID, err))
				}
				continue
			}
			// 沙箱要活到网关停：方言里的每个 hook 调用都经它。
			clearFailedState(st, p, &notes)
			continue
		}
		if err := reg.RegisterPlugin(loaded); err != nil {
			notes = append(notes, fmt.Sprintf("插件 %s %s 的方言登记失败，跳过：%v；用 lax plugin doctor %s 定位", p.ID, p.ActiveVersion, err, p.ID))
			continue
		}
		clearFailedState(st, p, &notes)
	}
	return reg, notes, nil
}

// ---------------------------------------------------------------- 沙箱日志落库

// pluginLogMaxBytes 是单条日志消息的上限，与沙箱里 lax.log 的上限一致。
const pluginLogMaxBytes = 8 << 10

// pluginSandboxLogFormat 是沙箱打 lax.log 时用的格式。
//
// 这里照抄 internal/plugin 里的那一份：SandboxOptions.Logf 只给 (format, args)，没有级别参数，
// 级别只能从这个固定形状里取（第二段），取不到就按 info 记。
const pluginSandboxLogFormat = "插件 %s %s: %s"

// pluginLogSink 把沙箱的日志写进 plugin_logs，同时照样打一行给人看：
// 排障时既要在 serve 的输出里看见，也要能在事后按插件翻出来。
//
// hook 字段留空：日志入口拿不到正在跑的是哪个钩子，编一个比空着更误导。
// 写库失败只打一行警告：插件日志不该把一次请求拖垮。
func pluginLogSink(st *store.Store, pluginID string, human io.Writer) func(format string, args ...any) {
	return func(format string, args ...any) {
		level, message := pluginLogFields(format, args)
		// 给人看的那一行用沙箱原本的整句（带插件 ID 与级别）：多插件同时说话时，
		// serve 的输出仍然能看出是谁在说。落库那一行只留正文，前缀由列承担。
		if human != nil {
			fmt.Fprintf(human, "%s %s\n", time.Now().Format("15:04:05"), pluginLogMessage(fmt.Sprintf(format, args...)))
		}
		err := st.AppendPluginLog(store.PluginLog{PluginID: pluginID, Level: level, Message: message})
		if err != nil && human != nil {
			fmt.Fprintf(human, "警告：插件 %s 的日志没写进库里：%v\n", pluginID, err)
		}
	}
}

// pluginLogFields 从沙箱的一次格式化调用里取出级别与正文。
// 认不出 lax.log 的形状时整条按 info 记，正文用原来那段格式化结果：
// 沙箱自己的告警本来就没有级别。
func pluginLogFields(format string, args []any) (string, string) {
	if format == pluginSandboxLogFormat && len(args) == 3 {
		level, isText := args[1].(string)
		message, messageIsText := args[2].(string)
		if isText && messageIsText && knownPluginLogLevel(level) {
			return level, pluginLogMessage(message)
		}
	}
	return "info", pluginLogMessage(fmt.Sprintf(format, args...))
}

// pluginLogMessage 把消息收成一行并截到上限。
// 换行会把后续内容伪装成宿主自己的输出；截断按字节退到字符边界，免得落进库的是半个汉字。
func pluginLogMessage(message string) string {
	message = strings.ReplaceAll(strings.ReplaceAll(message, "\n", " "), "\r", " ")
	if len(message) <= pluginLogMaxBytes {
		return message
	}
	cut := pluginLogMaxBytes
	for cut > 0 && !utf8.RuneStart(message[cut]) {
		cut--
	}
	return message[:cut] + "…"
}

// knownPluginLogLevel 是插件日志的四个级别，与 lax.log 接受的一致。
func knownPluginLogLevel(level string) bool {
	switch level {
	case "debug", "info", "warn", "error":
		return true
	}
	return false
}

// clearFailedState 把「上次加载失败」翻回 ok：启动成功就说明原因已经过去，
// 否则 failed 会挂在状态栏上永远不翻。
func clearFailedState(st *store.Store, p store.Plugin, notes *[]string) {
	if p.State != pluginStateFailed {
		return
	}
	if err := st.SetPluginState(p.ID, pluginStateOK, ""); err != nil {
		*notes = append(*notes, fmt.Sprintf("插件 %s 的状态没写进去：%v", p.ID, err))
	}
}
