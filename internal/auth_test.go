package internal

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestGenerateTokenPreservesSpecialCharacters(t *testing.T) {
	JWTSecret = []byte("test-secret")

	token, err := GenerateToken(7, `bad"name\user`)
	if err != nil {
		t.Fatalf("generateToken error: %v", err)
	}

	userID, username, err := ValidateToken(token)
	if err != nil {
		t.Fatalf("validateToken error: %v", err)
	}

	if userID != 7 {
		t.Fatalf("userID = %d, want 7", userID)
	}
	if username != `bad"name\user` {
		t.Fatalf("username = %q", username)
	}
}

func TestResolveJWTSecretGeneratesRandomFallback(t *testing.T) {
	secretA, ephemeralA, err := ResolveJWTSecret("")
	if err != nil {
		t.Fatalf("resolveJWTSecret A: %v", err)
	}
	secretB, ephemeralB, err := ResolveJWTSecret("")
	if err != nil {
		t.Fatalf("resolveJWTSecret B: %v", err)
	}

	if !ephemeralA || !ephemeralB {
		t.Fatalf("expected ephemeral fallback secrets")
	}
	if len(secretA) == 0 || len(secretB) == 0 {
		t.Fatalf("expected non-empty secrets")
	}
	if bytes.Equal(secretA, secretB) {
		t.Fatalf("expected random fallback secrets to differ")
	}
}

func TestResolveJWTSecretRequiresSufficientEntropy(t *testing.T) {
	if _, _, err := ResolveJWTSecret("too-short"); err == nil {
		t.Fatal("short JWT_SECRET unexpectedly accepted")
	}
	configured := strings.Repeat("x", 32)
	secret, ephemeral, err := ResolveJWTSecret(configured)
	if err != nil {
		t.Fatalf("resolveJWTSecret configured value: %v", err)
	}
	if ephemeral || string(secret) != configured {
		t.Fatalf("configured JWT secret not preserved")
	}
}

func TestSecurityHeaders(t *testing.T) {
	app := newTestApp(t)
	router := setupTestRouter(app)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/auth/check", nil))

	if got := w.Header().Get("Content-Security-Policy"); !strings.Contains(got, "script-src 'self'") || !strings.Contains(got, "frame-ancestors 'none'") {
		t.Fatalf("unexpected Content-Security-Policy: %q", got)
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := w.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Fatalf("X-Frame-Options = %q, want DENY", got)
	}
}

func TestHandleAuthCheckExposesSingleAdminModeBeforeSetup(t *testing.T) {
	app := newTestApp(t)
	JWTSecretEphemeral = true
	t.Cleanup(func() { JWTSecretEphemeral = false })

	router := setupTestRouter(app)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/auth/check", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}

	body := decodeBody(t, w)
	if got := mustBoolValue(t, body, "needs_setup"); !got {
		t.Fatalf("needs_setup = %v, want true", got)
	}
	if got := mustStringValue(t, body, "mode"); got != "single_admin" {
		t.Fatalf("mode = %q, want single_admin", got)
	}
	if got := mustBoolValue(t, body, "jwt_secret_ephemeral"); !got {
		t.Fatalf("jwt_secret_ephemeral = %v, want true", got)
	}
}

func TestSetupRequiresTokenAndCreatesOnlyOneAdmin(t *testing.T) {
	app := newTestApp(t)
	app.SetupToken = "one-time-setup-token"
	router := setupTestRouter(app)

	// Wrong token
	wrongReq := httptest.NewRequest(http.MethodPost, "/api/auth/setup", strings.NewReader(`{
		"username":"admin","password":"correct horse battery staple","setup_token":"wrong"
	}`))
	wrongReq.Header.Set("Content-Type", "application/json")
	wrong := httptest.NewRecorder()
	router.ServeHTTP(wrong, wrongReq)
	if wrong.Code != http.StatusForbidden {
		t.Fatalf("wrong setup token status = %d, want 403", wrong.Code)
	}
	if got := mustUserCount(t, app.DB); got != 0 {
		t.Fatalf("user count after rejected setup = %d, want 0", got)
	}

	// Correct token
	okReq := httptest.NewRequest(http.MethodPost, "/api/auth/setup", strings.NewReader(`{
		"username":"admin","password":"correct horse battery staple","setup_token":"one-time-setup-token"
	}`))
	okReq.Header.Set("Content-Type", "application/json")
	ok := httptest.NewRecorder()
	router.ServeHTTP(ok, okReq)
	if ok.Code != http.StatusOK {
		t.Fatalf("valid setup status = %d body=%s", ok.Code, ok.Body.String())
	}
	if got := mustUserCount(t, app.DB); got != 1 {
		t.Fatalf("user count after setup = %d, want 1", got)
	}
}

