// Package pluginfetch 把 GitHub 仓库里的一段路径下载成临时插件目录。
//
// 它只负责取得目录：manifest、方言与模型目录的校验仍由 internal/plugin 做，安装与落库
// 仍由 internal/cli 做。归档与解压都有硬上限，符号链接、特殊文件、重复条目与越界路径
// 一律拒绝；调用方用完目录后调 Package.Close 删除临时文件。
package pluginfetch

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	defaultMaxArchive   int64 = 64 << 20
	defaultMaxExtracted int64 = 64 << 20
	defaultMaxEntries         = 4096
)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// Package 是一个已经解开的远程插件目录。Dir 指向仓库里的插件路径；Origin 写进
// plugin_versions.source，格式是 github:<owner>/<repo>/<path>@<ref>。
type Package struct {
	Dir    string
	Origin string

	tempDir string
}

// Close 删除解压用的临时目录。本地来源没有临时目录，Close 是空操作。
func (p *Package) Close() error {
	if p == nil || p.tempDir == "" {
		return nil
	}
	return os.RemoveAll(p.tempDir)
}

// Fetcher 可替换 HTTP 客户端与归档地址，主要给测试用。零值按生产默认值工作。
type Fetcher struct {
	Client       *http.Client
	ArchiveURL   func(owner, repo, ref string) string
	MaxArchive   int64
	MaxExtracted int64
	MaxEntries   int
}

// Fetch 下载并解开一个 GitHub 插件来源。spec 支持：
//   - https://github.com/<owner>/<repo>/tree/<ref>/<path>
//   - github.com/<owner>/<repo>/<path>[@<ref>]
//   - github:<owner>/<repo>/<path>[@<ref>]
//
// ref 省略时用 HEAD，即仓库默认分支。只接受 github.com 的 HTTPS 地址。
func Fetch(ctx context.Context, spec string) (Package, error) {
	return Fetcher{}.Fetch(ctx, spec)
}

// Fetch 与包级 Fetch 相同，只是允许调用方替换 HTTP 客户端与归档地址。
func (f Fetcher) Fetch(ctx context.Context, spec string) (Package, error) {
	src, err := parseSource(spec)
	if err != nil {
		return Package{}, err
	}

	client := f.Client
	if client == nil {
		client = defaultClient()
	}
	archiveURL := f.ArchiveURL
	if archiveURL == nil {
		archiveURL = defaultArchiveURL
	}
	maxArchive := f.MaxArchive
	if maxArchive <= 0 {
		maxArchive = defaultMaxArchive
	}
	maxExtracted := f.MaxExtracted
	if maxExtracted <= 0 {
		maxExtracted = defaultMaxExtracted
	}
	maxEntries := f.MaxEntries
	if maxEntries <= 0 {
		maxEntries = defaultMaxEntries
	}

	archivePath, err := download(ctx, client, archiveURL(src.owner, src.repo, src.ref), maxArchive)
	if err != nil {
		return Package{}, err
	}
	defer os.Remove(archivePath)

	dir, tempDir, err := extract(archivePath, src.path, maxExtracted, maxEntries)
	if err != nil {
		return Package{}, err
	}
	return Package{Dir: dir, Origin: src.origin(), tempDir: tempDir}, nil
}

// IsRemote 判断一个 install/update 来源是不是 GitHub 来源。本地路径一律返回 false。
func IsRemote(spec string) bool {
	raw := strings.TrimSpace(spec)
	if raw == "" {
		return false
	}
	if strings.HasPrefix(raw, "github:") {
		return true
	}
	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "github.com/") || strings.HasPrefix(lower, "www.github.com/") {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && isGitHubHost(u.Hostname())
}

type source struct {
	owner string
	repo  string
	ref   string
	path  string
}

func (s source) origin() string {
	out := "github:" + s.owner + "/" + s.repo
	if s.path != "" {
		out += "/" + s.path
	}
	return out + "@" + s.ref
}

