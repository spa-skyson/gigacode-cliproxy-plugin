package gigacode

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Ограничения обработки запроса к модели.
const (
	// chatAttempts — одна попытка + один повтор после 401.
	chatAttempts = 2
	// maxErrorBodyBytes — сколько байт тела ошибочного стрим-ответа читаем
	// для текста ошибки.
	maxErrorBodyBytes = 64 << 10
	// errorBodyDrainTimeout — как долго ждём тело ошибочного стрим-ответа.
	errorBodyDrainTimeout = 3 * time.Second
)

// userGigaCodeAgent — User-Agent как в app.py: GigaCode/{plugin-version} (linux; x64).
func userGigaCodeAgent(cfg Config) string {
	return fmt.Sprintf("GigaCode/%s (linux; x64)", cfg.PluginVersion)
}

// newUUIDv4 — UUID версии 4 через crypto/rand.
func newUUIDv4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand не подводит в Linux; на всякий случай — детерминированный
		// резерв невозможен, сообщаем ошибкой времени выполнения.
		panic("crypto/rand недоступен: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // версия 4
	b[8] = (b[8] & 0x3f) | 0x80 // вариант RFC 4122
	const hex = "0123456789abcdef"
	out := make([]byte, 0, 36)
	for i, v := range b {
		switch i {
		case 4, 6, 8, 10:
			out = append(out, '-')
		}
		out = append(out, hex[v>>4], hex[v&0x0f])
	}
	return string(out)
}

// upstreamHeaders — заголовки запроса к апстриму:
// Authorization, x-session-id (новый UUID на попытку), User-Agent, Content-Type.
func upstreamHeaders(cfg Config, reqHeaders map[string][]string, token string) map[string][]string {
	contentType := firstHeader(reqHeaders, "Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	return map[string][]string{
		"Authorization": {"Bearer " + token},
		"x-session-id":  {newUUIDv4()},
		"User-Agent":    {userGigaCodeAgent(cfg)},
		"Content-Type":  {contentType},
	}
}

// handleExecutorExecute — executor.execute и executor.execute_stream.
func handleExecutorExecute(core *Core, raw []byte, stream bool) ([]byte, error) {
	var req ExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		core.log("warn", "Некорректный запрос executor.execute: "+sanitizeText(err.Error(), 512), nil)
		return nil, newErr(codeInvalidRequest, "Некорректный запрос", 400)
	}
	if req.AuthProvider != providerID {
		return nil, newErr(codeInvalidRequest,
			"Запрос предназначен не для провайдера gigacode: "+req.AuthProvider, 400)
	}
	deviceCode, err := parseStorageJSON(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	cfg := core.config()
	if len(req.Payload) > cfg.MaxBodyBytes {
		return nil, newErr(codeRequestScoped, "Тело запроса превышает лимит", 413)
	}
	tr := core.transportFor(req.HostCallbackID)
	snap, err := core.ensureAuth(context.Background(), tr, cfg, deviceCode, false)
	if err != nil {
		core.log("warn", "Авторизация GigaCode не удалась: "+sanitizeText(err.Error(), 512), nil)
		return nil, mapAuthErr(err)
	}
	chatURL := resolveChatURL(cfg.BaseURL, snap.ChatURL)
	// Абсолютный chat.url из конфига апстрима проходит ту же проверку схемы,
	// что и base-url; относительный путь не проверяется.
	if strings.HasPrefix(chatURL, "http://") || strings.HasPrefix(chatURL, "https://") {
		if urlErr := validateUpstreamURL(chatURL, cfg.AllowInsecureHTTP); urlErr != nil {
			core.log("warn", "chat.url апстрима недопустим ("+urlErr.Error()+"): "+chatURL, nil)
			return nil, newErr(codeTransientTransport, msgChatUnavailable, 502)
		}
	}
	if stream && strings.TrimSpace(req.StreamID) == "" {
		return nil, newErr(codeInvalidRequest, "stream_id обязателен для executor.execute_stream", 400)
	}

	for attempt := 0; attempt < chatAttempts; attempt++ {
		request := Request{
			Method:  "POST",
			URL:     chatURL,
			Headers: upstreamHeaders(cfg, req.Headers, snap.Token),
			Body:    req.Payload, // тело байт-в-байт, без изменений
		}
		if !stream {
			done, out := executeOnce(core, tr, req, request, attempt)
			if done {
				return out, nil
			}
		} else {
			done, out := executeStreamOnce(core, tr, req, request, attempt)
			if done {
				return out, nil
			}
		}
		// Попытка кончилась 401: обновляем токен и идём на повтор с тем же телом.
		snap, err = core.ensureAuth(context.Background(), tr, cfg, deviceCode, true)
		if err != nil {
			core.log("warn", "Повторная авторизация GigaCode не удалась: "+sanitizeText(err.Error(), 512), nil)
			return nil, mapAuthErr(err)
		}
	}
	return nil, newErr(codeUpstreamAuthFail, "Апстрим GigaCode отклонил токен после переавторизации", 502)
}

