package k8sclient

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/servicemap"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const maxReferencedObjects = 400

// ServiceMapInput reads what the service map's config layer needs from one
// cluster: workloads, Services, Ingresses, Pods, and only the ConfigMaps
// (and, when the connection opted in, Secrets) that workloads reference.
// A kind the kubeconfig may not list becomes a note, not a failure.
func (r *Registry) ServiceMapInput(ctx context.Context, conn models.K8sConnection, contextName string, namespaces []string) (servicemap.ClusterInput, []string, error) {
	var in servicemap.ClusterInput
	opt := parseOptions(conn.OptionsJSON)
	if len(namespaces) == 0 {
		namespaces = append(namespaces, opt.AllowedNamespaces...)
	}
	if len(namespaces) == 0 {
		namespaces = []string{""} // all namespaces
	}
	for _, ns := range namespaces {
		if ns == "" && len(opt.AllowedNamespaces) > 0 {
			return in, nil, fmt.Errorf("pick a namespace (allowed: %s)", strings.Join(opt.AllowedNamespaces, ", "))
		}
		if err := checkNamespace(conn, "pods", ns); err != nil {
			return in, nil, err
		}
	}
	p, err := r.resolve(conn, contextName)
	if err != nil {
		return in, nil, err
	}
	cs := p.clientset
	var (
		mu    sync.Mutex
		notes []string
		wg    sync.WaitGroup
		hard  error
	)
	fail := func(kind string, err error) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case apierrors.IsForbidden(err):
			notes = append(notes, "forbidden:"+kind)
		case apierrors.IsNotFound(err):
		default:
			if hard == nil {
				hard = err
			}
		}
	}
	lo := metav1.ListOptions{}
	for _, ns := range namespaces {
		ns := ns
		run := func(kind string, f func() error) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := f(); err != nil {
					fail(kind, err)
				}
			}()
		}
		run("deployments", func() error {
			l, err := cs.AppsV1().Deployments(ns).List(ctx, lo)
			if err == nil {
				mu.Lock()
				in.Deployments = append(in.Deployments, l.Items...)
				mu.Unlock()
			}
			return err
		})
		run("statefulsets", func() error {
			l, err := cs.AppsV1().StatefulSets(ns).List(ctx, lo)
			if err == nil {
				mu.Lock()
				in.StatefulSets = append(in.StatefulSets, l.Items...)
				mu.Unlock()
			}
			return err
		})
		run("daemonsets", func() error {
			l, err := cs.AppsV1().DaemonSets(ns).List(ctx, lo)
			if err == nil {
				mu.Lock()
				in.DaemonSets = append(in.DaemonSets, l.Items...)
				mu.Unlock()
			}
			return err
		})
		run("cronjobs", func() error {
			l, err := cs.BatchV1().CronJobs(ns).List(ctx, lo)
			if err == nil {
				mu.Lock()
				in.CronJobs = append(in.CronJobs, l.Items...)
				mu.Unlock()
			}
			return err
		})
		run("services", func() error {
			l, err := cs.CoreV1().Services(ns).List(ctx, lo)
			if err == nil {
				mu.Lock()
				in.Services = append(in.Services, l.Items...)
				mu.Unlock()
			}
			return err
		})
		run("ingresses", func() error {
			l, err := cs.NetworkingV1().Ingresses(ns).List(ctx, lo)
			if err == nil {
				mu.Lock()
				in.Ingresses = append(in.Ingresses, l.Items...)
				mu.Unlock()
			}
			return err
		})
		run("pods", func() error {
			l, err := cs.CoreV1().Pods(ns).List(ctx, lo)
			if err == nil {
				mu.Lock()
				for _, p := range l.Items {
					// Only what the resolvers need: no specs, no env.
					in.Pods = append(in.Pods, corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{Namespace: p.Namespace, Name: p.Name, Labels: p.Labels},
						Spec:       corev1.PodSpec{HostNetwork: p.Spec.HostNetwork},
						Status:     corev1.PodStatus{PodIP: p.Status.PodIP},
					})
				}
				mu.Unlock()
			}
			return err
		})
	}
	wg.Wait()
	if hard != nil {
		return in, notes, hard
	}

	// Referenced ConfigMaps / Secrets only — never a cluster-wide dump.
	cms, secs := referencedObjects(in)
	in.ConfigMaps = map[string]*corev1.ConfigMap{}
	getEach := func(kind string, keys []string, get func(ns, name string) error) {
		if len(keys) > maxReferencedObjects {
			mu.Lock()
			notes = append(notes, "references_capped:"+strconv.Itoa(maxReferencedObjects))
			mu.Unlock()
			keys = keys[:maxReferencedObjects]
		}
		sem := make(chan struct{}, 8)
		for _, k := range keys {
			ns, name, _ := strings.Cut(k, "/")
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				if err := get(ns, name); err != nil && apierrors.IsForbidden(err) {
					mu.Lock()
					notes = appendOnce(notes, "forbidden:"+kind)
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
	}
	getEach("configmaps", cms, func(ns, name string) error {
		cm, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			mu.Lock()
			in.ConfigMaps[ns+"/"+name] = &corev1.ConfigMap{Data: cm.Data}
			mu.Unlock()
		}
		return err
	})
	if opt.ReadSecretHosts {
		in.Secrets = map[string]*corev1.Secret{}
		getEach("secrets", secs, func(ns, name string) error {
			s, err := cs.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
			if err == nil {
				mu.Lock()
				in.Secrets[ns+"/"+name] = &corev1.Secret{Data: s.Data}
				mu.Unlock()
			}
			return err
		})
	}
	sort.Strings(notes)
	return in, notes, nil
}

