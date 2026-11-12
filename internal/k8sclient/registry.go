package k8sclient

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/services"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/discovery"
	memory "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// Registry pools one Kubernetes clientset per (connection, context), decrypting
// the kubeconfig just-in-time. Mirrors the pool/reap/Close shape of
// redisquery/esquery. The pool key embeds the context name so switching
// contexts is race-free — each context gets its own clientset.
//
// HARDENING: every rest.Config built here has ExecProvider forced to nil and
// all authinfo exec blocks stripped from the parsed kubeconfig, so a malicious
// kubeconfig can never make the BE shell out to an exec credential plugin (RCE
// guard).
//
// This strip is UNCONDITIONAL on a hosted/server BE. The ONLY exception is an
// embedded (desktop) BE where the connection has explicitly opted in via
// OptionsJSON.allowExecPlugins — there the BE runs on the user's own machine
// (where aws/gcloud live), so honoring the kubeconfig's exec credential plugin
// is equivalent to what the user's own kubectl already does. See allowExec().
type Registry struct {
	cipher *services.TokenCipher
	// embedded is true when the BE runs bundled inside the desktop app (set
	// from cfg.Mode == config.ModeEmbedded in main.go). It is a precondition
	// for ever honoring an exec credential plugin; a hosted BE always strips.
	embedded bool

	mu     sync.Mutex
	pooled map[string]*pooled // key = connID + "|" + context

	// promMu guards promCache, the resolved Prometheus service per
	// (connection, context). Discovery lists services / probes the apiserver
	// service-proxy, so the result (positive AND negative) is cached with a TTL
	// to keep chart opens cheap. See resolveProm in prommetrics.go.
	promMu    sync.Mutex
	promCache map[string]promCacheEntry // key = connID + "|" + context
}

type pooled struct {
	clientset *kubernetes.Clientset
	rest      *rest.Config
	lastUsed  time.Time

	// v2 write surface: a dynamic client + a GVK→GVR REST mapper, built lazily
	// on first apply/action and reused for the pooled clientset's lifetime. The
	// mapper is backed by a memory-cached discovery client so repeated applies
	// don't re-hit /apis on every call.
	dynOnce sync.Once
	dyn     dynamic.Interface
	mapper  meta.RESTMapper
	dynErr  error
}

// dynamicClient lazily builds (once per pooled clientset) the dynamic client and
// the discovery-backed REST mapper used by Server-Side Apply and the quick
// actions. It inherits the same hardened rest.Config (ExecProvider nil).
func (p *pooled) dynamicClient() (dynamic.Interface, meta.RESTMapper, error) {
	p.dynOnce.Do(func() {
		dyn, err := dynamic.NewForConfig(p.rest)
		if err != nil {
			p.dynErr = fmt.Errorf("build dynamic client: %w", err)
			return
		}
		disco, err := discovery.NewDiscoveryClientForConfig(p.rest)
		if err != nil {
			p.dynErr = fmt.Errorf("build discovery client: %w", err)
			return
		}
		cached := memory.NewMemCacheClient(disco)
		p.dyn = dyn
		p.mapper = restmapper.NewDeferredDiscoveryRESTMapper(cached)
	})
	return p.dyn, p.mapper, p.dynErr
}

// resolveDynamic resolves a connection/context to its dynamic client + REST
// mapper, building the pooled clientset on first use like resolve does.
func (r *Registry) resolveDynamic(conn models.K8sConnection, contextName string) (*pooled, dynamic.Interface, meta.RESTMapper, error) {
	p, err := r.resolve(conn, contextName)
	if err != nil {
		return nil, nil, nil, err
	}
	dyn, mapper, err := p.dynamicClient()
	if err != nil {
		return nil, nil, nil, err
	}
	return p, dyn, mapper, nil
}

const clientIdleTimeout = 5 * time.Minute

func NewRegistry(cipher *services.TokenCipher, embedded bool) *Registry {
	r := &Registry{cipher: cipher, embedded: embedded, pooled: map[string]*pooled{}, promCache: map[string]promCacheEntry{}}
	go r.reapLoop()
	return r
}

// options is the parsed view of K8sConnection.OptionsJSON.
type options struct {
	InsecureSkipVerify bool     `json:"insecureSkipVerify"`
	AllowedNamespaces  []string `json:"allowedNamespaces"`
	DisableExecPlugins *bool    `json:"disableExecPlugins"`
	// AllowExecPlugins opts this connection in to running its kubeconfig's exec
	// credential plugin (EKS `aws eks get-token` / GKE gcloud). Honored ONLY on
	// an embedded BE; ignored (always stripped) on a hosted BE. Default false.
	AllowExecPlugins bool `json:"allowExecPlugins"`
	// PrometheusService pins the in-cluster Prometheus for historical pod
	// metrics, skipping auto-discovery. Format "namespace/service:port" where
	// port is a number or a named service port (e.g. "monitoring/prometheus-operated:web").
	// Empty = auto-discover from a short list of well-known services.
	PrometheusService string `json:"prometheusService"`
	// ReadSecretHosts lets the service map read the Secrets a workload
	// references, to extract host:port only (values never leave the BE).
	ReadSecretHosts bool `json:"readSecretHosts"`
}

