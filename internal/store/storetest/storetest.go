//go:build integration

package storetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/luquinha/vigia/internal/store"
)

var (
	adminOnce sync.Once
	adminURL  string
	adminErr  error
)

func New(t *testing.T) *store.DB {
	t.Helper()
	ctx := context.Background()

	adminOnce.Do(func() {
		if u := os.Getenv("VIGIA_TEST_DATABASE_URL"); u != "" {
			adminURL = u
			return
		}
		adminURL, adminErr = startContainer(ctx)
	})
	if adminErr != nil {
		t.Fatalf("subir postgres de teste: %v", adminErr)
	}

	name := "vigia_test_" + randomSuffix()
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("conectar como admin: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("criar banco de teste: %v", err)
	}
	admin.Close(ctx)

	dbURL, err := withDatabase(adminURL, name)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, dbURL, 5)
	if err != nil {
		t.Fatalf("abrir banco de teste: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := store.Migrate(ctx, db.Pool, "up", logger); err != nil {
		t.Fatalf("migrations: %v", err)
	}

	t.Cleanup(func() {
		db.Close()
		admin, err := pgx.Connect(context.Background(), adminURL)
		if err != nil {
			return
		}
		defer admin.Close(context.Background())
		admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	return db
}

func startContainer(ctx context.Context) (string, error) {
	c, err := postgres.Run(ctx, "postgres:17-alpine",
		postgres.WithDatabase("vigia"),
		postgres.WithUsername("vigia"),
		postgres.WithPassword("vigia"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		return "", err
	}
	return c.ConnectionString(ctx, "sslmode=disable")
}

func withDatabase(raw, name string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("url do banco de teste: %w", err)
	}
	u.Path = "/" + name
	return u.String(), nil
}

func randomSuffix() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}