func parseSource(spec string) (source, error) {
	raw := strings.TrimSpace(spec)
	if raw == "" {
		return source{}, fmt.Errorf("插件来源为空：本地目录直接写路径，远程写 https://github.com/<owner>/<repo>/tree/<ref>/<path>")
	}
	if strings.HasPrefix(raw, "github:") {
		return parseShorthand(strings.TrimPrefix(raw, "github:"), raw)
	}

	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "github.com/") || strings.HasPrefix(lower, "www.github.com/") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return source{}, fmt.Errorf("解析插件来源 %q 失败：%v；远程来源写成 https://github.com/<owner>/<repo>/tree/<ref>/<path>", spec, err)
	}
	if u.Scheme != "https" {
		return source{}, fmt.Errorf("插件来源 %q 不是 HTTPS：远程插件只从 GitHub 的 HTTPS 地址下载，避免归档在传输中被替换", spec)
	}
	if !isGitHubHost(u.Hostname()) {
		return source{}, fmt.Errorf("插件来源 %q 的域名是 %s：远程插件只支持 github.com", spec, u.Hostname())
	}
	if u.User != nil {
		return source{}, fmt.Errorf("插件来源 %q 带了用户名或口令：本版只装公开仓库，不支持在地址里带凭据", spec)
	}

	pathText := strings.Trim(u.Path, "/")
	rawParts := strings.Split(pathText, "/")
	isTree := len(rawParts) >= 3 && rawParts[2] == "tree"
	ref := ""
	if !isTree {
		pathText, ref = splitRefText(pathText)
	}
	parts, err := cleanParts(strings.Split(pathText, "/"))
	if err != nil {
		return source{}, fmt.Errorf("插件来源 %q 的路径不合法：%w", spec, err)
	}
	if len(parts) < 2 {
		return source{}, fmt.Errorf("插件来源 %q 缺少仓库名：写成 https://github.com/<owner>/<repo>/tree/<ref>/<path>", spec)
	}
	src := source{owner: parts[0], repo: parts[1]}
	if err := checkName("owner", src.owner); err != nil {
		return source{}, fmt.Errorf("插件来源 %q：%w", spec, err)
	}
	if err := checkName("repo", src.repo); err != nil {
		return source{}, fmt.Errorf("插件来源 %q：%w", spec, err)
	}

	rest := parts[2:]
	if isTree {
		if len(rest) < 2 {
			return source{}, fmt.Errorf("插件来源 %q 的 /tree/ 后面缺少 ref：写成 /tree/<ref>/<path>", spec)
		}
		src.ref = rest[1]
		src.path = strings.Join(rest[2:], "/")
	} else {
		if len(rest) > 0 && (rest[0] == "blob" || rest[0] == "raw" || rest[0] == "archive") {
			return source{}, fmt.Errorf("插件来源 %q 指向文件或归档：要给目录地址，用 /tree/<ref>/<path>，或直接写 github.com/<owner>/<repo>/<path>", spec)
		}
		src.ref = ref
		src.path = strings.Join(rest, "/")
	}
	if src.ref == "" {
		src.ref = "HEAD"
	}
	if err := checkRef(src.ref); err != nil {
		return source{}, fmt.Errorf("插件来源 %q：%w", spec, err)
	}
	return src, nil
}

func parseShorthand(rest, original string) (source, error) {
	rest = strings.Trim(strings.TrimSpace(rest), "/")
	if rest == "" {
		return source{}, fmt.Errorf("插件来源 %q 缺少 owner/repo：写成 github:<owner>/<repo>/<path>[@<ref>]", original)
	}
	rest, ref := splitRefText(rest)
	parts := strings.Split(rest, "/")
	parts, err := cleanParts(parts)
	if err != nil {
		return source{}, fmt.Errorf("插件来源 %q 的路径不合法：%w", original, err)
	}
	if len(parts) < 2 {
		return source{}, fmt.Errorf("插件来源 %q 缺少仓库名：写成 github:<owner>/<repo>/<path>[@<ref>]", original)
	}
	src := source{owner: parts[0], repo: parts[1], ref: ref, path: strings.Join(parts[2:], "/")}
	if err := checkName("owner", src.owner); err != nil {
		return source{}, fmt.Errorf("插件来源 %q：%w", original, err)
	}
	if err := checkName("repo", src.repo); err != nil {
		return source{}, fmt.Errorf("插件来源 %q：%w", original, err)
	}
	if src.ref == "" {
		src.ref = "HEAD"
	}
	if err := checkRef(src.ref); err != nil {
		return source{}, fmt.Errorf("插件来源 %q：%w", original, err)
	}
	return src, nil
}

