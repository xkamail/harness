package pgharness

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	"github.com/acoshift/pgsql/pgctx"
)

//go:embed schema/*.sql
var schemaFS embed.FS

// Migrate applies embedded schema/NNNN_*.sql files not in harness.schema_version, each in its own tx.
// An advisory lock makes it safe for every instance to call at startup.
func Migrate(ctx context.Context) error {
	files, err := fs.Glob(schemaFS, "schema/*.sql")
	if err != nil {
		return err
	}
	for _, f := range files {
		name := strings.TrimPrefix(f, "schema/")
		version, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil {
			return fmt.Errorf("pgharness: migration %s: bad version", name)
		}
		sql, err := schemaFS.ReadFile(f)
		if err != nil {
			return err
		}
		err = pgctx.RunInTx(ctx, func(ctx context.Context) error {
			_, err := pgctx.Exec(ctx, `
				select pg_advisory_xact_lock(hashtext('pgharness.migrate'));
				create schema if not exists harness;
				create table if not exists harness.schema_version (
					version    int primary key,
					applied_at timestamptz not null default now()
				);
			`)
			if err != nil {
				return err
			}
			var applied bool
			err = pgctx.QueryRow(ctx, `
				select exists (
					select 1
					from harness.schema_version
					where version = $1
				)
			`, version).Scan(&applied)
			if err != nil || applied {
				return err
			}
			if _, err := pgctx.Exec(ctx, string(sql)); err != nil {
				return err
			}
			_, err = pgctx.Exec(ctx, `
				insert into harness.schema_version (version)
				values ($1)
			`, version)
			return err
		})
		if err != nil {
			return fmt.Errorf("pgharness: migration %s: %w", name, err)
		}
	}
	return nil
}
