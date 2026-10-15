package platform

import (
	"strings"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

func (s *Server) register(c *gin.Context) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Nickname string `json:"nickname"`
	}
	if c.ShouldBindJSON(&in) != nil {
		respondError(c, 400, "invalid JSON")
		return
	}
	in.Username = strings.ToLower(strings.TrimSpace(in.Username))
	in.Nickname = strings.TrimSpace(in.Nickname)
	if len(in.Username) < 3 || len(in.Username) > 32 || len(in.Password) < 8 || len(in.Password) > 72 {
		respondError(c, 400, "username must be 3-32 characters and password 8-72 characters")
		return
	}
	if in.Nickname == "" {
		in.Nickname = in.Username
	}
	if len(in.Nickname) > 48 {
		respondError(c, 400, "nickname is too long")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		respondError(c, 500, "could not create account")
		return
	}
	u := User{Username: in.Username, Nickname: in.Nickname, PasswordHash: string(hash)}
	if err = s.DB.Create(&u).Error; err != nil {
		respondError(c, 409, "username already exists")
		return
	}
	token, err := s.issueToken(u.ID)
	if err != nil {
		respondError(c, 500, "could not create session")
		return
	}
	c.JSON(201, gin.H{"user": u, "access_token": token})
}

func (s *Server) login(c *gin.Context) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if c.ShouldBindJSON(&in) != nil {
		respondError(c, 400, "invalid JSON")
		return
	}
	var u User
	if s.DB.Where("username = ?", strings.ToLower(strings.TrimSpace(in.Username))).First(&u).Error != nil || bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(in.Password)) != nil {
		respondError(c, 401, "invalid username or password")
		return
	}
	token, err := s.issueToken(u.ID)
	if err != nil {
		respondError(c, 500, "could not create session")
		return
	}
	c.JSON(200, gin.H{"user": u, "access_token": token})
}

func (s *Server) me(c *gin.Context) {
	var u User
	if s.DB.First(&u, uid(c)).Error != nil {
		respondError(c, 404, "user not found")
		return
	}
	c.JSON(200, u)
}

func (s *Server) updateMe(c *gin.Context) {
	var in struct {
		Nickname  *string `json:"nickname"`
		Bio       *string `json:"bio"`
		AvatarURL *string `json:"avatar_url"`
	}
	if c.ShouldBindJSON(&in) != nil {
		respondError(c, 400, "invalid JSON")
		return
	}
	updates := map[string]any{}
	if in.Nickname != nil {
		v := strings.TrimSpace(*in.Nickname)
		if len(v) > 48 || v == "" {
			respondError(c, 400, "invalid nickname")
			return
		}
		updates["nickname"] = v
	}
	if in.Bio != nil {
		if len(*in.Bio) > 280 {
			respondError(c, 400, "bio is too long")
			return
		}
		updates["bio"] = strings.TrimSpace(*in.Bio)
	}
	if in.AvatarURL != nil {
		updates["avatar_url"] = *in.AvatarURL
	}
	s.DB.Model(&User{}).Where("id = ?", uid(c)).Updates(updates)
	s.me(c)
}

func (s *Server) getUser(c *gin.Context) {
	var u User
	if s.DB.First(&u, c.Param("id")).Error != nil {
		respondError(c, 404, "user not found")
		return
	}
	var videos, followers, following int64
	s.DB.Model(&Video{}).Where("author_id = ? AND status = ?", u.ID, "published").Count(&videos)
	s.DB.Model(&Follow{}).Where("followee_id = ?", u.ID).Count(&followers)
	s.DB.Model(&Follow{}).Where("follower_id = ?", u.ID).Count(&following)
	c.JSON(200, gin.H{"user": u, "video_count": videos, "follower_count": followers, "following_count": following})
}
