package pluginfetch

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseSource(t *testing.T) {
	tests := []struct {
		name string
		spec string
		want source
	}{
		{
			name: "tree 地址",
			spec: "https://github.com/sumwai/lax-plugins/tree/master/plugins/doubao-seed-tts",
			want: source{owner: "sumwai", repo: "lax-plugins", ref: "master", path: "plugins/doubao-seed-tts"},
		},
		{
			name: "仓库根",
			spec: "https://github.com/sumwai/lax-plugins",
			want: source{owner: "sumwai", repo: "lax-plugins", ref: "HEAD"},
		},
		{
			name: "省略协议",
			spec: "github.com/sumwai/lax-plugins/plugins/doubao-seed-tts",
			want: source{owner: "sumwai", repo: "lax-plugins", ref: "HEAD", path: "plugins/doubao-seed-tts"},
		},
		{
			name: "简写带 ref",
			spec: "github:sumwai/lax-plugins/plugins/doubao-seed-tts@v1.2.0",
			want: source{owner: "sumwai", repo: "lax-plugins", ref: "v1.2.0", path: "plugins/doubao-seed-tts"},
		},
		{
			name: "ref 带斜杠",
			spec: "github:sumwai/lax-plugins/plugins/doubao-seed-tts@feature/voice",
			want: source{owner: "sumwai", repo: "lax-plugins", ref: "feature/voice", path: "plugins/doubao-seed-tts"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSource(tt.spec)
			if err != nil {
				t.Fatalf("parseSource：%v", err)
			}
			if got != tt.want {
				t.Fatalf("来源 = %+v，期望 %+v", got, tt.want)
			}
		})
	}
}

func TestParseSourceRejects(t *testing.T) {
	tests := []struct {
		name     string
		spec     string
		contains string
	}{
		{"非 GitHub", "https://example.com/a/b", "只支持 github.com"},
		{"HTTP", "http://github.com/a/b", "不是 HTTPS"},
		{"缺仓库", "https://github.com/a", "缺少仓库名"},
		{"越界路径", "https://github.com/a/b/tree/master/../c", "不合法"},
		{"文件地址", "https://github.com/a/b/blob/master/plugin.json", "文件或归档"},
		{"tree 缺 ref", "https://github.com/a/b/tree", "缺少 ref"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseSource(tt.spec)
			if err == nil || !strings.Contains(err.Error(), tt.contains) {
				t.Fatalf("错误 = %v，期望包含 %q", err, tt.contains)
			}
		})
	}
}

func TestIsRemote(t *testing.T) {
	for _, spec := range []string{
		"https://github.com/sumwai/lax-plugins",
		"github.com/sumwai/lax-plugins",
		"github:sumwai/lax-plugins",
	} {
		if !IsRemote(spec) {
			t.Fatalf("%q 应判成远程来源", spec)
		}
	}
	for _, spec := range []string{"", "./plugin", "plugins/foo", "/tmp/plugin"} {
		if IsRemote(spec) {
			t.Fatalf("%q 不应判成远程来源", spec)
		}
	}
}

func TestFetchExtractsPluginDir(t *testing.T) {
	archive := buildZip(t, []zipEntry{
		{name: "lax-plugins-master/", mode: os.ModeDir | 0o755},
		{name: "lax-plugins-master/plugins/doubao-seed-tts/lax.plugin.json", body: `{"id":"com.lax.doubao-seed"}`},
		{name: "lax-plugins-master/plugins/doubao-seed-tts/index.js", body: "export const ok = true;"},
	})
	var gotOwner, gotRepo, gotRef string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(archive)
	}))
	t.Cleanup(srv.Close)

	fetcher := Fetcher{
		Client: srv.Client(),
		ArchiveURL: func(owner, repo, ref string) string {
			gotOwner, gotRepo, gotRef = owner, repo, ref
			return srv.URL + "/archive.zip"
		},
	}
	pkg, err := fetcher.Fetch(context.Background(), "https://github.com/sumwai/lax-plugins/tree/master/plugins/doubao-seed-tts")
	if err != nil {
		t.Fatalf("Fetch：%v", err)
	}
	t.Cleanup(func() { _ = pkg.Close() })

	if gotOwner != "sumwai" || gotRepo != "lax-plugins" || gotRef != "master" {
		t.Fatalf("归档参数 = %s/%s@%s", gotOwner, gotRepo, gotRef)
	}
	if pkg.Origin != "github:sumwai/lax-plugins/plugins/doubao-seed-tts@master" {
		t.Fatalf("来源 = %q", pkg.Origin)
	}
	manifest, err := os.ReadFile(filepath.Join(pkg.Dir, "lax.plugin.json"))
	if err != nil {
		t.Fatalf("读解出的 manifest：%v", err)
	}
	if string(manifest) != `{"id":"com.lax.doubao-seed"}` {
		t.Fatalf("manifest = %s", manifest)
	}
	tempDir := pkg.tempDir
	if err := pkg.Close(); err != nil {
		t.Fatalf("Close：%v", err)
	}
	if _, err := os.Stat(tempDir); !os.IsNotExist(err) {
		t.Fatalf("Close 之后临时目录还在：%v", err)
	}
}