// executeOnce — одна нестримовая попытка. Возвращает (готово, ответ-конверт).
func executeOnce(core *Core, tr Transport, req ExecutorRequest, request Request, attempt int) (bool, []byte) {
	resp, err := tr.Do(context.Background(), request, maxChatBodyBytes)
	if err != nil {
		core.log("warn", "Ошибка запроса к чату GigaCode: POST "+request.URL+": "+sanitizeText(err.Error(), 512), nil)
		return true, errEnvelope(chatTransportErr(err))
	}
	if resp.StatusCode == 401 && attempt < chatAttempts-1 {
		core.log("info", "Апстрим GigaCode вернул 401, выполняется переавторизация", nil)
		return false, nil // повтор с новым токеном
	}
	if resp.StatusCode == 401 {
		core.log("warn", "Апстрим GigaCode отклонил токен после переавторизации", nil)
		return true, errEnvelope(newErr(codeUpstreamAuthFail,
			"Апстрим GigaCode отклонил токен после переавторизации", 502))
	}
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		return true, mustEnvelope(okEnvelope(ExecutorResponse{
			Payload: resp.Body,
			Headers: map[string][]string{"Content-Type": headerOr(firstHeaderAll(resp.Headers, "Content-Type"), "application/json")},
		}))
	}
	return true, errEnvelope(classifyUpstreamStatus(resp.StatusCode, shortUpstreamMessage(resp.Body)))
}

// executeStreamOnce — одна стримовая попытка. Апстрим открывается до возврата
// из метода; при 2xx метод сразу возвращает заголовки, кадры качает горутина.
func executeStreamOnce(core *Core, tr Transport, req ExecutorRequest, request Request, attempt int) (bool, []byte) {
	ctx := context.Background()
	st, err := tr.OpenStream(ctx, request)
	if err != nil {
		core.log("warn", "Ошибка открытия стрима к чату GigaCode: POST "+request.URL+": "+sanitizeText(err.Error(), 512), nil)
		return true, errEnvelope(chatTransportErr(err))
	}
	if st.StatusCode == 401 && attempt < chatAttempts-1 {
		_ = st.Stream.Close()
		core.log("info", "Апстрим GigaCode вернул 401 на стрим, выполняется переавторизация", nil)
		return false, nil
	}
	if st.StatusCode == 401 {
		_ = st.Stream.Close()
		core.log("warn", "Апстрим GigaCode отклонил токен на стриме после переавторизации", nil)
		return true, errEnvelope(newErr(codeUpstreamAuthFail,
			"Апстрим GigaCode отклонил токен после переавторизации", 502))
	}
	if st.StatusCode < 200 || st.StatusCode > 299 {
		// Ошибочный ответ: читаем немного тела для текста ошибки. Таймер
		// закрывает поток по истечении предела, чтение после этого падает.
		message := drainErrorBody(ctx, st.Stream, shortUpstreamMessage)
		_ = st.Stream.Close()
		return true, errEnvelope(classifyUpstreamStatus(st.StatusCode, message))
	}
	// 2xx: отдаём заголовки сразу, кадры — асинхронно через host.stream.emit.
	// Клиенту передаётся только Content-Type апстрима: чужие Content-Length,
	// Content-Encoding и служебные заголовки наружу не идут (как в app.py).
	streamHeaders := map[string][]string{}
	if ct := firstHeaderAll(st.Headers, "Content-Type"); len(ct) > 0 {
		streamHeaders["Content-Type"] = ct
	}
	sink := core.streamSinkFor(req.HostCallbackID)
	idle := time.Duration(core.config().StreamIdleTimeoutSeconds) * time.Second
	go pumpStream(core, sink, req.StreamID, st.Stream, idle)
	return true, mustEnvelope(okEnvelope(ExecutorStreamResponse{Headers: streamHeaders}))
}

