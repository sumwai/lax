package cli_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"lax/internal/store"
	"lax/internal/store/storetest"
)

// 插件生命周期：upgrade、rollback、verify、logs、reload 与审计。
// 这些用例只碰本地目录、库与信号本身，不联网，也不起真的 serve。

// writePluginVersion 在临时目录里写一个指定版本的夹具，并把方言 ID 换成 suffix 结尾的那一个。
//
// 换方言 ID 是为了让「目录事实」在升级后真的变了：rollback 用例靠它断言
// model_operations 的绑定回到了旧版本的值。
func writePluginVersion(t *testing.T, version, suffix string) string {
	t.Helper()
	dir := writePluginFixture(t)
	if version != "1.0.0" {
		bumpFixtureVersion(t, dir, version)
	}
	if suffix != "1" {
		for _, name := range []string{"dialects.json", "models.json"} {
			path := filepath.Join(dir, name)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("读夹具 %s: %v", name, err)
			}
			updated := strings.ReplaceAll(string(raw), "com.example.echo/speech@1", "com.example.echo/speech@"+suffix)
			if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
				t.Fatalf("改夹具 %s: %v", name, err)
			}
		}
	}
	return dir
}

// mustInstall 装一个夹具，失败即停。
func mustInstall(t *testing.T, src string, extra ...string) {
	t.Helper()
	args := append([]string{"plugin", "install", src}, extra...)
	if code, _, stderr := run(t, args...); code != 0 {
		t.Fatalf("install 失败（退出码 %d）: %s", code, stderr)
	}
}

// TestPluginUpdate 钉住升级的三条规矩：同版本被拒、切活动版本要 --yes、
// 给了 --yes 之后两个版本共存而活动版本指向新的。
func TestPluginUpdate(t *testing.T) {
	sandbox(t)
	mustInit(t)
	v1 := writePluginVersion(t, "1.0.0", "1")
	mustInstall(t, v1)

	// 同版本号：活动版本就是它，不算升级。
	code, _, stderr := run(t, "plugin", "update", "com.example.echo", "--source", v1)
	if code != 1 {
		t.Fatalf("同版本升级的退出码 = %d，期望 1；stderr = %s", code, stderr)
	}
	if !strings.Contains(stderr, "活动版本就是 1.0.0") {
		t.Fatalf("stderr 应说明活动版本就是它: %s", stderr)
	}

	v2 := writePluginVersion(t, "1.1.0", "2")
	code, _, stderr = run(t, "plugin", "update", "com.example.echo", "--source", v2)
	if code != 1 {
		t.Fatalf("不给 --yes 的退出码 = %d，期望 1；stderr = %s", code, stderr)
	}
	if !strings.Contains(stderr, "--yes") {
		t.Fatalf("stderr 应要求 --yes: %s", stderr)
	}
	// 不给 --yes 时不该动库，也不该复制。
	db := storetest.Open(t)
	if p, _, _ := db.PluginByID("com.example.echo"); p.ActiveVersion != "1.0.0" {
		t.Fatalf("没给 --yes 却换了活动版本: %+v", p)
	}
	if _, err := os.Stat(pluginInstalledDir(t, "com.example.echo", "1.1.0")); !os.IsNotExist(err) {
		t.Fatalf("没给 --yes 却复制了包目录: %v", err)
	}

	code, out, stderr := run(t, "plugin", "update", "com.example.echo", "--source", v2, "--yes")
	if code != 0 {
		t.Fatalf("升级失败（退出码 %d）: %s", code, stderr)
	}
	for _, want := range []string{"已更新插件 com.example.echo 1.1.0", "com.example.echo/speech@2", "包目录"} {
		if !strings.Contains(out, want) {
			t.Fatalf("update 输出缺 %q: %s", want, out)
		}
	}

	p, _, _ := db.PluginByID("com.example.echo")
	if p.ActiveVersion != "1.1.0" {
		t.Fatalf("活动版本 = %q，期望 1.1.0", p.ActiveVersion)
	}
	versions, err := db.PluginVersions("com.example.echo")
	if err != nil || len(versions) != 2 {
		t.Fatalf("版本行 = %+v，err = %v", versions, err)
	}
	for _, version := range []string{"1.0.0", "1.1.0"} {
		if _, err := os.Stat(filepath.Join(pluginInstalledDir(t, "com.example.echo", version), "lax.plugin.json")); err != nil {
			t.Fatalf("版本 %s 的包目录不在: %v", version, err)
		}
	}
	op, ok, err := db.ModelOperation("echo", "echo-tts", "speech.synthesize@1")
	if err != nil || !ok || op.DialectID != "com.example.echo/speech@2" {
		t.Fatalf("升级后的方言绑定 = %+v ok=%v err=%v", op, ok, err)
	}
	if !hasPluginAudit(t, db, store.AuditUpdate) {
		t.Fatal("审计里没有 update")
	}

	// 升到一个装过但不是活动版本的号也要拒：版本行不可变，切回去用 rollback。
	code, _, stderr = run(t, "plugin", "update", "com.example.echo", "--source", v1, "--yes")
	if code != 1 || !strings.Contains(stderr, "已经装过") {
		t.Fatalf("重装已装版本: code=%d stderr=%s", code, stderr)
	}
}

