package dbquery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Compass-style index management for Mongo connections
// (MONGO_COMPASS_PLAN §A). The handler reaches it through a type assertion
// on the pooled Session.

// MongoIndexer is implemented by Mongo sessions.
type MongoIndexer interface {
	MongoIndexes(ctx context.Context, db, coll string) ([]MongoIndex, error)
	MongoCreateIndex(ctx context.Context, db, coll string, spec MongoIndexSpec) (string, error)
	MongoDropIndex(ctx context.Context, db, coll, name string) error
	MongoSetIndexHidden(ctx context.Context, db, coll, name string, hidden bool) error
}

// MongoIndexKey is one field of an index key, in order. Value is 1 / -1 or
// a type name ("text", "2dsphere", "2d", "hashed").
type MongoIndexKey struct {
	Field string `json:"field"`
	Value any    `json:"value"`
}

// MongoIndex is one index as the Indexes view shows it. Size / Ops are -1
// when the server would not tell us (no $collStats / $indexStats right).
type MongoIndex struct {
	Name               string          `json:"name"`
	Keys               []MongoIndexKey `json:"keys"`
	Type               string          `json:"type"` // regular | compound | text | geospatial | hashed | wildcard
	Unique             bool            `json:"unique,omitempty"`
	Sparse             bool            `json:"sparse,omitempty"`
	Hidden             bool            `json:"hidden,omitempty"`
	ExpireAfterSeconds *int64          `json:"expireAfterSeconds,omitempty"`
	Partial            any             `json:"partialFilterExpression,omitempty"`
	Collation          any             `json:"collation,omitempty"`
	Size               int64           `json:"size"`
	Ops                int64           `json:"ops"`
	Since              *time.Time      `json:"since,omitempty"`
}

// MongoIndexSpec is a createIndexes request.
type MongoIndexSpec struct {
	Keys               []MongoIndexKey `json:"keys"`
	Name               string          `json:"name"`
	Unique             bool            `json:"unique"`
	Sparse             bool            `json:"sparse"`
	Hidden             bool            `json:"hidden"`
	ExpireAfterSeconds *int64          `json:"expireAfterSeconds"`
	// PartialFilter is shell-ish JSON text ({ status: "active" }).
	PartialFilter string `json:"partialFilter"`
}

func (s *mongoSession) dbFor(name string) (string, error) {
	if name = strings.TrimSpace(name); name != "" {
		return name, nil
	}
	if s.defaultDBName != "" {
		return s.defaultDBName, nil
	}
	return "", errors.New("database is required (this connection has no default database)")
}

func (s *mongoSession) MongoIndexes(ctx context.Context, dbName, coll string) ([]MongoIndex, error) {
	dbName, err := s.dbFor(dbName)
	if err != nil {
		return nil, err
	}
	c := s.client.Database(dbName).Collection(coll)
	cur, err := c.Indexes().List(ctx)
	if err != nil {
		return nil, err
	}
	var raw []bson.D
	if err := cur.All(ctx, &raw); err != nil {
		return nil, err
	}

	// Usage and size are best-effort extras.
	ops := map[string]int64{}
	since := map[string]time.Time{}
	if sc, err := c.Aggregate(ctx, bson.A{bson.D{{Key: "$indexStats", Value: bson.D{}}}}); err == nil {
		var stats []bson.M
		if sc.All(ctx, &stats) == nil {
			for _, st := range stats {
				name, _ := st["name"].(string)
				if acc := asM(st["accesses"]); acc != nil {
					// $indexStats is per mongod; sum across a replica set's
					// members only if several rows come back for one name.
					ops[name] += toInt64(acc["ops"])
					if t, ok := acc["since"].(bson.DateTime); ok {
						since[name] = t.Time()
					}
				}
			}
		}
	}
	sizes := map[string]int64{}
	if sc, err := c.Aggregate(ctx, bson.A{bson.D{{Key: "$collStats", Value: bson.D{{Key: "storageStats", Value: bson.D{}}}}}}); err == nil {
		var cs []bson.M
		if sc.All(ctx, &cs) == nil && len(cs) > 0 {
			if ss := asM(cs[0]["storageStats"]); ss != nil {
				if is := asM(ss["indexSizes"]); is != nil {
					for k, v := range is {
						sizes[k] = toInt64(v)
					}
				}
			}
		}
	}

	out := make([]MongoIndex, 0, len(raw))
	for _, d := range raw {
		ix := MongoIndex{Size: -1, Ops: -1}
		for _, e := range d {
			switch e.Key {
			case "name":
				ix.Name, _ = e.Value.(string)
			case "key":
				if kd, ok := e.Value.(bson.D); ok {
					for _, k := range kd {
						ix.Keys = append(ix.Keys, MongoIndexKey{Field: k.Key, Value: plainValue(k.Value)})
					}
				}
			case "unique":
				ix.Unique, _ = e.Value.(bool)
			case "sparse":
				ix.Sparse, _ = e.Value.(bool)
			case "hidden":
				ix.Hidden, _ = e.Value.(bool)
			case "expireAfterSeconds":
				n := toInt64(e.Value)
				ix.ExpireAfterSeconds = &n
			case "partialFilterExpression":
				ix.Partial = plainValue(e.Value)
			case "collation":
				ix.Collation = plainValue(e.Value)
			}
		}
		ix.Type = indexType(ix.Keys)
		if n, ok := sizes[ix.Name]; ok {
			ix.Size = n
		}
		if n, ok := ops[ix.Name]; ok {
			ix.Ops = n
			if t, ok := since[ix.Name]; ok {
				ix.Since = &t
			}
		}
		out = append(out, ix)
	}
	return out, nil
}

