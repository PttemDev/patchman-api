package servicemap

import (
	"encoding/json"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestExtractEndpoints(t *testing.T) {
	cases := []struct {
		key, val string
		want     []string
	}{
		{"DATABASE_URL", "postgres://shop:s3cret@postgres.shop.svc.cluster.local:5432/shop?sslmode=disable", []string{"postgres://postgres.shop.svc.cluster.local:5432"}},
		{"JDBC", "jdbc:postgresql://db.example.com/orders", []string{"postgres://db.example.com:5432"}},
		{"MONGO", "mongodb://u:p@m1:27017,m2:27018/app", []string{"mongodb://m1:27017", "mongodb://m2:27018"}},
		{"KAFKA_BOOTSTRAP_SERVERS", "kafka-0.kafka:9092,kafka-1.kafka:9092", []string{"kafka-0.kafka:9092", "kafka-1.kafka:9092"}},
		{"REDIS_HOST", "redis:6379", []string{"redis:6379"}},
		{"AMQP_URL", "amqps://guest:guest@rabbitmq", []string{"amqp://rabbitmq:5672"}},
		{"API_URL", "http://api.shop.svc.cluster.local:8080/v1", []string{"http://api.shop.svc.cluster.local:8080"}},
		{"CACHE_ADDR", "[fd00::5]:6379", nil},
		{"START_TIME", "10:30", nil},
		{"BIND", "0.0.0.0:8080", nil},
		{"LOCAL", "http://localhost:3000", nil},
		{"FEATURE", "true", nil},
	}
	for _, c := range cases {
		eps := ExtractEndpoints(c.key, c.val, nil)
		var got []string
		for _, e := range eps {
			got = append(got, e.String())
		}
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%s=%q → %v, want %v", c.key, c.val, got, c.want)
		}
	}
	// A bare host in a host key, with the sibling port.
	eps := ExtractEndpoints("PG_HOST", "postgres", func(p string) int {
		if p == "PG" {
			return 5433
		}
		return 0
	})
	if len(eps) != 1 || eps[0].String() != "postgres://postgres:5433" {
		t.Errorf("bare host: %v", eps)
	}
	if eps := ExtractEndpoints("APP_NAME", "orders", nil); len(eps) != 0 {
		t.Errorf("non-host key: %v", eps)
	}
}

func i32(v int32) *int32 { return &v }

func deploy(ns, name string, lbl map[string]string, image string, env []corev1.EnvVar, envFrom []corev1.EnvFromSource) appsv1.Deployment {
	return appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: appsv1.DeploymentSpec{Replicas: i32(2), Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: lbl},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: image, Env: env, EnvFrom: envFrom}}},
		}},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 2},
	}
}

func svc(ns, name string, sel map[string]string, ip string, port int32) corev1.Service {
	return corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: corev1.ServiceSpec{Selector: sel, ClusterIP: ip, Ports: []corev1.ServicePort{{Port: port}}}}
}

func shopInput(withSecrets bool) ClusterInput {
	pg := appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "postgres"},
		Spec: appsv1.StatefulSetSpec{Replicas: i32(1), Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "postgres"}},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "pg", Image: "postgres:16-alpine"}}},
		}},
	}
	in := ClusterInput{
		StatefulSets: []appsv1.StatefulSet{pg},
		Deployments: []appsv1.Deployment{
			deploy("shop", "api", map[string]string{"app": "api"}, "ghcr.io/x/api:1",
				[]corev1.EnvVar{
					{Name: "REDIS_HOST", Value: "redis:6379"},
					{Name: "DATABASE_URL", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "api-db"}, Key: "DATABASE_URL"}}},
					{Name: "PAYMENTS", Value: "https://payments.example.com/v2"},
				}, nil),
			deploy("shop", "web", map[string]string{"app": "web"}, "nginx:1", nil,
				[]corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "web-config"}}}}),
			deploy("shop", "redis", map[string]string{"app": "redis"}, "redis:7", nil, nil),
		},
		Services: []corev1.Service{
			svc("shop", "api", map[string]string{"app": "api"}, "10.96.0.10", 8080),
			svc("shop", "web", map[string]string{"app": "web"}, "10.96.0.11", 80),
			svc("shop", "redis", map[string]string{"app": "redis"}, "10.96.0.12", 6379),
			svc("shop", "postgres", map[string]string{"app": "postgres"}, "None", 5432),
		},
		Ingresses: []networkingv1.Ingress{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"},
			Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{Host: "shop.example.com",
				IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{
					Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "web"}}}}}}}}},
		}},
		ConfigMaps: map[string]*corev1.ConfigMap{
			"shop/web-config": {Data: map[string]string{"API_URL": "http://10.96.0.10:8080"}},
		},
	}
	if withSecrets {
		in.Secrets = map[string]*corev1.Secret{
			"shop/api-db": {Data: map[string][]byte{"DATABASE_URL": []byte("postgres://shop:TopSecretPW@postgres-0.postgres.shop.svc.cluster.local:5432/shop")}},
		}
	}
	return in
}

