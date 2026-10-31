package cli_test

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
			code, stdout, stderr := run(t, append([]string{"model", "speak"}, c.args...)...)
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
			code, _, stderr := run(t, append([]string{"model", "transcribe"}, c.args...)...)
			if code != 2 && code != 1 {
				t.Fatalf("退出码 = %d，期望 2（用法错误）或 1（读文件失败）；stderr = %s", code, stderr)
			}
			if !strings.Contains(stderr, c.want) {
				t.Fatalf("stderr 应提到 %q: %s", c.want, stderr)
			}
		})
	}
}

// TestSpeakJSONWritesOut 钉住 --json 与 --out 同时用时音频仍然落盘：
// JSON 只是元信息，不能反过来把音频写文件这一步跳过。
func TestSpeakJSONWritesOut(t *testing.T) {
	sandbox(t)
	audio := []byte("RIFF-fake-wav")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"audio":{"data":"`+
			base64.StdEncoding.EncodeToString(audio)+`"}}}]}`)
	}))
	t.Cleanup(up.Close)

	if code, _, stderr := run(t, "init"); code != 0 {
		t.Fatalf("init 失败: %s", stderr)
	}
	if code, _, stderr := addAccount(t, "mimo1", "mimo-cli", up.URL, "", "--key", "sk-test", "--models", "mimo-v2.5-tts"); code != 0 {
		t.Fatalf("account add 失败: %s", stderr)
	}
	if code, _, stderr := run(t, "model", "capability", "set", "mimo-v2.5-tts", "--provider", "mimo-cli",
		"--capability", "speech", "--voices", "Mia", "--audio-formats", "mp3,wav",
		"--dialect", "lax/mimo-chat-speech@1"); code != 0 {
		t.Fatalf("capability set 失败: %s", stderr)
	}

	out := filepath.Join(t.TempDir(), "out.wav")
	code, stdout, stderr := run(t, "model", "speak", "--model", "mimo-v2.5-tts", "--text", "hi", "--voice", "Mia",
		"--format", "wav", "--out", out, "--json")
	if code != 0 {
		t.Fatalf("speak 退出码 = %d，stderr = %s", code, stderr)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("--json 时音频文件没写出来: %v", err)
	}
	if string(got) != string(audio) {
		t.Fatalf("音频 = %q，期望 %q", got, audio)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(stdout), &meta); err != nil {
		t.Fatalf("--json 输出不是 JSON: %v，%s", err, stdout)
	}
	if meta["out"] != out {
		t.Fatalf("--json 的 out = %v，期望 %s", meta["out"], out)
	}
}
