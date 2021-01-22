package shared

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/golang/gddo/httputil"
	"github.com/graph-gophers/graphql-go/relay"
	"github.com/inconshreveable/log15"
	"github.com/opentracing/opentracing-go"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/sourcegraph/sourcegraph/cmd/frontend/envvar"
	"github.com/sourcegraph/sourcegraph/cmd/frontend/globals"
	"github.com/sourcegraph/sourcegraph/cmd/repo-updater/repoupdater"
	"github.com/sourcegraph/sourcegraph/cmd/repo-updater/shared/assets"
	"github.com/sourcegraph/sourcegraph/internal/actor"
	"github.com/sourcegraph/sourcegraph/internal/api"
	"github.com/sourcegraph/sourcegraph/internal/authz"
	"github.com/sourcegraph/sourcegraph/internal/conf"
	idb "github.com/sourcegraph/sourcegraph/internal/db"
	"github.com/sourcegraph/sourcegraph/internal/db/dbconn"
	"github.com/sourcegraph/sourcegraph/internal/debugserver"
	"github.com/sourcegraph/sourcegraph/internal/env"
	"github.com/sourcegraph/sourcegraph/internal/extsvc"
	"github.com/sourcegraph/sourcegraph/internal/gitserver"
	"github.com/sourcegraph/sourcegraph/internal/goroutine"
	"github.com/sourcegraph/sourcegraph/internal/httpcli"
	"github.com/sourcegraph/sourcegraph/internal/httpserver"
	"github.com/sourcegraph/sourcegraph/internal/logging"
	"github.com/sourcegraph/sourcegraph/internal/profiler"
	"github.com/sourcegraph/sourcegraph/internal/ratelimit"
	"github.com/sourcegraph/sourcegraph/internal/repos"
	"github.com/sourcegraph/sourcegraph/internal/trace"
	"github.com/sourcegraph/sourcegraph/internal/trace/ot"
	"github.com/sourcegraph/sourcegraph/internal/tracer"
	"github.com/sourcegraph/sourcegraph/internal/types"
	"github.com/sourcegraph/sourcegraph/schema"
)

const port = "3182"

// EnterpriseInit is a function that allows enterprise code to be triggered when dependencies
// created in Main are ready for use.
type EnterpriseInit func(db *sql.DB, store *repos.Store, cf *httpcli.Factory, server *repoupdater.Server) []debugserver.Dumper

