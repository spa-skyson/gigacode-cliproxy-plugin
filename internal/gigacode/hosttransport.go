package gigacode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// HostCaller — функция вызова колбэка хоста через C-ABI
// (main.go передаёт её в этот пакет при инициализации).
// Возвращает тело конверта хоста; ошибку — при неудаче самого вызова.
type HostCaller func(method string, payload []byte) ([]byte, error)

// Имена колбэков хоста (см. sdk/pluginabi/types.go хоста).
const (
	methodHostHTTPDoStream      = "host.http.do_stream"
	methodHostHTTPOperationOpen = "host.http.operation_open"
	methodHostHTTPCancel        = "host.http.cancel"
	methodHostHTTPStreamRead    = "host.http.stream_read"
	methodHostHTTPStreamClose   = "host.http.stream_close"
	methodHostStreamEmit        = "host.stream.emit"
	methodHostStreamClose       = "host.stream.close"
	methodHostLog               = "host.log"
)

// hostTransport — Transport поверх колбэков хоста. Каждый экземпляр привязан
// к host_callback_id конкретного запроса.
type hostTransport struct {
	call       HostCaller
	callbackID string
}

// compile-time проверка контракта.
var _ Transport = (*hostTransport)(nil)

// newHostTransport строит транспорт, привязанный к колбэку запроса.
func newHostTransport(call HostCaller, callbackID string) *hostTransport {
	return &hostTransport{call: call, callbackID: callbackID}
}

// hostCallError — ошибка, вернувшаяся в конверте хоста.
type hostCallError struct{ message string }

func (e *hostCallError) Error() string { return e.message }

// callHost выполняет колбэк хоста и разбирает конверт {"ok":…}.
func (t *hostTransport) callHost(method string, payload any) (json.RawMessage, error) {
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("маршалинг запроса %s: %w", method, err)
	}
	rawResp, err := t.call(method, rawPayload)
	if err != nil {
		return nil, err
	}
	var env Envelope
	if err := json.Unmarshal(rawResp, &env); err != nil {
		return nil, fmt.Errorf("разбор конверта %s: %w", method, err)
	}
	if !env.OK {
		msg := "колбэк хоста " + method + " завершился ошибкой"
		if env.Error != nil {
			if strings.TrimSpace(env.Error.Message) != "" {
				msg = env.Error.Message
			}
		}
		return nil, &hostCallError{message: msg}
	}
	return env.Result, nil
}

// ------------------------------------------------------------ host.http.do

// hostHTTPRequest — запрос host.http.do / host.http.do_stream
// (internal/pluginhost/host_callbacks.go: rpcHostHTTPRequest, ключи snake_case).
type hostHTTPRequest struct {
	Method         string              `json:"method,omitempty"`
	URL            string              `json:"url,omitempty"`
	Headers        map[string][]string `json:"headers,omitempty"`
	Body           []byte              `json:"body,omitempty"`
	HostCallbackID string              `json:"host_callback_id,omitempty"`
	OperationID    string              `json:"operation_id,omitempty"`
}

// hostHTTPStreamResponse — результат host.http.do_stream
// (rpcHostHTTPStreamResponse, ключи snake_case).
type hostHTTPStreamResponse struct {
	StatusCode int                 `json:"status_code"`
	Headers    map[string][]string `json:"headers,omitempty"`
	StreamID   string              `json:"stream_id,omitempty"`
}

// hostStreamReadResponse — результат host.http.stream_read.
type hostStreamReadResponse struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

// hostOperationOpenResponse — результат host.http.operation_open.
type hostOperationOpenResponse struct {
	OperationID string `json:"operation_id"`
}

// hostCancelRequest — запрос host.http.cancel.
type hostCancelRequest struct {
	HostCallbackID string `json:"host_callback_id,omitempty"`
	OperationID    string `json:"operation_id"`
}

