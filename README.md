# Max Notification Service для 1С

Сервис уведомлений на базе Max Bot API для интеграции с 1С.

## Возможности

- Отправка уведомлений пользователям Max через бота
- REST API для интеграции с 1С
- Мониторинг и метрики (Prometheus)
- Распределенная трассировка (OpenTelemetry)
- Логирование (Zap JSON)
- Health/Ready проверки для Kubernetes

## Быстрый старт

### Локальный запуск

```bash
export MAX_BOT_TOKEN="your-bot-token"
go mod tidy
go run main.go
```

### Запуск через Docker

```bash
docker build -t max-notification-service .
docker run -p 8080:8080 -p 9090:9090 \
  -e MAX_BOT_TOKEN=your-bot-token \
  max-notification-service
```

## API

### POST /api/v1/send

Отправка уведомления.

**Запрос:**
```json
{
  "user_id": 123456,
  "message": "Ваше уведомление",
  "source": "1c"
}
```

**Параметры:**
- `user_id` (int64, optional) - ID пользователя Max
- `chat_id` (int64, optional) - ID чата
- `phone_number` (string, optional) - Номер телефона (+7...)
- `message` (string, required) - Текст сообщения
- `request_id` (string, optional) - ID запроса для трассировки
- `source` (string, optional) - Источник запроса (1c, internal, etc.)

**Ответ успешный:**
```json
{
  "success": true,
  "message": "Notification sent",
  "request_id": "1234567890-123",
  "message_id": "msg_abc123"
}
```

**Ответ с ошибкой:**
```json
{
  "success": false,
  "message": "Failed to send notification: Max API error [invalid.user]: User not found",
  "request_id": "1234567890-123"
}
```

### GET /health

Проверка работоспособности сервиса.

### GET /ready

Проверка готовности принимать запросы.

### GET /metrics

Метрики Prometheus.

## Переменные окружения

| Переменная | По умолчанию | Описание |
|------------|--------------|----------|
| SERVER_PORT | 8080 | Порт основного сервера |
| METRICS_PORT | 9090 | Порт метрик |
| MAX_BOT_TOKEN | - | Токен бота Max (обязательно) |
| MAX_BOT_BASE_URL | https://botapi.max.ru | URL Max Bot API |
| OTEL_EXPORTER_OTLP_ENDPOINT | - | Endpoint для трассировки |
| SERVICE_VERSION | 1.0.0 | Версия сервиса |
| LOG_LEVEL | info | Уровень логирования (debug/info/warn/error) |

## Интеграция с 1С

### Пример кода 1С

```bsl
// Параметры
АдресСервиса = "https://notifications.yourdomain.com/api/v1/send";
ТокенБота = "your-bot-token";
IDПользователя = 123456;
ТекстСообщения = "Новое уведомление из 1С";

// Формирование запроса
Запрос = Новый HTTPЗапрос(АдресСервиса);
Заголовки = Запрос.Заголовки;
Заголовки.Вставить("Content-Type", "application/json");

// Тело запроса
Тело = Новый Структура;
Тело.Вставить("user_id", IDПользователя);
Тело.Вставить("message", ТекстСообщения);
Тело.Вставить("source", "1c");
Тело.Вставить("request_id", Строка(Новый УникальныйИдентификатор));

// Сериализация в JSON
ЗаписьJSON = Новый ЗаписьJSON();
ЗаписьJSON.УстановитьСтроку();
ЗаписьJSON.ЗаписатьЗначение(Тело);
JSONСтрока = ЗаписьJSON.Закрыть();

Запрос.УстановитьТелоИзСтроки(JSONСтрока);

// Отправка
HTTPСервис = Новый HTTPСоединение("notifications.yourdomain.com", 443, "", "", 
    Новый ЗащищенноеСоединениеOpenssl());
    
Ответ = HTTPСервис.ОтправитьДляОбработки(Запрос);

// Обработка ответа
Если Ответ.КодСостояния = 200 Тогда
    ЧтениеJSON = Новый ЧтениеJSON();
    ЧтениеJSON.УстановитьСтроку(Ответ.ПолучитьТелоКакСтроку());
    Результат = ПрочитатьJSON(ЧтениеJSON);
    Если Результат.success Тогда
        Сообщить("Уведомление отправлено, ID: " + Результат.message_id);
    Иначе
        Сообщить("Ошибка: " + Результат.message);
    КонецЕсли;
Иначе
    Сообщить("HTTP ошибка: " + Ответ.КодСостояния);
КонецЕсли;
```

## Мониторинг

### Метрики Prometheus

- `notification_requests_total` - Всего запросов
- `notification_requests_failed_total` - Неудачных запросов
- `notification_request_duration_seconds` - Длительность обработки
- `max_api_errors_total` - Ошибки Max Bot API
- `1c_client_errors_total` - Ошибки от 1С клиентов
- `notification_active_requests` - Активные запросы

### Дашборд Grafana

Импортируйте ConfigMap из `kubernetes/monitoring.yaml` для автоматического создания дашборда.

### Alerting Rules

Настроены следующие алерты:
- HighErrorRate - высокий уровень ошибок (>5%)
- MaxAPIErrors - много ошибок Max API (>10 за 5 мин)
- OneCErrors - много ошибок от 1С (>20 за 5 мин)
- HighLatency - высокая задержка (p95 > 2с)
- ServiceDown - сервис недоступен
- LowReplicaCount - мало реплик (<2)

## Деплой в Kubernetes

```bash
# Создать секрет с токеном
kubectl create secret generic max-bot-secret --from-literal=token=your-bot-token

# Применить манифесты
kubectl apply -f kubernetes/deployment.yaml
kubectl apply -f kubernetes/monitoring.yaml
```

## Система отслеживания сбоев

Сервис предоставляет трехуровневую систему мониторинга:

### 1. Уровень сервиса
- Health/Ready endpoints
- Метрики доступности и производительности
- Логирование всех операций

### 2. Уровень 1С клиента
- Счетчик ошибок валидации запросов (`1c_client_errors_total`)
- Логирование некорректных запросов с деталями
- Трассировка до момента валидации

### 3. Уровень Max Bot API
- Счетчик ошибок API (`max_api_errors_total`)
- Детализация по типам ошибок (код ошибки API)
- Трассировка полного пути запроса

### OpenTelemetry Tracing

При настройке `OTEL_EXPORTER_OTLP_ENDPOINT` все запросы трассируются:
- Получение запроса от 1С
- Валидация параметров
- Вызов Max Bot API
- Получение ответа

Экспортируйте трейсы в Jaeger, Tempo или другой OTLP-совместимый бэкенд.

## Лицензия

MIT
