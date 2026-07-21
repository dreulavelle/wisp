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
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtimehost"

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

func (s *runtimeServer) Configure(ctx context.Context, req *pluginv1.ConfigureRequest) (*pluginv1.ConfigureResponse, error) {
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

	client := aiostreams.New(next.aioURL, next.aioPassword)

	// The resolver route is public, so its tokens are the only thing standing
	// between the internet and stream links minted against the operator's
	// debrid quota. Those tokens are signed with a key derived from the
	// AIOStreams URL and password — which means with no password, the key
	// derives from the URL alone. For the alias form that URL carries no
	// encrypted config and is not secret, so anyone who knows it can recompute
	// the key. Say so plainly; silently running with a guessable key is worse
	// than refusing to start.
	if !client.HasCredentials() {
		s.log.Warn("configure: no AIOStreams password set — resolver tokens are signed with a key derived from the URL alone, which is guessable for alias-form URLs; set a password to make placeholder links unforgeable")
	}

	resolver := plugin.NewResolver(client)
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
		base := s.resolverBase(ctx)
		// Logged because it is baked into every placeholder written from here
		// on. If it is wrong, the symptom appears much later and far away — a
		// 404 at playback — so the value belongs in the log at the moment it is
		// chosen.
		s.log.Info("configure: placeholders will address this plugin at", "resolver_base", base)
		writer := plugin.NewWriter(next.libraryPath, base, plugin.NewSigner(next.aioURL, next.aioPassword))

		// The index lives in memory; the placeholders on disk are the durable
		// record. Rebuilding here means a restarted plugin knows what it has
		// already written instead of treating the library as empty.
		if adopted, skipped, err := s.library.Rebuild(next.libraryPath); err != nil {
			s.log.Warn("configure: could not rebuild the library index", "error", err)
		} else {
			s.log.Info("configure: library index rebuilt", "adopted", adopted, "skipped", skipped)
		}

		// Create the library roots up front. An operator has to point a Silo
		// library at each of these before requesting anything, and Silo cannot
		// be pointed at a directory that does not exist yet.
		if created, err := writer.EnsureRoots(); err != nil {
			s.log.Error("configure: could not create the library roots", "error", err)
		} else if len(created) > 0 {
			s.log.Info("configure: created library roots", "roots", created, "under", next.libraryPath)
		}

		// Episode numbering comes from Cinemeta, whose series data is
		// TVDB-derived, so seasons and episodes line up with what media servers
		// expect without needing a TVDB key of our own. The same service backs
		// the anime classifier.
		meta := plugin.NewMetadataAdapter(metadata.New())
		s.router.SetIntake(plugin.NewIntake(writer, s.library, meta, s.log).
			WithIdentityResolver(meta).
			WithAnimeClassifier(meta))
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
//
// Getting the installation id right is not cosmetic. Silo mounts plugin routes
// at /api/v1/plugins/<installation id>/, that id is a database key, and a
// placeholder is a durable file: write the wrong one and every placeholder
// produced from then on 404s the moment somebody presses play, long after the
// mistake was made.
func (s *runtimeServer) resolverBase(ctx context.Context) string {
	host := sdkruntime.Host()
	if host == nil {
		s.log.Warn("resolver base: no host connection; placeholder URLs may be wrong",
			"fallback", fallbackResolverBase)
		return fallbackResolverBase
	}

	// The intended mechanism. Silo does not implement it as of v0.10.0 of the
	// SDK, so this is expected to fail — but preferring it means Wisp picks up
	// the correct answer for free once Silo does.
	if info, err := host.GetHostInfo(ctx); err == nil {
		if base := strings.TrimSpace(info.PluginProxyBaseURL); base != "" {
			return base
		}
	}

	// Fall back to asking which installations exist and finding ourselves in
	// the list. Wisp authored its own manifest, so it knows its plugin id.
	if id, err := s.installationID(ctx, host); err == nil {
		return fmt.Sprintf("%s/api/v1/plugins/%d", internalBaseURL, id)
	} else {
		s.log.Error("resolver base: could not determine this plugin's installation id; "+
			"placeholders will point at the wrong route and fail at playback",
			"error", err, "fallback", fallbackResolverBase)
	}

	return fallbackResolverBase
}

// installationID finds this plugin's own installation id.
func (s *runtimeServer) installationID(ctx context.Context, host *runtimehost.Client) (int64, error) {
	installed, err := host.ListInstalledPlugins(ctx)
	if err != nil {
		return 0, fmt.Errorf("list installed plugins: %w", err)
	}

	pluginID := s.manifest.GetPluginId()
	var found []int64
	for _, p := range installed {
		if p.GetPluginId() == pluginID {
			found = append(found, p.GetInstallationId())
		}
	}

	switch len(found) {
	case 0:
		return 0, fmt.Errorf("no installation reports plugin id %q", pluginID)
	case 1:
		return found[0], nil
	default:
		// Two installations of the same plugin are indistinguishable from in
		// here — the host offers no way to ask "which one am I". Guessing would
		// silently hand half the placeholders to the wrong instance, so say so
		// and take the lowest id deterministically rather than at map order.
		s.log.Warn("resolver base: this plugin is installed more than once; "+
			"cannot tell which installation this process is",
			"plugin_id", pluginID, "installation_ids", found, "using", found[0])
		lowest := found[0]
		for _, id := range found[1:] {
			if id < lowest {
				lowest = id
			}
		}
		return lowest, nil
	}
}

// internalBaseURL is where Silo listens inside its own container. Placeholders
// are resolved by Silo itself rather than fetched by a client, so a loopback
// address is correct and deliberate.
const internalBaseURL = "http://127.0.0.1:8080"

// fallbackResolverBase is a last resort when the installation id cannot be
// determined at all. It is a guess — correct only for the first installation —
// and every path reaching it logs loudly first.
const fallbackResolverBase = internalBaseURL + "/api/v1/plugins/1"

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
