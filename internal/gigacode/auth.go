package gigacode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// Ограничения и константы флоу авторизации.
const (
	// authFlowAttempts — полное число попыток флоу (один повтор при временной ошибке).
	authFlowAttempts = 2
	// defaultExpiresIn — expires_in по умолчанию, когда апстрим не прислал ключ.
	defaultExpiresIn = 600
	// maxExpiresIn — верхняя граница expires_in, секунды.
	maxExpiresIn = 86400
	// minRefreshAfterSeconds — NextRefreshAfter не раньше чем через 1 с.
	minRefreshAfterSeconds = 1
	// backgroundRefreshAfterSeconds — при auth.parse запись отправляется на
	// первое обновление через 2 с, а атрибут refresh_interval_seconds="1"
	// разрешает хосту планировать обновления каждую секунду: для записей без
	// refresh-токена хост обновляет только по этому интервалу, а единица —
	// минимально возможный интервал (нижняя граница времени с прошлого
	// обновления), чтобы не блокировать обновления при коротком expires_in.
	// Ритм обновлений задаёт NextRefreshAfter из ответа auth.refresh.
	backgroundRefreshAfterSeconds = 2
	// refreshIntervalAttribute — атрибут записи с интервалом фоновых
	// обновлений, секунды (см. sdk/cliproxy/auth/conductor_refresh.go хоста).
	refreshIntervalAttribute = "refresh_interval_seconds"

	// discoveryHeader — заголовок ответа discovery с URL авторизации.
	discoveryHeader = "X-Authorization-URL"
	// signinHeader — признак «устройство не авторизовано» (как в app.py).
	signinHeader = "X-Signin-Gitverse"
	// notAuthorizedMessage — фиксированный текст «устройство не авторизовано».
	notAuthorizedMessage = msgNotAuthorized
)

// authFlowResult — результат флоу discovery → auth → config.
type authFlowResult struct {
	Token     string
	ExpiresAt time.Time
	Models    []map[string]any
	ChatURL   string
}

// isNotAuthorized — признак окончательной ошибки «устройство не привязано».
func isNotAuthorized(err error) bool {
	var pe *pluginError
	if errors.As(err, &pe) {
		return pe.code == codeDeviceNotAuth
	}
	return false
}

// runAuthFlow выполняет шаги 1–3; при временной ошибке весь флоу
// повторяется один раз. Окончательная ошибка не повторяется.
func runAuthFlow(ctx context.Context, tr Transport, cfg Config, deviceCode string, log Logger) (authFlowResult, error) {
	var lastErr error
	for attempt := 0; attempt < authFlowAttempts; attempt++ {
		if attempt > 0 {
			logDetail(log, "warn", "Не удалось получить токен GigaCode, повтор: "+lastErr.Error())
		}
		res, err := authFlowAttempt(ctx, tr, cfg, deviceCode, log)
		if err == nil {
			return res, nil
		}
		if isNotAuthorized(err) {
			return authFlowResult{}, err
		}
		lastErr = err
	}
	return authFlowResult{}, lastErr
}

// authFlowAttempt — одна попытка флоу.
func authFlowAttempt(ctx context.Context, tr Transport, cfg Config, deviceCode string, log Logger) (authFlowResult, error) {
	// Шаг 1: discovery — GET {base-url}/config без авторизации.
	authURL, err := discoverAuthURL(ctx, tr, cfg, log)
	if err != nil {
		return authFlowResult{}, err
	}
	// Шаг 2: авторизация — POST {auth_url}.
	token, expiresIn, err := requestToken(ctx, tr, cfg, authURL, deviceCode, log)
	if err != nil {
		return authFlowResult{}, err
	}
	// Шаг 3: конфиг — GET {base-url}/config с Bearer.
	models, chatURL, err := loadConfig(ctx, tr, cfg, token, log)
	if err != nil {
		return authFlowResult{}, err
	}
	if log != nil {
		log("info", fmt.Sprintf("JWT GigaCode обновлён: expires_in=%ds", int64(expiresIn.Seconds())), nil)
		log("info", fmt.Sprintf("Конфиг GigaCode загружен: моделей=%d", len(models)), nil)
	}
	return authFlowResult{
		Token:     token,
		ExpiresAt: time.Now().Add(expiresIn),
		Models:    models,
		ChatURL:   chatURL,
	}, nil
}

// authRequestCtx — контекст одного запроса авторизации с дедлайном
// auth-timeout-seconds из конфига.
func authRequestCtx(parent context.Context, cfg Config) (context.Context, context.CancelFunc) {
	timeout := time.Duration(cfg.AuthTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = time.Duration(defaultAuthTimeoutSeconds) * time.Second
	}
	return context.WithTimeout(parent, timeout)
}

