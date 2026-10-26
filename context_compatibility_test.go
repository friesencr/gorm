package gorm_test

// These tests intentionally use only the public API used by the legacy app.
import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/friesencr/gorm"
)

type legacyCompatibilityModel struct {
	ID   uint
	Name string
}

// Overrides on a wrapper that embeds *sql.DB must not be bypassed by calling
// its promoted context methods when the application has not opted in.
type legacyCompatibilityConnection struct {
	*sql.DB
	execs, queries, rows, prepares, begins, beginTxs int
	contextCalls                                     int
	beginError                                       error
}

func (db *legacyCompatibilityConnection) Exec(query string, args ...interface{}) (sql.Result, error) {
	db.execs++
	return db.DB.Exec(query, args...)
}

func (db *legacyCompatibilityConnection) Query(query string, args ...interface{}) (*sql.Rows, error) {
	db.queries++
	return db.DB.Query(query, args...)
}

func (db *legacyCompatibilityConnection) QueryRow(query string, args ...interface{}) *sql.Row {
	db.rows++
	return db.DB.QueryRow(query, args...)
}

func (db *legacyCompatibilityConnection) Prepare(query string) (*sql.Stmt, error) {
	db.prepares++
	return db.DB.Prepare(query)
}

func (db *legacyCompatibilityConnection) Begin() (*sql.Tx, error) {
	db.begins++
	return db.DB.Begin()
}

func (db *legacyCompatibilityConnection) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	db.beginTxs++
	if db.beginError != nil {
		return nil, db.beginError
	}
	return db.DB.BeginTx(ctx, opts)
}

func (db *legacyCompatibilityConnection) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	db.contextCalls++
	return db.DB.ExecContext(ctx, query, args...)
}

func (db *legacyCompatibilityConnection) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	db.contextCalls++
	return db.DB.QueryContext(ctx, query, args...)
}

func (db *legacyCompatibilityConnection) QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row {
	db.contextCalls++
	return db.DB.QueryRowContext(ctx, query, args...)
}

func (db *legacyCompatibilityConnection) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	db.contextCalls++
	return db.DB.PrepareContext(ctx, query)
}

func openLegacyCompatibilityDB(t *testing.T) (*gorm.DB, *legacyCompatibilityConnection) {
	t.Helper()
	raw, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	raw.SetMaxOpenConns(1)
	connection := &legacyCompatibilityConnection{DB: raw}
	db, err := gorm.Open("sqlite3", connection)
	if err != nil {
		raw.Close()
		t.Fatal(err)
	}
	db.LogMode(false)
	if err := db.CreateTable(&legacyCompatibilityModel{}).Error; err != nil {
		raw.Close()
		t.Fatal(err)
	}
	return db, connection
}

func TestLegacyConnectionDispatch(t *testing.T) {
	db, connection := openLegacyCompatibilityDB(t)
	defer db.Close()
	if db.CommonDB() != connection || db.NewScope(nil).SQLDB() != connection {
		t.Fatal("connection accessors changed the underlying connection")
	}
	if err := db.Exec("INSERT INTO legacy_compatibility_models (name) VALUES (?)", "legacy").Error; err != nil {
		t.Fatal(err)
	}
	var model legacyCompatibilityModel
	if err := db.New().Where("name = ?", "legacy").First(&model).Error; err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.Model(&model).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("count=%d, error=%v", count, err)
	}
	if !db.HasTable(&legacyCompatibilityModel{}) {
		t.Fatal("legacy schema query failed")
	}
	stmt, err := db.CommonDB().Prepare("SELECT name FROM legacy_compatibility_models")
	if err != nil {
		t.Fatal(err)
	}
	stmt.Close()
	if err := db.Create(&legacyCompatibilityModel{Name: "implicit transaction"}).Error; err != nil {
		t.Fatal(err)
	}
	if connection.begins != 1 || connection.beginTxs != 0 {
		t.Fatalf("implicit writes changed transaction dispatch: Begin=%d, BeginTx=%d", connection.begins, connection.beginTxs)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		return tx.Create(&legacyCompatibilityModel{Name: "explicit transaction"}).Error
	}); err != nil {
		t.Fatal(err)
	}
	if connection.beginTxs != 1 {
		t.Fatal("explicit transactions must retain BeginTx dispatch")
	}
	if connection.execs == 0 || connection.queries == 0 || connection.rows == 0 || connection.prepares != 1 || connection.contextCalls != 0 {
		t.Fatalf("legacy methods were bypassed: %+v", connection)
	}
}

