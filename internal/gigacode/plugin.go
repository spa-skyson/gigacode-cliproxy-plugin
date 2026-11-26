package gigacode

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// Имена методов RPC (sdk/pluginabi/types.go).
const (
	methodPluginRegister    = "plugin.register"
	methodPluginReconfigure = "plugin.reconfigure"
	methodPluginQuiesce     = "plugin.quiesce"
	methodPluginShutdown    = "plugin.shutdown"

	methodAuthIdentifier = "auth.identifier"
	methodAuthParse      = "auth.parse"
	methodAuthLoginStart = "auth.login.start"
	methodAuthLoginPoll  = "auth.login.poll"
	methodAuthRefresh    = "auth.refresh"

	methodModelStatic  = "model.static"
	methodModelForAuth = "model.for_auth"

	methodExecutorIdentifier    = "executor.identifier"
	methodExecutorExecute       = "executor.execute"
	methodExecutorExecuteStream = "executor.execute_stream"
	methodExecutorCountTokens   = "executor.count_tokens"
	methodExecutorHTTPRequest   = "executor.http_request"
)

// Метаданные и идентификаторы плагина.
const (
	registrationSchema = 1
	executorModelScope = "oauth"
	executorFormat     = "chat-completions"
)

// version — версия плагина; переопределяется линковщику при сборке
// (Makefile передаёт её через -ldflags "-X .../internal/gigacode.version=...").
var version = "0.1.0"

// author и repository — метаданные регистрации плагина.
const (
	author     = "spa-skyson"
	repository = "https://github.com/spa-skyson/gigacode-cliproxy-plugin"
)

// Logger — функция записи в журнал хоста.
type Logger func(level, message string, fields map[string]any)

// streamSink — получатель кадров стрима. В проде это host.stream.emit/close;
// в тестах — фальшивка.
type streamSink interface {
	// Emit отдаёт один кадр. Ошибка означает, что получатель закрыл стрим.
	Emit(streamID string, payload []byte) error
	// Close завершает стрим; errMsg непуст при ошибке чтения апстрима.
	Close(streamID string, errMsg string)
}

// Core — состояние плагина: конфиг, кэш токенов, привязки к хосту.
// Единственная точка входа — Handle.
type Core struct {
	// mu защищает конфиг и сброс кэша при reconfigure.
	mu sync.RWMutex
	// cfg — текущий конфиг.
	cfg Config
	// store — кэш токенов и конфига апстрима по device_code.
	store *tokenStore
	// hostCall — колбэки хоста (nil в юнит-тестах; тогда логи выключены).
	hostCall HostCaller
	// logFn — журнал через host.log; вызывается через nil-safe log.
	logFn Logger
	// newTransport строит транспорт, привязанный к колбэку запроса.
	newTransport func(callbackID string) Transport
	// newSink строит получателя кадров стрима для колбэка запроса.
	newSink func(callbackID string) streamSink
}

// hostTransport — реализация Transport поверх колбэков хоста.
var _ Transport = (*hostTransport)(nil)

// NewCore создаёт ядро для боевого запуска: транспорт и получатель кадров
// стрима строятся поверх колбэков хоста, логи идут в host.log.
func NewCore(call HostCaller) *Core {
	core := &Core{
		cfg:      DefaultConfig(),
		store:    newTokenStore(),
		hostCall: call,
	}
	if call != nil {
		core.logFn = func(level, message string, fields map[string]any) {
			transport := newHostTransport(call, "")
			transport.Log(level, message, fields)
		}
		core.newTransport = func(callbackID string) Transport {
			return newHostTransport(call, callbackID)
		}
		core.newSink = func(callbackID string) streamSink {
			return hostStreamSink{transport: newHostTransport(call, callbackID)}
		}
	} else {
		core.newTransport = func(string) Transport { return nil }
		core.newSink = func(string) streamSink { return nil }
	}
	return core
}

// hostStreamSink — streamSink поверх host.stream.emit/close.
type hostStreamSink struct{ transport *hostTransport }

// Emit отдаёт кадр через host.stream.emit.
func (s hostStreamSink) Emit(streamID string, payload []byte) error {
	_, err := s.transport.callHost(methodHostStreamEmit, StreamEmitRequest{StreamID: streamID, Payload: payload})
	return err
}

