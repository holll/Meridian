package internal

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// POST /api/auth/setup
func (a *App) handleSetup(c *gin.Context) {
	userCount, err := a.DB.UserCount()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "setup status unavailable"})
		return
	}
	if userCount > 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "admin user already exists"})
		return
	}
	var req struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		SetupToken string `json:"setup_token"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || len(req.Username) > 64 || len(req.Password) < 8 || len(req.Password) > 72 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "username must be 1-64 characters and password must be 8-72 bytes"})
		return
	}
	if a.SetupToken != "" && !SetupTokenMatches(a.SetupToken, req.SetupToken) {
		c.JSON(http.StatusForbidden, gin.H{"error": "invalid setup token"})
		return
	}
	id, err := a.DB.CreateInitialUser(req.Username, req.Password)
	if err != nil {
		if errors.Is(err, errAdminAlreadyExists) {
			c.JSON(http.StatusConflict, gin.H{"error": errAdminAlreadyExists.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to create admin user"})
		return
	}
	token, err := GenerateToken(id, req.Username)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"token": token, "username": req.Username})
}

// POST /api/auth/login
func (a *App) handleLogin(c *gin.Context) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	client := requestClientKey(c.Request, a.TrustedProxies)
	if allowed, retryAfter := a.limiter().allow(client, time.Now()); !allowed {
		c.Header("Retry-After", strconv.Itoa(max(1, int(retryAfter.Seconds()+0.5))))
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many login attempts; try again later"})
		return
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		a.limiter().recordFailure(client, time.Now())
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	username := strings.TrimSpace(req.Username)
	if username == "" || len(username) > 64 || req.Password == "" || len(req.Password) > 72 {
		a.limiter().recordFailure(client, time.Now())
		c.JSON(http.StatusUnauthorized, gin.H{"error": errInvalidCredentials.Error()})
		return
	}
	id, err := a.DB.VerifyUser(username, req.Password)
	if err != nil {
		a.limiter().recordFailure(client, time.Now())
		if errors.Is(err, errInvalidCredentials) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": errInvalidCredentials.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "authentication unavailable"})
		return
	}
	a.limiter().reset(client)
	token, err := GenerateToken(id, username)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"token": token, "username": username})
}

// handleChangePassword updates the administrator password after verifying
// the current one. POST /api/auth/change-password (panel JWT)
func (a *App) handleChangePassword(c *gin.Context) {
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	if _, err := a.DB.VerifyUser("admin", req.OldPassword); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "当前密码错误"})
		return
	}
	if err := a.DB.ResetAdminPassword(req.NewPassword); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// GET /api/auth/check
func (a *App) handleAuthCheck(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	userCount, err := a.DB.UserCount()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "setup status unavailable"})
		return
	}
	needsSetup := userCount == 0
	c.JSON(http.StatusOK, gin.H{
		"needs_setup":          needsSetup,
		"mode":                 "single_admin",
		"jwt_secret_ephemeral": JWTSecretEphemeral,
		"setup_token_required": needsSetup && a.SetupToken != "",
		"route_prefix":         a.RoutePrefix,
	})
}
