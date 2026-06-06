package main

import (
	"fmt"
	"os"
	"os/exec"
)

// tidectl shells out to the golang-migrate `migrate` binary for actual
// migration execution. We shell out instead of importing the library:
//
//  - The CLI surface (-path, -database, x-migrations-table) is documented
//    and stable across golang-migrate versions; the Go API is not.
//  - cmd/server already takes the library dependency for AUTO_MIGRATE;
//    tidectl staying CLI-only keeps the tidectl binary smaller for
//    operators who only need plan/approve.
//  - x-migrations-table=atlantis_schema_migrations is pinned here so
//    tidectl never collides with another service's _schema_migrations
//    history if the two share a database.
//
// Operators who prefer the bare `migrate` tool can use it directly; tidectl
// migrate-up / migrate-down are conveniences.

const migrationsTable = "atlantis_schema_migrations"

func cmdMigrateUp(args []string) int   { return runMigrate(args, "up") }
func cmdMigrateDown(args []string) int { return runMigrate(args, "down", "1") }

func runMigrate(args []string, migrateArgs ...string) int {
	fs := flagSet("migrate")
	migrationsDir := fs.String("migrations-dir", "migrations", "Directory containing migrations")
	pgURL := fs.String("pg-url", os.Getenv("PG_URL"), "Postgres URL (defaults to $PG_URL)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *pgURL == "" {
		fmt.Fprintln(os.Stderr, "migrate: -pg-url or $PG_URL required")
		return 2
	}

	migrateURL := *pgURL
	// Append the dedicated migrations-table param so we never collide with
	// any other service's schema_migrations history.
	sep := "?"
	for _, c := range migrateURL {
		if c == '?' {
			sep = "&"
			break
		}
	}
	migrateURL = migrateURL + sep + "x-migrations-table=" + migrationsTable

	cmdArgs := []string{"-path", *migrationsDir, "-database", migrateURL}
	cmdArgs = append(cmdArgs, migrateArgs...)
	cmd := exec.Command("migrate", cmdArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		return 1
	}
	return 0
}
