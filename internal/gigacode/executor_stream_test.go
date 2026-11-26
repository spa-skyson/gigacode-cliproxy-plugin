package gigacode

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spa-skyson/gigacode-cliproxy-plugin/internal/gigacodemock"
)

// streamRequestJSON — запрос executor.execute_stream.
func streamRequestJSON(storage []byte, payload []byte, streamID string) []byte {
	return executorRequestJSON(storage, payload, map[string]any{
		"Stream":    true,
		"stream_id": streamID,
	})
}

// Тест 11а: стриминг — кадры по порядку и по мере поступления,
// в конце close без ошибки, HTTP-стрим закрыт ровно один раз.
func TestExecutorStreamSuccess(t *testing.T) {
	mock := gigacodemock.New()
	defer mock.Close()
	mock.SetSSE(true)
	mock.SetSSEDelay(60 * time.Millisecond)

	sink := newFakeSink()
	core := newTestCore(testConfig(mock), nil, sink)

	raw := streamRequestJSON(storageFor(testDeviceCode), chatPayload(), "stream-1")
	resp := decodeResult[ExecutorStreamResponse](t, core.Handle("executor.execute_stream", raw))

	if len(resp.Chunks) != 0 {
		t.Fatalf("ожидался асинхронный режим без chunks: %d", len(resp.Chunks))
	}

	closeMsg := sink.waitClose(t, 10*time.Second)
	if closeMsg != "" {
		t.Fatalf("close с ошибкой: %q", closeMsg)
	}
	// Клиенту передаётся только Content-Type апстрима: X-Upstream-Extra,
	// Content-Length и прочие чужие заголовки не проходят.
	if len(resp.Headers) != 1 {
		t.Fatalf("заголовков стрима: %d (%v), ожидался только Content-Type", len(resp.Headers), resp.Headers)
	}
	if len(headerValueAll(resp.Headers, "Content-Type")) == 0 {
		t.Fatalf("Content-Type стрима не передан: %v", resp.Headers)
	}
	if headerValue(resp.Headers, "X-Upstream-Extra") != "" {
		t.Fatalf("чужой заголовок апстрима дошёл до клиента: %v", resp.Headers)
	}
	frames, seenAt, closeCnt, _ := sink.snapshot()
	if len(frames) < 2 {
		t.Fatalf("кадров: %d, ожидалось несколько", len(frames))
	}
	joined := strings.Join(frames, "")
	if !strings.Contains(joined, "первый") || !strings.Contains(joined, "второй") || !strings.Contains(joined, "[DONE]") {
		t.Fatalf("кадры неполные: %q", joined)
	}
	if strings.Index(joined, "первый") > strings.Index(joined, "второй") {
		t.Fatalf("порядок кадров нарушен: %q", joined)
	}
	// «По мере поступления»: между кадрами была пауза (задержка мока).
	for i := 1; i < len(seenAt); i++ {
		gap := time.Duration(seenAt[i]-seenAt[i-1]) * time.Nanosecond
		if gap < 30*time.Millisecond {
			t.Fatalf("кадры пришли разом (пауза %v у кадров %d/%d)", gap, i-1, i)
		}
	}
	if closeCnt != 1 {
		// sink.Close вызывается ровно один раз — из pumpStream.
		t.Fatalf("sink.Close вызван %d раз, ожидался 1", closeCnt)
	}
}

// Тест 11б: HTTP-стрим апстрима закрыт ровно один раз.
func TestExecutorStreamUpstreamClosedOnce(t *testing.T) {
	mock := gigacodemock.New()
	defer mock.Close()
	mock.SetSSE(true)
	mock.SetSSEDelay(10 * time.Millisecond)

	sink := newFakeSink()
	core := newTestCore(testConfig(mock), nil, sink)

	// Оборачиваем транспорт: считаем закрытия стрима апстрима.
	counting := &closeCountingTransport{inner: newHTTPTestTransport()}
	core.newTransport = func(string) Transport { return counting }

	raw := streamRequestJSON(storageFor(testDeviceCode), chatPayload(), "stream-2")
	decodeResult[ExecutorStreamResponse](t, core.Handle("executor.execute_stream", raw))
	sink.waitClose(t, 10*time.Second)

	if got := counting.streamCloses(); got != 1 {
		t.Fatalf("HTTP-стрим закрыт %d раз, ожидался 1", got)
	}
}

