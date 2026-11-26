//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	clientKey     = "client-key-for-integration"
	managementKey = "management-key-for-integration"
	deviceCode    = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	pluginVersion = "26.9.66"
)

type chatRequest struct {
	Path        string
	Token       string
	Session     string
	UserAgent   string
	ContentType string
	Body        []byte
}

type mockState struct {
	mu                sync.Mutex
	authCalls         atomic.Int64
	chatCalls         atomic.Int64
	mode              string
	unauthorized      bool
	first401          bool
	all401            bool
	chatStatus        int
	requests          []chatRequest
	tokens            []string
	firstFrame        chan time.Time
	lastFrame         chan time.Time
	cancelled         chan struct{}
	authStarted       chan struct{}
	authStartedCount  atomic.Int64
	blockAuth         bool
	expiresIn         atomic.Int64
	blockAuthAfter    atomic.Int64
	down              atomic.Bool
	addNewModel       atomic.Bool
	slowStream        atomic.Bool
	streamFrames      atomic.Int64
	framesAfterCancel atomic.Int64
	cancelSeen        atomic.Bool
	hangAuthBody      atomic.Bool
	modelDiscovery    atomic.Int64
	configAnonymous   atomic.Bool
	configAuthorized  atomic.Bool
	authDeviceCode    atomic.Bool
}

func newMockState() *mockState {
	state := &mockState{
		firstFrame:  make(chan time.Time, 1),
		lastFrame:   make(chan time.Time, 1),
		cancelled:   make(chan struct{}),
		authStarted: make(chan struct{}, 1),
	}
	state.expiresIn.Store(600)
	return state
}

func (s *mockState) handler(w http.ResponseWriter, r *http.Request) {
	if s.down.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if r.URL.Path == "/api/v2/config" {
		s.handleConfig(w, r)
		return
	}
	if r.URL.Path == "/authorize" {
		s.handleAuthorize(w, r)
		return
	}
	if r.URL.Path == "/api/v2/chat" {
		s.handleChat(w, r)
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func (s *mockState) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") == "" {
		s.configAnonymous.Store(true)
		w.Header().Set("x-authorization-url", "http://"+r.Host+"/authorize")
		w.WriteHeader(http.StatusOK)
		return
	}
	s.configAuthorized.Store(true)
	json.NewEncoder(w).Encode(map[string]any{
		"chat": map[string]any{
			"models": s.models(),
			"url":    "/chat",
		},
	})
	s.modelDiscovery.Add(1)
}

func (s *mockState) models() []map[string]any {
	models := []map[string]any{{"model": "vllm/CodeAgent"}}
	if s.addNewModel.Load() && s.authCalls.Load() >= 2 {
		models = append(models, map[string]any{"model": "vllm/NewModel"})
	}
	return models
}

func (s *mockState) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DeviceCode string `json:"device_code"`
	}
	data, _ := io.ReadAll(r.Body)
	if json.Unmarshal(data, &body) == nil && body.DeviceCode == deviceCode {
		s.authDeviceCode.Store(true)
	}
	select {
	case s.authStarted <- struct{}{}:
	default:
	}
	s.authStartedCount.Add(1)
	if s.blockAuth || (s.blockAuthAfter.Load() > 0 && s.authCalls.Load() >= s.blockAuthAfter.Load()) {
		<-r.Context().Done()
		return
	}
	if s.hangAuthBody.Load() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"access_token":"partial`)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
		return
	}
	if s.unauthorized {
		w.Header().Set("x-signin-gitverse", "signin")
		w.WriteHeader(http.StatusForbidden)
		return
	}
	s.authCalls.Add(1)
	token := fmt.Sprintf("jwt-token-%d", s.authCalls.Load())
	s.mu.Lock()
	s.tokens = append(s.tokens, token)
	s.mu.Unlock()
	json.NewEncoder(w).Encode(map[string]any{"access_token": token, "expires_in": s.expiresIn.Load()})
}

