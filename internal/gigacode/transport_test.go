package gigacode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spa-skyson/gigacode-cliproxy-plugin/internal/gigacodemock"
)

// Тестовый транспорт на net/http. Боевая реализация (hosttransport.go)
// живёт поверх колбэков хоста; реализация на net/http — только здесь.

// httpTestTransport — Transport поверх net/http.Client.
type httpTestTransport struct {
	client *http.Client
}

func newHTTPTestTransport() *httpTestTransport {
	return &httpTestTransport{client: &http.Client{}}
}

func (t *httpTestTransport) Do(ctx context.Context, req Request, maxBody int64) (*Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, req.URL, bytes.NewReader(req.Body))
	if err != nil {
		return nil, &transportError{cause: err}
	}
	for key, values := range req.Headers {
		for _, v := range values {
			httpReq.Header.Add(key, v)
		}
	}
	resp, err := t.client.Do(httpReq)
	if err != nil {
		return nil, &transportError{cause: err}
	}
	// Лимит maxBody+1: прочли больше maxBody — превышение.
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	closeErr := resp.Body.Close()
	if errRead != nil {
		return nil, &transportError{cause: errRead}
	}
	if closeErr != nil {
		return nil, &transportError{cause: closeErr}
	}
	if int64(len(body)) > maxBody {
		return nil, errResponseTooLarge
	}
	headers := make(map[string][]string, len(resp.Header))
	for key, values := range resp.Header {
		headers[key] = append([]string(nil), values...)
	}
	return &Response{StatusCode: resp.StatusCode, Headers: headers, Body: body}, nil
}

func (t *httpTestTransport) OpenStream(ctx context.Context, req Request) (*StreamResponse, error) {
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, req.URL, bytes.NewReader(req.Body))
	if err != nil {
		return nil, &transportError{cause: err}
	}
	for key, values := range req.Headers {
		for _, v := range values {
			httpReq.Header.Add(key, v)
		}
	}
	resp, err := t.client.Do(httpReq)
	if err != nil {
		return nil, &transportError{cause: err}
	}
	headers := make(map[string][]string, len(resp.Header))
	for key, values := range resp.Header {
		headers[key] = append([]string(nil), values...)
	}
	return &StreamResponse{
		StatusCode: resp.StatusCode,
		Headers:    headers,
		Stream:     &httpTestStream{body: resp.Body},
	}, nil
}

// httpTestStream — Stream поверх тела http.Response.
type httpTestStream struct {
	body io.ReadCloser
	once sync.Once
	err  error
}

func (s *httpTestStream) Read(ctx context.Context) ([]byte, bool, error) {
	buf := make([]byte, 8192)
	n, err := s.body.Read(buf)
	if n > 0 {
		return buf[:n], false, nil
	}
	if errors.Is(err, io.EOF) {
		return nil, true, nil
	}
	if err != nil {
		return nil, true, err
	}
	return nil, false, nil
}

func (s *httpTestStream) Close() error {
	s.once.Do(func() { s.err = s.body.Close() })
	return s.err
}

// countingStream — декоратор, считающий закрытия (тест «закрыт ровно один раз»).
type countingStream struct {
	inner  Stream
	mu     sync.Mutex
	closes int
}

func (c *countingStream) Read(ctx context.Context) ([]byte, bool, error) {
	return c.inner.Read(ctx)
}

func (c *countingStream) Close() error {
	c.mu.Lock()
	c.closes++
	c.mu.Unlock()
	return c.inner.Close()
}

func (c *countingStream) closeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closes
}

// failingStream — Stream, который после N успешных чтений даёт ошибку.
type failingStream struct {
	inner     Stream
	failAfter int
	reads     int
}

func (f *failingStream) Read(ctx context.Context) ([]byte, bool, error) {
	if f.reads >= f.failAfter {
		return nil, true, fmt.Errorf("обрыв соединения с апстримом (инъекция теста)")
	}
	f.reads++
	return f.inner.Read(ctx)
}

func (f *failingStream) Close() error { return f.inner.Close() }

// fakeSink — получатель кадров стрима: пишет кадры и время их прихода.
type fakeSink struct {
	mu        sync.Mutex
	frames    []string
	seenAt    []int64
	emitAfter int // после стольких кадров Emit возвращает ошибку; -1 — никогда
	closeMsg  string
	closeCnt  int
	closeCh   chan string
}

func newFakeSink() *fakeSink {
	return &fakeSink{emitAfter: -1, closeCh: make(chan string, 8)}
}

func (s *fakeSink) Emit(streamID string, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.emitAfter >= 0 && len(s.frames) >= s.emitAfter {
		return errors.New("получатель закрыл стрим (инъекция теста)")
	}
	s.frames = append(s.frames, string(payload))
	s.seenAt = append(s.seenAt, time.Now().UnixNano())
	return nil
}

func (s *fakeSink) Close(streamID string, errMsg string) {
	s.mu.Lock()
	s.closeCnt++
	s.closeMsg = errMsg
	s.mu.Unlock()
	select {
	case s.closeCh <- errMsg:
	default:
	}
}

func (s *fakeSink) snapshot() (frames []string, seenAt []int64, closeCnt int, closeMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.frames...),
		append([]int64(nil), s.seenAt...),
		s.closeCnt,
		s.closeMsg
}

