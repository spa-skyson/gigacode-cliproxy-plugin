package gigacode

import (
	"encoding/json"
	"time"
)

// Структуры провода RPC. Имена ключей повторяют контракт хоста CLIProxyAPI
// (sdk/pluginapi: у типов нет json-тегов, ключи — имена полей Go; обёртки
// internal/pluginhost добавляют snake_case-поля stream_id и host_callback_id).
// Модуль хоста сознательно не импортируется.

// ---------------------------------------------------------------- конверт

// Envelope — конверт ответа плагина (sdk/pluginabi/types.go).
type Envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *EnvelopeError  `json:"error,omitempty"`
}

// EnvelopeError — структурированная ошибка внутри конверта.
type EnvelopeError struct {
	Code              string `json:"code"`
	Message           string `json:"message"`
	Retryable         bool   `json:"retryable,omitempty"`
	HTTPStatus        int    `json:"http_status,omitempty"`
	RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
	CredentialScoped  bool   `json:"credential_scoped,omitempty"`
}

// ---------------------------------------------------------- регистрация

// LifecycleRequest — запрос plugin.register / plugin.reconfigure
// (internal/pluginhost/rpc_client.go).
type LifecycleRequest struct {
	// ConfigYAML — base64 YAML-блока plugins.configs.gigacode.
	ConfigYAML []byte `json:"config_yaml"`
	// SchemaVersion — версия контракта, поддержанная хостом.
	SchemaVersion uint32 `json:"schema_version"`
	// MultiAuthProviders — поддержка нескольких провайдеров одним плагином.
	MultiAuthProviders bool `json:"multi_auth_providers,omitempty"`
}

// Registration — ответ plugin.register / plugin.reconfigure
// (internal/pluginhost/rpc_schema.go: rpcRegistration).
type Registration struct {
	SchemaVersion uint32     `json:"schema_version"`
	Metadata      Metadata   `json:"metadata"`
	Capabilities  Capability `json:"capabilities"`
}

// Metadata — метаданные плагина (sdk/pluginapi.Metadata, ключи — имена полей).
type Metadata struct {
	Name             string        `json:"Name"`
	Version          string        `json:"Version"`
	Author           string        `json:"Author"`
	GitHubRepository string        `json:"GitHubRepository"`
	Logo             string        `json:"Logo"`
	ConfigFields     []ConfigField `json:"ConfigFields"`
}

// ConfigField — описание одного ключа конфига для management-клиентов.
type ConfigField struct {
	Name        string   `json:"Name"`
	Type        string   `json:"Type"`
	Default     any      `json:"Default"`
	EnumValues  []string `json:"EnumValues"`
	Description string   `json:"Description"`
}

// Capability — заявленные возможности плагина
// (internal/pluginhost/rpc_schema.go: rpcCapabilities, ключи в snake_case).
type Capability struct {
	ModelProvider         bool     `json:"model_provider"`
	AuthProvider          bool     `json:"auth_provider"`
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string `json:"executor_output_formats,omitempty"`
}

// IdentifierResponse — ответ auth.identifier / executor.identifier.
type IdentifierResponse struct {
	Identifier string `json:"identifier"`
}

// ------------------------------------------------------------------ auth

// AuthParseRequest — запрос auth.parse (sdk/pluginapi.AuthParseRequest).
type AuthParseRequest struct {
	Provider string         `json:"Provider"`
	Path     string         `json:"Path"`
	FileName string         `json:"FileName"`
	RawJSON  []byte         `json:"RawJSON"`
	Host     map[string]any `json:"Host"`
}

// AuthParseResponse — ответ auth.parse (sdk/pluginapi.AuthParseResponse).
type AuthParseResponse struct {
	Handled bool       `json:"Handled"`
	Auth    AuthData   `json:"Auth"`
	Auths   []AuthData `json:"Auths"`
}