// Тест 11в: ошибка emit останавливает чтение.
func TestExecutorStreamEmitErrorStopsReading(t *testing.T) {
	mock := gigacodemock.New()
	defer mock.Close()
	mock.SetSSE(true)
	mock.SetSSEDelay(30 * time.Millisecond)

	sink := newFakeSink()
	sink.emitAfter = 1 // после первого кадра Emit вернёт ошибку
	core := newTestCore(testConfig(mock), nil, sink)

	counting := &closeCountingTransport{inner: newHTTPTestTransport()}
	core.newTransport = func(string) Transport { return counting }

	raw := streamRequestJSON(storageFor(testDeviceCode), chatPayload(), "stream-3")
	decodeResult[ExecutorStreamResponse](t, core.Handle("executor.execute_stream", raw))

	// Ждём больше времени двух кадров: новых кадров быть не должно.
	time.Sleep(300 * time.Millisecond)
	frames, _, _, _ := sink.snapshot()
	if len(frames) != 1 {
		t.Fatalf("кадров после ошибки emit: %d, ожидался 1", len(frames))
	}
	if got := counting.streamCloses(); got != 1 {
		t.Fatalf("HTTP-стрим закрыт %d раз, ожидался 1", got)
	}
}

// Тест 11г: ошибка чтения → close с ошибкой.
func TestExecutorStreamReadErrorClosesWithError(t *testing.T) {
	mock := gigacodemock.New()
	defer mock.Close()
	mock.SetSSE(true)
	mock.SetSSEDelay(10 * time.Millisecond)

	sink := newFakeSink()
	core := newTestCore(testConfig(mock), nil, sink)

	// Инжектируем обрыв после первого чтения.
	inner := newHTTPTestTransport()
	core.newTransport = func(string) Transport {
		return &injectFailTransport{inner: inner, failAfter: 1}
	}

	raw := streamRequestJSON(storageFor(testDeviceCode), chatPayload(), "stream-4")
	decodeResult[ExecutorStreamResponse](t, core.Handle("executor.execute_stream", raw))

	closeMsg := sink.waitClose(t, 10*time.Second)
	if closeMsg == "" {
		t.Fatal("ожидался close с текстом ошибки")
	}
	// Клиенту уходит фиксированный текст, детали — в журнал.
	if closeMsg != msgStreamBroken {
		t.Fatalf("текст ошибки: %q, ожидался %q", closeMsg, msgStreamBroken)
	}
}

// closeCountingTransport — считает закрытия стрима апстрима.
type closeCountingTransport struct {
	inner   Transport
	mu      sync.Mutex
	streams []*countingStream
}

func (c *closeCountingTransport) Do(ctx context.Context, req Request, maxBody int64) (*Response, error) {
	return c.inner.Do(ctx, req, maxBody)
}

func (c *closeCountingTransport) OpenStream(ctx context.Context, req Request) (*StreamResponse, error) {
	resp, err := c.inner.OpenStream(ctx, req)
	if err != nil {
		return nil, err
	}
	counting := &countingStream{inner: resp.Stream}
	c.mu.Lock()
	c.streams = append(c.streams, counting)
	c.mu.Unlock()
	resp.Stream = counting
	return resp, nil
}

func (c *closeCountingTransport) streamCloses() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for _, s := range c.streams {
		total += s.closeCount()
	}
	return total
}

// injectFailTransport — подменяет стрим апстрима на «падающий».
type injectFailTransport struct {
	inner     Transport
	failAfter int
}

func (f *injectFailTransport) Do(ctx context.Context, req Request, maxBody int64) (*Response, error) {
	return f.inner.Do(ctx, req, maxBody)
}

func (f *injectFailTransport) OpenStream(ctx context.Context, req Request) (*StreamResponse, error) {
	resp, err := f.inner.OpenStream(ctx, req)
	if err != nil {
		return nil, err
	}
	resp.Stream = &failingStream{inner: resp.Stream, failAfter: f.failAfter}
	return resp, nil
}