func TestFetchRejectsUnsafeArchives(t *testing.T) {
	tests := []struct {
		name     string
		entries  []zipEntry
		contains string
	}{
		{
			name: "越界路径",
			entries: []zipEntry{
				{name: "repo/../../evil", body: "x"},
			},
			contains: "不安全路径",
		},
		{
			name: "符号链接",
			entries: []zipEntry{
				{name: "repo/link", mode: os.ModeSymlink | 0o777, body: "../../etc/passwd"},
			},
			contains: "符号链接",
		},
		{
			name: "重复条目",
			entries: []zipEntry{
				{name: "repo/plugins/a/lax.plugin.json", body: "{}"},
				{name: "repo/plugins/a/lax.plugin.json", body: "{}"},
			},
			contains: "重复条目",
		},
		{
			name: "多个顶层目录",
			entries: []zipEntry{
				{name: "one/lax.plugin.json", body: "{}"},
				{name: "two/index.js", body: "x"},
			},
			contains: "多个顶层目录",
		},
		{
			name: "插件路径不存在",
			entries: []zipEntry{
				{name: "repo/plugins/other/lax.plugin.json", body: "{}"},
			},
			contains: "不存在",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			archive := buildZip(t, tt.entries)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write(archive)
			}))
			t.Cleanup(srv.Close)
			fetcher := Fetcher{
				Client:     srv.Client(),
				ArchiveURL: func(_, _, _ string) string { return srv.URL + "/archive.zip" },
			}
			_, err := fetcher.Fetch(context.Background(), "https://github.com/acme/plugins/tree/main/plugins/foo")
			if err == nil || !strings.Contains(err.Error(), tt.contains) {
				t.Fatalf("错误 = %v，期望包含 %q", err, tt.contains)
			}
		})
	}
}

func TestFetchEnforcesLimitsAndStatus(t *testing.T) {
	archive := buildZip(t, []zipEntry{{name: "repo/plugins/foo/file", body: "1234567890"}})

	t.Run("归档上限", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(archive)
		}))
		t.Cleanup(srv.Close)
		fetcher := Fetcher{
			Client:     srv.Client(),
			ArchiveURL: func(_, _, _ string) string { return srv.URL + "/archive.zip" },
			MaxArchive: 8,
		}
		_, err := fetcher.Fetch(context.Background(), "https://github.com/acme/plugins/tree/main/plugins/foo")
		if err == nil || !strings.Contains(err.Error(), "超过 8 字节") {
			t.Fatalf("错误 = %v，期望归档上限提示", err)
		}
	})

	t.Run("解压上限", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(archive)
		}))
		t.Cleanup(srv.Close)
		fetcher := Fetcher{
			Client:       srv.Client(),
			ArchiveURL:   func(_, _, _ string) string { return srv.URL + "/archive.zip" },
			MaxExtracted: 5,
		}
		_, err := fetcher.Fetch(context.Background(), "https://github.com/acme/plugins/tree/main/plugins/foo")
		if err == nil || !strings.Contains(err.Error(), "超过 5 字节") {
			t.Fatalf("错误 = %v，期望解压上限提示", err)
		}
	})

	t.Run("HTTP 404", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		}))
		t.Cleanup(srv.Close)
		fetcher := Fetcher{Client: srv.Client(), ArchiveURL: func(_, _, _ string) string { return srv.URL + "/missing.zip" }}
		_, err := fetcher.Fetch(context.Background(), "https://github.com/acme/plugins/tree/main/plugins/foo")
		if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
			t.Fatalf("错误 = %v，期望 HTTP 404", err)
		}
	})
}

func TestDefaultArchiveURL(t *testing.T) {
	got := defaultArchiveURL("sumwai", "lax-plugins", "feature/voice")
	want := "https://codeload.github.com/sumwai/lax-plugins/zip/feature%2Fvoice"
	if got != want {
		t.Fatalf("归档地址 = %q，期望 %q", got, want)
	}
}

func TestFetchHonorsContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte("late"))
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	fetcher := Fetcher{Client: srv.Client(), ArchiveURL: func(_, _, _ string) string { return srv.URL + "/archive.zip" }}
	_, err := fetcher.Fetch(ctx, "https://github.com/acme/plugins/tree/main/plugins/foo")
	if err == nil || !strings.Contains(err.Error(), "下载插件归档失败") {
		t.Fatalf("错误 = %v", err)
	}
}

type zipEntry struct {
	name string
	body string
	mode os.FileMode
}

func buildZip(t *testing.T, entries []zipEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Store}
		mode := entry.mode
		if mode == 0 {
			mode = 0o644
		}
		header.SetMode(mode)
		file, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatalf("写 zip 条目 %s：%v", entry.name, err)
		}
		if _, err := file.Write([]byte(entry.body)); err != nil {
			t.Fatalf("写 zip 内容 %s：%v", entry.name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("关闭 zip：%v", err)
	}
	return buffer.Bytes()
}
