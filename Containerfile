# Сборка плагина gigacode для CLIProxyAPI.
# Образ по умолчанию закреплён по digest: glibc образа совместим с хостом.
ARG BUILD_IMAGE=golang:1.26-bookworm@sha256:79ecd0a7157c7a4cf3b281ced8465350591bf54fedab540e75374a003e1fde7e

FROM ${BUILD_IMAGE} AS build
WORKDIR /src

# Версия плагина: подставляется линковщику в переменную
# internal/gigacode.version и попадает в метаданные регистрации.
ARG VERSION=0.1.0

# Кэш модулей: слой go mod download переиспользуется, пока go.mod/go.sum не менялись.
COPY go.mod go.sum ./
RUN go mod download

COPY main.go ./
COPY cmd ./cmd
COPY internal ./internal

# Те же флаги, что и в локальной сборке: результат — один файл артефакта.
RUN mkdir -p /out && \
    CGO_ENABLED=1 GOAMD64=v1 CGO_CFLAGS="-march=x86-64" \
    go build -trimpath -buildvcs=false \
      -ldflags "-X github.com/spa-skyson/gigacode-cliproxy-plugin/internal/gigacode.version=${VERSION}" \
      -buildmode=c-shared -o /out/gigacode.so . && \
    rm -f /out/gigacode.h

# Итог: только артефакт.
FROM scratch
COPY --from=build /out/gigacode.so /gigacode.so