func (s *mockState) handleChat(w http.ResponseWriter, r *http.Request) {
	call := s.chatCalls.Add(1)
	body, _ := io.ReadAll(r.Body)
	record := chatRequest{
		Path:        r.URL.Path,
		Token:       r.Header.Get("Authorization"),
		Session:     r.Header.Get("x-session-id"),
		UserAgent:   r.Header.Get("User-Agent"),
		ContentType: r.Header.Get("Content-Type"),
		Body:        body,
	}
	s.mu.Lock()
	s.requests = append(s.requests, record)
	mode, first401, all401, status := s.mode, s.first401, s.all401, s.chatStatus
	unauthorized := s.unauthorized
	s.mu.Unlock()
	if unauthorized || all401 || (first401 && call == 1) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	if mode == "stream" {
		s.writeStream(w, r)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"choices": []map[string]any{{"message": map[string]string{"content": "mock response"}}},
	})
}

func (s *mockState) writeStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	frames := []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"one\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\" two\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
		"data: [DONE]\n\n",
	}
	if s.slowStream.Load() {
		frames = make([]string, 20)
		for i := range frames {
			frames[i] = fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"content\":\"frame-%02d\"}}]}\n\n", i)
		}
	}
	for i, frame := range frames {
		if s.cancelSeen.Load() {
			s.framesAfterCancel.Add(1)
		}
		s.streamFrames.Add(1)
		if _, err := io.WriteString(w, frame); err != nil {
			select {
			case <-s.cancelled:
			default:
				close(s.cancelled)
			}
			return
		}
		flusher.Flush()
		if i == 0 {
			s.firstFrame <- time.Now()
		}
		if i == len(frames)-1 {
			s.lastFrame <- time.Now()
			return
		}
		select {
		case <-r.Context().Done():
			s.cancelSeen.Store(true)
			select {
			case <-s.cancelled:
			default:
				close(s.cancelled)
			}
			return
		case <-time.After(func() time.Duration {
			if s.slowStream.Load() {
				return 300 * time.Millisecond
			}
			return 300 * time.Millisecond
		}()):
		}
	}
}

func newMock(t *testing.T, state *mockState) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(state.handler))
}

type hostOptions struct {
	Upstream string
	MaxBody  int
}

type hostProcess struct {
	base     string
	stderr   string
	cmd      *exec.Cmd
	stopOnce sync.Once
	stopFunc func()
}

func startHost(t *testing.T, options hostOptions) *hostProcess {
	t.Helper()
	root := t.TempDir()
	port := freePort(t)
	authDir := filepath.Join(root, "auths")
	pluginDir := filepath.Join(root, "plugins")
	if err := os.MkdirAll(authDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(pluginDir, 0755); err != nil {
		t.Fatal(err)
	}
	pluginPath := os.Getenv("GIGACODE_PLUGIN_SO")
	pluginBytes, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "gigacode.so"), pluginBytes, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "gigacode.json"), []byte(`{"type":"gigacode","device_code":"`+deviceCode+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	maxBody := ""
	if options.MaxBody > 0 {
		maxBody = fmt.Sprintf("      max-body-bytes: %d\n", options.MaxBody)
	}
	config := fmt.Sprintf(`config-version: 8
server:
  host: "127.0.0.1"
  port: %d
  response-compression:
    enabled: false
management:
  allow-remote: false
  secret-key: "%s"
  disable-control-panel: true
access:
  api-keys:
    - "%s"
oauth:
  auth-dir: %q
plugins:
  enabled: true
  dir: %q
  configs:
    gigacode:
      enabled: true
      priority: 1
      base-url: %q
      refresh-margin-seconds: 2
      auth-timeout-seconds: 3
      retry-seconds: 1
%s`, port, managementKey, clientKey, authDir, pluginDir, options.Upstream+"/api/v2", maxBody)
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	stderrPath := filepath.Join(root, "stderr.log")
	stderr, err := os.Create(stderrPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Getenv("CLIPROXY_BIN"), "--config", configPath, "--local-model")
	cmd.Stdout = stderr
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	host := &hostProcess{
		base:   fmt.Sprintf("http://127.0.0.1:%d", port),
		stderr: stderrPath,
		cmd:    cmd,
	}
	host.stopFunc = func() {
		host.stopOnce.Do(func() {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			_ = stderr.Close()
		})
	}
	t.Cleanup(host.stop)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		response, requestErr := http.Get(host.base + "/healthz")
		if requestErr == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return host
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("хост не готов: %s", readFile(stderrPath))
	return nil
}