// TestPluginUpdateDryRun 钉住 --dry-run 什么都不写。
func TestPluginUpdateDryRun(t *testing.T) {
	sandbox(t)
	mustInit(t)
	mustInstall(t, writePluginVersion(t, "1.0.0", "1"))

	src := writePluginVersion(t, "1.1.0", "2")
	code, out, stderr := run(t, "plugin", "update", "com.example.echo", "--source", src, "--dry-run")
	if code != 0 {
		t.Fatalf("update --dry-run 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"将更新插件 com.example.echo 1.1.0", "dry-run", "speech@2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run 输出缺 %q: %s", want, out)
		}
	}
	db := storetest.Open(t)
	if p, _, _ := db.PluginByID("com.example.echo"); p.ActiveVersion != "1.0.0" {
		t.Fatalf("dry-run 换了活动版本: %+v", p)
	}
	if _, err := os.Stat(pluginInstalledDir(t, "com.example.echo", "1.1.0")); !os.IsNotExist(err) {
		t.Fatalf("dry-run 复制了包目录: %v", err)
	}
}

// TestPluginRollback 钉住回滚：活动版本切回去，厂商目录事实按旧版本重写，审计留痕。
func TestPluginRollback(t *testing.T) {
	sandbox(t)
	mustInit(t)
	mustInstall(t, writePluginVersion(t, "1.0.0", "1"))
	if code, _, stderr := run(t, "plugin", "update", "com.example.echo", "--source", writePluginVersion(t, "1.1.0", "2"), "--yes"); code != 0 {
		t.Fatalf("升级失败: %s", stderr)
	}

	code, out, stderr := run(t, "plugin", "rollback", "com.example.echo", "--version", "1.0.0")
	if code != 0 {
		t.Fatalf("rollback 退出码 = %d，stderr = %s", code, stderr)
	}
	for _, want := range []string{"已回滚插件 com.example.echo：1.1.0 → 1.0.0", "重写了 1 家厂商"} {
		if !strings.Contains(out, want) {
			t.Fatalf("rollback 输出缺 %q: %s", want, out)
		}
	}

	db := storetest.Open(t)
	p, _, _ := db.PluginByID("com.example.echo")
	if p.ActiveVersion != "1.0.0" {
		t.Fatalf("活动版本 = %q，期望 1.0.0", p.ActiveVersion)
	}
	// 目录事实按旧版本重写：绑定回到旧版本声明的那条方言。
	op, ok, err := db.ModelOperation("echo", "echo-tts", "speech.synthesize@1")
	if err != nil || !ok || op.DialectID != "com.example.echo/speech@1" {
		t.Fatalf("回滚后的方言绑定 = %+v ok=%v err=%v，期望旧版本的方言", op, ok, err)
	}
	versions, err := db.PluginVersions("com.example.echo")
	if err != nil || len(versions) != 2 {
		t.Fatalf("回滚不该删版本行: %+v err=%v", versions, err)
	}
	if !hasPluginAudit(t, db, store.AuditRollback) {
		t.Fatal("审计里没有 rollback")
	}

	code, _, stderr = run(t, "plugin", "rollback", "com.example.echo", "--version", "9.9.9")
	if code != 1 || !strings.Contains(stderr, "没有装过版本 9.9.9") || !strings.Contains(stderr, "1.0.0") {
		t.Fatalf("不存在的版本的报错: code=%d stderr=%s", code, stderr)
	}
}

