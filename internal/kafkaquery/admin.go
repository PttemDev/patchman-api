package kafkaquery

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"

	"patchman-be/internal/models"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
)

// ListTopics returns every topic with its partition count + replication
// factor. Internal topics (__consumer_offsets, …) are included but flagged.
func (r *Registry) ListTopics(ctx context.Context, conn models.KafkaConnection) ([]TopicSummary, error) {
	adm, err := r.Admin(conn)
	if err != nil {
		return nil, err
	}
	details, err := adm.ListTopics(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]TopicSummary, 0, len(details))
	for _, d := range details {
		if d.Err != nil {
			continue
		}
		rf := 0
		for _, p := range d.Partitions {
			rf = len(p.Replicas)
			break
		}
		out = append(out, TopicSummary{
			Name:              d.Topic,
			Partitions:        len(d.Partitions),
			ReplicationFactor: rf,
			Internal:          d.IsInternal,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// TopicsOverview returns the enriched topic list for the Topics tab:
// partition + replication metadata, an approximate message count (end−start
// offset span, summed per topic) and health flags (under-replicated /
// offline). It costs two extra bulk offset RPCs over ListTopics.
//
// GRACEFUL DEGRADE: a call-level offset failure (e.g. no offset ACL at all)
// leaves every MessageCount at -1 but still returns the metadata rows; a
// per-topic/partition offset error degrades only that topic's count to -1.
func (r *Registry) TopicsOverview(ctx context.Context, conn models.KafkaConnection) ([]TopicOverview, error) {
	adm, err := r.Admin(conn)
	if err != nil {
		return nil, err
	}
	details, err := adm.ListTopics(ctx)
	if err != nil {
		return nil, translateAuthErr(err)
	}
	// Bulk watermarks for every topic (two RPCs total, not per-topic).
	starts, errS := adm.ListStartOffsets(ctx)
	ends, errE := adm.ListEndOffsets(ctx)
	offsetsOK := errS == nil && errE == nil
	// Partition sizes: one DescribeLogDirs per broker for every topic.
	leaders := topicLeaders{}
	for _, d := range details {
		m := map[int32]int32{}
		for _, p := range d.Partitions {
			m[p.Partition] = p.Leader
		}
		leaders[d.Topic] = m
	}
	sizes := describeSizes(ctx, adm, nil, leaders)

	out := make([]TopicOverview, 0, len(details))
	for _, d := range details {
		if d.Err != nil {
			continue
		}
		rf := 0
		underRep := false
		offline := false
		// countKnown starts true only if the bulk calls succeeded; any
		// partition we can't resolve flips it false for THIS topic.
		countKnown := offsetsOK
		var count int64
		for _, p := range d.Partitions {
			if len(p.Replicas) > rf {
				rf = len(p.Replicas)
			}
			if p.Err != nil || p.Leader < 0 {
				offline = true
			}
			if p.Err == nil && len(p.ISR) < len(p.Replicas) {
				underRep = true
			}
			if countKnown {
				if c, ok := partitionSpan(starts, ends, d.Topic, p.Partition); ok {
					count += c
				} else {
					countKnown = false
				}
			}
		}
		mc := int64(-1)
		if countKnown {
			mc = count
		}
		ids := make([]int32, 0, len(d.Partitions))
		for _, p := range d.Partitions {
			ids = append(ids, p.Partition)
		}
		size, replicaSize := sizes.topicTotals(d.Topic, ids)
		out = append(out, TopicOverview{
			Name:              d.Topic,
			Partitions:        len(d.Partitions),
			ReplicationFactor: rf,
			Internal:          d.IsInternal,
			MessageCount:      mc,
			UnderReplicated:   underRep,
			Offline:           offline,
			SizeBytes:         size,
			ReplicaSizeBytes:  replicaSize,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// TopicDetail returns the expanded view for one topic: per-partition
// placement (leader/replicas/ISR) + watermarks, health flags, and the full
// config list. Watermark/config failures degrade gracefully (Low/High/Count
// = -1, empty config) rather than failing the whole call.
func (r *Registry) TopicDetail(ctx context.Context, conn models.KafkaConnection, topic string) (*TopicDetail, error) {
	adm, err := r.Admin(conn)
	if err != nil {
		return nil, err
	}
	details, err := adm.ListTopics(ctx, topic)
	if err != nil {
		return nil, translateAuthErr(err)
	}
	d, ok := details[topic]
	if !ok {
		return nil, fmt.Errorf("topic %q not found", topic)
	}
	if d.Err != nil {
		return nil, d.Err
	}
	starts, errS := adm.ListStartOffsets(ctx, topic)
	ends, errE := adm.ListEndOffsets(ctx, topic)
	offsetsOK := errS == nil && errE == nil
	set := kadm.TopicsSet{}
	leaders := topicLeaders{topic: map[int32]int32{}}
	ids := make([]int32, 0, len(d.Partitions))
	for _, p := range d.Partitions {
		set.Add(topic, p.Partition)
		leaders[topic][p.Partition] = p.Leader
		ids = append(ids, p.Partition)
	}
	sizes := describeSizes(ctx, adm, set, leaders)

	det := &TopicDetail{
		Name:            topic,
		Partitions:      len(d.Partitions),
		Internal:        d.IsInternal,
		PartitionDetail: []TopicPartitionDetail{},
		Config:          []ConfigEntry{},
	}
	rf := 0
	countKnown := offsetsOK
	var total int64
	for _, p := range d.Partitions {
		if len(p.Replicas) > rf {
			rf = len(p.Replicas)
		}
		pd := TopicPartitionDetail{
			Partition: p.Partition,
			Leader:    p.Leader,
			Replicas:  p.Replicas,
			ISR:       p.ISR,
			Offline:   p.OfflineReplicas,
			Low:       -1,
			High:      -1,
			Count:     -1,
		}
		pd.SizeBytes, pd.ReplicaSizeBytes = sizes.partition(topic, p.Partition)
		if p.Err != nil {
			pd.Err = p.Err.Error()
			det.Offline = true
		} else {
			if p.Leader < 0 {
				det.Offline = true
			}
			if len(p.ISR) < len(p.Replicas) {
				det.UnderReplicated = true
			}
		}
		if offsetsOK {
			lo, okL := starts.Lookup(topic, p.Partition)
			hi, okH := ends.Lookup(topic, p.Partition)
			if okL && okH && lo.Err == nil && hi.Err == nil && lo.Offset >= 0 && hi.Offset >= 0 {
				pd.Low = lo.Offset
				pd.High = hi.Offset
				span := hi.Offset - lo.Offset
				if span < 0 {
					span = 0
				}
				pd.Count = span
				total += span
			} else {
				countKnown = false
			}
		}
		det.PartitionDetail = append(det.PartitionDetail, pd)
	}
	sort.Slice(det.PartitionDetail, func(i, j int) bool {
		return det.PartitionDetail[i].Partition < det.PartitionDetail[j].Partition
	})
	det.ReplicationFactor = rf
	det.SizeBytes, det.ReplicaSizeBytes = sizes.topicTotals(topic, ids)
	det.MessageCount = -1
	if countKnown {
		det.MessageCount = total
	}
	// Config is best-effort: a config-describe failure leaves it empty
	// rather than sinking the whole detail view.
	if cfg, cErr := r.TopicConfig(ctx, conn, topic); cErr == nil {
		det.Config = cfg
	}
	return det, nil
}

// partitionSpan returns (high-start) for one topic/partition from the bulk
// offset maps, or ok=false when either watermark is missing/errored/negative.
func partitionSpan(starts, ends kadm.ListedOffsets, topic string, part int32) (int64, bool) {
	lo, okL := starts.Lookup(topic, part)
	hi, okH := ends.Lookup(topic, part)
	if !okL || !okH || lo.Err != nil || hi.Err != nil || lo.Offset < 0 || hi.Offset < 0 {
		return 0, false
	}
	span := hi.Offset - lo.Offset
	if span < 0 {
		span = 0
	}
	return span, true
}

// TopicConfig returns all config entries for a topic, marking the ones the
// UI is allowed to edit.
func (r *Registry) TopicConfig(ctx context.Context, conn models.KafkaConnection, topic string) ([]ConfigEntry, error) {
	adm, err := r.Admin(conn)
	if err != nil {
		return nil, err
	}
	rcs, err := adm.DescribeTopicConfigs(ctx, topic)
	if err != nil {
		return nil, err
	}
	rc, err := rcs.On(topic, nil)
	if err != nil {
		return nil, err
	}
	if rc.Err != nil {
		return nil, rc.Err
	}
	out := make([]ConfigEntry, 0, len(rc.Configs))
	for _, cfg := range rc.Configs {
		val := ""
		if cfg.Value != nil {
			val = *cfg.Value
		}
		out = append(out, ConfigEntry{
			Key:       cfg.Key,
			Value:     val,
			Source:    cfg.Source.String(),
			Sensitive: cfg.Sensitive,
			Editable:  AllowedTopicConfigs[cfg.Key],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// AlterTopicConfig sets the given config keys on a topic. Only keys in the
// allow-list are accepted; anything else is rejected before the call.
func (r *Registry) AlterTopicConfig(ctx context.Context, conn models.KafkaConnection, topic string, changes map[string]string) error {
	if len(changes) == 0 {
		return fmt.Errorf("no config changes provided")
	}
	alters := make([]kadm.AlterConfig, 0, len(changes))
	for k, v := range changes {
		if !AllowedTopicConfigs[k] {
			return fmt.Errorf("config %q is not editable from this tool", k)
		}
		val := v
		alters = append(alters, kadm.AlterConfig{Op: kadm.SetConfig, Name: k, Value: &val})
	}
	adm, err := r.Admin(conn)
	if err != nil {
		return err
	}
	resps, err := adm.AlterTopicConfigs(ctx, alters, topic)
	if err != nil {
		return err
	}
	resp, err := resps.On(topic, nil)
	if err != nil {
		return err
	}
	return resp.Err
}

// CreateTopic provisions a new topic. Config values (retention.ms,
// retention.bytes, min.insync.replicas, cleanup.policy, …) are passed
// through as-is.
func (r *Registry) CreateTopic(ctx context.Context, conn models.KafkaConnection, name string, partitions int32, rf int16, configs map[string]string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("topic name is required")
	}
	if partitions < 1 {
		return fmt.Errorf("partitions must be >= 1")
	}
	if rf < 1 {
		return fmt.Errorf("replication factor must be >= 1")
	}
	cfgs := map[string]*string{}
	for k, v := range configs {
		val := v
		cfgs[k] = &val
	}
	adm, err := r.Admin(conn)
	if err != nil {
		return err
	}
	resp, err := adm.CreateTopic(ctx, partitions, rf, cfgs, name)
	if err != nil {
		return err
	}
	return resp.Err
}

// AddPartitions increases a topic's partition count to `total`. Kafka only
// allows growing partitions (never shrinking); we translate to the number
// to add.
func (r *Registry) AddPartitions(ctx context.Context, conn models.KafkaConnection, topic string, total int) error {
	adm, err := r.Admin(conn)
	if err != nil {
		return err
	}
	details, err := adm.ListTopics(ctx, topic)
	if err != nil {
		return err
	}
	d, ok := details[topic]
	if !ok || d.Err != nil {
		return fmt.Errorf("topic %q not found", topic)
	}
	current := len(d.Partitions)
	if total <= current {
		return fmt.Errorf("partitions can only be increased (current %d, requested %d)", current, total)
	}
	resps, err := adm.CreatePartitions(ctx, total-current, topic)
	if err != nil {
		return err
	}
	resp, err := resps.On(topic, nil)
	if err != nil {
		return err
	}
	return resp.Err
}

// DeleteTopic removes a topic (write grant + explicit confirm gate this in
// the handler).
func (r *Registry) DeleteTopic(ctx context.Context, conn models.KafkaConnection, topic string) error {
	adm, err := r.Admin(conn)
	if err != nil {
		return err
	}
	resp, err := adm.DeleteTopic(ctx, topic)
	if err != nil {
		return err
	}
	return resp.Err
}

// Brokers lists cluster brokers — used by the create-topic wizard to
// validate RF/min-ISR against the real broker count.
func (r *Registry) Brokers(ctx context.Context, conn models.KafkaConnection) ([]BrokerInfo, error) {
	adm, err := r.Admin(conn)
	if err != nil {
		return nil, err
	}
	brokers, err := adm.ListBrokers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]BrokerInfo, 0, len(brokers))
	for _, b := range brokers {
		rack := ""
		if b.Rack != nil {
			rack = *b.Rack
		}
		out = append(out, BrokerInfo{NodeID: b.NodeID, Host: b.Host, Port: b.Port, Rack: rack})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out, nil
}

// ListGroups returns consumer groups with their state + member count.
func (r *Registry) ListGroups(ctx context.Context, conn models.KafkaConnection) ([]GroupSummary, error) {
	adm, err := r.Admin(conn)
	if err != nil {
		return nil, err
	}
	listed, err := adm.ListGroups(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(listed))
	for name := range listed {
		names = append(names, name)
	}
	described, _ := adm.DescribeGroups(ctx, names...)
	out := make([]GroupSummary, 0, len(listed))
	for name, g := range listed {
		members := 0
		if dg, ok := described[name]; ok {
			members = len(dg.Members)
		}
		out = append(out, GroupSummary{
			Group:        name,
			State:        g.State,
			ProtocolType: g.ProtocolType,
			Members:      members,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Group < out[j].Group })
	return out, nil
}

// DescribeGroup returns a single group's state + committed offsets + lag
// (read-only; the tool never resets group offsets in v1).
func (r *Registry) DescribeGroup(ctx context.Context, conn models.KafkaConnection, group string) (*GroupDetail, error) {
	adm, err := r.Admin(conn)
	if err != nil {
		return nil, err
	}
	described, err := adm.DescribeGroups(ctx, group)
	if err != nil {
		return nil, err
	}
	dg, ok := described[group]
	if !ok {
		return nil, fmt.Errorf("group %q not found", group)
	}
	detail := &GroupDetail{
		Group:        group,
		State:        dg.State,
		ProtocolType: dg.ProtocolType,
		Protocol:     dg.Protocol,
		Members:      len(dg.Members),
		Offsets:      []GroupMemberOffset{},
	}

	committed, err := adm.FetchOffsets(ctx, group)
	if err != nil || committed.Error() != nil {
		return detail, nil // group may have no committed offsets yet
	}
	// Gather the topics this group tracks so we can fetch end offsets.
	topicSet := map[string]bool{}
	committed.Each(func(o kadm.OffsetResponse) { topicSet[o.Topic] = true })
	topics := make([]string, 0, len(topicSet))
	for t := range topicSet {
		topics = append(topics, t)
	}
	var ends kadm.ListedOffsets
	if len(topics) > 0 {
		ends, _ = adm.ListEndOffsets(ctx, topics...)
	}
	committed.Each(func(o kadm.OffsetResponse) {
		end := int64(-1)
		if ends != nil {
			if pm, ok := ends[o.Topic]; ok {
				if lo, ok := pm[o.Partition]; ok {
					end = lo.Offset
				}
			}
		}
		lag := int64(-1)
		if end >= 0 {
			lag = end - o.At
			if lag < 0 {
				lag = 0
			}
		}
		detail.Offsets = append(detail.Offsets, GroupMemberOffset{
			Topic:     o.Topic,
			Partition: o.Partition,
			Committed: o.At,
			End:       end,
			Lag:       lag,
		})
	})
	sort.Slice(detail.Offsets, func(i, j int) bool {
		if detail.Offsets[i].Topic != detail.Offsets[j].Topic {
			return detail.Offsets[i].Topic < detail.Offsets[j].Topic
		}
		return detail.Offsets[i].Partition < detail.Offsets[j].Partition
	})
	return detail, nil
}

// GroupsLag returns a cluster-wide consumer-group lag summary — one row per
// group with its state, member count, coordinator, and total lag. It lists
// the group names first (adm.ListGroups) and then calls adm.Lag on that
// explicit set. GRACEFUL DEGRADE: a per-group describe/fetch failure does
// not fail the whole call — that group is kept with Error set and
// TotalLag = -1, the rest are returned normally. If topic != "", only groups
// with at least one partition on that topic are included.
func (r *Registry) GroupsLag(ctx context.Context, conn models.KafkaConnection, topic string) ([]GroupLagSummary, error) {
	adm, err := r.Admin(conn)
	if err != nil {
		return nil, err
	}
	listed, err := adm.ListGroups(ctx)
	if err != nil {
		return nil, translateAuthErr(err)
	}
	names := listed.Groups()
	out := make([]GroupLagSummary, 0, len(names))
	if len(names) == 0 {
		return out, nil
	}
	lags, err := adm.Lag(ctx, names...)
	if err != nil {
		return nil, translateAuthErr(err)
	}
	for _, l := range lags {
		if topic != "" && !groupLagHasTopic(l.Lag, topic) {
			continue
		}
		s := GroupLagSummary{
			Group:        l.Group,
			State:        l.State,
			ProtocolType: l.ProtocolType,
			Members:      len(l.Members),
			TotalLag:     l.Lag.Total(),
			Coordinator:  l.Coordinator.NodeID,
		}
		if e := l.Error(); e != nil {
			s.Error = e.Error()
			s.TotalLag = -1
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Group < out[j].Group })
	return out, nil
}

// GroupLag returns full lag detail for a single consumer group: its state,
// members (with the partitions the leader assigned to each), and the
// per-topic/partition committed/end/lag breakdown. A group-level
// describe/fetch error is surfaced in Error (TotalLag = -1) rather than
// failing the call; a per-partition error is surfaced in that partition's
// Err with Lag = -1.
func (r *Registry) GroupLag(ctx context.Context, conn models.KafkaConnection, group string) (*GroupLagDetail, error) {
	adm, err := r.Admin(conn)
	if err != nil {
		return nil, err
	}
	lags, err := adm.Lag(ctx, group)
	if err != nil {
		return nil, translateAuthErr(err)
	}
	l, ok := lags[group]
	if !ok {
		return nil, fmt.Errorf("group %q not found", group)
	}
	detail := &GroupLagDetail{
		Group:        l.Group,
		State:        l.State,
		ProtocolType: l.ProtocolType,
		Coordinator:  l.Coordinator.NodeID,
		TotalLag:     l.Lag.Total(),
		MemberCount:  len(l.Members),
		Members:      []GroupMemberInfo{},
		Partitions:   []PartitionLag{},
	}
	if e := l.Error(); e != nil {
		detail.Error = e.Error()
		detail.TotalLag = -1
	}
	// Members and the partitions assigned to each. Empty-state groups have
	// no members (Member is also nil on the per-partition lag below).
	for _, m := range l.Members {
		info := GroupMemberInfo{
			MemberID:   m.MemberID,
			ClientID:   m.ClientID,
			ClientHost: m.ClientHost,
			Assigned:   []PartAssign{},
		}
		if m.InstanceID != nil {
			info.InstanceID = *m.InstanceID
		}
		if cm, ok := m.Assigned.AsConsumer(); ok && cm != nil {
			for _, t := range cm.Topics {
				info.Assigned = append(info.Assigned, PartAssign{
					Topic:      t.Topic,
					Partitions: t.Partitions,
				})
			}
		}
		detail.Members = append(detail.Members, info)
	}
	// Per-partition lag, sorted by (topic, partition).
	for _, ml := range l.Lag.Sorted() {
		pl := PartitionLag{
			Topic:     ml.Topic,
			Partition: ml.Partition,
			Committed: ml.Commit.At,
			End:       ml.End.Offset,
			Lag:       ml.Lag,
		}
		if ml.Member != nil {
			pl.MemberID = ml.Member.MemberID
			pl.ClientID = ml.Member.ClientID
		}
		if ml.Err != nil {
			pl.Err = ml.Err.Error()
		}
		detail.Partitions = append(detail.Partitions, pl)
	}
	return detail, nil
}

// groupLagHasTopic reports whether a group's lag map has any partition on the
// given topic (used by the ?topic= filter on the cluster lag summary).
func groupLagHasTopic(g kadm.GroupLag, topic string) bool {
	for _, ml := range g.Sorted() {
		if ml.Topic == topic {
			return true
		}
	}
	return false
}

// translateAuthErr converts a kadm *AuthError (Kafka ACL failure) into a
// message the dashboard can show plainly, instead of a raw kerr code.
func translateAuthErr(err error) error {
	var ae *kadm.AuthError
	if errors.As(err, &ae) {
		return fmt.Errorf("not authorized: describing consumer groups requires the Kafka DESCRIBE ACL on the group/cluster (%v)", ae.Err)
	}
	return err
}

// Produce publishes a single record. Partition -1 lets the client
// partition by key. An explicit partition uses a one-off client with the
// manual partitioner so the pooled client keeps its default behaviour.
func (r *Registry) Produce(ctx context.Context, conn models.KafkaConnection, req ProduceRequest) (int32, int64, error) {
	if req.Topic == "" {
		return 0, 0, fmt.Errorf("topic is required")
	}
	keyBytes, err := decodePayload(req.Key, req.KeyBase64)
	if err != nil {
		return 0, 0, fmt.Errorf("key: %w", err)
	}
	valBytes, err := decodePayload(req.Value, req.ValueBase64)
	if err != nil {
		return 0, 0, fmt.Errorf("value: %w", err)
	}
	rec := &kgo.Record{Topic: req.Topic, Value: valBytes}
	if len(keyBytes) > 0 {
		rec.Key = keyBytes
	}
	for _, h := range req.Headers {
		hv, _ := decodePayload(h.Value, false)
		rec.Headers = append(rec.Headers, kgo.RecordHeader{Key: h.Key, Value: hv})
	}

	var cl *kgo.Client
	if req.Partition >= 0 {
		rec.Partition = req.Partition
		cfg, err := r.resolve(conn)
		if err != nil {
			return 0, 0, err
		}
		opts, err := baseOpts(cfg)
		if err != nil {
			return 0, 0, err
		}
		opts = append(opts, kgo.RecordPartitioner(kgo.ManualPartitioner()))
		cl, err = kgo.NewClient(opts...)
		if err != nil {
			return 0, 0, err
		}
		defer cl.Close()
	} else {
		cl, err = r.client(conn)
		if err != nil {
			return 0, 0, err
		}
	}
	res := cl.ProduceSync(ctx, rec)
	if err := res.FirstErr(); err != nil {
		return 0, 0, err
	}
	return rec.Partition, rec.Offset, nil
}

func decodePayload(s string, isBase64 bool) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	if isBase64 {
		return base64.StdEncoding.DecodeString(s)
	}
	return []byte(s), nil
}
