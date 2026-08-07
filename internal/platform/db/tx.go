package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Querier is the read/write surface every repository is written against. Both
// *pgxpool.Pool and pgx.Tx satisfy it, which is what makes the unit of work
// invisible to repository code.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error)
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

type txKey struct{}

// FromContext returns the transaction travelling in ctx, falling back to fallback.
// Transactions travel in the context: there are no WithTx(tx) method variants
// anywhere in this codebase.
func FromContext(ctx context.Context, fallback Querier) Querier {
	if q, ok := ctx.Value(txKey{}).(Querier); ok && q != nil {
		return q
	}
	return fallback
}

// InTx reports whether ctx is already inside a transaction.
func InTx(ctx context.Context) bool {
	_, ok := ctx.Value(txKey{}).(Querier)
	return ok
}

func intoContext(ctx context.Context, q Querier) context.Context {
	return context.WithValue(ctx, txKey{}, q)
}

// Tx runs fn inside a single transaction on pool, committing when fn returns nil
// and rolling back otherwise. The transaction is placed in the context handed to
// fn, so every repository call underneath it joins the same unit of work.
//
// Tx nests safely: if ctx already carries a transaction, fn is run on that one
// and no second BEGIN is issued. A nested fn returning an error therefore fails
// the whole outer unit of work, which is the intended semantics.
func Tx(ctx context.Context, pool *pgxpool.Pool, fn func(ctx context.Context) error) error {
	return TxOptions(ctx, pool, pgx.TxOptions{}, fn)
}

// TxOptions is Tx with explicit transaction options (isolation, access mode).
func TxOptions(ctx context.Context, pool *pgxpool.Pool, opts pgx.TxOptions, fn func(ctx context.Context) error) (err error) {
	if InTx(ctx) {
		return fn(ctx)
	}
	if pool == nil {
		return errors.New("db: Tx called with a nil pool")
	}

	tx, err := pool.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("db: begin: %w", err)
	}

	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			panic(p)
		}
		if err != nil {
			if rbErr := tx.Rollback(context.WithoutCancel(ctx)); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
				err = errors.Join(err, fmt.Errorf("db: rollback: %w", rbErr))
			}
		}
	}()

	if err = fn(intoContext(ctx, tx)); err != nil {
		return err
	}

	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: commit: %w", err)
	}
	return nil
}
