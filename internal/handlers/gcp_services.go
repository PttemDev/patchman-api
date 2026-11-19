package handlers

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"patchman-be/internal/gcp"
	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ── Cloud Storage ───────────────────────────────────────────────────────────

func (h *GCPHandler) Buckets(c *gin.Context) {
	cl, row, ok := h.client(c, gcp.ScopeStorageRead)
	if !ok {
		return
	}
	project, ok := gcpProject(c, row)
	if !ok {
		return
	}
	var out struct {
		Items []struct {
			Name         string `json:"name"`
			Location     string `json:"location"`
			StorageClass string `json:"storageClass"`
			TimeCreated  string `json:"timeCreated"`
		} `json:"items"`
	}
	if err := cl.Do(c.Request.Context(), "GET", gcp.StorageBase()+"/b", url.Values{"project": {project}, "maxResults": {"1000"}}, nil, &out); err != nil {
		gcpErr(c, err)
		return
	}
	list := make([]gin.H, 0, len(out.Items))
	for _, b := range out.Items {
		list = append(list, gin.H{"name": b.Name, "location": b.Location, "storageClass": b.StorageClass, "createdAt": b.TimeCreated})
	}
	c.JSON(http.StatusOK, gin.H{"data": list})
}

type gcsObject struct {
	Name         string `json:"name"`
	Size         string `json:"size"`
	Updated      string `json:"updated"`
	ContentType  string `json:"contentType"`
	StorageClass string `json:"storageClass"`
	MD5Hash      string `json:"md5Hash"`
	Generation   string `json:"generation"`
}

func (o gcsObject) dto() gin.H {
	n, _ := strconv.ParseInt(o.Size, 10, 64)
	return gin.H{"name": o.Name, "size": n, "updated": o.Updated, "contentType": o.ContentType, "storageClass": o.StorageClass, "md5": o.MD5Hash}
}

// Objects lists one "folder" level: prefixes + objects (delimiter /).
func (h *GCPHandler) Objects(c *gin.Context) {
	cl, _, ok := h.client(c, gcp.ScopeStorageRead)
	if !ok {
		return
	}
	bucket := c.Query("bucket")
	if !nameOK(c, "bucket", bucket) {
		return
	}
	q := url.Values{"delimiter": {"/"}, "maxResults": {"200"}}
	if p := c.Query("prefix"); p != "" {
		q.Set("prefix", p)
	}
	if t := c.Query("pageToken"); t != "" {
		q.Set("pageToken", t)
	}
	var out struct {
		Items         []gcsObject `json:"items"`
		Prefixes      []string    `json:"prefixes"`
		NextPageToken string      `json:"nextPageToken"`
	}
	if err := cl.Do(c.Request.Context(), "GET", gcp.StorageBase()+"/b/"+url.PathEscape(bucket)+"/o", q, nil, &out); err != nil {
		gcpErr(c, err)
		return
	}
	objs := make([]gin.H, 0, len(out.Items))
	for _, o := range out.Items {
		objs = append(objs, o.dto())
	}
	if out.Prefixes == nil {
		out.Prefixes = []string{}
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"prefixes": out.Prefixes, "objects": objs, "nextPageToken": out.NextPageToken}})
}

const gcsPreviewMax = 5 << 20

