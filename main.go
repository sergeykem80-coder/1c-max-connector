package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.uber.org/zap"
)

// ChatStore хранилище информации о чатах
type ChatStore struct {
	mu     sync.RWMutex
	chats  map[int64]*ChatInfo // key: chat_id
}

// ChatInfo информация о чате
type ChatInfo struct {
	ChatID    int64
	Secret    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewChatStore создает новое хранилище чатов
func NewChatStore() *ChatStore {
	return &ChatStore{
		chats: make(map[int64]*ChatInfo),
	}
}

// Save сохраняет информацию о чате
func (cs *ChatStore) Save(chatID int64, secret string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	
	now := time.Now()
	if existing, ok := cs.chats[chatID]; ok {
		existing.Secret = secret
		existing.UpdatedAt = now
	} else {
		cs.chats[chatID] = &ChatInfo{
			ChatID:    chatID,
			Secret:    secret,
			CreatedAt: now,
			UpdatedAt: now,
		}
	}
}

// Get получает информацию о чате
func (cs *ChatStore) Get(chatID int64) (*ChatInfo, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	info, ok := cs.chats[chatID]
	return info, ok
}

var chatStore *ChatStore

// Config хранит конфигурацию сервиса
type Config struct {
	ServerPort      string
	MetricsPort     string
	MaxBotToken     string
	MaxBotBaseURL   string
	OTLPEndpoint    string
	ServiceVersion  string
	LogLevel        string
}

// NotificationRequest запрос на отправку уведомления от 1С
type NotificationRequest struct {
	UserID      int64  `json:"user_id,omitempty"`
	ChatID      int64  `json:"chat_id,omitempty"`
	PhoneNumber string `json:"phone_number,omitempty"`
	Message     string `json:"message"`
	RequestID   string `json:"request_id,omitempty"`
	Source      string `json:"source,omitempty"` // "1c", "internal", etc.
}

// Response ответ API
type Response struct {
	Success   bool   `json:"success"`
	Message   string `json:"message,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	MessageID string `json:"message_id,omitempty"`
}

// Metrics хранит метрики Prometheus
type Metrics struct {
	requestsTotal    prometheus.Counter
	requestsFailed   prometheus.Counter
	requestDuration  prometheus.Histogram
	maxAPIErrors     prometheus.Counter
	oneCErrors       prometheus.Counter
	activeRequests   prometheus.Gauge
}

// Service основной сервис уведомлений
type Service struct {
	config  *Config
	logger  *zap.Logger
	metrics *Metrics
	tracer  interface{}
	client  *http.Client
}

var (
	metrics *Metrics
	logger  *zap.Logger
	cfg     *Config
)

func main() {
	// Загрузка конфигурации
	cfg = loadConfig()

	// Инициализация логгера
	var err error
	logger, err = initLogger(cfg.LogLevel)
	if err != nil {
		fmt.Printf("Failed to initialize logger: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync()

	// Инициализация метрик
	metrics = initMetrics()

	// Инициализация трассировки
	shutdown, err := initTracer(cfg)
	if err != nil {
		logger.Warn("Failed to initialize tracer", zap.Error(err))
	} else if shutdown != nil {
		defer shutdown()
	}

	// Создание HTTP клиента для Max Bot API
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	// Инициализация хранилища чатов
	chatStore = NewChatStore()

	service := &Service{
		config:  cfg,
		logger:  logger,
		metrics: metrics,
		client:  client,
	}

	// Регистрация обработчиков на одном сервере
	mux := http.NewServeMux()
	mux.HandleFunc("/health", service.healthHandler)
	mux.HandleFunc("/ready", service.readyHandler)
	mux.HandleFunc("/api/v1/send", service.sendNotificationHandler)
	mux.HandleFunc("/webhook/bot", service.webhookHandler) // Обработчик вебхука от бота
	mux.Handle("/metrics", promhttp.Handler())

	// Запуск единого сервера
	addr := ":" + cfg.ServerPort
	logger.Info("Starting notification service",
		zap.String("address", addr),
		zap.String("version", cfg.ServiceVersion))

	if err := http.ListenAndServe(addr, mux); err != nil {
		logger.Fatal("Server failed", zap.Error(err))
	}
}

func loadConfig() *Config {
	return &Config{
		ServerPort:     getEnv("SERVER_PORT", "8080"),
		MetricsPort:    getEnv("METRICS_PORT", "9090"),
		MaxBotToken:    getEnv("MAX_BOT_TOKEN", ""),
		MaxBotBaseURL:  getEnv("MAX_BOT_BASE_URL", "https://platform-api.max.ru"),
		OTLPEndpoint:   getEnv("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		ServiceVersion: getEnv("SERVICE_VERSION", "1.0.0"),
		LogLevel:       getEnv("LOG_LEVEL", "info"),
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func initLogger(level string) (*zap.Logger, error) {
	config := zap.NewProductionConfig()
	config.Encoding = "json"
	config.OutputPaths = []string{"stdout"}
	config.ErrorOutputPaths = []string{"stderr"}

	switch level {
	case "debug":
		config.Level = zap.NewAtomicLevelAt(zap.DebugLevel)
	case "info":
		config.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
	case "warn":
		config.Level = zap.NewAtomicLevelAt(zap.WarnLevel)
	case "error":
		config.Level = zap.NewAtomicLevelAt(zap.ErrorLevel)
	default:
		config.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
	}

	return config.Build()
}

func initMetrics() *Metrics {
	return &Metrics{
		requestsTotal: promauto.NewCounter(prometheus.CounterOpts{
			Name: "notification_requests_total",
			Help: "Total number of notification requests",
		}),
		requestsFailed: promauto.NewCounter(prometheus.CounterOpts{
			Name: "notification_requests_failed_total",
			Help: "Total number of failed notification requests",
		}),
		requestDuration: promauto.NewHistogram(prometheus.HistogramOpts{
			Name:    "notification_request_duration_seconds",
			Help:    "Duration of notification requests in seconds",
			Buckets: prometheus.DefBuckets,
		}),
		maxAPIErrors: promauto.NewCounter(prometheus.CounterOpts{
			Name: "max_api_errors_total",
			Help: "Total number of Max Bot API errors",
		}),
		oneCErrors: promauto.NewCounter(prometheus.CounterOpts{
			Name: "onec_client_errors_total",
			Help: "Total number of 1C client errors",
		}),
		activeRequests: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "notification_active_requests",
			Help: "Number of active notification requests",
		}),
	}
}

func initTracer(cfg *Config) (func(), error) {
	if cfg.OTLPEndpoint == "" {
		return nil, nil
	}

	exporter, err := otlptracehttp.New(
		context.Background(),
		otlptracehttp.WithEndpointURL(cfg.OTLPEndpoint),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create trace exporter: %w", err)
	}

	res, err := resource.New(context.Background(),
		resource.WithAttributes(
			semconv.ServiceName("max-notification-service"),
			semconv.ServiceVersion(cfg.ServiceVersion),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)

	otel.SetTracerProvider(tp)

	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tp.Shutdown(ctx); err != nil {
			logger.Error("Tracer provider shutdown failed", zap.Error(err))
		}
	}, nil
}

func (s *Service) healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(Response{Success: true, Message: "healthy"})
}

func (s *Service) readyHandler(w http.ResponseWriter, r *http.Request) {
	// Проверка готовности: токен настроен и Max API доступен
	w.Header().Set("Content-Type", "application/json")

	if s.config.MaxBotToken == "" {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(Response{Success: false, Message: "MAX_BOT_TOKEN not configured"})
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(Response{Success: true, Message: "ready"})
}

func (s *Service) sendNotificationHandler(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	metrics.activeRequests.Inc()
	defer metrics.activeRequests.Dec()

	metrics.requestsTotal.Inc()

	// Создание контекста с трассировкой
	ctx, span := otel.Tracer("max-notification-service").Start(r.Context(), "sendNotification")
	defer span.End()

	span.SetAttributes(
		attribute.String("http.method", r.Method),
		attribute.String("http.url", r.URL.Path),
	)

	// Только POST метод
	if r.Method != http.MethodPost {
		span.SetStatus(codes.Error, "Method not allowed")
		metrics.requestsFailed.Inc()
		metrics.oneCErrors.Inc()
		s.sendJSONResponse(w, http.StatusMethodNotAllowed, Response{
			Success: false,
			Message: "Method not allowed, use POST",
		})
		return
	}

	// Чтение тела запроса
	body, err := io.ReadAll(r.Body)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Failed to read request body")
		metrics.requestsFailed.Inc()
		metrics.oneCErrors.Inc()
		s.logger.Error("Failed to read request body", zap.Error(err))
		s.sendJSONResponse(w, http.StatusBadRequest, Response{
			Success: false,
			Message: "Failed to read request body",
		})
		return
	}
	defer r.Body.Close()

	// Парсинг запроса
	var req NotificationRequest
	if err := json.Unmarshal(body, &req); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Invalid JSON")
		metrics.requestsFailed.Inc()
		metrics.oneCErrors.Inc()
		s.logger.Error("Invalid JSON", zap.Error(err), zap.ByteString("body", body))
		s.sendJSONResponse(w, http.StatusBadRequest, Response{
			Success: false,
			Message: "Invalid JSON format",
		})
		return
	}

	// Валидация
	if req.Message == "" {
		err := fmt.Errorf("message is required")
		span.RecordError(err)
		span.SetStatus(codes.Error, "Validation failed")
		metrics.requestsFailed.Inc()
		metrics.oneCErrors.Inc()
		s.logger.Warn("Validation failed", zap.String("reason", "empty message"))
		s.sendJSONResponse(w, http.StatusBadRequest, Response{
			Success: false,
			Message: "Message field is required",
		})
		return
	}

	if req.UserID == 0 && req.ChatID == 0 && req.PhoneNumber == "" {
		err := fmt.Errorf("user_id, chat_id or phone_number is required")
		span.RecordError(err)
		span.SetStatus(codes.Error, "Validation failed")
		metrics.requestsFailed.Inc()
		metrics.oneCErrors.Inc()
		s.logger.Warn("Validation failed", zap.String("reason", "no recipient specified"))
		s.sendJSONResponse(w, http.StatusBadRequest, Response{
			Success: false,
			Message: "One of user_id, chat_id, or phone_number must be specified",
		})
		return
	}

	// Генерация RequestID если не передан
	if req.RequestID == "" {
		req.RequestID = generateRequestID()
	}

	span.SetAttributes(
		attribute.String("request.id", req.RequestID),
		attribute.Int64("user.id", req.UserID),
		attribute.Int64("chat.id", req.ChatID),
		attribute.String("source", req.Source),
	)

	s.logger.Info("Processing notification request",
		zap.String("request_id", req.RequestID),
		zap.Int64("user_id", req.UserID),
		zap.Int64("chat_id", req.ChatID),
		zap.String("source", req.Source),
	)

	// Отправка сообщения через Max Bot API
	messageID, err := s.sendToMaxBot(ctx, req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Max Bot API error")
		metrics.requestsFailed.Inc()
		metrics.maxAPIErrors.Inc()

		// Определение типа ошибки
		errorType := "unknown"
		if apiErr, ok := err.(*MaxAPIError); ok {
			errorType = apiErr.Code
		}

		s.logger.Error("Failed to send to Max Bot",
			zap.String("request_id", req.RequestID),
			zap.String("error_type", errorType),
			zap.Error(err),
		)

		s.sendJSONResponse(w, http.StatusBadGateway, Response{
			Success:   false,
			Message:   fmt.Sprintf("Failed to send notification: %v", err),
			RequestID: req.RequestID,
		})
		return
	}

	duration := time.Since(startTime).Seconds()
	metrics.requestDuration.Observe(duration)

	s.logger.Info("Notification sent successfully",
		zap.String("request_id", req.RequestID),
		zap.String("message_id", messageID),
		zap.Float64("duration_sec", duration),
	)

	s.sendJSONResponse(w, http.StatusOK, Response{
		Success:   true,
		Message:   "Notification sent",
		RequestID: req.RequestID,
		MessageID: messageID,
	})
}

func (s *Service) sendToMaxBot(ctx context.Context, req NotificationRequest) (string, error) {
	// Формирование тела запроса к Max Bot API
	bodyData := map[string]interface{}{
		"text": req.Message,
	}

	// Сериализация тела запроса в JSON
	jsonBody, err := json.Marshal(bodyData)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request body: %w", err)
	}

	// Построение URL с user_id в query параметре
	var url string
	if req.UserID != 0 {
		url = fmt.Sprintf("%s/messages?user_id=%d", s.config.MaxBotBaseURL, req.UserID)
	} else if req.ChatID != 0 {
		url = fmt.Sprintf("%s/messages?chat_id=%d", s.config.MaxBotBaseURL, req.ChatID)
	} else if req.PhoneNumber != "" {
		url = fmt.Sprintf("%s/messages?phone_number=%s", s.config.MaxBotBaseURL, req.PhoneNumber)
	} else {
		return "", fmt.Errorf("no recipient specified")
	}

	// Создание HTTP запроса с JSON телом
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(jsonBody))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", "max-notification-service/1.0.0")
	httpReq.Header.Set("Authorization", s.config.MaxBotToken)

	// Выполнение запроса
	resp, err := s.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	// Чтение ответа
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	// Обработка ошибок API
	if resp.StatusCode != http.StatusOK {
		var apiErr MaxAPIError
		if err := json.Unmarshal(respBody, &apiErr); err != nil {
			return "", &MaxAPIError{
				Code:    fmt.Sprintf("HTTP_%d", resp.StatusCode),
				Message: string(respBody),
			}
		}
		apiErr.HTTPCode = resp.StatusCode
		return "", &apiErr
	}

	// Парсинг успешного ответа
	var result struct {
		Message struct {
			Mid string `json:"mid"`
		} `json:"message"`
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	return result.Message.Mid, nil
}

// MaxAPIError ошибка от Max Bot API
type MaxAPIError struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	HTTPCode int    `json:"-"`
}

func (e *MaxAPIError) Error() string {
	return fmt.Sprintf("Max API error [%s]: %s (HTTP %d)", e.Code, e.Message, e.HTTPCode)
}

func (s *Service) sendJSONResponse(w http.ResponseWriter, status int, response Response) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(response)
}

// WebhookEvent событие от вебхука бота
type WebhookEvent struct {
	UpdateID int64           `json:"update_id"`
	Message  *BotMessage     `json:"message,omitempty"`
	Callback *CallbackQuery  `json:"callback_query,omitempty"`
}

// BotMessage сообщение от бота
type BotMessage struct {
	MessageID int64  `json:"message_id"`
	From      *User  `json:"from,omitempty"`
	Chat      *Chat  `json:"chat"`
	Date      int64  `json:"date"`
	Text      string `json:"text,omitempty"`
	StartParam string `json:"start_param,omitempty"` // параметр из deeplink
}

// User пользователь
type User struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name,omitempty"`
	Username  string `json:"username,omitempty"`
}

// Chat чат
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

// CallbackQuery callback запрос
type CallbackQuery struct {
	ID      string     `json:"id"`
	From    *User      `json:"from"`
	Message *BotMessage `json:"message,omitempty"`
	Data    string     `json:"data"`
}

// webhookHandler обработчик вебхуков от бота
func (s *Service) webhookHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.logger.Warn("Webhook: method not allowed", zap.String("method", r.Method))
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.logger.Error("Webhook: failed to read body", zap.Error(err))
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var event WebhookEvent
	if err := json.Unmarshal(body, &event); err != nil {
		s.logger.Error("Webhook: invalid JSON", zap.Error(err), zap.ByteString("body", body))
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// Быстро отдаем OK
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})

	// Обработка события в горутине
	go s.processWebhookEvent(event)
}

