package pgproxy

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestSchemaInstalls(t *testing.T) {
	u := os.Getenv("OOPS_TEST_DATABASE_URL")
	if u == "" {
		t.Skip()
	}
	c, err := pgx.Connect(context.Background(), u)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Exec(context.Background(), schemaSQL)
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		p := int(pe.Position)
		lo := p - 200
		if lo < 0 {
			lo = 0
		}
		t.Fatalf("%v at %d:\n%s<<<HERE>>>%s", err, p, schemaSQL[lo:p], schemaSQL[p:p+50])
	}
}