// Close завершает стрим через host.stream.close.
func (s hostStreamSink) Close(streamID string, errMsg string) {
	_, _ = s.transport.callHost(methodHostStreamClose, StreamCloseRequest{StreamID: streamID, Error: errMsg})
}

// config — снимок конфига.
func (c *Core) config() Config {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg
}

// log пишет в журнал хоста; без колбэков — молчание. Сообщение и строковые
// поля проходят очистку: в журнал попадают внешние тексты, которые могут
// содержать управляющие и невидимые символы.
func (c *Core) log(level, message string, fields map[string]any) {
	c.mu.RLock()
	logFn := c.logFn
	c.mu.RUnlock()
	if logFn == nil {
		return
	}
	safe := make(map[string]any, len(fields))
	for key, value := range fields {
		if s, ok := value.(string); ok {
			value = sanitizeText(s, 256)
		}
		safe[key] = value
	}
	logFn(level, sanitizeText(message, 512), safe)
}

// transportFor — транспорт, привязанный к колбэку запроса.
func (c *Core) transportFor(callbackID string) Transport {
	c.mu.RLock()
	fn := c.newTransport
	c.mu.RUnlock()
	return fn(callbackID)
}

// streamSinkFor — получатель кадров стрима для колбэка запроса.
func (c *Core) streamSinkFor(callbackID string) streamSink {
	c.mu.RLock()
	fn := c.newSink
	c.mu.RUnlock()
	return fn(callbackID)
}

// ensureAuth — свежий токен с single-flight по device_code.
func (c *Core) ensureAuth(ctx context.Context, tr Transport, cfg Config, deviceCode string, force bool) (tokenSnapshot, error) {
	margin := time.Duration(cfg.RefreshMarginSeconds) * time.Second
	logFn := c.logFn
	return c.store.ensure(ctx, deviceCode, margin, force, func(flowCtx context.Context) (tokenSnapshot, error) {
		res, err := runAuthFlow(flowCtx, tr, cfg, deviceCode, logFn)
		if err != nil {
			return tokenSnapshot{}, err
		}
		return tokenSnapshot{
			Token:     res.Token,
			ExpiresAt: res.ExpiresAt,
			Models:    res.Models,
			ChatURL:   res.ChatURL,
		}, nil
	})
}

// registration — ответ plugin.register / plugin.reconfigure:
// метаданные и заявленные возможности плагина.
func registration() Registration {
	return Registration{
		SchemaVersion: registrationSchema,
		Metadata: Metadata{
			Name:             providerID,
			Version:          version,
			Author:           author,
			GitHubRepository: repository,
			ConfigFields:     configFields(),
		},
		Capabilities: Capability{
			AuthProvider:          true,
			Executor:              true,
			ExecutorModelScope:    executorModelScope,
			ExecutorInputFormats:  []string{executorFormat},
			ExecutorOutputFormats: []string{executorFormat},
			ModelProvider:         true,
		},
	}
}

// applyConfig — разбор и применение конфига. При смене base-url кэш
// chat.url и моделей сбрасывается, токены не трогаются.
func (c *Core) applyConfig(yaml []byte) error {
	cfg, err := parseConfig(yaml)
	if err != nil {
		// Детали отклонённого конфига (включая base-url) — только в журнал.
		c.log("warn", fmt.Sprintf("Конфигурация gigacode отклонена: %s; base_url=%s",
			sanitizeText(err.Error(), 256), sanitizeText(cfg.BaseURL, 256)), nil)
		return err
	}
	c.mu.Lock()
	oldBase := c.cfg.BaseURL
	c.cfg = cfg
	c.mu.Unlock()
	if oldBase != cfg.BaseURL {
		c.store.reset()
	}
	c.log("info", "Конфиг плагина gigacode применён", map[string]any{"base_url": cfg.BaseURL})
	return nil
}

// Handle — единственная точка входа плагина.
// Любая паника перехватывается и превращается в конверт internal_error:
// паника на границе cgo убила бы процесс хоста.
func (c *Core) Handle(method string, request []byte) []byte {
	return c.handle(method, request)
}

