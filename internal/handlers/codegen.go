package handlers

import (
	"net/http"

	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
)

type CodegenHandler struct{}

func NewCodegenHandler() *CodegenHandler {
	return &CodegenHandler{}
}

func (h *CodegenHandler) Generate(c *gin.Context) {
	var req services.CodegenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.Language == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "language is required"})
		return
	}

	if req.URL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "url is required"})
		return
	}

	if req.Method == "" {
		req.Method = "GET"
	}

	code, err := services.GenerateCode(&req)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"code": code}})
}
