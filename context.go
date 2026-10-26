package gorm

import (
	"context"
	"database/sql"
)

// WithContext returns a clone with ctx, preserving the current model and search
// conditions. Context-aware execution is opt-in; ordinary DB handles retain the
// legacy execution paths. A nil context selects context.Background(). Connections
// implementing only SQLCommon retain their legacy behavior; cancellation during
// execution requires the corresponding database/sql context methods.
func (s *DB) WithContext(ctx context.Context) *DB {
	if ctx == nil {
		ctx = context.Background()
	}
	return s.cloneWithContext(ctx)
}

// Context returns the context used by this DB, including in callbacks and hooks.
func (s *DB) Context() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

// sqlDB binds dialect and internal SQL calls to the current DB context without
// changing SQLCommon or the underlying connection exposed by CommonDB/SQLDB.
func (s *DB) sqlDB() SQLCommon {
	if s.ctx == nil {
		return s.db
	}
	return contextSQLCommon{db: s}
}

type contextSQLCommon struct {
	db *DB
}

func (s contextSQLCommon) Exec(query string, args ...interface{}) (sql.Result, error) {
	ctx := s.db.Context()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if db, ok := s.db.db.(interface {
		ExecContext(context.Context, string, ...interface{}) (sql.Result, error)
	}); ok {
		return db.ExecContext(ctx, query, args...)
	}
	return s.db.db.Exec(query, args...)
}

func (s contextSQLCommon) Prepare(query string) (*sql.Stmt, error) {
	ctx := s.db.Context()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if db, ok := s.db.db.(interface {
		PrepareContext(context.Context, string) (*sql.Stmt, error)
	}); ok {
		return db.PrepareContext(ctx, query)
	}
	return s.db.db.Prepare(query)
}

func (s contextSQLCommon) Query(query string, args ...interface{}) (*sql.Rows, error) {
	ctx := s.db.Context()
	// Dialect probes return booleans, so retain cancellation on the DB as well.
	defer func() { s.db.AddError(ctx.Err()) }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if db, ok := s.db.db.(interface {
		QueryContext(context.Context, string, ...interface{}) (*sql.Rows, error)
	}); ok {
		return db.QueryContext(ctx, query, args...)
	}
	return s.db.db.Query(query, args...)
}

func (s contextSQLCommon) QueryRow(query string, args ...interface{}) *sql.Row {
	ctx := s.db.Context()
	defer func() { s.db.AddError(ctx.Err()) }()
	if db, ok := s.db.db.(interface {
		QueryRowContext(context.Context, string, ...interface{}) *sql.Row
	}); ok {
		return db.QueryRowContext(ctx, query, args...)
	}
	return s.db.db.QueryRow(query, args...)
}
