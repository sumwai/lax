package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"lax/internal/gateway"
)

// ephemeralTimeout 是一条临时网关请求的上限。超时覆盖连接、等响应头与读响应体全过程：
// 上游卡死时命令不该一直挂着。流式对话与语音一般远小于这个上限。
const ephemeralTimeout = 10 * time.Minute

// ephemeralRequest 描述一次「在本进程起临时网关、从回环地址打一条请求」。
// options 里的 Store 必须是已经打开的库：请求记录由网关写回这份库。
type ephemeralRequest struct {
	options     gateway.Options
	path        string
	body        []byte
	contentType string
	header      http.Header
	timeout     time.Duration
}

// callEphemeralGateway 在 127.0.0.1:0 起一个只服务这一次请求的网关，执行 POST，
// 把响应交给 handle 处理。handle 运行期间网关与数据库都保持可用：请求记录是在
// 响应写完之后才落库的，提前收尾会让这一次的账漏记。
//
// 收尾统一在这里做：关闭响应体、取消 ctx、等 Serve 返回。Serve 的 Shutdown 会等
// 正在处理的请求结束，所以返回之后调用方再关数据库是安全的。
func callEphemeralGateway(req ephemeralRequest, handle func(*http.Response) error) error {
	gs, err := gateway.New(req.options)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("临时网关监听回环地址失败：%v；用 lax doctor 检查本机网络", err)
	}

	timeout := req.timeout
	if timeout <= 0 {
		timeout = ephemeralTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- gs.Serve(ctx, ln) }()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+ln.Addr().String()+req.path, bytes.NewReader(req.body))
	if err != nil {
		cancel()
		<-done
		return err
	}
	if req.contentType != "" {
		httpReq.Header.Set("Content-Type", req.contentType)
	}
	for name, values := range req.header {
		for _, value := range values {
			httpReq.Header.Add(name, value)
		}
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		cancel()
		<-done
		return fmt.Errorf("连本进程的网关失败：%v；网关起不来时先运行 lax doctor 看端口与数据库", err)
	}
	handleErr := handle(resp)
	_ = resp.Body.Close()

	cancel()
	serveErr := <-done
	if handleErr != nil {
		return handleErr
	}
	if serveErr != nil && !errors.Is(serveErr, context.Canceled) {
		return fmt.Errorf("临时网关异常退出：%w", serveErr)
	}
	return nil
}