// waitClose ждёт завершения стрима с таймаутом.
func (s *fakeSink) waitClose(t *testing.T, timeout time.Duration) string {
	t.Helper()
	select {
	case msg := <-s.closeCh:
		return msg
	case <-time.After(timeout):
		t.Fatal("стрим не завершился за отведённое время")
		return ""
	}
}

// envelopeOut — разобранный конверт ответа плагина.
type envelopeOut struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  *EnvelopeError  `json:"error"`
}

func decodeEnvelope(t *testing.T, raw []byte) envelopeOut {
	t.Helper()
	var env envelopeOut
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("разбор конверта: %v\n%s", err, raw)
	}
	return env
}

// decodeResult — result конверта в структуру.
func decodeResult[T any](t *testing.T, raw []byte) T {
	t.Helper()
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("ожидался успешный конверт, получен: %+v", env.Error)
	}
	var out T
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatalf("разбор result: %v\n%s", err, env.Result)
	}
	return out
}

// expectErr — конверт должен быть ошибкой с указанным кодом и статусом.
func expectErr(t *testing.T, raw []byte, code string, status int) envelopeOut {
	t.Helper()
	env := decodeEnvelope(t, raw)
	if env.OK || env.Error == nil {
		t.Fatalf("ожидалась ошибка %s, получен успешный конверт: %s", code, raw)
	}
	if env.Error.Code != code {
		t.Fatalf("ожидался код %s, получен %q (%s)", code, env.Error.Code, env.Error.Message)
	}
	if status != 0 && env.Error.HTTPStatus != status {
		t.Fatalf("ожидался http_status %d, получен %d (%s)", status, env.Error.HTTPStatus, env.Error.Message)
	}
	return env
}

// logRecorder — сбор всех записей журнала для проверки гигиены секретов.
type logRecorder struct {
	mu      sync.Mutex
	entries []logEntry
}

type logEntry struct {
	level   string
	message string
	fields  map[string]any
}

func (r *logRecorder) add(level, message string, fields map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, logEntry{level: level, message: message, fields: fields})
}

func (r *logRecorder) all() []logEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]logEntry(nil), r.entries...)
}

// newTestCore — ядро поверх net/http-транспорта и мока. Заменяет хост:
// логи пишутся в recorder, sink — фальшивка.
func newTestCore(cfg Config, logs *logRecorder, sink *fakeSink) *Core {
	core := &Core{cfg: cfg, store: newTokenStore()}
	if logs != nil {
		core.logFn = logs.add
	}
	core.newTransport = func(string) Transport { return newHTTPTestTransport() }
	core.newSink = func(string) streamSink { return sink }
	return core
}

// testConfig — конфиг для тестов: апстрим — мок.
func testConfig(mock *gigacodemock.Mock) Config {
	cfg := DefaultConfig()
	cfg.BaseURL = mock.URL() + "/api/v2"
	cfg.Login = "tester"
	return cfg
}

// testDeviceCode — фиктивный device_code из 64 hex.
const testDeviceCode = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// storageFor — StorageJSON для тестовой учётной записи.
func storageFor(deviceCode string) []byte { return canonicalStorageJSON(deviceCode, nil) }

// headerValue — первое значение заголовка без учёта регистра (для записей мока).
func headerValue(header map[string][]string, name string) string {
	for key, values := range header {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

// chatPayload — минимальное тело запроса к модели.
func chatPayload() []byte {
	return []byte(`{"model":"vllm/CodeAgent","messages":[{"role":"user","content":"привет"}]}`)
}

// executorRequestJSON — запрос executor.execute для тестов.
func executorRequestJSON(storage []byte, payload []byte, extra map[string]any) []byte {
	base := map[string]any{
		"AuthID":       "gigacode-test",
		"AuthProvider": providerID,
		"Model":        "vllm/CodeAgent",
		"Format":       "chat-completions",
		"Stream":       false,
		"Headers":      map[string][]string{"Content-Type": {"application/json"}},
		"Payload":      payload,
		"StorageJSON":  storage,
	}
	for key, value := range extra {
		base[key] = value
	}
	raw, err := json.Marshal(base)
	if err != nil {
		panic(err)
	}
	return raw
}

// uuidRegexp — формат UUID v4.
const uuidRegexp = `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`

// assertNoSecrets — гигиена: токены и device_code нигде не светятся.
func assertNoSecrets(t *testing.T, logs *logRecorder, envelopes [][]byte, tokens []string, deviceCode string) {
	t.Helper()
	needles := append([]string{deviceCode}, tokens...)
	// Журнал.
	for _, entry := range logs.all() {
		var fields strings.Builder
		for key, value := range entry.fields {
			fields.WriteString(key)
			fields.WriteString(fmt.Sprintf("=%v", value))
		}
		for _, needle := range needles {
			if strings.Contains(entry.message, needle) || strings.Contains(fields.String(), needle) {
				t.Fatalf("секрет %q найден в журнале: %q / поля %s", needle, entry.message, fields.String())
			}
		}
	}
	// Конверты.
	for _, raw := range envelopes {
		body := string(raw)
		for _, needle := range needles {
			if strings.Contains(body, needle) {
				t.Fatalf("секрет %q найден в конверте: %s", needle, body)
			}
		}
	}
}