// asM reads a sub-document whichever way the driver decoded it.
func asM(v any) bson.M {
	switch t := v.(type) {
	case bson.M:
		return t
	case bson.D:
		m, _ := plainValue(t).(bson.M)
		return m
	case map[string]any:
		return t
	}
	return nil
}

func indexType(keys []MongoIndexKey) string {
	for _, k := range keys {
		if strings.Contains(k.Field, "$**") {
			return "wildcard"
		}
		if v, ok := k.Value.(string); ok {
			switch v {
			case "text":
				return "text"
			case "2dsphere", "2d", "geoHaystack":
				return "geospatial"
			case "hashed":
				return "hashed"
			}
		}
	}
	if len(keys) > 1 {
		return "compound"
	}
	return "regular"
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case int32:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}

// indexKeyValue normalises a key direction / type from JSON (numbers come
// in as float64).
func indexKeyValue(v any) (any, error) {
	switch t := v.(type) {
	case float64:
		if t == 1 || t == -1 {
			return int32(t), nil
		}
	case int, int32, int64:
		n := toInt64(t)
		if n == 1 || n == -1 {
			return int32(n), nil
		}
	case string:
		switch t {
		case "text", "2dsphere", "2d", "hashed":
			return t, nil
		}
	}
	return nil, fmt.Errorf("index key value must be 1, -1, text, 2dsphere, 2d or hashed")
}

// DefaultMongoIndexName mirrors the server's naming: field_1_other_-1.
func DefaultMongoIndexName(keys []MongoIndexKey) string {
	parts := make([]string, 0, len(keys)*2)
	for _, k := range keys {
		parts = append(parts, k.Field, fmt.Sprint(k.Value))
	}
	return strings.Join(parts, "_")
}

func (s *mongoSession) MongoCreateIndex(ctx context.Context, dbName, coll string, spec MongoIndexSpec) (string, error) {
	dbName, err := s.dbFor(dbName)
	if err != nil {
		return "", err
	}
	if len(spec.Keys) == 0 {
		return "", errors.New("an index needs at least one field")
	}
	key := bson.D{}
	for i, k := range spec.Keys {
		f := strings.TrimSpace(k.Field)
		if f == "" || strings.HasPrefix(f, "$") && f != "$**" {
			return "", fmt.Errorf("field %d: invalid field name", i+1)
		}
		v, err := indexKeyValue(k.Value)
		if err != nil {
			return "", fmt.Errorf("field %q: %w", f, err)
		}
		spec.Keys[i] = MongoIndexKey{Field: f, Value: v}
		key = append(key, bson.E{Key: f, Value: v})
	}
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		name = DefaultMongoIndexName(spec.Keys)
	}
	ix := bson.D{{Key: "key", Value: key}, {Key: "name", Value: name}}
	if spec.Unique {
		ix = append(ix, bson.E{Key: "unique", Value: true})
	}
	if spec.Sparse {
		ix = append(ix, bson.E{Key: "sparse", Value: true})
	}
	if spec.Hidden {
		ix = append(ix, bson.E{Key: "hidden", Value: true})
	}
	if spec.ExpireAfterSeconds != nil {
		if *spec.ExpireAfterSeconds < 0 {
			return "", errors.New("TTL must be zero or more seconds")
		}
		ix = append(ix, bson.E{Key: "expireAfterSeconds", Value: int32(*spec.ExpireAfterSeconds)})
	}
	if pf := strings.TrimSpace(spec.PartialFilter); pf != "" {
		v, err := parseJSONish(pf)
		if err != nil {
			return "", fmt.Errorf("partial filter: %w", err)
		}
		ix = append(ix, bson.E{Key: "partialFilterExpression", Value: v})
	}
	cmd := bson.D{{Key: "createIndexes", Value: coll}, {Key: "indexes", Value: bson.A{ix}}}
	if err := s.client.Database(dbName).RunCommand(ctx, cmd).Err(); err != nil {
		return "", err
	}
	return name, nil
}

func (s *mongoSession) MongoDropIndex(ctx context.Context, dbName, coll, name string) error {
	dbName, err := s.dbFor(dbName)
	if err != nil {
		return err
	}
	if name == "_id_" {
		return errors.New("the _id index cannot be dropped")
	}
	return s.client.Database(dbName).Collection(coll).Indexes().DropOne(ctx, name)
}

func (s *mongoSession) MongoSetIndexHidden(ctx context.Context, dbName, coll, name string, hidden bool) error {
	dbName, err := s.dbFor(dbName)
	if err != nil {
		return err
	}
	if name == "_id_" {
		return errors.New("the _id index cannot be hidden")
	}
	cmd := bson.D{{Key: "collMod", Value: coll}, {Key: "index", Value: bson.D{{Key: "name", Value: name}, {Key: "hidden", Value: hidden}}}}
	return s.client.Database(dbName).RunCommand(ctx, cmd).Err()
}
