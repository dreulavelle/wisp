package main

import (
	"testing"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// resolverBase must address this plugin by its STABLE manifest id, not a numeric
// installation id: the numeric id is minted fresh on every upgrade and would
// strand a durable .strm the moment the plugin re-installs. Silo's fork-owned
// strm layer translates the by-name base to the current numeric route at
// resolve time.
func TestResolverBaseIsByNameForm(t *testing.T) {
	s := &runtimeServer{manifest: &pluginv1.PluginManifest{PluginId: "wisp"}}

	got := s.resolverBase()
	const want = "http://127.0.0.1:8080/api/v1/plugins/by-name/wisp"
	if got != want {
		t.Errorf("resolverBase() = %q, want %q", got, want)
	}
}
