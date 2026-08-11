// Command mora-backfill runs the Phase 1 legacy-document online migration
// (design-docs/14 §3.2 backfill, §3.3 reconciliation). It registers existing
// documents — written before Phase 1 — as Document knowledge assets WITHOUT
// copying content, idempotently and resumably.
//
// Usage:
//
//	mora-backfill                 # run backfill then reconcile across all workspaces
//	mora-backfill --reconcile     # reconciliation scan only
//	mora-backfill --workspace <id> # scope to one workspace
//
// Environment:
//
//	DATABASE_URL                       mora PG DSN (required)
//	MIGRATION_SERVICE_ACCOUNT_ID      the service account approving legacy reviews (§3.4).
//	                                  If unset, the CLI resolves/creates a dedicated
//	                                  'mora-legacy-migration' service_account row.
//
// The process is one-shot: it exits when the scan converges (no more documents
// to register / no more mismatches). knowledge-worker (§5.2) embeds the same
// Runner behind a reconcile_scan job for ongoing reconciliation; this CLI is the
// initial migration entrypoint used during deployment (run after 014 applies).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lynn901/mora/internal/infra/postgres"
	"github.com/lynn901/mora/internal/module/knowledge/migration"
	"github.com/lynn901/mora/internal/platform/outbox"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	reconcileOnly := flag.Bool("reconcile", false, "run only the §3.3 reconciliation scan (skip initial backfill)")
	wsStr := flag.String("workspace", "", "scope to one workspace id (UUID); empty = all workspaces")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	dsn := env("DATABASE_URL", "")
	if dsn == "" {
		log.Error("DATABASE_URL is required")
		os.Exit(2)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Error("pg connect failed", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	saID, err := resolveServiceAccount(ctx, pool, log)
	if err != nil {
		log.Error("service account resolve failed", "err", err)
		os.Exit(1)
	}

	runner := migration.NewRunner(pool, postgres.NewAssetRegistry(), outbox.NewStore(), migration.Options{
		MigrationServiceAccountID: saID,
	}, log)

	wsID, wsScoped := parseWorkspace(*wsStr)

	if !*reconcileOnly {
		var n int
		var err error
		if wsScoped {
			n, err = runner.BackfillWorkspace(ctx, wsID)
		} else {
			n, err = runner.BackfillAll(ctx)
		}
		if err != nil {
			log.Error("backfill failed (resumable; re-run to continue)", "registered", n, "err", err)
			os.Exit(1)
		}
		log.Info("backfill complete", "registered", n)
	} else {
		log.Info("skipping backfill (--reconcile)")
	}

	rep, err := runner.Reconcile(ctx)
	if err != nil {
		log.Error("reconcile failed", "report", fmt.Sprintf("%+v", rep), "err", err)
		os.Exit(1)
	}
	log.Info("reconcile complete", "missing_assets", rep.MissingAssets, "missing_versions", rep.MissingVersions, "mismatches_repaired", rep.VersionMismatches)
}

// resolveServiceAccount returns the migration service account id, taking it from
// MIGRATION_SERVICE_ACCOUNT_ID when set, else resolving the dedicated
// 'mora-legacy-migration' service_accounts row (creating it idempotently).
// service_accounts.name has no unique constraint (001 schema), so this is a
// lookup-or-insert — a SELECT by name, INSERT only if absent.
func resolveServiceAccount(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) (uuid.UUID, error) {
	if v := env("MIGRATION_SERVICE_ACCOUNT_ID", ""); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			return uuid.Nil, fmt.Errorf("MIGRATION_SERVICE_ACCOUNT_ID: %w", err)
		}
		return id, nil
	}
	var id uuid.UUID
	err := pool.QueryRow(ctx, `SELECT id FROM service_accounts WHERE name = 'mora-legacy-migration' LIMIT 1`).Scan(&id)
	if err == nil {
		return id, nil
	}
	err = pool.QueryRow(ctx, `
		INSERT INTO service_accounts (name, description)
		VALUES ('mora-legacy-migration', 'Phase 1 §3.4 legacy-document migration: approves backfill review requests')
		RETURNING id`).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("create migration service account: %w", err)
	}
	log.Info("created migration service account", "id", id)
	return id, nil
}

func parseWorkspace(s string) (uuid.UUID, bool) {
	if s == "" {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(s)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --workspace uuid: %v\n", err)
		os.Exit(2)
	}
	return id, true
}