// Do выполняет нестримовый запрос через host.http.do_stream и читает тело
// из host.http.stream_read, суммарно не больше maxBody байт: хост в
// host.http.do читает тело целиком без ограничений, поэтому лимит
// реализуется на стороне плагина. Превышение — errResponseTooLarge,
// поток при этом закрывается. host.http.stream_close выполняется ровно
// один раз при любом исходе.
//
// Операция отмены и таймер живут до конца чтения тела, а не только до
// получения stream_id: если апстрим отдал заголовки и завис на теле,
// таймер вызывает host.http.cancel — хост закрывает и поток
// (см. internal/pluginhost/host_callbacks.go), заблокированный
// stream_read возвращает ошибку, и запрос завершается по дедлайну.
func (t *hostTransport) Do(ctx context.Context, req Request, maxBody int64) (*Response, error) {
	payload := hostHTTPRequest{
		Method:         req.Method,
		URL:            req.URL,
		Headers:        req.Headers,
		Body:           req.Body,
		HostCallbackID: t.callbackID,
	}
	stop, err := t.armDeadlineCancel(ctx, &payload)
	if err != nil {
		// Запрос с дедлайном без механизма отмены не отправляется.
		return nil, &transportError{cause: err}
	}
	defer stop()
	raw, err := t.callHost(methodHostHTTPDoStream, payload)
	if err != nil {
		return nil, t.wrapCallErr(ctx, err)
	}
	var opened hostHTTPStreamResponse
	if err := json.Unmarshal(raw, &opened); err != nil {
		// Поток на стороне хоста мог уже открыться: откатываем.
		t.abortBrokenDoStream(payload, raw)
		return nil, fmt.Errorf("разбор ответа host.http.do_stream: %w", err)
	}
	if opened.StreamID == "" {
		return nil, &transportError{cause: fmt.Errorf("хост не вернул stream_id")}
	}
	stream := &hostStream{call: t.call, streamID: opened.StreamID}

	var body []byte
	tooLarge := false
readLoop:
	for {
		chunk, done, readErr := stream.Read(ctx)
		if readErr != nil {
			_ = stream.Close()
			return nil, t.wrapCallErr(ctx, readErr)
		}
		if len(chunk) > 0 {
			if int64(len(body)+len(chunk)) > maxBody {
				tooLarge = true
				break readLoop
			}
			body = append(body, chunk...)
		}
		if done {
			break readLoop
		}
	}
	// Close идемпотентен: вызывается на каждом пути ровно один раз.
	_ = stream.Close()
	if ctx.Err() != nil {
		return nil, t.wrapCallErr(ctx, ctx.Err())
	}
	if tooLarge {
		return nil, errResponseTooLarge
	}
	return &Response{StatusCode: opened.StatusCode, Headers: opened.Headers, Body: body}, nil
}

// OpenStream выполняет host.http.do_stream и возвращает поток-читалку.
// Дедлайн здесь не применяется: после установки соединения таймаутов нет,
// а для ошибочных ответов дренаж тела закрывает поток своим таймером.
func (t *hostTransport) OpenStream(ctx context.Context, req Request) (*StreamResponse, error) {
	payload := hostHTTPRequest{
		Method:         req.Method,
		URL:            req.URL,
		Headers:        req.Headers,
		Body:           req.Body,
		HostCallbackID: t.callbackID,
	}
	raw, err := t.callHost(methodHostHTTPDoStream, payload)
	if err != nil {
		return nil, t.wrapCallErr(ctx, err)
	}
	var opened hostHTTPStreamResponse
	if err := json.Unmarshal(raw, &opened); err != nil {
		// Поток на стороне хоста мог уже открыться: закрываем по мягкому разбору.
		t.abortBrokenDoStream(payload, raw)
		return nil, fmt.Errorf("разбор ответа host.http.do_stream: %w", err)
	}
	if opened.StreamID == "" {
		return nil, &transportError{cause: fmt.Errorf("хост не вернул stream_id")}
	}
	return &StreamResponse{
		StatusCode: opened.StatusCode,
		Headers:    opened.Headers,
		Stream: &hostStream{
			call:     t.call,
			streamID: opened.StreamID,
		},
	}, nil
}

// armDeadlineCancel — при наличии дедлайна открывает операцию отмены и
// заводит таймер, который по истечении дедлайна вызывает host.http.cancel
// (хост при этом закрывает и поток операции). Если механизм отмены
// недоступен — нет host_callback_id, операция не открылась или не вернула
// operation_id — запрос с дедлайном не отправляется: уходить в сеть без
// возможности отмены нельзя. Возвращает функцию остановки таймера.
func (t *hostTransport) armDeadlineCancel(ctx context.Context, payload *hostHTTPRequest) (func(), error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return func() {}, nil
	}
	if t.callbackID == "" {
		t.logWarn("Запрос с дедлайном не отправляется: host_callback_id отсутствует, механизм отмены недоступен")
		return nil, errors.New("механизм отмены недоступен: host_callback_id отсутствует")
	}
	raw, err := t.callHost(methodHostHTTPOperationOpen, map[string]string{"host_callback_id": t.callbackID})
	if err != nil {
		t.logWarn("Операция отмены не открыта (" + err.Error() + ") — запрос с дедлайном не отправляется")
		return nil, fmt.Errorf("операция отмены не открылась: %w", err)
	}
	var opened hostOperationOpenResponse
	if err := json.Unmarshal(raw, &opened); err != nil || opened.OperationID == "" {
		t.logWarn("Операция отмены не вернула operation_id — запрос с дедлайном не отправляется")
		return nil, errors.New("операция отмены не вернула operation_id")
	}
	payload.OperationID = opened.OperationID
	timer := time.AfterFunc(time.Until(deadline), func() {
		_, _ = t.callHost(methodHostHTTPCancel, hostCancelRequest{
			HostCallbackID: t.callbackID,
			OperationID:    opened.OperationID,
		})
	})
	return func() { timer.Stop() }, nil
}

