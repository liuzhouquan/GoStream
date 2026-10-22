package platform

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const deviceIDHeader = "X-GoStream-Device-ID"
const deviceProtocolPrefix = "gostream.device."

func normalizeDeviceID(value string) (string, bool) {
	if len(value) != 36 {
		return "", false
	}
	for i := 0; i < len(value); i++ {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if value[i] != '-' {
				return "", false
			}
			continue
		}
		if !((value[i] >= '0' && value[i] <= '9') || (value[i] >= 'a' && value[i] <= 'f') || (value[i] >= 'A' && value[i] <= 'F')) {
			return "", false
		}
	}
	return strings.ToLower(value), true
}

// requestDeviceID returns an empty ID for legacy clients that omit the header.
func requestDeviceID(c *gin.Context) (string, bool) {
	raw := c.GetHeader(deviceIDHeader)
	if raw == "" {
		return "", true
	}
	id, ok := normalizeDeviceID(raw)
	if !ok {
		respondError(c, http.StatusBadRequest, "invalid device ID")
	}
	return id, ok
}
