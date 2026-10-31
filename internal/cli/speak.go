package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"lax/internal/gateway"
)

// 语音两条命令：speak 发一条 TTS 请求，transcribe 发一条 ASR 请求。入站协议固定是
// openai-speech 与 openai-transcriptions，两者都只做同协议直通（见 DESIGN 5.16）。
//
// 两个命令都走与 call/decide 同一个办法：在本进程起一个临时网关（127.0.0.1:0），
// 从本机打一条真实请求，这样选号、冷却、别名解析、请求记录都走同一条路径，
// cli 不自己拼上游地址、不碰凭据。

const (
	speakUsage = "用法：lax speak --model <模型> --text <文本> [--voice <音色>] [--format mp3|opus|aac|flac|wav|pcm] [--out <文件>] [--json]\n" +
		"  音频默认写到标准输出（可重定向进播放器）；给了 --out 就写文件\n" +
		"  --json 只输出元信息，音频必须配 --out（JSON 与二进制不混在一起）"
	transcribeUsage = "用法：lax transcribe <音频文件或 -> --model <模型> [--language <代码>] [--prompt <文本>] [--format json|text|verbose_json|srt|vtt]\n" +
		"  文件参数给 - 时从标准输入读；响应体原样写到标准输出"
)

// speakFormats 是 OpenAI 的 six 种 response_format，也是 --format 的全部取值。
var speakFormats = map[string]bool{
	"mp3": true, "opus": true, "aac": true, "flac": true, "wav": true, "pcm": true,
}

func cmdSpeak(e *Env, args []string) error {
	fs := newFlagSet(e, "speak", "发一条语音合成请求（POST /v1/audio/speech），音频写标准输出或文件")
	model := fs.String("model", "", "模型名：有效出站协议必须是 openai-speech")
	text := fs.String("text", "", "要合成的文本")
	voice := fs.String("voice", "alloy", "音色：交给上游判，不认的音色由上游报 400")
	format := fs.String("format", "mp3", "音频格式：mp3 | opus | aac | flac | wav | pcm")
	out := fs.String("out", "", "音频写到这个文件；不给就写标准输出")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usagef("多余的参数 %q：文本走 --text，音频去处走 --out\n%s", fs.Arg(0), speakUsage)
	}
	if strings.TrimSpace(*model) == "" {
		return usagef("缺少 --model：语音合成按模型名选号，用 lax model ls 看哪些模型可用\n%s", speakUsage)
	}
	if *text == "" {
		return usagef("缺少 --text：要合成的文本就是它，内容为空时上游只会回 400\n%s", speakUsage)
	}
	if !speakFormats[*format] {
		return usagef("--format %q 不认识：可用 mp3、opus、aac、flac、wav、pcm\n%s", *format, speakUsage)
	}
	if e.JSON && strings.TrimSpace(*out) == "" {
		return usagef("--json 与「音频写标准输出」不能同时用：JSON 与二进制混在一路会读不出音频，" +
			"加 --out <文件> 把音频落到文件里\n" + speakUsage)
	}

	body, err := json.Marshal(map[string]any{
		"model":           *model,
		"input":           *text,
		"voice":           *voice,
		"response_format": *format,
	})
	if err != nil {
		return err
	}

	resp, err := postVoice(e, gateway.SpeechPath(), body, "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return voiceHTTPError(resp)
	}

	audio, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("读网关响应失败：%v；用 lax log tail 看这一次的记录", err)
	}

	if e.JSON {
		return e.printJSON(map[string]any{
			"model":        *model,
			"voice":        *voice,
			"format":       *format,
			"out":          *out,
			"bytes":        len(audio),
			"content_type": resp.Header.Get("Content-Type"),
			"chars":        len([]rune(*text)),
		})
	}

	if *out != "" {
		if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil && filepath.Dir(*out) != "." {
			return fmt.Errorf("创建目录 %s 失败：%v", filepath.Dir(*out), err)
		}
		if err := os.WriteFile(*out, audio, 0o644); err != nil {
			return fmt.Errorf("写音频文件 %s 失败：%v", *out, err)
		}
		fmt.Fprintf(e.Stderr, "已写入 %s（模型 %s，%d 字节，%s）\n",
			*out, *model, len(audio), dashIfEmpty(resp.Header.Get("Content-Type")))
		return nil
	}

	if _, err := e.Stdout.Write(audio); err != nil {
		return fmt.Errorf("写音频到标准输出失败：%v", err)
	}
	return nil
}