func TestCreateInitialUserIsAtomic(t *testing.T) {
	app := newTestApp(t)
	const contenders = 4
	var wg sync.WaitGroup
	results := make(chan error, contenders)
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := app.DB.CreateInitialUser(fmt.Sprintf("admin-%d", i), "correct horse battery staple")
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)

	created := 0
	alreadyExists := 0
	for err := range results {
		switch {
		case err == nil:
			created++
		case errors.Is(err, errAdminAlreadyExists):
			alreadyExists++
		default:
			t.Fatalf("unexpected setup error: %v", err)
		}
	}
	if created != 1 || alreadyExists != contenders-1 {
		t.Fatalf("created=%d alreadyExists=%d, want 1 and %d", created, alreadyExists, contenders-1)
	}
	if got := mustUserCount(t, app.DB); got != 1 {
		t.Fatalf("user count = %d, want 1", got)
	}
}

func TestVerifyUserAcceptsExistingXCryptoBcryptHash(t *testing.T) {
	app := newTestApp(t)
	// Compatibility vector generated by golang.org/x/crypto/bcrypt. Existing
	// installations must continue to authenticate after switching providers.
	const legacyHash = "$2a$10$XajjQvNhvvRt5GSeFk1xFeyqRrsxkhBkUiQeg0dt.wU1qD4aFDcga"
	result, err := app.DB.DB.Exec(
		"INSERT INTO users (username, password_hash) VALUES (?, ?)",
		"legacy-admin",
		legacyHash,
	)
	if err != nil {
		t.Fatalf("insert legacy user: %v", err)
	}
	wantID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("legacy user id: %v", err)
	}

	gotID, err := app.DB.VerifyUser("legacy-admin", "allmine")
	if err != nil {
		t.Fatalf("VerifyUser rejected a legacy bcrypt hash: %v", err)
	}
	if gotID != wantID {
		t.Fatalf("VerifyUser id = %d, want %d", gotID, wantID)
	}
	if _, err := app.DB.VerifyUser("legacy-admin", "not-the-password"); !errors.Is(err, errInvalidCredentials) {
		t.Fatalf("wrong password error = %v, want invalid credentials", err)
	}
}

func TestJWTSecretRotationInvalidatesExistingToken(t *testing.T) {
	originalSecret := JWTSecret
	originalEphemeral := JWTSecretEphemeral
	t.Cleanup(func() {
		JWTSecret = originalSecret
		JWTSecretEphemeral = originalEphemeral
	})

	JWTSecret = []byte("old-test-signing-secret-000000000000")
	token, err := GenerateToken(1, "admin")
	if err != nil {
		t.Fatalf("generateToken: %v", err)
	}
	JWTSecret = []byte("new-test-signing-secret-000000000000")
	if _, _, err := ValidateToken(token); err == nil {
		t.Fatal("token signed before JWT secret rotation remained valid")
	}
}

func TestLoginUsesGenericErrorsAndRateLimit(t *testing.T) {
	app := newTestApp(t)
	if _, err := app.DB.CreateInitialUser("admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateInitialUser: %v", err)
	}
	router := setupTestRouter(app)

	login := func(username, password string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(fmt.Sprintf(
			`{"username":%q,"password":%q}`, username, password,
		)))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "203.0.113.10:12345"
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr
	}

	unknown := login("missing", "wrong password")
	badPassword := login("admin", "wrong password")
	if unknown.Code != http.StatusUnauthorized || badPassword.Code != http.StatusUnauthorized {
		t.Fatalf("credential failure statuses = %d, %d; want 401", unknown.Code, badPassword.Code)
	}
	if unknown.Body.String() != badPassword.Body.String() {
		t.Fatalf("credential failure responses differ: %q vs %q", unknown.Body.String(), badPassword.Body.String())
	}

	for i := 0; i < maxLoginFailures-2; i++ {
		login("admin", "wrong password")
	}
	blocked := login("admin", "correct horse battery staple")
	if blocked.Code != http.StatusTooManyRequests {
		t.Fatalf("blocked login status = %d, want 429", blocked.Code)
	}
	if blocked.Header().Get("Retry-After") == "" {
		t.Fatal("blocked login is missing Retry-After")
	}
}

