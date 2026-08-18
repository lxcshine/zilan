package handler

import (
	"net/http"
	"strconv"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
)

// ParseReviewHandler manages the parse quality review queue (§5.4).
type ParseReviewHandler struct {
	repo interfaces.ParseReviewRepository
}

func NewParseReviewHandler(repo interfaces.ParseReviewRepository) *ParseReviewHandler {
	return &ParseReviewHandler{repo: repo}
}

// RegisterRoutes registers parse review endpoints on the given router group.
func (h *ParseReviewHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/parse-reviews", h.listReviews)
	rg.GET("/parse-reviews/:id", h.getReview)
	rg.PATCH("/parse-reviews/:id", h.updateReview)
}

// listReviews returns pending parse review items.
func (h *ParseReviewHandler) listReviews(c *gin.Context) {
	tenantInfo := c.MustGet("tenantInfo").(*types.Tenant)
	kbID := c.Query("knowledge_base_id")

	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))

	items, total, err := h.repo.ListPendingReviews(c.Request.Context(), tenantInfo.ID, kbID, limit, offset)
	if err != nil {
		logger.Errorf(c.Request.Context(), "list parse reviews failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list parse reviews"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"items":  items,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

// getReview returns a single parse review item.
func (h *ParseReviewHandler) getReview(c *gin.Context) {
	id := c.Param("id")
	item, err := h.repo.GetReviewItem(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Parse review not found"})
		return
	}
	c.JSON(http.StatusOK, item)
}

// updateReview resolves a parse review item (approve reparse, manual fix, or discard).
func (h *ParseReviewHandler) updateReview(c *gin.Context) {
	id := c.Param("id")

	var req struct {
		Status     string `json:"status" binding:"required"`
		Resolution string `json:"resolution"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	tenantInfo := c.MustGet("tenantInfo").(*types.Tenant)
	reviewerID := strconv.FormatUint(uint64(tenantInfo.ID), 10)

	if err := h.repo.UpdateReviewStatus(c.Request.Context(), id, req.Status, req.Resolution, reviewerID); err != nil {
		logger.Errorf(c.Request.Context(), "update parse review failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update parse review"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