// AuthData — запись учётной записи (sdk/pluginapi.AuthData).
type AuthData struct {
	Provider         string            `json:"Provider"`
	ID               string            `json:"ID"`
	FileName         string            `json:"FileName"`
	Label            string            `json:"Label"`
	Prefix           string            `json:"Prefix"`
	ProxyURL         string            `json:"ProxyURL"`
	Disabled         bool              `json:"Disabled"`
	StorageJSON      []byte            `json:"StorageJSON"`
	Metadata         map[string]any    `json:"Metadata"`
	Attributes       map[string]string `json:"Attributes"`
	NextRefreshAfter time.Time         `json:"NextRefreshAfter"`
}

// AuthLoginStartRequest — запрос auth.login.start (sdk/pluginapi.AuthLoginStartRequest).
type AuthLoginStartRequest struct {
	Provider string         `json:"Provider"`
	BaseURL  string         `json:"BaseURL"`
	Host     map[string]any `json:"Host"`
	Metadata map[string]any `json:"Metadata"`
}

// AuthLoginPollRequest — запрос auth.login.poll (sdk/pluginapi.AuthLoginPollRequest).
type AuthLoginPollRequest struct {
	Provider string         `json:"Provider"`
	State    string         `json:"State"`
	Host     map[string]any `json:"Host"`
	Metadata map[string]any `json:"Metadata"`
}

// AuthRefreshRequest — запрос auth.refresh (sdk/pluginapi.AuthRefreshRequest).
type AuthRefreshRequest struct {
	AuthID         string            `json:"AuthID"`
	AuthProvider   string            `json:"AuthProvider"`
	StorageJSON    []byte            `json:"StorageJSON"`
	Metadata       map[string]any    `json:"Metadata"`
	Attributes     map[string]string `json:"Attributes"`
	Host           map[string]any    `json:"Host"`
	HostCallbackID string            `json:"host_callback_id,omitempty"`
}

// AuthRefreshResponse — ответ auth.refresh (sdk/pluginapi.AuthRefreshResponse).
type AuthRefreshResponse struct {
	Auth             AuthData  `json:"Auth"`
	NextRefreshAfter time.Time `json:"NextRefreshAfter"`
}

