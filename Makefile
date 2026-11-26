# Сборка плагина gigacode для CLIProxyAPI.
#
# Версия задаётся в одном месте (VERSION) и передаётся линковщику в
# переменную internal/gigacode.version: от неё зависят метаданные
# регистрации плагина и имя поставочного архива.
#
# Все сборки требуют символы не выше GLIBC_2.34 (проверено objdump -T
# по артефакту). Локальная (build) — быстрая, для разработки и тестов.
# build-in-container — та же команда сборки, что в Containerfile, но
# напрямую: выполняется внутри готового контейнера на CI. Поставка
# (dist) собирается в контейнере по Containerfile: воспроизводимая и
# совпадающая по линии Go с хостом.

VERSION ?= 0.1.0
BUILD_IMAGE ?= golang:1.26-bookworm@sha256:79ecd0a7157c7a4cf3b281ced8465350591bf54fedab540e75374a003e1fde7e
PODMAN ?= podman

GO ?= go
GOFLAGS := CGO_ENABLED=1 GOAMD64=v1
CGOFLAGS := CGO_CFLAGS="-march=x86-64"
BUILDFLAGS := -trimpath -buildvcs=false
LDFLAGS := -X github.com/spa-skyson/gigacode-cliproxy-plugin/internal/gigacode.version=$(VERSION)

.PHONY: build build-in-container package dist test vet fmt clean

build:
	@mkdir -p dist
	$(GOFLAGS) $(CGOFLAGS) $(GO) build $(BUILDFLAGS) -ldflags "$(LDFLAGS)" -buildmode=c-shared -o dist/gigacode.so .
	@rm -f dist/gigacode.h

# Сборка напрямую, без контейнера — для CI, где шаг уже выполняется
# внутри готового контейнера сборки. Флаги совпадают с Containerfile.
build-in-container:
	@mkdir -p dist
	CGO_ENABLED=1 GOAMD64=v1 CGO_CFLAGS="-march=x86-64" \
		$(GO) build $(BUILDFLAGS) -ldflags "$(LDFLAGS)" -buildmode=c-shared -o dist/gigacode.so .
	@rm -f dist/gigacode.h

# Поставка из собранного dist/gigacode.so: архив магазина плагинов и
# checksums.txt. Самостоятельная сборка библиотеки не выполняется.
package:
	$(GO) run ./cmd/package -version $(VERSION) -in dist/gigacode.so -out dist

test:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	@out="$$(gofmt -l .)"; \
	if [ -n "$$out" ]; then \
		echo "gofmt -l нашёл неформатированные файлы:"; echo "$$out"; exit 1; \
	fi; \
	echo "gofmt: чисто"

clean:
	@rm -rf dist

# Сборка поставки в контейнере с закреплённым образом: podman build по
# Containerfile (репозиторий не монтируется на запись, SELinux-метки не
# отключаются), затем извлечение артефакта из одноразового контейнера.
# Кэш модулей хоста подключается томом только для чтения (podman 6.x
# поддерживает --volume у build). Тег образа уникален для каждой сборки.
# Очистка — через trap EXIT: при сбое любой команды контейнер и образ
# удаляются, исходный код возврата сохраняется и не маскируется.
# Идентификатор контейнера обнуляется только после подтверждённого удаления;
# trap снимается после подтверждённого удаления и контейнера, и образа.
# Если удаление не удалось, cleanup повторяет его с теми же идентификаторами,
# и цель завершается с ненулевым кодом.
dist:
	@mkdir -p dist
	@tag="gigacode-plugin-build:$$$$"; \
	cid=""; \
	cleanup() { rc=$$?; if [ -n "$$cid" ]; then $(PODMAN) rm "$$cid" >/dev/null 2>&1; fi; $(PODMAN) rmi "$$tag" >/dev/null 2>&1; exit $$rc; }; \
	trap cleanup EXIT; \
	$(PODMAN) build --build-arg BUILD_IMAGE=$(BUILD_IMAGE) \
		--build-arg VERSION=$(VERSION) \
		--volume "$$($(GO) env GOMODCACHE)":/go/pkg/mod:ro \
		-f Containerfile -t "$$tag" . || exit $$?; \
	cid=$$($(PODMAN) create "$$tag") || exit $$?; \
	$(PODMAN) cp $$cid:/gigacode.so dist/gigacode.so || exit $$?; \
	$(PODMAN) rm $$cid >/dev/null || exit $$?; \
	cid=""; \
	$(PODMAN) rmi "$$tag" >/dev/null || exit $$?; \
	trap - EXIT
