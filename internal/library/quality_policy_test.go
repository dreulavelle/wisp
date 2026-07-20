package library

import (
	"errors"
	"testing"
)

// The policy raises sub-minimum tiers, drops 2160p unless it is opted into, and
// rejects only when the policy itself emptied a non-empty request.
func TestQualityPolicyApply(t *testing.T) {
	defaults := QualityPolicy{Min: "1080p"} // wisp's shipped defaults: 1080p floor, no 4K
	permissive := QualityPolicy{Min: "1080p", Allow2160p: true}

	cases := []struct {
		name      string
		policy    QualityPolicy
		requested []string
		want      []string
		wantErr   error
	}{
		{
			name:   "below the minimum is raised, not rejected",
			policy: defaults, requested: []string{"720p"}, want: []string{"1080p"},
		},
		{
			name:   "480p is raised too",
			policy: defaults, requested: []string{"480p"}, want: []string{"1080p"},
		},
		{
			name:   "raising collapses into an existing 1080p rather than duplicating",
			policy: defaults, requested: []string{"720p", "1080p"}, want: []string{"1080p"},
		},
		{
			name:   "2160p is dropped when 4K is disabled",
			policy: defaults, requested: []string{"1080p", "2160p"}, want: []string{"1080p"},
		},
		{
			name:   "a 4K-only request is rejected outright",
			policy: defaults, requested: []string{"2160p"}, wantErr: ErrNoAllowedQuality,
		},
		{
			name:   "a 4K-only request in any spelling is rejected",
			policy: defaults, requested: []string{"4k", "UHD"}, wantErr: ErrNoAllowedQuality,
		},
		{
			name:   "sub-minimum plus 4K keeps the raised tier and drops 4K",
			policy: defaults, requested: []string{"720p", "2160p"}, want: []string{"1080p"},
		},
		{
			name:   "2160p survives when 4K is enabled",
			policy: permissive, requested: []string{"1080p", "2160p"}, want: []string{"1080p", "2160p"},
		},
		{
			name:   "4K-only is fine when 4K is enabled",
			policy: permissive, requested: []string{"4k"}, want: []string{"2160p"},
		},
		{
			name:   "order is preserved and labels canonicalized",
			policy: permissive, requested: []string{"4K", "1080P"}, want: []string{"2160p", "1080p"},
		},
		{
			name:   "an empty request stays unconstrained (best available)",
			policy: defaults, requested: nil, want: nil,
		},
		{
			name:   "unrecognized labels are dropped, not rejected",
			policy: defaults, requested: []string{"potato"}, want: nil,
		},
		{
			name:   "no minimum leaves low tiers alone",
			policy: QualityPolicy{Allow2160p: true}, requested: []string{"720p"}, want: []string{"720p"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.policy.Apply(tc.requested)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil {
				if got != nil {
					t.Fatalf("rejected request returned %v, want nil", got)
				}
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("Apply(%v) = %v, want %v", tc.requested, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("Apply(%v) = %v, want %v", tc.requested, got, tc.want)
				}
			}
		})
	}
}

// Apply is idempotent: feeding its own output back changes nothing, which is
// what makes the startup migration safe to run on every boot.
func TestQualityPolicyApplyIsIdempotent(t *testing.T) {
	p := QualityPolicy{Min: "1080p"}
	once, err := p.Apply([]string{"720p", "1080p", "2160p"})
	if err != nil {
		t.Fatal(err)
	}
	twice, err := p.Apply(once)
	if err != nil {
		t.Fatal(err)
	}
	if len(once) != len(twice) || once[0] != twice[0] {
		t.Fatalf("not idempotent: %v then %v", once, twice)
	}
}

func TestQualityPolicyApplyOne(t *testing.T) {
	p := QualityPolicy{Min: "1080p"}

	if got, err := p.ApplyOne("720p"); err != nil || got != "1080p" {
		t.Fatalf("ApplyOne(720p) = %q, %v; want 1080p, nil", got, err)
	}
	if got, err := p.ApplyOne("1080p"); err != nil || got != "1080p" {
		t.Fatalf("ApplyOne(1080p) = %q, %v; want 1080p, nil", got, err)
	}
	if _, err := p.ApplyOne("2160p"); !errors.Is(err, ErrNoAllowedQuality) {
		t.Fatalf("ApplyOne(2160p) err = %v, want ErrNoAllowedQuality", err)
	}
	// "best available" and unrecognized labels pass through untouched — the
	// resolver, not the policy, decides what they mean.
	if got, err := p.ApplyOne(""); err != nil || got != "" {
		t.Fatalf(`ApplyOne("") = %q, %v; want "", nil`, got, err)
	}
	permissive := QualityPolicy{Min: "1080p", Allow2160p: true}
	if got, err := permissive.ApplyOne("4k"); err != nil || got != "2160p" {
		t.Fatalf("ApplyOne(4k) with 4K enabled = %q, %v; want 2160p, nil", got, err)
	}
}