// Object — GET: metadata + content (base64, up to 5 MB; ?download=1 streams
// it as a file).
func (h *GCPHandler) Object(c *gin.Context) {
	cl, _, ok := h.client(c, gcp.ScopeStorageRead)
	if !ok {
		return
	}
	bucket, name := c.Query("bucket"), c.Query("name")
	if !nameOK(c, "bucket", bucket) || strings.TrimSpace(name) == "" {
		if !c.Writer.Written() {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		}
		return
	}
	p := gcp.StorageBase() + "/b/" + url.PathEscape(bucket) + "/o/" + url.PathEscape(name)
	var meta gcsObject
	if err := cl.Do(c.Request.Context(), "GET", p, nil, nil, &meta); err != nil {
		gcpErr(c, err)
		return
	}
	size, _ := strconv.ParseInt(meta.Size, 10, 64)
	if c.Query("download") == "1" {
		raw, _, err := cl.DoRaw(c.Request.Context(), "GET", p, url.Values{"alt": {"media"}}, nil, "", 512<<20)
		if err != nil {
			gcpErr(c, err)
			return
		}
		base := name[strings.LastIndex(name, "/")+1:]
		c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", base))
		c.Data(http.StatusOK, firstNonEmpty(meta.ContentType, "application/octet-stream"), raw)
		return
	}
	out := gin.H{"meta": meta.dto()}
	if size <= gcsPreviewMax {
		raw, _, err := cl.DoRaw(c.Request.Context(), "GET", p, url.Values{"alt": {"media"}}, nil, "", gcsPreviewMax)
		if err != nil {
			gcpErr(c, err)
			return
		}
		out["content"] = base64.StdEncoding.EncodeToString(raw)
	} else {
		out["tooLarge"] = true
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// Upload — POST multipart (file, bucket, name, allowWrite).
func (h *GCPHandler) Upload(c *gin.Context) {
	cl, row, ok := h.client(c, gcp.ScopeStorageWrite)
	if !ok {
		return
	}
	if !gcpWriteAllowed(c, row, c.PostForm("allowWrite") == "true") {
		return
	}
	bucket, name := c.PostForm("bucket"), strings.TrimSpace(c.PostForm("name"))
	fh, err := c.FormFile("file")
	if !nameOK(c, "bucket", bucket) {
		return
	}
	if err != nil || name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file and name are required"})
		return
	}
	f, err := fh.Open()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	defer f.Close()
	data, err := readAllLimit(f, 256<<20)
	if err != nil {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "file " + err.Error()})
		return
	}
	ct := fh.Header.Get("Content-Type")
	var meta gcsObject
	raw, _, err := cl.DoRaw(c.Request.Context(), "POST", gcp.UploadBase()+"/b/"+url.PathEscape(bucket)+"/o",
		url.Values{"uploadType": {"media"}, "name": {name}}, data, firstNonEmpty(ct, "application/octet-stream"), 1<<20)
	if err != nil {
		gcpErr(c, err)
		return
	}
	_ = json.Unmarshal(raw, &meta)
	c.JSON(http.StatusOK, gin.H{"data": meta.dto()})
}

// DeleteObject — DELETE ?bucket=&name=&allowWrite=true.
func (h *GCPHandler) DeleteObject(c *gin.Context) {
	cl, row, ok := h.client(c, gcp.ScopeStorageWrite)
	if !ok {
		return
	}
	if !gcpWriteAllowed(c, row, c.Query("allowWrite") == "true") {
		return
	}
	bucket, name := c.Query("bucket"), c.Query("name")
	if !nameOK(c, "bucket", bucket) {
		return
	}
	if strings.TrimSpace(name) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	if _, _, err := cl.DoRaw(c.Request.Context(), "DELETE", gcp.StorageBase()+"/b/"+url.PathEscape(bucket)+"/o/"+url.PathEscape(name), nil, nil, "", 1<<16); err != nil {
		gcpErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"deleted": true}})
}

// ── Pub/Sub ─────────────────────────────────────────────────────────────────

func shortName(full string) string { return full[strings.LastIndex(full, "/")+1:] }

func (h *GCPHandler) Topics(c *gin.Context) {
	cl, row, ok := h.client(c, gcp.ScopePubSub)
	if !ok {
		return
	}
	project, ok := gcpProject(c, row)
	if !ok {
		return
	}
	var out struct {
		Topics []struct {
			Name   string            `json:"name"`
			Labels map[string]string `json:"labels"`
		} `json:"topics"`
	}
	if err := cl.Do(c.Request.Context(), "GET", gcp.PubSubBase()+"/projects/"+project+"/topics", url.Values{"pageSize": {"500"}}, nil, &out); err != nil {
		gcpErr(c, err)
		return
	}
	list := make([]gin.H, 0, len(out.Topics))
	for _, t := range out.Topics {
		list = append(list, gin.H{"name": shortName(t.Name), "fullName": t.Name, "labels": t.Labels})
	}
	sort.Slice(list, func(i, j int) bool { return list[i]["name"].(string) < list[j]["name"].(string) })
	c.JSON(http.StatusOK, gin.H{"data": list})
}

