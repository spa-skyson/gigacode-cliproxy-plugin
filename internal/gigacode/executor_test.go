package gigacode

import (
	"regexp"
	"strings"
	"testing"

	"github.com/spa-skyson/gigacode-cliproxy-plugin/internal/gigacodemock"
)

var uuidRE = regexp.MustCompile(uuidRegexp)

// Тест 4: успешный execute — заголовки апстрима и тело байт-в-байт.
func TestExecutorExecuteSuccess(t *testing.T) {
	mock := gigacodemock.New()
	defer mock.Close()
	core := newTestCore(testConfig(mock), nil, nil)

	payload := chatPayload()
	raw := executorRequestJSON(storageFor(testDeviceCode), payload, nil)
	resp := decodeResult[ExecutorResponse](t, core.Handle("executor.execute", raw))

	req, ok := mock.LastChatRequest()
	if !ok {
		t.Fatal("чат не вызван")
	}
	// Тело запроса к апстриму — Payload байт-в-байт.
	if !equalBytes(req.Body, payload) {
		t.Fatalf("тело запроса апстриму не совпало с Payload: %q", req.Body)
	}
	// Ответ — тело апстрима без изменений.
	if !equalBytes(resp.Payload, []byte(`{"choices":[{"message":{"content":"ok"}}]}`)) {
		t.Fatalf("тело ответа не совпало: %q", resp.Payload)
	}
	token := mock.LastToken()
	if got := headerValue(req.Header, "Authorization"); got != "Bearer "+token {
		t.Fatalf("Authorization=%q, ожидался Bearer %q", got, token)
	}
	sessionID := headerValue(req.Header, "X-Session-Id")
	if !uuidRE.MatchString(sessionID) {
		t.Fatalf("x-session-id не UUID v4: %q", sessionID)
	}
	if !strings.HasPrefix(headerValue(req.Header, "User-Agent"), "GigaCode/") {
		t.Fatalf("User-Agent без префикса GigaCode/: %q", req.Header["User-Agent"])
	}
	if got := headerValue(req.Header, "Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type=%q", got)
	}
	if len(headerValueAll(resp.Headers, "Content-Type")) == 0 {
		t.Fatalf("Content-Type ответа не передан: %v", resp.Headers)
	}
}

// Тест 5: один 401 → переавторизация и успех.
func TestExecutorExecuteRetryAfter401(t *testing.T) {
	mock := gigacodemock.New()
	defer mock.Close()
	core := newTestCore(testConfig(mock), nil, nil)
	mock.FailChat401Once()

	raw := executorRequestJSON(storageFor(testDeviceCode), chatPayload(), nil)
	resp := decodeResult[ExecutorResponse](t, core.Handle("executor.execute", raw))
	if len(resp.Payload) == 0 {
		t.Fatal("пустой ответ после повтора")
	}
	if got := mock.AuthCalls(); got != 2 {
		t.Fatalf("точка авторизации вызвана %d раз, ожидалось 2", got)
	}
	chats := mock.ChatRequests()
	if len(chats) != 2 {
		t.Fatalf("попыток чата: %d, ожидалось 2", len(chats))
	}
	first := headerValue(chats[0].Header, "X-Session-Id")
	second := headerValue(chats[1].Header, "X-Session-Id")
	if first == second {
		t.Fatalf("x-session-id повтора совпал с первой попыткой: %q", first)
	}
	if !uuidRE.MatchString(second) {
		t.Fatalf("x-session-id повтора не UUID v4: %q", second)
	}
}

// Тест 6: два 401 → upstream_auth_failed, 502, ровно две попытки.
func TestExecutorExecuteAlways401(t *testing.T) {
	mock := gigacodemock.New()
	defer mock.Close()
	core := newTestCore(testConfig(mock), nil, nil)
	mock.SetAlways401(true)

	raw := executorRequestJSON(storageFor(testDeviceCode), chatPayload(), nil)
	expectErr(t, core.Handle("executor.execute", raw), codeUpstreamAuthFail, 502)
	if got := mock.ChatCalls(); got != 2 {
		t.Fatalf("попыток чата: %d, ожидалось 2", got)
	}
}

