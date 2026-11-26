package gigacode

import (
	"fmt"
	"os"
	"os/user"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config — конфиг плагина из блока plugins.configs.gigacode.
type Config struct {
	// BaseURL — базовый URL апстрима.
	BaseURL string
	// EnvName — поле env_name в теле авторизации.
	EnvName string
	// PluginName — поле plugin_name.
	PluginName string
	// PluginVersion — поле plugin_version; входит в User-Agent.
	PluginVersion string
	// EnvVersion — поле env_version.
	EnvVersion string
	// Locale — поле locale.
	Locale string
	// Login — поле login (по умолчанию — имя пользователя ОС).
	Login string
	// RefreshMarginSeconds — запас, при котором токен считается устаревшим.
	RefreshMarginSeconds int
	// AuthTimeoutSeconds — таймаут каждого запроса авторизации.
	AuthTimeoutSeconds int
	// RetrySeconds — интервал повтора фонового обновления токена после
	// ошибки; внутри одной попытки шаги авторизации повторяются сразу.
	RetrySeconds int
	// StreamIdleTimeoutSeconds — предел простоя SSE-потока: если прогресса
	// нет дольше этого времени, HTTP-стрим апстрима закрывается.
	StreamIdleTimeoutSeconds int
	// MaxBodyBytes — лимит тела запроса к модели.
	MaxBodyBytes int
	// AllowInsecureHTTP — разрешить открытый http к любому хосту.
	AllowInsecureHTTP bool
}

// Значения по умолчанию.
const (
	defaultBaseURL              = "https://gigacode.ru/api/v2"
	defaultEnvName              = "vscode"
	defaultPluginName           = "vscode-kilo-plugin"
	defaultPluginVersion        = "26.9.66"
	defaultEnvVersion           = "1.105.1"
	defaultLocale               = "ru"
	defaultRefreshMarginSeconds = 90
	defaultAuthTimeoutSeconds   = 30
	defaultRetrySeconds         = 30
	// defaultStreamIdleTimeoutSeconds — read-таймаут SSE-потока. Отступление
	// от правила хоста «после установки соединения таймаутов нет»: это
	// соответствует read-таймауту 600 секунд в proxy/app.py (build_client) —
	// апстрим, замолчавший посреди потока, обязан освобождать ресурсы.
	defaultStreamIdleTimeoutSeconds = 600
	defaultMaxBodyBytes             = 32 << 20 // 33554432
)

// defaultLogin — имя пользователя ОС с запасными вариантами.
func defaultLogin() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if v := os.Getenv("USER"); v != "" {
		return v
	}
	return "unknown"
}

// Диапазоны числовых ключей конфига: [мин, макс].
var (
	rangeRefreshMargin = [2]int{0, 86400}
	rangeAuthTimeout   = [2]int{1, 600}
	rangeRetrySeconds  = [2]int{1, 3600}
	rangeStreamIdle    = [2]int{1, 86400}
	rangeMaxBodyBytes  = [2]int{1, 1 << 30} // 1073741824
)

// DefaultConfig — конфиг со значениями по умолчанию.
func DefaultConfig() Config {
	return Config{
		BaseURL:                  defaultBaseURL,
		EnvName:                  defaultEnvName,
		PluginName:               defaultPluginName,
		PluginVersion:            defaultPluginVersion,
		EnvVersion:               defaultEnvVersion,
		Locale:                   defaultLocale,
		Login:                    defaultLogin(),
		RefreshMarginSeconds:     defaultRefreshMarginSeconds,
		AuthTimeoutSeconds:       defaultAuthTimeoutSeconds,
		RetrySeconds:             defaultRetrySeconds,
		StreamIdleTimeoutSeconds: defaultStreamIdleTimeoutSeconds,
		MaxBodyBytes:             defaultMaxBodyBytes,
	}
}

