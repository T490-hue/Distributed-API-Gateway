package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

const (
	ctxClientID   = "client_id"
	ctxClientTier = "client_tier"
	ctxRateLimit  = "rate_limit"
	ctxWindowSecs = "window_seconds"
)

type Handler struct {
	db           *sql.DB
	jwtSecret    string
	allowPremium bool // demo only: let signups pick their own tier
}

// allowPremium should be false in any real deployment; otherwise anyone can
// sign up for the 1000 req/min tier. Upgrade accounts out-of-band instead.
func NewHandler(db *sql.DB, secret string, allowPremium bool) *Handler {
	return &Handler{db: db, jwtSecret: secret, allowPremium: allowPremium}
}

// HashAPIKey hashes with SHA-256 (deterministic, fast for lookup).
// Passwords use bcrypt (slow, salted); API keys are already high-entropy
// random strings so bcrypt's extra slowness buys nothing here.
func HashAPIKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

func (h *Handler) Signup(c *gin.Context) {
	var req struct {
		Email    string `json:"email" binding:"required,email"`
		Password string `json:"password" binding:"required,min=8"`
		Tier     string `json:"tier"` // "free" | "premium"
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	if req.Tier != "premium" || !h.allowPremium {
		req.Tier = "free"
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not hash password"})
		return
	}
	apiKey := generateToken()
	apiKeyHash := HashAPIKey(apiKey)

	rateLimit, windowSecs := 100, 60
	if req.Tier == "premium" {
		rateLimit = 1000
	}

	var id string
	err = h.db.QueryRow(`
		INSERT INTO clients (email, password_hash, api_key_hash, tier, rate_limit, window_seconds)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		req.Email, string(hash), apiKeyHash, req.Tier, rateLimit, windowSecs,
	).Scan(&id)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "email already registered"})
		return
	}
	// Return plain key once only — never stored again
	c.JSON(http.StatusCreated, gin.H{"id": id, "api_key": apiKey, "tier": req.Tier})
}

func (h *Handler) Login(c *gin.Context) {
	var req struct {
		Email    string `json:"email" binding:"required"`
		Password string `json:"password" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var (
		id           string
		passwordHash string
		tier         string
		rateLimit    int
		windowSecs   int
	)
	err := h.db.QueryRow(`
		SELECT id, password_hash, tier, rate_limit, window_seconds
		FROM clients WHERE email=$1`, strings.ToLower(strings.TrimSpace(req.Email)),
	).Scan(&id, &passwordHash, &tier, &rateLimit, &windowSecs)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(req.Password)) != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}

	token, err := issueJWT(id, tier, rateLimit, windowSecs, h.jwtSecret)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "token error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"token": token})
}

func (h *Handler) Me(c *gin.Context) {
	id := c.GetString(ctxClientID)
	var email, tier string
	h.db.QueryRow(`SELECT email, tier FROM clients WHERE id=$1`, id).Scan(&email, &tier)
	c.JSON(http.StatusOK, gin.H{"id": id, "email": email, "tier": tier})
}

// Middleware validates JWT or API key on every request.
func Middleware(db *sql.DB, secret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		var clientID, tier string
		var rateLimit, windowSecs int
		var ok bool

		bearer := c.GetHeader("Authorization")
		apiKey := c.GetHeader("X-API-Key")

		switch {
		case strings.HasPrefix(bearer, "Bearer "):
			clientID, tier, rateLimit, windowSecs, ok = verifyJWT(strings.TrimPrefix(bearer, "Bearer "), secret)
		case apiKey != "":
			clientID, tier, rateLimit, windowSecs, ok = lookupAPIKey(db, apiKey)
		}

		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing or invalid credentials"})
			return
		}
		c.Set(ctxClientID, clientID)
		c.Set(ctxClientTier, tier)
		c.Set(ctxRateLimit, rateLimit)
		c.Set(ctxWindowSecs, windowSecs)
		c.Next()
	}
}

// --- helpers ---

func issueJWT(id, tier string, rateLimit, windowSecs int, secret string) (string, error) {
	claims := jwt.MapClaims{
		"sub":         id,
		"tier":        tier,
		"rate_limit":  rateLimit,
		"window_secs": windowSecs,
		"iss":         "api-gateway",
		"exp":         time.Now().Add(24 * time.Hour).Unix(),
		"iat":         time.Now().Unix(),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
}

func verifyJWT(tokenStr, secret string) (id, tier string, rateLimit, windowSecs int, ok bool) {
	t, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
		if _, valid := t.Method.(*jwt.SigningMethodHMAC); !valid {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return []byte(secret), nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer("api-gateway"))
	if err != nil || !t.Valid {
		return
	}
	claims, _ := t.Claims.(jwt.MapClaims)
	id, _ = claims["sub"].(string)
	tier, _ = claims["tier"].(string)
	if rl, ok2 := claims["rate_limit"].(float64); ok2 {
		rateLimit = int(rl)
	}
	if ws, ok2 := claims["window_secs"].(float64); ok2 {
		windowSecs = int(ws)
	}
	ok = id != ""
	return
}

func lookupAPIKey(db *sql.DB, key string) (id, tier string, rateLimit, windowSecs int, ok bool) {
	hash := HashAPIKey(key)
	err := db.QueryRow(`
		SELECT id, tier, rate_limit, window_seconds
		FROM clients WHERE api_key_hash=$1`, hash,
	).Scan(&id, &tier, &rateLimit, &windowSecs)
	ok = err == nil
	return
}

// generateToken returns a 256-bit API key from the OS CSPRNG.
func generateToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}
