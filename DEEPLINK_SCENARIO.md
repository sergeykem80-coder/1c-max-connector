# Сценарий работы с deeplink бота

## Описание сценария

Пользователь запускает бота через deeplink, сервис получает событие через вебхук, быстро отдаёт ответ OK, сохраняет secret и chat_id в хранилище, и только потом отправляет приветственное сообщение.

## Архитектура

```
┌─────────────┐     ┌──────────────────┐     ┌─────────────────┐
│   Telegram  │────▶│  Webhook Handler  │────▶│    ChatStore    │
│    Bot API  │     │  (быстрый OK)     │     │  (secret+chat)  │
└─────────────┘     └──────────────────┘     └─────────────────┘
                           │
                           ▼
                    (в горутине)
                           │
                           ▼
                    ┌─────────────────┐
                    │ Welcome Message │
                    │  (отложенная)   │
                    └─────────────────┘
```

## Как это работает

### 1. Deeplink формат

Пользователь переходит по ссылке вида:
```
https://t.me/your_bot?start=SECRET123
```

Где `SECRET123` - уникальный идентификатор (например, ID пользователя в вашей системе, токен приглашения и т.д.)

### 2. Обработка вебхука

**Шаг 1: Быстрый ответ**
- Получаем POST запрос от Telegram Bot API на `/webhook/bot`
- Парсим JSON события
- **Немедленно** возвращаем `{"ok": true}` (HTTP 200)
- Это важно для таймаутов Telegram (они ждут ответ максимум 60 секунд)

**Шаг 2: Асинхронная обработка**
- В горутине обрабатываем событие
- Извлекаем `start_param` из сообщения
- Сохраняем пару `(chat_id, secret)` в хранилище
- Отправляем приветственное сообщение "Спасибо за подписку!"

### 3. Хранение данных

Данные хранятся в памяти в структуре `ChatStore`:
- `chat_id` - идентификатор чата для отправки сообщений
- `secret` - параметр из deeplink
- `created_at` / `updated_at` - временные метки

**Важно**: В production замените in-memory хранилище на Redis/PostgreSQL для персистентности.

## API Endpoints

### POST /webhook/bot

Обработчик вебхуков от Telegram Bot API.

**Тело запроса** (пример):
```json
{
  "update_id": 123456789,
  "message": {
    "message_id": 1,
    "from": {
      "id": 987654321,
      "first_name": "John",
      "username": "john_doe"
    },
    "chat": {
      "id": 987654321,
      "type": "private"
    },
    "date": 1704067200,
    "text": "/start SECRET123",
    "start_param": "SECRET123"
  }
}
```

**Ответ**:
```json
{"ok": true}
```

## Настройка вебхука в Telegram

Установите вебхук через Telegram Bot API:

```bash
curl -X POST "https://api.telegram.org/bot<BOT_TOKEN>/setWebhook" \
  -H "Content-Type: application/json" \
  -d '{
    "url": "https://your-domain.com/webhook/bot"
  }'
```

Для проверки:
```bash
curl "https://api.telegram.org/bot<BOT_TOKEN>/getWebhookInfo"
```

## Пример использования

### 1. Запуск сервиса

```bash
export SERVER_PORT=8080
export MAX_BOT_TOKEN=your_token
export MAX_BOT_BASE_URL=https://botapi.max.ru
./notification-service
```

### 2. Создание deeplink

Сгенерируйте ссылку для пользователя:
```
https://t.me/your_bot?start=user_12345
```

### 3. Мониторинг событий

В логах вы увидите:
```json
{"level":"info","msg":"Webhook: /start command received","chat_id":987654321,"secret":"user_12345","username":"john_doe"}
{"level":"info","msg":"Webhook: chat info saved","chat_id":987654321,"secret":"user_12345"}
{"level":"info","msg":"Welcome message sent","chat_id":987654321}
```

## Расширение функциональности

### Отправка уведомлений позже

Используйте сохранённые данные для отправки уведомлений:

```go
// Получаем chat_id по secret
if info, ok := chatStore.Get("user_12345"); ok {
    req := NotificationRequest{
        ChatID:  info.ChatID,
        Message: "Ваше уведомление",
        Source:  "notification",
    }
    s.sendToMaxBot(ctx, req)
}
```

### Добавление персистентного хранилища

Замените `ChatStore` на реализацию с Redis:

```go
type RedisChatStore struct {
    client *redis.Client
}

func (cs *RedisChatStore) Save(chatID int64, secret string) {
    key := fmt.Sprintf("chat:%d", chatID)
    cs.client.Set(ctx, key, secret, 24*time.Hour)
}
```

## Метрики

Сервис экспортирует метрики Prometheus на `/metrics`:
- `notification_requests_total` - всего запросов
- `notification_requests_failed_total` - неудачных запросов
- `notification_request_duration_seconds` - длительность запросов

## Трассировка

Поддерживается OpenTelemetry для трассировки запросов.
Настройте через переменные окружения:
- `OTEL_EXPORTER_OTLP_ENDPOINT` - endpoint OTLP коллектора
- `SERVICE_VERSION` - версия сервиса

## Безопасность

1. **Проверка токена**: Добавьте проверку `X-Telegram-Bot-Api-Secret-Token` заголовка
2. **Rate limiting**: Ограничьте количество запросов от одного IP
3. **Валидация secret**: Проверяйте формат secret перед сохранением

## Production рекомендации

1. **Персистентность**: Используйте Redis/PostgreSQL вместо in-memory хранилища
2. **Репликация**: Для high-load добавьте репликацию хранилища
3. **Мониторинг**: Настройте алерты на ошибки отправки сообщений
4. **Логирование**: Увеличьте уровень логирования до `info` или `warn`
5. **Graceful shutdown**: Обрабатывайте SIGTERM для корректного завершения
