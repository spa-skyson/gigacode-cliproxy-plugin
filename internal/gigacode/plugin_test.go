package gigacode

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/spa-skyson/gigacode-cliproxy-plugin/internal/gigacodemock"
)

// Тест 14а: конфиг — значения по умолчанию.
func TestConfigDefaults(t *testing.T) {
	cfg, err := parseConfig(nil)
	if err != nil {
		t.Fatalf("пустой конфиг должен давать значения по умолчанию: %v", err)
	}
	want := DefaultConfig()
	if cfg != want {
		t.Fatalf("конфиг по умолчанию:\n%+v\nожидалось:\n%+v", cfg, want)
	}
	if !strings.HasSuffix(cfg.BaseURL, "/api/v2") || cfg.RefreshMarginSeconds != 90 ||
		cfg.AuthTimeoutSeconds != 30 || cfg.MaxBodyBytes != 33554432 {
		t.Fatalf("значения по умолчанию отличаются: %+v", cfg)
	}
}

// Тест 14б: числовые ключи с нечисловым или отрицательным значением — ошибка.
func TestConfigNumericErrors(t *testing.T) {
	for _, tc := range []struct {
		key   string
		value string
	}{
		{"refresh-margin-seconds", "abc"},
		{"refresh-margin-seconds", "-5"},
		{"auth-timeout-seconds", "скоро"},
		{"auth-timeout-seconds", "-30"},
		{"retry-seconds", "abc"},
		{"retry-seconds", "-30"},
		{"retry-seconds", "0"},
		{"max-body-bytes", "true"},
		{"max-body-bytes", "-1"},
	} {
		t.Run(tc.key+"/"+tc.value, func(t *testing.T) {
			yaml := fmt.Sprintf("%s: %s", tc.key, tc.value)
			_, err := parseConfig([]byte(yaml))
			if err == nil {
				t.Fatalf("ожидалась ошибка для %q", yaml)
			}
			pe, ok := asPluginErr(err)
			if !ok || pe.code != codeInvalidConfig {
				t.Fatalf("код ошибки: %v", err)
			}
		})
	}
}

// Тест 14в: корректный конфиг читается, неизвестные ключи игнорируются.
func TestConfigParseValid(t *testing.T) {
	yaml := `
enabled: true
priority: 1
base-url: "http://127.0.0.1:18090/api/v2/"
plugin-version: "27.0.0"
login: "tester"
refresh-margin-seconds: 60
auth-timeout-seconds: 15
max-body-bytes: 1024
неизвестный-ключ: 5
`
	cfg, err := parseConfig([]byte(yaml))
	if err != nil {
		t.Fatalf("разбор корректного конфига: %v", err)
	}
	if cfg.BaseURL != "http://127.0.0.1:18090/api/v2" {
		t.Fatalf("base-url со слэшем не нормализован: %q", cfg.BaseURL)
	}
	if cfg.PluginVersion != "27.0.0" || cfg.Login != "tester" ||
		cfg.RefreshMarginSeconds != 60 || cfg.AuthTimeoutSeconds != 15 || cfg.MaxBodyBytes != 1024 {
		t.Fatalf("неожиданный конфиг: %+v", cfg)
	}
}

// Тест 14г: plugin.register — метаданные и возможности плагина.
func TestPluginRegister(t *testing.T) {
	mock := gigacodemock.New()
	defer mock.Close()
	core := newTestCore(testConfig(mock), nil, nil)

	reqRaw, _ := json.Marshal(LifecycleRequest{
		ConfigYAML:    []byte("base-url: " + mock.URL() + "/api/v2\n"),
		SchemaVersion: 10,
	})
	resp := decodeResult[Registration](t, core.Handle("plugin.register", reqRaw))

	if resp.SchemaVersion != 1 {
		t.Fatalf("schema_version=%d", resp.SchemaVersion)
	}
	m := resp.Metadata
	if m.Name != "gigacode" || m.Version != "0.1.0" || m.Author != "spa-skyson" ||
		m.GitHubRepository != "https://github.com/spa-skyson/gigacode-cliproxy-plugin" {
		t.Fatalf("метаданные: %+v", m)
	}
	if len(m.ConfigFields) != 13 {
		t.Fatalf("описаний ключей конфига: %d, ожидалось 13", len(m.ConfigFields))
	}
	retryField := ""
	for _, field := range m.ConfigFields {
		if field.Name == "retry-seconds" {
			retryField = field.Type
			// Default приходит из JSON как float64.
			if v, ok := field.Default.(float64); !ok || v != 30 {
				t.Fatalf("retry-seconds Default: %v, ожидалось 30", field.Default)
			}
		}
	}
	if retryField != "integer" {
		t.Fatalf("retry-seconds не описан в ConfigFields (тип %q)", retryField)
	}
	c := resp.Capabilities
	if !c.AuthProvider || !c.Executor || !c.ModelProvider {
		t.Fatalf("возможности: %+v", c)
	}
	if c.ExecutorModelScope != "oauth" {
		t.Fatalf("executor_model_scope=%q", c.ExecutorModelScope)
	}
	if len(c.ExecutorInputFormats) != 1 || c.ExecutorInputFormats[0] != "chat-completions" ||
		len(c.ExecutorOutputFormats) != 1 || c.ExecutorOutputFormats[0] != "chat-completions" {
		t.Fatalf("форматы: %+v / %+v", c.ExecutorInputFormats, c.ExecutorOutputFormats)
	}
}

