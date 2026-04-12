package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
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

	service := &Service{
		config:  cfg,
		logger:  logger,
		metrics: metrics,
		client:  client,
	}

	// Регистрация обработчиков
	http.HandleFunc("/health", service.healthHandler)
	http.HandleFunc("/ready", service.readyHandler)
	http.HandleFunc("/api/v1/send", service.sendNotificationHandler)
	http.Handle("/metrics", promhttp.Handler())

	// Запуск сервера метрик
	go func() {
		addr := ":" + cfg.MetricsPort
		logger.Info("Starting metrics server", zap.String("address", addr))
		if err := http.ListenAndServe(addr, nil); err != nil {
			logger.Fatal("Metrics server failed", zap.Error(err))
		}
	}()

	// Запуск основного сервера
	addr := ":" + cfg.ServerPort
	logger.Info("Starting notification service",
		zap.String("address", addr),
		zap.String("version", cfg.ServiceVersion))

	if err := http.ListenAndServe(addr, nil); err != nil {
		logger.Fatal("Server failed", zap.Error(err))
	}
}

func loadConfig() *Config {
	return &Config{
		ServerPort:     getEnv("SERVER_PORT", "8080"),
		MetricsPort:    getEnv("METRICS_PORT", "9090"),
		MaxBotToken:    getEnv("MAX_BOT_TOKEN", ""),
		MaxBotBaseURL:  getEnv("MAX_BOT_BASE_URL", "https://botapi.max.ru"),
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
			Name: "1c_client_errors_total",
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
		otlptracehttp.WithEndpoint(cfg.OTLPEndpoint),
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
	// Формирование запроса к Max Bot API
	maxReq := map[string]interface{}{
		"text": req.Message,
		"notify": true,
	}

	// Добавление получателя
	values := make(map[string]string)
	if req.UserID != 0 {
		values["userId"] = strconv.FormatInt(req.UserID, 10)
	} else if req.ChatID != 0 {
		values["chatId"] = strconv.FormatInt(req.ChatID, 10)
	} else if req.PhoneNumber != "" {
		values["phoneNumbers"] = req.PhoneNumber
	}

	// Построение URL
	url := fmt.Sprintf("%s/messages?access_token=%s&version=0.0.10", 
		s.config.MaxBotBaseURL, s.config.MaxBotToken)
	
	for key, val := range values {
		url += fmt.Sprintf("&%s=%s", key, val)
	}

	// Создание HTTP запроса
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", "max-notification-service/"+cfg.ServiceVersion)

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

func generateRequestID() string {
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), time.Now().UnixMilli()%1000)
}
