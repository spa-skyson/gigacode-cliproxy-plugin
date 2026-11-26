package gigacode

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Ключи и формат auth-файла GigaCode.
const (
	providerID       = "gigacode"
	pluginLabel      = "GigaCode"
	authFileType     = "gigacode"
	deviceCodeRegexp = `^[0-9a-f]{64}$`
)

var deviceCodeRE = regexp.MustCompile(deviceCodeRegexp)

// canonicalStorageJSON — канонический StorageJSON учётной записи: type,
// device_code и пользовательские ключи label (непустая строка), prefix и
// proxy_url (строки), перенесённые из исходного JSON (src), если они
// присутствуют там с нужным типом. Без переноса остаются посторонние ключи
// и disabled — им управляет хост через метаданные записи (см.
// sdk/auth/filestore.go хоста: при записи файла через метаданные переносится
// только disabled). Такой состав переживает перезапись файла хостом.
func canonicalStorageJSON(deviceCode string, src []byte) []byte {
	payload := map[string]any{
		"type":        authFileType,
		"device_code": deviceCode,
	}
	if len(src) > 0 {
		var extra map[string]any
		if err := json.Unmarshal(src, &extra); err == nil {
			if label, ok := extra["label"].(string); ok && strings.TrimSpace(label) != "" {
				payload["label"] = label
			}
			if prefix, ok := extra["prefix"].(string); ok {
				payload["prefix"] = prefix
			}
			if proxyURL, ok := extra["proxy_url"].(string); ok {
				payload["proxy_url"] = proxyURL
			}
		}
	}
	raw, _ := json.Marshal(payload)
	return raw
}

// parseStorageJSON извлекает device_code из StorageJSON.
// Пустой/невалидный device_code — ошибка invalid_auth_file.
func parseStorageJSON(raw []byte) (string, error) {
	if len(raw) == 0 {
		return "", newErr(codeInvalidAuthFile, "В auth-файле GigaCode нет поля device_code", 502)
	}
	var payload struct {
		Type       string `json:"type"`
		DeviceCode string `json:"device_code"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", newErr(codeInvalidAuthFile, "Auth-файл GigaCode не является корректным JSON: "+err.Error(), 502)
	}
	deviceCode := strings.TrimSpace(payload.DeviceCode)
	if !deviceCodeRE.MatchString(deviceCode) {
		return "", newErr(codeInvalidAuthFile, "device_code в auth-файле GigaCode должен быть строкой из 64 hex-символов", 502)
	}
	return deviceCode, nil
}

// tokenSnapshot — согласованный срез состояния учётной записи.
type tokenSnapshot struct {
	Token     string
	ExpiresAt time.Time
	Models    []map[string]any
	ChatURL   string
}

// fresh проверяет свежесть с запасом refresh-margin-seconds из конфига.
func (s tokenSnapshot) fresh(now time.Time, margin time.Duration) bool {
	return s.Token != "" && now.Before(s.ExpiresAt.Add(-margin))
}

// tokenState — состояние одной учётной записи (ключ — device_code).
type tokenState struct {
	mu       sync.Mutex
	snapshot tokenSnapshot
	loaded   bool
	loading  bool
	lastErr  error         // итог последнего завершённого флоу
	flowDone chan struct{} // закрывается по завершении текущего флоу
}

// tokenStore — все известные состояния. JWT, chat.url и модели живут
// только в памяти процесса.
type tokenStore struct {
	mu     sync.Mutex
	states map[string]*tokenState
}

func newTokenStore() *tokenStore {
	return &tokenStore{states: make(map[string]*tokenState)}
}

// get возвращает (создавая при необходимости) состояние по device_code.
func (s *tokenStore) get(deviceCode string) *tokenState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.states[deviceCode]
	if !ok {
		st = &tokenState{}
		s.states[deviceCode] = st
	}
	return st
}

// peekFresh возвращает свежий снимок, не запуская флоу авторизации.
func (s *tokenStore) peekFresh(deviceCode string, margin time.Duration) (tokenSnapshot, bool) {
	s.mu.Lock()
	st, ok := s.states[deviceCode]
	s.mu.Unlock()
	if !ok {
		return tokenSnapshot{}, false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.loaded && st.snapshot.fresh(time.Now(), margin) {
		return st.snapshot, true
	}
	return tokenSnapshot{}, false
}

// reset сбрасывает все состояния (смена base-url при reconfigure, shutdown).
func (s *tokenStore) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states = make(map[string]*tokenState)
}

// flowRunner — выполняет флоу авторизации и возвращает его результат.
// Разрыв цикла зависимостей: tokenStore не знает о конфиге и транспорте.
type flowRunner func(ctx context.Context) (tokenSnapshot, error)

// ensure возвращает свежий токен, при необходимости выполняя флоу.
// Параллельные вызовы с одним device_code ждут один флоу (single-flight).
// Ожидающий вызов без force получает итог чужого флоу — снимок при успехе,
// ошибку при неудаче; собственный флоу он запускает, только если флоу не был
// запущен на момент проверки. Вызов с force после чужого флоу выполняет свой.
func (s *tokenStore) ensure(ctx context.Context, deviceCode string, margin time.Duration, force bool, run flowRunner) (tokenSnapshot, error) {
	st := s.get(deviceCode)
	for {
		st.mu.Lock()
		if !force && st.loaded && st.snapshot.fresh(time.Now(), margin) {
			snap := st.snapshot
			st.mu.Unlock()
			return snap, nil
		}
		if st.loading {
			// Чужой флоу в полёте: дождаться и взять его итог.
			done := st.flowDone
			st.mu.Unlock()
			select {
			case <-done:
			case <-ctx.Done():
				return tokenSnapshot{}, &transportError{cause: ctx.Err()}
			}
			st.mu.Lock()
			if !force {
				if st.loaded && st.snapshot.fresh(time.Now(), margin) {
					snap := st.snapshot
					st.mu.Unlock()
					return snap, nil
				}
				if st.lastErr != nil {
					err := st.lastErr
					st.mu.Unlock()
					return tokenSnapshot{}, err
				}
			}
			// force либо после чужого уже пошёл новый флоу — решаем на новой
			// итерации: свежий снимок, чужой флоу или свой запуск.
			st.mu.Unlock()
			continue
		}
		// Флоу нет — запускаем свой.
		st.loading = true
		st.lastErr = nil
		st.flowDone = make(chan struct{})
		done := st.flowDone
		st.mu.Unlock()

		snap, err := run(ctx)
		st.mu.Lock()
		if err == nil {
			st.snapshot = snap
			st.loaded = true
		} else {
			st.lastErr = err
		}
		st.loading = false
		st.mu.Unlock()
		close(done)
		if err != nil {
			return tokenSnapshot{}, err
		}
		return snap, nil
	}
}
