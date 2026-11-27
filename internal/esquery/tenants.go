package esquery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"unicode"

	"patchman-be/internal/models"
)

// OpenSearch Dashboards (security plugin) multi-tenancy. Saved objects —
// index patterns, saved searches, dashboards — live in a per-tenant index,
// so the same OSD shows different data views per tenant. The tenant is picked
// per request with the `securitytenant` header; OSD's tenant resolver
// (security-dashboards-plugin server/multitenancy/tenant_resolver.ts) reads
// it before the session cookie and accepts "global" / "private" / a custom
// tenant name. Searches against OpenSearch itself are not tenant-scoped.

// Tenant kinds.
const (
	TenantGlobal  = "global"
	TenantPrivate = "private"
	TenantCustom  = "custom"
)

// OSDTenant is one tenant the user can read.
type OSDTenant struct {
	ID       string `json:"id"`   // the securitytenant header value
	Name     string `json:"name"` // display name
	Kind     string `json:"kind"` // global | private | custom
	Writable bool   `json:"writable"`
}

// TenantsResult carries the user's tenants plus whether the server has
// multi-tenancy at all (Kibana, OSD without the security plugin → false).
type TenantsResult struct {
	Tenants   []OSDTenant `json:"tenants"`
	Supported bool        `json:"supported"`
	Default   string      `json:"default"`           // the connection's configured tenant ("" = OSD default)
	Current   string      `json:"current,omitempty"` // OSD's user_requested_tenant (the login choice), normalized
	User      string      `json:"user,omitempty"`
}

// SanitizeTenant validates a tenant id. "" = OSD's default (no header).
// Global / private aliases are normalized to "global" / "private".
func SanitizeTenant(s string) (string, error) {
	s = strings.TrimSpace(s)
	switch strings.ToLower(s) {
	case "":
		return "", nil
	case "global", "global_tenant":
		return TenantGlobal, nil
	case "private", "__user__":
		return TenantPrivate, nil
	}
	if len(s) > 100 {
		return "", fmt.Errorf("invalid tenant %q", s)
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("invalid tenant %q", s)
		}
	}
	return s, nil
}

// ConfiguredTenant returns the connection's default tenant (options.kibanaTenant).
func ConfiguredTenant(conn models.ESConnection) string {
	s, _ := parseOptions(conn.OptionsJSON)["kibanaTenant"].(string)
	out, err := SanitizeTenant(s)
	if err != nil {
		return ""
	}
	return out
}

// Tenants lists the OSD tenants of the connection's user. It asks OSD's
// security plugin (/api/v1/configuration/account); a direct connection whose
// Kibana answers 404 falls back to the OpenSearch security account API
// itself. Neither present → Supported=false.
func (r *Registry) Tenants(ctx context.Context, conn models.ESConnection) (*TenantsResult, error) {
	key := conn.ID + "|tenants"
	if v, ok := cacheGet(key); ok && conn.ID != "" {
		return v.(*TenantsResult), nil
	}
	out := &TenantsResult{Default: ConfiguredTenant(conn), Tenants: []OSDTenant{}}
	raw, err := r.kibanaGet(ctx, conn, KibanaScope{}, "/api/v1/configuration/account")
	if errors.Is(err, ErrKibanaNotFound) {
		raw, err = r.securityAccount(ctx, conn)
	}
	switch {
	case errors.Is(err, ErrKibanaNotFound):
		// no multi-tenancy (Kibana / plain OpenSearch)
	case err != nil:
		return nil, err
	default:
		if err := parseAccount(raw, out); err != nil {
			return nil, err
		}
	}
	if out.Supported {
		r.applyTenancyInfo(ctx, conn, out)
	}
	if conn.ID != "" {
		cachePut(key, out)
	}
	return out, nil
}

// applyTenancyInfo reads OSD's /api/v1/multitenancy/info: multi-tenancy off in
// OSD → unsupported (the header would do nothing); private tenant off → drop
// it. Best effort: a missing route (direct OpenSearch) changes nothing.
func (r *Registry) applyTenancyInfo(ctx context.Context, conn models.ESConnection, out *TenantsResult) {
	raw, err := r.kibanaGet(ctx, conn, KibanaScope{}, "/api/v1/multitenancy/info")
	if err != nil {
		return
	}
	var info struct {
		MTEnabled      *bool `json:"multitenancy_enabled"`
		OSDMTEnabled   *bool `json:"opensearch_dashboards_mt_enabled"`
		PrivateEnabled *bool `json:"private_tenant_enabled"`
	}
	if json.Unmarshal(raw, &info) != nil {
		return
	}
	if (info.MTEnabled != nil && !*info.MTEnabled) || (info.OSDMTEnabled != nil && !*info.OSDMTEnabled) {
		out.Supported = false
		out.Tenants = []OSDTenant{}
		out.Current = ""
		return
	}
	if info.PrivateEnabled != nil && !*info.PrivateEnabled {
		kept := out.Tenants[:0]
		for _, tn := range out.Tenants {
			if tn.Kind != TenantPrivate {
				kept = append(kept, tn)
			}
		}
		out.Tenants = kept
	}
}

