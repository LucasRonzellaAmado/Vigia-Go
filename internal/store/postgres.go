package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

var ErrNotFound = errors.New("registro não encontrado")

type DB struct {
	*Queries
	Pool *pgxpool.Pool
}

func Open(ctx context.Context, url string, maxConns int32) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.ConnConfig.RuntimeParams["timezone"] = "UTC"
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		conn.TypeMap().RegisterType(&pgtype.Type{
			Name:  "timestamptz",
			OID:   pgtype.TimestamptzOID,
			Codec: &pgtype.TimestamptzCodec{ScanLocation: time.UTC},
		})
		return nil
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return &DB{Queries: New(pool), Pool: pool}, nil
}

func (db *DB) Close() {
	db.Pool.Close()
}

func (db *DB) Ping(ctx context.Context) error {
	return db.Pool.Ping(ctx)
}

func (db *DB) InTx(ctx context.Context, fn func(q *Queries) error) error {
	tx, err := db.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	if err := fn(db.Queries.WithTx(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

func Migrate(ctx context.Context, pool *pgxpool.Pool, command string, logger *slog.Logger) error {
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer sqlDB.Close()

	fsys, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("migrations fs: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, fsys)
	if err != nil {
		return fmt.Errorf("goose provider: %w", err)
	}

	switch command {
	case "up":
		results, err := provider.Up(ctx)
		for _, r := range results {
			logger.Info("migration aplicada", "source", r.Source.Path, "duration", r.Duration)
		}
		if err != nil {
			return fmt.Errorf("goose up: %w", err)
		}
	case "down":
		r, err := provider.Down(ctx)
		if err != nil {
			return fmt.Errorf("goose down: %w", err)
		}
		logger.Info("migration revertida", "source", r.Source.Path)
	case "status":
		statuses, err := provider.Status(ctx)
		if err != nil {
			return fmt.Errorf("goose status: %w", err)
		}
		for _, st := range statuses {
			logger.Info("migration", "source", st.Source.Path, "state", st.State, "applied_at", st.AppliedAt)
		}
	default:
		return fmt.Errorf("comando de migration desconhecido: %q (use up, down ou status)", command)
	}
	return nil
}

func IsNotFound(err error) bool {
	return errors.Is(err, pgx.ErrNoRows) || errors.Is(err, ErrNotFound)
}

func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