// parseConfig разбирает YAML-блок plugins.configs.gigacode.
// Неизвестные ключи игнорируются; числовой ключ с нечисловым значением,
// значением вне диапазона или отрицательным — ошибка регистрации.
func parseConfig(data []byte) (Config, error) {
	cfg := DefaultConfig()
	if len(strings.TrimSpace(string(data))) == 0 {
		return cfg, nil
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return cfg, newErr(codeInvalidConfig, fmt.Sprintf("Некорректный YAML конфига gigacode: %v", err), 400)
	}
	str := func(key string, dst *string) error {
		v, ok := raw[key]
		if !ok || v == nil {
			return nil
		}
		switch val := v.(type) {
		case string:
			if strings.TrimSpace(val) != "" {
				*dst = val
			}
		case int:
			*dst = strconv.Itoa(val)
		case float64:
			*dst = strconv.FormatFloat(val, 'f', -1, 64)
		default:
			return newErr(codeInvalidConfig, fmt.Sprintf("Ключ %q конфига gigacode: ожидалась строка, получено %T", key, v), 400)
		}
		return nil
	}
	boolean := func(key string, dst *bool) error {
		v, ok := raw[key]
		if !ok || v == nil {
			return nil
		}
		val, ok := v.(bool)
		if !ok {
			return newErr(codeInvalidConfig, fmt.Sprintf("Ключ %q конфига gigacode: ожидалось булево значение, получено %T", key, v), 400)
		}
		*dst = val
		return nil
	}
	num := func(key string, dst *int, bounds [2]int) error {
		v, ok := raw[key]
		if !ok || v == nil {
			return nil
		}
		n, err := configNumber(key, v, bounds)
		if err != nil {
			return err
		}
		*dst = n
		return nil
	}
	keys := []string{
		"base-url", "env-name", "plugin-name", "plugin-version", "env-version",
		"locale", "login", "refresh-margin-seconds", "auth-timeout-seconds",
		"retry-seconds", "stream-idle-timeout-seconds", "max-body-bytes", "allow-insecure-http",
	}
	for _, key := range keys {
		var err error
		switch key {
		case "base-url":
			err = str(key, &cfg.BaseURL)
		case "env-name":
			err = str(key, &cfg.EnvName)
		case "plugin-name":
			err = str(key, &cfg.PluginName)
		case "plugin-version":
			err = str(key, &cfg.PluginVersion)
		case "env-version":
			err = str(key, &cfg.EnvVersion)
		case "locale":
			err = str(key, &cfg.Locale)
		case "login":
			err = str(key, &cfg.Login)
		case "refresh-margin-seconds":
			err = num(key, &cfg.RefreshMarginSeconds, rangeRefreshMargin)
		case "auth-timeout-seconds":
			err = num(key, &cfg.AuthTimeoutSeconds, rangeAuthTimeout)
		case "retry-seconds":
			err = num(key, &cfg.RetrySeconds, rangeRetrySeconds)
		case "stream-idle-timeout-seconds":
			err = num(key, &cfg.StreamIdleTimeoutSeconds, rangeStreamIdle)
		case "max-body-bytes":
			err = num(key, &cfg.MaxBodyBytes, rangeMaxBodyBytes)
		case "allow-insecure-http":
			err = boolean(key, &cfg.AllowInsecureHTTP)
		}
		if err != nil {
			return cfg, err
		}
	}
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if err := validateUpstreamURL(cfg.BaseURL, cfg.AllowInsecureHTTP); err != nil {
		// Значение base-url — в журнал вызывающим; клиенту фиксированный текст.
		return cfg, newErr(codeInvalidConfig, "base-url имеет недопустимое значение", 400)
	}
	return cfg, nil
}

// configNumber — строгое чтение числового значения конфига с проверкой
// диапазона до преобразования в int.
func configNumber(key string, v any, bounds [2]int) (int, error) {
	tooBig := func(value string) error {
		return newErr(codeInvalidConfig,
			fmt.Sprintf("Ключ %q конфига gigacode: допустимы целые от %d до %d, получено %s", key, bounds[0], bounds[1], value), 400)
	}
	wrongType := func() error {
		return newErr(codeInvalidConfig,
			fmt.Sprintf("Ключ %q конфига gigacode: ожидалось число, получено значение типа %T", key, v), 400)
	}
	switch val := v.(type) {
	case int:
		if val < bounds[0] || val > bounds[1] {
			return 0, tooBig(strconv.Itoa(val))
		}
		return val, nil
	case int64:
		if val < int64(bounds[0]) || val > int64(bounds[1]) {
			return 0, tooBig(strconv.FormatInt(val, 10))
		}
		return int(val), nil
	case uint64:
		if val > uint64(bounds[1]) {
			return 0, tooBig(strconv.FormatUint(val, 10))
		}
		return int(val), nil
	case float64:
		if val != float64(int64(val)) {
			return 0, wrongType()
		}
		if val < float64(bounds[0]) || val > float64(bounds[1]) {
			return 0, tooBig(strconv.FormatFloat(val, 'f', -1, 64))
		}
		return int(val), nil
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(val))
		if err != nil {
			return 0, wrongType()
		}
		if n < bounds[0] || n > bounds[1] {
			return 0, tooBig(strconv.Itoa(n))
		}
		return n, nil
	default:
		return 0, wrongType()
	}
}

// configFields — описания ключей конфига для реестра плагинов.
func configFields() []ConfigField {
	return []ConfigField{
		{Name: "base-url", Type: "string", Default: defaultBaseURL, Description: "Базовый URL апстрима GigaCode"},
		{Name: "env-name", Type: "string", Default: defaultEnvName, Description: "Поле env_name в теле авторизации"},
		{Name: "plugin-name", Type: "string", Default: defaultPluginName, Description: "Поле plugin_name в теле авторизации"},
		{Name: "plugin-version", Type: "string", Default: defaultPluginVersion, Description: "Поле plugin_version; входит в User-Agent"},
		{Name: "env-version", Type: "string", Default: defaultEnvVersion, Description: "Поле env_version в теле авторизации"},
		{Name: "locale", Type: "string", Default: defaultLocale, Description: "Поле locale в теле авторизации"},
		{Name: "login", Type: "string", Default: defaultLogin(), Description: "Поле login в теле авторизации"},
		{Name: "refresh-margin-seconds", Type: "integer", Default: defaultRefreshMarginSeconds, Description: "Запас в секундах, при котором JWT считается устаревшим"},
		{Name: "auth-timeout-seconds", Type: "integer", Default: defaultAuthTimeoutSeconds, Description: "Таймаут каждого запроса авторизации, секунды"},
		{Name: "retry-seconds", Type: "integer", Default: defaultRetrySeconds, Description: "Интервал повтора фонового обновления токена после ошибки; внутри одной попытки шаги авторизации повторяются сразу"},
		{Name: "stream-idle-timeout-seconds", Type: "integer", Default: defaultStreamIdleTimeoutSeconds, Description: "Предел простоя SSE-потока без прогресса, секунды"},
		{Name: "max-body-bytes", Type: "integer", Default: defaultMaxBodyBytes, Description: "Лимит тела запроса к модели, байты"},
		{Name: "allow-insecure-http", Type: "boolean", Default: false, Description: "Разрешить открытый http к любому хосту (по умолчанию — только loopback)"},
	}
}
