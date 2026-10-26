# Task Manager API

REST API для командных задач по `ТЗ.pdf`: JWT, роли owner/admin/member, история изменений, комментарии, Redis-кеш и SQL-аналитика. Реализованы также все дополнительные пункты: unit-тесты прав, rate limiting, курсорная пагинация, JSON-логи и circuit breaker.

## Быстрый запуск

Нужны работающий Docker Engine и Docker Compose. Из каталога проекта:

```powershell
Copy-Item .env.example .env
docker compose up --build -d
```

На Linux/macOS вместо первой команды: `cp .env.example .env`.

Приложение: <http://localhost:8080>. Swagger UI: <http://localhost:8080/docs>. Спецификация: <http://localhost:8080/openapi.yaml>. Swagger UI загружает JS/CSS с CDN; спецификация доступна без интернета.

Compose дожидается готовности MySQL, выполняет миграции отдельным сервисом `migrate`, затем запускает API. MySQL и Redis доступны только во внутренней сети Compose. Данные MySQL сохраняются в именованном томе.

```sh
docker compose logs -f app
docker compose ps -a
docker compose down
```

`GET /health/live` проверяет процесс, `GET /health/ready` — доступность MySQL. Недоступность Redis не блокирует API: списки читаются из MySQL.

## Конфигурация

Все настройки передаются через окружение. `.env.example` содержит параметры локального стенда; `.env` исключён из Git.

| Переменная | Назначение / значение по умолчанию |
|---|---|
| `HTTP_PORT` | Порт Compose на хосте, `8080` |
| `HTTP_ADDR` | Адрес HTTP-сервера, `:8080` |
| `MYSQL_DATABASE`, `MYSQL_USER`, `MYSQL_PASSWORD`, `MYSQL_ROOT_PASSWORD` | Настройки контейнера MySQL; примеры в `.env.example` |
| `MYSQL_DSN` | Обязательный DSN при запуске Go напрямую; Compose собирает его из настроек MySQL |
| `JWT_SECRET` | Обязательный секрет, минимум 32 байта |
| `REDIS_ADDR` | `localhost:6379` при прямом запуске; `redis:6379` в Compose |
| `REDIS_PASSWORD`, `REDIS_DB` | Пароль и номер БД Redis; пустой пароль, БД `0` |
| `REDIS_TIMEOUT` | Таймаут обращения к кешу, `200ms` |
| `RATE_AUTH_RPM`, `RATE_AUTH_BURST` | Общий лимит регистрации и входа на IP: `10` в минуту, burst `5` |
| `RATE_API_RPM`, `RATE_API_BURST` | Лимит защищённого API на пользователя: `120` в минуту, burst `20` |
| `LOG_LEVEL` | `debug`, `info`, `warn`, `error`; по умолчанию `info` |

Приложение использует UTC, `utf8mb4` и `parseTime=true` независимо от параметров DSN. Redis TTL фиксирован требованиями задания: 300 секунд. JWT действует один час, обновление токенов не предусмотрено.

Локальная разработка требует Go 1.25 и доступных MySQL/Redis. Можно указать адреса собственных экземпляров через `MYSQL_DSN` и `REDIS_ADDR`, затем выполнить:

```sh
go run ./cmd/api migrate
go run ./cmd/api
```

## API и правила

Все бизнес-маршруты имеют префикс `/api/v1`. Только регистрация и вход доступны без `Authorization: Bearer <token>`.

| Метод | Маршрут | Назначение |
|---|---|---|
| POST | `/register` | Регистрация: `email`, `password`, `name` |
| POST | `/login` | JWT по `email`, `password` |
| POST / GET | `/teams` | Создание команды / свои команды |
| POST | `/teams/{id}/invite` | Добавить существующего пользователя: `email`, необязательная `role` |
| PATCH | `/teams/{id}/members/{user_id}` | Изменить `role` |
| DELETE | `/teams/{id}/members/{user_id}` | Удалить участника |
| POST / GET | `/tasks` | Создание / фильтрованный список задач |
| PUT | `/tasks/{id}` | Обновить переданные поля с обязательной `version` |
| GET | `/tasks/{id}/history` | История изменений |
| POST / GET | `/tasks/{id}/comments` | Добавление / список комментариев |
| GET | `/teams/{id}/stats` | SQL-отчёт команды |

