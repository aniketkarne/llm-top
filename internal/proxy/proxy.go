// Package proxy implements the OpenAI-compatible reverse proxy. It accepts
// inbound HTTP requests on ListenAddr, rewrites the request to the configured
// upstream (UpstreamBaseURL), injects the API key, intercepts SSE streaming
// responses, and records per-request metrics into the shared Recorder and
// Buffer.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	ring "github.com/aniketkarne-com/llm-top/internal/buffer"
	"github.com/aniketkarne-com/llm-top/internal/metrics"
	"github.com/aniketkarne-com/llm-top/internal/pricing"
	"github.com/aniketkarne-com/llm-top/internal/redactor"
	"github.com/aniketkarne-com/llm-top/internal/sse"
)

// RequestInserter is the subset of *store.Store that the proxy uses to persist
// v0.2.0 structured Request records. Defining it as an interface here keeps
// the proxy package free of an import cycle on the store package.
type RequestInserter interface {
	Insert(r Request) error
}

// PostPersistHook is invoked after every successful persist (i.e.
// after the Request has been written to the store). The hook is
// passed the Request that was just persisted. Used by main.go to
// wire the anomaly detector into the proxy hot path without
// coupling the proxy package to the anomaly package.
//
// The hook is intentionally `func(Request)` rather than an interface
// so this package stays out of the anomaly import graph entirely.
// Anomaly types flow through the wire as opaque values when needed.
type PostPersistHook func(Request)

// Server is the proxy HTTP server with attached state.
type Server struct {
	cfg            Config
	recorder       *metrics.Recorder
	ring           *ring.Buffer
	red            *redactor.Redactor
	client         *http.Client
	prices         pricing.Table
	store          RequestInserter
	postPersist    PostPersistHook
	metricsHandler http.Handler // optional; mounted at /metrics when set

	totalRequests atomic.Uint64
	totalErrors   atomic.Uint64
}

// Config is the proxy's view of runtime configuration.
type Config struct {
	ListenAddr      string
	UpstreamBaseURL string
	UpstreamAPIKey  string
	BufferSize      int
	SessionID       string
	Pricing         pricing.Table
}

// New constructs a Server. The Recorder, Buffer, and Redactor are shared with
// the TUI so both views observe the same state. The store is optional; when
// non-nil, every proxied request is persisted as a structured Request.
func New(cfg Config, rec *metrics.Recorder, ring *ring.Buffer, red *redactor.Redactor) *Server {
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = 500
	}
	tr := &http.Transport{
		Proxy:              http.ProxyFromEnvironment,
		MaxIdleConns:       100,
		IdleConnTimeout:    90 * time.Second,
		DisableCompression: true,
	}
	prices := cfg.Pricing
	if prices.IsZero() {
		prices = pricing.Defaults()
	}
	return &Server{
		cfg:      cfg,
		recorder: rec,
		ring:     ring,
		red:      red,
		prices:   prices,
		store:    nil, // set via WithStore if persistence is desired
		client:   &http.Client{Transport: tr},
	}
}

// WithStore attaches a RequestStore. Returns the receiver for chaining.
func (s *Server) WithStore(st RequestInserter) *Server {
	s.store = st
	return s
}

// WithPostPersistHook registers a callback that fires after every
// successful store Insert. Returns the receiver for chaining.
//
// Typical use: main.go wires a hook that runs the anomaly detector
// and persists any anomalies it detects, keeping the proxy package
// decoupled from internal/anomaly.
func (s *Server) WithPostPersistHook(h PostPersistHook) *Server {
	s.postPersist = h
	return s
}

// WithMetricsHandler attaches an http.Handler that will be served
// at GET /metrics. Typical use is prom.Handler(rec, src); main.go
// owns the wiring so the proxy package doesn't import internal/prom.
// Pass nil to disable the metrics endpoint.
func (s *Server) WithMetricsHandler(h http.Handler) *Server {
	s.metricsHandler = h
	return s
}

// Handler returns the http.Handler that serves proxy traffic.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/", s.handleProxy)
	mux.HandleFunc("/", s.handleRoot)
	if s.metricsHandler != nil {
		mux.Handle("/metrics", s.metricsHandler)
	}
	return mux
}