// Тест 14д: plugin.reconfigure заменяет конфиг; при смене base-url кэш
// моделей и chat.url сбрасывается, токен у старого апстрима живёт.
func TestPluginReconfigure(t *testing.T) {
	mock := gigacodemock.New()
	defer mock.Close()
	core := newTestCore(testConfig(mock), nil, nil)

	raw := executorRequestJSON(storageFor(testDeviceCode), chatPayload(), nil)
	decodeResult[ExecutorResponse](t, core.Handle("executor.execute", raw))
	if mock.AuthCalls() != 1 {
		t.Fatalf("флоу: %d, ожидался 1", mock.AuthCalls())
	}

	// reconfigure с тем же base-url: токен сохраняется.
	reconf, _ := json.Marshal(LifecycleRequest{ConfigYAML: []byte("base-url: " + mock.URL() + "/api/v2\n")})
	decodeResult[Registration](t, core.Handle("plugin.reconfigure", reconf))
	decodeResult[ExecutorResponse](t, core.Handle("executor.execute", raw))
	if mock.AuthCalls() != 1 {
		t.Fatalf("после reconfigure флоу: %d, ожидался 1", mock.AuthCalls())
	}
	if mock.ChatCalls() != 2 {
		t.Fatalf("запросов к чату: %d, ожидалось 2", mock.ChatCalls())
	}

	// reconfigure на другой апстрим: кэш сбрасывается → флоу у нового апстрима.
	mock2 := gigacodemock.New()
	defer mock2.Close()
	reconf2, _ := json.Marshal(LifecycleRequest{ConfigYAML: []byte("base-url: " + mock2.URL() + "/api/v2\n")})
	decodeResult[Registration](t, core.Handle("plugin.reconfigure", reconf2))
	decodeResult[ExecutorResponse](t, core.Handle("executor.execute", raw))
	if mock2.AuthCalls() != 1 {
		t.Fatalf("флоу у нового апстрима: %d, ожидался 1", mock2.AuthCalls())
	}
	if mock2.ChatCalls() != 1 {
		t.Fatalf("запросов к чату нового апстрима: %d, ожидался 1", mock2.ChatCalls())
	}
	if _, ok := mock2.LastChatRequest(); !ok {
		t.Fatal("чат нового апстрима не вызван")
	}
}

// Тест 14е: некорректный конфиг → ошибка из plugin.register.
func TestPluginRegisterBadConfig(t *testing.T) {
	mock := gigacodemock.New()
	defer mock.Close()
	core := newTestCore(testConfig(mock), nil, nil)

	reqRaw, _ := json.Marshal(LifecycleRequest{ConfigYAML: []byte("auth-timeout-seconds: нисколько\n")})
	expectErr(t, core.Handle("plugin.register", reqRaw), codeInvalidConfig, 400)
}

// Тест 14ж: неизвестный метод → unknown_method; quiesce/shutdown — успех.
func TestUnknownMethodAndLifecycle(t *testing.T) {
	mock := gigacodemock.New()
	defer mock.Close()
	core := newTestCore(testConfig(mock), nil, nil)

	expectErr(t, core.Handle("совсем.неизвестно", []byte("{}")), codeUnknownMethod, 404)

	decodeEnvelope(t, core.Handle("plugin.quiesce", []byte("{}")))
	decodeEnvelope(t, core.Handle("plugin.shutdown", []byte("{}")))
}

