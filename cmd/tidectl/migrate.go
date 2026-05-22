package main

import (
	"fmt"
	"os"
	"os/exec"
)

// tidectl shells out to the golang-migrate `migrate` binary for actual
// migration execution. We avoid importing golang-migrate as a library:
//
//  - The CLI tool is what every dev already has installed via the Makefile.
//  - Shelling out keeps tidectl's binary surface small.
//  - Migration history collision risk is addressed by passing
//    x-migrations-table=atlantis_schema_migrations on every call,
//    hardcoded here rather than left to the caller.
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
