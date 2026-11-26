package gigacode

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/spa-skyson/gigacode-cliproxy-plugin/internal/gigacodemock"
)

// -------------------------------------------------- лимит тела (hostTransport)

// fakeHostHTTP — фейковый вызов колбэков хоста для hostTransport.Do:
// do_stream отдаёт stream_id, stream_read выдаёт заготовленные кадры.
type fakeHostHTTP struct {
	mu         sync.Mutex
	chunks     [][]byte
	readErr    string
	closeCount int
	position   int
}

func (f *fakeHostHTTP) call(method string, payload []byte) ([]byte, error) {
	switch method {
	case methodHostHTTPDoStream:
		return []byte(`{"ok":true,"result":{"status_code":200,"headers":{"content-type":["application/json"]},"stream_id":"s1"}}`), nil
	case methodHostHTTPStreamRead:
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.readErr != "" {
			return []byte(fmt.Sprintf(`{"ok":true,"result":{"error":%q,"done":true}}`, f.readErr)), nil
		}
		if f.position >= len(f.chunks) {
			return []byte(`{"ok":true,"result":{"done":true}}`), nil
		}
		chunk := f.chunks[f.position]
		f.position++
		return []byte(fmt.Sprintf(`{"ok":true,"result":{"payload":%q}}`, base64.StdEncoding.EncodeToString(chunk))), nil
	case methodHostHTTPStreamClose:
		f.mu.Lock()
		f.closeCount++
		f.mu.Unlock()
		return []byte(`{"ok":true,"result":{}}`), nil
	default:
		return []byte(`{"ok":true,"result":{}}`), nil
	}
}

func (f *fakeHostHTTP) closeCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closeCount
}

// Тело ровно в лимит читается целиком; stream_close выполнен один раз.
func TestHostTransportDoBodyWithinLimit(t *testing.T) {
	fake := &fakeHostHTTP{chunks: [][]byte{[]byte(strings.Repeat("a", 100)), []byte(strings.Repeat("b", 100))}}
	tr := newHostTransport(fake.call, "cb-1")

	resp, err := tr.Do(context.Background(), Request{Method: "GET", URL: "https://upstream/config"}, 200)
	if err != nil {
		t.Fatalf("чтение упало: %v", err)
	}
	if len(resp.Body) != 200 || resp.StatusCode != 200 {
		t.Fatalf("тело не прочитано целиком: %d байт, статус %d", len(resp.Body), resp.StatusCode)
	}
	if got := fake.closeCalls(); got != 1 {
		t.Fatalf("stream_close вызван %d раз, ожидался 1", got)
	}
}

// Тело больше лимита — errResponseTooLarge, поток закрыт ровно один раз.
func TestHostTransportDoBodyTooLarge(t *testing.T) {
	fake := &fakeHostHTTP{chunks: [][]byte{[]byte(strings.Repeat("a", 100)), []byte(strings.Repeat("b", 100))}}
	tr := newHostTransport(fake.call, "cb-1")

	_, err := tr.Do(context.Background(), Request{Method: "GET", URL: "https://upstream/config"}, 150)
	if !errors.Is(err, errResponseTooLarge) {
		t.Fatalf("ожидалась errResponseTooLarge, получено: %v", err)
	}
	if got := fake.closeCalls(); got != 1 {
		t.Fatalf("stream_close вызван %d раз, ожидался 1", got)
	}
}

// ------------------------------------------------------ допустимые URL (2.1)

func TestValidateUpstreamURL(t *testing.T) {
	cases := []struct {
		raw           string
		allowInsecure bool
		ok            bool
	}{
		{"https://gigacode.example/api/v2", false, true},
		{"http://127.0.0.1:18090/api/v2", false, true},
		{"http://127.255.0.9/api/v2", false, true},
		{"http://localhost:18090/api/v2", false, true},
		{"http://LOCALHOST/api/v2", false, true},
		{"http://sub.localhost/api/v2", false, false},
		{"http://localhost./api/v2", false, false},
		{"http://[::1]:18090/api/v2", false, true},
		{"https://user@gigacode.example/api/v2", false, false},
		{"https://gigacode.ru@evil.example/", false, false},
		{"http://example.com/api/v2", false, false},
		{"http://10.0.0.7/api/v2", false, false},
		{"ftp://example.com", false, false},
		{"/only/path", false, false},
		{"", false, false},
		{"http://example.com/api/v2", true, true},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s/insecure=%v", tc.raw, tc.allowInsecure), func(t *testing.T) {
			err := validateUpstreamURL(tc.raw, tc.allowInsecure)
			if tc.ok && err != nil {
				t.Fatalf("ожидалась допустимость, получено: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("ожидался отказ, URL принят")
			}
		})
	}
}

// base-url на http вне loopback — отказ регистрации; с allow-insecure-http —
// регистрация проходит.
func TestConfigBaseURLScheme(t *testing.T) {
	if _, err := parseConfig([]byte("base-url: http://example.com/api/v2\n")); err == nil {
		t.Fatal("http вне loopback должен отклоняться")
	} else if pe, ok := asPluginErr(err); !ok || pe.code != codeInvalidConfig {
		t.Fatalf("код ошибки: %v", err)
	}
	cfg, err := parseConfig([]byte("base-url: http://example.com/api/v2\nallow-insecure-http: true\n"))
	if err != nil {
		t.Fatalf("allow-insecure-http должен разрешить http: %v", err)
	}
	if !cfg.AllowInsecureHTTP {
		t.Fatal("allow-insecure-http не разобран")
	}
	def := DefaultConfig()
	if def.AllowInsecureHTTP {
		t.Fatal("allow-insecure-http по умолчанию должен быть выключен")
	}
}