// A custom dialect may inspect the concrete connection it receives in SetDB.
type legacyCompatibilityDialect struct {
	gorm.Dialect
	connection gorm.SQLCommon
}

func (d *legacyCompatibilityDialect) GetName() string { return "legacy_compatibility" }
func (d *legacyCompatibilityDialect) SetDB(connection gorm.SQLCommon) {
	switch connection.(type) {
	case *sql.DB, *sql.Tx, *legacyCompatibilityConnection:
	default:
		panic("legacy dialect received a wrapped connection")
	}
	d.connection = connection
	inner, err := gorm.Open("sqlite3", connection)
	if err != nil {
		panic(err)
	}
	d.Dialect = inner.Dialect()
}

func TestLegacyDialectConnectionIdentity(t *testing.T) {
	original, connection := openLegacyCompatibilityDB(t)
	defer original.Close()
	gorm.RegisterDialect("legacy_compatibility", &legacyCompatibilityDialect{})
	for _, source := range []gorm.SQLCommon{connection.DB, connection} {
		db, err := gorm.Open("legacy_compatibility", source)
		if err != nil {
			t.Fatal(err)
		}
		for _, clone := range []*gorm.DB{db, db.New(), db.Where("name = ?", "legacy"), db.NewScope(nil).NewDB()} {
			if clone.Dialect().(*legacyCompatibilityDialect).connection != source {
				t.Fatal("dialect lost the original connection")
			}
		}
		// BeginTx predates this feature and must not opt an existing application
		// into connection wrapping or different SQL method dispatch.
		tx := db.BeginTx(context.Background(), &sql.TxOptions{})
		if tx.Error != nil {
			t.Fatal(tx.Error)
		}
		if tx.Dialect().(*legacyCompatibilityDialect).connection != tx.CommonDB() {
			t.Error("transaction dialect must receive the underlying *sql.Tx")
		}
		tx.Rollback()
	}
}

func TestLegacyTransactionBeginFailure(t *testing.T) {
	db, connection := openLegacyCompatibilityDB(t)
	defer db.Close()
	connection.beginError = errors.New("legacy begin failure")
	tx := db.Begin()
	if tx.Error != connection.beginError {
		t.Fatalf("unexpected begin error: %v", tx.Error)
	}
	if sqlTx, ok := tx.CommonDB().(*sql.Tx); !ok || sqlTx != nil {
		t.Fatal("failed Begin must preserve its nil *sql.Tx connection")
	}
	called := false
	err := db.Transaction(func(tx *gorm.DB) error {
		called = true
		return tx.Error
	})
	if !called || err != connection.beginError {
		t.Fatalf("legacy callback behavior changed: called=%v, error=%v", called, err)
	}
}

// The original transaction interface required both Begin and BeginTx.
type legacyBeginTxOnlyConnection struct {
	gorm.SQLCommon
	beginTx func(context.Context, *sql.TxOptions) (*sql.Tx, error)
}

func (db legacyBeginTxOnlyConnection) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	return db.beginTx(ctx, opts)
}

func TestLegacyTransactionCapabilityDetection(t *testing.T) {
	original, connection := openLegacyCompatibilityDB(t)
	defer original.Close()
	wrapped := legacyBeginTxOnlyConnection{SQLCommon: connection.DB, beginTx: connection.BeginTx}
	db, err := gorm.Open("sqlite3", wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&legacyCompatibilityModel{Name: "no automatic transaction"}).Error; err != nil {
		t.Fatal(err)
	}
	if connection.beginTxs != 0 || db.Begin().Error != gorm.ErrCantStartTransaction {
		t.Fatal("transaction capability detection changed")
	}
}

func TestLegacyRemoveIndexErrorBehavior(t *testing.T) {
	db, _ := openLegacyCompatibilityDB(t)
	defer db.Close()
	if err := db.Model(&legacyCompatibilityModel{}).RemoveIndex("missing_legacy_index").Error; err != nil {
		t.Fatalf("legacy RemoveIndex started returning errors: %v", err)
	}
}
