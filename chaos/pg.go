package chaos

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

var dbName = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)

// FreshDB creates a database for one run on the server admin names, and
// returns its DSN plus a function that drops it. The campaign's PostgreSQL is a
// throwaway one (the CI service): the run never touches another database.
func FreshDB(ctx context.Context, admin string) (dsn string, drop func(), err error) {
	c, err := pgx.Connect(ctx, admin)
	if err != nil {
		return "", nil, fmt.Errorf("postgres: %w", err)
	}
	defer c.Close(ctx)
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", nil, err
	}
	name := "llmchaos_" + hex.EncodeToString(b[:])
	if !dbName.MatchString(name) {
		return "", nil, fmt.Errorf("database name %q", name)
	}
	if _, err := c.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		return "", nil, err
	}
	u, err := url.Parse(admin)
	if err != nil {
		return "", nil, err
	}
	u.Path = "/" + name
	drop = func() {
		cctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if c, err := pgx.Connect(cctx, admin); err == nil {
			_, _ = c.Exec(cctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			_ = c.Close(cctx)
		}
	}
	return u.String(), drop, nil
}

// viaProxy rewrites a DSN's host to the proxy's address, and returns the
// original host:port as the proxy's target.
func viaProxy(dsn string, px func(target string) (*Proxy, error)) (string, *Proxy, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", nil, err
	}
	p, err := px(u.Host)
	if err != nil {
		return "", nil, err
	}
	u.Host = p.Addr()
	return u.String(), p, nil
}
