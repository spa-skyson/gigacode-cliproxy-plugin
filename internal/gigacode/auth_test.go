package gigacode

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/spa-skyson/gigacode-cliproxy-plugin/internal/gigacodemock"
)

// authRequestJSON — запрос auth.parse/auth.refresh для тестов.
func authRequestJSON(t *testing.T, payload map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("маршалинг запроса: %v", err)
	}
	return raw
}

// Тест 1: auth.parse — свой файл → Handled:true, чужой → false,
// невалидный device_code → invalid_auth_file.
func TestAuthParse(t *testing.T) {
	mock := gigacodemock.New()
	defer mock.Close()
	core := newTestCore(testConfig(mock), nil, nil)

	validFile, _ := json.Marshal(map[string]any{
		"type":        "gigacode",
		"device_code": testDeviceCode,
		"extra":       "игнорируется",
	})

	t.Run("свой_файл", func(t *testing.T) {
		raw := authRequestJSON(t, map[string]any{
			"Provider": "gigacode",
			"FileName": "gigacode.json",
			"RawJSON":  validFile,
		})
		resp := decodeResult[AuthParseResponse](t, core.Handle("auth.parse", raw))
		if !resp.Handled {
			t.Fatal("ожидалось Handled:true")
		}
		auth := resp.Auth
		if auth.Provider != "gigacode" || auth.Label != "GigaCode" || auth.FileName != "gigacode.json" {
			t.Fatalf("неожиданные поля Auth: %+v", auth)
		}
		var storage map[string]string
		if err := json.Unmarshal(auth.StorageJSON, &storage); err != nil {
			t.Fatalf("StorageJSON не JSON: %v", err)
		}
		if len(storage) != 2 || storage["type"] != "gigacode" || storage["device_code"] != testDeviceCode {
			t.Fatalf("StorageJSON должен содержать ровно type и device_code: %s", auth.StorageJSON)
		}
		// Фоновое обновление: первое — через ~2 секунды, интервал планировщика — 1 с.
		delay := time.Until(auth.NextRefreshAfter)
		if delay < 1*time.Second || delay > 3*time.Second {
			t.Fatalf("NextRefreshAfter через %v, ожидалось ~2с", delay)
		}
		if len(auth.Attributes) != 1 || auth.Attributes[refreshIntervalAttribute] != "1" {
			t.Fatalf("атрибуты: %v, ожидался %s=1", auth.Attributes, refreshIntervalAttribute)
		}
	})

	t.Run("перенос_необязательных_ключей", func(t *testing.T) {
		fileRaw, _ := json.Marshal(map[string]any{
			"type":        "gigacode",
			"device_code": testDeviceCode,
			"disabled":    true,
			"label":       "Моя учётка",
			"prefix":      "gc",
			"proxy_url":   "socks5://127.0.0.1:1080",
			"unknown":     42,
		})
		raw := authRequestJSON(t, map[string]any{
			"Provider": "gigacode",
			"FileName": "custom.json",
			"RawJSON":  fileRaw,
		})
		resp := decodeResult[AuthParseResponse](t, core.Handle("auth.parse", raw))
		auth := resp.Auth
		if !auth.Disabled {
			t.Fatal("disabled не перенесён")
		}
		if auth.Label != "Моя учётка" {
			t.Fatalf("Label=%q", auth.Label)
		}
		if auth.Prefix != "gc" || auth.ProxyURL != "socks5://127.0.0.1:1080" {
			t.Fatalf("prefix/proxy_url не перенесены: %q / %q", auth.Prefix, auth.ProxyURL)
		}
		// Пользовательские ключи переносятся и в канонический StorageJSON:
		// иначе они стираются при перезаписи auth-файла хостом.
		var storage map[string]any
		if err := json.Unmarshal(auth.StorageJSON, &storage); err != nil {
			t.Fatalf("StorageJSON не JSON: %v", err)
		}
		if storage["label"] != "Моя учётка" || storage["prefix"] != "gc" || storage["proxy_url"] != "socks5://127.0.0.1:1080" {
			t.Fatalf("ключи не перенесены в StorageJSON: %s", auth.StorageJSON)
		}
		if _, ok := storage["disabled"]; ok {
			t.Fatal("disabled не должен попадать в StorageJSON")
		}
	})

	t.Run("ключ_не_того_типа_игнорируется", func(t *testing.T) {
		fileRaw, _ := json.Marshal(map[string]any{
			"type":        "gigacode",
			"device_code": testDeviceCode,
			"label":       123,
			"disabled":    "да",
			"prefix":      7,
			"proxy_url":   true,
		})
		raw := authRequestJSON(t, map[string]any{
			"Provider": "gigacode",
			"FileName": "weird.json",
			"RawJSON":  fileRaw,
		})
		resp := decodeResult[AuthParseResponse](t, core.Handle("auth.parse", raw))
		auth := resp.Auth
		if auth.Disabled {
			t.Fatal("disabled с не-булевым значением должен игнорироваться")
		}
		if auth.Label != "GigaCode" {
			t.Fatalf("Label без валидного ключа = %q, ожидалось GigaCode", auth.Label)
		}
		if auth.Prefix != "" || auth.ProxyURL != "" {
			t.Fatalf("prefix/proxy_url с не-строками должны игнорироваться: %q / %q", auth.Prefix, auth.ProxyURL)
		}
	})

	t.Run("без_label_подставляется_GigaCode", func(t *testing.T) {
		fileRaw, _ := json.Marshal(map[string]any{"type": "gigacode", "device_code": testDeviceCode})
		raw := authRequestJSON(t, map[string]any{
			"Provider": "gigacode",
			"FileName": "plain.json",
			"RawJSON":  fileRaw,
		})
		resp := decodeResult[AuthParseResponse](t, core.Handle("auth.parse", raw))
		if resp.Auth.Label != "GigaCode" {
			t.Fatalf("Label=%q, ожидалось GigaCode", resp.Auth.Label)
		}
	})

	t.Run("чужой_провайдер", func(t *testing.T) {
		raw := authRequestJSON(t, map[string]any{
			"Provider": "other-provider",
			"FileName": "other.json",
			"RawJSON":  validFile,
		})
		resp := decodeResult[AuthParseResponse](t, core.Handle("auth.parse", raw))
		if resp.Handled {
			t.Fatal("чужой файл не должен распознаваться")
		}
	})

	t.Run("невалидный_device_code", func(t *testing.T) {
		for name, bad := range map[string]any{
			"не_hex":      map[string]any{"type": "gigacode", "device_code": strings.Repeat("z", 64)},
			"короче":      map[string]any{"type": "gigacode", "device_code": "abc"},
			"пустой":      map[string]any{"type": "gigacode", "device_code": "  "},
			"отсутствует": map[string]any{"type": "gigacode"},
			"не_строка":   map[string]any{"type": "gigacode", "device_code": 123},
			"пустой_файл": map[string]any{},
		} {
			t.Run(name, func(t *testing.T) {
				fileRaw, _ := json.Marshal(bad)
				raw := authRequestJSON(t, map[string]any{
					"Provider": "gigacode",
					"FileName": "bad.json",
					"RawJSON":  fileRaw,
				})
				expectErr(t, core.Handle("auth.parse", raw), codeInvalidAuthFile, 502)
			})
		}
	})
}