// pumpStream читает апстрим и отдаёт кадры в хост. HTTP-стрим закрывается
// ровно один раз при любом исходе; ошибка emit останавливает чтение.
//
// Простой потока ограничен: если прогресса (успешное чтение кадра или
// успешная отправка кадра получателю) нет дольше idle, таймер закрывает
// HTTP-стрим апстрима — заблокированное чтение возвращает ошибку, поток
// завершается через host.stream.close. Закрытие выполняется ровно один раз.
func pumpStream(core *Core, sink streamSink, streamID string, st Stream, idle time.Duration) {
	closer := &onceStreamCloser{inner: st}
	idleTimer := time.AfterFunc(idle, func() {
		core.log("warn", fmt.Sprintf("Поток GigaCode остановлен: нет прогресса дольше %d с", int(idle.Seconds())), nil)
		_ = closer.Close()
	})
	defer func() {
		idleTimer.Stop()
		if r := recover(); r != nil {
			core.log("error", fmt.Sprintf("Паника в горутине стрима: %v", r), nil)
			sink.Close(streamID, msgInternal)
		}
		_ = closer.Close()
	}()
	ctx := context.Background()
	for {
		chunk, done, err := st.Read(ctx)
		if err != nil {
			core.log("warn", "Ошибка чтения стрима GigaCode: "+sanitizeText(err.Error(), 512), nil)
			sink.Close(streamID, msgStreamBroken)
			return
		}
		if len(chunk) > 0 {
			idleTimer.Reset(idle) // прогресс: кадр прочитан
			if emitErr := sink.Emit(streamID, chunk); emitErr != nil {
				// Клиент ушёл или мост закрыт: прекращаем чтение.
				core.log("debug", "Стрим GigaCode остановлен: получатель закрыт", nil)
				return
			}
			idleTimer.Reset(idle) // прогресс: кадр доставлен получателю
		}
		if done {
			sink.Close(streamID, "")
			return
		}
	}
}

// onceStreamCloser гарантирует одно выполнение Close: таймер простоя и
// владелец потока могут вызывать его независимо.
type onceStreamCloser struct {
	inner Stream
	once  sync.Once
	err   error
}

func (c *onceStreamCloser) Close() error {
	c.once.Do(func() { c.err = c.inner.Close() })
	return c.err
}

// drainErrorBody читает немного тела ошибочного стрим-ответа, ограничивая
// и объём, и время ожидания. Горутин нет: по истечении предела таймер
// закрывает поток, чтение после этого возвращает ошибку и цикл завершается.
// Кадр ограничивается оставшимся объёмом, чтение прекращается на заполнении.
func drainErrorBody(ctx context.Context, st Stream, shape func([]byte) string) string {
	var buf bytes.Buffer
	timer := time.AfterFunc(errorBodyDrainTimeout, func() { _ = st.Close() })
	defer timer.Stop()
	for buf.Len() < maxErrorBodyBytes {
		chunk, done, err := st.Read(ctx)
		if err != nil || done {
			break
		}
		room := maxErrorBodyBytes - buf.Len()
		if len(chunk) > room {
			chunk = chunk[:room]
		}
		buf.Write(chunk)
	}
	return shape(buf.Bytes())
}

// classifyUpstreamStatus — маппинг статусов апстрима на коды ошибки.
// Текст апстрима для статусов 4xx и 5xx чата остаётся в сообщении клиенту:
// это осознанное решение, как в proxy/app.py — причина отказа формируется
// апстримом и полезна клиенту API; текст проходит очистку sanitizeText.
func classifyUpstreamStatus(status int, message string) *pluginError {
	message = sanitizeText(message, 512)
	switch {
	case status == 401:
		return newErr(codeUpstreamAuthFail, "Апстрим GigaCode отклонил токен: "+message, 502)
	case status == 403 || status == 429 || status >= 500:
		return newErr(codeUpstreamError, fmt.Sprintf("Апстрим GigaCode вернул статус %d: %s", status, message), status)
	case status >= 400:
		return newErr(codeRequestScoped, fmt.Sprintf("Запрос отклонён апстримом GigaCode (%d): %s", status, message), status)
	default:
		return newErr(codeUpstreamError, fmt.Sprintf("Апстрим GigaCode вернул неожиданный статус %d: %s", status, message), status)
	}
}

// mapAuthErr — классификация ошибки флоу авторизации для ответа исполнителя:
// фиксированные сообщения без деталей, детали уходят в журнал вызывающим.
func mapAuthErr(err error) *pluginError {
	if err == nil {
		return nil
	}
	if isNotAuthorized(err) {
		return newErr(codeDeviceNotAuth, notAuthorizedMessage, 502)
	}
	if pe, ok := asPluginErr(err); ok {
		return pe
	}
	return authTransportErr(err)
}

// headerOr — значение заголовка или замена по умолчанию.
func headerOr(values []string, fallback string) []string {
	if len(values) == 0 {
		return []string{fallback}
	}
	return values
}

// firstHeaderAll — все значения заголовка без учёта регистра.
func firstHeaderAll(headers map[string][]string, name string) []string {
	for key, values := range headers {
		if strings.EqualFold(key, name) {
			return values
		}
	}
	return nil
}

// mustEnvelope — для уже собранных конвертов: маршалинг не может упасть.
func mustEnvelope(raw []byte, err error) []byte {
	if err != nil {
		return errEnvelope(err)
	}
	return raw
}