- Owner и admin приглашают, меняют роли и удаляют участников, включая других admin. Owner нельзя удалить или понизить; назначить owner через API нельзя. Перед удалением участника нужно переназначить или снять все его задачи, включая закрытые.
- Любой участник создаёт задачи и оставляет комментарии в своей команде. Owner/admin редактируют все задачи; автор задачи — название, описание, статус и исполнителя; исполнитель — только статус.
- Исполнитель должен состоять в той же команде. Автор задачи определяется по JWT; поля автора и команды при обновлении не принимаются.
- Статусы: `todo`, `in_progress`, `done`. При переходе в `done` фиксируется `closed_at`; при открытии дата очищается. Повторное закрытие фиксирует новую дату.
- Приглашение сразу добавляет зарегистрированного пользователя по email; отправка писем и подтверждение приглашения не предусмотрены. Email приводится к нижнему регистру. Пароль — 8–72 UTF-8 байта, хранится как bcrypt-хеш.
- Права проверяются в бизнес-логике и по актуальным данным MySQL. Чужие ресурсы возвращают `404`; участник с неподходящей ролью — `403`.

### Конкурентность и история

Обновление требует версии из списка задач или ответа создания/обновления:

```json
{"version":1,"status":"done"}
```

`PUT` обновляет только переданные поля. `assignee_id: null` снимает назначение. Устаревшая версия возвращает `409`; клиент должен получить текущее состояние и повторить намеренное изменение. Запрос без фактических изменений не увеличивает версию и не добавляет историю.

Все изменения команды сериализуются блокировкой её строки, затем блокируется изменяемая задача. Это также защищает от гонки между назначением исполнителя и удалением участника. Изменение задачи, её версия, история и ревизия кеша сохраняются одной транзакцией. Создание команды и owner-участника также атомарно.

История содержит автора, UTC-время и `changes` с `old`/`new`. При создании старые значения равны `null`. Комментарии хранятся отдельно и не меняют версию задачи.

### Списки и кеш

```text
GET /api/v1/tasks?team_id=1&status=todo&assignee_id=5&limit=20&offset=0
GET /api/v1/tasks?team_id=1&pagination=cursor&limit=20
GET /api/v1/tasks?team_id=1&pagination=cursor&limit=20&cursor=<next_cursor>
```

`team_id` обязателен. `limit` — от 1 до 100, по умолчанию 20; `offset` — неотрицательный, по умолчанию 0. Сортировка: `created_at DESC, id DESC`. Курсор привязан к команде и фильтрам; параметр `offset` с курсорным режимом запрещён. Курсорный ответ содержит `next_cursor`, если следующая страница существует. Между страницами изменения данных возможны; постоянный снимок на всю цепочку страниц не создаётся. История и комментарии используют `limit/offset`, сортировку по ID по возрастанию.

Redis хранит только список задач. Ключ содержит команду, её ревизию, фильтры и пагинацию. Каждое изменение задачи увеличивает `teams.tasks_revision` в той же MySQL-транзакции. Старые ключи становятся недоступными и исчезают по TTL. На промахе ревизия и задачи читаются в одном снимке `REPEATABLE READ`, поэтому позднее заполнение кеша не записывает старые данные под новой ревизией. Даже попадание в кеш требует проверки участия в команде через MySQL.

### SQL-отчёт

После проверки owner/admin выполняется один SQL-запрос с CTE, JOIN, GROUP BY, агрегатами и функциями дат. Текст запроса: `internal/repository/stats.sql`.

- `tasks_by_status`: все задачи выбранной команды, включая нули для отсутствующих статусов.
- `top_assignees`: максимум три текущих исполнителя по количеству задач в статусе `done` с закрытием в интервале `[now − 30 дней, now]`. Неназначенные задачи исключены; равенство разрешается по `user_id ASC`.
- `average_close_seconds`: среднее `closed_at − created_at` для всех текущих закрытых задач команды; `null`, если таких задач нет.
- `comments_total` и `comments_by_task`: общее количество комментариев и счётчик каждой задачи, включая нули.

Комментарии агрегируются отдельно от остальных метрик, чтобы JOIN не умножал количество задач. Переназначение закрытой задачи меняет её текущего исполнителя и, соответственно, атрибуцию в топе. Повторно открытые задачи не считаются закрытыми.

### Отказоустойчивость и логи

Circuit breaker для Redis открывается после пяти последовательных ошибок на 30 секунд, затем пропускает один пробный запрос. Промах кеша не считается ошибкой. При недоступности Redis API продолжает работать через MySQL; изменение задачи не зависит от Redis.

