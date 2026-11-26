// Package gigacodemock — мок API GigaCode на net/http/httptest.
// Повторяет сценарии Python-прокси (proxy/tests/test_proxy.py) и добавляет
// режимы, нужные юнит-тестам плагина.
package gigacodemock

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// Пути внутри мока (как на настоящем апстриме: {base-url}/…).
const (
	discoveryPath   = "/api/v2/config"
	authPath        = "/api/v2/auth/plugin"
	chatCompletions = "/api/v2/chat/completions"

	// discoveryHeaderName — заголовок с URL авторизации.
	discoveryHeaderName = "x-authorization-url"
	// signinHeaderName — признак «устройство не авторизовано».
	signinHeaderName = "x-signin-gitverse"
)

// RecordedRequest — запись одного запроса к моку.
type RecordedRequest struct {
	Method string              `json:"method"`
	Path   string              `json:"path"`
	Header map[string][]string `json:"header"`
	Body   []byte              `json:"body"`
}

// Mock — состояние фейкового GigaCode: режимы ошибок, счётчики, записи.
type Mock struct {
	mu sync.Mutex

	server               *httptest.Server
	expiresIn            int
	failChat401Once      bool
	always401            bool
	signinError          bool
	hangAuth             bool
	hangCh               chan struct{}
	chatStatus           int // 0 — обычные ответы
	sse                  bool
	sseDelay             time.Duration
	badAuthResponse      map[string]any
	failNextAuthStatus   int // одноразовый неуспех авторизации без signin-заголовка
	failNextConfigStatus int // одноразовый неуспех шага 3
	authCalls            int
	chatCalls            int
	tokens               []string
	requests             []RecordedRequest
	models               []any
	chatURL              string
}

// New запускает мок на случайном порту.
func New() *Mock {
	m := &Mock{
		expiresIn: 600,
		sseDelay:  10 * time.Millisecond,
		models: []any{
			map[string]any{"model": "vllm/CodeAgent", "name": "CodeAgent", "description": "Code agent"},
		},
		chatURL: "/chat/completions",
	}
	m.server = httptest.NewServer(http.HandlerFunc(m.handle))
	return m
}

// URL — базовый адрес мока (без /api/v2).
func (m *Mock) URL() string { return m.server.URL }

// Close останавливает мок; снимает зависания авторизации.
func (m *Mock) Close() {
	m.mu.Lock()
	if m.hangCh != nil {
		close(m.hangCh)
		m.hangCh = nil
		m.hangAuth = false
	}
	m.mu.Unlock()
	m.server.Close()
}

// Handler — обработчик мока для внешнего http.Server (cmd/gigacode-mock).
func (m *Mock) Handler() http.Handler { return http.HandlerFunc(m.handle) }

// ------------------------------------------------------------ режимы мока

// SetExpiresIn меняет expires_in в ответе авторизации.
func (m *Mock) SetExpiresIn(seconds int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expiresIn = seconds
}

// FailChat401Once заставляет чат один раз ответить 401 на текущий токен.
func (m *Mock) FailChat401Once() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failChat401Once = true
}

// SetAlways401 — чат отклоняет все токены.
func (m *Mock) SetAlways401(v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.always401 = v
}

// SetSigninError — авторизация отвечает «устройство не авторизовано».
func (m *Mock) SetSigninError(v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.signinError = v
}

// SetHangAuth — авторизация «зависает» (не отвечает до снятия режима).
func (m *Mock) SetHangAuth(v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v == m.hangAuth {
		return
	}
	m.hangAuth = v
	if v {
		m.hangCh = make(chan struct{})
	} else if m.hangCh != nil {
		close(m.hangCh)
		m.hangCh = nil
	}
}

// SetChatStatus подменяет статус ответа чата (0 — обычные ответы).
func (m *Mock) SetChatStatus(status int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.chatStatus = status
}

// SetSSE включает потоковый ответ чата (SSE-кадры с задержкой).
func (m *Mock) SetSSE(v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sse = v
}

// SetSSEDelay задаёт паузу между SSE-кадрами.
func (m *Mock) SetSSEDelay(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d > 0 {
		m.sseDelay = d
	}
}

