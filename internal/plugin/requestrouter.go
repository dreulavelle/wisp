package plugin

import (
	"context"
	"net/url"
	"path"
	"strings"
	"sync"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// RequestRouter adapts intake to Silo's request_router.v1 service, so a request
// made in Silo's own UI produces a placeholder without users learning a second
// interface.
type RequestRouter struct {
	pluginv1.UnimplementedRequestRouterServer
	mu     sync.RWMutex
	intake *Intake
}

// NewRequestRouter returns a router over an intake.
func NewRequestRouter(intake *Intake) *RequestRouter {
	return &RequestRouter{intake: intake}
}

// SetIntake swaps the active intake. Configuration can arrive after the host
// has already started routing, and can arrive again when an operator edits
// settings, so this is safe to call at any time.
func (r *RequestRouter) SetIntake(in *Intake) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.intake = in
}

func (r *RequestRouter) current() *Intake {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.intake
}

func (r *RequestRouter) Fulfill(ctx context.Context, req *pluginv1.FulfillRequest) (*pluginv1.FulfillResponse, error) {
	in := r.current()
	if in == nil {
		return &pluginv1.FulfillResponse{Message: "wisp is not configured yet"}, nil
	}
	return in.Fulfill(ctx, req)
}

func (r *RequestRouter) CheckStatus(ctx context.Context, req *pluginv1.CheckStatusRequest) (*pluginv1.CheckStatusResponse, error) {
	in := r.current()
	if in == nil {
		return &pluginv1.CheckStatusResponse{}, nil
	}
	return in.CheckStatus(ctx, req)
}

// TestConnection reports whether Wisp can accept requests.
//
// Deliberately local: it checks that a library path and resolver are configured
// rather than reaching out to AIOStreams. An operator pressing Test Connection
// wants to know their Wisp settings are right, and a provider outage should not
// make correct configuration look broken.
func (r *RequestRouter) TestConnection(_ context.Context, _ *pluginv1.TestConnectionRequest) (*pluginv1.TestConnectionResponse, error) {
	in := r.current()
	if in == nil || in.writer == nil {
		return &pluginv1.TestConnectionResponse{
			Ok: false, Message: "Wisp has no library path configured yet",
		}, nil
	}
	if in.writer.root == "" {
		return &pluginv1.TestConnectionResponse{
			Ok: false, Message: "Set a library path in Wisp's settings",
		}, nil
	}
	return &pluginv1.TestConnectionResponse{
		Ok: true, Message: "Wisp is ready; requests become playable placeholders",
	}, nil
}

// Validate checks configuration without touching the network.
//
// Errors are returned per field so the admin form can mark the offending input
// rather than showing a form-level message the operator has to map back to a
// box themselves.
func (r *RequestRouter) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateResponse, error) {
	fieldErrors := map[string]string{}
	fields := req.GetConnection().GetConfig().GetFields()

	if raw := strings.TrimSpace(fields["aiostreams_url"].GetStringValue()); raw != "" {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			fieldErrors["aiostreams_url"] =
				"Must be a full http(s) URL, e.g. https://host/stremio/<uuid>/manifest.json"
		}
	}

	if raw := strings.TrimSpace(fields["library_path"].GetStringValue()); raw != "" && !path.IsAbs(raw) {
		// A relative path would resolve against the plugin's working directory,
		// which is not somewhere a media server is looking.
		fieldErrors["library_path"] = "Must be an absolute path, e.g. /library"
	}

	if len(fieldErrors) == 0 {
		return &pluginv1.ValidateResponse{}, nil
	}
	return &pluginv1.ValidateResponse{FieldErrors: fieldErrors}, nil
}

// ListConfigOptions returns nothing: Wisp's admin form has no dynamic dropdowns.
func (r *RequestRouter) ListConfigOptions(context.Context, *pluginv1.ListConfigOptionsRequest) (*pluginv1.ListConfigOptionsResponse, error) {
	return &pluginv1.ListConfigOptionsResponse{}, nil
}
