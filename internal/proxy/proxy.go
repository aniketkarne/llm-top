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
	"github.com/aniketkarne-com/llm-top/internal/redactor"
	"github.com/aniketkarne-com/llm-top/internal/sse"
)

// Server is the proxy HTTP server with attached state.
type Server struct {
	cfg      Config
	recorder *metrics.Recorder
	ring     *ring.Buffer
	red      *redactor.Redactor
	client   *http.Client

	totalRequests atomic.Uint64
	totalErrors   atomic.Uint64
}

// Config is the proxy's view of runtime configuration.
type Config struct {
	ListenAddr      string
	UpstreamBaseURL string
	UpstreamAPIKey  string
	BufferSize      int
}

// New constructs a Server. The Recorder, Buffer, and Redactor are shared with
// the TUI so both views observe the same state.
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
	return &Server{
		cfg:      cfg,
		recorder: rec,
		ring:     ring,
		red:      red,
		client:   &http.Client{Transport: tr},
	}
}

// Handler returns the http.Handler that serves proxy traffic.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/", s.handleProxy)
	mux.HandleFunc("/", s.handleRoot)
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
			"version":     "0.1.0",
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

	upstream, err := url.Parse(s.cfg.UpstreamBaseURL)
	if err != nil {
		http.Error(w, "bad upstream config", http.StatusInternalServerError)
		s.totalErrors.Add(1)
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
	if s.ring != nil {
		s.ring.Append("request", modelFromBody(reqBody), s.red.Apply(string(reqBody)))
	}
	inTok := metrics.EstimateTokens(string(reqBody))

	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bytes.NewReader(reqBody))
	if err != nil {
		http.Error(w, "build upstream request: "+err.Error(), http.StatusInternalServerError)
		s.recordError(r, start, 0, inTok, "build upstream: "+err.Error())
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
		s.recordError(r, start, 0, inTok, err.Error())
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

	isStream := isStreamingResponse(resp)
	w.WriteHeader(resp.StatusCode)

	if isStream {
		s.handleStream(w, resp, r, start, inTok)
	} else {
		s.handleJSON(w, resp, r, start, inTok)
	}
}

// handleStream copies the upstream SSE bytes to the client while measuring
// time-to-first-token, accumulating the textual response, and recording the
// total response time.
func (s *Server) handleStream(w http.ResponseWriter, resp *http.Response, r *http.Request, start time.Time, inTok int) {
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
			s.ring.Append("response:preview", modelFromPath(r.URL.Path), s.red.Apply(preview))
			previewDone = true
		}
	}

	full := outBuilder.String()
	if s.ring != nil && full != "" {
		s.ring.Append("response", modelFromPath(r.URL.Path), s.red.Apply(full))
	}
	s.recorder.Add(metrics.Record{
		Start:     start,
		End:       time.Now(),
		TTFT:      firstToken,
		Total:     time.Since(start),
		PromptTok: inTok,
		OutputTok: metrics.EstimateTokens(full),
		Model:     modelFromPath(r.URL.Path),
		Status:    resp.StatusCode,
		Path:      r.URL.Path,
		Stream:    true,
	})
}

// handleJSON forwards a non-streaming JSON response and records metrics.
func (s *Server) handleJSON(w http.ResponseWriter, resp *http.Response, r *http.Request, start time.Time, inTok int) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		s.totalErrors.Add(1)
		return
	}
	if s.ring != nil {
		s.ring.Append("response", modelFromPath(r.URL.Path), s.red.Apply(string(body)))
	}
	outTok := jsonOutputTokens(body)
	_, _ = w.Write(body)
	s.recorder.Add(metrics.Record{
		Start:     start,
		End:       time.Now(),
		Total:     time.Since(start),
		PromptTok: inTok,
		OutputTok: outTok,
		Model:     modelFromPath(r.URL.Path),
		Status:    resp.StatusCode,
		Path:      r.URL.Path,
		Stream:    false,
	})
}

func (s *Server) recordError(r *http.Request, start time.Time, status int, inTok int, msg string) {
	s.totalErrors.Add(1)
	s.recorder.Add(metrics.Record{
		Start:     start,
		End:       time.Now(),
		Total:     time.Since(start),
		PromptTok: inTok,
		Model:     modelFromPath(r.URL.Path),
		Status:    status,
		Path:      r.URL.Path,
		Err:       msg,
	})
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