// Абсолютный chat.url на http вне loopback — ошибка чата, апстрим не вызывался.
func TestExecutorChatURLScheme(t *testing.T) {
	mock := gigacodemock.New()
	defer mock.Close()
	core := newTestCore(testConfig(mock), nil, nil)
	mock.SetChatURL("http://example.com/chat/completions")

	raw := executorRequestJSON(storageFor(testDeviceCode), chatPayload(), nil)
	env := expectErr(t, core.Handle("executor.execute", raw), codeTransientTransport, 502)
	if env.Error.Message != msgChatUnavailable {
		t.Fatalf("сообщение: %q, ожидалось %q", env.Error.Message, msgChatUnavailable)
	}
	if mock.ChatCalls() != 0 {
		t.Fatalf("чат вызывался %d раз, ожидался 0", mock.ChatCalls())
	}
}

// -------------------------------------------------------- expires_in (3.1)

func TestParseAuthResponseExpiresIn(t *testing.T) {
	ok := func(t *testing.T, body string, want time.Duration) {
		t.Helper()
		token, expiresIn, err := parseAuthResponse([]byte(body))
		if err != nil || token == "" {
			t.Fatalf("неожиданная ошибка: %v", err)
		}
		if expiresIn != want {
			t.Fatalf("expires_in=%v, ожидалось %v", expiresIn, want)
		}
	}
	transient := func(t *testing.T, body string) {
		t.Helper()
		_, _, err := parseAuthResponse([]byte(body))
		pe, ok := asPluginErr(err)
		if !ok || pe.code != codeTransientTransport {
			t.Fatalf("ожидалась временная ошибка, получено: %v", err)
		}
	}
	t.Run("отсутствует_по_умолчанию_600", func(t *testing.T) {
		ok(t, `{"access_token":"jwt-1"}`, 600*time.Second)
	})
	t.Run("границы_допустимы", func(t *testing.T) {
		ok(t, `{"access_token":"jwt-1","expires_in":1}`, 1*time.Second)
		ok(t, `{"access_token":"jwt-1","expires_in":86400}`, 86400*time.Second)
		ok(t, `{"access_token":"jwt-1","expires_in":600.0}`, 600*time.Second)
	})
	t.Run("вне_диапазона_временная_ошибка", func(t *testing.T) {
		transient(t, `{"access_token":"jwt-1","expires_in":0}`)
		transient(t, `{"access_token":"jwt-1","expires_in":-5}`)
		transient(t, `{"access_token":"jwt-1","expires_in":86401}`)
		transient(t, `{"access_token":"jwt-1","expires_in":"600"}`)
		transient(t, `{"access_token":"jwt-1","expires_in":true}`)
		transient(t, `{"access_token":"jwt-1","expires_in":null}`)
	})
	t.Run("нет_токена_временная_ошибка", func(t *testing.T) {
		transient(t, `{"expires_in":600}`)
		transient(t, `не-json`)
	})
}

// -------------------------------------------------- диапазоны конфига (3.2)

