package cli

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/api"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/config"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/consolidate"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/embed"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/logx"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/store"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/version"
)

func cmdServe(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(Err)
	configPath := fs.String("config", config.DefaultConfigPath(), "path to config.yaml")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		// Configuration errors are printed bare, without the "dkm:" prefix.
		// They are multi-line and name a file, a line, and a key; a prefix on
		// the first line only makes them harder to read.
		fmt.Fprintln(Err, err)
		return 1
	}

	log, err := logx.New(cfg.Log.Level, cfg.Log.Format, Err)
	if err != nil {
		return fail("%v", err)
	}

	st, err := store.Open(ctx, cfg)
	if err != nil {
		return fail("%v", err)
	}
	defer st.Close()

	if cfg.Database.MigrateOnStart {
		res, err := st.Migrate(ctx)
		if err != nil {
			return fail("%v", err)
		}
		if len(res.Applied) > 0 {
			log.Info("migrations applied", "count", len(res.Applied), "version", res.Current)
		}
	} else {
		current, err := st.SchemaVersion(ctx)
		if err != nil {
			return fail("reading the schema version: %v", err)
		}
		if latest := store.LatestVersion(); current < latest {
			// Refuse rather than auto-migrate. Applying schema changes as a
			// side effect of a restart means a rollback of the binary leaves a
			// schema the old build has never seen.
			return fail("database schema is at version %d but this build expects %d.\n\n  dkm migrate --config %s\n\nOr set database.migrate_on_start: true.",
				current, latest, *configPath)
		}
	}

	if issued, err := st.Bootstrap(ctx, "default", "Default team", "admin", "Admin"); err != nil {
		log.Warn("bootstrap check failed", "error", err)
	} else if issued != nil {
		// Printed to stdout, once, and never logged: a key in a log file is a
		// key in a log aggregator, a backup, and whatever reads them.
		rule := strings.Repeat("─", 68)
		fmt.Fprintf(Out, "\n%s\n", rule)
		fmt.Fprint(Out, "  First boot. An admin key has been issued and will not be shown again:\n\n")
		fmt.Fprintf(Out, "      %s\n\n", issued.Plaintext)
		fmt.Fprint(Out, "  Save it now, then connect a client:\n")
		fmt.Fprintf(Out, "      dkm login %s\n", cfg.Server.PublicURL)
		fmt.Fprintf(Out, "%s\n\n", rule)
		log.Info("issued bootstrap admin key", "key_id", issued.ID, "prefix", issued.Prefix)
	}

	embedder, err := embed.New(cfg)
	if err != nil {
		return fail("%v", err)
	}
	if err := embedder.Health(ctx); err != nil {
		// A degraded embedder is a warning, not a failure. Keyword search still
		// works and writes still land; the backfill pass vectorises them once
		// the sidecar returns.
		log.Warn("embedder is not ready; starting anyway with keyword-only search",
			"provider", embedder.Name(), "error", err)
	}

	worker, err := consolidate.NewWorker(cfg, st, embedder, log)
	if err != nil {
		return fail("%v", err)
	}

	srv := api.New(api.Options{
		Config:       cfg,
		Store:        st,
		Embedder:     embedder,
		Logger:       log,
		Consolidator: consolidatorAdapter{worker},
	})

	go worker.Run(ctx)

	log.Info("starting",
		"version", version.Short(),
		"config", *configPath,
		"embedder", embedder.Name(),
		"consolidation", cfg.Consolidation.Enabled)

	if err := srv.ListenAndServe(ctx); err != nil {
		return fail("%v", err)
	}
	log.Info("stopped cleanly")
	return 0
}

// consolidatorAdapter widens the worker's typed report to the `any` the API
// package's interface takes.
//
// The alternative — making RunNow return `any` — would throw away the type
// everywhere it is genuinely useful just to satisfy one boundary. Adapting at
// the boundary keeps the loss where it belongs.
type consolidatorAdapter struct{ w *consolidate.Worker }

func (a consolidatorAdapter) RunNow(ctx context.Context, tiers []int, drain bool) (any, error) {
	return a.w.RunNow(ctx, tiers, drain)
}
func (a consolidatorAdapter) Enabled() bool          { return a.w.Enabled() }
func (a consolidatorAdapter) Provider() string       { return a.w.Provider() }
func (a consolidatorAdapter) BatchSize() int         { return a.w.BatchSize() }
func (a consolidatorAdapter) DisabledReason() string { return a.w.DisabledReason() }

func cmdMigrate(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(Err)
	configPath := fs.String("config", config.DefaultConfigPath(), "path to config.yaml")
	dryRun := fs.Bool("dry-run", false, "list pending migrations without applying them")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(Err, err)
		return 1
	}

	st, err := store.Open(ctx, cfg)
	if err != nil {
		return fail("%v", err)
	}
	defer st.Close()

	current, err := st.SchemaVersion(ctx)
	if err != nil {
		return fail("%v", err)
	}

	if *dryRun {
		migrations, err := store.LoadMigrations(cfg.Embedding.Dimensions)
		if err != nil {
			return fail("%v", err)
		}
		pending := 0
		for _, m := range migrations {
			if m.Version > current {
				fmt.Fprintf(Out, "pending  %04d_%s\n", m.Version, m.Name)
				pending++
			}
		}
		if pending == 0 {
			fmt.Fprintf(Out, "Schema is up to date at version %d. Nothing to apply.\n", current)
		}
		return 0
	}

	res, err := st.Migrate(ctx)
	if err != nil {
		return fail("%v", err)
	}

	if len(res.Applied) == 0 {
		// The idempotency guarantee, stated plainly: running this twice is safe
		// and the second run says so rather than staying silent.
		fmt.Fprintf(Out, "Schema is already at version %d. Nothing to do.\n", res.Current)
		return 0
	}
	for _, m := range res.Applied {
		fmt.Fprintf(Out, "applied  %04d_%s\n", m.Version, m.Name)
	}
	fmt.Fprintf(Out, "\nSchema is now at version %d.\n", res.Current)
	return 0
}