func edgeSet(g *Graph) map[string]*Edge {
	m := map[string]*Edge{}
	for _, e := range g.Edges {
		m[e.From+" > "+e.To] = e
	}
	return m
}

func TestBuildConfig(t *testing.T) {
	b := NewBuilder()
	st := &SourceStatus{OK: true}
	BuildConfig(b, shopInput(true), st)
	g := b.Graph(map[string]*SourceStatus{"config": st})
	edges := edgeSet(g)
	want := map[string]string{
		"ingress:shop/web > shop/Deployment/web":             "http",
		"shop/Deployment/web > shop/Deployment/api":          "http",     // ConfigMap, by ClusterIP
		"shop/Deployment/api > shop/Deployment/redis":        "redis",    // plain env, short svc name
		"shop/Deployment/api > shop/StatefulSet/postgres":    "postgres", // Secret, headless pod DNS
		"shop/Deployment/api > ext:payments.example.com:443": "http",
	}
	for k, proto := range want {
		e, ok := edges[k]
		if !ok {
			t.Errorf("missing edge %s (have %v)", k, keys(edges))
			continue
		}
		if e.Protocol != proto {
			t.Errorf("%s protocol %q, want %q", k, e.Protocol, proto)
		}
	}
	if len(edges) != len(want) {
		t.Errorf("edges %v", keys(edges))
	}
	if n := b.Node("shop/StatefulSet/postgres"); n == nil || n.Kind != KindDatastore || n.Subtype != "postgres" {
		t.Errorf("postgres node %+v", n)
	}
	if n := b.Node("shop/Deployment/redis"); n == nil || n.Kind != KindDatastore || n.Subtype != "redis" {
		t.Errorf("redis node %+v", n)
	}
	if n := b.Node("shop/Deployment/api"); n == nil || n.Kind != KindService || len(n.Services) != 1 || n.Replicas != "2/2" {
		t.Errorf("api node %+v", n)
	}
	ev := edges["shop/Deployment/api > shop/StatefulSet/postgres"].Evidence
	if len(ev) != 1 || ev[0].Source != "secret" || ev[0].Name != "api-db" || ev[0].Key != "DATABASE_URL" ||
		ev[0].Extracted != "postgres://postgres-0.postgres.shop.svc.cluster.local:5432" {
		t.Errorf("secret evidence %+v", ev)
	}

	// The invariant: no value, password or userinfo anywhere in the output.
	raw, _ := json.Marshal(g)
	for _, leak := range []string{"TopSecretPW", "shop:Top", "sslmode", "/v2"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("graph leaks %q", leak)
		}
	}
}

func TestMountedCodeIsNotConfig(t *testing.T) {
	in := shopInput(false)
	in.Deployments[1].Spec.Template.Spec.Volumes = []corev1.Volume{{Name: "code", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "code"}}}}}
	in.ConfigMaps["shop/code"] = &corev1.ConfigMap{Data: map[string]string{
		"api.py":          "r = redis.Redis(host='redis', port=6379)",
		"application.yml": "spring:\n  datasource:\n    url: jdbc:postgresql://postgres.shop.svc.cluster.local:5432/shop",
	}}
	b := NewBuilder()
	BuildConfig(b, in, &SourceStatus{OK: true})
	edges := edgeSet(b.Graph(nil))
	if _, ok := edges["shop/Deployment/web > shop/Deployment/redis"]; ok {
		t.Error("source code in a mounted ConfigMap must not create edges")
	}
	if _, ok := edges["shop/Deployment/web > shop/StatefulSet/postgres"]; !ok {
		t.Errorf("mounted application.yml should: %v", keys(edges))
	}
}

func TestBuildConfigWithoutSecrets(t *testing.T) {
	b := NewBuilder()
	st := &SourceStatus{OK: true}
	BuildConfig(b, shopInput(false), st)
	edges := edgeSet(b.Graph(nil))
	if _, ok := edges["shop/Deployment/api > shop/StatefulSet/postgres"]; ok {
		t.Error("secret-only edge must not appear when secrets are not read")
	}
	if len(st.Notes) != 1 || st.Notes[0] != "secrets_skipped:1" {
		t.Errorf("notes %v", st.Notes)
	}
}

func keys(m map[string]*Edge) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
