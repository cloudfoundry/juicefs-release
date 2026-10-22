package backend

import (
	"context"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"
)

//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 -generate
//counterfeiter:generate -o ../../fakes/http_client.go . HTTPClient
//counterfeiter:generate -o ../../fakes/database.go . Database
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}
type Database interface {
	Check(context.Context, string) error
}

type Postgres struct{}

func (Postgres) Check(ctx context.Context, uri string) error {
	db, err := pgx.Connect(ctx, uri)
	if err != nil {
		return err
	}
	defer db.Close(context.Background())
	var result int
	if err := db.QueryRow(ctx, "SELECT 1").Scan(&result); err != nil {
		return err
	}
	if result != 1 {
		return fmt.Errorf("postgres SELECT 1 returned %d", result)
	}
	return nil
}