func TestConfigNumericRanges(t *testing.T) {
	cases := []struct {
		key   string
		value string
		ok    bool
	}{
		{"refresh-margin-seconds", "0", true},
		{"refresh-margin-seconds", "86400", true},
		{"refresh-margin-seconds", "86401", false},
		{"refresh-margin-seconds", "-1", false},
		{"auth-timeout-seconds", "1", true},
		{"auth-timeout-seconds", "600", true},
		{"auth-timeout-seconds", "0", false},
		{"auth-timeout-seconds", "601", false},
		{"retry-seconds", "1", true},
		{"retry-seconds", "3600", true},
		{"retry-seconds", "0", false},
		{"retry-seconds", "3601", false},
		{"max-body-bytes", "1", true},
		{"max-body-bytes", "1073741824", true},
		{"max-body-bytes", "0", false},
		{"max-body-bytes", "1073741825", false},
	}
	for _, tc := range cases {
		t.Run(tc.key+"/"+tc.value, func(t *testing.T) {
			yaml := "base-url: https://gigacode.example/api/v2\n" + tc.key + ": " + tc.value
			_, err := parseConfig([]byte(yaml))
			if tc.ok && err != nil {
				t.Fatalf("значение в диапазоне отклонено: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("значение вне диапазона принято")
			}
		})
	}
}

// --------------------------------------------- канонический StorageJSON (3.3)

func TestAuthRefreshReturnsCanonicalStorage(t *testing.T) {
	mock := gigacodemock.New()
	defer mock.Close()
	core := newTestCore(testConfig(mock), nil, nil)

	// В запросе — не-каноничный StorageJSON с лишним ключом.
	dirty, _ := json.Marshal(map[string]any{
		"type":        "gigacode",
		"device_code": testDeviceCode,
		"extra":       "лишний ключ",
	})
	raw := authRequestJSON(t, map[string]any{
		"AuthID":       "gigacode-test",
		"AuthProvider": "gigacode",
		"StorageJSON":  dirty,
	})
	resp := decodeResult[AuthRefreshResponse](t, core.Handle("auth.refresh", raw))
	// Канонический результат строится из req.StorageJSON: для этого запроса
	// пользовательских ключей нет, поэтому эквивалентно переносу из nil.
	if !equalBytes(resp.Auth.StorageJSON, canonicalStorageJSON(testDeviceCode, dirty)) {
		t.Fatalf("StorageJSON должен быть каноническим: %s", resp.Auth.StorageJSON)
	}
	if strings.Contains(string(resp.Auth.StorageJSON), "лишний ключ") {
		t.Fatal("лишний ключ из запроса попал в ответ")
	}
}

// ---------------------------------------------------- очистка текстов (3.4)

func TestSanitizeText(t *testing.T) {
	in := "первая\nстрока\tс\x1b[31mESC\u200bневидимый\u202eразворот"
	out := sanitizeText(in, 512)
	if strings.ContainsAny(out, "\n\t\r\x1b\u200b\u202e") {
		t.Fatalf("управляющие и невидимые символы не удалены: %q", out)
	}
	if strings.Contains(out, "  ") {
		t.Fatalf("подряд идущие пробелы не схлопнуты: %q", out)
	}
	// Обрезка по байтам не разрезает руны; маркер усечения учитывается в лимите.
	long := strings.Repeat("ы", 2000)
	cut := sanitizeText(long, 10)
	if len(cut) > 10 {
		t.Fatalf("лимит байт превышен: %d", len(cut))
	}
	if !utf8.ValidString(cut) {
		t.Fatalf("строка разрезана посреди руны: %q", cut)
	}
}

// ------------------------------------- сообщения клиенту и журнал (3.5, 3.6)

func TestClientMessagesAndLogDetails(t *testing.T) {
	mock := gigacodemock.New()
	logs := &logRecorder{}
	core := newTestCore(testConfig(mock), logs, nil)

	// Первый запрос успешен: токен и конфиг в кэше.
	raw := executorRequestJSON(storageFor(testDeviceCode), chatPayload(), nil)
	decodeResult[ExecutorResponse](t, core.Handle("executor.execute", raw))

	// Апстрим недоступен: конверт — фиксированный текст без URL,
	// в журнале — детали с URL.
	mock.Close()
	env := expectErr(t, core.Handle("executor.execute", raw), codeTransientTransport, 502)
	if env.Error.Message != msgChatUnavailable {
		t.Fatalf("сообщение: %q, ожидалось %q", env.Error.Message, msgChatUnavailable)
	}
	if strings.Contains(env.Error.Message, "http") {
		t.Fatalf("сообщение клиенту содержит URL: %q", env.Error.Message)
	}
	foundDetail := false
	for _, entry := range logs.all() {
		if strings.Contains(entry.message, "POST http://") {
			foundDetail = true
		}
		// Управляющие символы в журнале недопустимы.
		if strings.ContainsAny(entry.message, "\n\r\x1b") {
			t.Fatalf("журнал содержит управляющие символы: %q", entry.message)
		}
	}
	if !foundDetail {
		t.Fatal("в журнале нет деталей с URL")
	}
}

// canonicalStorageJSON переносит пользовательские ключи из исходного JSON:
// label (непустая строка), prefix, proxy_url; не переносит disabled и
// посторонние ключи.
func TestCanonicalStorageJSONPreservesKeys(t *testing.T) {
	src, _ := json.Marshal(map[string]any{
		"type":        "gigacode",
		"device_code": testDeviceCode,
		"label":       "Моя учётка",
		"prefix":      "gc",
		"proxy_url":   "socks5://127.0.0.1:1080",
		"disabled":    true,
		"посторонний": "лишнее",
	})
	out := canonicalStorageJSON(testDeviceCode, src)

	var storage map[string]any
	if err := json.Unmarshal(out, &storage); err != nil {
		t.Fatalf("результат не JSON: %v", err)
	}
	if storage["type"] != "gigacode" || storage["device_code"] != testDeviceCode {
		t.Fatalf("базовые ключи повреждены: %s", out)
	}
	if storage["label"] != "Моя учётка" || storage["prefix"] != "gc" || storage["proxy_url"] != "socks5://127.0.0.1:1080" {
		t.Fatalf("пользовательские ключи не перенесены: %s", out)
	}
	if _, ok := storage["disabled"]; ok {
		t.Fatal("disabled не должен переноситься: им управляет хост")
	}
	if _, ok := storage["посторонний"]; ok {
		t.Fatal("посторонний ключ не должен переноситься")
	}

	// Ключ другого типа не переносится; пустой label — тоже.
	bad, _ := json.Marshal(map[string]any{
		"device_code": testDeviceCode,
		"label":       42,
		"prefix":      7,
		"proxy_url":   true,
	})
	out2 := canonicalStorageJSON(testDeviceCode, bad)
	var storage2 map[string]any
	if err := json.Unmarshal(out2, &storage2); err != nil {
		t.Fatalf("результат не JSON: %v", err)
	}
	if len(storage2) != 2 {
		t.Fatalf("ключи с не-строками должны игнорироваться: %s", out2)
	}

	// Без исходника — только type и device_code.
	out3 := canonicalStorageJSON(testDeviceCode, nil)
	var storage3 map[string]any
	if err := json.Unmarshal(out3, &storage3); err != nil {
		t.Fatalf("результат не JSON: %v", err)
	}
	if len(storage3) != 2 {
		t.Fatalf("без исходника ожидались только type и device_code: %s", out3)
	}
}

// Ответ auth.refresh сохраняет пользовательские ключи из StorageJSON:
// после перезаписи auth-файла хостом label, prefix и proxy_url переживают
// перезапуск.
func TestAuthRefreshPreservesUserKeys(t *testing.T) {
	mock := gigacodemock.New()
	defer mock.Close()
	core := newTestCore(testConfig(mock), nil, nil)

	dirty, _ := json.Marshal(map[string]any{
		"type":        "gigacode",
		"device_code": testDeviceCode,
		"label":       "Моя учётка",
		"prefix":      "gc",
		"proxy_url":   "socks5://127.0.0.1:1080",
		"disabled":    true,
		"extra":       "лишнее",
	})
	raw := authRequestJSON(t, map[string]any{
		"AuthID":       "gigacode-test",
		"AuthProvider": "gigacode",
		"StorageJSON":  dirty,
	})
	resp := decodeResult[AuthRefreshResponse](t, core.Handle("auth.refresh", raw))

	var storage map[string]any
	if err := json.Unmarshal(resp.Auth.StorageJSON, &storage); err != nil {
		t.Fatalf("StorageJSON не JSON: %v", err)
	}
	if storage["label"] != "Моя учётка" || storage["prefix"] != "gc" || storage["proxy_url"] != "socks5://127.0.0.1:1080" {
		t.Fatalf("пользовательские ключи потеряны: %s", resp.Auth.StorageJSON)
	}
	if _, ok := storage["disabled"]; ok {
		t.Fatal("disabled не должен попадать в StorageJSON")
	}
	if _, ok := storage["extra"]; ok {
		t.Fatal("посторонний ключ не должен попадать в StorageJSON")
	}
}

// ------------------------------------------------- фиксированные тексты (п.4)

// Невалидный base-url: клиенту фиксированный текст без значения,
// значение — только в журнале.
func TestRegisterInvalidBaseURLFixedMessage(t *testing.T) {
	logs := &logRecorder{}
	core := newTestCore(DefaultConfig(), logs, nil)

	reqRaw, _ := json.Marshal(LifecycleRequest{
		ConfigYAML: []byte("base-url: http://gigacode.ru@evil.example/api/v2\n"),
	})
	env := expectErr(t, core.Handle("plugin.register", reqRaw), codeInvalidConfig, 400)
	if env.Error.Message != "base-url имеет недопустимое значение" {
		t.Fatalf("сообщение: %q", env.Error.Message)
	}
	if strings.Contains(env.Error.Message, "evil.example") || strings.Contains(env.Error.Message, "http") {
		t.Fatalf("значение base-url попало в сообщение клиенту: %q", env.Error.Message)
	}
	found := false
	for _, entry := range logs.all() {
		if strings.Contains(entry.message, "evil.example") {
			found = true
		}
	}
	if !found {
		t.Fatal("значения base-url нет в журнале")
	}
}

// Некорректный JSON запроса хоста: клиенту «Некорректный запрос»,
// детали разбора — в журнал уровня warn.
func TestInvalidRequestFixedMessage(t *testing.T) {
	mock := gigacodemock.New()
	defer mock.Close()
	logs := &logRecorder{}
	core := newTestCore(testConfig(mock), logs, nil)

	for _, method := range []string{"auth.parse", "auth.refresh", "executor.execute", "model.for_auth"} {
		t.Run(method, func(t *testing.T) {
			raw := []byte("{ это не json")
			env := expectErr(t, core.Handle(method, raw), codeInvalidRequest, 400)
			if env.Error.Message != "Некорректный запрос" {
				t.Fatalf("сообщение: %q, ожидалось «Некорректный запрос»", env.Error.Message)
			}
			found := false
			for _, entry := range logs.all() {
				if entry.level == "warn" && strings.Contains(entry.message, method) {
					found = true
				}
			}
			if !found {
				t.Fatal("деталей разбора нет в журнале")
			}
		})
	}
}

// exceedTransport — сценарный транспорт без сети: шаги авторизации отвечают
// заготовками, чат — по указанию; применяется для проверки сообщений
// о превышении лимита ответа.
type exceedTransport struct {
	mu      sync.Mutex
	chatErr error // ошибка чата; nil — обычный успешный ответ
}

func (t *exceedTransport) Do(ctx context.Context, req Request, maxBody int64) (*Response, error) {
	t.mu.Lock()
	chatErr := t.chatErr
	t.mu.Unlock()
	switch {
	case req.Method == "POST" && strings.Contains(req.URL, "/chat/completions"):
		if chatErr != nil {
			return nil, chatErr
		}
		return &Response{StatusCode: 200, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: []byte(`{"ok":true}`)}, nil
	case req.Method == "POST":
		// Шаг 2: обмен device_code на JWT.
		return &Response{StatusCode: 200, Headers: nil, Body: []byte(`{"access_token":"jwt-scripted","expires_in":600}`)}, nil
	case firstHeader(req.Headers, "Authorization") != "":
		// Шаг 3: конфиг с моделями.
		return &Response{StatusCode: 200, Headers: nil, Body: []byte(`{"chat":{"models":[{"model":"vllm/Scripted"}],"url":"/chat/completions"}}`)}, nil
	default:
		// Шаг 1: discovery.
		return &Response{StatusCode: 401, Headers: map[string][]string{"X-Authorization-URL": {"https://upstream.test/auth"}}, Body: nil}, nil
	}
}

func (t *exceedTransport) OpenStream(ctx context.Context, req Request) (*StreamResponse, error) {
	return nil, &transportError{cause: errors.New("стрим в этом сценарии не используется")}
}

// Превышение лимита ответа чата: клиенту upstream_error/502 и точная причина.
func TestChatResponseTooLargeMessage(t *testing.T) {
	mock := gigacodemock.New()
	defer mock.Close()
	logs := &logRecorder{}
	core := newTestCore(testConfig(mock), logs, nil)
	core.newTransport = func(string) Transport { return &exceedTransport{chatErr: errResponseTooLarge} }

	raw := executorRequestJSON(storageFor(testDeviceCode), chatPayload(), nil)
	env := expectErr(t, core.Handle("executor.execute", raw), codeUpstreamError, 502)
	if env.Error.Message != msgResponseTooLarge {
		t.Fatalf("сообщение: %q, ожидалось %q", env.Error.Message, msgResponseTooLarge)
	}
}

// Превышение лимита на шаге авторизации: прежнее поведение — временная
// ошибка доступности.
func TestAuthResponseTooLargeStaysAvailability(t *testing.T) {
	core := newTestCore(DefaultConfig(), nil, nil)
	core.newTransport = func(string) Transport {
		return &exceedTransport{chatErr: errResponseTooLarge}
	}
	// Шаг 2 (POST auth_url) отвечает errResponseTooLarge: заменяем URL шага 2
	// через недоступный discovery? Проще напрямую: authTransportErr должен
	// классифицировать превышение как временную ошибку доступности.
	err := &transportError{cause: errResponseTooLarge}
	pe := authTransportErr(err)
	if pe.code != codeTransientTransport || pe.status != 502 || pe.message != msgAuthUnavailable {
		t.Fatalf("классификация auth: %s/%d/%q", pe.code, pe.status, pe.message)
	}
	// И сквозной сценарий: флоу падает на шаге 2 — executor возвращает
	// «Не удалось получить токен GigaCode».
	core2 := newTestCore(DefaultConfig(), nil, nil)
	core2.newTransport = func(string) Transport {
		return &failingPostTransport{}
	}
	raw := executorRequestJSON(storageFor(testDeviceCode), chatPayload(), nil)
	env := expectErr(t, core2.Handle("executor.execute", raw), codeTransientTransport, 502)
	if env.Error.Message != msgAuthUnavailable {
		t.Fatalf("сообщение: %q, ожидалось %q", env.Error.Message, msgAuthUnavailable)
	}
}

// failingPostTransport — шаг 2 авторизации отвечает превышением лимита.
type failingPostTransport struct{}

func (t *failingPostTransport) Do(ctx context.Context, req Request, maxBody int64) (*Response, error) {
	if req.Method == "POST" {
		return nil, errResponseTooLarge
	}
	if firstHeader(req.Headers, "Authorization") != "" {
		return &Response{StatusCode: 200, Body: []byte(`{"chat":{"models":[],"url":""}}`)}, nil
	}
	return &Response{StatusCode: 401, Headers: map[string][]string{"X-Authorization-URL": {"https://upstream.test/auth"}}}, nil
}

func (t *failingPostTransport) OpenStream(ctx context.Context, req Request) (*StreamResponse, error) {
	return nil, &transportError{cause: errors.New("стрим не используется")}
}

// ------------------------------------------------- простой SSE-потока (п.1)

// pacedStream — поток с кадрами через заданный интервал; после frames
// кадров ведёт себя по сценарию (done или зависание).
type pacedStream struct {
	mu        sync.Mutex
	interval  time.Duration
	frames    int
	hangAfter bool
	sent      int
	closed    chan struct{}
	closeOnce sync.Once
}

func (s *pacedStream) Read(ctx context.Context) ([]byte, bool, error) {
	s.mu.Lock()
	sent := s.sent
	s.mu.Unlock()
	if sent >= s.frames && s.hangAfter {
		select {
		case <-s.closed:
			return nil, true, errors.New("поток закрыт")
		case <-ctx.Done():
			return nil, true, ctx.Err()
		case <-time.After(30 * time.Second):
			return nil, true, errors.New("тест завис")
		}
	}
	if sent >= s.frames {
		return nil, true, nil
	}
	select {
	case <-s.closed:
		return nil, true, errors.New("поток закрыт")
	case <-time.After(s.interval):
	}
	s.mu.Lock()
	s.sent++
	n := s.sent
	s.mu.Unlock()
	return []byte(fmt.Sprintf("кадр-%d", n)), false, nil
}

func (s *pacedStream) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	return nil
}