// abortBrokenDoStream — откат при повреждённом ответе host.http.do_stream:
// отменить операцию (если открывалась) и закрыть поток, если его stream_id
// удалось извлечь мягким разбором сырого ответа.
func (t *hostTransport) abortBrokenDoStream(payload hostHTTPRequest, raw []byte) {
	if payload.OperationID != "" {
		_, _ = t.callHost(methodHostHTTPCancel, hostCancelRequest{
			HostCallbackID: t.callbackID,
			OperationID:    payload.OperationID,
		})
	}
	var partial struct {
		StreamID string `json:"stream_id"`
	}
	if json.Unmarshal(raw, &partial) == nil && partial.StreamID != "" {
		_, _ = callHostRaw(t.call, methodHostHTTPStreamClose, map[string]string{"stream_id": partial.StreamID})
	}
}

// logWarn — запись warn в журнал хоста от транспорта (host.log).
func (t *hostTransport) logWarn(message string) {
	payload := map[string]any{"level": "warn", "message": sanitizeText(message, 512)}
	if t.callbackID != "" {
		payload["host_callback_id"] = t.callbackID
	}
	_, _ = callHostRaw(t.call, methodHostLog, payload)
}

// wrapCallErr — классификация ошибки вызова хоста: если контекст вышел за
// дедлайн, это таймаут; иначе — транспортная ошибка.
func (t *hostTransport) wrapCallErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return &transportError{cause: fmt.Errorf("%w: %v", errDeadline, err)}
	}
	return &transportError{cause: err}
}

// hostStream — потоковый ответ апстрима поверх host.http.stream_read/close.
type hostStream struct {
	call     HostCaller
	streamID string
	once     sync.Once
	closeErr error
}

// Read читает очередной кадр (host.http.stream_read блокируется до кадра или конца).
func (s *hostStream) Read(ctx context.Context) ([]byte, bool, error) {
	raw, err := callHostRaw(s.call, methodHostHTTPStreamRead, map[string]string{"stream_id": s.streamID})
	if err != nil {
		return nil, true, fmt.Errorf("чтение апстрим-стрима: %w", err)
	}
	var resp hostStreamReadResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, true, fmt.Errorf("разбор ответа host.http.stream_read: %w", err)
	}
	if resp.Error != "" {
		return nil, true, fmt.Errorf("%s", resp.Error)
	}
	return resp.Payload, resp.Done, nil
}

// Close закрывает HTTP-стрим апстрима ровно один раз.
func (s *hostStream) Close() error {
	s.once.Do(func() {
		_, s.closeErr = callHostRaw(s.call, methodHostHTTPStreamClose, map[string]string{"stream_id": s.streamID})
	})
	return s.closeErr
}

// callHostRaw — вызов колбэка хоста без привязки к транспорту
// (нужен для host.stream.emit/close из горутины стрима с тем же callback ID).
func callHostRaw(call HostCaller, method string, payload any) (json.RawMessage, error) {
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	rawResp, err := call(method, rawPayload)
	if err != nil {
		return nil, err
	}
	var env Envelope
	if err := json.Unmarshal(rawResp, &env); err != nil {
		return nil, fmt.Errorf("разбор конверта %s: %w", method, err)
	}
	if !env.OK {
		msg := "колбэк хоста " + method + " завершился ошибкой"
		if env.Error != nil && strings.TrimSpace(env.Error.Message) != "" {
			msg = env.Error.Message
		}
		return nil, &hostCallError{message: msg}
	}
	return env.Result, nil
}

// Log пишет запись в журнал хоста (host.log). Токен и device_code сюда не
// передаются никогда.
func (t *hostTransport) Log(level, message string, fields map[string]any) {
	payload := map[string]any{"level": level, "message": message}
	if t.callbackID != "" {
		payload["host_callback_id"] = t.callbackID
	}
	if len(fields) > 0 {
		payload["fields"] = fields
	}
	_, _ = callHostRaw(t.call, methodHostLog, payload)
}