func parseOptions(raw string) options {
	var o options
	if strings.TrimSpace(raw) == "" {
		return o
	}
	_ = json.Unmarshal([]byte(raw), &o)
	return o
}

// allowExec reports whether the exec credential plugin from this connection's
// kubeconfig may be kept intact. True ONLY when the BE is embedded (runs on the
// user's own machine) AND the connection opted in. On a hosted BE this is
// always false, so exec plugins are always stripped (RCE guard). This is the
// single decision point for the whole security model.
func (r *Registry) allowExec(conn models.K8sConnection) bool {
	return r.embedded && parseOptions(conn.OptionsJSON).AllowExecPlugins
}

// loadKubeconfig decrypts the stored kubeconfig and parses it into a
// clientcmdapi.Config WITHOUT contacting any cluster. It strips every authinfo
// exec block up-front (RCE guard) before the config is ever used.
func (r *Registry) loadKubeconfig(conn models.K8sConnection) (*clientcmdapi.Config, error) {
	if strings.TrimSpace(conn.KubeconfigEnc) == "" {
		return nil, fmt.Errorf("no kubeconfig configured")
	}
	raw, err := r.cipher.Decrypt(conn.KubeconfigEnc)
	if err != nil {
		return nil, fmt.Errorf("decrypt kubeconfig: %w", err)
	}
	cfg, err := clientcmd.Load([]byte(raw))
	if err != nil {
		return nil, fmt.Errorf("parse kubeconfig: %w", err)
	}
	// RCE guard: strip every authinfo exec block UNLESS this embedded BE has an
	// explicit per-connection opt-in (allowExec). A hosted BE always strips.
	if !r.allowExec(conn) {
		for k := range cfg.AuthInfos {
			if cfg.AuthInfos[k] != nil {
				cfg.AuthInfos[k].Exec = nil
			}
		}
	}
	return cfg, nil
}

// LoadKubeconfigForShell is the exported wrapper around loadKubeconfig used by
// the local-shell handler: it decrypts + parses the stored kubeconfig (no
// cluster call). Like every load path it strips authinfo exec blocks so the
// kubeconfig PatchMan writes to the temp file can never carry an exec
// credential plugin — UNLESS this is an embedded BE and the connection opted in
// (allowExec), in which case the exec block is kept so the local terminal's
// kubectl can authenticate to EKS/GKE via the user's local aws/gcloud. The
// local-shell handler itself is already gated embedded-only.
func (r *Registry) LoadKubeconfigForShell(conn models.K8sConnection) (*clientcmdapi.Config, error) {
	return r.loadKubeconfig(conn)
}

// kubeconfigUsesExecAuth reports whether any context's user in the parsed
// kubeconfig authenticates via an exec credential plugin. Call it on a config
// parsed BEFORE any exec stripping (e.g. the raw clientcmd.Load in
// ParseContexts) so the FE can be told a context needs exec auth. Cheap:
// a single map scan, no cluster call.
func kubeconfigUsesExecAuth(cfg *clientcmdapi.Config) bool {
	if cfg == nil {
		return false
	}
	for _, ai := range cfg.AuthInfos {
		if ai != nil && ai.Exec != nil {
			return true
		}
	}
	return false
}

// pooledKey builds the pool key for a connection/context pair.
func pooledKey(connID, contextName string) string {
	return connID + "|" + contextName
}

// resolve returns a pooled clientset + rest.Config for the connection/context,
// building it on first use. contextName "" selects the kubeconfig
// current-context (falling back to DefaultContext).
func (r *Registry) resolve(conn models.K8sConnection, contextName string) (*pooled, error) {
	ctxName := strings.TrimSpace(contextName)
	if ctxName == "" {
		ctxName = strings.TrimSpace(conn.DefaultContext)
	}

	if conn.ID != "" {
		key := pooledKey(conn.ID, ctxName)
		r.mu.Lock()
		if p, ok := r.pooled[key]; ok {
			p.lastUsed = time.Now()
			r.mu.Unlock()
			return p, nil
		}
		r.mu.Unlock()
	}

	cfg, err := r.loadKubeconfig(conn)
	if err != nil {
		return nil, err
	}
	restCfg, err := buildRESTConfig(cfg, ctxName, parseOptions(conn.OptionsJSON), r.allowExec(conn))
	if err != nil {
		return nil, err
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("build clientset: %w", err)
	}
	p := &pooled{clientset: clientset, rest: restCfg, lastUsed: time.Now()}
	if conn.ID != "" {
		r.mu.Lock()
		r.pooled[pooledKey(conn.ID, ctxName)] = p
		r.mu.Unlock()
	}
	return p, nil
}

