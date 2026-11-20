package bootstrap

import (
	"fmt"

	"github.com/gin-gonic/gin"
	"github.com/rafialfauzan/layrd/frontend"
	"github.com/rafialfauzan/layrd/internal/buildinfo"
	"github.com/rafialfauzan/layrd/pkg/core"
	"github.com/rafialfauzan/layrd/pkg/response"
)

func registerRoutes(app *core.App) error {
	app.Router.GET("/version", func(c *gin.Context) {
		response.OK(c, "Application version info", gin.H{
			"version":    buildinfo.Version,
			"build_time": buildinfo.BuildTime,
			"git_commit": buildinfo.GitCommit,
		})
	})

	// Enable WebSocket route if enabled in configuration
	if app.Config.WSEnabled {
		app.SetupWebSocketRoute(app.Config.WSPath)
	}

	// Enable Single Page Application (SPA) if enabled in configuration
	if app.Config.SPAEnabled {
		dist, err := frontend.Dist()
		if err != nil {
			return fmt.Errorf("open embedded frontend: %w", err)
		}
		if err := app.SetupSPAFS(dist); err != nil {
			return fmt.Errorf("setup embedded frontend: %w", err)
		}
	}

	return nil
}