// discoverAuthURL — шаг 1: URL авторизации из заголовка discovery-ответа.
func discoverAuthURL(ctx context.Context, tr Transport, cfg Config, log Logger) (string, error) {
	actx, cancel := authRequestCtx(ctx, cfg)
	defer cancel()
	resp, err := tr.Do(actx, Request{Method: "GET", URL: cfg.BaseURL + "/config"}, maxDiscoveryBodyBytes)
	if err != nil {
		logDetail(log, "warn", "Discovery не удался: GET "+cfg.BaseURL+"/config: "+err.Error())
		return "", authTransportErr(err)
	}
	authURL := strings.TrimSpace(firstHeader(resp.Headers, discoveryHeader))
	if authURL == "" {
		logDetail(log, "warn", fmt.Sprintf("Discovery не удался: GET %s/config → %d, заголовок %s отсутствует", cfg.BaseURL, resp.StatusCode, discoveryHeader))
		return "", authTransientErr()
	}
	if err := validateUpstreamURL(authURL, cfg.AllowInsecureHTTP); err != nil {
		logDetail(log, "warn", "Discovery вернул недопустимый URL авторизации: "+sanitizeText(authURL, 200)+": "+err.Error())
		return "", authTransientErr()
	}
	return authURL, nil
}

// authRequestBody — тело POST {auth_url} (порядок полей как в app.py).
type authRequestBody struct {
	DeviceCode    string `json:"device_code"`
	EnvName       string `json:"env_name"`
	PluginVersion string `json:"plugin_version"`
	PluginName    string `json:"plugin_name"`
	EnvVersion    string `json:"env_version"`
	Login         string `json:"login"`
	Locale        string `json:"locale"`
	PluginType    string `json:"pluginType"`
}

// parseAuthResponse разбирает ответ шага 2: access_token обязателен,
// expires_in — отсутствует (по умолчанию 600) или конечное число
// больше 0 и не больше 86400.
func parseAuthResponse(body []byte) (string, time.Duration, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", 0, authTransientErr()
	}
	token, _ := payload["access_token"].(string)
	if token == "" {
		return "", 0, authTransientErr()
	}
	rawExpires, present := payload["expires_in"]
	if !present {
		return token, defaultExpiresIn * time.Second, nil
	}
	seconds, ok := rawExpires.(float64)
	if !ok || seconds <= 0 || seconds > maxExpiresIn {
		return "", 0, authTransientErr()
	}
	return token, time.Duration(seconds * float64(time.Second)), nil
}

// requestToken — шаг 2: обмен device_code на JWT.
func requestToken(ctx context.Context, tr Transport, cfg Config, authURL, deviceCode string, log Logger) (string, time.Duration, error) {
	body, err := json.Marshal(authRequestBody{
		DeviceCode:    deviceCode,
		EnvName:       cfg.EnvName,
		PluginVersion: cfg.PluginVersion,
		PluginName:    cfg.PluginName,
		EnvVersion:    cfg.EnvVersion,
		Login:         cfg.Login,
		Locale:        cfg.Locale,
		PluginType:    "base",
	})
	if err != nil {
		return "", 0, newErr(codeInternalError, msgInternal, 500)
	}
	actx, cancel := authRequestCtx(ctx, cfg)
	defer cancel()
	resp, err := tr.Do(actx, Request{
		Method:  "POST",
		URL:     authURL,
		Headers: map[string][]string{"Content-Type": {"application/json"}},
		Body:    body,
	}, maxAuthBodyBytes)
	if err != nil {
		logDetail(log, "warn", "Auth не удался: POST "+authURL+": "+err.Error())
		return "", 0, authTransportErr(err)
	}
	if resp.StatusCode != 200 {
		if firstHeader(resp.Headers, signinHeader) != "" {
			return "", 0, newErr(codeDeviceNotAuth, notAuthorizedMessage, 502)
		}
		logDetail(log, "warn", fmt.Sprintf("Auth не удался: POST %s → %d: %s", authURL, resp.StatusCode, shortUpstreamMessage(resp.Body)))
		return "", 0, authTransientErr()
	}
	token, expiresIn, err := parseAuthResponse(resp.Body)
	if err != nil {
		logDetail(log, "warn", "Auth ответил некорректно: тело не содержит access_token или валидный expires_in")
		return "", 0, err
	}
	return token, expiresIn, nil
}

// configResponseBody — ответ шага 3. Элементы models разбираются по одному:
// не-объектные значения пропускаются.
type configResponseBody struct {
	Chat *struct {
		Models []json.RawMessage `json:"models"`
		URL    string            `json:"url"`
	} `json:"chat"`
}

// parseModels превращает элементы chat.models[] в объекты, пропуская мусор.
func parseModels(rawModels []json.RawMessage) []map[string]any {
	models := make([]map[string]any, 0, len(rawModels))
	for _, raw := range rawModels {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil || m == nil {
			continue
		}
		models = append(models, m)
	}
	return models
}