// Тест 7: «устройство не авторизовано» → device_not_authorized, 502.
func TestExecutorExecuteNotAuthorized(t *testing.T) {
	mock := gigacodemock.New()
	defer mock.Close()
	core := newTestCore(testConfig(mock), nil, nil)
	mock.SetSigninError(true)

	raw := executorRequestJSON(storageFor(testDeviceCode), chatPayload(), nil)
	env := expectErr(t, core.Handle("executor.execute", raw), codeDeviceNotAuth, 502)
	if !strings.Contains(env.Error.Message, "не авторизовано") {
		t.Fatalf("текст ошибки без причины: %q", env.Error.Message)
	}
}

// Тест 8: статусы апстрима → коды и статусы конверта.
func TestExecutorExecuteUpstreamStatuses(t *testing.T) {
	cases := []struct {
		status int
		code   string
	}{
		{429, codeUpstreamError},
		{400, codeRequestScoped},
		{500, codeUpstreamError},
	}
	for _, tc := range cases {
		t.Run("", func(t *testing.T) {
			mock := gigacodemock.New()
			defer mock.Close()
			core := newTestCore(testConfig(mock), nil, nil)
			mock.SetChatStatus(tc.status)

			raw := executorRequestJSON(storageFor(testDeviceCode), chatPayload(), nil)
			expectErr(t, core.Handle("executor.execute", raw), tc.code, tc.status)
		})
	}
}

// Тест 10: тело больше лимита → 413, апстрим не вызывался.
func TestExecutorExecuteBodyTooLarge(t *testing.T) {
	mock := gigacodemock.New()
	defer mock.Close()
	cfg := testConfig(mock)
	cfg.MaxBodyBytes = 1024
	core := newTestCore(cfg, nil, nil)

	big := []byte(`{"model":"x","pad":"` + strings.Repeat("п", 4096) + `"}`)
	raw := executorRequestJSON(storageFor(testDeviceCode), big, nil)
	expectErr(t, core.Handle("executor.execute", raw), codeRequestScoped, 413)
	if got := mock.ChatCalls(); got != 0 {
		t.Fatalf("чат вызывался %d раз, ожидался 0", got)
	}
}

// Тест 12: запас обновления — второй запрос сразу после первого
// не выполняет флоу авторизации.
func TestExecutorExecuteTokenReusedWithinMargin(t *testing.T) {
	mock := gigacodemock.New()
	defer mock.Close()
	core := newTestCore(testConfig(mock), nil, nil)

	raw := executorRequestJSON(storageFor(testDeviceCode), chatPayload(), nil)
	decodeResult[ExecutorResponse](t, core.Handle("executor.execute", raw))
	decodeResult[ExecutorResponse](t, core.Handle("executor.execute", raw))

	if got := mock.AuthCalls(); got != 1 {
		t.Fatalf("флоу авторизации выполнен %d раз, ожидался 1", got)
	}
	if got := mock.ChatCalls(); got != 2 {
		t.Fatalf("запросов к чату: %d, ожидалось 2", got)
	}
}

// Тест 4+: чужой AuthProvider → invalid_request.
func TestExecutorExecuteForeignProvider(t *testing.T) {
	mock := gigacodemock.New()
	defer mock.Close()
	core := newTestCore(testConfig(mock), nil, nil)

	raw := executorRequestJSON(storageFor(testDeviceCode), chatPayload(), map[string]any{
		"AuthProvider": "other-provider",
	})
	expectErr(t, core.Handle("executor.execute", raw), codeInvalidRequest, 400)
	if mock.AuthCalls() != 0 {
		t.Fatal("флоу авторизации не должен запускаться для чужого провайдера")
	}
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func headerValueAll(header map[string][]string, name string) []string {
	for key, values := range header {
		if strings.EqualFold(key, name) {
			return values
		}
	}
	return nil
}