// splitRefText 取出路径末尾的 @<ref>。GitHub 的 owner、repo 与目录名里不会有 @，
// 所以这个后缀不会与普通路径混淆；ref 里的斜杠因此也能保留。
func splitRefText(value string) (string, string) {
	index := strings.LastIndex(value, "@")
	if index <= 0 {
		return value, ""
	}
	return value[:index], value[index+1:]
}

func cleanParts(parts []string) ([]string, error) {
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		if part == "." || part == ".." {
			return nil, fmt.Errorf("路径里不能有 %q", part)
		}
		if strings.Contains(part, "\\") || strings.ContainsRune(part, 0) || strings.ContainsAny(part, "\r\n") {
			return nil, fmt.Errorf("路径 %q 含反斜杠或控制字符", part)
		}
		out = append(out, part)
	}
	return out, nil
}

func checkName(what, name string) error {
	if name == "" || name == "." || name == ".." || !namePattern.MatchString(name) {
		return fmt.Errorf("%s %q 不合法：只能有字母、数字、点、下划线与连字符", what, name)
	}
	return nil
}

func checkRef(ref string) error {
	if ref == "" || strings.Contains(ref, "..") || strings.Contains(ref, "\\") ||
		strings.ContainsRune(ref, 0) || strings.ContainsAny(ref, "\r\n?#") {
		return fmt.Errorf("ref %q 不合法：只写分支、tag 或提交号，不要带 ? 与 #", ref)
	}
	return nil
}

func isGitHubHost(host string) bool {
	switch strings.ToLower(host) {
	case "github.com", "www.github.com":
		return true
	default:
		return false
	}
}

func defaultArchiveURL(owner, repo, ref string) string {
	return "https://codeload.github.com/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/zip/" + url.PathEscape(ref)
}

func defaultClient() *http.Client {
	return &http.Client{
		Timeout: 2 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("重定向次数过多")
			}
			if req.URL.Scheme != "https" {
				return fmt.Errorf("归档地址被重定向到 %s：只允许 HTTPS", req.URL.Scheme)
			}
			host := strings.ToLower(req.URL.Hostname())
			if host != "github.com" && host != "codeload.github.com" {
				return fmt.Errorf("归档地址被重定向到 %s：只允许 github.com 与 codeload.github.com", req.URL.Host)
			}
			return nil
		},
	}
}

func download(ctx context.Context, client *http.Client, archiveURL string, max int64) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, archiveURL, nil)
	if err != nil {
		return "", fmt.Errorf("构造插件归档请求失败：%v", err)
	}
	req.Header.Set("Accept", "application/zip")
	req.Header.Set("User-Agent", "lax-plugin-install")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("下载插件归档失败：%v；检查网络与代理，或改用本地目录安装", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载插件归档返回 HTTP %d：仓库、ref 或插件路径可能不存在，确认地址后重试（私有仓库本版不支持）", resp.StatusCode)
	}

	file, err := os.CreateTemp("", "lax-plugin-*.zip")
	if err != nil {
		return "", fmt.Errorf("创建插件归档临时文件失败：%v；检查临时目录的权限与剩余空间", err)
	}
	name := file.Name()
	ok := false
	defer func() {
		if !ok {
			file.Close()
			os.Remove(name)
		}
	}()

	written, err := io.Copy(file, io.LimitReader(resp.Body, max+1))
	if err != nil {
		return "", fmt.Errorf("写插件归档临时文件失败：%v；检查临时目录的剩余空间", err)
	}
	if written > max {
		return "", fmt.Errorf("插件归档超过 %s：这个包太大，先精简再发布，或把仓库克隆下来用本地目录安装", limitText(max))
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("写完插件归档临时文件失败：%v", err)
	}
	ok = true
	return name, nil
}

