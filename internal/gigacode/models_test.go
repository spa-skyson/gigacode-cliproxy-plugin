package gigacode

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/spa-skyson/gigacode-cliproxy-plugin/internal/gigacodemock"
)

// Тест 13: model.for_auth — модели из конфига (мусор пропущен),
// чужой провайдер → пустой список.
func TestModelForAuth(t *testing.T) {
	mock := gigacodemock.New()
	defer mock.Close()
	core := newTestCore(testConfig(mock), nil, nil)

	mock.SetModels([]any{
		map[string]any{"model": "vllm/CodeAgent", "name": "Code Agent", "description": "агент"},
		"строка вместо объекта",
		42,
		nil,
		map[string]any{"model": ""},
		map[string]any{},
		map[string]any{"model": "vllm/Fast"},
	})

	raw := authRequestJSON(t, map[string]any{
		"AuthID":       "gigacode-test",
		"AuthProvider": "gigacode",
		"StorageJSON":  storageFor(testDeviceCode),
	})
	resp := decodeResult[ModelResponse](t, core.Handle("model.for_auth", raw))
	if resp.Provider != "gigacode" {
		t.Fatalf("Provider=%q", resp.Provider)
	}
	if len(resp.Models) != 2 {
		t.Fatalf("моделей: %d, ожидалось 2 (%v)", len(resp.Models), resp.Models)
	}
	first := resp.Models[0]
	if first.ID != "vllm/CodeAgent" || first.Object != "model" || first.OwnedBy != "gigacode" {
		t.Fatalf("поля модели: %+v", first)
	}
	if first.DisplayName != "Code Agent" || first.Description != "агент" {
		t.Fatalf("отображение модели: %+v", first)
	}
	if len(first.SupportedGenerationMethods) != 1 || first.SupportedGenerationMethods[0] != "chat" {
		t.Fatalf("методы генерации: %v", first.SupportedGenerationMethods)
	}
	if !first.UserDefined {
		t.Fatal("UserDefined должен быть true")
	}
	if resp.Models[1].ID != "vllm/Fast" || resp.Models[1].DisplayName != "vllm/Fast" {
		t.Fatalf("вторая модель: %+v", resp.Models[1])
	}
}

// Тест 13+: чужой провайдер — пустой список, флоу не запускается.
func TestModelForAuthForeignProvider(t *testing.T) {
	mock := gigacodemock.New()
	defer mock.Close()
	core := newTestCore(testConfig(mock), nil, nil)

	raw := authRequestJSON(t, map[string]any{
		"AuthID":       "other",
		"AuthProvider": "other-provider",
		"StorageJSON":  storageFor(testDeviceCode),
	})
	resp := decodeResult[ModelResponse](t, core.Handle("model.for_auth", raw))
	if len(resp.Models) != 0 {
		t.Fatalf("ожидался пустой список, получено %d", len(resp.Models))
	}
	if mock.AuthCalls() != 0 {
		t.Fatal("флоу авторизации не должен запускаться")
	}
}

// Тест 13+: при свежем сеансе в менеджере токенов model.for_auth отдаёт
// кэш без обращений в сеть.
func TestModelForAuthUsesCache(t *testing.T) {
	mock := gigacodemock.New()
	defer mock.Close()
	mock.SetModels([]any{
		map[string]any{"model": "vllm/CodeAgent", "name": "Code Agent", "description": "агент"},
		map[string]any{"model": "vllm/Fast"},
	})
	core := newTestCore(testConfig(mock), nil, nil)

	reqRaw := authRequestJSON(t, map[string]any{
		"AuthID":       "gigacode-test",
		"AuthProvider": "gigacode",
		"StorageJSON":  storageFor(testDeviceCode),
	})
	// Прогрев: первый вызов выполняет флоу.
	first := decodeResult[ModelResponse](t, core.Handle("model.for_auth", reqRaw))
	if len(first.Models) != 2 {
		t.Fatalf("моделей после прогрева: %d, ожидалось 2", len(first.Models))
	}
	requestsAfterWarmup := len(mock.Requests())
	if mock.AuthCalls() != 1 {
		t.Fatalf("флоу после прогрева: %d, ожидался 1", mock.AuthCalls())
	}

	// Второй вызов: сеть молчит, модели из кэша.
	second := decodeResult[ModelResponse](t, core.Handle("model.for_auth", reqRaw))
	if len(second.Models) != 2 {
		t.Fatalf("моделей из кэша: %d, ожидалось 2", len(second.Models))
	}
	if got := len(mock.Requests()); got != requestsAfterWarmup {
		t.Fatalf("при свежем сеансе были обращения в сеть: было записей %d, стало %d", requestsAfterWarmup, got)
	}
	if mock.AuthCalls() != 1 {
		t.Fatalf("флоу не должен повторяться: %d", mock.AuthCalls())
	}
}

// Тест 13+: неудача флоу — успешный конверт с пустым списком моделей.
func TestModelForAuthFlowFailureEmptyList(t *testing.T) {
	mock := gigacodemock.New()
	defer mock.Close()
	cfg := testConfig(mock)
	cfg.BaseURL = "http://127.0.0.1:1/api/v2" // апстрим недоступен
	core := newTestCore(cfg, nil, nil)

	reqRaw := authRequestJSON(t, map[string]any{
		"AuthID":       "gigacode-test",
		"AuthProvider": "gigacode",
		"StorageJSON":  storageFor(testDeviceCode),
	})
	resp := decodeResult[ModelResponse](t, core.Handle("model.for_auth", reqRaw))
	if len(resp.Models) != 0 {
		t.Fatalf("при неудаче флоу ожидался пустой список, получено %d", len(resp.Models))
	}
}

// Тест 13+: model.static — пустой статический каталог.
func TestModelStatic(t *testing.T) {
	mock := gigacodemock.New()
	defer mock.Close()
	core := newTestCore(testConfig(mock), nil, nil)

	raw := authRequestJSON(t, map[string]any{
		"Plugin": map[string]any{"Name": "gigacode"},
	})
	resp := decodeResult[ModelResponse](t, core.Handle("model.static", raw))
	if resp.Provider != "gigacode" || len(resp.Models) != 0 {
		t.Fatalf("model.static должен вернуть пустой список gigacode: %+v", resp)
	}
}

// Вспомогательное: StorageJSON в model.for_auth не обязан быть каноничным —
// сервер отдаёт то, что вернул auth.parse.
func TestModelForAuthWithCanonicalStorage(t *testing.T) {
	mock := gigacodemock.New()
	defer mock.Close()
	core := newTestCore(testConfig(mock), nil, nil)

	// Каноничный StorageJSON из auth.parse.
	parseFile, _ := json.Marshal(map[string]any{"type": "gigacode", "device_code": testDeviceCode})
	parseReq := authRequestJSON(t, map[string]any{
		"Provider": "gigacode",
		"FileName": "gigacode.json",
		"RawJSON":  parseFile,
	})
	parsed := decodeResult[AuthParseResponse](t, core.Handle("auth.parse", parseReq))

	forAuth := authRequestJSON(t, map[string]any{
		"AuthID":       "gigacode-test",
		"AuthProvider": "gigacode",
		"StorageJSON":  parsed.Auth.StorageJSON,
	})
	resp := decodeResult[ModelResponse](t, core.Handle("model.for_auth", forAuth))
	if len(resp.Models) == 0 {
		t.Fatal("модели не найдены по каноничному StorageJSON")
	}
	if !strings.Contains(mock.URL(), "127.0.0.1") {
		t.Fatalf("мок на неожиданном адресе: %s", mock.URL())
	}
}