// HostCallbackEnvelope — общая обёртка запросов с host_callback_id.
// Встраивается в запросы, несущие контекст колбэков хоста.
type HostCallbackEnvelope struct {
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// ----------------------------------------------------------------- models

// StaticModelRequest — запрос model.static (sdk/pluginapi.StaticModelRequest).
type StaticModelRequest struct {
	Plugin              Metadata       `json:"Plugin"`
	Host                map[string]any `json:"Host"`
	MultiProviderModels bool           `json:"MultiProviderModels"`
}

// AuthModelRequest — запрос model.for_auth (sdk/pluginapi.AuthModelRequest).
type AuthModelRequest struct {
	Plugin         Metadata          `json:"Plugin"`
	AuthID         string            `json:"AuthID"`
	AuthProvider   string            `json:"AuthProvider"`
	StorageJSON    []byte            `json:"StorageJSON"`
	Metadata       map[string]any    `json:"Metadata"`
	Attributes     map[string]string `json:"Attributes"`
	Host           map[string]any    `json:"Host"`
	HostCallbackID string            `json:"host_callback_id,omitempty"`
}

// ModelResponse — ответ model.static / model.for_auth (sdk/pluginapi.ModelResponse).
type ModelResponse struct {
	Provider      string      `json:"Provider"`
	Models        []ModelInfo `json:"Models"`
	Registrations []any       `json:"Registrations,omitempty"`
	AuthUpdate    AuthData    `json:"AuthUpdate"`
}

// ModelInfo — описание модели (sdk/pluginapi.ModelInfo, ключи — имена полей).
type ModelInfo struct {
	ID                         string   `json:"ID"`
	Object                     string   `json:"Object"`
	Created                    int64    `json:"Created"`
	OwnedBy                    string   `json:"OwnedBy"`
	Type                       string   `json:"Type"`
	DisplayName                string   `json:"DisplayName"`
	Name                       string   `json:"Name"`
	Version                    string   `json:"Version"`
	Description                string   `json:"Description"`
	InputTokenLimit            int64    `json:"InputTokenLimit"`
	OutputTokenLimit           int64    `json:"OutputTokenLimit"`
	SupportedGenerationMethods []string `json:"SupportedGenerationMethods"`
	ContextLength              int64    `json:"ContextLength"`
	MaxCompletionTokens        int64    `json:"MaxCompletionTokens"`
	UserDefined                bool     `json:"UserDefined"`
}

// --------------------------------------------------------------- executor

// ExecutorRequest — запрос executor.execute / executor.execute_stream
// (sdk/pluginapi.ExecutorRequest).
type ExecutorRequest struct {
	AuthID          string              `json:"AuthID"`
	AuthProvider    string              `json:"AuthProvider"`
	Model           string              `json:"Model"`
	Format          string              `json:"Format"`
	Stream          bool                `json:"Stream"`
	Alt             string              `json:"Alt"`
	Headers         map[string][]string `json:"Headers"`
	Query           map[string][]string `json:"Query"`
	OriginalRequest []byte              `json:"OriginalRequest"`
	SourceFormat    string              `json:"SourceFormat"`
	Payload         []byte              `json:"Payload"`
	Metadata        map[string]any      `json:"Metadata"`
	StorageJSON     []byte              `json:"StorageJSON"`
	AuthMetadata    map[string]any      `json:"AuthMetadata"`
	AuthAttributes  map[string]string   `json:"AuthAttributes"`

	// StreamID присутствует только в executor.execute_stream
	// (internal/pluginhost/rpc_schema.go: rpcExecutorRequest).
	StreamID string `json:"stream_id,omitempty"`

	// HostCallbackID — контекст колбэков хоста для этого запроса.
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// ExecutorResponse — ответ executor.execute без стриминга
// (sdk/pluginapi.ExecutorResponse).
type ExecutorResponse struct {
	Payload     []byte              `json:"Payload"`
	Headers     map[string][]string `json:"Headers"`
	Metadata    map[string]any      `json:"Metadata"`
	Attribution map[string]any      `json:"attribution,omitempty"`
}

// ExecutorStreamResponse — синхронный ответ стрима
// (internal/pluginhost/rpc_schema.go: rpcExecutorStreamResponse, ключи в нижнем регистре).
// Пустой Chunks переключает хост на асинхронный режим: кадры отдаются
// колбэками host.stream.emit / host.stream.close.
type ExecutorStreamResponse struct {
	Headers     map[string][]string   `json:"headers,omitempty"`
	Chunks      []ExecutorStreamChunk `json:"chunks,omitempty"`
	Attribution map[string]any        `json:"attribution,omitempty"`
}

// ExecutorStreamChunk — один кадр синхронного стрима (sdk/pluginapi.ExecutorStreamChunk).
type ExecutorStreamChunk struct {
	Payload []byte `json:"Payload"`
	Err     string `json:"Err"`
}

// StreamEmitRequest — запрос host.stream.emit
// (internal/pluginhost/stream_bridge.go: rpcStreamEmitRequest).
type StreamEmitRequest struct {
	StreamID    string         `json:"stream_id"`
	Payload     []byte         `json:"payload,omitempty"`
	Error       string         `json:"error,omitempty"`
	ErrorDetail *EnvelopeError `json:"error_detail,omitempty"`
}

// StreamCloseRequest — запрос host.stream.close
// (internal/pluginhost/stream_bridge.go: rpcStreamCloseRequest).
type StreamCloseRequest struct {
	StreamID    string         `json:"stream_id"`
	Error       string         `json:"error,omitempty"`
	ErrorDetail *EnvelopeError `json:"error_detail,omitempty"`
}
