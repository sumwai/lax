package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lax/internal/config"
)

// TestDoctorLegacyDataHint 钉住改名后的老数据提示：新库还没建、旧目录里还有库时，
// doctor 要报一项失败，并给出把库与密钥一起搬过来的命令；按提示搬完之后这一项消失。
// 这里从 legacyDB 拿到旧路径，不另写一份字面量，免得两处对不上。
func TestDoctorLegacyDataHint(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	t.Setenv("HOME", filepath.Join(dir, "home"))

	paths, err := config.Resolve("", "")
	if err != nil {
		t.Fatalf("解析路径失败: %v", err)
	}
	oldDB := legacyDB()
	if oldDB == "" {
		t.Fatal("legacyDB 返回空路径，测不了这一项")
	}
	if err := os.MkdirAll(filepath.Dir(oldDB), 0o700); err != nil {
		t.Fatalf("建旧目录失败: %v", err)
	}
	if err := os.WriteFile(oldDB, nil, 0o600); err != nil {
		t.Fatalf("写旧库文件失败: %v", err)
	}

	doctor := func() (int, string) {
		var buf bytes.Buffer
		code := Run([]string{"doctor"}, &buf, &buf)
		return code, buf.String()
	}

	code, out := doctor()
	if !strings.Contains(out, "旧数据搬迁") {
		t.Fatalf("新库不存在、旧库还在时 doctor 应报旧数据搬迁，退出码 = %d，输出 %s", code, out)
	}
	for _, want := range []string{"fail", "mv " + oldDB, filepath.Join(filepath.Dir(oldDB), "secret.key"), filepath.Dir(paths.DB)} {
		if !strings.Contains(out, want) {
			t.Fatalf("旧数据搬迁的文案缺 %q: %s", want, out)
		}
	}

	// 照着文案把库搬到新位置之后，这一项就该消失。
	if err := os.Rename(oldDB, paths.DB); err != nil {
		t.Fatalf("搬库失败: %v", err)
	}
	if _, out := doctor(); strings.Contains(out, "旧数据搬迁") {
		t.Fatalf("新库已存在，不该再提示搬迁: %s", out)
	}
}