// TestPluginVerify 钉住 verify 的两头：干净包退出码 0；改一个字节之后指出是包哈希。
func TestPluginVerify(t *testing.T) {
	sandbox(t)
	mustInit(t)
	mustInstall(t, writePluginVersion(t, "1.0.0", "1"))

	code, out, stderr := run(t, "plugin", "verify")
	if code != 0 {
		t.Fatalf("干净包的 verify 退出码 = %d，stderr = %s，输出 %s", code, stderr, out)
	}
	for _, want := range []string{"ok", "包哈希", "manifest 哈希", "方言登记", "运行时"} {
		if !strings.Contains(out, want) {
			t.Fatalf("verify 输出缺 %q: %s", want, out)
		}
	}
	if strings.Contains(out, "fail") {
		t.Fatalf("干净包的 verify 有失败项: %s", out)
	}

	// 改包里的一个字节：目录哈希必然变，manifest 没动。
	models := filepath.Join(pluginInstalledDir(t, "com.example.echo", "1.0.0"), "models.json")
	raw, err := os.ReadFile(models)
	if err != nil {
		t.Fatalf("读包内文件: %v", err)
	}
	if err := os.WriteFile(models, append(raw, '\n'), 0o600); err != nil {
		t.Fatalf("改包内文件: %v", err)
	}

	code, out, stderr = run(t, "plugin", "verify", "com.example.echo")
	if code != 1 {
		t.Fatalf("被改过的包 verify 退出码 = %d，期望 1；stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "fail") || !strings.Contains(out, "包哈希") {
		t.Fatalf("verify 应指出包哈希没过: %s", out)
	}
	if strings.Contains(out, "manifest 哈希 fail") {
		t.Fatalf("manifest 没动，不该报 manifest 哈希失败: %s", out)
	}
	// verify 不写状态：tampered 只有 doctor 会写。
	db := storetest.Open(t)
	if p, _, _ := db.PluginByID("com.example.echo"); p.State != "ok" {
		t.Fatalf("verify 写了状态: %+v", p)
	}
}

// TestPluginLogs 钉住日志的三个筛选条件。
func TestPluginLogs(t *testing.T) {
	sandbox(t)
	mustInit(t)
	mustInstall(t, writePluginVersion(t, "1.0.0", "1"))

	db := storetest.Open(t)
	now := time.Now()
	old := store.PluginLog{
		PluginID: "com.example.echo", Level: "info", Message: "旧的一条",
		At: now.Add(-2 * time.Hour).Unix(),
	}
	fresh := store.PluginLog{
		PluginID: "com.example.echo", Level: "warn", Message: "新的一条",
		At: now.Add(-time.Minute).Unix(),
	}
	for _, entry := range []store.PluginLog{old, fresh} {
		if err := db.AppendPluginLog(entry); err != nil {
			t.Fatalf("写日志: %v", err)
		}
	}

	code, out, stderr := run(t, "plugin", "logs", "com.example.echo", "--json")
	if code != 0 {
		t.Fatalf("logs 退出码 = %d，stderr = %s", code, stderr)
	}
	var rows []struct {
		ID      int64  `json:"id"`
		Level   string `json:"level"`
		Message string `json:"message"`
		At      int64  `json:"at"`
	}
	mustJSON(t, out, &rows)
	if len(rows) != 2 || rows[0].Message != "新的一条" || rows[1].Message != "旧的一条" {
		t.Fatalf("logs --json = %+v，期望按时间倒序两条", rows)
	}

	code, out, _ = run(t, "plugin", "logs", "com.example.echo", "--level", "warn", "--json")
	if code != 0 {
		t.Fatalf("logs --level 退出码 = %d", code)
	}
	mustJSON(t, out, &rows)
	if len(rows) != 1 || rows[0].Message != "新的一条" {
		t.Fatalf("logs --level warn = %+v", rows)
	}

	// 时长写法：只看最近 30 分钟，旧的那条被筛掉。
	code, out, _ = run(t, "plugin", "logs", "com.example.echo", "--since", "30m", "--json")
	if code != 0 {
		t.Fatalf("logs --since 退出码 = %d", code)
	}
	mustJSON(t, out, &rows)
	if len(rows) != 1 || rows[0].Message != "新的一条" {
		t.Fatalf("logs --since 30m = %+v", rows)
	}

	// RFC3339 与时长等价。
	stamp := now.Add(-30 * time.Minute).Format(time.RFC3339)
	code, out, _ = run(t, "plugin", "logs", "com.example.echo", "--since", stamp, "--json")
	if code != 0 {
		t.Fatalf("logs --since RFC3339 退出码 = %d", code)
	}
	mustJSON(t, out, &rows)
	if len(rows) != 1 || rows[0].Level != "warn" {
		t.Fatalf("logs --since %s = %+v", stamp, rows)
	}

	code, _, stderr = run(t, "plugin", "logs", "com.example.echo", "--since", "昨天")
	if code != 2 || !strings.Contains(stderr, "RFC3339") {
		t.Fatalf("坏 --since: code=%d stderr=%s", code, stderr)
	}

	code, _, stderr = run(t, "plugin", "logs", "com.example.nope")
	if code != 1 || !strings.Contains(stderr, "没有安装") {
		t.Fatalf("未知插件的 logs: code=%d stderr=%s", code, stderr)
	}
}

// TestPluginReloadRequiresServe 钉住 reload 的两种失败：没有 pid 文件、有 pid 文件但进程不在。
func TestPluginReloadRequiresServe(t *testing.T) {
	sandbox(t)
	mustInit(t)
	mustInstall(t, writePluginVersion(t, "1.0.0", "1"))

	code, _, stderr := run(t, "plugin", "reload")
	if code != 1 {
		t.Fatalf("没有 pid 文件时 reload 的退出码 = %d，期望 1；stderr = %s", code, stderr)
	}
	if !strings.Contains(stderr, "没在跑") || !strings.Contains(stderr, "lax.pid") {
		t.Fatalf("stderr 应说明 serve 没在跑: %s", stderr)
	}

	// 起一个马上退出的子进程，拿它回收后的 pid 当「文件在、进程已不在」。
	child := exec.Command("true")
	if err := child.Run(); err != nil {
		t.Fatalf("跑子进程: %v", err)
	}
	pidPath := filepath.Join(pluginDataDir(t), "lax.pid")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(child.Process.Pid)+"\n"), 0o600); err != nil {
		t.Fatalf("写 pid 文件: %v", err)
	}

	code, _, stderr = run(t, "plugin", "reload")
	if code != 1 {
		t.Fatalf("进程不在时 reload 的退出码 = %d，期望 1；stderr = %s", code, stderr)
	}
	if !strings.Contains(stderr, "进程不存在") || !strings.Contains(stderr, "没在跑") {
		t.Fatalf("stderr 应说明进程不存在: %s", stderr)
	}

	code, _, stderr = run(t, "plugin", "reload", "com.example.nope")
	if code != 1 || !strings.Contains(stderr, "没有安装") {
		t.Fatalf("未知插件的 reload: code=%d stderr=%s", code, stderr)
	}
}