func cmdTranscribe(e *Env, args []string) error {
	fs := newFlagSet(e, "transcribe", "发一条语音转写请求（POST /v1/audio/transcriptions），响应体原样输出")
	model := fs.String("model", "", "模型名：有效出站协议必须是 openai-transcriptions")
	language := fs.String("language", "", "音频语言的 ISO-639-1 代码，例如 zh、en；不给就交给上游自动判断")
	prompt := fs.String("prompt", "", "提示词：给上游一点上下文（专有名词、标点风格）")
	format := fs.String("format", "json", "响应格式：json | text | verbose_json | srt | vtt")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("需要且只给一个音频文件参数（给 - 表示从标准输入读）\n%s", transcribeUsage)
	}
	if strings.TrimSpace(*model) == "" {
		return usagef("缺少 --model：语音转写按模型名选号，用 lax model ls 看哪些模型可用\n%s", transcribeUsage)
	}

	source := fs.Arg(0)
	var audio []byte
	fileName := filepath.Base(source)
	if source == "-" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("读标准输入失败：%v", err)
		}
		audio, fileName = data, "stdin"
	} else {
		data, err := os.ReadFile(source)
		if err != nil {
			return fmt.Errorf("读音频文件 %s 失败：%v", source, err)
		}
		audio = data
	}
	if len(audio) == 0 {
		return fmt.Errorf("音频是空的：%s 一个字节都没有，上游只会回 400", source)
	}

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	if err := writer.WriteField("model", *model); err != nil {
		return err
	}
	if err := writer.WriteField("response_format", *format); err != nil {
		return err
	}
	if *language != "" {
		if err := writer.WriteField("language", *language); err != nil {
			return err
		}
	}
	if *prompt != "" {
		if err := writer.WriteField("prompt", *prompt); err != nil {
			return err
		}
	}
	part, err := writer.CreateFormFile("file", fileName)
	if err != nil {
		return err
	}
	if _, err := part.Write(audio); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}

	resp, err := postVoice(e, gateway.TranscriptionsPath(), buf.Bytes(), writer.FormDataContentType())
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return voiceHTTPError(resp)
	}

	// 响应体原样写到标准输出：json 是 JSON、text 是纯文本、srt 与 vtt 是字幕。
	// 不再包一层 JSON（那会把字幕弄成转义过的字符串），也不重新格式化上游的 JSON。
	if _, err := io.Copy(e.Stdout, resp.Body); err != nil {
		return fmt.Errorf("写转写结果失败：%v", err)
	}
	return nil
}

// postVoice 在本进程起一个临时网关并打一条语音请求，返回上游响应。
// 网关日志关掉：每个命令自己已经把这次请求的事实印出来了，失败时靠 lax log tail 查。
func postVoice(e *Env, path string, body []byte, contentType string) (*http.Response, error) {
	a, err := openApp(e, true)
	if err != nil {
		return nil, err
	}
	defer a.Close()

	gs, err := gateway.New(gateway.Options{Store: a.st, Key: a.key, Logf: func(string, ...any) {}})
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = gs.Serve(ctx, ln) }()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+ln.Addr().String()+path, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("连本进程的网关失败：%v；网关起不来时先运行 lax doctor 看端口与数据库", err)
	}
	// 临时网关的生命周期跟响应走：读完响应体之后才能关，否则会提前切断流。
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// cancelOnClose 在响应体关闭时取消临时网关的 context。
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// voiceHTTPError 把非 2xx 的语音响应变成一条错误（退出码 1），文案取 openai 的
// `{"error":{"message":…}}`；形状不认识时回退成状态码加正文片段。
func voiceHTTPError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<10))
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && env.Error.Message != "" {
		return fmt.Errorf("上游返回 %d：%s；用 lax log tail 看这一次的记录，或 lax model show 核对模型的协议", resp.StatusCode, env.Error.Message)
	}
	return fmt.Errorf("上游返回 %d：%s；用 lax log tail 看这一次的记录", resp.StatusCode, strings.TrimSpace(truncate(string(raw), 300)))
}
