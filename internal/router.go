package internal

import (
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
)

// SetupRouter creates the Gin engine with all routes and middleware.
// If accessLog is non-nil, HTTP requests are logged to it.
func SetupRouter(app *App, pm *ProxyManager, staticFS fs.FS, accessLog io.Writer) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	if accessLog != nil {
		r.Use(RequestLogger(accessLog))
	}
	r.Use(securityHeaders(), corsMiddleware())

	// Public auth routes
	r.POST("/api/auth/setup", app.handleSetup)
	r.POST("/api/auth/login", app.handleLogin)
	r.GET("/api/auth/check", app.handleAuthCheck)

	// Protected routes
	auth := r.Group("/api")
	auth.Use(app.authMiddleware())
	{
		auth.GET("/dashboard", app.handleDashboard)

		// Sites CRUD
		auth.GET("/sites", app.listSites)
		auth.POST("/sites", app.createSite)
		auth.PUT("/sites/:id", app.updateSite)
		auth.DELETE("/sites/:id", app.deleteSite)
		auth.POST("/sites/:id/toggle", app.toggleSite)
		auth.GET("/sites/:id/diag", app.diagSite)

		// Access logs
		auth.GET("/access_logs", app.handleAccessLogs)
		auth.GET("/access_logs/stats", app.handleAccessLogStats)

		// Relay node status (panel view)
		auth.GET("/relay/nodes", app.handleRelayNodes)
		auth.GET("/relay/install-cmd", app.handleRelayInstallCmd)
		auth.POST("/relay/nodes/update", app.handleRelayNodeUpdate)
		auth.DELETE("/relay/nodes/:name", app.handleRelayNodeDelete)

		// Panel self-update
		auth.GET("/admin/update/check", app.handleUpdateCheck)
		auth.POST("/admin/update", app.handleUpdateStart)
		auth.GET("/admin/settings", app.handleAdminSettings)

		// Account
		auth.POST("/auth/change-password", app.handleChangePassword)

		// Misc
		auth.GET("/ua-profiles", app.handleUAProfiles)
		auth.GET("/events", app.handleSSE)
	}

	// Relay API — authenticated by shared RELAY_TOKEN (not by user JWT)
	relay := r.Group("/api/relay")
	relay.Use(relayTokenMiddleware(app.RelayToken))
	{
		relay.GET("/sites", app.handleRelayGetSites)
		relay.POST("/traffic", app.handleRelayTraffic)
		relay.POST("/nodes/register", app.handleRelayRegister)
		relay.POST("/access_logs", app.handleRelayAccessLogs)
	}

	// Catch-all: proxy routes → embedded SPA
	static := StaticHandler(staticFS)
	r.NoRoute(func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/api/") {
			c.JSON(http.StatusNotFound, gin.H{"error": "API route not found"})
			return
		}
		// Try proxy routing with global route prefix stripping
		if pm != nil {
			served := false
			if app.RoutePrefix != "" {
				path := c.Request.URL.Path
				if path == app.RoutePrefix || strings.HasPrefix(path, app.RoutePrefix+"/") {
					r2 := c.Request.Clone(c.Request.Context())
					r2.URL.Path = strings.TrimPrefix(path, app.RoutePrefix)
					if r2.URL.Path == "" {
						r2.URL.Path = "/"
					}
					if c.Request.URL.RawPath != "" {
						r2.URL.RawPath = strings.TrimPrefix(c.Request.URL.RawPath, app.RoutePrefix)
						if r2.URL.RawPath == "" {
							r2.URL.RawPath = "/"
						}
					}
					served = pm.TryServe(c.Writer, r2)
				}
			} else {
				served = pm.TryServe(c.Writer, c.Request)
			}
			if served {
				return
			}
		}
		if staticFS != nil {
			static.ServeHTTP(c.Writer, c.Request)
		}
	})

	return r
}

// authMiddleware validates JWT bearer tokens.
func (a *App) authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		auth := c.GetHeader("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
			return
		}
		_, _, err := ValidateToken(strings.TrimPrefix(auth, "Bearer "))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "token expired or invalid"})
			return
		}
		c.Next()
	}
}

func securityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Content-Security-Policy", "default-src 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'; form-action 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Referrer-Policy", "no-referrer")
		c.Header("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		c.Header("Cross-Origin-Opener-Policy", "same-origin")
		c.Next()
	}
}

func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin != "" {
			parsed, err := url.Parse(origin)
			if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || !strings.EqualFold(parsed.Host, c.Request.Host) {
				c.AbortWithStatus(http.StatusForbidden)
				return
			}
			c.Header("Access-Control-Allow-Origin", origin)
			c.Header("Vary", "Origin")
		}
		c.Header("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type,Authorization")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}
