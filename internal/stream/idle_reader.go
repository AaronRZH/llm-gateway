package stream

import (
	"io"
	"sync"
	"time"
)

// idleTimeoutReader 包装一个 io.ReadCloser，在两次成功读取之间超过 idle 时间未收到任何数据时，
// 主动关闭底层 reader 以中断阻塞中的 Read，从而防止上游静默 stall 导致流永久卡住（"中途卡住"）。
//
// 原理：http.Response.Body 的 Read 在连接半开、上游不再发数据也不关闭时会永久阻塞。
// 标准做法是并发调用 Body.Close() 打断正在进行的 Read（http 包保证 Close 与 Read 并发安全）。
// 这里用定时器在空闲超时时触发 Close，使 scanner 以错误结束，进而由上层优雅终止流。
type idleTimeoutReader struct {
	mu      sync.Mutex
	r       io.ReadCloser
	timeout time.Duration
	timer   *time.Timer
	closed  bool
}

// NewIdleTimeoutReader 创建带空闲超时的 reader；timeout<=0 时原样返回（不启用超时）
func NewIdleTimeoutReader(r io.ReadCloser, timeout time.Duration) io.ReadCloser {
	if timeout <= 0 {
		return r
	}
	it := &idleTimeoutReader{r: r, timeout: timeout}
	it.timer = time.AfterFunc(timeout, it.fire)
	return it
}

// fire 定时器触发：关闭底层 reader 以中断阻塞中的 Read
func (it *idleTimeoutReader) fire() {
	it.mu.Lock()
	defer it.mu.Unlock()
	if it.closed {
		return
	}
	// 并发关闭底层 reader 是安全的，会使阻塞的 Read 返回错误
	_ = it.r.Close()
}

// reset 每次成功读取后重置空闲定时器
func (it *idleTimeoutReader) reset() {
	it.mu.Lock()
	defer it.mu.Unlock()
	if it.closed {
		return
	}
	if it.timer != nil {
		it.timer.Reset(it.timeout)
	}
}

// Read 读取数据，成功后重置空闲定时器
func (it *idleTimeoutReader) Read(p []byte) (int, error) {
	n, err := it.r.Read(p)
	if n > 0 {
		it.reset()
	}
	return n, err
}

// Close 停止定时器并关闭底层 reader（幂等）
func (it *idleTimeoutReader) Close() error {
	it.mu.Lock()
	it.closed = true
	if it.timer != nil {
		it.timer.Stop()
	}
	it.mu.Unlock()
	return it.r.Close()
}

// errWriter 包装 io.Writer，记录首次写错误。
// 用于在转换循环中检测客户端断开（写失败）后及时退出，避免无谓地继续读取上游。
type errWriter struct {
	w   io.Writer
	err error
}

// Write 写入数据；一旦出错则记录并后续写入直接返回该错误
func (e *errWriter) Write(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	n, err := e.w.Write(p)
	if err != nil {
		e.err = err
	}
	return n, err
}
