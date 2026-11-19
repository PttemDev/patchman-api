package dbquery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Aggregation builder stage previews (MONGO_COMPASS_PLAN §B): run the
// pipeline up to one stage, capped by $limit and a time budget, and return
// the documents as relaxed extended JSON (key order kept). Never runs a
// writing stage: $out / $merge are refused.

// MongoPipeliner is implemented by Mongo sessions.
type MongoPipeliner interface {
	MongoAggregatePreview(ctx context.Context, db, coll string, stages []string, upTo, limit int) ([]string, error)
}

// PreviewTimeout bounds one stage preview.
const PreviewTimeout = 5 * time.Second

// ParseStage reads one stage document ({ $match: {...} }) and returns it with
// its operator.
func ParseStage(text string) (bson.D, string, error) {
	v, err := parseJSONish(text)
	if err != nil {
		return nil, "", err
	}
	var d bson.D
	switch t := v.(type) {
	case bson.D:
		d = t
	case bson.M:
		for k, x := range t {
			d = append(d, bson.E{Key: k, Value: x})
		}
	default:
		return nil, "", errors.New("a stage must be a document like { $match: { … } }")
	}
	if len(d) != 1 || !strings.HasPrefix(d[0].Key, "$") {
		return nil, "", errors.New("a stage must have exactly one $operator key")
	}
	return d, d[0].Key, nil
}

func (s *mongoSession) MongoAggregatePreview(ctx context.Context, dbName, coll string, stages []string, upTo, limit int) ([]string, error) {
	dbName, err := s.dbFor(dbName)
	if err != nil {
		return nil, err
	}
	if upTo < 0 || upTo >= len(stages) {
		return nil, fmt.Errorf("stage %d is out of range", upTo)
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	pipeline := bson.A{}
	for i := 0; i <= upTo; i++ {
		d, op, err := ParseStage(stages[i])
		if err != nil {
			return nil, fmt.Errorf("stage %d: %w", i+1, err)
		}
		if op == "$out" || op == "$merge" {
			return nil, fmt.Errorf("stage %d (%s) writes to a collection; it is not previewed", i+1, op)
		}
		pipeline = append(pipeline, d)
	}
	pipeline = append(pipeline, bson.D{{Key: "$limit", Value: limit}})
	ctx, cancel := context.WithTimeout(ctx, PreviewTimeout)
	defer cancel()
	cur, err := s.client.Database(dbName).Collection(coll).Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	out := []string{}
	for cur.Next(ctx) {
		b, err := bson.MarshalExtJSON(cur.Current, false, false)
		if err != nil {
			return nil, err
		}
		out = append(out, string(b))
	}
	return out, cur.Err()
}
