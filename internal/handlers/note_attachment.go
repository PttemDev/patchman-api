package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const noteAttachmentMax = 5 << 20 // 5 MB

// Raster images only. SVG is refused: it can carry script, and these bytes are
// served from the API origin.
var noteAttachmentTypes = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

// UploadAttachment stores an image pasted or dropped into a note and returns
// the markdown that references it (`![name](attachment:<id>)`). Editor-gated.
// The type is sniffed from the bytes, never trusted from the client.
func (h *NoteHandler) UploadAttachment(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	if !canEditNote(h.db, userID, id) {
		c.JSON(http.StatusForbidden, gin.H{"error": "You can't add images to this note"})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, noteAttachmentMax+(1<<20))
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file is required"})
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, noteAttachmentMax+1))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "read file"})
		return
	}
	if len(data) > noteAttachmentMax {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "Images can be at most 5 MB"})
		return
	}
	mime := http.DetectContentType(data)
	ext, ok := noteAttachmentTypes[mime]
	if !ok {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{"error": "Only PNG, JPEG, GIF and WebP images are supported"})
		return
	}
	name := strings.TrimSpace(filepath.Base(header.Filename))
	if name == "" || name == "." || name == "/" {
		name = "image" + ext
	}
	if len(name) > 200 {
		name = name[:200]
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])

	// The same image pasted twice into one note is stored once.
	var att models.NoteAttachment
	if h.db.Select("id", "note_id", "file_name", "mime", "size", "sha256", "created_by", "created_at").
		Where("note_id = ? AND sha256 = ?", id, digest).First(&att).Error != nil {
		att = models.NoteAttachment{ID: uuid.New().String(), NoteID: id, FileName: name, Mime: mime,
			Size: int64(len(data)), SHA256: digest, Data: data, CreatedBy: userID}
		if err := h.db.Create(&att).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "save image"})
			return
		}
	}
	alt := strings.TrimSuffix(att.FileName, filepath.Ext(att.FileName))
	c.JSON(http.StatusCreated, gin.H{"data": gin.H{
		"id": att.ID, "fileName": att.FileName, "mime": att.Mime, "size": att.Size,
		"markdown": "![" + alt + "](attachment:" + att.ID + ")",
	}})
}

// GetAttachment serves an image to anyone who can read its note.
func (h *NoteHandler) GetAttachment(c *gin.Context) {
	userID := c.GetString("userID")
	var att models.NoteAttachment
	if h.db.Where("id = ?", c.Param("attId")).First(&att).Error != nil || !canReadNote(h.db, userID, att.NoteID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Image not found"})
		return
	}
	if _, ok := noteAttachmentTypes[att.Mime]; !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "Image not found"})
		return
	}
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Security-Policy", "default-src 'none'")
	c.Header("Cache-Control", "private, max-age=31536000, immutable") // ids are never reused
	c.Header("Content-Disposition", "inline")
	c.Data(http.StatusOK, att.Mime, att.Data)
}