// handle — тело Handle с recover: паника перехватывается, детали уходят
// в журнал, клиент получает фиксированный текст.
func (c *Core) handle(method string, request []byte) (out []byte) {
	defer func() {
		if recovered := recover(); recovered != nil {
			c.log("error", fmt.Sprintf("Внутренняя ошибка плагина gigacode: %v", recovered), nil)
			out = errEnvelope(newErr(codeInternalError, msgInternal, 500))
		}
	}()
	if c == nil {
		return errEnvelope(newErr(codeInternalError, msgInternal, 500))
	}
	result, err := c.dispatch(method, request)
	if err != nil {
		// Ошибки, не являющиеся плагинными, уходят клиенту с фиксированным
		// текстом (errEnvelope); исходный текст — в журнал уровня error.
		if _, ok := asPluginErr(err); !ok {
			c.log("error", fmt.Sprintf("Неожиданная ошибка метода %s: %s", method, sanitizeText(err.Error(), 512)), nil)
		}
		return errEnvelope(err)
	}
	if result == nil {
		return okEmpty()
	}
	return result
}

// dispatch — маршрутизация методов RPC.
func (c *Core) dispatch(method string, request []byte) ([]byte, error) {
	switch method {
	case methodPluginRegister, methodPluginReconfigure:
		return c.handleLifecycle(method, request)
	case methodPluginQuiesce:
		c.log("debug", "Плагин gigacode переведён в режим покоя", nil)
		return nil, nil
	case methodPluginShutdown:
		c.store.reset()
		c.log("info", "Плагин gigacode остановлен: состояние токенов очищено", nil)
		return nil, nil

	case methodAuthIdentifier:
		return okEnvelope(IdentifierResponse{Identifier: providerID})
	case methodAuthParse:
		return handleAuthParse(c, request)
	case methodAuthLoginStart:
		return handleAuthLogin(methodAuthLoginStart)
	case methodAuthLoginPoll:
		return handleAuthLogin(methodAuthLoginPoll)
	case methodAuthRefresh:
		return handleAuthRefresh(c, request)

	case methodModelStatic:
		return handleModelStatic(c, request)
	case methodModelForAuth:
		return handleModelForAuth(c, request)

	case methodExecutorIdentifier:
		return okEnvelope(IdentifierResponse{Identifier: providerID})
	case methodExecutorExecute:
		return handleExecutorExecute(c, request, false)
	case methodExecutorExecuteStream:
		return handleExecutorExecute(c, request, true)
	case methodExecutorCountTokens:
		return nil, newErr(codeNotImplemented,
			"executor.count_tokens не поддержан провайдером gigacode", 501)
	case methodExecutorHTTPRequest:
		return nil, newErr(codeNotImplemented,
			"executor.http_request не поддержан провайдером gigacode", 501)

	default:
		return nil, newErr(codeUnknownMethod, "Неизвестный метод: "+method, 404)
	}
}

// handleLifecycle — plugin.register / plugin.reconfigure: разбор и
// применение конфига, ответ с метаданными и возможностями.
func (c *Core) handleLifecycle(method string, request []byte) ([]byte, error) {
	var req LifecycleRequest
	if err := json.Unmarshal(request, &req); err != nil {
		c.log("warn", "Некорректный запрос "+method+": "+sanitizeText(err.Error(), 512), nil)
		return nil, newErr(codeInvalidRequest, "Некорректный запрос", 400)
	}
	if err := c.applyConfig(req.ConfigYAML); err != nil {
		return nil, err
	}
	reg := registration()
	if method == methodPluginRegister {
		c.log("info", fmt.Sprintf("Плагин gigacode зарегистрирован: версия=%s", reg.Metadata.Version), nil)
	}
	return okEnvelope(reg)
}

// SetHostCaller — привязка колбэков хоста; вызывается из main.go в
// cliproxy_plugin_init.
func SetHostCaller(call HostCaller) {
	globalCore = NewCore(call)
}

// globalCore — ядро процесса плагина.
var globalCore *Core

// HandleMethod — точка входа для cgo-клея main.go.
func HandleMethod(method string, request []byte) []byte {
	core := globalCore
	if core == nil {
		return errEnvelope(newErr(codeInternalError, msgInternal, 500))
	}
	return core.Handle(method, request)
}

// Shutdown — остановка плагина: очищает состояние токенов.
func Shutdown() {
	if core := globalCore; core != nil {
		core.Handle(methodPluginShutdown, []byte("{}"))
	}
}