Rate limiter использует token bucket в памяти одного процесса; при нескольких экземплярах лимиты независимы. IP берётся из TCP-соединения, заголовки `X-Forwarded-For` не считаются доверенными. Удаляются неактивные записи, число ключей ограничено. При превышении возвращаются `429` и `Retry-After`.

JSON-логи содержат request ID, метод, маршрут, статус, время обработки и ошибки зависимостей; JWT и тела запросов не записываются. Request ID возвращается в заголовке `X-Request-ID`.

Ошибки имеют единый вид:

```json
{"error":{"code":"conflict","message":"task version is outdated"}}
```

Коды: `400` — валидация, `401` — аутентификация, `403` — права, `404` — отсутствующий/чужой ресурс, `409` — конфликт, `429` — лимит, `500` — внутренняя ошибка. JSON-запросы требуют `Content-Type: application/json`; неизвестные поля и тела больше 1 MiB отклоняются.

## Тестирование

Unit-тесты не требуют Docker:

```sh
go test ./...
go vet ./...
go test -race ./...
```

Для `-race` нужен C-компилятор. Интеграционные тесты на настоящем MySQL и Redis запускаются в **отдельном Compose-проекте**, чтобы не использовать рабочий том:

```sh
docker compose --env-file .env.example -p taskmanager-test --profile test build tests
docker compose --env-file .env.example -p taskmanager-test --profile test run --rm tests
docker compose --env-file .env.example -p taskmanager-test --profile test down -v
```

Последняя команда удаляет только том тестового Compose-проекта. Контейнер `tests` запускает `go test -race -tags=integration -count=1 ./...`; миграции применяются самим тестовым набором. Интеграционные тесты требуют `TEST_MYSQL_DSN` и `TEST_REDIS_ADDR`; MySQL-пользователь должен иметь права миграций и изменения схемы для временного тестового CHECK-ограничения. Фикстуры удаляют только созданные ими строки, без `TRUNCATE` и очистки чужих данных.

Проверяются SQL-отчёт, пустая команда, граница 30 дней, изоляция команд, конкурентные обновления, атомарный откат при ошибке истории, кеш, отзыв доступа после удаления участника, курсорные страницы и отказ Redis. Unit-тесты проверяют матрицу прав, JWT, rate limiter, TTL и восстановление circuit breaker.

Полный сценарий через HTTP после запуска приложения:

```powershell
./scripts/smoke.ps1
```

Сценарий создаёт двух пользователей, команды, задачу и комментарий, проверяет конфликт версий и запреты доступа, затем переназначение и удаление участника. Созданные демонстрационные данные сохраняются.

## Примеры запросов

```sh
curl -X POST http://localhost:8080/api/v1/register \
  -H 'Content-Type: application/json' \
  -d '{"email":"owner@example.com","password":"password123","name":"Owner"}'

curl -X POST http://localhost:8080/api/v1/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"owner@example.com","password":"password123"}'

# TOKEN — access_token из ответа login.
curl -X POST http://localhost:8080/api/v1/teams \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"Engineering"}'

curl -X POST http://localhost:8080/api/v1/tasks \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"team_id":1,"title":"Finish test assignment"}'

curl -X PUT http://localhost:8080/api/v1/tasks/1 \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"version":1,"status":"done"}'

curl 'http://localhost:8080/api/v1/tasks?team_id=1&limit=20&offset=0' \
  -H "Authorization: Bearer $TOKEN"

curl http://localhost:8080/api/v1/tasks/1/history -H "Authorization: Bearer $TOKEN"
curl http://localhost:8080/api/v1/teams/1/stats -H "Authorization: Bearer $TOKEN"
```

В примерах замените ID на значения из ответов сервера. Полные модели, комментарии, приглашения и управление участниками описаны в Swagger.

## Структура

- `cmd/api` — запуск HTTP-сервера и миграций.
- `internal/httpapi` — HTTP, JSON, middleware и rate limiting.
- `internal/service` — бизнес-правила, авторизация, транзакции и JWT.
- `internal/domain` — модели, ошибки и политики прав.
- `internal/repository` — SQL и отчёт; `internal/cache` — Redis и breaker.
- `migrations`, `docs`, `internal/integration` — схема, OpenAPI и интеграционные тесты.

Миграции встроены в бинарник, содержат Up/Down SQL; команда `api migrate` применяет Up. Для обновления локального стенда: `docker compose up --build -d` — сервис миграций применит только новые версии. Автоматический откат схемы при запуске не выполняется.