func extract(archivePath, subdir string, maxExtracted int64, maxEntries int) (string, string, error) {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return "", "", fmt.Errorf("插件归档不是合法的 zip：%v；GitHub 仓库地址可能指向了非归档内容", err)
	}
	defer reader.Close()
	if len(reader.File) > maxEntries {
		return "", "", fmt.Errorf("插件归档有 %d 个条目，超过上限 %d：这个包太大，先精简再发布", len(reader.File), maxEntries)
	}

	tempDir, err := os.MkdirTemp("", "lax-plugin-*")
	if err != nil {
		return "", "", fmt.Errorf("创建插件解压目录失败：%v；检查临时目录的权限与剩余空间", err)
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(tempDir)
		}
	}()

	root := ""
	seen := map[string]bool{}
	var total int64
	for _, entry := range reader.File {
		name, err := archiveName(entry.Name)
		if err != nil {
			return "", "", fmt.Errorf("插件归档里有不安全路径 %q：%w；不要用带 ../ 或绝对路径的归档", entry.Name, err)
		}
		parts := strings.Split(name, "/")
		if len(parts) == 0 {
			continue
		}
		if root == "" {
			root = parts[0]
		} else if parts[0] != root {
			return "", "", fmt.Errorf("插件归档有多个顶层目录 %q 与 %q：只接受 GitHub 单仓库归档", root, parts[0])
		}
		rel := strings.Join(parts[1:], "/")
		if rel == "" {
			continue
		}
		if seen[rel] {
			return "", "", fmt.Errorf("插件归档里有重复条目 %q：解压结果不确定，拒绝安装", rel)
		}
		seen[rel] = true

		target := filepath.Join(tempDir, filepath.FromSlash(rel))
		if !within(tempDir, target) {
			return "", "", fmt.Errorf("插件归档条目 %q 会写到解压目录之外", entry.Name)
		}
		isDir := strings.HasSuffix(entry.Name, "/") || entry.FileInfo().IsDir()
		if isDir {
			if err := os.MkdirAll(target, 0o700); err != nil {
				return "", "", fmt.Errorf("创建插件目录 %s 失败：%v", target, err)
			}
			continue
		}
		if entry.Mode()&os.ModeSymlink != 0 {
			return "", "", fmt.Errorf("插件归档里有符号链接 %q：链接能指到包外，拒绝安装", entry.Name)
		}
		if !entry.Mode().IsRegular() {
			return "", "", fmt.Errorf("插件归档里有特殊文件 %q：只接受普通文件与目录", entry.Name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return "", "", fmt.Errorf("创建插件目录 %s 失败：%v", filepath.Dir(target), err)
		}
		src, err := entry.Open()
		if err != nil {
			return "", "", fmt.Errorf("读取归档条目 %q 失败：%v", entry.Name, err)
		}
		remaining := maxExtracted - total
		if remaining < 0 {
			src.Close()
			return "", "", fmt.Errorf("插件解压后超过 %s：这个包太大，先精简再发布", limitText(maxExtracted))
		}
		dst, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			src.Close()
			return "", "", fmt.Errorf("创建插件文件 %s 失败：%v", target, err)
		}
		n, err := io.Copy(dst, io.LimitReader(src, remaining+1))
		src.Close()
		closeErr := dst.Close()
		if err != nil {
			return "", "", fmt.Errorf("解压归档条目 %q 失败：%v", entry.Name, err)
		}
		if closeErr != nil {
			return "", "", fmt.Errorf("写完插件文件 %s 失败：%v", target, closeErr)
		}
		if n > remaining {
			return "", "", fmt.Errorf("插件解压后超过 %s：这个包太大，先精简再发布", limitText(maxExtracted))
		}
		total += n
	}
	if root == "" {
		return "", "", fmt.Errorf("插件归档是空的：仓库里没有可安装的内容")
	}

	// 解压时已经去掉了归档的顶层目录，插件路径因此直接从 tempDir 算起。
	target := tempDir
	if subdir != "" {
		target = filepath.Join(target, filepath.FromSlash(subdir))
	}
	if !within(tempDir, target) {
		return "", "", fmt.Errorf("插件路径 %q 指到了归档目录之外", subdir)
	}
	info, err := os.Stat(target)
	if err != nil {
		return "", "", fmt.Errorf("插件路径 %q 在归档里不存在：确认仓库、ref 与目录名，tree 地址要从仓库根写起", subdir)
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("插件路径 %q 在归档里不是目录：安装来源必须是插件目录，不是单个文件", subdir)
	}
	ok = true
	return target, tempDir, nil
}

func archiveName(name string) (string, error) {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") {
		return "", fmt.Errorf("必须是相对路径")
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", fmt.Errorf("不能含 .. 或写到根目录之外")
		}
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("不能含 .. 或写到根目录之外")
	}
	for _, part := range strings.Split(clean, "/") {
		if part == "" || part == "." || part == ".." || strings.ContainsRune(part, 0) {
			return "", fmt.Errorf("路径段不合法")
		}
	}
	return clean, nil
}

func within(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

func limitText(limit int64) string {
	if limit%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", limit>>20)
	}
	return fmt.Sprintf("%d 字节", limit)
}