// TestPluginAuditLifecycle 钉住 install → enable → disable → rm 四条审计。
func TestPluginAuditLifecycle(t *testing.T) {
	sandbox(t)
	mustInit(t)
	src := writePluginVersion(t, "1.0.0", "1")
	mustInstall(t, src)
	for _, step := range [][]string{
		{"plugin", "enable", "com.example.echo"},
		{"plugin", "disable", "com.example.echo"},
		{"plugin", "rm", "com.example.echo"},
	} {
		if code, _, stderr := run(t, step...); code != 0 {
			t.Fatalf("%v 失败（退出码 %d）: %s", step, code, stderr)
		}
	}

	db := storetest.Open(t)
	audits, err := db.PluginAuditLog("com.example.echo", 20)
	if err != nil {
		t.Fatalf("读审计: %v", err)
	}
	want := []string{store.AuditInstall, store.AuditEnable, store.AuditDisable, store.AuditRemove}
	if len(audits) != len(want) {
		t.Fatalf("审计条数 = %d，期望 %d：%+v", len(audits), len(want), audits)
	}
	seen := map[string]bool{}
	for _, a := range audits {
		seen[a.Action] = true
		if a.Actor != "cli" {
			t.Fatalf("审计的发起人 = %q，期望 cli", a.Actor)
		}
	}
	for _, action := range want {
		if !seen[action] {
			t.Fatalf("审计里没有 %s：%+v", action, audits)
		}
	}
}

// hasPluginAudit 判断一个插件的审计里有没有某个动作。
func hasPluginAudit(t *testing.T, db *storetest.DB, action string) bool {
	t.Helper()
	audits, err := db.PluginAuditLog("com.example.echo", 20)
	if err != nil {
		t.Fatalf("读审计: %v", err)
	}
	for _, a := range audits {
		if a.Action == action {
			return true
		}
	}
	return false
}
