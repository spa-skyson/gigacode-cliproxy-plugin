package gigacode

import (
	"context"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Фиксированные сообщения клиенту: без URL и без сырых текстов транспорта.
// Подробности diagnostics уходят в журнал хоста.
const (
	msgAuthUnavailable  = "Не удалось получить токен GigaCode"
	msgAuthTimeout      = "Таймаут запроса авторизации GigaCode"
	msgChatUnavailable  = "Апстрим GigaCode недоступен"
	msgStreamBroken     = "Апстрим GigaCode прервал поток"
	msgNotAuthorized    = "Устройство не авторизовано в GigaCode — выполните вход через плагин один раз"
	msgInternal         = "Внутренняя ошибка плагина GigaCode"
	msgResponseTooLarge = "Ответ апстрима GigaCode превышает лимит"
)

// Лимиты тела нестримовых ответов апстрима.
const (
	maxDiscoveryBodyBytes = 64 << 10 // шаг 1: discovery
	maxAuthBodyBytes      = 1 << 20  // шаг 2: обмен device_code на JWT
	maxConfigBodyBytes    = 4 << 20  // шаг 3: конфиг с моделями
	maxChatBodyBytes      = 64 << 20 // нестримовый ответ чата
)

// errResponseTooLarge — тело ответа превысило лимит.
var errResponseTooLarge = errors.New("ответ апстрима превышает лимит")

// Transport — исходящий HTTP плагина: вся сеть идёт только через него.
type Transport interface {
	// Do выполняет нестримовый запрос. Тело ответа читается не больше
	// maxBody байт; превышение — errResponseTooLarge.
	Do(ctx context.Context, req Request, maxBody int64) (*Response, error)
	// OpenStream открывает потоковый запрос: статус и заголовки доступны
	// сразу, тело читается через Stream.
	OpenStream(ctx context.Context, req Request) (*StreamResponse, error)
}

// Stream — открытый потоковый ответ апстрима.
type Stream interface {
	// Read возвращает очередной кадр. done=true — апстрим закончил тело.
	// err — ошибка чтения (после неё поток непригоден).
	Read(ctx context.Context) (chunk []byte, done bool, err error)
	// Close закрывает поток; вызывается ровно один раз владельцем.
	Close() error
}

// Request — запрос к апстриму.
type Request struct {
	Method  string
	URL     string
	Headers map[string][]string
	Body    []byte
}

// Response — обычный ответ апстрима.
type Response struct {
	StatusCode int
	Headers    map[string][]string
	Body       []byte
}

// StreamResponse — заголовочная часть потокового ответа.
type StreamResponse struct {
	StatusCode int
	Headers    map[string][]string
	// Stream — тело; всегда не nil при отсутствии ошибки.
	Stream Stream
}

// errDeadline — маркер превышения дедлайна (таймаут запроса авторизации).
var errDeadline = errors.New("истёк таймаут запроса")

// transportError — транспортная ошибка (соединение, TLS, обрыв, отмена).
type transportError struct{ cause error }

func (e *transportError) Error() string { return e.cause.Error() }
func (e *transportError) Unwrap() error { return e.cause }

// isDeadlineErr — ошибка вызвана истечением дедлайна.
func isDeadlineErr(err error) bool {
	return errors.Is(err, errDeadline) || errors.Is(err, context.DeadlineExceeded)
}

// authTransportErr — код и фиксированный текст для сбоя шага авторизации.
func authTransportErr(err error) *pluginError {
	if err == nil {
		return nil
	}
	if isDeadlineErr(err) {
		return newErr(codeUpstreamTimeout, msgAuthTimeout, 504)
	}
	return newErr(codeTransientTransport, msgAuthUnavailable, 502)
}

// authTransientErr — фиксированная временная ошибка флоу без причины:
// причина уходит в журнал вызывающим.
func authTransientErr() *pluginError {
	return newErr(codeTransientTransport, msgAuthUnavailable, 502)
}

// chatTransportErr — код и фиксированный текст для транспортного сбоя чата.
// Превышение лимита тела ответа — отдельный случай: апстрим доступен,
// клиенту сообщается точная причина.
func chatTransportErr(err error) *pluginError {
	if errors.Is(err, errResponseTooLarge) {
		return newErr(codeUpstreamError, msgResponseTooLarge, 502)
	}
	if isDeadlineErr(err) {
		return newErr(codeUpstreamTimeout, msgAuthTimeout, 504)
	}
	return newErr(codeTransientTransport, msgChatUnavailable, 502)
}

// sanitizeText готовит внешний текст к попаданию в журнал или текст ошибки:
// управляющие символы Unicode (категории Cc и Cf — включая \r\n\t, ESC,
// невидимые U+200B/U+202E, которыми можно подделать порядок текста) заменяются
// пробелом, подряд идущие пробелы схлопываются, результат обрезается до
// max байт вместе с маркером усечения, не разрезая руны.
func sanitizeText(s string, max int) string {
	var b strings.Builder
	b.Grow(min(len(s), max))
	prevSpace := true
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			r = ' '
		}
		if r == ' ' {
			if prevSpace {
				continue
			}
			prevSpace = true
		} else {
			prevSpace = false
		}
		b.WriteRune(r)
	}
	return truncateBytes(b.String(), max)
}

// truncateBytes обрезает строку до limit байт, не разрезая руны; при
// усечении добавляется маркер «…», который учитывается в лимите.
func truncateBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	if limit <= len("…") {
		return ""
	}
	cut := s[:limit-len("…")]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
}

// logDetail пишет деталь диагностики в журнал хоста; без журнала — молчание.
// Текст проходит очистку: внешние строки могут содержать управляющие символы.
func logDetail(log Logger, level, message string) {
	if log == nil {
		return
	}
	log(level, sanitizeText(message, 512), nil)
}
