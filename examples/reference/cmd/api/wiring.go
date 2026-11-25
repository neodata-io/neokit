package main

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v3/middleware/adaptor"

	neokit "github.com/neodata-io/neokit"
	"github.com/neodata-io/neokit/apitoken"
	"github.com/neodata-io/neokit/app"
	"github.com/neodata-io/neokit/health"
	"github.com/neodata-io/neokit/ids"
	"github.com/neodata-io/neokit/jobs"
	"github.com/neodata-io/neokit/lifecycle"
	"github.com/neodata-io/neokit/oidcauth"
	"github.com/neodata-io/neokit/oidcauth/fiberauth"
	"github.com/neodata-io/neokit/session"

	"github.com/neodata-io/neokit/examples/reference/internal/adapter/http/httpx"
	"github.com/neodata-io/neokit/examples/reference/internal/adapter/http/taskshttp"
	"github.com/neodata-io/neokit/examples/reference/internal/adapter/sqlite"
	"github.com/neodata-io/neokit/examples/reference/internal/app/tasks"
)

// ReadyPath is the readiness probe. Named once, because a compose file, an
// ingress and a test all have to agree on it.
const ReadyPath = "/readyz"

// keepDoneFor is how long a completed task is kept before the nightly purge.
const keepDoneFor = 30 * 24 * time.Hour

// service is what wire built, for run to start and for a test to drive.
type service struct {
	app   *neokit.App
	store *sqlite.Store
	gate  *fiberauth.Gate
}

// wire builds the whole service and starts nothing that listens. It is the one
// function that names every package — which is the point of a composition root:
// the order things are built in, and the order they are torn down in, can be
// read top to bottom in one place.
func wire(cfg Config) (*service, error) {
	a, err := neokit.New(app.Options{Name: "reference", Base: cfg.Base})
	if err != nil {
		return nil, err
	}
	// Both are things only this service can know: which of its sentinels is which
	// status, and which of its routes is noise in the request log.
	a.Errors.Mapper = httpx.MapError
	a.Errors.QuietPath = func(path string) bool { return path == ReadyPath }

	// From here on a failure has to undo what was built. Close runs the shutdown
	// stack as it stands, so each resource is pushed the moment it exists.
	fail := func(err error) (*service, error) {
		_ = a.Close()
		return nil, err
	}

	store, err := sqlite.Open(cfg.DatabasePath)
	if err != nil {
		return fail(err)
	}
	// Pushed before anything that uses it, so it closes after everything that
	// does: the stack unwinds in reverse, behind the HTTP drain and the jobs.
	a.Shutdown.Push("database", lifecycle.Closer(store))

	gate, err := login(a, cfg, store)
	if err != nil {
		return fail(err)
	}

	svc := tasks.New(tasks.Deps{Repo: store, NewID: ids.UUIDGenerator{}.New})

	// One Register per module. A second module is a second call here.
	taskshttp.Register(a.API, taskshttp.Deps{
		Tasks:  svc,
		Member: gate.RequireAuth(),
		Owner:  gate.RequireOwner(),
	})

	// Liveness is mounted by the app and consults nothing. Readiness is ours,
	// because only this service knows what it cannot work without.
	ready := &health.Registry{Log: a.Log}
	ready.Register("database", store.DB().PingContext)
	a.HTTP.Get(ReadyPath, adaptor.HTTPHandler(ready.ReadyHandler()))

	// Daily, not Every: 24 hours — a day is 23 or 25 hours twice a year. Started
	// on the app's context, so it is stopped and joined before the database
	// closes without this function owning a goroutine.
	jobs.Daily{
		Name: "purge done tasks", Hour: 3, Timeout: time.Minute, Log: a.Log,
		Do: func(ctx context.Context) error {
			_, err := svc.PurgeDone(ctx, keepDoneFor)
			return err
		},
	}.Start(a.Context())

	return &service{app: a, store: store, gate: gate}, nil
}

// login mounts the sign-in gate and personal API tokens, both kept in the
// service's own database.
//
// With no OIDC settings the provider is nil and the gate is off: its guards
// pass everything through and its routes answer 404. That is one state, not a
// second flag — the service ships open on a private network and closes the day
// the four variables are set.
func login(a *neokit.App, cfg Config, store *sqlite.Store) (*fiberauth.Gate, error) {
	sessions, err := session.NewSQLite(store.DB())
	if err != nil {
		return nil, err
	}
	tokens, err := apitoken.NewSQLite(store.DB(), apitoken.SQLiteOptions{})
	if err != nil {
		return nil, err
	}

	provider, configured := oidcauth.New(oidcauth.Config{
		Issuer: cfg.OIDCIssuer, ClientID: cfg.OIDCClientID, ClientSecret: cfg.OIDCClientSecret,
		BaseURL: cfg.OIDCBaseURL, OwnerGroup: cfg.OIDCOwnerGroup,
	})
	if !configured {
		a.Log.Warn("no login configured — the API is OPEN. Keep it on a private network, or set OIDC_ISSUER, OIDC_CLIENT_ID, OIDC_CLIENT_SECRET and OIDC_BASE_URL.")
	}

	return a.Login(fiberauth.Options{
		Provider: func() *oidcauth.Provider {
			if !configured {
				return nil
			}
			return provider
		},
		Sessions:     sessions,
		CookiePrefix: "reference",
		Tokens:       tokens,
		// A token for a script should not outlive the script's author's attention.
		TokenMaxAge: 365 * 24 * time.Hour,
	}), nil
}
