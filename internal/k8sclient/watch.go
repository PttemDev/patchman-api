package k8sclient

import (
	"context"
	"fmt"
	"strings"

	"patchman-be/internal/models"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
)

// Watch starts a typed client-go watch for kind/namespace resuming from
// resourceVersion rv, translates watch.Added/Modified/Deleted into a normalized
// ResourceRow via the SAME per-kind builder List uses (so a watched row is
// byte-identical to a listed one), and invokes emit for each. evType is the
// lowercased event kind: "added", "modified", or "deleted".
//
// It follows the logs.go teardown contract: it returns on ctx cancel (the SSE
// clientGone), on watch.Error (e.g. 410 Gone → the FE re-lists), and on channel
// close — so the handler can end the stream and the FE re-subscribes. Bookmark
// events are consumed silently to keep the connection's resourceVersion fresh.
func (r *Registry) Watch(ctx context.Context, conn models.K8sConnection, contextName, namespace, kind, rv string, emit func(evType string, row ResourceRow)) error {
	k := normalizeKind(kind)
	if err := checkNamespace(conn, k, namespace); err != nil {
		return err
	}
	p, err := r.resolve(conn, contextName)
	if err != nil {
		return err
	}
	cs := p.clientset
	lo := metav1.ListOptions{
		Watch:               true,
		ResourceVersion:     rv,
		AllowWatchBookmarks: true,
	}

	var w watch.Interface
	switch k {
	case "pods":
		w, err = cs.CoreV1().Pods(namespace).Watch(ctx, lo)
	case "deployments":
		w, err = cs.AppsV1().Deployments(namespace).Watch(ctx, lo)
	case "statefulsets":
		w, err = cs.AppsV1().StatefulSets(namespace).Watch(ctx, lo)
	case "daemonsets":
		w, err = cs.AppsV1().DaemonSets(namespace).Watch(ctx, lo)
	case "jobs":
		w, err = cs.BatchV1().Jobs(namespace).Watch(ctx, lo)
	case "cronjobs":
		w, err = cs.BatchV1().CronJobs(namespace).Watch(ctx, lo)
	case "services":
		w, err = cs.CoreV1().Services(namespace).Watch(ctx, lo)
	case "configmaps":
		w, err = cs.CoreV1().ConfigMaps(namespace).Watch(ctx, lo)
	case "secrets":
		w, err = cs.CoreV1().Secrets(namespace).Watch(ctx, lo)
	case "ingresses":
		w, err = cs.NetworkingV1().Ingresses(namespace).Watch(ctx, lo)
	case "nodes":
		w, err = cs.CoreV1().Nodes().Watch(ctx, lo)
	case "namespaces":
		w, err = cs.CoreV1().Namespaces().Watch(ctx, lo)
	case "events":
		w, err = cs.CoreV1().Events(namespace).Watch(ctx, lo)
	default:
		return fmt.Errorf("unsupported kind %q", kind)
	}
	if err != nil {
		return err
	}
	defer w.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.ResultChan():
			if !ok {
				// Channel closed — apiserver ended the watch (idle timeout, etc.).
				return nil
			}
			switch ev.Type {
			case watch.Added, watch.Modified, watch.Deleted:
				row, ok := rowFromObject(k, ev.Object)
				if !ok {
					continue
				}
				emit(strings.ToLower(string(ev.Type)), row)
			case watch.Bookmark:
				// resourceVersion-only heartbeat; nothing to emit.
			case watch.Error:
				// Typically a 410 Gone Status — end the stream so the FE re-lists.
				return fmt.Errorf("watch error: %v", ev.Object)
			}
		}
	}
}

// rowFromObject normalizes a watch event object into a ResourceRow using the
// exact per-kind builder List uses. The kind argument (already lowercased)
// matches the object type; the type switch is the authority and returns false
// for anything unexpected (e.g. a *metav1.Status leaking through).
func rowFromObject(kind string, obj runtime.Object) (ResourceRow, bool) {
	switch o := obj.(type) {
	case *corev1.Pod:
		return podRow(o), true
	case *appsv1.Deployment:
		return deploymentRow(o), true
	case *appsv1.StatefulSet:
		return statefulSetRow(o), true
	case *appsv1.DaemonSet:
		return daemonSetRow(o), true
	case *batchv1.Job:
		return jobRow(o), true
	case *batchv1.CronJob:
		return cronJobRow(o), true
	case *corev1.Service:
		return serviceRow(o), true
	case *corev1.ConfigMap:
		return configMapRow(o), true
	case *corev1.Secret:
		return secretRow(o), true
	case *networkingv1.Ingress:
		return ingressRow(o), true
	case *corev1.Node:
		return nodeRow(o), true
	case *corev1.Namespace:
		return namespaceRow(o), true
	case *corev1.Event:
		return eventRow(o), true
	}
	return ResourceRow{}, false
}