// ListenAndServe starts the HTTP server on ListenAddr. Blocks until the
// server exits. Cancelling ctx triggers graceful shutdown.
func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.ListenAddr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return ctx.Err()
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// Stats returns runtime counters.
func (s *Server) Stats() (total, errs uint64) {
	return s.totalRequests.Load(), s.totalErrors.Load()
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"service":     "llm-top",
			"version":     "0.2.0",
			"upstream":    s.cfg.UpstreamBaseURL,
			"requests":    s.totalRequests.Load(),
			"errors":      s.totalErrors.Load(),
			"buffer_used": s.ring.Len(),
			"buffer_cap":  s.ring.Cap(),
		})
		return
	}
	http.NotFound(w, r)
}

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	s.totalRequests.Add(1)
	start := time.Now()

	rec := Request{
		ID:        NewID(),
		StartedAt: start.UTC(),
		Method:    r.Method,
		Path:      r.URL.Path,
		Upstream:  s.cfg.UpstreamBaseURL,
		Provider:  ProviderFromUpstream(s.cfg.UpstreamBaseURL),
		SessionID: s.cfg.SessionID,
	}
	// X-Request-Id so clients can correlate immediately. If the client sent
	// one, honor it; otherwise use our freshly generated id.
	clientID := r.Header.Get("X-Request-Id")
	if clientID != "" {
		rec.ID = clientID
	}
	w.Header().Set("X-Request-Id", rec.ID)

	upstream, err := url.Parse(s.cfg.UpstreamBaseURL)
	if err != nil {
		http.Error(w, "bad upstream config", http.StatusInternalServerError)
		s.totalErrors.Add(1)
		rec.Error = "bad upstream config"
		rec.StatusCode = http.StatusInternalServerError
		rec.EndedAt = time.Now().UTC()
		rec.TotalMillis = rec.EndedAt.Sub(rec.StartedAt).Milliseconds()
		s.persist(rec)
		return
	}
	target := *r.URL
	target.Scheme = upstream.Scheme
	target.Host = upstream.Host

	var reqBody []byte
	if r.Body != nil {
		reqBody, _ = io.ReadAll(r.Body)
		_ = r.Body.Close()
	}
	rec.Model = modelFromBody(reqBody)
	rec.PromptHash = HashPrompt(reqBody)
	redacted := s.red.Apply(string(reqBody))
	rec.RequestBody = redacted
	if s.ring != nil {
		s.ring.Append("request", rec.Model, redacted)
	}
	inTok := metrics.EstimateTokens(string(reqBody))
	rec.PromptTokens = inTok

	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bytes.NewReader(reqBody))
	if err != nil {
		http.Error(w, "build upstream request: "+err.Error(), http.StatusInternalServerError)
		s.totalErrors.Add(1)
		s.recordError(&rec, r, http.StatusInternalServerError, inTok, "build upstream: "+err.Error())
		return
	}
	for k, vv := range r.Header {
		for _, v := range vv {
			upReq.Header.Add(k, v)
		}
	}
	upReq.Host = upstream.Host
	if s.cfg.UpstreamAPIKey != "" {
		upReq.Header.Set("Authorization", "Bearer "+s.cfg.UpstreamAPIKey)
	}
	if accept := upReq.Header.Get("Accept"); accept == "" {
		upReq.Header.Set("Accept", "application/json, text/event-stream")
	}

	resp, err := s.client.Do(upReq)
	if err != nil {
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		s.totalErrors.Add(1)
		s.recordError(&rec, r, http.StatusBadGateway, inTok, err.Error())
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		switch strings.ToLower(k) {
		case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
			"te", "trailers", "transfer-encoding", "upgrade":
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	// Preserve the request id from the upstream if it sent one, but our
	// own header (set earlier) wins.
	w.Header().Set("X-Request-Id", rec.ID)

	isStream := isStreamingResponse(resp)
	rec.Stream = isStream
	w.WriteHeader(resp.StatusCode)
	rec.StatusCode = resp.StatusCode

	if isStream {
		s.handleStream(w, resp, r, &rec, start, inTok)
	} else {
		s.handleJSON(w, resp, r, &rec, start, inTok)
	}
}

// handleStream copies the upstream SSE bytes to the client while measuring
// time-to-first-token, accumulating the textual response, and recording the
// total response time.
func (s *Server) handleStream(w http.ResponseWriter, resp *http.Response, r *http.Request, rec *Request, start time.Time, inTok int) {
	flusher, _ := w.(http.Flusher)

	var (
		outBuilder  strings.Builder
		firstToken  time.Duration
		firstSet    bool
		previewDone bool
	)

	parser := sse.New(resp.Body)
	for {
		ev, err := parser.NextEvent()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, sse.ErrDone) {
				break
			}
			s.totalErrors.Add(1)
			break
		}
		if ev.Event != "message" {
			_, _ = fmt.Fprintf(w, "event: %s\n", ev.Event)
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", ev.Data)
		if flusher != nil {
			flusher.Flush()
		}
		if !firstSet {
			firstToken = time.Since(start)
			firstSet = true
		}
		if text, terr := sse.ExtractDeltaText(ev.Data); terr == nil && text != "" {
			outBuilder.WriteString(text)
		}
		if !previewDone && s.ring != nil {
			preview := outBuilder.String()
			if len(preview) > 256 {
				preview = preview[:256] + "…"
			}
			s.ring.Append("response:preview", rec.Model, s.red.Apply(preview))
			previewDone = true
		}
	}

	full := outBuilder.String()
	if s.ring != nil && full != "" {
		s.ring.Append("response", rec.Model, s.red.Apply(full))
	}
	rec.TTFTMillis = firstToken.Milliseconds()
	rec.TotalMillis = time.Since(start).Milliseconds()
	rec.EndedAt = time.Now().UTC()
	rec.OutputTokens = metrics.EstimateTokens(full)
	rec.ResponseBody = s.red.Apply(full)
	if cost, ok := s.prices.Cost(rec.Model, inTok, rec.OutputTokens); ok {
		rec.CostUSD = cost
	}
	s.recorder.Add(metrics.Record{
		Start:     start,
		End:       rec.EndedAt,
		TTFT:      firstToken,
		Total:     time.Since(start),
		PromptTok: inTok,
		OutputTok: rec.OutputTokens,
		Model:     rec.Model,
		Provider:  rec.Provider,
		Status:    resp.StatusCode,
		Path:      r.URL.Path,
		Stream:    true,
	})
	s.persist(*rec)
}

