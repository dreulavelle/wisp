// Command silo-plugin-wisp runs Wisp as a Silo plugin.
//
// Wisp writes .strm placeholders into a Silo library and resolves each one to a
// live stream when playback starts. This binary is the resolver half: Silo calls
// it over the plugin gRPC channel for every placeholder a user presses play on.
package main

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	publicmanifest "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtimedefault"

	"github.com/dreulavelle/wisp/internal/aiostreams"
	"github.com/dreulavelle/wisp/internal/plugin"
)

//go:embed manifest.json
var manifestJSON []byte

// version is injected at build time via -ldflags "-X main.version=<version>".
// Empty means "use whatever the embedded manifest declares".
var version string

// runtimeServer serves the manifest and applies admin configuration.
//
// Configure can arrive more than once — an operator editing settings triggers
// it again — so it rebuilds the handler each time rather than assuming a single
// startup call.
type runtimeServer struct {
	runtimedefault.Server

	manifest *pluginv1.PluginManifest
	routes   *plugin.HTTPRoutes
	log      *slog.Logger
	library  *plugin.Library
	recorder *plugin.Recorder

	mu  sync.Mutex
	cfg settings
}

type settings struct {
	aioURL      string
	aioPassword string
	libraryPath string
}

func (s *runtimeServer) GetManifest(context.Context, *pluginv1.GetManifestRequest) (*pluginv1.GetManifestResponse, error) {
	return &pluginv1.GetManifestResponse{Manifest: s.manifest}, nil
}

func (s *runtimeServer) Configure(_ context.Context, req *pluginv1.ConfigureRequest) (*pluginv1.ConfigureResponse, error) {
	next := settings{}
	for _, entry := range req.GetConfig() {
		fields := entry.GetValue().GetFields()
		if v, ok := fields["aiostreams_url"]; ok {
			next.aioURL = strings.TrimSpace(v.GetStringValue())
		}
		if v, ok := fields["aiostreams_password"]; ok {
			next.aioPassword = v.GetStringValue()
		}
		if v, ok := fields["library_path"]; ok {
			next.libraryPath = strings.TrimSpace(v.GetStringValue())
		}
	}

	s.mu.Lock()
	s.cfg = next
	s.mu.Unlock()

	if next.aioURL == "" {
		// Leave the previous handler in place rather than tearing a working
		// resolver down over an incomplete save.
		s.log.Warn("configure: aiostreams_url is empty; resolver left unconfigured")
		return &pluginv1.ConfigureResponse{}, nil
	}

	host := next.aioURL
	if u, err := url.Parse(next.aioURL); err == nil && u.Host != "" {
		host = u.Host
	}

	resolver := plugin.NewResolver(aiostreams.New(next.aioURL, next.aioPassword))
	s.routes.SetHandler(plugin.NewRouterWith(plugin.RouterOptions{
		Resolver: resolver,
		Log:      s.log,
		Version:  s.manifest.GetVersion(),
		Settings: plugin.Settings{AIOStreamsHost: host, LibraryPath: next.libraryPath},
		Library:  s.library,
		Recorder: s.recorder,
		// Derived from configuration so placeholder URLs stay valid across
		// restarts without persisting a secret anywhere.
		Signer: plugin.NewSigner(next.aioURL, next.aioPassword),
	}).Handler())
	s.log.Info("configure: resolver ready", "aiostreams_host", host, "library_path", next.libraryPath)

	return &pluginv1.ConfigureResponse{}, nil
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	manifest, err := publicmanifest.LoadWithChecksum(manifestJSON, version)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load manifest: %v\n", err)
		os.Exit(1)
	}

	routes := plugin.NewHTTPRoutes()

	sdkruntime.Serve(sdkruntime.ServeConfig{
		Servers: sdkruntime.CapabilityServers{
			Runtime: &runtimeServer{
				manifest: manifest,
				routes:   routes,
				log:      log,
				library:  plugin.NewLibrary(),
				recorder: plugin.NewRecorder(),
			},
			HttpRoutes: routes,
		},
	})
}
