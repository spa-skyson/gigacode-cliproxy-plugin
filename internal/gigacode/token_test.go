package gigacode

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spa-skyson/gigacode-cliproxy-plugin/internal/gigacodemock"
)

// Тест 3: single-flight — 20 параллельных запросов с одним device_code
// выполняют ровно один флоу авторизации.
func TestTokenSingleFlight(t *testing.T) {
	mock := gigacodemock.New()
	defer mock.Close()
	core := newTestCore(testConfig(mock), nil, nil)

	raw := executorRequestJSON(storageFor(testDeviceCode), chatPayload(), nil)

	const workers = 20
	var wg sync.WaitGroup
	var failures atomic.Int64
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			env := decodeEnvelope(t, core.Handle("executor.execute", raw))
			if !env.OK {
				failures.Add(1)
				t.Errorf("запрос упал: %+v", env.Error)
			}
		}()
	}
	close(start)
	wg.Wait()

	if failures.Load() != 0 {
		t.Fatalf("упало запросов: %d", failures.Load())
	}
	if got := mock.AuthCalls(); got != 1 {
		t.Fatalf("выполнено флоу авторизации: %d, ожидался 1", got)
	}
	if got := mock.ChatCalls(); got != workers {
		t.Fatalf("запросов к чату: %d, ожидалось %d", got, workers)
	}
}

// Тест: ожидающий вызов ensure без force получает итог чужого флоу —
// снимок при успехе, ту же ошибку при неудаче; собственный флоу он не
// запускает. Вызов с force после чужого флоу выполняет свой.
func TestEnsureWaitsForeignFlowResult(t *testing.T) {
	store := newTokenStore()

	var runs atomic.Int64
	release := make(chan struct{})
	runner := func(ctx context.Context) (tokenSnapshot, error) {
		runs.Add(1)
		<-release // флоу «в полёте», пока тест не отпустит
		return tokenSnapshot{}, errors.New("апстрим недоступен (инъекция теста)")
	}

	const waiters = 2
	var wg sync.WaitGroup
	errs := make(chan error, waiters)
	start := make(chan struct{})
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := store.ensure(context.Background(), testDeviceCode, 90*time.Second, false, runner)
			errs <- err
		}()
	}
	close(start)
	time.Sleep(50 * time.Millisecond) // оба вызова увидели флоу в полёте
	close(release)
	wg.Wait()

	if got := runs.Load(); got != 1 {
		t.Fatalf("флоу запущен %d раз, ожидался 1", got)
	}
	close(errs)
	for err := range errs {
		if err == nil || err.Error() != "апстрим недоступен (инъекция теста)" {
			t.Fatalf("ожидающий получил не тот итог: %v", err)
		}
	}

	// force после чужого флоу выполняет свой собственный.
	runnerNoHang := func(ctx context.Context) (tokenSnapshot, error) {
		runs.Add(1)
		return tokenSnapshot{Token: "свежий", ExpiresAt: time.Now().Add(10 * time.Minute)}, nil
	}
	snap, err := store.ensure(context.Background(), testDeviceCode, 90*time.Second, true, runnerNoHang)
	if err != nil {
		t.Fatalf("force-вызов упал: %v", err)
	}
	if snap.Token != "свежий" {
		t.Fatalf("force-вызов получил снимок от чужого флоу: %+v", snap)
	}
	if got := runs.Load(); got != 2 {
		t.Fatalf("флоу после force: %d, ожидалось 2", got)
	}
}
