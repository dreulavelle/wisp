// Command silo-plugin-wisp runs Wisp as a Silo plugin.
//
// Wisp writes .strm placeholders into a Silo library and resolves each one to a
// live stream when playback starts. This binary is the resolver half: Silo calls
// it over the plugin gRPC channel for every placeholder a user presses play on.
package main

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
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
	aioURL        string
	aioPassword   string
	libraryPath   string
	signingSecret string
}

// signingConfigKey is the plugin-owned config entry holding the resolver
// signing secret. It is written by Wisp, never by an operator, and is separate
// from the "global" entry so an admin editing settings cannot disturb it.
const signingConfigKey = "signing"

func (s *runtimeServer) GetManifest(context.Context, *pluginv1.GetManifestRequest) (*pluginv1.GetManifestResponse, error) {
	return &pluginv1.GetManifestResponse{Manifest: s.manifest}, nil
}

func (s *runtimeServer) Configure(ctx context.Context, req *pluginv1.ConfigureRequest) (*pluginv1.ConfigureResponse, error) {
	next := settings{}
	for _, entry := range req.GetConfig() {
		fields := entry.GetValue().GetFields()
		if entry.GetKey() == signingConfigKey {
			if v, ok := fields["secret"]; ok {
				next.signingSecret = strings.TrimSpace(v.GetStringValue())
			}
			continue
		}
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

	client := aiostreams.New(next.aioURL)

	// The Search API needs credentials even though AIOStreams' Stremio routes
	// do not, and a full manifest URL carries them. A URL that cannot supply
	// them would fail every lookup with a 401 at the worst possible moment —
	// when somebody presses play — so it is called out here instead, while
	// whoever pasted it is still looking at the settings page.
	if !client.HasCredentials() {
		s.log.Error("configure: this AIOStreams URL carries no credentials, so every lookup will fail. " +
			"Use the full manifest URL from the AIOStreams configure page, of the form " +
			"https://<host>/stremio/<id>/<config>/manifest.json")
	}

	signer := s.signerFor(ctx, next)

	resolver := plugin.NewResolverWith(client, s.log)
	s.routes.SetHandler(plugin.NewRouterWith(plugin.RouterOptions{
		Resolver: resolver,
		Log:      s.log,
		Version:  s.manifest.GetVersion(),
		Settings: plugin.Settings{AIOStreamsHost: host, LibraryPath: next.libraryPath},
		Library:  s.library,
		Recorder: s.recorder,
		Monitor:  s.monitor,
		Signer:   signer,
	}).Handler())
	// Requests can only produce placeholders once a library path is known, so
	// intake is wired here rather than at startup.
	if next.libraryPath != "" {
		base := s.resolverBase()
		// Logged because it is baked into every placeholder written from here
		// on. If it is wrong, the symptom appears much later and far away — a
		// 404 at playback — so the value belongs in the log at the moment it is
		// chosen.
		s.log.Info("configure: placeholders will address this plugin at", "resolver_base", base)
		writer := plugin.NewWriter(next.libraryPath, base, signer)

		// The index lives in memory; the placeholders on disk are the durable
		// record. Rebuilding here means a restarted plugin knows what it has
		// already written instead of treating the library as empty.
		if adopted, skipped, err := s.library.Rebuild(next.libraryPath); err != nil {
			s.log.Warn("configure: could not rebuild the library index", "error", err)
		} else {
			s.log.Info("configure: library index rebuilt", "adopted", adopted, "skipped", skipped)
		}

		// Placeholders written before this configure may address a resolver
		// base that no longer routes: Silo mints a new installation id on
		// every plugin upgrade, and the id is baked into every file. Bring
		// them current now, before anyone presses play against a 404.
		if rewritten, failed, err := plugin.RetargetPlaceholders(next.libraryPath, writer, s.log); err != nil {
			s.log.Warn("configure: could not retarget placeholders", "error", err)
		} else if rewritten > 0 || failed > 0 {
			s.log.Info("configure: placeholders retargeted",
				"rewritten", rewritten, "failed", failed, "resolver_base", base)
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

		// Report placeholders to Silo the moment they are written. Without
		// this they wait for autoscan's next poll — up to ten minutes on the
		// default interval, which for on-demand playback is the entire delay
		// between requesting something and being able to watch it.
		pusher := plugin.NewScanPusher(hostEvents(), s.log)

		s.router.SetIntake(plugin.NewIntake(writer, s.library, meta, s.log).
			WithIdentityResolver(meta).
			WithAnimeClassifier(meta).
			WithScanPusher(pusher))
		s.monitor.Set(plugin.NewMonitor(s.library, writer, meta, s.log).WithScanPusher(pusher))
	} else {
		s.log.Warn("configure: no library path set; requests cannot create placeholders")
	}

	s.log.Info("configure: resolver ready", "aiostreams_host", host, "library_path", next.libraryPath)

	return &pluginv1.ConfigureResponse{}, nil
}

// signerFor builds the signer that authenticates resolver URLs.
//
// The secret is generated once and persisted through the host, so it survives
// restarts and — critically — does not move when AIOStreams credentials do.
// Wisp used to derive this key from the AIOStreams URL and password, which
// meant editing either one silently invalidated every placeholder already
// written: the files stayed on disk, scanned fine, and 404'd the moment
// somebody pressed play. Recovering needed every .strm rewritten.
//
// Placeholders written under the old derived key keep working: that key is
// still accepted for verification, it is just no longer used for signing.
func (s *runtimeServer) signerFor(ctx context.Context, cfg settings) *plugin.Signer {
	legacy := plugin.NewSigner(cfg.aioURL, cfg.aioPassword)

	secret := cfg.signingSecret
	if secret == "" {
		generated, err := newSigningSecret()
		if err != nil {
			// Falling back to the derived key keeps playback working rather
			// than failing closed on a randomness error, at the cost of the
			// durability this function exists to provide.
			s.log.Error("configure: could not generate a signing secret; falling back to the credential-derived key", "error", err)
			return legacy
		}
		secret = generated
		if err := s.persistSigningSecret(ctx, secret); err != nil {
			// Not persisted means a different secret next restart, which would
			// invalidate everything written under this one. Use the derived key
			// instead: it is reproducible, which is the property that matters
			// more than durability here.
			s.log.Error("configure: could not persist the signing secret; falling back to the credential-derived key", "error", err)
			return legacy
		}
		s.log.Info("configure: generated a durable resolver signing secret")
	}

	return plugin.NewSignerFromSecret(secret).AcceptAlso(legacy)
}

// newSigningSecret returns a fresh random secret.
func newSigningSecret() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// persistSigningSecret stores the secret as a plugin-owned config entry, which
// the host hands back on the next Configure.
func (s *runtimeServer) persistSigningSecret(ctx context.Context, secret string) error {
	host := sdkruntime.Host()
	if host == nil {
		return fmt.Errorf("no host connection")
	}
	return host.SetGlobalConfigEntry(ctx, signingConfigKey, map[string]any{"secret": secret})
}

// hostEvents returns the host connection used to publish events, or nil when
// there is none. A nil publisher makes every push a no-op, so a plugin running
// without a host simply falls back to being polled.
func hostEvents() plugin.EventPublisher {
	host := sdkruntime.Host()
	if host == nil {
		return nil
	}
	return host
}

// resolverBase is the URL placeholders point at.
//
// Silo reaches its plugins over loopback, and resolves that hop itself before
// redirecting a client, so a host-local base is correct here — a client is
// never sent to this address.
//
// The base addresses this plugin by its STABLE manifest id
// (/api/v1/plugins/by-name/wisp/...) rather than its numeric installation id.
// The numeric id is a database key minted fresh on every plugin upgrade, and a
// placeholder is a durable file: baking the id in stranded the whole library on
// the first upgrade (4 -> 8 -> 9), which is exactly what RetargetPlaceholders
// existed to paper over. The manifest id is authored by Wisp, known locally
// without asking the host, and survives every reinstall. Silo's fork-owned strm
// layer translates by-name -> current numeric id at resolve time, so no client
// or token is affected: the HMAC signs the resolve tuple, not the URL.
//
// This deliberately does NOT prefer a host-provided proxy base (the old
// GetHostInfo path): such a base would carry the mutable numeric id and silently
// reintroduce the very churn this indirection removes.
func (s *runtimeServer) resolverBase() string {
	return fmt.Sprintf("%s/api/v1/plugins/by-name/%s", internalBaseURL, s.manifest.GetPluginId())
}

// internalBaseURL is where Silo listens inside its own container. Placeholders
// are resolved by Silo itself rather than fetched by a client, so a loopback
// address is correct and deliberate.
const internalBaseURL = "http://127.0.0.1:8080"

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
