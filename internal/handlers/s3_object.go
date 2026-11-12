package handlers

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/s3client"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// S3ObjectHandler serves bucket/prefix browsing + object CRUD + presign.
type S3ObjectHandler struct {
	db        *gorm.DB
	registry  *s3client.Registry
	connsHand *S3ConnectionHandler
}

func NewS3ObjectHandler(db *gorm.DB, reg *s3client.Registry, connsHand *S3ConnectionHandler) *S3ObjectHandler {
	return &S3ObjectHandler{db: db, registry: reg, connsHand: connsHand}
}

func (h *S3ObjectHandler) access(c *gin.Context, needWrite bool) (*models.S3Connection, bool) {
	userID := c.GetString("userID")
	conn, role, err := h.connsHand.loadAccessible(c.Param("id"), userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return nil, false
	}
	if needWrite && role != "owner" && role != "write" {
		c.JSON(http.StatusForbidden, gin.H{"error": "This action requires write access on the connection."})
		return nil, false
	}
	return conn, true
}

func (h *S3ObjectHandler) ctx(c *gin.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(c.Request.Context(), d)
}

func (h *S3ObjectHandler) Buckets(c *gin.Context) {
	conn, ok := h.access(c, false)
	if !ok {
		return
	}
	ctx, cancel := h.ctx(c, 20*time.Second)
	defer cancel()
	buckets, err := h.registry.Buckets(ctx, *conn)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": buckets})
}

func (h *S3ObjectHandler) ListObjects(c *gin.Context) {
	conn, ok := h.access(c, false)
	if !ok {
		return
	}
	bucket := c.Query("bucket")
	prefix := c.Query("prefix")
	token := c.Query("token")
	max := int32(parseInt(c.Query("max"), 500))
	ctx, cancel := h.ctx(c, 25*time.Second)
	defer cancel()
	listing, err := h.registry.ListObjects(ctx, *conn, bucket, prefix, token, max)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": listing})
}

func (h *S3ObjectHandler) ObjectMeta(c *gin.Context) {
	conn, ok := h.access(c, false)
	if !ok {
		return
	}
	ctx, cancel := h.ctx(c, 20*time.Second)
	defer cancel()
	meta, err := h.registry.Head(ctx, *conn, c.Query("bucket"), c.Query("key"))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": meta})
}

// GetObject streams the object body to the client (preview or download).
// A byte Range can be supplied for cheap previews of large files.
func (h *S3ObjectHandler) GetObject(c *gin.Context) {
	conn, ok := h.access(c, false)
	if !ok {
		return
	}
	bucket := c.Query("bucket")
	key := c.Query("key")
	rangeHeader := c.Query("range") // e.g. "bytes=0-65535"
	ctx, cancel := h.ctx(c, 60*time.Second)
	defer cancel()
	body, contentType, size, err := h.registry.Get(ctx, *conn, bucket, key, rangeHeader)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	defer body.Close()
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if c.Query("download") == "1" {
		c.Header("Content-Disposition", "attachment; filename=\""+s3client.BaseName(key)+"\"")
	}
	if size > 0 {
		c.Header("Content-Length", strconv.FormatInt(size, 10))
	}
	h.audit(c, conn.ID, "get", bucket, key, nil)
	c.DataFromReader(http.StatusOK, size, contentType, body, nil)
}

func (h *S3ObjectHandler) PutObject(c *gin.Context) {
	conn, ok := h.access(c, true)
	if !ok {
		return
	}
	bucket := c.Query("bucket")
	key := c.Query("key")
	if bucket == "" || key == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bucket and key are required"})
		return
	}
	var reader io.Reader
	contentType := ""
	// Support both multipart form-file uploads and raw body PUT.
	if fh, err := c.FormFile("file"); err == nil {
		f, err := fh.Open()
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "open upload"})
			return
		}
		defer f.Close()
		reader = f
		contentType = fh.Header.Get("Content-Type")
	} else {
		reader = c.Request.Body
		contentType = c.ContentType()
	}
	ctx, cancel := h.ctx(c, 5*time.Minute)
	defer cancel()
	err := h.registry.Put(ctx, *conn, bucket, key, contentType, reader)
	h.audit(c, conn.ID, "put", bucket, key, err)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

func (h *S3ObjectHandler) CopyObject(c *gin.Context) {
	conn, ok := h.access(c, true)
	if !ok {
		return
	}
	var input struct {
		SrcBucket string `json:"srcBucket"`
		SrcKey    string `json:"srcKey"`
		DstBucket string `json:"dstBucket"`
		DstKey    string `json:"dstKey"`
		Move      bool   `json:"move"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx, cancel := h.ctx(c, 60*time.Second)
	defer cancel()
	if err := h.registry.Copy(ctx, *conn, input.SrcBucket, input.SrcKey, input.DstBucket, input.DstKey); err != nil {
		h.audit(c, conn.ID, "copy", input.DstBucket, input.DstKey, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	if input.Move {
		if err := h.registry.Delete(ctx, *conn, input.SrcBucket, input.SrcKey); err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "copied but delete of source failed: " + err.Error()})
			return
		}
	}
	h.audit(c, conn.ID, "copy", input.DstBucket, input.DstKey, nil)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

func (h *S3ObjectHandler) DeleteObject(c *gin.Context) {
	conn, ok := h.access(c, true)
	if !ok {
		return
	}
	bucket := c.Query("bucket")
	key := c.Query("key")
	ctx, cancel := h.ctx(c, 30*time.Second)
	defer cancel()
	err := h.registry.Delete(ctx, *conn, bucket, key)
	h.audit(c, conn.ID, "delete", bucket, key, err)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

func (h *S3ObjectHandler) CreateFolder(c *gin.Context) {
	conn, ok := h.access(c, true)
	if !ok {
		return
	}
	var input struct {
		Bucket string `json:"bucket"`
		Prefix string `json:"prefix"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx, cancel := h.ctx(c, 20*time.Second)
	defer cancel()
	err := h.registry.CreateFolder(ctx, *conn, input.Bucket, input.Prefix)
	h.audit(c, conn.ID, "create_folder", input.Bucket, input.Prefix, err)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

func (h *S3ObjectHandler) Presign(c *gin.Context) {
	// Presigned GET requires read; presigned PUT requires write.
	var input struct {
		Bucket string `json:"bucket"`
		Key    string `json:"key"`
		Method string `json:"method"` // GET | PUT
		TTLSec int    `json:"ttlSec"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	needWrite := input.Method == "PUT"
	conn, ok := h.access(c, needWrite)
	if !ok {
		return
	}
	ttl := time.Duration(input.TTLSec) * time.Second
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	if ttl > 7*24*time.Hour {
		ttl = 7 * 24 * time.Hour // AWS SigV4 presign cap
	}
	ctx, cancel := h.ctx(c, 15*time.Second)
	defer cancel()
	url, err := h.registry.Presign(ctx, *conn, input.Bucket, input.Key, input.Method, ttl)
	h.audit(c, conn.ID, "presign", input.Bucket, input.Key, err)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"url": url, "expiresInSec": int(ttl.Seconds())}})
}

func (h *S3ObjectHandler) audit(c *gin.Context, connID, action, bucket, key string, actErr error) {
	userID := c.GetString("userID")
	errText := ""
	if actErr != nil {
		errText = actErr.Error()
	}
	row := models.S3ActivityHistory{
		ID: uuid.New().String(), UserID: userID, ConnectionID: connID,
		Action: action, Bucket: bucket, ObjectKey: key, ErrorText: errText,
		ExecutedAt: time.Now(),
	}
	go func() { _ = h.db.Create(&row).Error }()
}