// ErrUnknownTenant marks a tenant the user cannot read. OSD would silently
// fall back to another tenant, showing its data views under the wrong label.
var ErrUnknownTenant = errors.New("unknown tenant")

// checkTenant rejects a tenant that is not in the user's tenant list (when
// the list is known). Errors fetching the list don't block the request.
func (r *Registry) checkTenant(ctx context.Context, conn models.ESConnection, tenant string) error {
	if tenant == "" {
		return nil
	}
	tr, err := r.Tenants(ctx, conn)
	if err != nil || !tr.Supported {
		return nil
	}
	ids := make([]string, 0, len(tr.Tenants))
	for _, tn := range tr.Tenants {
		if tn.ID == tenant {
			return nil
		}
		ids = append(ids, tn.ID)
	}
	return fmt.Errorf("%w %q: not available to this user (available: %s)", ErrUnknownTenant, tenant, strings.Join(ids, ", "))
}

// securityAccount calls GET _plugins/_security/api/account on the cluster
// (through rawDo, so kibanaProxy goes via the console proxy). 400/404 mean
// the security plugin is absent → ErrKibanaNotFound.
func (r *Registry) securityAccount(ctx context.Context, conn models.ESConnection) ([]byte, error) {
	res, err := r.rawDo(ctx, conn, http.MethodGet, "/_plugins/_security/api/account", nil)
	if err != nil {
		return nil, err
	}
	switch {
	case res.Status == http.StatusNotFound || res.Status == http.StatusBadRequest:
		return nil, ErrKibanaNotFound
	case res.Status >= 400:
		return nil, upstreamError(res)
	}
	return []byte(res.Body), nil
}

// parseAccount reads the security plugin account body:
// {"user_name","user_requested_tenant","tenants":{"global_tenant":true,"<user>":true,"team":false}}.
// OSD's route may wrap it as {"data":{...}}.
func parseAccount(raw []byte, out *TenantsResult) error {
	type account struct {
		UserName            string          `json:"user_name"`
		UserRequestedTenant *string         `json:"user_requested_tenant"`
		Tenants             map[string]bool `json:"tenants"`
	}
	var wrapped struct {
		Data *account `json:"data"`
	}
	var acc account
	if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Data != nil {
		acc = *wrapped.Data
	} else if err := json.Unmarshal(raw, &acc); err != nil {
		return fmt.Errorf("unexpected OpenSearch account response: %w", err)
	}
	if acc.Tenants == nil {
		// Security plugin present but no tenant map → multi-tenancy off.
		return nil
	}
	out.Supported = true
	out.User = acc.UserName
	var custom []OSDTenant
	for name, w := range acc.Tenants {
		switch {
		case name == "global_tenant":
			out.Tenants = append(out.Tenants, OSDTenant{ID: TenantGlobal, Name: "Global", Kind: TenantGlobal, Writable: w})
		case acc.UserName != "" && name == acc.UserName:
			out.Tenants = append(out.Tenants, OSDTenant{ID: TenantPrivate, Name: "Private", Kind: TenantPrivate, Writable: w})
		default:
			custom = append(custom, OSDTenant{ID: name, Name: name, Kind: TenantCustom, Writable: w})
		}
	}
	// Global first, then private, then custom tenants by name.
	sort.SliceStable(out.Tenants, func(i, j int) bool { return out.Tenants[i].Kind == TenantGlobal })
	sort.Slice(custom, func(i, j int) bool { return strings.ToLower(custom[i].Name) < strings.ToLower(custom[j].Name) })
	out.Tenants = append(out.Tenants, custom...)
	if acc.UserRequestedTenant != nil {
		if t, err := SanitizeTenant(*acc.UserRequestedTenant); err == nil {
			out.Current = t
		}
	}
	return nil
}
