package gigacode

import (
	"strings"
	"testing"
	"time"

	"github.com/spa-skyson/gigacode-cliproxy-plugin/internal/gigacodemock"
)

// Тест 9а: апстрим недоступен → transient_transport, 502.
func TestExecutorExecuteUpstreamDown(t *testing.T) {
	mock := gigacodemock.New()
	core := newTestCore(testConfig(mock), nil, nil)
	mock.Close() // соединение отклоняется

	raw := executorRequestJSON(storageFor(testDeviceCode), chatPayload(), nil)
	expectErr(t, core.Handle("executor.execute", raw), codeTransientTransport, 502)
}

// Тест 9б: авторизация «зависает» → upstream_timeout, 504,
// общее время не больше двух таймаутов с запасом.
func TestExecutorExecuteAuthTimeout(t *testing.T) {
	mock := gigacodemock.New()
	defer mock.Close()
	cfg := testConfig(mock)
	cfg.AuthTimeoutSeconds = 1 // запрос авторизации живёт 1 с
	core := newTestCore(cfg, nil, nil)
	mock.SetHangAuth(true)
	defer mock.SetHangAuth(false)

	raw := executorRequestJSON(storageFor(testDeviceCode), chatPayload(), nil)
	started := time.Now()
	env := expectErr(t, core.Handle("executor.execute", raw), codeUpstreamTimeout, 504)
	elapsed := time.Since(started)

	// Флоу повторяется один раз: максимум 2 запроса по 1 с + запас.
	if elapsed > 4*time.Second {
		t.Fatalf("таймаут занял %v, ожидалось не больше ~2 с с запасом", elapsed)
	}
	if env.Error.Message == "" {
		t.Fatal("текст ошибки пуст")
	}
	// Гигиена: ни один выпущенный токен не попал в текст ошибки.
	for _, token := range mock.Tokens() {
		if strings.Contains(env.Error.Message, token) {
			t.Fatalf("токен %q попал в текст ошибки", token)
		}
	}
}
