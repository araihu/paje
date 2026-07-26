package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/araihu/paje/internal/submission"
	"github.com/araihu/paje/internal/submission/auth"
	"github.com/araihu/paje/internal/template"
	templatecodechange "github.com/araihu/paje/internal/template/codechange"
)

type Dependencies struct {
	Service       *submission.Service
	Authenticator *auth.Authenticator
	Control       http.Handler
	Ready         func(context.Context) error
}

type Option func(*server)

func WithLogger(logger *log.Logger) Option {
	return func(server *server) {
		if logger != nil {
			server.logger = logger
		}
	}
}

func WithRequestTimeout(timeout time.Duration) Option {
	return func(server *server) {
		if timeout > 0 {
			server.requestTimeout = timeout
		}
	}
}

type server struct {
	service        *submission.Service
	authenticator  *auth.Authenticator
	control        http.Handler
	ready          func(context.Context) error
	logger         *log.Logger
	requestTimeout time.Duration
	requestCounter atomic.Uint64
}

func New(dependencies Dependencies, options ...Option) (http.Handler, error) {
	if dependencies.Service == nil {
		return nil, errors.New("create submission HTTP API: service is required")
	}
	if dependencies.Authenticator == nil {
		return nil, errors.New("create submission HTTP API: authenticator is required")
	}
	result := &server{
		service: dependencies.Service, authenticator: dependencies.Authenticator,
		control: dependencies.Control, ready: dependencies.Ready,
		logger: log.New(io.Discard, "", 0), requestTimeout: 15 * time.Second,
	}
	for _, option := range options {
		if option != nil {
			option(result)
		}
	}
	return result, nil
}

func (s *server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	requestID := s.requestID()
	writer.Header().Set("X-Request-ID", requestID)
	destination := &statusWriter{ResponseWriter: writer}
	tracked := newBufferedResponse(maxResponseBytes)
	credentialID := "anonymous"
	defer func() {
		recovered := recover()
		if recovered != nil || tracked.overflow {
			writeError(destination, errors.New("request processing failed"))
		} else {
			tracked.flushTo(destination)
		}
		s.logger.Printf(
			"request_id=%s credential_id=%s method=%s route=%s status=%d",
			requestID, credentialID, request.Method, routeLabel(request.URL.Path), destination.Status(),
		)
	}()

	ctx, cancel := context.WithTimeout(request.Context(), s.requestTimeout)
	defer cancel()
	request = request.WithContext(ctx)

	if request.URL.Path == "/healthz" {
		s.handleHealth(tracked, request)
		return
	}
	if request.URL.Path == "/readyz" {
		s.handleReady(tracked, request)
		return
	}
	if authorizationSize(request) > maxAuthorizationBytes {
		writeError(tracked, errHeaderTooLarge)
		return
	}
	principal, err := s.authenticate(request)
	if err != nil {
		writeError(tracked, err)
		return
	}
	credentialID = principal.CredentialID
	request = request.WithContext(auth.ContextWithPrincipal(request.Context(), principal))

	if request.URL.Path == "/v1/capabilities" || strings.HasPrefix(request.URL.Path, "/v1/control-runs") {
		if s.control == nil {
			writeError(tracked, submission.ErrNotFound)
			return
		}
		s.control.ServeHTTP(tracked, request)
		return
	}
	s.handleSubmission(tracked, request, principal)
}

func (s *server) authenticate(request *http.Request) (submission.Principal, error) {
	values := request.Header.Values("Authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") ||
		strings.Count(values[0], " ") != 1 {
		return submission.Principal{}, auth.ErrUnauthenticated
	}
	return s.authenticator.Authenticate(strings.TrimPrefix(values[0], "Bearer "))
}

func (s *server) handleHealth(writer http.ResponseWriter, request *http.Request) {
	if len(request.URL.Query()) != 0 {
		writeError(writer, errInvalidRequest)
		return
	}
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *server) handleReady(writer http.ResponseWriter, request *http.Request) {
	if len(request.URL.Query()) != 0 {
		writeError(writer, errInvalidRequest)
		return
	}
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	if s.ready != nil {
		if err := s.ready(request.Context()); err != nil {
			writeError(writer, submission.ErrProviderUnavailable)
			return
		}
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *server) handleSubmission(
	writer http.ResponseWriter,
	request *http.Request,
	principal submission.Principal,
) {
	if len(request.URL.Query()) != 0 {
		writeError(writer, errInvalidRequest)
		return
	}
	if request.URL.Path == "/v1/submissions" {
		if request.Method != http.MethodPost {
			methodNotAllowed(writer, http.MethodPost)
			return
		}
		s.handleSubmit(writer, request, principal)
		return
	}
	const prefix = "/v1/submissions/"
	if !strings.HasPrefix(request.URL.Path, prefix) {
		writeError(writer, submission.ErrNotFound)
		return
	}
	remainder := strings.TrimPrefix(request.URL.Path, prefix)
	parts := strings.Split(remainder, "/")
	if len(parts) == 0 || !safePathID(parts[0]) {
		writeError(writer, submission.ErrNotFound)
		return
	}
	if len(parts) == 1 {
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, http.MethodGet)
			return
		}
		s.handleInspect(writer, request, principal, parts[0])
		return
	}
	if len(parts) == 2 && parts[1] == "cancel" {
		if request.Method != http.MethodPost {
			methodNotAllowed(writer, http.MethodPost)
			return
		}
		s.handleCancel(writer, request, principal, parts[0])
		return
	}
	writeError(writer, submission.ErrNotFound)
}

