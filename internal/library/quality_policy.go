package library

import "errors"

// qualityRank orders wisp's tier vocabulary (the labels DetectQuality emits) so a
// minimum can be expressed as a floor rather than an explicit allow-list. An
// unrecognized label ranks 0 and is never comparable — callers drop it first.
var qualityRank = map[string]int{
	"480p":  1,
	"720p":  2,
	"1080p": 3,
	"2160p": 4,
}

// ErrNoAllowedQuality means every tier a caller asked for is disallowed by
// policy, so honoring the request would create work that can never be satisfied.
// The only way to reach it is a 2160p-only request while 4K is disabled — a tier
// below the minimum is raised, not dropped.
var ErrNoAllowedQuality = errors.New("no requested quality tier is allowed by the configured quality policy")

// QualityPolicy constrains which resolution tiers wisp is willing to request. It
// is applied at intake, so a disallowed tier is never stored on a monitor and
// therefore never scraped, never scheduled, and never surfaced — rather than
// being filtered out of results after the fact.
type QualityPolicy struct {
	// Min is the canonical floor tier ("" = no floor). A request below it is
	// raised to Min rather than rejected: the caller wants the content, and a
	// better file still satisfies them, whereas rejecting would turn a perfectly
	// servable request into a permanent failure over a policy they cannot see.
	Min string
	// Allow2160p opts into 4K. When false, 2160p is dropped from every request.
	Allow2160p bool
}

// Apply returns the tiers wisp will actually pursue for a request: canonicalized,
// deduped, order-preserving, with sub-minimum tiers raised to Min and 2160p
// dropped unless it is allowed.
//
// An empty result is only an error when the policy is what emptied it
// (ErrNoAllowedQuality). Two cases deliberately return (nil, nil) instead:
//
//   - An empty or absent request. That means "best available", which imposes no
//     resolution constraint at all — the resolver takes the top-ranked stream. It
//     is deliberately NOT pinned to Min: doing so would convert a graceful
//     best-effort into a hard constraint, and a title whose only release is below
//     the floor would go from pinned to permanently unsatisfiable. Min is a floor
//     on what is *requested*, and "the best available" is by construction not
//     below anything else on offer.
//   - A request of nothing but unrecognized labels, which has always been
//     treated as "best available" rather than an error.
func (p QualityPolicy) Apply(requested []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	blocked := false
	for _, q := range requested {
		n := NormalizeQuality(q)
		if n == "" {
			continue // unrecognized — historically dropped, not an error
		}
		if n == "2160p" && !p.Allow2160p {
			blocked = true
			continue
		}
		if p.Min != "" && qualityRank[n] < qualityRank[p.Min] {
			n = p.Min
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	if len(out) == 0 && blocked {
		return nil, ErrNoAllowedQuality
	}
	return out, nil
}

// ApplyOne is Apply for a single tier, for the direct-pin path that names one
// quality. An empty quality ("best available") passes through untouched.
func (p QualityPolicy) ApplyOne(quality string) (string, error) {
	if NormalizeQuality(quality) == "" {
		return quality, nil // best-available, or an unrecognized label the resolver ignores
	}
	out, err := p.Apply([]string{quality})
	if err != nil {
		return "", err
	}
	return out[0], nil
}
