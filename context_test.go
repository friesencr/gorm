package gorm_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/friesencr/gorm"
)

type contextParent struct {
	ID       uint
	Name     string
	Children []contextChild
}

type contextChild struct {
	ID              uint
	ContextParentID uint
	Name            string
}

func openContextDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	if err := db.AutoMigrate(&contextParent{}, &contextChild{}).Error; err != nil {
		db.Close()
		t.Fatal(err)
	}
	parent := contextParent{Name: "parent", Children: []contextChild{{Name: "child"}}}
	if err := db.Create(&parent).Error; err != nil {
		db.Close()
		t.Fatal(err)
	}
	return db
}

func TestWithContextPreservesQuery(t *testing.T) {
	db := openContextDB(t)
	defer db.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	query := db.Model(&contextParent{}).Where("name = ?", "missing")
	bound := query.WithContext(ctx)
	var count int
	if err := bound.Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("WithContext lost model or conditions: count=%d, error=%v", count, err)
	}
	if bound.Context() != ctx || query.Context() == ctx {
		t.Fatal("WithContext must attach the context to a clone")
	}
	if bound.New().Context() != ctx || bound.NewScope(nil).New(nil).DB().Context() != ctx {
		t.Fatal("derived DBs and scopes must inherit the context")
	}
	fresh := bound.NewWithContext(context.Background())
	if fresh.Value != nil {
		t.Fatal("NewWithContext must clear the model")
	}
	if err := fresh.Model(&contextParent{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("NewWithContext retained conditions: count=%d, error=%v", count, err)
	}
	if bound.WithContext(nil).Context() == nil {
		t.Fatal("nil context must select the default context")
	}
}

func TestContextCancellation(t *testing.T) {
	db := openContextDB(t)
	defer db.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name string
		run  func(*gorm.DB) error
	}{
		{"find", func(db *gorm.DB) error { return db.Find(&[]contextParent{}).Error }},
		{"first", func(db *gorm.DB) error { return db.First(&contextParent{}).Error }},
		{"create", func(db *gorm.DB) error { return db.Create(&contextParent{Name: "canceled"}).Error }},
		{"update", func(db *gorm.DB) error {
			return db.Model(&contextParent{}).Where("id = ?", 1).Update("name", "canceled").Error
		}},
		{"delete", func(db *gorm.DB) error { return db.Delete(&contextParent{ID: 1}).Error }},
		{"exec", func(db *gorm.DB) error { return db.Exec("DELETE FROM context_parents").Error }},
		{"scan", func(db *gorm.DB) error { return db.Raw("SELECT * FROM context_parents").Scan(&[]contextParent{}).Error }},
		{"row", func(db *gorm.DB) error {
			var count int
			return db.Raw("SELECT count(*) FROM context_parents").Row().Scan(&count)
		}},
		{"rows", func(db *gorm.DB) error {
			rows, err := db.Model(&contextParent{}).Rows()
			if rows != nil {
				rows.Close()
			}
			return err
		}},
		{"pluck", func(db *gorm.DB) error { return db.Model(&contextParent{}).Pluck("name", &[]string{}).Error }},
		{"count", func(db *gorm.DB) error {
			var count int
			return db.Model(&contextParent{}).Count(&count).Error
		}},
		{"remove index", func(db *gorm.DB) error { return db.Model(&contextParent{}).RemoveIndex("unused").Error }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.run(db.WithContext(ctx)); err != context.Canceled {
				t.Fatalf("expected context.Canceled, got %v", err)
			}
		})
	}
	schema := db.WithContext(ctx)
	if schema.HasTable(&contextParent{}) || schema.Error != context.Canceled {
		t.Fatalf("canceled schema query must report cancellation: %v", schema.Error)
	}
	var parent contextParent
	if err := db.Preload("Children").First(&parent).Error; err != nil || parent.Name != "parent" || len(parent.Children) != 1 {
		t.Fatalf("canceled operations changed data or leaked into the original DB: parent=%+v, error=%v", parent, err)
	}
}

func TestContextDeadlineWhileWaitingForConnection(t *testing.T) {
	db := openContextDB(t)
	defer db.Close()
	// Occupy the only connection so cancellation must interrupt a pending query.
	blocker, err := db.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if err := db.WithContext(ctx).Find(&[]contextParent{}).Error; err != context.DeadlineExceeded {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
}

func TestContextTransactions(t *testing.T) {
	db := openContextDB(t)
	defer db.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := db.WithContext(ctx).Begin().Error; err != context.Canceled {
		t.Fatalf("Begin ignored cancellation: %v", err)
	}
	called := false
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		called = true
		return nil
	})
	if err != context.Canceled || called {
		t.Fatalf("Transaction must stop when Begin fails: called=%v, error=%v", called, err)
	}
	live, stop := context.WithCancel(context.Background())
	defer stop()
	tx := db.WithContext(ctx).BeginTx(live, nil)
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	if tx.Context() != live {
		t.Fatal("BeginTx must attach its explicit context")
	}
	if err := tx.Create(&contextParent{Name: "transaction"}).Error; err != nil {
		t.Fatal(err)
	}
	if !tx.HasTable(&contextParent{}) {
		t.Fatal("schema operations must use the transaction's connection")
	}
}