// processWebhookEvent обрабатывает событие вебхука
func (s *Service) processWebhookEvent(event WebhookEvent) {
	ctx := context.Background()

	// Обработка сообщения со стартом (deeplink)
	if event.Message != nil && event.Message.Text == "/start" {
		s.handleStartCommand(ctx, event.Message)
		return
	}

	// Обработка callback query (если нужно)
	if event.Callback != nil {
		s.handleCallbackQuery(ctx, event.Callback)
		return
	}

	s.logger.Debug("Webhook: unhandled event type")
}

// handleStartCommand обрабатывает команду /start с deeplink
func (s *Service) handleStartCommand(ctx context.Context, msg *BotMessage) {
	chatID := msg.Chat.ID
	
	// Извлекаем secret из start_param (deeplink параметр)
	secret := ""
	if msg.StartParam != "" {
		secret = msg.StartParam
	} else if len(msg.Text) > 6 { // "/start " + secret
		parts := strings.SplitN(msg.Text, " ", 2)
		if len(parts) == 2 {
			secret = parts[1]
		}
	}

	s.logger.Info("Webhook: /start command received",
		zap.Int64("chat_id", chatID),
		zap.String("secret", secret),
		zap.String("username", msg.From.Username),
	)

	// Сохраняем secret и chat_id в хранилище
	if secret != "" {
		chatStore.Save(chatID, secret)
		s.logger.Info("Webhook: chat info saved",
			zap.Int64("chat_id", chatID),
			zap.String("secret", secret),
		)
	}

	// Отправляем приветственное сообщение
	if err := s.sendWelcomeMessage(ctx, chatID); err != nil {
		s.logger.Error("Webhook: failed to send welcome message",
			zap.Int64("chat_id", chatID),
			zap.Error(err),
		)
	}
}

// handleCallbackQuery обрабатывает callback запросы
func (s *Service) handleCallbackQuery(ctx context.Context, callback *CallbackQuery) {
	chatID := callback.Message.Chat.ID
	
	s.logger.Debug("Webhook: callback query received",
		zap.Int64("chat_id", chatID),
		zap.String("callback_data", callback.Data),
	)

	// Здесь можно добавить логику обработки callback
}

// sendWelcomeMessage отправляет приветственное сообщение
func (s *Service) sendWelcomeMessage(ctx context.Context, chatID int64) error {
	message := "Спасибо за подписку!"

	req := NotificationRequest{
		ChatID:  chatID,
		Message: message,
		Source:  "bot_welcome",
	}

	_, err := s.sendToMaxBot(ctx, req)
	if err != nil {
		return fmt.Errorf("failed to send welcome message: %w", err)
	}

	s.logger.Info("Welcome message sent", zap.Int64("chat_id", chatID))
	return nil
}

func generateRequestID() string {
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), time.Now().UnixMilli()%1000)
}