func (h *GCPHandler) Subscriptions(c *gin.Context) {
	cl, row, ok := h.client(c, gcp.ScopePubSub)
	if !ok {
		return
	}
	project, ok := gcpProject(c, row)
	if !ok {
		return
	}
	var out struct {
		Subscriptions []struct {
			Name                     string `json:"name"`
			Topic                    string `json:"topic"`
			AckDeadlineSeconds       int    `json:"ackDeadlineSeconds"`
			MessageRetentionDuration string `json:"messageRetentionDuration"`
			Filter                   string `json:"filter"`
			State                    string `json:"state"`
			PushConfig               *struct {
				PushEndpoint string `json:"pushEndpoint"`
			} `json:"pushConfig"`
			DeadLetterPolicy *struct {
				DeadLetterTopic     string `json:"deadLetterTopic"`
				MaxDeliveryAttempts int    `json:"maxDeliveryAttempts"`
			} `json:"deadLetterPolicy"`
		} `json:"subscriptions"`
	}
	if err := cl.Do(c.Request.Context(), "GET", gcp.PubSubBase()+"/projects/"+project+"/subscriptions", url.Values{"pageSize": {"500"}}, nil, &out); err != nil {
		gcpErr(c, err)
		return
	}
	topic := c.Query("topic")
	list := []gin.H{}
	for _, s := range out.Subscriptions {
		if topic != "" && shortName(s.Topic) != topic {
			continue
		}
		row := gin.H{"name": shortName(s.Name), "topic": shortName(s.Topic), "ackDeadlineSeconds": s.AckDeadlineSeconds,
			"retention": s.MessageRetentionDuration, "filter": s.Filter, "state": s.State, "push": s.PushConfig != nil && s.PushConfig.PushEndpoint != ""}
		if s.DeadLetterPolicy != nil && s.DeadLetterPolicy.DeadLetterTopic != "" {
			row["deadLetterTopic"] = shortName(s.DeadLetterPolicy.DeadLetterTopic)
			row["maxDeliveryAttempts"] = s.DeadLetterPolicy.MaxDeliveryAttempts
		}
		list = append(list, row)
	}
	sort.Slice(list, func(i, j int) bool { return list[i]["name"].(string) < list[j]["name"].(string) })
	c.JSON(http.StatusOK, gin.H{"data": list})
}

