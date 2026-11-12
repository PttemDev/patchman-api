package database

import (
	"fmt"
	"log"
	"path/filepath"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/notesearch"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	DriverPostgres = "postgres"
	DriverSQLite   = "sqlite"
)

// Init opens a GORM connection using the requested driver and runs the full
// AutoMigrate set. The embedded desktop build selects sqlite; the hosted
// server stays on postgres. `defaultAdminEmail`, when non-empty, is
// promoted to the admin role at migrate time so the install always has
// at least one human who can grant the role to others. Pass "" to skip
// (e.g. greenfield installs without a designated bootstrap user).
func Init(driver, dsn, defaultAdminEmail string) (*gorm.DB, error) {
	var dialector gorm.Dialector
	switch driver {
	case DriverSQLite:
		if dsn == "" {
			return nil, fmt.Errorf("sqlite driver requires a db path")
		}
		// Clean the path so relative paths resolve predictably (Tauri sidecar
		// launches with an arbitrary CWD).
		if abs, err := filepath.Abs(dsn); err == nil {
			dsn = abs
		}
		// _pragma args are understood by modernc sqlite (glebarez).
		// - foreign_keys=on matches Postgres behavior for FK constraints
		// - journal_mode=WAL keeps reads non-blocking during writes
		// - busy_timeout gives concurrent goroutines time to serialize
		dsn = dsn + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
		dialector = sqlite.Open(dsn)
	case DriverPostgres, "":
		dialector = postgres.Open(dsn)
	default:
		return nil, fmt.Errorf("unsupported db driver: %q", driver)
	}

	db, err := gorm.Open(dialector, &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return nil, err
	}

	// One-shot migration: the legacy `team_members` table held
	// workspace-scoped members (workspace_id, role admin/editor/viewer)
	// — that feature is replaced by the new Teams entity, which claims
	// the same table name with a different schema (team_id, user_id,
	// role admin/member; PK = team_id+user_id). Detect the old shape
	// by the presence of the `workspace_id` column and drop the table
	// before AutoMigrate so GORM creates the new schema fresh.
	// On a brand-new DB the table doesn't exist and this is a no-op.
	// Approved fresh start: existing workspace members are not migrated.
	if db.Migrator().HasTable("team_members") && db.Migrator().HasColumn("team_members", "workspace_id") {
		if err := db.Migrator().DropTable("team_members"); err != nil {
			return nil, fmt.Errorf("drop legacy team_members: %w", err)
		}
	}

	// Auto-migrate all models
	err = db.AutoMigrate(
		&models.User{},
		&models.Collection{},
		&models.SavedRequest{},
		&models.Environment{},
		&models.History{},
		&models.MockServer{},
		&models.MockRoute{},
		&models.TestCase{},
		&models.TestPlan{},
		&models.TestRun{},
		// Workspace stays as a personal organizational scope; team-style
		// membership lives on the new Team / TeamMember pair instead.
		&models.Workspace{},
		&models.Team{},
		&models.TeamMember{},
		&models.CollectionPublicGrant{},
		&models.EnvironmentPublicGrant{},
		&models.Defect{},
		&models.Comment{},
		&models.AuditLog{},
		&models.ActivityFeed{},
		&models.CookieJar{},
		&models.SharedStep{},
		&models.TestCaseVersion{},
		&models.TestCaseFolder{},
		&models.Requirement{},
		&models.RequirementCoverage{},
		&models.JUnitImportResult{},
		&models.LoadTest{},
		&models.CollectionShare{},
		&models.EnvironmentShare{},
		&models.EnvironmentActivation{},
		&models.UserGitIntegration{},
		&models.CollectionGitSource{},
		&models.OAuthPendingState{},
		&models.SystemSetting{},
		// DB Query Tool (RFC: DB_QUERY_TOOL_RFC.md). Encrypted
		// connection store + delegated-execution grants + audit/history.
		&models.DBConnection{},
		&models.DBConnectionGrant{},
		&models.DBQueryHistory{},
		&models.SavedDBQuery{},
		// MCP personal access tokens — auth for the /api/mcp Streamable
		// HTTP endpoint used by external MCP clients (Claude, Inspector).
		&models.MCPToken{},
		// Kafka tool (RFC: KAFKA_S3_AND_DBQUERY_FIXES_RFC.md §A). Encrypted
		// cluster credentials + delegated grants + activity audit.
		&models.KafkaConnection{},
		&models.KafkaConnectionGrant{},
		&models.KafkaActivityHistory{},
		// S3 / object-storage tool (RFC §B).
		&models.S3Connection{},
		&models.S3ConnectionGrant{},
		&models.S3ActivityHistory{},
		// Redis tool. Encrypted endpoint credentials + delegated grants +
		// activity audit.
		&models.RedisConnection{},
		&models.RedisConnectionGrant{},
		&models.RedisActivityHistory{},
		// Elasticsearch tool. Encrypted endpoint credentials + delegated
		// grants + activity audit; thin authenticated HTTP forwarder.
		&models.ESConnection{},
		&models.ESConnectionGrant{},
		&models.ESActivityHistory{},
		&models.GrafanaConnection{},
		&models.GrafanaConnectionGrant{},
		&models.GrafanaActivityHistory{},
		&models.ObservabilityConnection{},
		&models.ObservabilityConnectionGrant{},
		&models.ObservabilityActivityHistory{},
		&models.RabbitConnection{},
		&models.RabbitConnectionGrant{},
		&models.RabbitActivityHistory{},
		&models.GraylogConnection{},
		&models.GraylogConnectionGrant{},
		&models.GraylogActivityHistory{},
		&models.SavedResponse{},
		// Kubernetes tool (read-only explorer, v1). Encrypted kubeconfig +
		// delegated grants + activity audit.
		&models.K8sConnection{},
		&models.K8sConnectionGrant{},
		&models.K8sActivityHistory{},
		// Git tools (v1) — see GIT_V1_CONTRACT.md. Encrypted PAT/SSH secret +
		// delegated grants + managed local clones + activity audit.
		&models.GitConnection{},
		&models.GitConnectionGrant{},
		&models.GitRepo{},
		&models.GitActivityHistory{},
		// LLM providers — encrypted API key for OpenAI-compatible /
		// Anthropic endpoints; requests are proxied + streamed server-side
		// so the key never reaches the browser.
		&models.LLMProvider{},
		// Excalidraw whiteboards — per-user scene JSON (no secrets).
		&models.Whiteboard{},
		&models.WhiteboardPublicGrant{},
		&models.WhiteboardPublicLink{},
		&models.WhiteboardDiagram{},
		// Notes knowledge base — markdown pages/folders, history, sharing, images.
		&models.Note{},
		&models.NoteRevision{},
		&models.NotePublicGrant{},
		&models.NoteAttachment{},
		&models.NoteLink{},
		&models.NoteSuggestion{},
		&models.NoteInbox{},
		&models.NoteMemorySettings{},
	)
	if err != nil {
		return nil, err
	}
	// Full-text search structures for notes (Postgres generated tsvector +
	// GIN, or an SQLite FTS5 table). Idempotent; outside the GORM models.
	// A broken search index degrades note search; it must not stop the boot.
	if err := notesearch.Ensure(db); err != nil {
		log.Printf("notes search index: %v", err)
	}
	// One project folder per memory folder and project key, even when two
	// pods file the first card of a new project at once.
	if err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_notes_memory_project ON notes (parent_id, project)
		WHERE project <> '' AND deleted_at IS NULL`).Error; err != nil {
		log.Printf("notes project index: %v", err)
	}

	// One-shot migration: convert legacy EnvironmentShare collaborator
	// rows into EnvironmentPublicGrant rows so the new share-with-team
	// surface is the single source of truth. Idempotent — the INSERT
	// has a NOT EXISTS guard, and after the first successful run the
	// EnvironmentShare table is wiped (no rows = no work on the next
	// boot). Link-only rows (user_id IS NULL — share-code templates
	// with nobody joined) can't migrate because they have no recipient;
	// they get dropped with the collaborator rows. Wrapped in a single
	// transaction so a partial migration can't leak access.
	if err := migrateEnvironmentSharesToPublicGrants(db); err != nil {
		// Log + continue: don't block boot on a migration that didn't
		// take. The next boot retries; in the worst case admins can
		// re-run manually.
		log.Printf("[migrate] environment_shares → environment_public_grants failed: %v", err)
	}

	// Seed the system_setting singleton row if it doesn't exist yet.
	// FirstOrCreate uses the column defaults declared on the struct
	// (boundary on, rejection/window off) so a fresh DB starts in the
	// expected baseline.
	db.FirstOrCreate(&models.SystemSetting{}, models.SystemSetting{ID: models.SystemSettingSingletonID})

	// Bootstrap the default admin. Idempotent — the WHERE-clause matches
	// the admin email regardless of role, so subsequent boots are no-ops
	// when the user is already admin, and corrective if they were ever
	// demoted by accident. Skipped when no admin email is configured.
	if defaultAdminEmail != "" {
		db.Model(&models.User{}).
			Where("LOWER(email) = LOWER(?)", defaultAdminEmail).
			Where("role IS NULL OR role <> ?", models.RoleAdmin).
			UpdateColumn("role", models.RoleAdmin)
	}

	// Heal orphan workspace_id references left behind by historical
	// workspace deletes (the pre-fix DeleteWorkspace handler dropped the
	// workspace row without touching its dependent rows). Idempotent:
	// after one successful pass no rows match the orphan predicate so
	// subsequent boots are no-ops.
	if err := healOrphanWorkspaceRefs(db); err != nil {
		log.Printf("[migrate] heal orphan workspace_ids failed: %v", err)
	}

	// Saved responses whose collection is gone (deleted before deletes
	// re-homed them, 1.49.0) can never be listed again — drop them.
	// Idempotent; a no-op once clean.
	if res := db.Exec("DELETE FROM saved_responses WHERE collection_id NOT IN (SELECT id FROM collections)"); res.Error != nil {
		log.Printf("[migrate] drop orphan saved responses failed: %v", res.Error)
	} else if res.RowsAffected > 0 {
		log.Printf("[migrate] dropped %d orphan saved responses", res.RowsAffected)
	}

	return db, nil
}

// healOrphanWorkspaceRefs clears `workspace_id` to '' on every row that
// points at a workspace that no longer exists. Affects the four tables
// where workspace_id is a soft (string) reference: collections,
// environments, db_connections, saved_db_queries. EnvironmentActivation
// rows scoped to a missing workspace are deleted outright (they pin
// "active env in workspace X" — the workspace is gone, the row is dead
// weight).
//
// Implementation note: we use NOT EXISTS rather than a LEFT JOIN +
// IS NULL because the latter generates UPDATE ... FROM in Postgres but
// UPDATE WHERE EXISTS in SQLite, and the NOT EXISTS form is the one
// both drivers accept verbatim.
func healOrphanWorkspaceRefs(db *gorm.DB) error {
	tables := []string{"collections", "environments", "db_connections", "saved_db_queries"}
	for _, t := range tables {
		sql := fmt.Sprintf(
			"UPDATE %s SET workspace_id = '' "+
				"WHERE workspace_id IS NOT NULL AND workspace_id != '' "+
				"AND NOT EXISTS (SELECT 1 FROM workspaces w WHERE w.id = %s.workspace_id)",
			t, t,
		)
		res := db.Exec(sql)
		if res.Error != nil {
			return fmt.Errorf("%s: %w", t, res.Error)
		}
		if res.RowsAffected > 0 {
			log.Printf("[migrate] healed %d orphan workspace_id row(s) in %s", res.RowsAffected, t)
		}
	}
	// EnvironmentActivation: drop dangling per-workspace pins. The
	// global bucket (workspace_id = '') is always valid; only the
	// non-empty ones can dangle.
	res := db.Exec(
		"DELETE FROM environment_activations " +
			"WHERE workspace_id IS NOT NULL AND workspace_id != '' " +
			"AND NOT EXISTS (SELECT 1 FROM workspaces w WHERE w.id = environment_activations.workspace_id)",
	)
	if res.Error != nil {
		return fmt.Errorf("environment_activations: %w", res.Error)
	}
	if res.RowsAffected > 0 {
		log.Printf("[migrate] dropped %d orphan environment_activations row(s)", res.RowsAffected)
	}
	return nil
}

// migrateEnvironmentSharesToPublicGrants converts every legacy
// EnvironmentShare collaborator row into an EnvironmentPublicGrant
// row so the new public-grant surface owns the only sharing path
// post-1.16.
//
// Rules:
//   - For each (environment_id, user_id) collaborator row, ensure the
//     env is visibility='public' (flip if currently 'private') and
//     upsert an environment_public_grants row with subject_kind='user'.
//   - Link-only rows (user_id IS NULL) can't migrate because they have
//     no recipient; they're share-code templates only useful for the
//     legacy join flow which goes away in this release.
//   - At the end, wipe environment_shares entirely so subsequent boots
//     find nothing to migrate. Table stays present (handler code still
//     references the model) but the row set is empty.
//
// Wrapped in a single transaction so a partial migration can't leak
// access. Idempotent — re-runs after a successful migration find 0
// collaborator rows and short-circuit.
func migrateEnvironmentSharesToPublicGrants(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		// Pull collaborator rows (user_id IS NOT NULL). If none, the
		// previous migration succeeded — nothing to do.
		type shareRow struct {
			EnvironmentID string
			UserID        string
			Role          string
		}
		var rows []shareRow
		if err := tx.Table("environment_shares").
			Select("environment_id, user_id, role").
			Where("user_id IS NOT NULL AND user_id != ''").
			Scan(&rows).Error; err != nil {
			return fmt.Errorf("scan environment_shares: %w", err)
		}
		if len(rows) == 0 {
			// Still drop the link templates so the table is empty on
			// success; nothing else uses them.
			return tx.Exec("DELETE FROM environment_shares").Error
		}
		log.Printf("[migrate] env shares → grants: %d collaborator rows to convert", len(rows))

		// Collect distinct env ids so we can flip their visibility
		// to 'public' in one statement.
		envIDs := map[string]struct{}{}
		for _, r := range rows {
			envIDs[r.EnvironmentID] = struct{}{}
		}
		envIDSlice := make([]string, 0, len(envIDs))
		for id := range envIDs {
			envIDSlice = append(envIDSlice, id)
		}

		// Flip visibility on every env that had a collaborator — public
		// grants only resolve when visibility='public', and the
		// recipient previously had implicit access via the share row.
		// Leaving them private would drop access on the next request.
		if err := tx.Exec(
			"UPDATE environments SET visibility='public' WHERE id IN ? AND (visibility IS NULL OR visibility <> 'public')",
			envIDSlice,
		).Error; err != nil {
			return fmt.Errorf("flip visibility on migrated envs: %w", err)
		}

		// Insert grants. Idempotent via NOT EXISTS guard so re-running
		// (and the user-grant being added manually before the migration
		// fired) doesn't double up. Roles map directly: 'editor' →
		// editor grant, anything else → viewer.
		now := time.Now()
		for _, r := range rows {
			role := r.Role
			if role != "editor" {
				role = "viewer"
			}
			var existing int64
			tx.Table("environment_public_grants").
				Where("environment_id = ? AND role = ? AND subject_kind = 'user' AND subject_id = ?", r.EnvironmentID, role, r.UserID).
				Count(&existing)
			if existing > 0 {
				continue
			}
			if err := tx.Exec(
				"INSERT INTO environment_public_grants (id, environment_id, role, subject_kind, subject_id, created_at) VALUES (?, ?, ?, 'user', ?, ?)",
				uuid.New().String(), r.EnvironmentID, role, r.UserID, now,
			).Error; err != nil {
				return fmt.Errorf("insert grant for env=%s user=%s: %w", r.EnvironmentID, r.UserID, err)
			}
		}

		// Wipe environment_shares — both link templates and the
		// just-migrated collaborator rows. After this point the new
		// share dialog (post-1.16) is the single source of truth.
		if err := tx.Exec("DELETE FROM environment_shares").Error; err != nil {
			return fmt.Errorf("wipe environment_shares: %w", err)
		}

		log.Printf("[migrate] env shares → grants: migrated %d collaborator rows across %d environments", len(rows), len(envIDSlice))
		return nil
	})
}
