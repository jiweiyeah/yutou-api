package controller

import (
	"net/http"

	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

// GetChannelCacheStats 获取渠道缓存性能统计
func GetChannelCacheStats(c *gin.Context) {
	stats := model.GetChannelCacheStats()
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    stats,
	})
}

// ResetChannelCacheStats 重置渠道缓存统计数据（仅用于测试）
func ResetChannelCacheStats(c *gin.Context) {
	model.ResetChannelCacheStats()
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "统计数据已重置",
	})
}
