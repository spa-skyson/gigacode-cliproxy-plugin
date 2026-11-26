# GigaCode — плагин CLIProxyAPI

[![CI](https://github.com/spa-skyson/gigacode-cliproxy-plugin/actions/workflows/ci.yml/badge.svg)](https://github.com/spa-skyson/gigacode-cliproxy-plugin/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Latest release](https://img.shields.io/github/v/release/spa-skyson/gigacode-cliproxy-plugin)](https://github.com/spa-skyson/gigacode-cliproxy-plugin/releases/latest)

This repository provides the native `gigacode.so` plugin for CLIProxyAPI. It adds GigaCode as a separate provider, obtains and refreshes the account token, exposes models, and proxies chat requests in regular and SSE modes. Install it through the CLIProxyAPI plugin store. Documentation below is in Russian.

Нативный плагин `gigacode.so` для CLIProxyAPI v8.1.2. Он регистрирует GigaCode
как отдельного провайдера, получает краткоживущий JWT по `device_code`, обновляет
его и проксирует чат в обычном и SSE-режимах.

Схема:

```
Клиент → CLIProxyAPI → плагин gigacode → GigaCode
```

## Требования

- CLIProxyAPI v8.1.2, собранный с CGO и поддержкой плагинов. Признак поддержки:
  заголовок `X-CPA-SUPPORT-PLUGIN: 1` в ответе management API.
- Плагины включены в конфигурации хоста.

## Сборка

`make build` собирает `dist/gigacode.so` локально для тестов. `make dist` собирает
`dist/gigacode.so` в Podman-контейнере по `Containerfile`; образ сборки по умолчанию
закреплён по digest. Для зеркала реестра задайте `BUILD_IMAGE`:

```bash
make BUILD_IMAGE=registry.example/golang:1.26-bookworm@sha256:<digest> dist
```

`make package` упаковывает уже собранный `dist/gigacode.so` в поставочный архив
магазина плагинов `dist/gigacode_<версия>_linux_amd64.zip` и `dist/checksums.txt`.
Версия по умолчанию — `0.1.0`; переопределяется переменной `VERSION`, она же
подставляется в метаданные плагина при сборке.

Проверки:

```bash
make test  # go test -race ./...
make vet   # go vet ./...
make fmt   # проверка gofmt
```

## Установка

Основной способ — через магазин плагинов CLIProxyAPI (схема `github-release`).

Добавьте источник магазина в конфиг хоста:

```yaml
plugins:
  enabled: true
  store-sources:
    - "https://raw.githubusercontent.com/spa-skyson/gigacode-cliproxy-plugin/main/registry.json"
```

Затем установите плагин в панели управления хоста (раздел магазина плагинов)
либо запросом с ключом management:

```text
POST /v0/management/plugin-store/gigacode/install
```

Хост берёт последний релиз репозитория, проверяет checksums.txt и кладёт
библиотеку в `<plugins.dir>/linux/amd64/gigacode-v<версия>.so`; плагин включается
в конфиге и загружается без перезапуска хоста.

Первая установка всё равно требует auth-файла — он описан в разделе
«Учётная запись». Если та же версия уже загружена, хост отвечает
`409 plugin_update_requires_restart`; новая версия подхватывается на лету.

Ручная установка — запасной способ: скачайте
`gigacode_<версия>_linux_amd64.zip` из релизов, распакуйте `gigacode.so`
в каталог `plugins.dir` и перезапустите хост.

## Отказ от ответственности

Проект не связан с GigaCode и CLIProxyAPI и не одобрен ими. `device_code` даёт
доступ к аккаунту GigaCode, поэтому храните его как пароль и не публикуйте.
Используя плагин, вы обязуетесь соблюдать условия обслуживания GigaCode.
Автор не отвечает за блокировку аккаунта или изменения API.

## Конфигурация

```yaml
plugins:
  enabled: true
  dir: <каталог плагинов хоста>
  configs:
    gigacode:
      enabled: true
      priority: 1
      base-url: https://gigacode.ru/api/v2
      env-name: vscode
      plugin-name: vscode-kilo-plugin
      plugin-version: "26.9.66"
      env-version: "1.105.1"
      locale: ru
      login: app-user
      refresh-margin-seconds: 90
      auth-timeout-seconds: 30
      retry-seconds: 30
      stream-idle-timeout-seconds: 600
      max-body-bytes: 33554432
      allow-insecure-http: false
```

| Ключ | Тип | Диапазон | По умолчанию |
| --- | --- | --- | --- |
| `base-url` | string | абсолютный `https` или loopback `http`; другой `http` — только с `allow-insecure-http` | `https://gigacode.ru/api/v2` |
| `env-name` | string | — | `vscode` |
| `plugin-name` | string | — | `vscode-kilo-plugin` |
| `plugin-version` | string | — | `26.9.66` |
| `env-version` | string | — | `1.105.1` |
| `locale` | string | — | `ru` |
| `login` | string | — | имя пользователя ОС или `unknown` |
| `refresh-margin-seconds` | integer | `0..86400` | `90` |
| `auth-timeout-seconds` | integer | `1..600` | `30` |
| `retry-seconds` | integer | `1..3600` | `30` |
| `stream-idle-timeout-seconds` | integer | `1..86400` | `600` |
| `max-body-bytes` | integer | `1..1073741824` | `33554432` |
| `allow-insecure-http` | boolean | — | `false` |

Назначение ключей: `base-url` — базовый URL апстрима; `env-name` — поле
`env_name` в теле авторизации; `plugin-name` — поле `plugin_name`; `plugin-version`
— поле `plugin_version` и часть User-Agent; `env-version` — поле `env_version`;
`locale` — поле `locale`; `login` — поле `login`; `refresh-margin-seconds` —
запас, при котором JWT считается устаревшим; `auth-timeout-seconds` — таймаут
каждого запроса авторизации; `retry-seconds` — интервал повтора фонового
обновления токена после ошибки, при этом внутри одной попытки шаги авторизации
повторяются сразу; `stream-idle-timeout-seconds` — предел простоя SSE-потока без
прогресса; `max-body-bytes` — лимит тела запроса к модели;
`allow-insecure-http` — разрешение открытого `http` к любому хосту.

Неизвестные ключи игнорируются.

## Учётная запись

Auth-файл содержит обязательные поля `type` и `device_code`; для этой записи
`type` имеет значение `gigacode`, а `device_code` — строка из 64 строчных
hex-символов. Необязательные поля: `disabled`, `label`, `prefix`, `proxy_url`.

`device_code` появляется после входа через плагин GigaCode в VSCode: установите
плагин, выполните вход и возьмите значение из `~/.gigacode_vsc/device_id`.

```json
{
  "type": "gigacode",
  "device_code": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
  "label": "GigaCode",
  "disabled": false
}
```

При первой перезаписи хост сохраняет канонические поля `type` и `device_code`,
а также заданные пользователем `label`, `prefix` и `proxy_url`. Поле `disabled` и
служебные ключи `priority` и `weight` хост добавляет сам; посторонние ключи из
файла удаляются. JWT,
`chat.url` и список моделей находятся только в памяти процесса.

## Поведение

Для получения токена плагин выполняет `GET {base-url}/config`, берёт URL
авторизации из `X-Authorization-URL`, отправляет туда `POST` с `device_code` и
параметрами конфигурации, затем получает `/config` с Bearer-токеном. Из ответа
берутся модели и `chat.url`.

Токен обновляется заранее: срок действия уменьшается на
`refresh-margin-seconds`. При ошибке следующая попытка назначается через
`retry-seconds`; сам auth-флоу повторяется один раз при временной ошибке.

Список моделей формируется из `chat.models[]`; в каталог попадают записи с
непустым строковым полем `model`. При неудаче авторизации возвращается пустой
список, а причина записывается в журнал хоста.
После каждого фонового обновления токена список моделей обновляется; новые модели
появляются в `/v1/models` без перезапуска хоста.

Для чата тело запроса передаётся без изменений. При ответе `401` плагин один
раз обновляет токен и повторяет запрос. Если после повтора апстрим снова вернул
`401`, клиент получает `502` с сообщением
`Апстрим GigaCode отклонил токен после переавторизации`.

SSE-поток закрывается, если прогресса нет дольше
`stream-idle-timeout-seconds`.
Если клиент отключается посреди SSE-потока, хост отменяет запрос к апстриму.

После ответа `408`, `500`, `502`, `503` или `504` хост по умолчанию ставит
учётную запись на паузу на минуту; в это время клиент получает `503` с кодом
ошибки `auth_unavailable`. Пауза настраивается ключом
`routing.cooldown.transient-error-cooldown-seconds`.

| Ситуация на апстриме | Статус клиенту | Сообщение клиенту |
| --- | ---: | --- |
| первый `401` | запрос повторяется после переавторизации | клиент не получает ошибку |
| повторный `401` | `502` | `Апстрим GigaCode отклонил токен после переавторизации` |
| `403`, `429` или `500+` | тот же статус | `Апстрим GigaCode вернул статус <N>: <текст>` |
| учётная запись на паузе после `408`, `500`, `502`, `503` или `504` | `503` | код ошибки `auth_unavailable` |
| другой `4xx` | тот же статус | `Запрос отклонён апстримом GigaCode (<N>): <текст>` |
| другой статус вне `2xx`, `4xx` и `5xx` | тот же статус | `Апстрим GigaCode вернул неожиданный статус <N>: <текст>` |
| устройство не авторизовано | `502` | `Устройство не авторизовано в GigaCode — выполните вход через плагин один раз` |
| таймаут авторизации | `504` | `Таймаут запроса авторизации GigaCode` |
| ошибка транспорта при авторизации | `502` | `Не удалось получить токен GigaCode` |
| транспортная ошибка при запросе чата | `502` | `Апстрим GigaCode недоступен` |
| ответ авторизации превышает лимит | `502` | `Не удалось получить токен GigaCode` |
| ответ чата превышает лимит | `502` | `Ответ апстрима GigaCode превышает лимит` |

Текст ответа апстрима очищается и ограничивается 512 байтами.

## Безопасность и остаточные риски

- Если сервер авторизации ответит `307` или `308` на другой origin, HTTP-клиент
  хоста повторит `POST` вместе с `device_code`. Политику редиректов задаёт
  апстрим.
- Если конфиг апстрима вернёт абсолютный `chat.url`, JWT уйдёт по этому адресу.
  Разрешены только `https` и `http` на loopback, если не включён
  `allow-insecure-http`.
- Очищенный текст ошибки апстрима до 512 байт передаётся клиенту для `4xx` и
  `5xx`; содержимое контролирует апстрим.
- `login` по умолчанию — имя пользователя ОС; в контейнере это может быть
  `root` или `unknown`. Задайте `login` явно при необходимости.
- При `expires_in` около секунды фоновое обновление может идти примерно раз в
  секунду; при отказе апстрима интервал задаётся `retry-seconds`.
- Лимит тела применяется после буферизации хостом; для чата хост может
  буферизовать до 64 МиБ на запрос.
- Если устройство не авторизовано, модели GigaCode не появляются в
  `/v1/models`. Причина есть только в журнале хоста: предупреждение
  `Устройство не авторизовано в GigaCode…`; в панели управления запись может
  выглядеть активной. После входа восстановление выполняется по расписанию
  `retry-seconds`.

## Проверка и диагностика

```bash
curl -H "Authorization: Bearer $MANAGEMENT_KEY" \
  http://127.0.0.1:<port>/v0/management/plugins
```

Проверьте плагин `gigacode` и заголовок `X-CPA-SUPPORT-PLUGIN: 1`. Плагин пишет
через журнал хоста сообщения об обновлении токена, авторизации, моделях и ошибках.

Мок апстрима:

```bash
go run ./cmd/gigacode-mock
go run ./cmd/gigacode-mock -addr 127.0.0.1:18091 -expires-in 3
```

Флаги: `-addr` (по умолчанию `127.0.0.1:18090`) и `-expires-in` (по умолчанию
`600` секунд).

## Выпуск версии

Версия берётся из имени тега: тег `v0.1.0` даёт версию `0.1.0`. При push тега
`v*` срабатывает workflow Release: выполняется сборка и все тесты, включая
интеграционный, затем архив и `checksums.txt` публикуются в релизе.

Переменная `VERSION` в `Makefile` задаёт версию только для локальной сборки.
Имя архива — `gigacode_<версия>_linux_amd64.zip`, в корне архива — один файл
`gigacode.so`.

## Интеграционный тест

Тест имеет build-тег `integration`. Нужны CLIProxyAPI, собранный из исходников,
и плагин:

Соберите CLIProxyAPI из репозитория версии v8.1.2 с включённым CGO:

```bash
CGO_ENABLED=1 go build -o <путь>/CLIProxyAPI ./cmd/server
```

```bash
make dist
CLIPROXY_BIN=<путь>/CLIProxyAPI \
GIGACODE_PLUGIN_SO=$PWD/dist/gigacode.so \
go test -tags=integration ./integration -v
```

Тест создаёт временный конфиг хоста, каталог плагинов и auth-файл с
`device_code`, запускает хост с `--local-model` и проверяет management API,
авторизацию, модели, чат, SSE и повтор после `401`.
Тест автоматически выполняется в GitHub Actions при каждом изменении: workflow
собирает хост v8.1.2 и выполняет полный прогон.