func (s *pacedStream) setMu() *sync.Mutex { return &s.mu }

// Кадры с интервалом меньше лимита простоя: поток доходит до конца без ошибки.
func TestPumpStreamProgressWithinIdleLimit(t *testing.T) {
	mock := gigacodemock.New()
	defer mock.Close()
	logs := &logRecorder{}
	sink := newFakeSink()
	core := newTestCore(testConfig(mock), logs, sink)
	core.cfg.StreamIdleTimeoutSeconds = 1 // лимит 1 с, кадры каждые 100 мс

	stream := &pacedStream{interval: 100 * time.Millisecond, frames: 3, closed: make(chan struct{})}
	counting := &countingStream{inner: stream}

	started := time.Now()
	pumpStream(core, sink, "s-idle-ok", counting, time.Second)
	elapsed := time.Since(started)

	if elapsed > 3*time.Second {
		t.Fatalf("поток занял %v, ожидалось <1с", elapsed)
	}
	frames, _, _, closeMsg := sink.snapshot()
	if len(frames) != 3 {
		t.Fatalf("кадров: %d, ожидалось 3", len(frames))
	}
	if closeMsg != "" {
		t.Fatalf("поток завершился ошибкой: %q", closeMsg)
	}
	for _, entry := range logs.all() {
		if strings.Contains(entry.message, "нет прогресса") {
			t.Fatalf("ложное предупреждение о простое: %q", entry.message)
		}
	}
}

