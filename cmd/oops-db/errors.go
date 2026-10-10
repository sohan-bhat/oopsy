package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5/pgconn"
)

// dbError explains a failure to talk to Postgres in plain words, with what
// to do next. raw is the URL that was used, for naming the server.
func dbError(err error, raw string) error {
	if err == nil {
		return nil
	}
	where, user, db := describeURL(raw)
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr):
		switch pgErr.Code {
		case "28P01", "28000":
			return fmt.Errorf("Postgres at %s turned down the login for %s: %s. Check the user and password in the URL (--url or OOPS_DATABASE_URL)", where, user, pgErr.Message)
		case "3D000":
			return fmt.Errorf("there is no database %s on %s. Check the name in the URL, or run oops db list to see dumps of dropped databases", db, where)
		case "42501":
			return fmt.Errorf("%s does not have permission for this on %s: %s. Connect as the owner or a superuser", user, where, pgErr.Message)
		}
		return fmt.Errorf("Postgres at %s said: %s (code %s)", where, pgErr.Message, pgErr.Code)
	case errors.Is(err, syscall.ECONNREFUSED) || strings.Contains(err.Error(), "connection refused"):
		return fmt.Errorf("could not reach Postgres at %s: nothing is listening there. Start Postgres, or point Oopsy at the right server with --url or OOPS_DATABASE_URL", where)
	case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
		return fmt.Errorf("Postgres at %s did not answer in time. Check that the host is reachable and the server is running", where)
	case strings.Contains(err.Error(), "no such host"):
		return fmt.Errorf("could not find the host in %s. Check the URL for typos", where)
	}
	return fmt.Errorf("could not talk to Postgres at %s: %v", where, err)
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// describeURL names the server, user and database in a URL, never the password.
func describeURL(raw string) (where, user, db string) {
	where, user, db = "the database", "this user", "the database"
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		if cfg, err := pgconn.ParseConfig(raw); err == nil {
			return fmt.Sprintf("%s:%d", cfg.Host, cfg.Port), cfg.User, cfg.Database
		}
		return
	}
	where = u.Host
	if u.User != nil && u.User.Username() != "" {
		user = u.User.Username()
	}
	if d := strings.TrimPrefix(u.Path, "/"); d != "" {
		db = d
	}
	return
}