// handleJSON forwards a non-streaming JSON response and records metrics.
func (s *Server) handleJSON(w http.ResponseWriter, resp *http.Response, r *http.Request, rec *Request, start time.Time, inTok int) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		s.totalErrors.Add(1)
		rec.Error = err.Error()
		rec.EndedAt = time.Now().UTC()
		rec.TotalMillis = rec.EndedAt.Sub(rec.StartedAt).Milliseconds()
		s.persist(*rec)
		return
	}
	redacted := s.red.Apply(string(body))
	if s.ring != nil {
		s.ring.Append("response", rec.Model, redacted)
	}
	outTok := jsonOutputTokens(body)
	rec.OutputTokens = outTok
	rec.ResponseBody = redacted
	rec.EndedAt = time.Now().UTC()
	rec.TotalMillis = rec.EndedAt.Sub(rec.StartedAt).Milliseconds()
	if cost, ok := s.prices.Cost(rec.Model, inTok, outTok); ok {
		rec.CostUSD = cost
	}
	_, _ = w.Write(body)
	s.recorder.Add(metrics.Record{
		Start:     start,
		End:       rec.EndedAt,
		Total:     time.Since(start),
		PromptTok: inTok,
		OutputTok: outTok,
		Model:     rec.Model,
		Provider:  rec.Provider,
		Status:    resp.StatusCode,
		Path:      r.URL.Path,
		Stream:    false,
	})
	s.persist(*rec)
}

// recordError finalizes a Request for an error path and persists it.
func (s *Server) recordError(rec *Request, r *http.Request, status int, inTok int, msg string) {
	rec.StatusCode = status
	rec.Error = msg
	rec.EndedAt = time.Now().UTC()
	rec.TotalMillis = rec.EndedAt.Sub(rec.StartedAt).Milliseconds()
	rec.PromptTokens = inTok
	s.recorder.Add(metrics.Record{
		Start:     rec.StartedAt,
		End:       rec.EndedAt,
		Total:     rec.EndedAt.Sub(rec.StartedAt),
		PromptTok: inTok,
		Model:     rec.Model,
		Provider:  rec.Provider,
		Status:    status,
		Path:      r.URL.Path,
		Err:       msg,
	})
	s.persist(*rec)
}

// persist writes the Request to the configured store, if any. Failures are
// swallowed; persistence must never break the proxy. (Follow-up: a rate-
// limited stderr log would help diagnose disk-full / locked-db conditions.)
func (s *Server) persist(rec Request) {
	if s.store == nil {
		return
	}
	if err := s.store.Insert(rec); err != nil {
		return
	}
	if s.postPersist != nil {
		// Hook is fire-and-forget by contract; callers must not
		// panic or block. main.go wires a hook that runs the
		// anomaly detector, which is bounded and safe.
		defer func() { _ = recover() }()
		s.postPersist(rec)
	}
}

// isStreamingResponse returns true if the upstream response is an SSE stream.
func isStreamingResponse(resp *http.Response) bool {
	ct := resp.Header.Get("Content-Type")
	return strings.HasPrefix(ct, "text/event-stream")
}

// modelFromBody extracts the model name from a JSON request body if possible.
func modelFromBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var v struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &v); err == nil {
		return v.Model
	}
	return ""
}

func modelFromPath(p string) string {
	parts := strings.Split(p, "/")
	if len(parts) >= 3 {
		return strings.Join(parts[len(parts)-3:], "/")
	}
	return p
}

func jsonOutputTokens(body []byte) int {
	if len(body) == 0 {
		return 0
	}
	var v struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return metrics.EstimateTokens(string(body))
	}
	if v.Usage.CompletionTokens > 0 {
		return v.Usage.CompletionTokens
	}
	if len(v.Choices) > 0 {
		return metrics.EstimateTokens(v.Choices[0].Message.Content)
	}
	return 0
}

// ErrPortInUse indicates the configured port is already bound.
var ErrPortInUse = errors.New("listen address in use")

// CheckAvailable returns nil if addr can be bound.
func CheckAvailable(addr string) error {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPortInUse, err)
	}
	_ = l.Close()
	return nil
}
