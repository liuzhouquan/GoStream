package platform

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
	"gorm.io/gorm"
)

type Server struct {
	DB       *gorm.DB
	Secret   []byte
	MediaDir string
	Hub      *Hub
	upgrader websocket.Upgrader
}

func NewServer(db *gorm.DB, secret, mediaDir string) *Server {
	return &Server{DB: db, Secret: []byte(secret), MediaDir: mediaDir, Hub: NewHub(), upgrader: websocket.Upgrader{
		ReadBufferSize: 4096, WriteBufferSize: 4096,
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			if origin == "" {
				return true
			}
			u, err := url.Parse(origin)
			return err == nil && u.Host == r.Host
		},
	}}
}

func (s *Server) Router() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery(), cors())
	r.Static("/media", s.MediaDir)
	r.StaticFile("/", "web/index.html")
	r.StaticFile("/app.js", "web/app.js")
	r.StaticFile("/styles.css", "web/styles.css")
	r.GET("/health", func(c *gin.Context) { c.JSON(200, gin.H{"status": "ok"}) })
	api := r.Group("/api")
	api.POST("/users", s.register)
	api.POST("/sessions", s.login)
	api.POST("/uploads", s.auth(), s.upload)
	api.GET("/feed", s.optionalAuth(), s.feed)
	api.GET("/search", s.search)
	api.GET("/users/:id", s.getUser)
	api.GET("/users/:id/videos", s.userVideos)
	api.GET("/users/me", s.auth(), s.me)
	api.PATCH("/users/me", s.auth(), s.updateMe)
	api.PUT("/users/me/following/:id", s.auth(), s.follow)
	api.DELETE("/users/me/following/:id", s.auth(), s.unfollow)
	api.GET("/users/me/following", s.auth(), s.following)
	api.GET("/users/me/followers", s.auth(), s.followers)
	api.POST("/videos", s.auth(), s.createVideo)
	api.GET("/videos/:id", s.getVideo)
	api.DELETE("/videos/:id", s.auth(), s.deleteVideo)
	api.PUT("/videos/:id/like", s.auth(), s.action("like", true))
	api.DELETE("/videos/:id/like", s.auth(), s.action("like", false))
	api.PUT("/videos/:id/favorite", s.auth(), s.action("favorite", true))
	api.DELETE("/videos/:id/favorite", s.auth(), s.action("favorite", false))
	api.POST("/videos/:id/comments", s.auth(), s.createComment)
	api.GET("/videos/:id/comments", s.comments)
	api.POST("/videos/:id/views", s.recordView)
	api.GET("/notifications", s.auth(), s.notifications)
	api.GET("/notifications/unread", s.auth(), s.unread)
	api.PATCH("/notifications/read", s.auth(), s.markNotificationsRead)
	api.GET("/messages", s.auth(), s.history)
	api.POST("/messages/:id/ack", s.auth(), s.ackMessage)
	api.GET("/ws", s.websocket)
	return r
}

func cors() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.Request.Header.Get("Origin")
		if origin != "" {
			u, err := url.Parse(origin)
			if err == nil && (u.Host == c.Request.Host || u.Host == "localhost:5173" || u.Host == "127.0.0.1:5173") {
				c.Header("Access-Control-Allow-Origin", origin)
				c.Header("Vary", "Origin")
			}
		}
		c.Header("Access-Control-Allow-Headers", "Authorization, Content-Type")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	}
}
func respondError(c *gin.Context, status int, message string) {
	c.JSON(status, gin.H{"error": message})
}
func uid(c *gin.Context) uint { v, _ := c.Get("userID"); id, _ := v.(uint); return id }
func (s *Server) userFromToken(raw string) (uint, error) {
	token, err := jwt.Parse(raw, func(t *jwt.Token) (any, error) {
		if t.Method != jwt.SigningMethodHS256 {
			return nil, errors.New("unexpected signing method")
		}
		return s.Secret, nil
	})
	if err != nil || !token.Valid {
		return 0, errors.New("invalid token")
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return 0, errors.New("invalid token claims")
	}
	n, ok := claims["sub"].(float64)
	if !ok || n < 1 {
		return 0, errors.New("invalid token subject")
	}
	return uint(n), nil
}

func (s *Server) auth() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
		if h == "" {
			respondError(c, 401, "authentication required")
			c.Abort()
			return
		}
		id, err := s.userFromToken(h)
		if err != nil {
			respondError(c, 401, "invalid token")
			c.Abort()
			return
		}
		c.Set("userID", id)
		c.Next()
	}
}
func (s *Server) optionalAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.GetHeader("Authorization") == "" {
			c.Next()
			return
		}
		h := strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
		id, err := s.userFromToken(h)
		if err != nil {
			respondError(c, 401, "invalid token")
			c.Abort()
			return
		}
		c.Set("userID", id)
		c.Next()
	}
}
func (s *Server) issueToken(userID uint) (string, error) {
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": userID, "iat": time.Now().Unix(), "exp": time.Now().Add(24 * time.Hour).Unix()})
	return t.SignedString(s.Secret)
}
