package configsync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mcpjungle/mcpjungle/internal/migrations"
	"github.com/mcpjungle/mcpjungle/internal/model"
	"github.com/mcpjungle/mcpjungle/pkg/types"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open db: %v", err)
	}
	if err := migrations.Migrate(db); err != nil {
		t.Fatalf("failed to migrate db: %v", err)
	}
	return db
}

func TestReconcileUsers_AdminUserIsRejected(t *testing.T) {
	db := newTestDB(t)
	tmp := t.TempDir()
	if err := ensureSubDirs(tmp); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	if err := db.Create(&model.User{Username: "admin", Role: types.UserRoleAdmin, AccessToken: "mcpjungle_test_admin"}).Error; err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	cfg := `{"name":"admin","access_token":"mcpjungle_test_replacement"}`
	if err := os.WriteFile(filepath.Join(tmp, "users", "admin.json"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	s := &Service{services: Services{DB: db}, opts: Options{Dir: tmp}}
	err := s.reconcileUsers()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "cannot manage admin user") {
		t.Fatalf("expected admin restriction error, got: %v", err)
	}

	var tracked []model.ManagedConfigFile
	if err := db.Where("entity_type = ?", model.EntityTypeUser).Find(&tracked).Error; err != nil {
		t.Fatalf("query tracked: %v", err)
	}
	if len(tracked) != 0 {
		t.Fatalf("expected no tracking rows for admin user, got %d", len(tracked))
	}
}

func TestReconcileUsers_AdoptsExistingManualUser(t *testing.T) {
	db := newTestDB(t)
	tmp := t.TempDir()
	if err := ensureSubDirs(tmp); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	if err := db.Create(&model.User{Username: "alice", Role: types.UserRoleUser, AccessToken: "mcpjungle_test_oldtoken"}).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	cfg := `{"name":"alice","access_token":"mcpjungle_test_newtoken"}`
	if err := os.WriteFile(filepath.Join(tmp, "users", "alice.json"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	s := &Service{services: Services{DB: db}, opts: Options{Dir: tmp}}
	if err := s.reconcileUsers(); err != nil {
		t.Fatalf("reconcile users: %v", err)
	}

	var updated model.User
	if err := db.Where("username = ?", "alice").First(&updated).Error; err != nil {
		t.Fatalf("fetch user: %v", err)
	}
	if updated.AccessToken != "mcpjungle_test_newtoken" {
		t.Fatalf("expected token update, got %s", updated.AccessToken)
	}

	var tracked model.ManagedConfigFile
	if err := db.Where("entity_type = ? AND entity_name = ?", model.EntityTypeUser, "alice").First(&tracked).Error; err != nil {
		t.Fatalf("fetch tracking row: %v", err)
	}
	if tracked.FilePath == "" || tracked.FileHash == "" {
		t.Fatalf("expected tracking metadata to be set")
	}
}
