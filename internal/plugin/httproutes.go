package plugin

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"

	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// HTTPRoutes adapts an ordinary http.Handler to Silo's http_routes.v1 service.
//
// The host does not proxy a socket: it calls this gRPC method once per inbound
// HTTP request and expects a complete response back. That makes the transport
// unary and fully buffered, which rules out streaming media through it — but a
// redirect is a few hundred bytes, so answering playback with a 302 costs
// nothing and keeps media bytes off this path entirely.
type HTTPRoutes struct {
	pluginv1.UnimplementedHttpRoutesServer
	handler atomic.Pointer[http.Handler]
}

// NewHTTPRoutes returns a server that answers 503 until SetHandler is called.
func NewHTTPRoutes() *HTTPRoutes { return &HTTPRoutes{} }

// SetHandler atomically swaps the active handler. Passing nil clears it, which
// makes the plugin report "not configured" rather than panicking — the host may
// route requests before Configure has arrived.
func (s *HTTPRoutes) SetHandler(h http.Handler) {
	if h == nil {
		s.handler.Store(nil)
		return
	}
	s.handler.Store(&h)
}

// Handle replays a host request against the wrapped handler.
func (s *HTTPRoutes) Handle(ctx context.Context, req *pluginv1.HandleHTTPRequest) (*pluginv1.HandleHTTPResponse, error) {
	h := s.handler.Load()
	if h == nil {
		return &pluginv1.HandleHTTPResponse{
			StatusCode: http.StatusServiceUnavailable,
			Headers:    map[string]string{"Content-Type": "application/json"},
			Body:       []byte(`{"error":"wisp is not configured yet"}`),
		}, nil
	}

	httpReq := buildRequest(ctx, req)
	rec := httptest.NewRecorder()
	(*h).ServeHTTP(rec, httpReq)

	result := rec.Result()
	defer result.Body.Close()
	body, _ := io.ReadAll(result.Body)

	headers := make(map[string]string, len(result.Header))
	for k := range result.Header {
		// A redirect's Location is the entire payload of a playback response,
		// so header fidelity matters as much as the body here.
		headers[k] = result.Header.Get(k)
	}

	return &pluginv1.HandleHTTPResponse{
		StatusCode: int32(result.StatusCode),
		Headers:    headers,
		Body:       body,
	}, nil
}

// buildRequest reconstructs an http.Request from the host's representation.
func buildRequest(ctx context.Context, req *pluginv1.HandleHTTPRequest) *http.Request {
	method := req.GetMethod()
	if method == "" {
		method = http.MethodGet
	}

	target := &url.URL{Path: req.GetPath(), RawQuery: encodeQuery(req.GetQuery())}
	httpReq := httptest.NewRequest(method, target.String(), bytes.NewReader(req.GetBody()))
	for k, v := range req.GetHeaders() {
		httpReq.Header.Set(k, v)
	}
	return httpReq.WithContext(ctx)
}

// encodeQuery flattens the host's Struct-encoded query back into a raw string.
//
// The host models query parameters as JSON values, so numbers arrive as float64
// and booleans as bool. Rendering them back to their literal text keeps
// handlers written against net/http working unchanged.
func encodeQuery(q *structpb.Struct) string {
	if q == nil {
		return ""
	}
	values := url.Values{}
	for k, v := range q.GetFields() {
		switch val := v.AsInterface().(type) {
		case string:
			values.Set(k, val)
		case bool:
			values.Set(k, strconv.FormatBool(val))
		case float64:
			// Avoid rendering whole numbers as "1e+06".
			values.Set(k, strconv.FormatFloat(val, 'f', -1, 64))
		case nil:
			values.Set(k, "")
		}
	}
	return values.Encode()
}