// Апстрим отдал кадр и замолчал: поток закрывается ошибкой примерно за лимит,
// Close вызван ровно один раз.
func TestPumpStreamIdleTimeout(t *testing.T) {
	mock := gigacodemock.New()
	defer mock.Close()
	logs := &logRecorder{}
	sink := newFakeSink()
	core := newTestCore(testConfig(mock), logs, sink)

	stream := &pacedStream{interval: 10 * time.Millisecond, frames: 1, hangAfter: true, closed: make(chan struct{})}
	counting := &countingStream{inner: stream}

	idle := 300 * time.Millisecond
	started := time.Now()
	pumpStream(core, sink, "s-idle-dead", counting, idle)
	elapsed := time.Since(started)

	if elapsed < 200*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("поток завершился за %v, ожидалось ~%v", elapsed, idle)
	}
	frames, _, _, closeMsg := sink.snapshot()
	if len(frames) != 1 {
		t.Fatalf("кадров до простоя: %d, ожидался 1", len(frames))
	}
	if closeMsg != msgStreamBroken {
		t.Fatalf("close-сообщение: %q, ожидалось %q", closeMsg, msgStreamBroken)
	}
	if got := counting.closeCount(); got != 1 {
		t.Fatalf("Close вызван %d раз, ожидался 1", got)
	}
	found := false
	for _, entry := range logs.all() {
		if entry.level == "warn" && strings.Contains(entry.message, "нет прогресса дольше 0 с") {
			found = true
		}
	}
	if !found {
		t.Fatal("нет предупреждения о простое потока")
	}
}

