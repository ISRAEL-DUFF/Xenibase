package pgverify

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// TableCounts reads only the tables in the schemas asked for, so a source
// login that may read the imported schema and nothing else can be
// verified; a table elsewhere it may not read used to fail the import.
func TestTableCountsReadsOnlyTheChosenSchemas(t *testing.T) {
	ctx := context.Background()
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	role := "pgverify_" + hex.EncodeToString(b)
	// Registered first, so it runs after the database is dropped.
	t.Cleanup(func() {
		if admin, err := pgx.Connect(context.Background(), os.Getenv(storetest.EnvVar)); err == nil {
			_, _ = admin.Exec(context.Background(), `DROP ROLE IF EXISTS `+role)
			_ = admin.Close(context.Background())
		}
	})
	pool := storetest.NewEmpty(t)
	db := pool.Config().ConnConfig.Database
	for _, stmt := range []string{
		`CREATE SCHEMA shop`, `CREATE TABLE shop.items AS SELECT generate_series(1, 3) AS id`,
		`CREATE SCHEMA private`, `CREATE TABLE private.secrets AS SELECT 1 AS id`,
		`CREATE ROLE ` + role + ` LOGIN PASSWORD 'pw'`,
		`GRANT CONNECT ON DATABASE ` + db + ` TO ` + role,
		`GRANT USAGE ON SCHEMA shop, private TO ` + role, `GRANT SELECT ON shop.items TO ` + role,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	cfg, err := pgx.ParseConfig(os.Getenv(storetest.EnvVar))
	if err != nil {
		t.Fatal(err)
	}
	cfg.User, cfg.Password, cfg.Database = role, "pw", db
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	got, err := TableCounts(ctx, conn, []string{"shop"})
	if err != nil {
		t.Fatalf("counting the chosen schema: %v", err)
	}
	if len(got) != 1 || got[0].Table != "shop.items" || got[0].Rows != 3 {
		t.Fatalf("counts: %+v", got)
	}
	if _, err := TableCounts(ctx, conn, nil); err == nil {
		t.Fatal("every schema: read a table the login may not")
	}
}
