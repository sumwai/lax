package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 语音两条命令的命令面：参数校验与「--json 与二进制不能混一路」。
// 转发与计费由 internal/gateway 的 TestForwardSpeech* 与 TestTranscriptions* 覆盖，
// 这里只钉住 cli 自己判的那几件事。

func TestSpeakUsageErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"缺模型", []string{"--text", "你好"}, "--model"},
		{"缺文本", []string{"--model", "tts-1"}, "--text"},
		{"格式不认识", []string{"--model", "tts-1", "--text", "hi", "--format", "ogg"}, "--format"},
		{"json 与标准输出冲突", []string{"--model", "tts-1", "--text", "hi", "--json"}, "--out"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, stdout, stderr := run(t, append([]string{"speak"}, c.args...)...)
			if code != 2 {
				t.Fatalf("退出码 = %d，期望 2（用法错误）；stderr = %s", code, stderr)
			}
			if stdout != "" {
				t.Fatalf("用法错误不该写 stdout: %s", stdout)
			}
			if !strings.Contains(stderr, c.want) {
				t.Fatalf("stderr 应提到 %q: %s", c.want, stderr)
			}
		})
	}
}

func TestTranscribeUsageErrors(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.wav")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatalf("建空文件: %v", err)
	}
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"缺文件参数", []string{"--model", "whisper-1"}, "音频文件"},
		{"缺模型", []string{"a.wav"}, "--model"},
		{"音频为空", []string{empty, "--model", "whisper-1"}, "空的"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, _, stderr := run(t, append([]string{"transcribe"}, c.args...)...)
			if code != 2 && code != 1 {
				t.Fatalf("退出码 = %d，期望 2（用法错误）或 1（读文件失败）；stderr = %s", code, stderr)
			}
			if !strings.Contains(stderr, c.want) {
				t.Fatalf("stderr 应提到 %q: %s", c.want, stderr)
			}
		})
	}
}