func (h *hostProcess) stop() {
	if h.stopFunc != nil {
		h.stopFunc()
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}

func request(t *testing.T, method, url, key string, body []byte) *http.Response {
	return requestWithTimeout(t, 15*time.Second, method, url, key, body)
}

func requestWithTimeout(t *testing.T, timeout time.Duration, method, url, key string, body []byte) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	response, err := (&http.Client{Timeout: timeout}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func readJSON(t *testing.T, response *http.Response, target any) {
	t.Helper()
	defer response.Body.Close()
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		t.Fatal(err)
	}
}

func readFile(path string) string {
	data, _ := os.ReadFile(path)
	return string(data)
}

func waitForModel(t *testing.T, host *hostProcess, want string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		response := request(t, http.MethodGet, host.base+"/v1/models", clientKey, nil)
		var current struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		readJSON(t, response, &current)
		for _, model := range current.Data {
			if model.ID == want {
				return true
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func waitForNoModel(t *testing.T, host *hostProcess, unwanted string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		response := request(t, http.MethodGet, host.base+"/v1/models", clientKey, nil)
		var current struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		readJSON(t, response, &current)
		found := false
		for _, model := range current.Data {
			if model.ID == unwanted {
				found = true
			}
		}
		if !found {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("модель %q неожиданно присутствует", unwanted)
}

type pluginEntry struct {
	ID               string `json:"id"`
	Registered       bool   `json:"registered"`
	EffectiveEnabled bool   `json:"effective_enabled"`
}

type pluginList struct {
	Plugins []pluginEntry `json:"plugins"`
}

func findPlugin(t *testing.T, list pluginList) pluginEntry {
	t.Helper()
	for _, entry := range list.Plugins {
		if entry.ID == "gigacode" {
			return entry
		}
	}
	t.Fatal("элемент gigacode отсутствует")
	return pluginEntry{}
}

func lastToken(t *testing.T, state *mockState) string {
	t.Helper()
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.tokens) == 0 {
		t.Fatal("мок не выдал токен")
	}
	return state.tokens[len(state.tokens)-1]
}

func chatRequests(t *testing.T, state *mockState) []chatRequest {
	t.Helper()
	state.mu.Lock()
	defer state.mu.Unlock()
	return append([]chatRequest(nil), state.requests...)
}

func TestPluginIntegration(t *testing.T) {
	if os.Getenv("CLIPROXY_BIN") == "" || os.Getenv("GIGACODE_PLUGIN_SO") == "" {
		t.Skip("нужны CLIPROXY_BIN и GIGACODE_PLUGIN_SO")
	}

	t.Run("регистрация", func(t *testing.T) {
		state := newMockState()
		mock := newMock(t, state)
		defer mock.Close()
		host := startHost(t, hostOptions{Upstream: mock.URL})
		response := request(t, http.MethodGet, host.base+"/v0/management/plugins", managementKey, nil)
		if response.Header.Get("X-CPA-SUPPORT-PLUGIN") != "1" {
			t.Fatalf("X-CPA-SUPPORT-PLUGIN=%q", response.Header.Get("X-CPA-SUPPORT-PLUGIN"))
		}
		var list pluginList
		readJSON(t, response, &list)
		entry := findPlugin(t, list)
		if !entry.Registered || !entry.EffectiveEnabled {
			t.Fatalf("gigacode: registered=%v effective_enabled=%v", entry.Registered, entry.EffectiveEnabled)
		}
	})

	t.Run("модели-и-флоу-авторизации", func(t *testing.T) {
		state := newMockState()
		mock := newMock(t, state)
		defer mock.Close()
		host := startHost(t, hostOptions{Upstream: mock.URL})
		found := waitForModel(t, host, "vllm/CodeAgent", 10*time.Second)
		if !found {
			t.Fatal("vllm/CodeAgent отсутствует")
		}
		if state.authCalls.Load() == 0 {
			t.Fatal("мок не видел авторизацию")
		}
		if !state.configAnonymous.Load() || !state.authDeviceCode.Load() || !state.configAuthorized.Load() {
			t.Fatalf("неполный флоу авторизации: anonymous=%v auth=%v authorized=%v", state.configAnonymous.Load(), state.authDeviceCode.Load(), state.configAuthorized.Load())
		}
	})

	t.Run("чат-без-стриминга", func(t *testing.T) {
		state := newMockState()
		mock := newMock(t, state)
		defer mock.Close()
		host := startHost(t, hostOptions{Upstream: mock.URL})
		if !waitForModel(t, host, "vllm/CodeAgent", 10*time.Second) {
			t.Fatal("модель не появилась перед chat-запросом")
		}
		body := []byte(`{"model":"vllm/CodeAgent","messages":[{"role":"user","content":"hello"}],"stream":false}`)
		response := request(t, http.MethodPost, host.base+"/v1/chat/completions", clientKey, body)
		responseBody, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.StatusCode, responseBody)
		}
		var output struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(responseBody, &output); err != nil {
			t.Fatalf("ответ не JSON: %v body=%s", err, responseBody)
		}
		if len(output.Choices) == 0 {
			t.Fatalf("ответ не содержит choices: %s", responseBody)
		}
		if output.Choices[0].Message.Content != "mock response" {
			t.Fatalf("content=%q", output.Choices[0].Message.Content)
		}
		requests := chatRequests(t, state)
		if len(requests) != 1 {
			t.Fatalf("chat calls=%d", len(requests))
		}
		chat := requests[0]
		if chat.Path != "/api/v2/chat" || chat.Token != "Bearer "+lastToken(t, state) {
			t.Fatalf("path/token: %q %q", chat.Path, chat.Token)
		}
		if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(chat.Session) {
			t.Fatalf("не UUID v4: %q", chat.Session)
		}
		if !strings.HasPrefix(chat.UserAgent, "GigaCode/"+pluginVersion+" (") || chat.ContentType != "application/json" {
			t.Fatalf("headers: ua=%q content-type=%q", chat.UserAgent, chat.ContentType)
		}
		var got, want map[string]any
		json.Unmarshal(chat.Body, &got)
		json.Unmarshal(body, &want)
		if !jsonEqualValue(got["model"], want["model"]) || !jsonEqualValue(got["messages"], want["messages"]) {
			t.Fatalf("model/messages различаются: got=%s want=%s", chat.Body, body)
		}
	})

	t.Run("стриминг", func(t *testing.T) {
		state := newMockState()
		state.mode = "stream"
		mock := newMock(t, state)
		defer mock.Close()
		host := startHost(t, hostOptions{Upstream: mock.URL})
		if !waitForModel(t, host, "vllm/CodeAgent", 10*time.Second) {
			t.Fatal("модель не появилась перед streaming-запросом")
		}
		req, _ := http.NewRequest(http.MethodPost, host.base+"/v1/chat/completions", strings.NewReader(`{"model":"vllm/CodeAgent","messages":[],"stream":true}`))
		req.Header.Set("Authorization", "Bearer "+clientKey)
		response, err := (&http.Client{}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		start := time.Now()
		var firstClientFrame time.Time
		scanner := bufio.NewScanner(response.Body)
		var text string
		for scanner.Scan() {
			line := scanner.Text()
			t.Logf("SSE: %s", line)
			if strings.HasPrefix(line, "data: ") && !strings.Contains(line, "[DONE]") {
				var frame struct {
					Choices []struct {
						Delta struct {
							Content string `json:"content"`
						} `json:"delta"`
					} `json:"choices"`
				}
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame); err != nil {
					t.Fatal(err)
				}
				if len(frame.Choices) == 0 {
					continue
				}
				if firstClientFrame.IsZero() {
					firstClientFrame = time.Now()
				}
				text += frame.Choices[0].Delta.Content
			}
		}
		if err := scanner.Err(); err != nil {
			t.Fatal(err)
		}
		if text != "one two" {
			t.Fatalf("stream text=%q", text)
		}
		last := <-state.lastFrame
		if firstClientFrame.IsZero() || !firstClientFrame.Before(last) || !firstClientFrame.After(start) {
			t.Fatalf("SSE timing invalid: first=%s last=%s", firstClientFrame, last)
		}
	})

	t.Run("один-401", func(t *testing.T) {
		state := newMockState()
		state.first401 = true
		mock := newMock(t, state)
		defer mock.Close()
		host := startHost(t, hostOptions{Upstream: mock.URL})
		if !waitForModel(t, host, "vllm/CodeAgent", 10*time.Second) {
			t.Fatal("модель не появилась перед chat-запросом")
		}
		response := request(t, http.MethodPost, host.base+"/v1/chat/completions", clientKey, []byte(`{"model":"vllm/CodeAgent","messages":[]}`))
		response.Body.Close()
		calls := chatRequests(t, state)
		if response.StatusCode != http.StatusOK || len(calls) != 2 || calls[0].Token == calls[1].Token || calls[0].Session == calls[1].Session {
			t.Fatalf("status=%d calls=%+v", response.StatusCode, calls)
		}
	})

	t.Run("два-401", func(t *testing.T) {
		state := newMockState()
		mock := newMock(t, state)
		defer mock.Close()
		host := startHost(t, hostOptions{Upstream: mock.URL})
		if !waitForModel(t, host, "vllm/CodeAgent", 10*time.Second) {
			t.Fatal("модель не появилась перед chat-запросом")
		}
		state.all401 = true
		response := request(t, http.MethodPost, host.base+"/v1/chat/completions", clientKey, []byte(`{"model":"vllm/CodeAgent","messages":[]}`))
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusBadGateway || state.chatCalls.Load() != 2 || bytes.Contains(body, []byte(deviceCode)) {
			t.Fatalf("status=%d calls=%d", response.StatusCode, state.chatCalls.Load())
		}
	})
	t.Run("устройство-не-авторизовано", func(t *testing.T) {
		state := newMockState()
		state.unauthorized = true
		mock := newMock(t, state)
		defer mock.Close()
		host := startHost(t, hostOptions{Upstream: mock.URL})
		waitForNoModel(t, host, "vllm/CodeAgent", 2*time.Second)
		response := request(t, http.MethodPost, host.base+"/v1/chat/completions", clientKey, []byte(`{"model":"vllm/CodeAgent","messages":[]}`))
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		// Неизвестная модель возвращается как model_not_found, 400: sdk/api/handlers/handlers_routing.go:204-220.
		if response.StatusCode != http.StatusBadRequest || bytes.Contains(body, []byte(deviceCode)) {
			t.Fatalf("status=%d body=%s", response.StatusCode, body)
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			stderr := strings.ToLower(readFile(host.stderr))
			if strings.Contains(stderr, "не авториз") || strings.Contains(stderr, "unauthor") || strings.Contains(stderr, "signin") {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		stderr := readFile(host.stderr)
		stderrLower := strings.ToLower(stderr)
		if (!strings.Contains(stderrLower, "не авториз") && !strings.Contains(stderrLower, "unauthor") && !strings.Contains(stderrLower, "signin")) || strings.Contains(stderr, deviceCode) {
			t.Fatalf("stderr не содержит ожидаемого сообщения: %q", stderr)
		}
		state.unauthorized = false
		if !waitForModel(t, host, "vllm/CodeAgent", 10*time.Second) {
			t.Fatal("модель не появилась после авторизации")
		}
	})
	for _, tc := range []struct {
		name   string
		status int
		want   int
	}{{"400", 400, 400}, {"429", 429, 429}, {"500", 500, 0}} {
		tc := tc
		t.Run("статус-"+tc.name, func(t *testing.T) {
			state := newMockState()
			mock := newMock(t, state)
			defer mock.Close()
			host := startHost(t, hostOptions{Upstream: mock.URL})
			if !waitForModel(t, host, "vllm/CodeAgent", 10*time.Second) {
				t.Fatal("модель не появилась перед chat-запросом")
			}
			state.chatStatus = tc.status
			response := request(t, http.MethodPost, host.base+"/v1/chat/completions", clientKey, []byte(`{"model":"vllm/CodeAgent","messages":[]}`))
			response.Body.Close()
			if tc.want != 0 && response.StatusCode != tc.want || tc.want == 0 && (response.StatusCode < 500 || response.StatusCode > 599) {
				t.Fatalf("status=%d", response.StatusCode)
			}
		})
	}
	t.Run("апстрим-недоступен", func(t *testing.T) {
		state := newMockState()
		state.down.Store(true)
		mock := newMock(t, state)
		defer mock.Close()
		host := startHost(t, hostOptions{Upstream: mock.URL})
		waitForNoModel(t, host, "vllm/CodeAgent", 2*time.Second)
		health, err := http.Get(host.base + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		health.Body.Close()
		if health.StatusCode != http.StatusOK {
			t.Fatalf("health=%d", health.StatusCode)
		}
		state.down.Store(false)
		if !waitForModel(t, host, "vllm/CodeAgent", 10*time.Second) {
			t.Fatal("модель не появилась после восстановления")
		}
		mock.Close()
		response := request(t, http.MethodPost, host.base+"/v1/chat/completions", clientKey, []byte(`{"model":"vllm/CodeAgent","messages":[]}`))
		response.Body.Close()
		if response.StatusCode != http.StatusBadGateway {
			t.Fatalf("status=%d", response.StatusCode)
		}
		health, err = http.Get(host.base + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		health.Body.Close()
		if health.StatusCode != http.StatusOK {
			t.Fatalf("health=%d", health.StatusCode)
		}
	})
	t.Run("обновление-без-трафика", func(t *testing.T) {
		state := newMockState()
		state.expiresIn.Store(3)
		mock := newMock(t, state)
		defer mock.Close()
		host := startHost(t, hostOptions{Upstream: mock.URL})
		if !waitForModel(t, host, "vllm/CodeAgent", 10*time.Second) {
			t.Fatal("модель не появилась перед обновлением")
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) && state.authCalls.Load() < 2 {
			time.Sleep(100 * time.Millisecond)
		}
		if state.authCalls.Load() < 2 {
			t.Fatalf("auth calls=%d", state.authCalls.Load())
		}
	})
	t.Run("модели-обновляются", func(t *testing.T) {
		state := newMockState()
		state.expiresIn.Store(3)
		state.addNewModel.Store(true)
		mock := newMock(t, state)
		defer mock.Close()
		host := startHost(t, hostOptions{Upstream: mock.URL})
		if !waitForModel(t, host, "vllm/CodeAgent", 10*time.Second) {
			t.Fatal("первая модель не появилась")
		}
		state.addNewModel.Store(true)
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) && state.authCalls.Load() < 2 {
			time.Sleep(100 * time.Millisecond)
		}
		if state.authCalls.Load() < 2 || !waitForModel(t, host, "vllm/NewModel", 10*time.Second) {
			t.Fatal("новая модель не появилась")
		}
	})
	t.Run("таймаут-авторизации", func(t *testing.T) {
		state := newMockState()
		mock := newMock(t, state)
		defer mock.Close()
		host := startHost(t, hostOptions{Upstream: mock.URL})
		if !waitForModel(t, host, "vllm/CodeAgent", 10*time.Second) {
			t.Fatal("модель не появилась перед chat-запросом")
		}
		state.blockAuthAfter.Store(1)
		state.all401 = true
		started := time.Now()
		response := requestWithTimeout(t, 45*time.Second, http.MethodPost, host.base+"/v1/chat/completions", clientKey, []byte(`{"model":"vllm/CodeAgent","messages":[]}`))
		response.Body.Close()
		if response.StatusCode != http.StatusBadGateway && response.StatusCode != http.StatusGatewayTimeout {
			t.Fatalf("status=%d", response.StatusCode)
		}
		if time.Since(started) > 10*time.Second {
			t.Fatalf("таймаут превысил предел: %s", time.Since(started))
		}
		if state.authStartedCount.Load() < 2 || state.chatCalls.Load() != 1 {
			t.Fatal("мок не видел повторных авторизаций")
		}
	})
	t.Run("таймаут-зависшего-тела", func(t *testing.T) {
		state := newMockState()
		mock := newMock(t, state)
		defer mock.Close()
		host := startHost(t, hostOptions{Upstream: mock.URL})
		if !waitForModel(t, host, "vllm/CodeAgent", 10*time.Second) {
			t.Fatal("модель не появилась перед проверкой зависшего тела")
		}
		state.all401 = true
		state.hangAuthBody.Store(true)
		for i := 0; i < 2; i++ {
			started := time.Now()
			response := requestWithTimeout(t, 45*time.Second, http.MethodPost, host.base+"/v1/chat/completions", clientKey, []byte(`{"model":"vllm/CodeAgent","messages":[]}`))
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != http.StatusBadGateway && response.StatusCode != http.StatusGatewayTimeout {
				t.Fatalf("запрос %d: status=%d body=%s", i+1, response.StatusCode, body)
			}
			if time.Since(started) > 12*time.Second {
				t.Fatalf("запрос %d завис: %s", i+1, time.Since(started))
			}
		}
		health, err := http.Get(host.base + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		health.Body.Close()
		if health.StatusCode != http.StatusOK {
			t.Fatalf("health=%d", health.StatusCode)
		}
	})
	t.Run("таймаут-фонового-обновления", func(t *testing.T) {
		state := newMockState()
		state.expiresIn.Store(3)
		mock := newMock(t, state)
		defer mock.Close()
		host := startHost(t, hostOptions{Upstream: mock.URL})
		if !waitForModel(t, host, "vllm/CodeAgent", 10*time.Second) {
			t.Fatal("модель не появилась перед фоновым обновлением")
		}
		state.blockAuthAfter.Store(1)
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) && state.authStartedCount.Load() < 2 {
			time.Sleep(100 * time.Millisecond)
		}
		if state.authStartedCount.Load() < 2 {
			t.Fatalf("фоновая авторизация не началась: authStarted=%d authCalls=%d", state.authStartedCount.Load(), state.authCalls.Load())
		}
		started := time.Now()
		response := requestWithTimeout(t, 45*time.Second, http.MethodPost, host.base+"/v1/chat/completions", clientKey, []byte(`{"model":"vllm/CodeAgent","messages":[]}`))
		response.Body.Close()
		if response.StatusCode != http.StatusBadGateway && response.StatusCode != http.StatusGatewayTimeout {
			t.Fatalf("status=%d", response.StatusCode)
		}
		if time.Since(started) > 25*time.Second {
			t.Fatalf("запрос завис: %s", time.Since(started))
		}
		health, err := http.Get(host.base + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		health.Body.Close()
		if health.StatusCode != http.StatusOK {
			t.Fatalf("health=%d", health.StatusCode)
		}
	})
	t.Run("обрыв-клиента", func(t *testing.T) {
		state := newMockState()
		state.mode = "stream"
		state.slowStream.Store(true)
		mock := newMock(t, state)
		defer mock.Close()
		host := startHost(t, hostOptions{Upstream: mock.URL})
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, host.base+"/v1/chat/completions", strings.NewReader(`{"model":"vllm/CodeAgent","messages":[],"stream":true}`))
		req.Header.Set("Authorization", "Bearer "+clientKey)
		response, err := (&http.Client{}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		scanner := bufio.NewScanner(response.Body)
		if !scanner.Scan() {
			t.Fatal("первый кадр не получен")
		}
		cancel()
		response.Body.Close()
		select {
		case <-state.cancelled:
		case <-time.After(3 * time.Second):
			t.Fatalf("хост не передал отмену клиентского контекста моку: frames=%d after_cancel=%d", state.streamFrames.Load(), state.framesAfterCancel.Load())
		}
		followUp := request(t, http.MethodPost, host.base+"/v1/chat/completions", clientKey, []byte(`{"model":"vllm/CodeAgent","messages":[]}`))
		followUpBody, _ := io.ReadAll(followUp.Body)
		followUp.Body.Close()
		if followUp.StatusCode != http.StatusOK || !bytes.Contains(followUpBody, []byte("mock response")) {
			t.Fatalf("следующий запрос: status=%d body=%s", followUp.StatusCode, followUpBody)
		}
	})
	t.Run("гигиена-секретов", func(t *testing.T) {
		state := newMockState()
		mock := newMock(t, state)
		defer mock.Close()
		host := startHost(t, hostOptions{Upstream: mock.URL})
		if !waitForModel(t, host, "vllm/CodeAgent", 10*time.Second) {
			t.Fatal("модель не появилась перед chat-запросом")
		}
		response := request(t, http.MethodPost, host.base+"/v1/chat/completions", clientKey, []byte(`{"model":"vllm/CodeAgent","messages":[]}`))
		responseBody, _ := io.ReadAll(response.Body)
		response.Body.Close()
		state.all401 = true
		errorResponse := request(t, http.MethodPost, host.base+"/v1/chat/completions", clientKey, []byte(`{"model":"vllm/CodeAgent","messages":[]}`))
		errorBody, _ := io.ReadAll(errorResponse.Body)
		errorResponse.Body.Close()
		state.all401 = false
		state.unauthorized = true
		unauthorizedResponse := request(t, http.MethodPost, host.base+"/v1/chat/completions", clientKey, []byte(`{"model":"vllm/CodeAgent","messages":[]}`))
		unauthorizedBody, _ := io.ReadAll(unauthorizedResponse.Body)
		unauthorizedResponse.Body.Close()
		state.unauthorized = false
		upstreamDown := state.down.Swap(true)
		downResponse := request(t, http.MethodPost, host.base+"/v1/chat/completions", clientKey, []byte(`{"model":"vllm/CodeAgent","messages":[]}`))
		downBody, _ := io.ReadAll(downResponse.Body)
		downResponse.Body.Close()
		state.down.Store(upstreamDown)
		state.blockAuthAfter.Store(1)
		timeoutResponse := requestWithTimeout(t, 45*time.Second, http.MethodPost, host.base+"/v1/chat/completions", clientKey, []byte(`{"model":"vllm/CodeAgent","messages":[]}`))
		timeoutBody, _ := io.ReadAll(timeoutResponse.Body)
		timeoutResponse.Body.Close()
		state.blockAuthAfter.Store(0)
		state.hangAuthBody.Store(true)
		state.all401 = true
		bodyTimeoutResponse := requestWithTimeout(t, 45*time.Second, http.MethodPost, host.base+"/v1/chat/completions", clientKey, []byte(`{"model":"vllm/CodeAgent","messages":[]}`))
		bodyTimeoutBody, _ := io.ReadAll(bodyTimeoutResponse.Body)
		bodyTimeoutResponse.Body.Close()
		state.hangAuthBody.Store(false)
		stderr := readFile(host.stderr)
		allBodies := append(append(append(append(responseBody, errorBody...), unauthorizedBody...), downBody...), append(timeoutBody, bodyTimeoutBody...)...)
		if bytes.Contains(allBodies, []byte(deviceCode)) || strings.Contains(stderr, deviceCode) {
			t.Fatal("device_code раскрыт")
		}
		if bytes.Contains(allBodies, []byte(mock.URL)) || bytes.Contains(allBodies, []byte("http://")) {
			t.Fatalf("адрес мока раскрыт в ответах: %s", allBodies)
		}
		state.mu.Lock()
		tokens := append([]string(nil), state.tokens...)
		state.mu.Unlock()
		for _, token := range tokens {
			if bytes.Contains(allBodies, []byte(token)) || strings.Contains(stderr, token) {
				t.Fatalf("токен раскрыт: %s", token)
			}
		}
	})
	t.Run("тело-больше-лимита", func(t *testing.T) {
		state := newMockState()
		mock := newMock(t, state)
		defer mock.Close()
		host := startHost(t, hostOptions{Upstream: mock.URL, MaxBody: 64})
		if !waitForModel(t, host, "vllm/CodeAgent", 10*time.Second) {
			t.Fatal("модель не появилась перед проверкой лимита тела")
		}
		response := request(t, http.MethodPost, host.base+"/v1/chat/completions", clientKey, []byte(`{"model":"vllm/CodeAgent","messages":[{"role":"user","content":"012345678901234567890123456789012345678901234567890123456789"}]}`))
		response.Body.Close()
		if response.StatusCode != http.StatusRequestEntityTooLarge || state.chatCalls.Load() != 0 {
			t.Fatalf("status=%d chat=%d", response.StatusCode, state.chatCalls.Load())
		}
	})
	t.Run("без-ключа", func(t *testing.T) {
		state := newMockState()
		mock := newMock(t, state)
		defer mock.Close()
		host := startHost(t, hostOptions{Upstream: mock.URL})
		response := request(t, http.MethodPost, host.base+"/v1/chat/completions", "", []byte(`{}`))
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status=%d", response.StatusCode)
		}
	})
}

func jsonEqual(left, right map[string]any) bool {
	a, _ := json.Marshal(left)
	b, _ := json.Marshal(right)
	return bytes.Equal(a, b)
}

func jsonEqualValue(left, right any) bool {
	a, _ := json.Marshal(left)
	b, _ := json.Marshal(right)
	return bytes.Equal(a, b)
}