// Peek pulls up to max messages and hands them straight back (ack deadline
// 0): nothing is consumed, but each look is a delivery attempt — they come
// back marked redelivered, and with a dead-letter policy enough looks move a
// message to the dead-letter topic. The answer says so.
func (h *GCPHandler) Peek(c *gin.Context) {
	cl, row, ok := h.client(c, gcp.ScopePubSub)
	if !ok {
		return
	}
	project, ok := gcpProject(c, row)
	if !ok {
		return
	}
	var in struct {
		Subscription string `json:"subscription"`
		Max          int    `json:"max"`
	}
	if err := c.ShouldBindJSON(&in); err != nil || !nameOK(c, "subscription", in.Subscription) {
		if !c.Writer.Written() {
			c.JSON(http.StatusBadRequest, gin.H{"error": "subscription is required"})
		}
		return
	}
	if in.Max <= 0 || in.Max > 100 {
		in.Max = 10
	}
	sub := gcp.PubSubBase() + "/projects/" + project + "/subscriptions/" + in.Subscription
	var out struct {
		ReceivedMessages []struct {
			AckID   string `json:"ackId"`
			Message struct {
				Data        string            `json:"data"`
				Attributes  map[string]string `json:"attributes"`
				MessageID   string            `json:"messageId"`
				PublishTime string            `json:"publishTime"`
				OrderingKey string            `json:"orderingKey"`
			} `json:"message"`
			DeliveryAttempt int `json:"deliveryAttempt"`
		} `json:"receivedMessages"`
	}
	if err := cl.Do(c.Request.Context(), "POST", sub+":pull", nil, gin.H{"maxMessages": in.Max, "returnImmediately": true}, &out); err != nil {
		gcpErr(c, err)
		return
	}
	acks := []string{}
	msgs := make([]gin.H, 0, len(out.ReceivedMessages))
	for _, m := range out.ReceivedMessages {
		acks = append(acks, m.AckID)
		data, _ := base64.StdEncoding.DecodeString(m.Message.Data)
		msgs = append(msgs, gin.H{"id": m.Message.MessageID, "data": string(data), "attributes": m.Message.Attributes,
			"publishTime": m.Message.PublishTime, "orderingKey": m.Message.OrderingKey, "deliveryAttempt": m.DeliveryAttempt})
	}
	released := true
	if len(acks) > 0 {
		if err := cl.Do(c.Request.Context(), "POST", sub+":modifyAckDeadline", nil, gin.H{"ackIds": acks, "ackDeadlineSeconds": 0}, nil); err != nil {
			released = false // they come back after the subscription's ack deadline
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"messages": msgs, "released": released,
		"note": "Peeked messages were handed back at once and will be delivered again (as redelivered). Each peek counts as a delivery attempt toward a dead-letter policy."}})
}

func (h *GCPHandler) Publish(c *gin.Context) {
	cl, row, ok := h.client(c, gcp.ScopePubSub)
	if !ok {
		return
	}
	project, ok := gcpProject(c, row)
	if !ok {
		return
	}
	var in struct {
		Topic       string            `json:"topic"`
		Data        string            `json:"data"`
		Attributes  map[string]string `json:"attributes"`
		OrderingKey string            `json:"orderingKey"`
		AllowWrite  bool              `json:"allowWrite"`
	}
	if err := c.ShouldBindJSON(&in); err != nil || !nameOK(c, "topic", in.Topic) {
		if !c.Writer.Written() {
			c.JSON(http.StatusBadRequest, gin.H{"error": "topic is required"})
		}
		return
	}
	if !gcpWriteAllowed(c, row, in.AllowWrite) {
		return
	}
	msg := gin.H{"data": base64.StdEncoding.EncodeToString([]byte(in.Data))}
	if len(in.Attributes) > 0 {
		msg["attributes"] = in.Attributes
	}
	if in.OrderingKey != "" {
		msg["orderingKey"] = in.OrderingKey
	}
	var out struct {
		MessageIDs []string `json:"messageIds"`
	}
	if err := cl.Do(c.Request.Context(), "POST", gcp.PubSubBase()+"/projects/"+project+"/topics/"+in.Topic+":publish", nil, gin.H{"messages": []gin.H{msg}}, &out); err != nil {
		gcpErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"messageIds": out.MessageIDs}})
}

// ── Cloud Logging ───────────────────────────────────────────────────────────

