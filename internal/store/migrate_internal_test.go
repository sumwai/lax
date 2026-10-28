package store

import (
	"strings"
	"testing"
)

// TestMigrationVersionParsedFromFileName 钉住版本号来自文件名前缀，而不是它在下标里的位置。
// 早先按下标当版本号，改个名字或删掉一个迁移文件就会让后面的版本整体错位：
// 老库升级时那些迁移一条都不会执行，user_version 却已经推到「最新」。
func TestMigrationVersionParsedFromFileName(t *testing.T) {
	cases := []struct {
		name string
		want int
	}{
		{"migrations/0001_init.sql", 1},
		{"migrations/0008_provider_models_path.sql", 8},
		{"migrations/0010_later.sql", 10},
	}
	for _, c := range cases {
		got, err := migrationVersion(c.name)
		if err != nil {
			t.Fatalf("解析 %q: %v", c.name, err)
		}
		if got != c.want {
			t.Fatalf("migrationVersion(%q) = %d，期望 %d", c.name, got, c.want)
		}
	}

	for _, name := range []string{"migrations/init.sql", "migrations/0000_zero.sql", "migrations/abc_x.sql"} {
		if v, err := migrationVersion(name); err == nil {
			t.Fatalf("文件名 %q 应该报错，实际解析成 %d", name, v)
		} else if !strings.Contains(err.Error(), "迁移文件") {
			t.Fatalf("错误文案应说明迁移文件名不对: %v", err)
		}
	}
}