// loadConfig — шаг 3: список моделей и URL чата.
func loadConfig(ctx context.Context, tr Transport, cfg Config, token string, log Logger) ([]map[string]any, string, error) {
	actx, cancel := authRequestCtx(ctx, cfg)
	defer cancel()
	resp, err := tr.Do(actx, Request{
		Method:  "GET",
		URL:     cfg.BaseURL + "/config",
		Headers: map[string][]string{"Authorization": {"Bearer " + token}},
	}, maxConfigBodyBytes)
	if err != nil {
		logDetail(log, "warn", "Конфиг GigaCode не получен: GET "+cfg.BaseURL+"/config: "+err.Error())
		return nil, "", authTransportErr(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		logDetail(log, "warn", fmt.Sprintf("Конфиг GigaCode не получен: GET %s/config → %d: %s", cfg.BaseURL, resp.StatusCode, shortUpstreamMessage(resp.Body)))
		return nil, "", authTransientErr()
	}
	var payload configResponseBody
	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		logDetail(log, "warn", "Конфиг GigaCode не является корректным JSON: "+err.Error())
		return nil, "", authTransientErr()
	}
	models := []map[string]any{}
	chatURL := ""
	if payload.Chat != nil {
		models = parseModels(payload.Chat.Models)
		chatURL = strings.TrimSpace(payload.Chat.URL)
	}
	return models, chatURL, nil
}

// resolveChatURL — правила подстановки chat.url из конфига апстрима.
func resolveChatURL(baseURL, chatURL string) string {
	target := strings.TrimSpace(chatURL)
	if target == "" {
		return baseURL + "/chat/completions"
	}
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		return target
	}
	if !strings.HasPrefix(target, "/") {
		return baseURL + "/" + target
	}
	return baseURL + target
}

// firstHeader — первое значение заголовка без учёта регистра.
func firstHeader(headers map[string][]string, name string) string {
	for key, values := range headers {
		if !strings.EqualFold(key, name) {
			continue
		}
		for _, v := range values {
			if strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

// shortUpstreamMessage — очищенное сообщение из тела ответа апстрима:
// для JSON берётся error.message или message, управляющие символы удаляются,
// результат обрезается до 512 байт.
func shortUpstreamMessage(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var structured struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &structured); err == nil {
		if structured.Error != nil && structured.Error.Message != "" {
			return sanitizeText(structured.Error.Message, 512)
		}
		if structured.Message != "" {
			return sanitizeText(structured.Message, 512)
		}
	}
	return sanitizeText(string(body), 512)
}

// validateUpstreamURL допускает только абсолютные URL с непустым хостом:
// https, либо http для loopback — IP из 127.0.0.0/8 или ::1, буквальное имя
// localhost без учёта регистра. allowInsecure снимает ограничение схемы
// http для любого хоста. Userinfo в URL запрещён: запись вида
// https://gigacode.ru@evil.example/ уводит запрос на другой хост.
// Порт и путь не ограничиваются.
//
// Origin ответа апстрима сознательно не ограничивается: абсолютный chat.url —
// поддерживаемое поведение апстрима (так делает proxy/app.py в resolve_url),
// а подменить ответ апстрима можно только при открытом http.
func validateUpstreamURL(raw string, allowInsecure bool) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return errors.New("URL должен быть абсолютным и содержать хост")
	}
	if parsed.User != nil {
		return errors.New("userinfo в URL не допускается")
	}
	switch parsed.Scheme {
	case "https":
		return nil
	case "http":
		if allowInsecure {
			return nil
		}
		if isLoopbackHost(parsed.Hostname()) {
			return nil
		}
		return errors.New("схема http допустима только для loopback-адресов")
	default:
		return errors.New("допустимы только схемы https и http")
	}
}

// isLoopbackHost проверяет loopback-хост без порта: только буквальное
// имя localhost без учёта регистра (поддомены *.localhost и запись с точкой
// на конце не проходят), а также IP из 127.0.0.0/8 и ::1.
func isLoopbackHost(host string) bool {
	name := strings.ToLower(strings.Trim(host, "[]"))
	if name == "localhost" {
		return true
	}
	ip := net.ParseIP(name)
	return ip != nil && ip.IsLoopback()
}

// ------------------------------------------------------------- auth.* RPC

