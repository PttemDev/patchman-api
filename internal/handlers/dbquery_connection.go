package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"patchman-be/internal/dbquery"
	"patchman-be/internal/models"
	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// DBConnectionHandler covers the connection-side surface — CRUD, the
// one-shot "test connection" probe, owner-only password reveal, and
// the cached-schema accessor used by the FE autocomplete. The execute
// path lives in dbquery_execute.go because it needs a different layer
// of permission checks (delegated grants + intent enforcement).
type DBConnectionHandler struct {
	db       *gorm.DB
	cipher   *services.TokenCipher
	registry *dbquery.Registry
}

func NewDBConnectionHandler(db *gorm.DB, cipher *services.TokenCipher, reg *dbquery.Registry) *DBConnectionHandler {
	return &DBConnectionHandler{db: db, cipher: cipher, registry: reg}
}

// pgFamilyKinds are the SQL kinds that bind (or, for SQL Server, introspect)
// exactly one database per connection, so a default DB must be named. MySQL-
// family kinds (mysql/mariadb/tidb, incl. StarRocks) and mongo can leave it
// blank to browse every database at once. SQL Server's INFORMATION_SCHEMA is
// per-database, so it needs the DB named too even though it isn't Postgres.
var pgFamilyKinds = map[string]bool{
	"postgres":    true,
	"cockroachdb": true,
	"yugabytedb":  true,
	"redshift":    true,
	"sqlserver":   true,
}

// ── payloads ────────────────────────────────────────────────────────

type createConnectionInput struct {
	// FE-supplied UUID — lets the FE dual-write the same connection
	// to multiple BEs (the embedded sidecar + the team server) and
	// dedup by primary key when listing. Optional; we generate when blank.
	ID   string `json:"id"`
	Name string `json:"name" binding:"required"`
	Kind string `json:"kind" binding:"required"`
	// Host/Port are anchor labels for the sidebar but optional when a
	// raw ConnectionString is supplied — `mongodb+srv://…` has no port,
	// and exotic DSNs (multi-host, unix socket) don't survive the
	// structured form. The DSN takes priority at connect time; the
	// structured fields just label the row in the UI. See the manual
	// check below for the "must have one of (host+port) or DSN" rule.
	Host             string         `json:"host"`
	Port             int            `json:"port"`
	Database         string         `json:"database"`
	Username         string         `json:"username"`
	Password         string         `json:"password"`
	Options          map[string]any `json:"options"`
	WorkspaceID      string         `json:"workspaceId"`
	// ConnectionString overrides the auto-built DSN entirely. Stored
	// encrypted alongside the password.
	ConnectionString string `json:"connectionString"`
	// PrimaryScope is the user-chosen default ('local' | 'remote') for
	// the toolbar Local/Remote toggle. The FE sends the same value on
	// every dual-write so both BE copies agree.
	PrimaryScope string `json:"primaryScope"`
	// AllowWrite is the per-connection read-write master switch. Optional
	// on create; defaults to false (read-only) so a new connection behaves
	// exactly as in v1 until an owner/co-owner flips it on.
	AllowWrite bool `json:"allowWrite"`
	// CopySecretsFrom is the "Duplicate" flow: an existing connection whose
	// stored password fills in a blank Password — and, with
	// CopyConnectionString, whose custom DSN fills in a blank
	// ConnectionString. The ciphertext is reused as-is on this BE, so the
	// secret never round-trips through the browser. Off for the DSN when
	// the copy switched to the structured form (the DSN would override it).
	CopySecretsFrom      string `json:"copySecretsFrom"`
	CopyConnectionString bool   `json:"copyConnectionString"`
}

type updateConnectionInput struct {
	Name             *string        `json:"name"`
	Host             *string        `json:"host"`
	Port             *int           `json:"port"`
	Database         *string        `json:"database"`
	Username         *string        `json:"username"`
	Password         *string        `json:"password"`         // nil = keep existing
	ConnectionString *string        `json:"connectionString"` // empty string clears
	Options          map[string]any `json:"options"`
	PrimaryScope     *string        `json:"primaryScope"` // nil = keep existing
	// AllowWrite toggles the per-connection read-write master switch. nil =
	// keep existing; requires the caller to be owner or a 'write' co-owner
	// (enforced by the Update handler's canManage gate).
	AllowWrite *bool `json:"allowWrite"`
}

