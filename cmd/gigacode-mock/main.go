// Утилита gigacode-mock: мок апстрима GigaCode на заданном адресе.
// Служит для ручной проверки плагина.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spa-skyson/gigacode-cliproxy-plugin/internal/gigacodemock"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18090", "адрес прослушивания мока")
	expiresIn := flag.Int("expires-in", 600, "expires_in в ответе авторизации, секунды")
	flag.Parse()

	mock := gigacodemock.New()
	mock.SetExpiresIn(*expiresIn)
	server := &http.Server{
		Addr:    *addr,
		Handler: mock.Handler(),
	}

	done := make(chan struct{})
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-signals
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		mock.Close()
		close(done)
	}()

	log.Printf("Мок GigaCode слушает http://%s/api/v2 (auth: %s, чат: %s)", *addr, "/api/v2/auth/plugin", "/api/v2/chat/completions")
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("Мок упал: %v", err)
	}
	<-done
}
