package core

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/rafialfauzan/layrd/pkg/config"
)

type migrationTestModule struct {
	migrated bool
}

func (*migrationTestModule) Name() string                    { return "migration-test" }
func (*migrationTestModule) RegisterRoutes(*gin.RouterGroup) {}
func (m *migrationTestModule) AutoMigrate(*App) error {
	m.migrated = true
	return nil
}

func TestModuleMigrationsAreExplicit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := NewApp(&config.Config{GinMode: gin.TestMode, APIPrefix: "/api"})
	module := &migrationTestModule{}

	if err := app.RegisterModule(module); err != nil {
		t.Fatalf("RegisterModule() error = %v", err)
	}
	if module.migrated {
		t.Fatal("module migrated during registration")
	}
	if err := app.MigrateModules(); err != nil {
		t.Fatalf("MigrateModules() error = %v", err)
	}
	if !module.migrated {
		t.Fatal("MigrateModules() did not invoke the module migrator")
	}
}

type failingInitializerModule struct {
	err error
}

func (*failingInitializerModule) Name() string                    { return "failing-initializer" }
func (*failingInitializerModule) RegisterRoutes(*gin.RouterGroup) {}
func (m *failingInitializerModule) Init(*App) error               { return m.err }

func TestRegisterModuleReturnsInitializerError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := NewApp(&config.Config{GinMode: gin.TestMode, APIPrefix: "/api"})
	wantErr := errors.New("initialization failed")

	err := app.RegisterModule(&failingInitializerModule{err: wantErr})
	if !errors.Is(err, wantErr) {
		t.Fatalf("RegisterModule() error = %v, want %v", err, wantErr)
	}
}

func TestInitDatabasesSkipsOptionalDatabaseOnFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := NewApp(&config.Config{
		GinMode: gin.TestMode,
		Databases: map[string]config.DatabaseConfig{
			"optional_fail": {
				Name:     "optional_fail",
				Driver:   "unsupported_driver",
				Optional: true,
			},
		},
	})

	err := app.InitDatabases()
	if err != nil {
		t.Fatalf("InitDatabases() should not fail when an optional database fails to connect, got: %v", err)
	}
	if app.DBManager.Has("optional_fail") {
		t.Fatal("DBManager should not register optional_fail when connection fails")
	}
}

func TestPureAPIReturnsJSON404(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := NewApp(&config.Config{
		GinMode:    gin.TestMode,
		APIPrefix:  "/api/v1",
		SPAEnabled: false,
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/unknown-page", nil)
	app.Router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"code":404`) {
		t.Fatalf("expected JSON 404 response, got %s", w.Body.String())
	}
}

func TestSPAFallbackToIndexHTML(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Create a temporary mock dist directory
	tempDir, err := os.MkdirTemp("", "spa-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	indexContent := "<html><body>Vue App Mock</body></html>"
	if err := os.WriteFile(filepath.Join(tempDir, "index.html"), []byte(indexContent), 0644); err != nil {
		t.Fatalf("failed to write index.html: %v", err)
	}

	app := NewApp(&config.Config{
		GinMode:     gin.TestMode,
		APIPrefix:   "/api/v1",
		StorageURL:  "/storage",
		SPAEnabled:  true,
		SPADistPath: tempDir,
	})
	app.SetupSPA(tempDir)

	// Web page route should fallback to index.html
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/dashboard/users", nil)
	app.Router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for SPA fallback, got %d", w.Code)
	}
	if w.Body.String() != indexContent {
		t.Fatalf("expected index.html body, got %s", w.Body.String())
	}

	// API route should NOT fallback to index.html
	wAPI := httptest.NewRecorder()
	reqAPI, _ := http.NewRequest(http.MethodGet, "/api/v1/non-existent", nil)
	app.Router.ServeHTTP(wAPI, reqAPI)

	if wAPI.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown API route, got %d", wAPI.Code)
	}
	if strings.Contains(wAPI.Body.String(), "Vue App Mock") {
		t.Fatal("API route unexpectedly fell back to index.html")
	}
}

func TestWebSocketCanBeDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := NewApp(&config.Config{
		GinMode:   gin.TestMode,
		WSEnabled: false,
		WSPath:    "/ws",
	})
	app.SetupWebSocketRoute("/ws")

	if app.WSHub != nil {
		t.Fatal("WSHub should be nil when WSEnabled is false")
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/ws", nil)
	app.Router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for disabled /ws route, got %d", w.Code)
	}
}