func TestCORSAllowsSameOriginAndRejectsCrossOrigin(t *testing.T) {
	app := newTestApp(t)
	router := setupTestRouter(app)

	sameReq := httptest.NewRequest(http.MethodGet, "/api/auth/check", nil)
	sameReq.Host = "panel.example"
	sameReq.Header.Set("Origin", "http://panel.example")
	same := httptest.NewRecorder()
	router.ServeHTTP(same, sameReq)
	if same.Code != http.StatusOK || same.Header().Get("Access-Control-Allow-Origin") != "http://panel.example" {
		t.Fatalf("same-origin request status=%d allow-origin=%q", same.Code, same.Header().Get("Access-Control-Allow-Origin"))
	}

	crossReq := httptest.NewRequest(http.MethodGet, "/api/auth/check", nil)
	crossReq.Host = "panel.example"
	crossReq.Header.Set("Origin", "https://evil.example")
	cross := httptest.NewRecorder()
	router.ServeHTTP(cross, crossReq)
	if cross.Code != http.StatusForbidden {
		t.Fatalf("cross-origin request status = %d, want 403", cross.Code)
	}
}

func TestHandleAuthCheckExposesConfiguredSingleAdminMode(t *testing.T) {
	app := newTestApp(t)
	originalEphemeral := JWTSecretEphemeral
	JWTSecretEphemeral = false
	t.Cleanup(func() { JWTSecretEphemeral = originalEphemeral })

	if _, err := app.DB.CreateUser("admin", "admin123"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	router := setupTestRouter(app)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/auth/check", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}

	body := decodeBody(t, w)
	if got := mustBoolValue(t, body, "needs_setup"); got {
		t.Fatalf("needs_setup = %v, want false", got)
	}
	if got := mustStringValue(t, body, "mode"); got != "single_admin" {
		t.Fatalf("mode = %q, want single_admin", got)
	}
	if got := mustBoolValue(t, body, "jwt_secret_ephemeral"); got {
		t.Fatalf("jwt_secret_ephemeral = %v, want false", got)
	}
}

func TestCleanDatabaseInitializationAPIFlow(t *testing.T) {
	app := newTestApp(t)
	app.SetupToken = "clean-database-setup-token"
	router := SetupRouter(app, app.PM, nil, nil)

	check := httptest.NewRecorder()
	router.ServeHTTP(check, httptest.NewRequest(http.MethodGet, "/api/auth/check", nil))
	if check.Code != http.StatusOK || !mustBoolValue(t, decodeBody(t, check), "needs_setup") {
		t.Fatalf("initial auth check = status %d body=%s", check.Code, check.Body.String())
	}

	setupReq := httptest.NewRequest(http.MethodPost, "/api/auth/setup", strings.NewReader("{\"username\":\"admin\",\"password\":\"correct horse battery staple\",\"setup_token\":\"clean-database-setup-token\"}"))
	setupReq.Header.Set("Content-Type", "application/json")
	setup := httptest.NewRecorder()
	router.ServeHTTP(setup, setupReq)
	if setup.Code != http.StatusOK {
		t.Fatalf("setup status=%d body=%s", setup.Code, setup.Body.String())
	}

	loginReq := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader("{\"username\":\"admin\",\"password\":\"correct horse battery staple\"}"))
	loginReq.Header.Set("Content-Type", "application/json")
	login := httptest.NewRecorder()
	router.ServeHTTP(login, loginReq)
	if login.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", login.Code, login.Body.String())
	}
	token := mustStringValue(t, decodeBody(t, login), "token")

	secondSetupReq := httptest.NewRequest(http.MethodPost, "/api/auth/setup", strings.NewReader("{\"username\":\"other\",\"password\":\"correct horse battery staple\",\"setup_token\":\"clean-database-setup-token\"}"))
	secondSetupReq.Header.Set("Content-Type", "application/json")
	secondSetup := httptest.NewRecorder()
	router.ServeHTTP(secondSetup, secondSetupReq)
	if secondSetup.Code != http.StatusBadRequest {
		t.Fatalf("second setup status=%d body=%s", secondSetup.Code, secondSetup.Body.String())
	}

	sitesRequest := httptest.NewRequest(http.MethodGet, "/api/sites", nil)
	sitesRequest.Header.Set("Authorization", "Bearer "+token)
	sites := httptest.NewRecorder()
	router.ServeHTTP(sites, sitesRequest)
	if sites.Code != http.StatusOK {
		t.Fatalf("authenticated sites status=%d body=%s", sites.Code, sites.Body.String())
	}
}