// Дренаж ограничивает кадр оставшимся объёмом: один крупный кадр не
// превышает лимит, чтение прекращается после заполнения.
func TestDrainErrorBodyCapsOversizedChunk(t *testing.T) {
	stream := newSizedChunkStream(bytes.Repeat([]byte("x"), 200<<10))
	counting := &countingStream{inner: stream}

	var gotLen int
	drainErrorBody(context.Background(), counting, func(body []byte) string {
		gotLen = len(body)
		return ""
	})

	if gotLen != maxErrorBodyBytes {
		t.Fatalf("тело дренажа: %d байт, ожидалось ровно %d", gotLen, maxErrorBodyBytes)
	}
	if stream.reads() != 1 {
		t.Fatalf("Read вызван %d раз, ожидался 1 — чтение должно прекратиться на заполнении", stream.reads())
	}
}

// sizedChunkStream — отдаёт один крупный кадр, затем конец.
type sizedChunkStream struct {
	chunk     []byte
	calls     int
	closed    chan struct{}
	closeOnce sync.Once
}

func newSizedChunkStream(chunk []byte) *sizedChunkStream {
	return &sizedChunkStream{chunk: chunk, closed: make(chan struct{})}
}

func (s *sizedChunkStream) Read(ctx context.Context) ([]byte, bool, error) {
	s.calls++
	if s.calls == 1 {
		return s.chunk, false, nil
	}
	return nil, true, nil
}

func (s *sizedChunkStream) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	return nil
}

func (s *sizedChunkStream) reads() int {
	return s.calls
}

// Конверт с неплагинной ошибкой: клиенту фиксированный текст,
// исходные детали не покидают плагин.
func TestErrEnvelopeNonPluginErrorFixedMessage(t *testing.T) {
	env := decodeEnvelope(t, errEnvelope(errors.New("детали: http://host/path и секрет")))
	if env.OK || env.Error == nil {
		t.Fatal("ожидался конверт с ошибкой")
	}
	if env.Error.Message != msgInternal {
		t.Fatalf("сообщение: %q, ожидалось %q", env.Error.Message, msgInternal)
	}
	if strings.Contains(env.Error.Message, "http") || strings.Contains(env.Error.Message, "секрет") {
		t.Fatalf("исходный текст попал в конверт: %q", env.Error.Message)
	}
}

// ------------------------------------------------------- дренаж тела (3.7)

// hangStream — «зависший» поток: Read блокируется до Close.
type hangStream struct {
	mu         sync.Mutex
	closed     chan struct{}
	closeCalls int
}

func newHangStream() *hangStream { return &hangStream{closed: make(chan struct{})} }

func (s *hangStream) Read(ctx context.Context) ([]byte, bool, error) {
	<-s.closed
	return nil, true, errors.New("поток закрыт")
}

