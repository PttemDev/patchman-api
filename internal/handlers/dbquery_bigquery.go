package handlers

// BigQuery-specific parts of the DB connection / execute handlers: input
// normalisation (no host/port; project + key or the desktop's gcloud) and
// the cost guard limits (GOOGLE_CLOUD_RFC.md ADR-3):
//
//	limit = connection's own limit, else the user's setting, else 10 GB,
//	        never above the admin ceiling (BIGQUERY_MAX_BYTES_CEILING);
//	ask   = connection's own threshold, else the user's, else 1 GB.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const (
	gib                 = int64(1) << 30
	defaultBigQueryMax  = 10 * gib
	defaultBigQueryAsk  = 1 * gib
	bigQuerySettingsKey = "bigquery"
	bigQueryDefaultHost = "bigquery.googleapis.com"
	bigQueryDefaultPort = 443
)

var (
	embeddedMode       atomic.Bool
	bigQueryCeilingVal atomic.Int64
)

// SetEmbeddedMode records whether this backend runs inside the desktop app
// (the gcloud / ADC auth mode is desktop-only).
func SetEmbeddedMode(v bool) { embeddedMode.Store(v) }

// SetBigQueryCeiling sets the admin ceiling in bytes (0 = none).
func SetBigQueryCeiling(n int64) { bigQueryCeilingVal.Store(n) }

// normalizeBigQueryInput fills / checks a BigQuery connection's fields.
// passwordGiven: a key was supplied now or copied from a source connection.
func normalizeBigQueryInput(in *createConnectionInput, passwordGiven bool) error {
	if in.Options == nil {
		in.Options = map[string]any{}
	}
	in.Database = strings.TrimSpace(in.Database)
	if in.Database == "" {
		return errors.New("project id is required for BigQuery")
	}
	if strings.TrimSpace(in.Host) == "" {
		in.Host = bigQueryDefaultHost
	}
	if in.Port == 0 {
		in.Port = bigQueryDefaultPort
	}
	auth, _ := in.Options["auth"].(string)
	switch auth {
	case "adc":
		if !embeddedMode.Load() {
			return errors.New("gcloud credentials can only be used in the PatchMan desktop app — use a service account key on a server")
		}
		in.Password = ""
		if strings.TrimSpace(in.Username) == "" {
			in.Username = "gcloud"
		}
	case "", "service_account":
		in.Options["auth"] = "service_account"
		key := strings.TrimSpace(in.Password)
		if key == "" {
			if !passwordGiven {
				return errors.New("a service account key (JSON) is required")
			}
			return nil
		}
		var k struct {
			Type        string `json:"type"`
			ClientEmail string `json:"client_email"`
			ProjectID   string `json:"project_id"`
		}
		if json.Unmarshal([]byte(key), &k) != nil || k.Type == "" {
			return errors.New("the key must be the JSON file Google gives for a service account")
		}
		if k.ClientEmail != "" {
			in.Username = k.ClientEmail
		}
	case "credential":
		id, _ := in.Options["gcpCredentialId"].(string)
		if strings.TrimSpace(id) == "" {
			return errors.New("pick the Google Cloud credential this BigQuery connection signs in with")
		}
		in.Options["gcpCredentialId"] = strings.TrimSpace(id)
		in.Password = ""
	default:
		return errors.New("auth must be service_account, adc or credential")
	}
	if auth != "credential" {
		delete(in.Options, "gcpCredentialId")
	}
	return nil
}

// lookupBigQueryCredential finds the shared Google Cloud credential a BigQuery
// connection signs in with: it must belong to the connection's owner (a
// co-owner editing / a grantee querying can never point it at their own or
// someone else's credential), and a linked Google account must carry the
// BigQuery scope.
func lookupBigQueryCredential(db *gorm.DB, ownerID, credID string) (models.GCPCredential, error) {
	var row models.GCPCredential
	if strings.TrimSpace(credID) == "" {
		return row, errors.New("pick the Google Cloud credential this BigQuery connection signs in with")
	}
	if err := db.Where("id = ? AND user_id = ?", credID, ownerID).First(&row).Error; err != nil {
		return row, errors.New("the Google Cloud credential of this BigQuery connection isn't one of the connection owner's credentials on this server (deleted, or added on another backend) — pick another in the connection settings")
	}
	if !gcpGrantsBigQuery(row) {
		return row, fmt.Errorf("the linked Google account %s wasn't given BigQuery access — connect it again in Settings → Google Cloud with BigQuery ticked", row.Account)
	}
	return row, nil
}