// FailNextAuthOnce — один неуспех авторизации со статусом status
// (без signin-заголовка: временная ошибка).
func (m *Mock) FailNextAuthOnce(status int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failNextAuthStatus = status
}

// FailNextConfigOnce — один неуспех шага 3 со статусом status.
func (m *Mock) FailNextConfigOnce(status int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failNextConfigStatus = status
}

// SetModels меняет список моделей в конфиге (элементы могут быть любыми —
// даже не объектами: плагин обязан их пропускать).
func (m *Mock) SetModels(models []any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.models = models
}

// SetChatURL меняет поле chat.url в конфиге.
func (m *Mock) SetChatURL(url string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.chatURL = url
}

// SetBadAuthResponse подменяет тело ответа авторизации.
func (m *Mock) SetBadAuthResponse(body map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.badAuthResponse = body
}

// ------------------------------------------------------- наблюдение мока

// AuthCalls — сколько раз точка авторизации выпустила токен.
func (m *Mock) AuthCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.authCalls
}

// ChatCalls — сколько запросов пришло на чат.
func (m *Mock) ChatCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.chatCalls
}

// Tokens — копия списка выпущенных токенов.
func (m *Mock) Tokens() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.tokens...)
}

// LastToken — последний выпущенный токен.
func (m *Mock) LastToken() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.tokens) == 0 {
		return ""
	}
	return m.tokens[len(m.tokens)-1]
}

// Requests — копия всех записанных запросов.
func (m *Mock) Requests() []RecordedRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]RecordedRequest(nil), m.requests...)
}

// ChatRequests — записанные запросы к чату.
func (m *Mock) ChatRequests() []RecordedRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]RecordedRequest, 0, 2)
	for _, req := range m.requests {
		if req.Path == chatCompletions {
			out = append(out, req)
		}
	}
	return out
}

// LastAuthRequest — последний запрос к точке авторизации.
func (m *Mock) LastAuthRequest() (RecordedRequest, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.requests) - 1; i >= 0; i-- {
		if m.requests[i].Path == authPath {
			return m.requests[i], true
		}
	}
	return RecordedRequest{}, false
}

// LastChatRequest — последний запрос к чату.
func (m *Mock) LastChatRequest() (RecordedRequest, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.requests) - 1; i >= 0; i-- {
		if m.requests[i].Path == chatCompletions {
			return m.requests[i], true
		}
	}
	return RecordedRequest{}, false
}

// LastDiscoveryRequest — последний discovery-запрос шага 1.
func (m *Mock) LastDiscoveryRequest() (RecordedRequest, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.requests) - 1; i >= 0; i-- {
		if m.requests[i].Path == discoveryPath && headerValue(m.requests[i].Header, "Authorization") == "" {
			return m.requests[i], true
		}
	}
	return RecordedRequest{}, false
}

func headerValue(header map[string][]string, name string) string {
	for key, values := range header {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

// ---------------------------------------------------------------- сервер

func (m *Mock) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	m.record(r, body)
	switch {
	case r.Method == "GET" && r.URL.Path == discoveryPath:
		// Один путь на два шага: без Authorization — discovery (шаг 1),
		// с Authorization — конфиг (шаг 3), как на настоящем апстриме.
		if headerValue(r.Header, "Authorization") != "" {
			m.handleConfig(w)
		} else {
			m.handleDiscovery(w, r)
		}
	case r.Method == "POST" && r.URL.Path == authPath:
		m.handleAuth(w, body)
	case r.Method == "POST" && r.URL.Path == chatCompletions:
		m.handleChat(w, r, body)
	default:
		http.NotFound(w, r)
	}
}

// record — счётчики и записи (заголовки и тело).
func (m *Mock) record(r *http.Request, body []byte) {
	header := make(map[string][]string, len(r.Header))
	for key, values := range r.Header {
		header[key] = append([]string(nil), values...)
	}
	entry := RecordedRequest{Method: r.Method, Path: r.URL.Path, Header: header, Body: append([]byte(nil), body...)}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, entry)
}

