package core

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/rafialfauzan/layrd/pkg/config"
	"github.com/rafialfauzan/layrd/pkg/database"
	"github.com/rafialfauzan/layrd/pkg/middleware"
	"github.com/rafialfauzan/layrd/pkg/response"
	"github.com/rafialfauzan/layrd/pkg/utils"
	"github.com/rafialfauzan/layrd/pkg/websocket"

	"github.com/gin-gonic/gin"
)

// App is the central framework application container
type App struct {
	Config       *config.Config
	Router       *gin.Engine
	APIGroup     *gin.RouterGroup
	DBManager    *database.DBManager
	WSHub        *websocket.Hub
	TokenService *utils.TokenService
	modules      []Module
	httpServer   *http.Server
}

// NewApp creates and configures a new framework application
func NewApp(cfg *config.Config) *App {
	gin.SetMode(cfg.GinMode)
	router := gin.New()

	isDebug := cfg.GinMode == gin.DebugMode

	// Global Middlewares
	router.Use(gin.Logger())
	router.Use(middleware.Recovery(isDebug))
	router.Use(middleware.RequestID())
	router.Use(middleware.CORS(cfg.CORSAllowedOrigins, isDebug))
	if cfg.RateLimitRPS > 0 {
		router.Use(middleware.RateLimit(cfg.RateLimitRPS, cfg.RateLimitBurst))
	}

	// Root health check endpoint
	router.GET("/health", func(c *gin.Context) {
		response.OK(c, "Service is healthy", gin.H{
			"app_name": cfg.AppName,
			"env":      cfg.AppEnv,
			"time":     time.Now().Format(time.RFC3339),
		})
	})

	// Default 404 handler for pure API mode
	router.NoRoute(func(c *gin.Context) {
		response.NotFound(c, fmt.Sprintf("Resource [%s %s] not found", c.Request.Method, c.Request.URL.Path))
	})

	apiGroup := router.Group(cfg.APIPrefix)

	var wsHub *websocket.Hub
	if cfg.WSEnabled {
		wsHub = websocket.NewHub()
	}

	return &App{
		Config:       cfg,
		Router:       router,
		APIGroup:     apiGroup,
		DBManager:    database.NewDBManager(),
		WSHub:        wsHub,
		TokenService: utils.NewTokenService(cfg.JWTSecret, cfg.JWTExpirationHours),
		modules:      make([]Module, 0),
	}
}

// InitDatabases connects to all databases configured in Config
func (a *App) InitDatabases() error {
	isDebug := a.Config.GinMode == gin.DebugMode

	for name, dbCfg := range a.Config.Databases {
		_, err := a.DBManager.Connect(dbCfg, isDebug)
		if err != nil {
			if dbCfg.Optional {
				log.Printf("[DBManager] Warning: optional database [%s] connection failed: %v (skipping)\n", name, err)
				continue
			}
			return fmt.Errorf("failed to connect to db [%s]: %w", name, err)
		}
	}
	return nil
}

// RegisterModule registers one or more pluggable functional modules
func (a *App) RegisterModule(modules ...Module) error {
	for _, mod := range modules {
		if mod == nil {
			return fmt.Errorf("cannot register a nil module")
		}

		log.Printf("[Framework] Registering module: %s\n", mod.Name())

		if initMod, ok := mod.(Initializer); ok {
			if err := initMod.Init(a); err != nil {
				return fmt.Errorf("initialize module [%s]: %w", mod.Name(), err)
			}
		}

		mod.RegisterRoutes(a.APIGroup)
		a.modules = append(a.modules, mod)
	}
	return nil
}

func (a *App) MigrateModules() error {
	for _, mod := range a.modules {
		if migMod, ok := mod.(Migrator); ok {
			if err := migMod.AutoMigrate(a); err != nil {
				return fmt.Errorf("migrate module [%s]: %w", mod.Name(), err)
			}
		}
	}
	return nil
}

// SetupWebSocketRoute enables real-time WebSocket connection endpoint
func (a *App) SetupWebSocketRoute(path string) {
	if !a.Config.WSEnabled {
		log.Println("[WebSocket] Feature is disabled by configuration (WS_ENABLED=false)")
		return
	}

	if a.WSHub == nil {
		a.WSHub = websocket.NewHub()
	}

	a.Router.GET(path, func(c *gin.Context) {
		room := c.Query("room")
		if room == "" {
			room = "global"
		}
		websocket.ServeWebSocket(a.WSHub, c, room)
	})
	log.Printf("[Framework] WebSocket endpoint enabled at %s?room=:room\n", path)
}