// Embedding only SQLCommon deliberately hides the database/sql context methods.
// This models existing connection wrappers that must remain source compatible.
type legacyContextConnection struct {
	gorm.SQLCommon
}

func TestContextLegacyConnection(t *testing.T) {
	original := openContextDB(t)
	defer original.Close()
	legacy := legacyContextConnection{SQLCommon: original.CommonDB()}
	db, err := gorm.Open("sqlite3", legacy)
	if err != nil {
		t.Fatal(err)
	}
	if db.CommonDB() != legacy || db.NewScope(nil).SQLDB() != legacy {
		t.Fatal("public connection accessors must expose the original connection")
	}
	var parent contextParent
	if err := db.WithContext(context.Background()).First(&parent).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(context.Background()).Exec("UPDATE context_parents SET name = ?", "legacy").Error; err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.Raw("SELECT count(*) FROM context_parents").Row().Scan(&count); err != nil || count != 1 {
		t.Fatalf("legacy QueryRow failed: count=%d, error=%v", count, err)
	}
}

type contextCall struct {
	ctx   context.Context
	query string
}

type recordingContextConnection struct {
	*sql.DB
	calls       []contextCall
	cancelChild context.CancelFunc
	dummy       bool
}

func (db *recordingContextConnection) record(ctx context.Context, query string) {
	db.calls = append(db.calls, contextCall{ctx: ctx, query: query})
}

func (db *recordingContextConnection) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	db.record(ctx, query)
	if db.dummy {
		return db.DB.ExecContext(ctx, "SELECT 1")
	}
	return db.DB.ExecContext(ctx, query, args...)
}

func (db *recordingContextConnection) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	db.record(ctx, query)
	if db.cancelChild != nil && strings.Contains(query, "context_children") {
		db.cancelChild()
	}
	if db.dummy {
		return db.DB.QueryContext(ctx, "SELECT 0")
	}
	return db.DB.QueryContext(ctx, query, args...)
}

func (db *recordingContextConnection) QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row {
	db.record(ctx, query)
	if db.dummy {
		return db.DB.QueryRowContext(ctx, "SELECT 0")
	}
	return db.DB.QueryRowContext(ctx, query, args...)
}

func TestContextPreloads(t *testing.T) {
	original := openContextDB(t)
	defer original.Close()
	recorder := &recordingContextConnection{DB: original.DB()}
	db, err := gorm.Open("sqlite3", recorder)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var parents []contextParent
	if err := db.WithContext(ctx).Preload("Children").Find(&parents).Error; err != nil {
		t.Fatal(err)
	}
	if len(parents) != 1 || len(parents[0].Children) != 1 || len(recorder.calls) != 2 {
		t.Fatalf("expected a parent query and a preload query, got parents=%+v, calls=%v", parents, recorder.calls)
	}
	for _, call := range recorder.calls {
		if call.ctx != ctx {
			t.Fatalf("query lost context: %s", call.query)
		}
	}
	recorder.cancelChild = cancel
	if err := db.WithContext(ctx).Preload("Children").Find(&parents).Error; err != context.Canceled {
		t.Fatalf("preload ignored cancellation: %v", err)
	}
}

func TestContextDialects(t *testing.T) {
	original := openContextDB(t)
	defer original.Close()
	for _, name := range []string{"common", "mysql", "postgres", "sqlite3", "mssql"} {
		t.Run(name, func(t *testing.T) {
			recorder := &recordingContextConnection{DB: original.DB(), dummy: true}
			db, err := gorm.Open(name, recorder)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			dialect := db.WithContext(ctx).Dialect()
			dialect.HasTable("context_parents")
			dialect.HasColumn("context_parents", "name")
			dialect.HasIndex("context_parents", "idx_parent")
			dialect.HasForeignKey("context_parents", "fk_parent")
			dialect.CurrentDatabase()
			if err := dialect.RemoveIndex("context_parents", "idx_parent"); err != nil {
				t.Fatal(err)
			}
			if err := dialect.ModifyColumn("context_parents", "name", "TEXT"); err != nil {
				t.Fatal(err)
			}
			if len(recorder.calls) == 0 {
				t.Fatal("dialect did not use the context methods")
			}
			for _, call := range recorder.calls {
				if call.ctx != ctx {
					t.Fatalf("dialect lost context: %s", call.query)
				}
			}
			cancel()
			if dialect.HasTable("context_parents") || dialect.HasColumn("context_parents", "name") || dialect.HasIndex("context_parents", "idx_parent") {
				t.Fatal("canceled schema query must return false")
			}
			if err := dialect.RemoveIndex("context_parents", "idx_parent"); err != context.Canceled {
				t.Fatalf("RemoveIndex ignored cancellation: %v", err)
			}
			if err := dialect.ModifyColumn("context_parents", "name", "TEXT"); err != context.Canceled {
				t.Fatalf("ModifyColumn ignored cancellation: %v", err)
			}
		})
	}
}