// handleConfig — шаг 3: конфиг с моделями и chat.url.
func (m *Mock) handleConfig(w http.ResponseWriter) {
	m.mu.Lock()
	failStatus := m.failNextConfigStatus
	if failStatus != 0 {
		m.failNextConfigStatus = 0
	}
	models := m.models
	chatURL := m.chatURL
	m.mu.Unlock()

	if failStatus != 0 {
		writeJSON(w, failStatus, map[string]any{"error": map[string]any{"message": "mock config failure"}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"chat": map[string]any{"models": models, "url": chatURL},
	})
}

// handleDiscovery — шаг 1: без авторизации отдаём URL авторизации.
func (m *Mock) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	authURL := fmt.Sprintf("http://%s%s", r.Host, authPath)
	w.Header().Set(discoveryHeaderName, authURL)
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":{"message":"требуется авторизация"}}`))
}

// handleAuth — шаг 2: обмен device_code на JWT.
func (m *Mock) handleAuth(w http.ResponseWriter, body []byte) {
	m.mu.Lock()
	signinError := m.signinError
	hangCh := m.hangCh
	failStatus := m.failNextAuthStatus
	if failStatus != 0 {
		m.failNextAuthStatus = 0
	}
	badResponse := m.badAuthResponse
	expiresIn := m.expiresIn
	m.mu.Unlock()

	if signinError {
		w.Header().Set(signinHeaderName, "required")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"устройство не привязано"}}`))
		return
	}
	if hangCh != nil {
		<-hangCh // «зависаем»: ответ появится только после снятия режима
	}
	if failStatus != 0 {
		w.WriteHeader(failStatus)
		_, _ = w.Write([]byte(`{"error":{"message":"mock auth failure"}}`))
		return
	}

	m.mu.Lock()
	m.authCalls++
	token := fmt.Sprintf("jwt-%d", m.authCalls)
	m.tokens = append(m.tokens, token)
	m.mu.Unlock()

	if badResponse != nil {
		writeJSON(w, http.StatusOK, badResponse)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": token,
		"expires_in":   expiresIn,
	})
}

// handleChat — чат: подмены статусов, 401-сценарии, SSE по флагу stream.
func (m *Mock) handleChat(w http.ResponseWriter, r *http.Request, body []byte) {
	m.mu.Lock()
	chatStatus := m.chatStatus
	always401 := m.always401
	failOnce := m.failChat401Once
	sse := m.sse
	sseDelay := m.sseDelay
	lastToken := ""
	if len(m.tokens) > 0 {
		lastToken = m.tokens[len(m.tokens)-1]
	}
	m.mu.Unlock()

	m.mu.Lock()
	m.chatCalls++
	m.mu.Unlock()

	if chatStatus != 0 {
		writeJSON(w, chatStatus, map[string]any{
			"error": map[string]any{"message": fmt.Sprintf("подменённый статус %d", chatStatus)},
		})
		return
	}
	if always401 {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"токен отклонён"}}`))
		return
	}
	if failOnce {
		m.mu.Lock()
		m.failChat401Once = false
		m.mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"токен устарел"}}`))
		return
	}
	if headerValue(r.Header, "Authorization") != "Bearer "+lastToken {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"неизвестный токен"}}`))
		return
	}
	// Флаг stream из тела запроса — как настоящий апстрим: тело плагин
	// пересылает без изменений, так что клиентский флаг виден здесь.
	var bodyFlag struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &bodyFlag)
	if sse || bodyFlag.Stream {
		m.streamSSE(w, sseDelay)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{"content": "ok"},
		}},
	})
}

// streamSSE — SSE-кадры с задержкой, затем [DONE].
func (m *Mock) streamSSE(w http.ResponseWriter, delay time.Duration) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	// Чужой заголовок: плагин не должен передавать его клиенту.
	w.Header().Set("X-Upstream-Extra", "echo")
	w.WriteHeader(http.StatusOK)
	frames := []string{
		`data: {"choices":[{"delta":{"content":"первый"}}]}` + "\n\n",
		`data: {"choices":[{"delta":{"content":"второй"}}]}` + "\n\n",
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n",
		`data: [DONE]` + "\n\n",
	}
	for _, frame := range frames {
		time.Sleep(delay)
		if _, err := w.Write([]byte(frame)); err != nil {
			return
		}
		flusher.Flush()
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	raw, _ := json.Marshal(payload)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}
