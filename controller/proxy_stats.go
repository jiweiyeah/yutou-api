package controller

import (
	"net/http"
	"strconv"

	relayproxy "github.com/QuantumNous/new-api/relay/proxy"
	"github.com/gin-gonic/gin"
)

// GetProxyStats 获取渠道代理统计信息
func GetProxyStats(c *gin.Context) {
	channelIDStr := c.Param("id")
	channelID, err := strconv.Atoi(channelIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "invalid channel id",
		})
		return
	}

	stats := relayproxy.GetProxyStats(channelID)
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    stats,
	})
}

// ResetProxyState 重置渠道代理状态
func ResetProxyState(c *gin.Context) {
	channelIDStr := c.Param("id")
	channelID, err := strconv.Atoi(channelIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "invalid channel id",
		})
		return
	}

	relayproxy.ResetProxyState(channelID)
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "proxy state reset successfully",
	})
}
