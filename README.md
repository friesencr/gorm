# GORM

GORM V2 moved to https://github.com/go-gorm/gorm

GORM V1 Doc https://v1.gorm.io/

Context support is opt-in. Existing calls keep the original SQL methods,
connection types, and transaction behavior. Use `WithContext` to attach a request
context while preserving the current model and query conditions:

```go
err := db.Where("name = ?", name).WithContext(ctx).Find(&users).Error
err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
    return tx.Create(&user).Error
})
```

The context is inherited by chained operations, associations, preloads, schema
operations, and transactions. On a `WithContext` handle, `BeginTx(ctx, opts)` uses
its explicit context for subsequent operations. On an ordinary handle, `BeginTx`
keeps its original behavior: the context controls the transaction lifetime.
Hooks and callbacks can access an attached context with `db.Context()`.
`NewWithContext(ctx)` also clears the current model and query conditions.

`SQLCommon` remains compatible with existing connection wrappers. After opting in,
GORM uses the underlying connection's `ExecContext`, `QueryContext`,
`QueryRowContext`, and `PrepareContext` methods when available. Legacy wrappers
retain their original execution behavior; cancellation during execution requires
those context methods.