func Main(enterpriseInit EnterpriseInit) {
	// NOTE: Internal actor is required to have full visibility of the repo table
	// 	(i.e. bypass repository authorization).
	ctx := actor.WithInternalActor(context.Background())
	env.Lock()
	env.HandleHelpFlag()

	if err := profiler.Init(); err != nil {
		log.Fatalf("failed to start profiler: %v", err)
	}

	logging.Init()
	tracer.Init()
	trace.Init(true)

	clock := func() time.Time { return time.Now().UTC() }

	// Syncing relies on access to frontend and git-server, so wait until they started up.
	if err := api.InternalClient.WaitForFrontend(ctx); err != nil {
		log.Fatalf("sourcegraph-frontend not reachable: %v", err)
	}
	log15.Debug("detected frontend ready")

	if err := gitserver.DefaultClient.WaitForGitServers(ctx); err != nil {
		log.Fatalf("gitservers not reachable: %v", err)
	}
	log15.Debug("detected gitservers ready")

	dsn := conf.Get().ServiceConnections.PostgresDSN
	conf.Watch(func() {
		newDSN := conf.Get().ServiceConnections.PostgresDSN
		if dsn != newDSN {
			// The DSN was changed (e.g. by someone modifying the env vars on
			// the frontend). We need to respect the new DSN. Easiest way to do
			// that is to restart our service (kubernetes/docker/goreman will
			// handle starting us back up).
			log.Fatalf("Detected repository DSN change, restarting to take effect: %q", newDSN)
		}
	})

	db, err := dbconn.New(dsn, "repo-updater")
	if err != nil {
		log.Fatalf("failed to initialize db store: %v", err)
	}

	repos.MustRegisterMetrics(db)

	store := repos.NewStore(db, sql.TxOptions{Isolation: sql.LevelDefault})
	{
		m := repos.NewStoreMetrics()
		m.MustRegister(prometheus.DefaultRegisterer)
		store.Metrics = m
	}

	cf := httpcli.NewExternalHTTPClientFactory()

	var src repos.Sourcer
	{
		m := repos.NewSourceMetrics()
		m.MustRegister(prometheus.DefaultRegisterer)

		src = repos.NewSourcer(cf, repos.ObservedSource(log15.Root(), m))
	}

	scheduler := repos.NewUpdateScheduler()
	server := &repoupdater.Server{
		Store:           store,
		Scheduler:       scheduler,
		GitserverClient: gitserver.DefaultClient,
	}

	rateLimitSyncer := repos.NewRateLimitSyncer(ratelimit.DefaultRegistry, store.ExternalServiceStore)
	server.RateLimitSyncer = rateLimitSyncer
	// Attempt to perform an initial sync with all external services
	if err := rateLimitSyncer.SyncRateLimiters(ctx); err != nil {
		// This is not a fatal error since the syncer has been added to the server above
		// and will still be run whenever an external service is added or updated
		log15.Error("Performing initial rate limit sync", "err", err)
	}

	// All dependencies ready
	var debugDumpers []debugserver.Dumper
	if enterpriseInit != nil {
		debugDumpers = enterpriseInit(db, store, cf, server)
	}

	if envvar.SourcegraphDotComMode() {
		server.SourcegraphDotComMode = true

		es, err := store.ExternalServiceStore.List(ctx, idb.ExternalServicesListOptions{
			// On Cloud we only want to fetch site level external services here where the
			// cloud_default flag has been set.
			NamespaceUserID:  -1,
			OnlyCloudDefault: true,
			Kinds:            []string{extsvc.KindGitHub, extsvc.KindGitLab},
		})

		if err != nil {
			log.Fatalf("failed to list external services: %v", err)
		}

		for _, e := range es {
			cfg, err := e.Configuration()
			if err != nil {
				log.Fatalf("bad external service config: %v", err)
			}

			switch c := cfg.(type) {
			case *schema.GitHubConnection:
				if strings.HasPrefix(c.Url, "https://github.com") && c.Token != "" {
					server.GithubDotComSource, err = repos.NewGithubSource(e, cf)
				}
			case *schema.GitLabConnection:
				if strings.HasPrefix(c.Url, "https://gitlab.com") && c.Token != "" {
					server.GitLabDotComSource, err = repos.NewGitLabSource(e, cf)
				}
			}

			if err != nil {
				log.Fatalf("failed to construct source: %v", err)
			}
		}

		if server.GithubDotComSource == nil {
			log.Fatalf("No github.com external service configured with a token")
		}

		if server.GitLabDotComSource == nil {
			log.Fatalf("No gitlab.com external service configured with a token")
		}
	}

	syncer := &repos.Syncer{
		Sourcer: src,
		Store:   store,
		// We always want to listen on the Synced channel since external service syncing
		// happens on both Cloud and non Cloud instances.
		Synced:     make(chan repos.Diff),
		Logger:     log15.Root(),
		Now:        clock,
		Registerer: prometheus.DefaultRegisterer,
	}

	var gps *repos.GitolitePhabricatorMetadataSyncer
	if !envvar.SourcegraphDotComMode() {
		gps = repos.NewGitolitePhabricatorMetadataSyncer(store)
		syncer.SubsetSynced = make(chan repos.Diff)
	}

	go watchSyncer(ctx, syncer, scheduler, gps)
	go func() {
		log.Fatal(syncer.Run(ctx, db, store, repos.RunOptions{
			EnqueueInterval: repos.ConfRepoListUpdateInterval,
			IsCloud:         envvar.SourcegraphDotComMode(),
			MinSyncInterval: repos.ConfRepoListUpdateInterval,
		}))
	}()
	server.Syncer = syncer

	go syncScheduler(ctx, scheduler, gitserver.DefaultClient, store)

	go repos.RunPhabricatorRepositorySyncWorker(ctx, store)

	if !envvar.SourcegraphDotComMode() {
		// git-server repos purging thread
		go repos.RunRepositoryPurgeWorker(ctx)
	}

	// Git fetches scheduler
	go repos.RunScheduler(ctx, scheduler)
	log15.Debug("started scheduler")

	host := ""
	if env.InsecureDev {
		host = "127.0.0.1"
	}

	addr := net.JoinHostPort(host, port)
	log15.Info("repo-updater: listening", "addr", addr)

	var handler http.Handler
	{
		m := repoupdater.NewHandlerMetrics()
		m.MustRegister(prometheus.DefaultRegisterer)

		handler = repoupdater.ObservedHandler(
			log15.Root(),
			m,
			opentracing.GlobalTracer(),
		)(server.Handler())
	}

	globals.WatchExternalURL(nil)
	go debugserver.Start(debugserver.Endpoint{
		Name: "Repo Updater State",
		Path: "/repo-updater-state",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			dumps := []interface{}{
				scheduler.DebugDump(),
			}
			for _, dumper := range debugDumpers {
				dumps = append(dumps, dumper.DebugDump())
			}

			const (
				textPlain       = "text/plain"
				applicationJson = "application/json"
			)

			// Negotiate the content type.
			contentTypeOffers := []string{textPlain, applicationJson}
			defaultOffer := textPlain
			contentType := httputil.NegotiateContentType(r, contentTypeOffers, defaultOffer)

			// Allow users to override the negotiated content type so that e.g. browser
			// users can easily request json by adding ?format=json to
			// the URL.
			switch r.URL.Query().Get("format") {
			case "json":
				contentType = applicationJson
			}

			switch contentType {
			case applicationJson:
				p, err := json.MarshalIndent(dumps, "", "  ")
				if err != nil {
					http.Error(w, "failed to marshal snapshot: "+err.Error(), http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(p)

			default:
				// This case also applies for defaultOffer. Note that this is preferred
				// over e.g. a 406 status code, according to the MDN:
				// https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/406
				tmpl := template.New("state.html").Funcs(template.FuncMap{
					"truncateDuration": func(d time.Duration) time.Duration {
						return d.Truncate(time.Second)
					},
				})
				template.Must(tmpl.Parse(assets.MustAssetString("state.html.tmpl")))
				err := tmpl.Execute(w, dumps)
				if err != nil {
					http.Error(w, "failed to render template: "+err.Error(), http.StatusInternalServerError)
					return
				}
			}
		}),
	}, debugserver.Endpoint{
		Name: "List Authz Providers",
		Path: "/list-authz-providers",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			type providerInfo struct {
				ServiceType        string `json:"service_type"`
				ServiceID          string `json:"service_id"`
				ExternalServiceURL string `json:"external_service_url"`
			}

			_, providers := authz.GetProviders()
			infos := make([]providerInfo, len(providers))
			for i, p := range providers {
				_, id := extsvc.DecodeURN(p.URN())

				// Note that the ID marshalling below replicates code found in `graphqlbackend`.
				// We cannot import that package's code into this one (see /dev/check/go-dbconn-import.sh).
				infos[i] = providerInfo{
					ServiceType:        p.ServiceType(),
					ServiceID:          p.ServiceID(),
					ExternalServiceURL: fmt.Sprintf("%s/site-admin/external-services/%s", globals.ExternalURL(), relay.MarshalID("ExternalService", id)),
				}
			}

			resp, err := json.MarshalIndent(infos, "", "  ")
			if err != nil {
				http.Error(w, "failed to marshal infos: "+err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(resp)
		}),
	},
	)

	// NOTE: Internal actor is required to have full visibility of the repo table
	// 	(i.e. bypass repository authorization).
	authzBypass := func(f http.Handler) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(actor.WithInternalActor(r.Context()))
			f.ServeHTTP(w, r)
		}
	}
	httpSrv := httpserver.NewFromAddr(addr, &http.Server{
		ReadTimeout:  75 * time.Second,
		WriteTimeout: 10 * time.Minute,
		Handler:      ot.Middleware(authzBypass(handler)),
	})
	goroutine.MonitorBackgroundRoutines(ctx, httpSrv)
}