// SetupSPA configures the application to serve a Single Page Application (e.g. Vue/React)
// from the designated dist directory, with client-side HTML5 history fallback.
func (a *App) SetupSPA(distPath string) {
	cleanPath := filepath.Clean(distPath)
	indexPath := filepath.Join(cleanPath, "index.html")

	assetsPath := filepath.Join(cleanPath, "assets")
	if stat, err := os.Stat(assetsPath); err == nil && stat.IsDir() {
		a.Router.Static("/assets", assetsPath)
	}

	a.Router.NoRoute(func(c *gin.Context) {
		reqPath := c.Request.URL.Path

		// Never fallback API, WebSocket, or Storage paths to index.html
		if strings.HasPrefix(reqPath, a.Config.APIPrefix) ||
			strings.HasPrefix(reqPath, a.Config.StorageURL) ||
			strings.HasPrefix(reqPath, "/ws") {
			response.NotFound(c, fmt.Sprintf("API endpoint [%s %s] not found", c.Request.Method, reqPath))
			return
		}

		// Try serving specific static root files if requested (e.g. favicon.ico, manifest.json)
		targetFile := filepath.Join(cleanPath, filepath.Clean(reqPath))
		if stat, err := os.Stat(targetFile); err == nil && !stat.IsDir() {
			c.File(targetFile)
			return
		}

		// Client-side HTML5 History Mode Fallback
		if _, err := os.Stat(indexPath); err == nil {
			c.File(indexPath)
			return
		}

		response.NotFound(c, "SPA frontend build not found. Run 'npm run build' inside frontend/ directory.")
	})

	log.Printf("[SPA] Serving Single Page Application from %s\n", cleanPath)
}

// SetupSPAFS serves a Single Page Application from an embedded filesystem.
func (a *App) SetupSPAFS(dist fs.FS) error {
	indexHTML, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		return fmt.Errorf("read SPA index.html: %w", err)
	}

	distHTTPFS := http.FS(dist)
	if assetsFS, err := fs.Sub(dist, "assets"); err == nil {
		a.Router.StaticFS("/assets", http.FS(assetsFS))
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("open embedded SPA assets: %w", err)
	}

	a.Router.NoRoute(func(c *gin.Context) {
		reqPath := c.Request.URL.Path

		if strings.HasPrefix(reqPath, a.Config.APIPrefix) ||
			strings.HasPrefix(reqPath, a.Config.StorageURL) ||
			strings.HasPrefix(reqPath, "/ws") {
			response.NotFound(c, fmt.Sprintf("API endpoint [%s %s] not found", c.Request.Method, reqPath))
			return
		}

		filePath := strings.TrimPrefix(path.Clean("/"+reqPath), "/")
		if filePath != "." && fs.ValidPath(filePath) {
			if fileInfo, err := fs.Stat(dist, filePath); err == nil && !fileInfo.IsDir() {
				c.FileFromFS(filePath, distHTTPFS)
				return
			}
		}

		c.Data(http.StatusOK, "text/html; charset=utf-8", indexHTML)
	})

	log.Println("[SPA] Serving Single Page Application embedded in the binary")
	return nil
}

// Run starts the HTTP server with graceful shutdown handling
func (a *App) Run() error {
	defer a.DBManager.CloseAll()

	addr := fmt.Sprintf(":%s", a.Config.AppPort)

	a.httpServer = &http.Server{
		Addr:         addr,
		Handler:      a.Router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Server run context
	serverErrors := make(chan error, 1)

	go func() {
		log.Printf("==================================================")
		log.Printf(" 🚀 %s is running on port %s (%s mode)", a.Config.AppName, a.Config.AppPort, a.Config.AppEnv)
		log.Printf(" API Base URL: http://localhost:%s%s", a.Config.AppPort, a.Config.APIPrefix)
		log.Printf(" Health Check: http://localhost:%s/health", a.Config.AppPort)
		log.Printf("==================================================")

		serverErrors <- a.httpServer.ListenAndServe()
	}()

	// Listen for OS interrupt signals
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(shutdown)

	select {
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("server error: %w", err)
		}
	case sig := <-shutdown:
		log.Printf("[Framework] Received shutdown signal: %v. Initiating graceful shutdown...", sig)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		// Stop HTTP server
		if err := a.httpServer.Shutdown(ctx); err != nil {
			a.httpServer.Close()
			return fmt.Errorf("could not stop server gracefully: %w", err)
		}

		log.Println("[Framework] Graceful shutdown completed cleanly.")
	}

	return nil
}