// handleAuthParse — auth.parse: разбор auth-файла GigaCode.
func handleAuthParse(core *Core, raw []byte) ([]byte, error) {
	var req AuthParseRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		// Детали разбора — только в журнал, клиенту фиксированный текст.
		core.log("warn", "Некорректный запрос auth.parse: "+sanitizeText(err.Error(), 512), nil)
		return nil, newErr(codeInvalidRequest, "Некорректный запрос", 400)
	}
	if req.Provider != providerID {
		// Чужой провайдер: файл не наш.
		return okEnvelope(AuthParseResponse{Handled: false})
	}
	// Сырое тело файла — валидируем device_code и переносим необязательные
	// ключи (disabled, label, prefix, proxy_url); ключ с неожиданным типом
	// значения игнорируется.
	deviceCode, err := parseStorageJSON(req.RawJSON)
	if err != nil {
		return nil, err
	}
	var fileBody map[string]any
	if err := json.Unmarshal(req.RawJSON, &fileBody); err != nil {
		return nil, newErr(codeInvalidAuthFile, "Auth-файл GigaCode не является корректным JSON: "+err.Error(), 502)
	}
	label := pluginLabel
	if userLabel, ok := fileBody["label"].(string); ok && strings.TrimSpace(userLabel) != "" {
		label = userLabel
	}
	var prefix, proxyURL string
	if userPrefix, ok := fileBody["prefix"].(string); ok {
		prefix = userPrefix
	}
	if userProxy, ok := fileBody["proxy_url"].(string); ok {
		proxyURL = userProxy
	}
	disabled := false
	if userDisabled, ok := fileBody["disabled"].(bool); ok {
		disabled = userDisabled
	}
	resp := AuthParseResponse{
		Handled: true,
		Auth: AuthData{
			Provider:         providerID,
			FileName:         req.FileName,
			Label:            label,
			Prefix:           prefix,
			ProxyURL:         proxyURL,
			Disabled:         disabled,
			StorageJSON:      canonicalStorageJSON(deviceCode, req.RawJSON),
			Attributes:       map[string]string{refreshIntervalAttribute: "1"},
			NextRefreshAfter: time.Now().Add(backgroundRefreshAfterSeconds * time.Second),
		},
	}
	return okEnvelope(resp)
}

// handleAuthRefresh — auth.refresh. Ошибки апстрима не отдаются хосту:
// хост разбирает ошибку refresh как отказ учётной записи и может перевести
// её в терминальное состояние (sdk/cliproxy/auth/conductor_refresh.go).
// Конверт с ошибкой — только invalid_auth_file.
func handleAuthRefresh(core *Core, raw []byte) ([]byte, error) {
	var req AuthRefreshRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		core.log("warn", "Некорректный запрос auth.refresh: "+sanitizeText(err.Error(), 512), nil)
		return nil, newErr(codeInvalidRequest, "Некорректный запрос", 400)
	}
	deviceCode, err := parseStorageJSON(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	cfg := core.config()
	tr := core.transportFor(req.HostCallbackID)
	snap, err := core.ensureAuth(context.Background(), tr, cfg, deviceCode, true)
	if err != nil {
		if isNotAuthorized(err) {
			core.log("warn", notAuthorizedMessage, nil)
		} else {
			core.log("warn", fmt.Sprintf("Не удалось обновить токен GigaCode, повтор через %d с: %v", cfg.RetrySeconds, err), nil)
		}
		// Учётная запись не меняется, повтор по расписанию. Label не
		// заполняется: непустое значение перезаписало бы метку, заданную
		// пользователем (хост дополняет пустые поля из существующей записи,
		// см. internal/pluginhost/adapters_executors.go).
		retry := time.Duration(cfg.RetrySeconds) * time.Second
		return okEnvelope(AuthRefreshResponse{
			Auth: AuthData{
				Provider:    providerID,
				ID:          req.AuthID,
				StorageJSON: canonicalStorageJSON(deviceCode, req.StorageJSON),
			},
			NextRefreshAfter: time.Now().Add(retry),
		})
	}
	margin := time.Duration(cfg.RefreshMarginSeconds) * time.Second
	next := snap.ExpiresAt.Add(-margin)
	if min := time.Now().Add(minRefreshAfterSeconds * time.Second); next.Before(min) {
		next = min
	}
	resp := AuthRefreshResponse{
		Auth: AuthData{
			Provider:         providerID,
			ID:               req.AuthID,
			StorageJSON:      canonicalStorageJSON(deviceCode, req.StorageJSON),
			NextRefreshAfter: next,
		},
		NextRefreshAfter: next,
	}
	return okEnvelope(resp)
}

// handleAuthLogin — auth.login.start / auth.login.poll: вход через плагин
// не поддержан, учётная запись задаётся auth-файлом.
func handleAuthLogin(method string) ([]byte, error) {
	return nil, newErr(codeNotImplemented,
		method+": вход через management API не поддержан; учётная запись GigaCode задаётся auth-файлом с device_code", 501)
}

// asPluginErr приводит ошибку к pluginError.
func asPluginErr(err error) (*pluginError, bool) {
	var pe *pluginError
	if errors.As(err, &pe) {
		return pe, true
	}
	return nil, false
}