// Тест 2: auth.refresh — полный флоу; NextRefreshAfter = истечение − margin;
// временная ошибка и «не авторизовано» дают успех с повтором через retry-seconds
// (хост разбирает ошибку как отказ учётной записи — плагин сам восстанавливается).
func TestAuthRefresh(t *testing.T) {
	t.Run("успех", func(t *testing.T) {
		mock := gigacodemock.New()
		defer mock.Close()
		core := newTestCore(testConfig(mock), nil, nil)

		storage := storageFor(testDeviceCode)
		raw := authRequestJSON(t, map[string]any{
			"AuthID":       "gigacode-test",
			"AuthProvider": "gigacode",
			"StorageJSON":  storage,
		})
		before := time.Now()
		resp := decodeResult[AuthRefreshResponse](t, core.Handle("auth.refresh", raw))
		if mock.AuthCalls() != 1 {
			t.Fatalf("ожидался один флоу авторизации, было %d", mock.AuthCalls())
		}
		// expires_in 600, margin 90 → NextRefreshAfter ≈ сейчас + 510 с (допуск 1 с).
		delay := resp.NextRefreshAfter.Sub(before).Seconds()
		if delay < 509 || delay > 511 {
			t.Fatalf("NextRefreshAfter через %v с, ожидалось ≈510", delay)
		}
		// Строго в будущем, иначе планировщик хоста пропустит обновление.
		if !resp.NextRefreshAfter.After(before) {
			t.Fatal("NextRefreshAfter должен быть в будущем")
		}
		// Непустой Auth: Provider, тот же StorageJSON, тот же NextRefreshAfter.
		if resp.Auth.Provider != "gigacode" || resp.Auth.ID != "gigacode-test" {
			t.Fatalf("неожиданный Auth: %+v", resp.Auth)
		}
		// Label не возвращается: иначе хост перезаписал бы метку пользователя.
		if resp.Auth.Label != "" {
			t.Fatalf("auth.refresh не должен возвращать Label, получено %q", resp.Auth.Label)
		}
		if !equalBytes(resp.Auth.StorageJSON, storage) {
			t.Fatalf("StorageJSON должен вернуться без изменений: %s", resp.Auth.StorageJSON)
		}
		if !resp.Auth.NextRefreshAfter.Equal(resp.NextRefreshAfter) {
			t.Fatalf("NextRefreshAfter в Auth (%v) и на верхнем уровне (%v) расходятся",
				resp.Auth.NextRefreshAfter, resp.NextRefreshAfter)
		}
	})

	t.Run("временная_ошибка_успех_с_повтором", func(t *testing.T) {
		mock := gigacodemock.New()
		defer mock.Close()
		cfg := testConfig(mock)
		// Апстрим на мёртвом порту: обе попытки флоу упадут временной ошибкой.
		cfg.BaseURL = "http://127.0.0.1:1/api/v2"
		core := newTestCore(cfg, nil, nil)

		raw := authRequestJSON(t, map[string]any{
			"AuthID":       "gigacode-test",
			"AuthProvider": "gigacode",
			"StorageJSON":  storageFor(testDeviceCode),
		})
		before := time.Now()
		resp := decodeResult[AuthRefreshResponse](t, core.Handle("auth.refresh", raw))
		delay := resp.NextRefreshAfter.Sub(before).Seconds()
		if delay < 29 || delay > 31 {
			t.Fatalf("при временной ошибке NextRefreshAfter через %v с, ожидалось ≈30 (retry-seconds)", delay)
		}
		// Данные учётной записи не меняются.
		if resp.Auth.Provider == "" {
			t.Fatal("Provider должен остаться заполненным")
		}
	})

	t.Run("устройство_не_авторизовано_успех_с_повтором", func(t *testing.T) {
		mock := gigacodemock.New()
		defer mock.Close()
		logs := &logRecorder{}
		core := newTestCore(testConfig(mock), logs, nil)
		mock.SetSigninError(true)

		raw := authRequestJSON(t, map[string]any{
			"AuthID":       "gigacode-test",
			"AuthProvider": "gigacode",
			"StorageJSON":  storageFor(testDeviceCode),
		})
		before := time.Now()
		resp := decodeResult[AuthRefreshResponse](t, core.Handle("auth.refresh", raw))
		delay := resp.NextRefreshAfter.Sub(before).Seconds()
		if delay < 29 || delay > 31 {
			t.Fatalf("при отказе устройства NextRefreshAfter через %v с, ожидалось ≈30 (retry-seconds)", delay)
		}
		// В журнале — предупреждение с текстом для пользователя.
		found := false
		for _, entry := range logs.all() {
			if entry.level == "warn" && strings.Contains(entry.message, "не авторизовано") {
				found = true
			}
		}
		if !found {
			t.Fatal("нет warn-записи «устройство не авторизовано»")
		}
		assertNoSecrets(t, logs, nil, mock.Tokens(), testDeviceCode)
	})
}