func (s *server) handleSubmit(writer http.ResponseWriter, request *http.Request, principal submission.Principal) {
	if err := requireJSON(request); err != nil {
		writeError(writer, err)
		return
	}
	key, err := requireIdempotencyKey(request)
	if err != nil {
		writeError(writer, err)
		return
	}
	var envelope struct {
		Template template.ID       `json:"template"`
		Origin   submission.Origin `json:"origin"`
		Input    json.RawMessage   `json:"input"`
	}
	if err := decodeJSON(writer, request, &envelope); err != nil {
		writeError(writer, err)
		return
	}
	if envelope.Template == templatecodechange.ID {
		if err := auth.ValidateExactJSON(envelope.Input, &templatecodechange.Input{}); err != nil {
			writeError(writer, errInvalidRequest)
			return
		}
	}
	view, reused, err := s.service.Submit(request.Context(), principal, submission.SubmitRequest{
		IdempotencyKey: key, Template: envelope.Template, Input: envelope.Input, Origin: envelope.Origin,
	})
	if err != nil {
		writeError(writer, err)
		return
	}
	status := http.StatusAccepted
	if reused {
		status = http.StatusOK
	}
	writeJSON(writer, status, responseForView(view, &reused))
}

func (s *server) handleInspect(
	writer http.ResponseWriter,
	request *http.Request,
	principal submission.Principal,
	runID string,
) {
	view, err := s.service.Inspect(request.Context(), principal, runID)
	if err != nil {
		writeError(writer, err)
		return
	}
	if !terminalSubmissionStatus(view.Status) {
		writer.Header().Set("Retry-After", "1")
	}
	writeJSON(writer, http.StatusOK, responseForView(view, nil))
}

func (s *server) handleCancel(
	writer http.ResponseWriter,
	request *http.Request,
	principal submission.Principal,
	runID string,
) {
	if err := requireJSON(request); err != nil {
		writeError(writer, err)
		return
	}
	if _, err := requireIdempotencyKey(request); err != nil {
		writeError(writer, err)
		return
	}
	var body struct{}
	if err := decodeJSON(writer, request, &body); err != nil {
		writeError(writer, err)
		return
	}
	view, newlyRequested, err := s.service.Cancel(request.Context(), principal, runID)
	if err != nil {
		writeError(writer, err)
		return
	}
	status := http.StatusOK
	if newlyRequested {
		status = http.StatusAccepted
	}
	writeJSON(writer, status, responseForView(view, nil))
}

func methodNotAllowed(writer http.ResponseWriter, methods ...string) {
	writer.Header().Set("Allow", strings.Join(methods, ", "))
	writeJSON(writer, http.StatusMethodNotAllowed, errorBody{Error: errorDetail{
		Code: "invalid_request", Message: "the method is not allowed for this route",
	}})
}

func authorizationSize(request *http.Request) int {
	total := 0
	for _, value := range request.Header.Values("Authorization") {
		total += len(value)
	}
	return total
}

func safePathID(value string) bool {
	if value == "" || len(value) > maxPathIDBytes || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func terminalSubmissionStatus(status submission.Status) bool {
	switch status {
	case submission.StatusSucceeded, submission.StatusFailed, submission.StatusCanceled, submission.StatusDeclined:
		return true
	default:
		return false
	}
}

func routeLabel(requestPath string) string {
	switch {
	case requestPath == "/healthz":
		return "health"
	case requestPath == "/readyz":
		return "ready"
	case requestPath == "/v1/capabilities":
		return "capabilities"
	case strings.HasPrefix(requestPath, "/v1/control-runs"):
		return "control"
	case strings.HasPrefix(requestPath, "/v1/submissions"):
		return "submission"
	default:
		return "unlisted"
	}
}

func (s *server) requestID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return hex.EncodeToString(raw[:])
	}
	return "fallback-" + strings.ToLower(hex.EncodeToString([]byte{
		byte(s.requestCounter.Add(1) >> 56), byte(s.requestCounter.Load() >> 48),
		byte(s.requestCounter.Load() >> 40), byte(s.requestCounter.Load() >> 32),
		byte(s.requestCounter.Load() >> 24), byte(s.requestCounter.Load() >> 16),
		byte(s.requestCounter.Load() >> 8), byte(s.requestCounter.Load()),
	}))
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

type bufferedResponse struct {
	header      http.Header
	status      int
	body        bytes.Buffer
	limit       int
	overflow    bool
	wroteHeader bool
}

func newBufferedResponse(limit int) *bufferedResponse {
	return &bufferedResponse{header: make(http.Header), limit: limit}
}

func (w *bufferedResponse) Header() http.Header { return w.header }

func (w *bufferedResponse) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
}

func (w *bufferedResponse) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.body.Len()+len(body) > w.limit {
		w.overflow = true
		return 0, errors.New("response exceeds the allowed size")
	}
	return w.body.Write(body)
}

func (w *bufferedResponse) flushTo(destination http.ResponseWriter) {
	for name, values := range w.header {
		for _, value := range values {
			destination.Header().Add(name, value)
		}
	}
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	destination.WriteHeader(status)
	_, _ = destination.Write(w.body.Bytes())
}

func (w *statusWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (w *statusWriter) Status() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

var jsonDecoder = func(reader io.Reader) *json.Decoder { return json.NewDecoder(reader) }