func (s *hangStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closeCalls == 0 {
		s.closeCalls++
		close(s.closed)
	}
	return nil
}

// Зависшее чтение завершается по таймеру примерно за 3 секунды, Close — один.
func TestDrainErrorBodyTimeout(t *testing.T) {
	stream := newHangStream()
	counting := &countingStream{inner: stream}

	started := time.Now()
	drainErrorBody(context.Background(), counting, func(body []byte) string { return "" })
	elapsed := time.Since(started)

	if elapsed < 2*time.Second || elapsed > 5*time.Second {
		t.Fatalf("дренаж занял %v, ожидалось ~3с", elapsed)
	}
	if got := counting.closeCount(); got != 1 {
		t.Fatalf("Close вызван %d раз, ожидался 1", got)
	}
}

// ------------------------------------------------- таймаут чтения тела (п.1)

// slowHostHTTP — хост, который зависает: либо на do_stream (до заголовков),
// либо на втором stream_read (после заголовков и части тела). Отмена
// по host.http.cancel «отвисает» вызов, как настоящий хост, который
// закрывает поток операции. openFail имитирует отказ host.http.operation_open:
// "error" — ошибка вызова, "no_id" — ответ без operation_id.
type slowHostHTTP struct {
	mu             sync.Mutex
	hangIn         string // "do_stream" или "stream_read"
	openFail       string // "" — операция открывается штатно
	doStreamResult string // нестандартный сырой ответ do_stream
	cancelled      chan struct{}
	cancelOnce     sync.Once
	closeCount     int
	reads          int
	doStreamCalls  int
	cancelCalls    int
	logWarnCalls   []string
}

func newSlowHostHTTP(hangIn string) *slowHostHTTP {
	return &slowHostHTTP{hangIn: hangIn, cancelled: make(chan struct{})}
}

func (s *slowHostHTTP) hangOrError() ([]byte, error, bool) {
	select {
	case <-s.cancelled:
		// Хост прервал операцию: вызов завершается ошибкой.
		return []byte(`{"ok":false,"error":{"code":"host_call_failed","message":"operation canceled"}}`), nil, true
	case <-time.After(30 * time.Second):
		return nil, errors.New("тест завис: отмена не пришла"), false
	}
}