func appendOnce(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}

// referencedObjects lists the "ns/name" ConfigMaps and Secrets that
// workload pod templates reference.
func referencedObjects(in servicemap.ClusterInput) (cms, secs []string) {
	cm, sec := map[string]bool{}, map[string]bool{}
	scan := func(ns string, spec corev1.PodSpec) {
		for _, c := range append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...) {
			for _, e := range c.Env {
				if e.ValueFrom != nil && e.ValueFrom.ConfigMapKeyRef != nil {
					cm[ns+"/"+e.ValueFrom.ConfigMapKeyRef.Name] = true
				}
				if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
					sec[ns+"/"+e.ValueFrom.SecretKeyRef.Name] = true
				}
			}
			for _, ef := range c.EnvFrom {
				if ef.ConfigMapRef != nil {
					cm[ns+"/"+ef.ConfigMapRef.Name] = true
				}
				if ef.SecretRef != nil {
					sec[ns+"/"+ef.SecretRef.Name] = true
				}
			}
		}
		for _, v := range spec.Volumes {
			if v.ConfigMap != nil {
				cm[ns+"/"+v.ConfigMap.Name] = true
			}
			if v.Secret != nil {
				sec[ns+"/"+v.Secret.SecretName] = true
			}
			if v.Projected != nil {
				for _, ps := range v.Projected.Sources {
					if ps.ConfigMap != nil {
						cm[ns+"/"+ps.ConfigMap.Name] = true
					}
					if ps.Secret != nil {
						sec[ns+"/"+ps.Secret.Name] = true
					}
				}
			}
		}
	}
	for _, d := range in.Deployments {
		scan(d.Namespace, d.Spec.Template.Spec)
	}
	for _, s := range in.StatefulSets {
		scan(s.Namespace, s.Spec.Template.Spec)
	}
	for _, d := range in.DaemonSets {
		scan(d.Namespace, d.Spec.Template.Spec)
	}
	for _, c := range in.CronJobs {
		scan(c.Namespace, c.Spec.JobTemplate.Spec.Template.Spec)
	}
	for k := range cm {
		cms = append(cms, k)
	}
	for k := range sec {
		secs = append(secs, k)
	}
	sort.Strings(cms)
	sort.Strings(secs)
	return cms, secs
}

// clusterProm queries the cluster's own Prometheus through the apiserver
// service proxy (same discovery as the pod-metrics history).
type clusterProm struct {
	p   *pooled
	tgt promTarget
}

func (q clusterProm) Query(ctx context.Context, expr string, at time.Time) ([]servicemap.Sample, error) {
	raw, err := q.p.clientset.CoreV1().RESTClient().Get().
		AbsPath("api", "v1", "namespaces", q.tgt.Namespace, "services", q.tgt.Service+":"+q.tgt.Port, "proxy", "api", "v1", "query").
		Param("query", expr).Param("time", strconv.FormatInt(at.Unix(), 10)).
		DoRaw(ctx)
	if err != nil {
		return nil, promHTTPErr(err)
	}
	var resp promResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("prometheus answer was not JSON")
	}
	if resp.Status != "success" {
		return nil, fmt.Errorf("prometheus: %s", resp.Error)
	}
	out := make([]servicemap.Sample, 0, len(resp.Data.Result))
	for _, s := range resp.Data.Result {
		if pt, ok := parsePromValue(s.Value); ok {
			out = append(out, servicemap.Sample{Metric: s.Metric, Value: pt.Val})
		}
	}
	return out, nil
}

// ClusterPrometheus returns a querier for the cluster's own Prometheus, or
// found=false when none is discoverable.
func (r *Registry) ClusterPrometheus(ctx context.Context, conn models.K8sConnection, contextName string) (servicemap.PromQuerier, string, bool, error) {
	p, err := r.resolve(conn, contextName)
	if err != nil {
		return nil, "", false, err
	}
	tgt, found, err := r.resolveProm(ctx, conn, contextName, p)
	if err != nil || !found {
		return nil, "", false, err
	}
	return clusterProm{p: p, tgt: tgt}, tgt.String(), true, nil
}