// checkBigQueryCredential validates a "credential" connection against its
// owner's credentials and labels the row with the credential's account.
func (h *DBConnectionHandler) checkBigQueryCredential(ownerID string, opts map[string]any, username *string) error {
	if a, _ := opts["auth"].(string); a != "credential" {
		return nil
	}
	id, _ := opts["gcpCredentialId"].(string)
	row, err := lookupBigQueryCredential(h.db, ownerID, id)
	if err != nil {
		return err
	}
	if username != nil {
		*username = firstNonEmpty(row.Account, row.Name)
	}
	return nil
}

// bigQueryLimits resolves the effective scan limit and ask threshold.
func bigQueryLimits(db *gorm.DB, userID string, conn *models.DBConnection) (maxBytes, askBytes int64) {
	user := loadBigQuerySettings(db, userID)
	maxBytes, askBytes = defaultBigQueryMax, defaultBigQueryAsk
	if user.MaxBytes > 0 {
		maxBytes = user.MaxBytes
	}
	if user.AskBytes > 0 {
		askBytes = user.AskBytes
	}
	var opts map[string]any
	_ = json.Unmarshal([]byte(conn.OptionsJSON), &opts)
	if v := numberOpt(opts["maxBytesBilled"]); v > 0 {
		maxBytes = v
	}
	if v := numberOpt(opts["askBytes"]); v > 0 {
		askBytes = v
	}
	if ceil := bigQueryCeilingVal.Load(); ceil > 0 && maxBytes > ceil {
		maxBytes = ceil
	}
	if askBytes > maxBytes {
		askBytes = maxBytes
	}
	return maxBytes, askBytes
}

func numberOpt(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case json.Number:
		n, _ := t.Int64()
		return n
	}
	return 0
}

// ── per-user settings (Settings → BigQuery) ─────────────────────────────────

type bigQuerySettings struct {
	MaxBytes int64 `json:"maxBytes"`
	AskBytes int64 `json:"askBytes"`
}

func loadBigQuerySettings(db *gorm.DB, userID string) bigQuerySettings {
	var row models.UserSetting
	var s bigQuerySettings
	if db.Where("user_id = ? AND key = ?", userID, bigQuerySettingsKey).Limit(1).Find(&row).Error == nil && row.Value != "" {
		_ = json.Unmarshal([]byte(row.Value), &s)
	}
	return s
}

// BigQuerySettingsHandler serves GET/PUT /api/settings/bigquery.
type BigQuerySettingsHandler struct{ db *gorm.DB }

func NewBigQuerySettingsHandler(db *gorm.DB) *BigQuerySettingsHandler {
	return &BigQuerySettingsHandler{db: db}
}

func (h *BigQuerySettingsHandler) Get(c *gin.Context) {
	s := loadBigQuerySettings(h.db, c.GetString("userID"))
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"maxBytes":        s.MaxBytes,
		"askBytes":        s.AskBytes,
		"defaultMaxBytes": defaultBigQueryMax,
		"defaultAskBytes": defaultBigQueryAsk,
		"ceilingBytes":    bigQueryCeilingVal.Load(),
	}})
}

func (h *BigQuerySettingsHandler) Put(c *gin.Context) {
	var in bigQuerySettings
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if in.MaxBytes < 0 || in.AskBytes < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "limits must be positive (0 = default)"})
		return
	}
	if ceil := bigQueryCeilingVal.Load(); ceil > 0 && in.MaxBytes > ceil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "the limit is above the server's ceiling"})
		return
	}
	b, _ := json.Marshal(in)
	row := models.UserSetting{UserID: c.GetString("userID"), Key: bigQuerySettingsKey, Value: string(b), UpdatedAt: time.Now()}
	if err := h.db.Save(&row).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save settings"})
		return
	}
	h.Get(c)
}

// ParseByteSize reads "10GB", "500 MB", "1TB" or a plain byte count
// (1024-based units). 0 / "" means none.
func ParseByteSize(s string) int64 {
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "" {
		return 0
	}
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		mult   int64
	}{{"TB", 1 << 40}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(s, u.suffix) {
			mult = u.mult
			s = strings.TrimSpace(strings.TrimSuffix(s, u.suffix))
			break
		}
	}
	var n float64
	if _, err := fmt.Sscanf(s, "%g", &n); err != nil || n < 0 {
		return 0
	}
	return int64(n * float64(mult))
}
