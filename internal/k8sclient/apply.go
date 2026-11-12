package k8sclient

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"patchman-be/internal/models"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"
)

// fieldManager is the server-side-apply field manager PatchMan claims. Every
// apply the tool performs is attributed to this manager so conflicts against
// other managers (kubectl, controllers) are reported honestly.
const fieldManager = "patchman"

// Apply performs a Server-Side Apply of a single YAML document. It decodes the
// YAML → unstructured, maps its GVK to a GVR via the discovery-backed REST
// mapper, fetches the current object (for the diff), then issues an
// apply-patch with FieldManager=patchman + FieldValidation=Strict. When
// dryRun is true nothing is mutated. When force is false a field-ownership
// conflict surfaces as a 409 whose causes we parse into ApplyResult.Conflicts.
//
// We deliberately use the raw apply-patch (types.ApplyPatchType) rather than
// client-go/applyconfigurations — the typed apply-config packages pull a huge
// amount of generated code into the binary for no functional gain here.
func (r *Registry) Apply(ctx context.Context, conn models.K8sConnection, contextName string, yamlDoc []byte, dryRun, force bool) (*ApplyResult, error) {
	if strings.TrimSpace(string(yamlDoc)) == "" {
		return nil, fmt.Errorf("empty document")
	}

	// YAML → JSON → unstructured (also gives us the GVK).
	jsonBytes, err := yaml.YAMLToJSON(yamlDoc)
	if err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	obj := &unstructured.Unstructured{}
	if err := obj.UnmarshalJSON(jsonBytes); err != nil {
		return nil, fmt.Errorf("decode object: %w", err)
	}
	gvk := obj.GroupVersionKind()
	if gvk.Kind == "" || gvk.Version == "" {
		return nil, fmt.Errorf("document is missing apiVersion/kind")
	}
	name := obj.GetName()
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("document is missing metadata.name")
	}

	_, dyn, mapper, err := r.resolveDynamic(conn, contextName)
	if err != nil {
		return nil, err
	}

	mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, fmt.Errorf("map %s: %w", gvk.String(), err)
	}
	gvr := mapping.Resource

	// Namespace: only for namespaced kinds. The object's own namespace wins;
	// fall back to the connection default is intentionally NOT done here —
	// server-side apply requires the namespace be explicit in the document.
	ns := obj.GetNamespace()
	namespaced := mapping.Scope.Name() == "namespace"
	if namespaced {
		if err := checkNamespace(conn, "generic", ns); err != nil {
			// checkNamespace treats "generic" as namespaced; enforce allowlist.
			return nil, err
		}
	}

	ri := resourceClient(dyn, gvr, namespaced, ns)

	// Current object (best effort) for a readable diff. A NotFound means this
	// apply creates the object — diff renders as a create.
	var currentJSON []byte
	if cur, gerr := ri.Get(ctx, name, metav1.GetOptions{}); gerr == nil && cur != nil {
		currentJSON, _ = cur.MarshalJSON()
	}

	patchOpts := metav1.PatchOptions{
		FieldManager:    fieldManager,
		Force:           &force,
		FieldValidation: metav1.FieldValidationStrict,
	}
	if dryRun {
		patchOpts.DryRun = []string{metav1.DryRunAll}
	}

	applied, err := ri.Patch(ctx, name, types.ApplyPatchType, jsonBytes, patchOpts)
	if err != nil {
		if conflicts := parseConflicts(err); len(conflicts) > 0 {
			return &ApplyResult{Conflicts: conflicts, DryRun: dryRun}, nil
		}
		return nil, err
	}

	appliedJSON, _ := applied.MarshalJSON()
	return &ApplyResult{
		Applied: json.RawMessage(appliedJSON),
		Diff:    diffJSON(currentJSON, appliedJSON),
		DryRun:  dryRun,
	}, nil
}

// parseConflicts extracts the conflicting field managers/paths from a 409
// StatusError returned by an apply-patch with force=false. Returns nil when the
// error is not a conflict.
func parseConflicts(err error) []string {
	var se *apierrors.StatusError
	if !apierrors.IsConflict(err) {
		return nil
	}
	if !asStatusError(err, &se) || se.ErrStatus.Details == nil {
		// A conflict with no structured causes still deserves a line.
		return []string{err.Error()}
	}
	out := make([]string, 0, len(se.ErrStatus.Details.Causes))
	for _, cause := range se.ErrStatus.Details.Causes {
		msg := cause.Message
		if cause.Field != "" {
			msg = cause.Field + ": " + msg
		}
		out = append(out, msg)
	}
	if len(out) == 0 {
		out = append(out, se.ErrStatus.Message)
	}
	return out
}

// asStatusError unwraps err into *apierrors.StatusError.
func asStatusError(err error, target **apierrors.StatusError) bool {
	if se, ok := err.(*apierrors.StatusError); ok {
		*target = se
		return true
	}
	return false
}

// diffJSON renders a compact, readable field-level diff between two JSON
// objects (current → applied). It walks the flattened dotted-path leaf set of
// both sides and emits +/-/~ lines. metadata bookkeeping fields that always
// churn on apply (resourceVersion, managedFields, generation, timestamps) are
// suppressed so the diff shows intent, not noise.
func diffJSON(current, applied []byte) string {
	if len(current) == 0 {
		return "(new object — created by this apply)"
	}
	curFlat := map[string]string{}
	appFlat := map[string]string{}
	flatten("", decodeAny(current), curFlat)
	flatten("", decodeAny(applied), appFlat)

	keys := map[string]struct{}{}
	for k := range curFlat {
		keys[k] = struct{}{}
	}
	for k := range appFlat {
		keys[k] = struct{}{}
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		if isNoiseField(k) {
			continue
		}
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	var b strings.Builder
	for _, k := range sorted {
		cv, cok := curFlat[k]
		av, aok := appFlat[k]
		switch {
		case cok && aok && cv != av:
			fmt.Fprintf(&b, "~ %s: %s -> %s\n", k, cv, av)
		case !cok && aok:
			fmt.Fprintf(&b, "+ %s: %s\n", k, av)
		case cok && !aok:
			fmt.Fprintf(&b, "- %s: %s\n", k, cv)
		}
	}
	if b.Len() == 0 {
		return "(no field-level changes)"
	}
	return b.String()
}

// isNoiseField reports whether a dotted path is server-managed bookkeeping that
// always churns and should be hidden from the intent diff.
func isNoiseField(path string) bool {
	noisy := []string{
		"metadata.resourceVersion",
		"metadata.generation",
		"metadata.managedFields",
		"metadata.creationTimestamp",
		"metadata.uid",
		"status.",
	}
	for _, n := range noisy {
		if path == strings.TrimSuffix(n, ".") || strings.HasPrefix(path, n) {
			return true
		}
	}
	return false
}

func decodeAny(raw []byte) any {
	var v any
	_ = json.Unmarshal(raw, &v)
	return v
}

// flatten walks a decoded JSON value into dotted-path → scalar-string leaves.
func flatten(prefix string, v any, out map[string]string) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			key := k
			if prefix != "" {
				key = prefix + "." + k
			}
			flatten(key, val, out)
		}
	case []any:
		for i, val := range t {
			key := fmt.Sprintf("%s[%d]", prefix, i)
			flatten(key, val, out)
		}
	default:
		out[prefix] = fmt.Sprintf("%v", t)
	}
}
