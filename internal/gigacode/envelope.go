package gigacode

import (
	"encoding/json"
	"fmt"
)

// Коды ошибок конверта, распознаваемые хостом (sdk/pluginabi/types.go).
const (
	codeRequestScoped      = "request_scoped"
	codeTransientTransport = "transient_transport"
	codeNotImplemented     = "not_implemented"
	codeUpstreamAuthFail   = "upstream_auth_failed"
	codeUpstreamError      = "upstream_error"
	codeUpstreamTimeout    = "upstream_timeout"
	codeDeviceNotAuth      = "device_not_authorized"
	codeInvalidAuthFile    = "invalid_auth_file"
	codeInvalidRequest     = "invalid_request"
	codeUnknownMethod      = "unknown_method"
	codeInvalidConfig      = "invalid_config"
	codeInternalError      = "internal_error"
)

// pluginError — ошибка, разворачиваемая в конверт {"ok":false,"error":{…}}.
type pluginError struct {
	code    string
	message string
	status  int
}

// Error реализует error.
func (e *pluginError) Error() string { return e.message }

func (e *pluginError) envelopeError() *EnvelopeError {
	return &EnvelopeError{Code: e.code, Message: e.message, HTTPStatus: e.status}
}

// newErr строит pluginError.
func newErr(code, message string, status int) *pluginError {
	return &pluginError{code: code, message: message, status: status}
}

// okEnvelope — успешный конверт с произвольным результатом.
func okEnvelope(result any) ([]byte, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("маршалинг результата: %w", err)
	}
	out, err := json.Marshal(Envelope{OK: true, Result: raw})
	if err != nil {
		return nil, fmt.Errorf("маршалинг конверта: %w", err)
	}
	return out, nil
}

// okEmpty — успешный конверт с пустым объектом в result.
func okEmpty() []byte {
	raw, _ := json.Marshal(Envelope{OK: true, Result: json.RawMessage("{}")})
	return raw
}

// errEnvelope — конверт с ошибкой. Ошибки, не являющиеся плагинными
// (*pluginError), приходят клиенту с фиксированным текстом: их исходные
// тексты могут содержать внешние детали; логирование — задача вызывающего.
func errEnvelope(err error) []byte {
	var ee *EnvelopeError
	switch v := err.(type) {
	case *pluginError:
		ee = v.envelopeError()
	case nil:
		ee = &EnvelopeError{Code: codeInternalError, Message: msgInternal}
	default:
		ee = &EnvelopeError{Code: codeInternalError, Message: msgInternal}
	}
	raw, _ := json.Marshal(Envelope{OK: false, Error: ee})
	return raw
}