// connectionDTO is the shape returned to FE — strips password_enc and
// surfaces a sharing summary so the FE doesn't need follow-up calls
// for the sidebar row.
type connectionDTO struct {
	ID                  string    `json:"id"`
	UserID              string    `json:"userId"`
	WorkspaceID         string    `json:"workspaceId,omitempty"`
	Name                string    `json:"name"`
	Kind                string    `json:"kind"`
	Host                string    `json:"host"`
	Port                int       `json:"port"`
	Database            string    `json:"database"`
	Username            string    `json:"username"`
	Options             any       `json:"options,omitempty"`
	// HasConnectionString is true when the row carries a custom DSN
	// override. The plaintext is never returned in list/get responses —
	// owners reveal it through the dedicated endpoint, same way as the
	// password.
	HasConnectionString bool      `json:"hasConnectionString"`
	OwnerName           string    `json:"ownerName,omitempty"`
	Shared              bool      `json:"shared"`
	GrantRole           string    `json:"grantRole,omitempty"`
	// PrimaryScope ('local' | 'remote' | '') — user-chosen default for
	// the toolbar Local/Remote toggle. Empty = legacy row, FE falls back
	// to 'remote'.
	PrimaryScope string    `json:"primaryScope,omitempty"`
	// AllowWrite mirrors the model's per-connection read-write switch so
	// the FE can render the R / R+W badge and gate its write affordances.
	AllowWrite bool      `json:"allowWrite"`
	// CopiedFrom echoes a create's CopySecretsFrom once its secrets were
	// applied, so the FE can tell a BE that predates the field (which
	// would silently store an empty password) from one that copied them.
	CopiedFrom string    `json:"copiedFrom,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// ── handlers ────────────────────────────────────────────────────────

func (h *DBConnectionHandler) Create(c *gin.Context) {
	userID := c.GetString("userID")
	var input createConnectionInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !validKind(input.Kind) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "kind must be one of: postgres, mysql, mongo, cockroachdb, yugabytedb, redshift, mariadb, tidb, sqlserver"})
		return
	}
	sec, ok := h.secretsFor(c, userID, input)
	if !ok {
		return
	}
	// Either a raw DSN OR a host+port pair must be present. DSN-only
	// (mongodb+srv://, multi-host, unix socket) skips the structured
	// fields entirely. The driver layer decides which to consume. A DSN
	// copied from a duplicate's source counts.
	hasDSN := strings.TrimSpace(input.ConnectionString) != "" || sec.copiedDSN
	if !hasDSN {
		if strings.TrimSpace(input.Host) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "host is required when no connection string is provided"})
			return
		}
		if input.Port == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "port is required when no connection string is provided"})
			return
		}
	}
	// Postgres-family drivers bind one database per connection, so a default
	// DB is required. Mongo and MySQL/StarRocks can roam every database
	// without one — introspection then lists all user schemas so the sidebar
	// groups them by DB. Keep this in sync with the FE (databaseRequired).
	if input.Database == "" && !hasDSN && pgFamilyKinds[input.Kind] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "database is required for this connection type unless a custom connection string is provided"})
		return
	}
	passwordEnc, dsnEnc := sec.passwordEnc, sec.dsnEnc
	optBytes, _ := json.Marshal(or(input.Options, map[string]any{}))
	id := strings.TrimSpace(input.ID)
	if id == "" {
		id = uuid.New().String()
	}
	row := models.DBConnection{
		ID:                  id,
		UserID:              userID,
		WorkspaceID:         input.WorkspaceID,
		Name:                input.Name,
		Kind:                input.Kind,
		Host:                input.Host,
		Port:                input.Port,
		Database:            input.Database,
		Username:            input.Username,
		PasswordEnc:         passwordEnc,
		ConnectionStringEnc: dsnEnc,
		OptionsJSON:         string(optBytes),
		PrimaryScope:        normalizeScope(input.PrimaryScope),
		AllowWrite:          input.AllowWrite,
	}
	if err := h.db.Create(&row).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save connection"})
		return
	}
	dto := toDTO(row, "", false, "")
	dto.CopiedFrom = sec.copiedFrom
	c.JSON(http.StatusCreated, gin.H{"data": dto})
}

func (h *DBConnectionHandler) List(c *gin.Context) {
	userID := c.GetString("userID")
	// Own + grant-shared rows in one trip. The shared half uses an
	// EXISTS join against grants + team_members so user-grants and
	// team-grants both surface without N+1.
	var own []models.DBConnection
	if err := h.db.Where("user_id = ?", userID).Order("created_at DESC").Find(&own).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list connections"})
		return
	}
	var shared []models.DBConnection
	err := h.db.
		Where(`id IN (
			SELECT g.connection_id FROM db_connection_grants g
			WHERE (g.subject_kind = 'user' AND g.subject_id = ?)
			   OR (g.subject_kind = 'team' AND g.subject_id IN (
				   SELECT tm.team_id FROM team_members tm WHERE tm.user_id = ?
			   ))
		) AND user_id <> ?`, userID, userID, userID).
		Order("created_at DESC").
		Find(&shared).Error
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list shared connections"})
		return
	}

	// Owner-name lookup for shared rows.
	ownerIDs := make([]string, 0, len(shared))
	for _, s := range shared {
		ownerIDs = append(ownerIDs, s.UserID)
	}
	owners := map[string]string{}
	if len(ownerIDs) > 0 {
		var rows []struct{ ID, Email, Name string }
		h.db.Table("users").Select("id, email, name").Where("id IN ?", ownerIDs).Scan(&rows)
		for _, r := range rows {
			if r.Name != "" {
				owners[r.ID] = r.Name
			} else {
				owners[r.ID] = r.Email
			}
		}
	}

	out := make([]connectionDTO, 0, len(own)+len(shared))
	for _, c := range own {
		out = append(out, toDTO(c, "", false, "owner"))
	}
	// Surface each shared row's ACTUAL grant role so the FE can tell a
	// co-owner ('write') from a query-only grantee ('read') — a co-owner
	// needs the ⋯ menu (edit/share/delete/toggle) that a read-grantee must
	// never see. Falls back to "read" defensively if the grant vanished.
	for i := range shared {
		role := dbConnRole(h.db, &shared[i], userID)
		if role == "" {
			role = "read"
		}
		out = append(out, toDTO(shared[i], owners[shared[i].UserID], true, role))
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

func (h *DBConnectionHandler) Get(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	conn, role, err := h.loadAccessible(c.Request.Context(), id, userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	shared := conn.UserID != userID
	c.JSON(http.StatusOK, gin.H{"data": toDTO(*conn, "", shared, role)})
}

func (h *DBConnectionHandler) Update(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	// Owner OR a 'write' co-owner may edit. loadAccessible resolves the
	// caller's effective role; canManage relaxes the old owner-only gate to
	// admit co-owners while still refusing read-grantees. A user with no
	// access at all falls through loadAccessible's error → 404.
	conn, role, err := h.loadAccessible(c.Request.Context(), id, userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	if !canManage(role) {
		c.JSON(http.StatusForbidden, gin.H{"error": "You have read-only access to this shared connection."})
		return
	}
	var input updateConnectionInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	updates := map[string]any{}
	if input.Name != nil {
		updates["name"] = *input.Name
	}
	if input.Host != nil {
		updates["host"] = *input.Host
	}
	if input.Port != nil {
		updates["port"] = *input.Port
	}
	if input.Database != nil {
		updates["database"] = *input.Database
	}
	if input.Username != nil {
		updates["username"] = *input.Username
	}
	if input.Password != nil {
		enc, err := h.cipher.Encrypt(*input.Password)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt password"})
			return
		}
		updates["password_enc"] = enc
	}
	if input.ConnectionString != nil {
		if *input.ConnectionString == "" {
			updates["connection_string_enc"] = ""
		} else {
			enc, err := h.cipher.Encrypt(*input.ConnectionString)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt connection string"})
				return
			}
			updates["connection_string_enc"] = enc
		}
	}
	if input.Options != nil {
		b, _ := json.Marshal(input.Options)
		updates["options_json"] = string(b)
	}
	if input.PrimaryScope != nil {
		updates["primary_scope"] = normalizeScope(*input.PrimaryScope)
	}
	// The read-write master switch. A map update writes zero values, so
	// flipping AllowWrite back to false persists correctly. Gated above by
	// canManage — a read-grantee never reaches here.
	if input.AllowWrite != nil {
		updates["allow_write"] = *input.AllowWrite
	}
	if err := h.db.Model(conn).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update connection"})
		return
	}
	// Edits invalidate any pooled session + schema cache — the host
	// might be different now.
	h.registry.CloseSession(id)
	h.registry.InvalidateSchema(id)
	h.db.First(conn, "id = ?", id)
	c.JSON(http.StatusOK, gin.H{"data": toDTO(*conn, "", conn.UserID != userID, role)})
}

func (h *DBConnectionHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	// Owner OR a 'write' co-owner may delete. Read-grantees cannot.
	_, role, err := h.loadAccessible(c.Request.Context(), id, userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	if !canManage(role) {
		c.JSON(http.StatusForbidden, gin.H{"error": "You have read-only access to this shared connection."})
		return
	}
	if err := h.db.Where("id = ?", id).Delete(&models.DBConnection{}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete connection"})
		return
	}
	h.registry.CloseSession(id)
	h.registry.InvalidateSchema(id)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "deleted"}})
}

// Test runs Connect+ping without saving — used by the connection
// dialog's "Test connection" button. Accepts either an existing ID
// (uses the stored encrypted password) or a full inline payload (for
// pre-save validation).
func (h *DBConnectionHandler) Test(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	var conn *models.DBConnection
	if id != "" && id != "new" {
		// Probing a saved connection reuses its stored (encrypted)
		// password. Owner OR a 'write' co-owner may test — they can already
		// reveal the password and edit the row, so testing adds no
		// exposure; a read-grantee is refused.
		existing, role, err := h.loadAccessible(c.Request.Context(), id, userID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
			return
		}
		if !canManage(role) {
			c.JSON(http.StatusForbidden, gin.H{"error": "You have read-only access to this shared connection."})
			return
		}
		conn = existing
	} else {
		var input createConnectionInput
		if err := c.ShouldBindJSON(&input); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if !validKind(input.Kind) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "kind must be one of: postgres, mysql, mongo, cockroachdb, yugabytedb, redshift, mariadb, tidb, sqlserver"})
			return
		}
		// A duplicate being tested before save dials with its source's
		// stored secrets (same manage gate as Create).
		sec, ok := h.secretsFor(c, userID, input)
		if !ok {
			return
		}
		// Same DSN-vs-host gate as Create — the probe must have something
		// to dial.
		if strings.TrimSpace(input.ConnectionString) == "" && !sec.copiedDSN {
			if strings.TrimSpace(input.Host) == "" || input.Port == 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "host and port are required when no connection string is provided"})
				return
			}
		}
		optBytes, _ := json.Marshal(or(input.Options, map[string]any{}))
		conn = &models.DBConnection{
			Kind: input.Kind, Host: input.Host, Port: input.Port,
			Database: input.Database, Username: input.Username,
			PasswordEnc: sec.passwordEnc, ConnectionStringEnc: sec.dsnEnc,
			OptionsJSON: string(optBytes),
		}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 8*time.Second)
	defer cancel()
	info, err := h.registry.TestInfo(ctx, *conn)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": false, "error": err.Error()}})
		return
	}
	res := gin.H{"ok": true}
	if info.AuthSource != "" {
		res["authSource"] = info.AuthSource
	}
	if info.DirectHost != "" {
		res["directHost"] = info.DirectHost
		res["directPrimary"] = info.DirectPrimary
		res["members"] = info.Members
	}
	c.JSON(http.StatusOK, gin.H{"data": res})
}

// RevealPassword is restricted to callers who can manage the connection
// (owner or a 'write' co-owner); read-grantees are refused so a delegated
// query-only share never surfaces the plaintext secret. The BE audit
// middleware (mutating method on a configured route) captures the call
// site so a "show me the secret" event is traceable.
func (h *DBConnectionHandler) RevealPassword(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	conn, role, err := h.loadAccessible(c.Request.Context(), id, userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	if !canManage(role) {
		c.JSON(http.StatusForbidden, gin.H{"error": "You have read-only access to this shared connection."})
		return
	}
	pw, err := h.cipher.Decrypt(conn.PasswordEnc)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "decrypt"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"password": pw}})
}

// Schema returns the cached metadata snapshot. Read access is enough —
// owner OR grant-holder can pull it.
func (h *DBConnectionHandler) Schema(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	conn, _, err := h.loadAccessible(c.Request.Context(), id, userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	schema, err := h.registry.Schema(ctx, *conn)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": schema})
}

func (h *DBConnectionHandler) RefreshSchema(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	conn, _, err := h.loadAccessible(c.Request.Context(), id, userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	h.registry.InvalidateSchema(id)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	schema, err := h.registry.Schema(ctx, *conn)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": schema})
}

// ── helpers ─────────────────────────────────────────────────────────

// connSecrets is a create/test payload's password + DSN, encrypted.
type connSecrets struct {
	passwordEnc, dsnEnc string
	copiedFrom          string // the CopySecretsFrom source that was applied
	copiedDSN           bool   // dsnEnc came from that source
}

// secretsFor encrypts the payload's password and DSN, filling blank ones
// from CopySecretsFrom. The source must be one the caller can manage —
// the same gate as reveal-password: a read-grantee may query a shared
// connection but never obtain its credentials, and owning a copy would
// hand them exactly that. ok=false means a response was already written.
func (h *DBConnectionHandler) secretsFor(c *gin.Context, userID string, in createConnectionInput) (connSecrets, bool) {
	var out connSecrets
	if srcID := strings.TrimSpace(in.CopySecretsFrom); srcID != "" {
		src, role, err := h.loadAccessible(c.Request.Context(), srcID, userID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Source connection not found"})
			return out, false
		}
		if !canManage(role) {
			c.JSON(http.StatusForbidden, gin.H{"error": "You have read-only access to this shared connection."})
			return out, false
		}
		out.copiedFrom = src.ID
		if in.Password == "" {
			out.passwordEnc = src.PasswordEnc
		}
		if in.ConnectionString == "" && in.CopyConnectionString && src.ConnectionStringEnc != "" {
			out.dsnEnc = src.ConnectionStringEnc
			out.copiedDSN = true
		}
	}
	if out.passwordEnc == "" {
		enc, err := h.cipher.Encrypt(in.Password)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt password"})
			return out, false
		}
		out.passwordEnc = enc
	}
	if in.ConnectionString != "" {
		enc, err := h.cipher.Encrypt(in.ConnectionString)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt connection string"})
			return out, false
		}
		out.dsnEnc = enc
	}
	return out, true
}

// loadAccessible loads a connection if the user can access it (owner OR
// any active grant). Returns the row + the user's effective role on it:
// "owner", "write" (a co-owner grant), or "read" (a query-only grant).
func (h *DBConnectionHandler) loadAccessible(_ context.Context, id, userID string) (*models.DBConnection, string, error) {
	var conn models.DBConnection
	if err := h.db.First(&conn, "id = ?", id).Error; err != nil {
		return nil, "", err
	}
	role := dbConnRole(h.db, &conn, userID)
	if role == "" {
		return nil, "", errors.New("forbidden")
	}
	return &conn, role, nil
}

// dbConnRole computes a user's effective role on a DB connection: "owner"
// when they own it, otherwise the highest role among their direct + team
// grants ("write" beats "read"), or "" when they have no access. Shared by
// loadAccessible and the grant handler so every permission decision reads
// the same source of truth. Team grants are honored via team_members.
func dbConnRole(db *gorm.DB, conn *models.DBConnection, userID string) string {
	if conn.UserID == userID {
		return "owner"
	}
	var grants []models.DBConnectionGrant
	db.Where(`connection_id = ? AND (
		(subject_kind = 'user' AND subject_id = ?)
		OR (subject_kind = 'team' AND subject_id IN (
			SELECT tm.team_id FROM team_members tm WHERE tm.user_id = ?
		))
	)`, conn.ID, userID, userID).Find(&grants)
	role := ""
	for _, g := range grants {
		if g.Role == "write" {
			return "write"
		}
		if g.Role == "read" {
			role = "read"
		}
	}
	return role
}

// canManage reports whether a role may perform owner-class actions: edit,
// delete, reveal password, manage grants, toggle the AllowWrite switch, run
// write queries (when AllowWrite is on), and lift the row limit. Only the
// owner and 'write' co-owners qualify — a 'read' grantee never can.
func canManage(role string) bool {
	return role == "owner" || role == "write"
}

func validKind(k string) bool {
	switch k {
	case "postgres", "mysql", "mongo",
		"cockroachdb", "yugabytedb", "redshift",
		"mariadb", "tidb", "sqlserver":
		return true
	}
	return false
}

func toDTO(c models.DBConnection, ownerName string, shared bool, grantRole string) connectionDTO {
	var opts any
	if c.OptionsJSON != "" {
		_ = json.Unmarshal([]byte(c.OptionsJSON), &opts)
	}
	return connectionDTO{
		ID: c.ID, UserID: c.UserID, WorkspaceID: c.WorkspaceID,
		Name: c.Name, Kind: c.Kind, Host: c.Host, Port: c.Port,
		Database: c.Database, Username: c.Username, Options: opts,
		HasConnectionString: c.ConnectionStringEnc != "",
		OwnerName:           ownerName, Shared: shared, GrantRole: grantRole,
		PrimaryScope: c.PrimaryScope,
		AllowWrite:   c.AllowWrite,
		CreatedAt:    c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}

// normalizeScope rejects anything outside the allowed enum so a typo'd
// payload doesn't poison the column. Empty string survives so the FE
// can keep its "legacy → fall back to remote" default.
func normalizeScope(s string) string {
	switch strings.TrimSpace(strings.ToLower(s)) {
	case "local":
		return "local"
	case "remote":
		return "remote"
	}
	return ""
}

func or[T any](v T, def T) T {
	// JSON `null` for a typed map deserializes as a nil map — make the
	// distinction explicit instead of letting downstream code blow up.
	any := any(v)
	if any == nil {
		return def
	}
	return v
}