// Тест 14з: не поддержанные методы → not_implemented.
func TestNotImplemented(t *testing.T) {
	mock := gigacodemock.New()
	defer mock.Close()
	core := newTestCore(testConfig(mock), nil, nil)

	for _, method := range []string{
		"auth.login.start", "auth.login.poll",
		"executor.count_tokens", "executor.http_request",
	} {
		t.Run(method, func(t *testing.T) {
			env := expectErr(t, core.Handle(method, []byte("{}")), codeNotImplemented, 501)
			if method == "auth.login.start" || method == "auth.login.poll" {
				if !strings.Contains(env.Error.Message, "auth-файл") {
					t.Fatalf("в тексте нет причины (auth-файл): %q", env.Error.Message)
				}
			}
		})
	}
}

// Тест 15: гигиена секретов — ни журнал, ни конверты ошибок, ни ответы
// не содержат токен и device_code (кроме by-design StorageJSON в auth.parse).
func TestSecretHygiene(t *testing.T) {
	mock := gigacodemock.New()
	defer mock.Close()
	logs := &logRecorder{}
	sink := newFakeSink()
	core := newTestCore(testConfig(mock), logs, sink)
	mock.SetSSE(true)
	mock.SetSSEDelay(5 * time.Millisecond)
	mock.FailChat401Once()

	var envelopes [][]byte
	fileRaw, _ := json.Marshal(map[string]any{"type": "gigacode", "device_code": testDeviceCode})
	parseEnv := decodeEnvelope(t, core.Handle("auth.parse", authRequestJSON(t, map[string]any{
		"Provider": "gigacode", "FileName": "gigacode.json", "RawJSON": fileRaw,
	})))
	// auth.refresh возвращает тот же StorageJSON, что пришёл в запросе,
	// поэтому его конверт проверяется отдельно от строгого сканирования.
	refreshEnv := decodeEnvelope(t, core.Handle("auth.refresh", authRequestJSON(t, map[string]any{
		"AuthID": "gigacode-test", "AuthProvider": "gigacode", "StorageJSON": storageFor(testDeviceCode),
	})))
	envelopes = append(envelopes, core.Handle("executor.execute", executorRequestJSON(storageFor(testDeviceCode), chatPayload(), nil)))
	envelopes = append(envelopes, core.Handle("executor.execute", executorRequestJSON(storageFor(testDeviceCode), chatPayload(), nil)))
	envelopes = append(envelopes, core.Handle("executor.execute_stream", streamRequestJSON(storageFor(testDeviceCode), chatPayload(), "stream-hygiene")))
	sink.waitClose(t, 10*time.Second)
	envelopes = append(envelopes, core.Handle("model.for_auth", authRequestJSON(t, map[string]any{
		"AuthID": "gigacode-test", "AuthProvider": "gigacode", "StorageJSON": storageFor(testDeviceCode),
	})))
	// Пара ошибок: недоступный апстрим и чужой провайдер.
	mock.Close()
	envelopes = append(envelopes, core.Handle("executor.execute", executorRequestJSON(storageFor(testDeviceCode), chatPayload(), nil)))
	envelopes = append(envelopes, core.Handle("executor.execute", executorRequestJSON(storageFor(testDeviceCode), chatPayload(), map[string]any{"AuthProvider": "other"})))

	// Все выпущенные моком токены и device_code не должны нигде светиться.
	assertNoSecrets(t, logs, envelopes, mock.Tokens(), testDeviceCode)

	// StorageJSON с device_code по дизайну возвращают auth.parse и
	// auth.refresh; всё остальное в их конвертах сканируем строго.
	var parsed AuthParseResponse
	if err := json.Unmarshal(parseEnv.Result, &parsed); err != nil {
		t.Fatalf("разбор auth.parse: %v", err)
	}
	if !parsed.Handled || !strings.Contains(string(parsed.Auth.StorageJSON), testDeviceCode) {
		t.Fatal("auth.parse должен вернуть device_code в StorageJSON — это единственная поверхность")
	}
	var refreshed AuthRefreshResponse
	if err := json.Unmarshal(refreshEnv.Result, &refreshed); err != nil {
		t.Fatalf("разбор auth.refresh: %v", err)
	}
	if !equalBytes(refreshed.Auth.StorageJSON, storageFor(testDeviceCode)) {
		t.Fatal("auth.refresh должен вернуть тот же StorageJSON, что пришёл в запросе")
	}
	// Всё прочее в обоих конвертах — без токенов.
	parseBody := string(parseEnv.Result)
	refreshBody := string(refreshEnv.Result)
	for _, token := range mock.Tokens() {
		if strings.Contains(parseBody, token) || strings.Contains(refreshBody, token) {
			t.Fatalf("токен %q попал в ответ auth.*", token)
		}
	}
	assertNoSecrets(t, logs, envelopes, mock.Tokens(), testDeviceCode)
}
