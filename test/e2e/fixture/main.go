// Command fixture writes end-to-end test placeholders using Wisp's real
// production writer and signer.
//
// Using the shipping code rather than hand-rolled fixture text is the point: a
// placeholder that the real writer cannot produce, or that the real signer will
// not verify, is a bug the test must catch rather than paper over.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/dreulavelle/wisp/internal/plugin"
)

func main() {
	root := flag.String("root", "", "library root")
	base := flag.String("resolver-base", "", "resolver base URL, e.g. http://127.0.0.1:8080/api/v1/plugins/1")
	aioURL := flag.String("aiostreams-url", "", "AIOStreams URL, seeds the signing key")
	quality := flag.String("quality", "1080p", "requested quality tier")
	flag.Parse()

	if *root == "" || *base == "" {
		fmt.Fprintln(os.Stderr, "fixture: -root and -resolver-base are required")
		os.Exit(1)
	}

	// The plugin signs new placeholders with a persisted random secret, but it
	// still ACCEPTS the legacy credential-derived key so placeholders written
	// before that change keep playing. Signing with the legacy derivation here
	// exercises exactly that compatibility path — a fixture the running plugin
	// refuses to verify is a regression this test must catch.
	signer := plugin.NewSigner(*aioURL, "")
	writer := plugin.NewWriter(*root, *base, signer)

	items := []plugin.Item{
		{
			MediaType: "movie",
			Title:     "E2E Movie",
			Year:      2024,
			ID:        plugin.MediaID{Source: plugin.SourceTMDB, Value: "603"},
			IMDbID:    "tt0133093",
			Quality:   *quality,
		},
		{
			MediaType: "movie",
			Title:     "E2E Anime Movie",
			Year:      2001,
			ID:        plugin.MediaID{Source: plugin.SourceTMDB, Value: "129"},
			IMDbID:    "tt0245429",
			Quality:   *quality,
			Anime:     true,
		},
		{
			MediaType: "series",
			Title:     "E2E Show",
			Year:      2024,
			ID:        plugin.MediaID{Source: plugin.SourceTVDB, Value: "121361"},
			IMDbID:    "tt0944947",
			Season:    1,
			Episode:   9,
			Quality:   *quality,
		},
	}

	for _, item := range items {
		path, err := writer.Write(item)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fixture: write %s: %v\n", item.Title, err)
			os.Exit(1)
		}
		fmt.Println(path)
	}
}
