package capset

import (
	"sync"
	"testing"
)

// A component set in the shape this package is for: one required base interface,
// everything else optional and discovered by type assertion.
type component interface{ Name() string }

type Charger interface {
	component
	Charge() string
}

type Meter interface {
	component
	Read() int
}

type Player interface{ Play() string }

type base struct{ name string }

func (b base) Name() string { return b.name }

type chargerOnly struct{ base }

func (chargerOnly) Charge() string { return "charging" }

type meterOnly struct{ base }

func (meterOnly) Read() int { return 42 }

type chargerMeter struct{ base }

func (chargerMeter) Charge() string { return "charging" }
func (chargerMeter) Read() int      { return 7 }

type playerCharger struct{ base }

func (playerCharger) Charge() string { return "charging" }
func (playerCharger) Play() string   { return "playing" }

func fixture() *Set[component] {
	return New([]component{
		chargerOnly{base{"easee"}},
		meterOnly{base{"sma"}},
		chargerMeter{base{"equalizer"}},
		playerCharger{base{"tesla"}},
		base{"plain"},
	}, component.Name)
}

func TestAllFindsEveryImplementorInRegistrationOrder(t *testing.T) {
	s := fixture()

	chargers := All[Charger](s)
	if len(chargers) != 3 {
		t.Fatalf("got %d chargers, want 3", len(chargers))
	}
	want := []string{"easee", "equalizer", "tesla"}
	for i, c := range chargers {
		if c.Name() != want[i] {
			t.Errorf("chargers[%d] = %q, want %q — registration order is load-bearing for First", i, c.Name(), want[i])
		}
	}
	if got := len(All[Meter](s)); got != 2 {
		t.Errorf("got %d meters, want 2", got)
	}
}

// The zero value is the contract: "non-nil means available" is what every caller
// branches on, so a missing capability reads as absent rather than needing a
// second return value at each call site.
func TestOfReturnsNilForAMissingCapability(t *testing.T) {
	s := fixture()

	if c := Of[Charger](s, "easee"); c == nil || c.Charge() != "charging" {
		t.Errorf("Of[Charger](easee) = %v, want the implementor", c)
	}
	if m := Of[Meter](s, "easee"); m != nil {
		t.Error("Of must return a nil interface when the component lacks the capability")
	}
	if c := Of[Charger](s, "no-such-component"); c != nil {
		t.Error("Of must return nil for an unknown name")
	}
}

func TestFirstPicksTheFirstRegistered(t *testing.T) {
	s := fixture()
	if got := First[Charger](s); got == nil || got.Name() != "easee" {
		t.Errorf("First[Charger] = %v, want easee", got)
	}
	// A capability nobody implements yields the zero value, not a panic.
	type Absent interface{ Nope() }
	if got := First[Absent](s); got != nil {
		t.Errorf("First for an unimplemented capability = %v, want nil", got)
	}
}

// Some capabilities are only meaningful in combination — a search provider that
// is also a player, so a result can actually be played.
func TestBothIntersectsTwoCapabilities(t *testing.T) {
	s := fixture()
	got := Both[Charger, Player](s)
	if len(got) != 1 || got[0].Name() != "tesla" {
		t.Errorf("Both[Charger, Player] = %v, want just tesla", names(got))
	}
}

func TestAnyReportsPresence(t *testing.T) {
	s := fixture()
	if !Any[Charger](s) {
		t.Error("Any[Charger] must be true")
	}
	type Absent interface{ Nope() }
	if Any[Absent](s) {
		t.Error("Any for an unimplemented capability must be false")
	}
}

// The cache assumes membership is fixed, so a repeated lookup must be identical
// — and cheap.
func TestLookupsAreMemoised(t *testing.T) {
	s := fixture()
	first := All[Charger](s)
	second := All[Charger](s)
	if len(first) != len(second) {
		t.Fatal("a memoised lookup changed length")
	}
	if &first[0] != &second[0] {
		t.Error("All must return the cached slice rather than rebuilding it")
	}
}

// The set is read from every request handler while capabilities are resolved
// lazily; the first resolution must be race-free.
func TestConcurrentResolutionIsRaceFree(t *testing.T) {
	s := fixture()
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = All[Charger](s)
			_ = All[Meter](s)
			_ = Of[Charger](s, "tesla")
			_ = First[Meter](s)
			_ = Any[Player](s)
		}()
	}
	wg.Wait()
}

// ByName and Of need a name function; New(items, nil) is legitimate for a caller
// that only ever uses All/First, so the failure must be loud rather than a
// silently empty map.
func TestByNameWithoutANameFunctionPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("want a panic naming the missing name function")
		}
	}()
	s := New([]component{chargerOnly{base{"easee"}}}, nil)
	_ = Of[Charger](s, "easee")
}

func TestEmptySetResolvesToNothing(t *testing.T) {
	s := New([]component{}, component.Name)
	if len(All[Charger](s)) != 0 || First[Charger](s) != nil || Any[Charger](s) {
		t.Error("an empty set must resolve every capability to nothing")
	}
	if s.Len() != 0 {
		t.Errorf("Len = %d, want 0", s.Len())
	}
}

func names[T component](in []T) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = v.Name()
	}
	return out
}

// ── Benchmarks ──────────────────────────────────────────────────────────────

// Steady state is what matters: a cached lookup should cost about what the
// struct field read it replaces cost, on a path that then makes a network call.
func BenchmarkAllCached(b *testing.B) {
	s := fixture()
	_ = All[Charger](s) // warm
	b.ReportAllocs()
	for b.Loop() {
		_ = All[Charger](s)
	}
}

func BenchmarkOfCached(b *testing.B) {
	s := fixture()
	_ = Of[Charger](s, "easee") // warm
	b.ReportAllocs()
	for b.Loop() {
		_ = Of[Charger](s, "easee")
	}
}

// The cold path runs once per capability per generation, so its cost is paid on
// a config reload rather than per request.
func BenchmarkAllCold(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		s := fixture()
		_ = All[Charger](s)
	}
}
