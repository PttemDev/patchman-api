package esquery

import "encoding/json"

// Field is one flattened mapping field. JSON tags are the FE wire contract.
type Field struct {
	Path         string `json:"path"`
	Type         string `json:"type"`
	Searchable   bool   `json:"searchable"`
	Aggregatable bool   `json:"aggregatable"`
	IsKeyword    bool   `json:"isKeyword"`
	IsNested     bool   `json:"isNested"`
}

// ClusterInfo is the flattened GET / + _cluster/health summary.
type ClusterInfo struct {
	Name              string `json:"name"`
	ClusterName       string `json:"clusterName"`
	Version           string `json:"version"`
	Flavor            string `json:"flavor"`
	HealthStatus      string `json:"healthStatus"`
	NumberOfNodes     int    `json:"numberOfNodes"`
	NumberOfDataNodes int    `json:"numberOfDataNodes"`
	ActiveShards      int    `json:"activeShards"`
	UnassignedShards  int    `json:"unassignedShards"`
	RelocatingShards  int    `json:"relocatingShards"`
}

// NodeInfo is one row of _cat/nodes.
type NodeInfo struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Roles       string `json:"roles"`
	HeapPercent string `json:"heapPercent"`
	CPU         string `json:"cpu"`
	Load        string `json:"load"`
	DiskPercent string `json:"diskPercent"`
}

// ClusterResult aggregates cluster info + node list.
type ClusterResult struct {
	Info  ClusterInfo `json:"info"`
	Nodes []NodeInfo  `json:"nodes"`
}

// ExecResult is the pass-through response of a forwarded request.
type ExecResult struct {
	Status int             `json:"status"`
	TookMs int             `json:"tookMs"`
	Body   json.RawMessage `json:"body"`
}

// IndexRow is one row of _cat/indices.
type IndexRow struct {
	Health       string `json:"health"`
	Status       string `json:"status"`
	Index        string `json:"index"`
	UUID         string `json:"uuid"`
	DocsCount    int64  `json:"docsCount"`
	DocsDeleted  int64  `json:"docsDeleted"`
	PriShards    int    `json:"priShards"`
	RepShards    int    `json:"repShards"`
	StoreSize    string `json:"storeSize"`
	PriStoreSize string `json:"priStoreSize"` // primary-only size (bytes=b), human-formatted in the FE
}

// AliasRow is one row of _cat/aliases — an alias and the index it points at,
// plus the routing/filter/write-index facts the FE surfaces as badges.
type AliasRow struct {
	Alias         string `json:"alias"`
	Index         string `json:"index"`
	Filter        string `json:"filter"`        // "-" (none) or "*" (a filter is set)
	RoutingIndex  string `json:"routingIndex"`  // "-" or a routing value
	RoutingSearch string `json:"routingSearch"` // "-" or a routing value
	IsWriteIndex  string `json:"isWriteIndex"`  // "true" / "false" / "-"
}