func (s *slowHostHTTP) call(method string, payload []byte) ([]byte, error) {
	switch method {
	case methodHostHTTPDoStream:
		s.mu.Lock()
		s.doStreamCalls++
		s.mu.Unlock()
		if s.doStreamResult != "" {
			return []byte(s.doStreamResult), nil
		}
		if s.hangIn == "do_stream" {
			raw, err, _ := s.hangOrError()
			if err != nil {
				return nil, err
			}
			return raw, nil
		}
		return []byte(`{"ok":true,"result":{"status_code":200,"headers":{"content-type":["application/json"]},"stream_id":"s1"}}`), nil
	case methodHostHTTPStreamRead:
		s.mu.Lock()
		s.reads++
		position := s.reads
		s.mu.Unlock()
		if s.hangIn == "stream_read" && position > 1 {
			raw, err, _ := s.hangOrError()
			if err != nil {
				return nil, err
			}
			return raw, nil
		}
		if position > 1 {
			return []byte(`{"ok":true,"result":{"done":true}}`), nil
		}
		chunk := []byte(strings.Repeat("a", 50))
		return []byte(fmt.Sprintf(`{"ok":true,"result":{"payload":%q}}`, base64.StdEncoding.EncodeToString(chunk))), nil
	case methodHostHTTPStreamClose:
		s.mu.Lock()
		s.closeCount++
		s.mu.Unlock()
		return []byte(`{"ok":true,"result":{}}`), nil
	case methodHostHTTPOperationOpen:
		switch s.openFail {
		case "error":
			return nil, errors.New("операции отмены недоступны")
		case "bad_json":
			return []byte(`{"ok":true,"result":` + "`"), nil
		case "no_id":
			return []byte(`{"ok":true,"result":{}}`), nil
		}
		return []byte(`{"ok":true,"result":{"operation_id":"op-1"}}`), nil
	case methodHostHTTPCancel:
		s.mu.Lock()
		s.cancelCalls++
		s.mu.Unlock()
		s.cancelOnce.Do(func() { close(s.cancelled) })
		return []byte(`{"ok":true,"result":{}}`), nil
	case methodHostLog:
		var req struct {
			Level   string `json:"level"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(payload, &req)
		s.mu.Lock()
		s.logWarnCalls = append(s.logWarnCalls, req.Level+": "+req.Message)
		s.mu.Unlock()
		return []byte(`{"ok":true,"result":{}}`), nil
	default:
		return []byte(`{"ok":true,"result":{}}`), nil
	}
}

func (s *slowHostHTTP) closeCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeCount
}

func (s *slowHostHTTP) counters() (doStream, cancel int, warns []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.doStreamCalls, s.cancelCalls, append([]string(nil), s.logWarnCalls...)
}

// assertDeadlineErr — ошибка дедлайна.
func assertDeadlineErr(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, errDeadline) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ожидалась ошибка дедлайна, получено: %v", err)
	}
}

// ------------------------------------------------- таймаут чтения тела (п.1)

// Апстрим не отвечает до заголовков: дедлайн прерывает запрос.
func TestDoDeadlineBeforeHeaders(t *testing.T) {
	fake := newSlowHostHTTP("do_stream")
	tr := newHostTransport(fake.call, "cb-1")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := tr.Do(ctx, Request{Method: "GET", URL: "https://upstream/config"}, 64<<10)
	elapsed := time.Since(started)

	assertDeadlineErr(t, err)
	if elapsed > 2*time.Second {
		t.Fatalf("запрос занял %v, ожидалось ~300мс", elapsed)
	}
	if got := fake.closeCalls(); got != 0 {
		t.Fatalf("стрим не открывался, Close вызван %d раз", got)
	}
}

// Апстрим отдал заголовки и часть тела и завис: дедлайн прерывает чтение,
// Close вызван ровно один раз.
func TestDoDeadlineWhileReadingBody(t *testing.T) {
	fake := newSlowHostHTTP("stream_read")
	tr := newHostTransport(fake.call, "cb-1")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := tr.Do(ctx, Request{Method: "GET", URL: "https://upstream/config"}, 64<<10)
	elapsed := time.Since(started)

	assertDeadlineErr(t, err)
	if elapsed > 2*time.Second {
		t.Fatalf("запрос занял %v, ожидалось ~300мс", elapsed)
	}
	if got := fake.closeCalls(); got != 1 {
		t.Fatalf("Close вызван %d раз, ожидался 1", got)
	}
}

// Запрос с дедлайном без механизма отмены не отправляется: operation_open
// не удался — do_stream не вызывается, ошибка классифицируется как
// временная ошибка доступности, отказ записан в журнал уровня warn.
func TestDoDeadlineWithoutCancelMechanism(t *testing.T) {
	for _, openFail := range []string{"error", "bad_json", "no_id"} {
		t.Run(openFail, func(t *testing.T) {
			fake := newSlowHostHTTP("none")
			fake.openFail = openFail
			tr := newHostTransport(fake.call, "cb-1")

			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			_, err := tr.Do(ctx, Request{Method: "GET", URL: "https://upstream/config"}, 64<<10)

			if err == nil || errors.Is(err, errDeadline) {
				t.Fatalf("ожидалась транспортная (не дедлайн) ошибка, получено: %v", err)
			}
			// Классификация для клиента — временная ошибка доступности.
			if got := authTransportErr(err).code; got != codeTransientTransport {
				t.Fatalf("код классификации: %q, ожидался %q", got, codeTransientTransport)
			}
			doStream, cancelCalls, warns := fake.counters()
			if doStream != 0 {
				t.Fatalf("do_stream вызван %d раз, ожидался 0 — запрос не должен был уйти", doStream)
			}
			if cancelCalls != 0 {
				t.Fatalf("cancel вызван %d раз, ожидался 0", cancelCalls)
			}
			found := false
			for _, w := range warns {
				if strings.HasPrefix(w, "warn: ") {
					found = true
				}
			}
			if !found {
				t.Fatalf("отказа уровня warn нет в журнале хоста: %v", warns)
			}
		})
	}
}

// Без дедлайна запрос уходит штатно, операция отмены не открывается.
func TestDoNoDeadlineSkipsOperation(t *testing.T) {
	fake := newSlowHostHTTP("none")
	fake.openFail = "error" // операция недоступна, но дедлайна нет — не важно
	tr := newHostTransport(fake.call, "cb-1")

	resp, err := tr.Do(context.Background(), Request{Method: "GET", URL: "https://upstream/config"}, 64<<10)
	if err != nil {
		t.Fatalf("запрос без дедлайна не должен зависеть от механизма отмены: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("статус: %d", resp.StatusCode)
	}
	if _, cancelCalls, _ := fake.counters(); cancelCalls != 0 {
		t.Fatalf("cancel вызван %d раз, ожидался 0", cancelCalls)
	}
}

// Повреждённый ответ do_stream: операция отменяется, поток закрывается
// по мягкому разбору stream_id; то же для OpenStream.
func TestBrokenDoStreamResponseCleanup(t *testing.T) {
	fake := newSlowHostHTTP("none")
	// result не соответствует hostHTTPStreamResponse, но stream_id извлекаем.
	fake.doStreamResult = `{"ok":true,"result":{"status_code":"200","stream_id":"s-broken"}}`
	tr := newHostTransport(fake.call, "cb-1")

	// Do с дедлайном: операция открыта → должен быть и cancel, и stream_close.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := tr.Do(ctx, Request{Method: "GET", URL: "https://upstream/config"}, 1024)
	if err == nil || errors.Is(err, errDeadline) {
		t.Fatalf("ожидалась ошибка разбора, получено: %v", err)
	}
	if _, cancelCalls, _ := fake.counters(); cancelCalls != 1 {
		t.Fatalf("cancel вызван %d раз, ожидался 1", cancelCalls)
	}
	if got := fake.closeCalls(); got != 1 {
		t.Fatalf("stream_close вызван %d раз, ожидался 1", got)
	}

	// OpenStream без дедлайна: только stream_close.
	if _, err := tr.OpenStream(context.Background(), Request{Method: "GET", URL: "https://upstream/config"}); err == nil {
		t.Fatal("ожидалась ошибка разбора")
	}
	if got := fake.closeCalls(); got != 2 {
		t.Fatalf("stream_close после OpenStream: %d раз, ожидалось 2 суммарно", got)
	}
}