// Logs — POST {project, filter, from, to, pageSize, pageToken}: newest first.
func (h *GCPHandler) Logs(c *gin.Context) {
	cl, row, ok := h.client(c, gcp.ScopeLoggingRead)
	if !ok {
		return
	}
	var in struct {
		Project   string `json:"project"`
		Filter    string `json:"filter"`
		From      string `json:"from"`
		To        string `json:"to"`
		PageSize  int    `json:"pageSize"`
		PageToken string `json:"pageToken"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	project := firstNonEmpty(in.Project, row.ProjectID)
	if project == "" || strings.ContainsAny(project, "/?# ") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "project is required"})
		return
	}
	if in.PageSize <= 0 || in.PageSize > 1000 {
		in.PageSize = 200
	}
	parts := []string{}
	if f := strings.TrimSpace(in.Filter); f != "" {
		parts = append(parts, "("+f+")")
	}
	from := in.From
	if from == "" {
		from = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	}
	parts = append(parts, fmt.Sprintf(`timestamp>=%q`, from))
	if in.To != "" {
		parts = append(parts, fmt.Sprintf(`timestamp<=%q`, in.To))
	}
	body := gin.H{"resourceNames": []string{"projects/" + project}, "filter": strings.Join(parts, " AND "), "orderBy": "timestamp desc", "pageSize": in.PageSize}
	if in.PageToken != "" {
		body["pageToken"] = in.PageToken
	}
	var out struct {
		Entries []struct {
			InsertID    string          `json:"insertId"`
			LogName     string          `json:"logName"`
			Timestamp   string          `json:"timestamp"`
			Severity    string          `json:"severity"`
			TextPayload string          `json:"textPayload"`
			JSONPayload json.RawMessage `json:"jsonPayload"`
			Proto       json.RawMessage `json:"protoPayload"`
			Trace       string          `json:"trace"`
			Resource    struct {
				Type   string            `json:"type"`
				Labels map[string]string `json:"labels"`
			} `json:"resource"`
			Labels      map[string]string `json:"labels"`
			HTTPRequest json.RawMessage   `json:"httpRequest"`
		} `json:"entries"`
		NextPageToken string `json:"nextPageToken"`
	}
	if err := cl.Do(c.Request.Context(), "POST", gcp.LoggingBase()+"/entries:list", nil, body, &out); err != nil {
		gcpErr(c, err)
		return
	}
	list := make([]gin.H, 0, len(out.Entries))
	for _, e := range out.Entries {
		msg := e.TextPayload
		var payload any
		switch {
		case len(e.JSONPayload) > 0:
			_ = json.Unmarshal(e.JSONPayload, &payload)
			if m, ok := payload.(map[string]any); ok {
				for _, k := range []string{"message", "msg", "textPayload", "error"} {
					if s, ok := m[k].(string); ok && s != "" {
						msg = s
						break
					}
				}
			}
			if msg == "" {
				msg = string(e.JSONPayload)
			}
		case len(e.Proto) > 0:
			_ = json.Unmarshal(e.Proto, &payload)
			msg = string(e.Proto)
		}
		logName, _ := url.PathUnescape(shortName(e.LogName))
		item := gin.H{"id": e.InsertID, "timestamp": e.Timestamp, "severity": firstNonEmpty(e.Severity, "DEFAULT"), "log": logName,
			"resource": e.Resource.Type, "resourceLabels": e.Resource.Labels, "labels": e.Labels, "message": msg, "trace": e.Trace}
		if payload != nil {
			item["payload"] = payload
		}
		list = append(list, item)
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"entries": list, "nextPageToken": out.NextPageToken, "filter": body["filter"]}})
}

// ── GKE ─────────────────────────────────────────────────────────────────────

type gkeCluster struct {
	Name                 string `json:"name"`
	Location             string `json:"location"`
	Status               string `json:"status"`
	Endpoint             string `json:"endpoint"`
	CurrentMasterVersion string `json:"currentMasterVersion"`
	CurrentNodeCount     int    `json:"currentNodeCount"`
	Autopilot            *struct {
		Enabled bool `json:"enabled"`
	} `json:"autopilot"`
	MasterAuth struct {
		ClusterCaCertificate string `json:"clusterCaCertificate"`
	} `json:"masterAuth"`
	PrivateClusterConfig *struct {
		EnablePrivateEndpoint bool   `json:"enablePrivateEndpoint"`
		PrivateEndpoint       string `json:"privateEndpoint"`
	} `json:"privateClusterConfig"`
}

func (h *GCPHandler) Clusters(c *gin.Context) {
	cl, row, ok := h.client(c, gcp.ScopeReadOnly)
	if !ok {
		return
	}
	project, ok := gcpProject(c, row)
	if !ok {
		return
	}
	var out struct {
		Clusters []gkeCluster `json:"clusters"`
	}
	if err := cl.Do(c.Request.Context(), "GET", gcp.ContainerBase()+"/projects/"+project+"/locations/-/clusters", nil, nil, &out); err != nil {
		gcpErr(c, err)
		return
	}
	list := make([]gin.H, 0, len(out.Clusters))
	for _, k := range out.Clusters {
		list = append(list, gin.H{"name": k.Name, "location": k.Location, "status": k.Status, "endpoint": k.Endpoint,
			"version": k.CurrentMasterVersion, "nodes": k.CurrentNodeCount, "autopilot": k.Autopilot != nil && k.Autopilot.Enabled,
			"privateEndpoint": k.PrivateClusterConfig != nil && k.PrivateClusterConfig.EnablePrivateEndpoint})
	}
	c.JSON(http.StatusOK, gin.H{"data": list})
}

// AddCluster — POST {project, location, cluster, name}: a Kubernetes
// connection for the cluster that authenticates with this credential (no
// kubeconfig / gcloud plugin needed; the token refreshes by itself).
func (h *GCPHandler) AddCluster(c *gin.Context) {
	cl, row, ok := h.client(c, gcp.ScopeReadOnly)
	if !ok {
		return
	}
	var in struct {
		Project  string `json:"project"`
		Location string `json:"location"`
		Cluster  string `json:"cluster"`
		Name     string `json:"name"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	project := firstNonEmpty(in.Project, row.ProjectID)
	for _, v := range []string{project, in.Location, in.Cluster} {
		if !nameOK(c, "project, location and cluster", v) {
			return
		}
	}
	var k gkeCluster
	if err := cl.Do(c.Request.Context(), "GET", gcp.ContainerBase()+"/projects/"+project+"/locations/"+in.Location+"/clusters/"+in.Cluster, nil, nil, &k); err != nil {
		gcpErr(c, err)
		return
	}
	server := k.Endpoint
	if k.PrivateClusterConfig != nil && k.PrivateClusterConfig.EnablePrivateEndpoint && k.PrivateClusterConfig.PrivateEndpoint != "" {
		server = k.PrivateClusterConfig.PrivateEndpoint
	}
	if server == "" || k.MasterAuth.ClusterCaCertificate == "" {
		c.JSON(http.StatusBadGateway, gin.H{"error": "GKE didn't return the cluster's endpoint and CA"})
		return
	}
	ctxName := "gke_" + project + "_" + in.Location + "_" + in.Cluster
	if !strings.HasPrefix(server, "https://") {
		server = "https://" + server
	}
	kubeconfig := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: %[1]s
  cluster:
    server: %[2]s
    certificate-authority-data: %[3]s
contexts:
- name: %[1]s
  context:
    cluster: %[1]s
    user: %[1]s
current-context: %[1]s
users:
- name: %[1]s
  user: {}
`, ctxName, server, k.MasterAuth.ClusterCaCertificate)
	enc, err := h.cipher.Encrypt(kubeconfig)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt kubeconfig"})
		return
	}
	opts, _ := json.Marshal(map[string]any{"gcpCredentialId": row.ID, "gke": gin.H{"project": project, "location": in.Location, "cluster": in.Cluster}})
	ctxJSON, _ := json.Marshal([]map[string]any{{"name": ctxName, "cluster": ctxName, "user": ctxName}})
	conn := models.K8sConnection{ID: uuid.NewString(), UserID: row.UserID, Name: firstNonEmpty(strings.TrimSpace(in.Name), in.Cluster+" (GKE)"),
		KubeconfigEnc: enc, DefaultContext: ctxName, ContextsJSON: string(ctxJSON), AuthMode: "gke", OptionsJSON: string(opts)}
	if err := h.db.Create(&conn).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save connection"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": gin.H{"id": conn.ID, "name": conn.Name, "context": ctxName}})
}