// buildRESTConfig turns a parsed kubeconfig + context name into a *rest.Config
// and applies the insecureSkipVerify option. HARDENING: ExecProvider is forced
// nil so no exec credential plugin is ever invoked — UNLESS allowExec is true
// (embedded BE + per-connection opt-in), in which case the exec provider is
// left intact so client-go runs the aws/gcloud token helper locally.
func buildRESTConfig(cfg *clientcmdapi.Config, contextName string, opt options, allowExec bool) (*rest.Config, error) {
	overrides := &clientcmd.ConfigOverrides{}
	if strings.TrimSpace(contextName) != "" {
		overrides.CurrentContext = contextName
	}
	clientConfig := clientcmd.NewNonInteractiveClientConfig(*cfg, contextName, overrides, nil)
	restCfg, err := clientConfig.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("build rest config: %w", err)
	}
	// RCE guard: disable exec credential plugins on the transport unless the
	// caller explicitly allows them (embedded BE + per-connection opt-in).
	if !allowExec {
		restCfg.ExecProvider = nil
	}
	if opt.InsecureSkipVerify {
		restCfg.TLSClientConfig.Insecure = true
		restCfg.TLSClientConfig.CAData = nil
		restCfg.TLSClientConfig.CAFile = ""
	}
	restCfg.UserAgent = "patchman-k8s/1.0"
	return restCfg, nil
}

// Test probes reachability + auth via the Discovery ServerVersion endpoint.
func (r *Registry) Test(ctx context.Context, conn models.K8sConnection, contextName string) (*ClusterVersion, error) {
	p, err := r.resolve(conn, contextName)
	if err != nil {
		return nil, err
	}
	// The discovery client does not accept a context; guard with a goroutine so
	// the caller's timeout still bounds the call.
	type res struct {
		v   *ClusterVersion
		err error
	}
	ch := make(chan res, 1)
	go func() {
		info, err := p.clientset.Discovery().ServerVersion()
		if err != nil {
			ch <- res{err: err}
			return
		}
		ch <- res{v: &ClusterVersion{
			Version:   info.GitVersion,
			Platform:  info.Platform,
			Reachable: true,
		}}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case out := <-ch:
		return out.v, out.err
	}
}

// Contexts parses the stored kubeconfig into a context list with NO cluster
// call.
func (r *Registry) Contexts(conn models.K8sConnection) ([]ContextInfo, error) {
	cfg, err := r.loadKubeconfig(conn)
	if err != nil {
		return nil, err
	}
	list, _ := contextsFromConfig(cfg)
	return list, nil
}

// ParseContexts parses a PLAINTEXT kubeconfig (no cluster call) into its context
// list + the kubeconfig current-context. Used by the connection handler to cache
// the non-secret context list at create/update time. It strips exec blocks like
// every other load path.
func ParseContexts(rawKubeconfig string) ([]ContextInfo, string, error) {
	if strings.TrimSpace(rawKubeconfig) == "" {
		return nil, "", fmt.Errorf("empty kubeconfig")
	}
	cfg, err := clientcmd.Load([]byte(rawKubeconfig))
	if err != nil {
		return nil, "", fmt.Errorf("parse kubeconfig: %w", err)
	}
	for k := range cfg.AuthInfos {
		if cfg.AuthInfos[k] != nil {
			cfg.AuthInfos[k].Exec = nil
		}
	}
	list, current := contextsFromConfig(cfg)
	return list, current, nil
}

// contextsFromConfig builds a sorted ContextInfo list + current-context from a
// parsed kubeconfig.
func contextsFromConfig(cfg *clientcmdapi.Config) ([]ContextInfo, string) {
	out := make([]ContextInfo, 0, len(cfg.Contexts))
	for name, kctx := range cfg.Contexts {
		if kctx == nil {
			continue
		}
		info := ContextInfo{Name: name, Cluster: kctx.Cluster, Namespace: kctx.Namespace}
		if cl := cfg.Clusters[kctx.Cluster]; cl != nil {
			info.Server = cl.Server
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, cfg.CurrentContext
}

// Close discards every pooled clientset for a connection (config edit / delete).
// It also purges the Prometheus discovery cache for the connection so a freshly
// set/changed `prometheusService` override (or a moved Prometheus) is picked up
// immediately instead of being shadowed by a stale hit/miss until the TTL.
func (r *Registry) Close(connectionID string) {
	prefix := connectionID + "|"
	r.mu.Lock()
	for key := range r.pooled {
		if strings.HasPrefix(key, prefix) {
			delete(r.pooled, key)
		}
	}
	r.mu.Unlock()

	r.promMu.Lock()
	for key := range r.promCache {
		if strings.HasPrefix(key, prefix) {
			delete(r.promCache, key)
		}
	}
	r.promMu.Unlock()
}

func (r *Registry) reapLoop() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		cutoff := time.Now().Add(-clientIdleTimeout)
		r.mu.Lock()
		for key, p := range r.pooled {
			if p.lastUsed.Before(cutoff) {
				delete(r.pooled, key)
			}
		}
		r.mu.Unlock()
	}
}