type scheduler interface {
	// UpdateFromDiff updates the scheduled and queued repos from the given sync diff.
	UpdateFromDiff(repos.Diff)

	// SetCloned ensures uncloned repos are given priority in the scheduler.
	SetCloned([]string)

	// EnsureScheduled ensures that all the repos provided are known to the scheduler
	EnsureScheduled([]*types.RepoName)
}

func watchSyncer(ctx context.Context, syncer *repos.Syncer, sched scheduler, gps *repos.GitolitePhabricatorMetadataSyncer) {
	log15.Debug("started new repo syncer updates scheduler relay thread")

	for {
		select {
		case <-ctx.Done():
			return
		case diff := <-syncer.Synced:
			if !conf.Get().DisableAutoGitUpdates {
				sched.UpdateFromDiff(diff)
			}
			if gps == nil {
				continue
			}
			go func() {
				if err := gps.Sync(ctx, diff.Repos()); err != nil {
					log15.Error("GitolitePhabricatorMetadataSyncer", "error", err)
				}
			}()

		case diff := <-syncer.SubsetSynced:
			if !conf.Get().DisableAutoGitUpdates {
				sched.UpdateFromDiff(diff)
			}
		}
	}
}

// syncScheduler will periodically list the cloned repositories on gitserver and
// update the scheduler with the list. It also ensures that if any of our default
// repos are missing from the cloned list they will be added for cloning ASAP.
func syncScheduler(ctx context.Context, sched scheduler, gitserverClient *gitserver.Client, store *repos.Store) {
	baseRepoStore := idb.ReposWith(store)

	doSync := func() {
		cloned, err := gitserverClient.ListCloned(ctx)
		if err != nil {
			log15.Warn("failed to fetch list of cloned repositories", "error", err)
			return
		}

		err = store.SetClonedRepos(ctx, cloned...)
		if err != nil {
			log15.Warn("failed to set cloned repository list", "error", err)
			return
		}

		// Fetch all default repos that are NOT cloned so that we can add them to the
		// scheduler

		batchSize := 30_000
		opts := idb.ListDefaultReposOptions{
			Limit:        batchSize,
			AfterID:      0,
			OnlyUncloned: true,
		}

		for {
			batch, err := baseRepoStore.ListDefaultRepos(ctx, opts)
			if err != nil {
				log15.Error("Listing default repos", "error", err)
				return
			}

			// Ensure that uncloned repos are known to the scheduler
			sched.EnsureScheduled(batch)

			if len(batch) < batchSize {
				break
			}
			opts.AfterID = int32(batch[len(batch)-1].ID)
		}

		// Ensure that any uncloned repos are moved to the front of the schedule
		sched.SetCloned(cloned)
	}

	for ctx.Err() == nil {
		doSync()
		select {
		case <-ctx.Done():
		case <-time.After(30 * time.Second):
		}
	}
}
