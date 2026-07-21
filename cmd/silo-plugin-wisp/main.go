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
	"github.com/dreulavelle/wisp/internal/metadata"
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
	router   *plugin.RequestRouter
	monitor  *plugin.MonitorHolder
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
	// Requests can only produce placeholders once a library path is known, so
	// intake is wired here rather than at startup.
	if next.libraryPath != "" {
		writer := plugin.NewWriter(next.libraryPath, s.resolverBase(), plugin.NewSigner(next.aioURL, next.aioPassword))

		// The index lives in memory; the placeholders on disk are the durable
		// record. Rebuilding here means a restarted plugin knows what it has
		// already written instead of treating the library as empty.
		if adopted, skipped, err := s.library.Rebuild(next.libraryPath); err != nil {
			s.log.Warn("configure: could not rebuild the library index", "error", err)
		} else {
			s.log.Info("configure: library index rebuilt", "adopted", adopted, "skipped", skipped)
		}

		// Episode numbering comes from Cinemeta, whose series data is
		// TVDB-derived, so seasons and episodes line up with what media servers
		// expect without needing a TVDB key of our own.
		meta := plugin.NewMetadataAdapter(metadata.New("", nil))
		s.router.SetIntake(plugin.NewIntake(writer, s.library, meta, s.log).WithIdentityResolver(meta))
		s.monitor.Set(plugin.NewMonitor(s.library, writer, meta, s.log))
	} else {
		s.log.Warn("configure: no library path set; requests cannot create placeholders")
	}

	s.log.Info("configure: resolver ready", "aiostreams_host", host, "library_path", next.libraryPath)

	return &pluginv1.ConfigureResponse{}, nil
}

// resolverBase is the URL placeholders point at.
//
// Silo reaches its plugins over loopback, and resolves that hop itself before
// redirecting a client, so a host-local base is correct here — a client is
// never sent to this address.
func (s *runtimeServer) resolverBase() string {
	if host := sdkruntime.Host(); host != nil {
		if info, err := host.GetHostInfo(context.Background()); err == nil {
			if base := strings.TrimSpace(info.PluginProxyBaseURL); base != "" {
				return base
			}
		}
	}
	return defaultResolverBase
}

// defaultResolverBase is used when the host cannot supply its plugin proxy URL.
const defaultResolverBase = "http://127.0.0.1:8080/api/v1/plugins/1"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	manifest, err := publicmanifest.LoadWithChecksum(manifestJSON, version)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load manifest: %v\n", err)
		os.Exit(1)
	}

	routes := plugin.NewHTTPRoutes()
	library := plugin.NewLibrary()
	recorder := plugin.NewRecorder()
	router := plugin.NewRequestRouter(nil)
	monitor := plugin.NewMonitorHolder()

	// Serve the dashboard immediately, before any configuration arrives. It
	// reports an unconfigured resolver rather than refusing to load: the page
	// that explains what to set up must not itself require setup.
	routes.SetHandler(plugin.NewRouterWith(plugin.RouterOptions{
		Log:      log,
		Version:  manifest.GetVersion(),
		Library:  library,
		Recorder: recorder,
	}).Handler())

	sdkruntime.Serve(sdkruntime.ServeConfig{
		Servers: sdkruntime.CapabilityServers{
			Runtime: &runtimeServer{
				manifest: manifest,
				routes:   routes,
				log:      log,
				router:   router,
				monitor:  monitor,
				library:  library,
				recorder: recorder,
			},
			HttpRoutes: routes,
			// Autoscan pulls new placeholders from us rather than us pushing
			// webhooks at a media server: the host then owns the poll timer,
			// marker persistence, and path rewriting.
			ScanSource: plugin.NewScanSource(library, log),
			// Requests made in Silo's own UI route here, so users never have to
			// learn a second interface.
			RequestRouter: router,
			// Fills in episodes that aired since the last pass. Pure bookkeeping:
			// a new episode only needs a placeholder, and resolution happens later
			// if anyone plays it.
			ScheduledTask: monitor,
		},
	})
}
