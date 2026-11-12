package handlers

import (
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// Workspace-team cascade: a team admin edits the content in the team's
// workspace, a member only views it, and an outsider sees nothing.
func TestWorkspaceTeamRole(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"CREATE TABLE workspaces (id TEXT, team_id TEXT)",
		"CREATE TABLE team_members (team_id TEXT, user_id TEXT, role TEXT)",
		"CREATE TABLE collections (id TEXT, user_id TEXT, visibility TEXT, workspace_id TEXT, parent_id TEXT)",
		"CREATE TABLE collection_shares (collection_id TEXT, user_id TEXT, role TEXT)",
		"CREATE TABLE environments (id TEXT, user_id TEXT, visibility TEXT, workspace_id TEXT)",
		"CREATE TABLE environment_shares (environment_id TEXT, user_id TEXT, role TEXT)",
		"CREATE TABLE whiteboards (id TEXT, user_id TEXT, visibility TEXT, workspace_id TEXT, parent_id TEXT)",
		"INSERT INTO workspaces VALUES ('ws', 'team'), ('solo', NULL)",
		"INSERT INTO team_members VALUES ('team', 'owner', 'admin'), ('team', 'admin', 'admin'), ('team', 'member', 'member')",
		"INSERT INTO collections VALUES ('c1', 'owner', 'private', 'ws', NULL), ('c2', 'owner', 'private', 'solo', NULL)",
		"INSERT INTO environments VALUES ('e1', 'owner', 'private', 'ws'), ('e2', 'owner', 'private', 'solo')",
		"INSERT INTO environment_shares VALUES ('e1', 'admin', 'viewer')",
		"INSERT INTO whiteboards VALUES ('folder', 'owner', 'private', 'ws', NULL), ('board', 'owner', 'private', '', 'folder')",
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatal(q, err)
		}
	}

	cases := []struct {
		name string
		got  Role
		want Role
	}{
		{"owner keeps owner", ResolveCollectionRole(db, "owner", "c1"), OwnerRole},
		{"admin edits collection", ResolveCollectionRole(db, "admin", "c1"), EditorRole},
		{"member views collection", ResolveCollectionRole(db, "member", "c1"), ViewerRole},
		{"outsider sees no collection", ResolveCollectionRole(db, "stranger", "c1"), NoRole},
		{"no team, no cascade", ResolveCollectionRole(db, "admin", "c2"), NoRole},
		{"admin edits env over a viewer share", ResolveEnvironmentRole(db, "admin", "e1"), EditorRole},
		{"member views env", ResolveEnvironmentRole(db, "member", "e1"), ViewerRole},
		{"no team, no env cascade", ResolveEnvironmentRole(db, "member", "e2"), NoRole},
		{"admin edits board under a workspace folder", ResolveWhiteboardRole(db, "admin", "board"), EditorRole},
		{"member views board", ResolveWhiteboardRole(db, "member", "board"), ViewerRole},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, c.got, c.want)
		}
	}
	if role := grantRoleFor(db, "e1", "admin"); role != "editor" {
		t.Errorf("env list role for admin = %q, want editor", role)
	}
}
