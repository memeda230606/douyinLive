//go:build p3accacceptance || p5stbacceptance

package eventstore

import "math"

// AggregateQueueStats is a privacy-safe, read-only view of all queues owned by
// a Manager. It intentionally contains no session identifiers or payload data.
type AggregateQueueStats struct {
	Complete         bool
	QueueCount       int64
	ClosedQueueCount int64
	ItemCapacity     int64
	ByteCapacity     int64
	Items            int64
	Bytes            int64
	NormalItems      int64
	NormalBytes      int64
	EmergencyItems   int64
	EmergencyBytes   int64
}

// AggregateQueueStats returns a bounded aggregate without holding Manager.mu
// while acquiring an individual queue lock. That lock order is important:
// shutdown holds Manager.mu while requesting runtime closure, and closure in
// turn closes the queue. A session-set change during sampling makes the result
// incomplete instead of mixing two ownership generations.
func (m *Manager) AggregateQueueStats() AggregateQueueStats {
	if m == nil {
		return AggregateQueueStats{}
	}
	m.mu.Lock()
	generation := m.sessionSetGeneration
	valid := !m.sessionSetGenerationInvalid
	queues := make(map[string]*EnvelopeQueue, len(m.sessions))
	for sessionID, sink := range m.sessions {
		if sink == nil || sink.runtime == nil || sink.runtime.queue == nil {
			valid = false
			break
		}
		queues[sessionID] = sink.runtime.queue
	}
	m.mu.Unlock()
	if !valid {
		return AggregateQueueStats{}
	}
	return m.aggregateQueueStatsSnapshot(queues, generation)
}

func (m *Manager) aggregateQueueStatsSnapshot(queues map[string]*EnvelopeQueue, generation uint64) AggregateQueueStats {
	result := AggregateQueueStats{Complete: true, QueueCount: int64(len(queues))}
	for _, queue := range queues {
		stats, limits, sourceValid := snapshotAggregateQueueSource(queue)
		itemCapacity, byteCapacity, capacityValid := effectiveAggregateQueueCapacity(limits)
		if !sourceValid ||
			!capacityValid ||
			!addAggregateQueueValue(&result.ItemCapacity, itemCapacity) ||
			!addAggregateQueueValue(&result.ByteCapacity, byteCapacity) ||
			!addAggregateQueueValue(&result.Items, int64(stats.Items)) ||
			!addAggregateQueueValue(&result.Bytes, stats.Bytes) ||
			!addAggregateQueueValue(&result.NormalItems, int64(stats.NormalItems)) ||
			!addAggregateQueueValue(&result.NormalBytes, stats.NormalBytes) ||
			!addAggregateQueueValue(&result.EmergencyItems, int64(stats.EmergencyItems)) ||
			!addAggregateQueueValue(&result.EmergencyBytes, stats.EmergencyBytes) {
			return AggregateQueueStats{}
		}
		if stats.Closed {
			result.ClosedQueueCount++
		}
	}
	if result.Items != result.NormalItems+result.EmergencyItems ||
		result.Bytes != result.NormalBytes+result.EmergencyBytes ||
		result.Items > result.ItemCapacity || result.Bytes > result.ByteCapacity {
		return AggregateQueueStats{}
	}
	m.mu.Lock()
	result.Complete = !m.sessionSetGenerationInvalid &&
		m.sessionSetGeneration == generation &&
		len(m.sessions) == len(queues)
	if result.Complete {
		for sessionID, queue := range queues {
			sink := m.sessions[sessionID]
			if sink == nil || sink.runtime == nil || sink.runtime.queue != queue {
				result.Complete = false
				break
			}
		}
	}
	m.mu.Unlock()
	return result
}

func snapshotAggregateQueueSource(queue *EnvelopeQueue) (QueueStats, QueueLimits, bool) {
	if queue == nil {
		return QueueStats{}, QueueLimits{}, false
	}
	queue.mu.Lock()
	stats := queue.stats
	limits := queue.limits
	count := queue.count
	closed := queue.closed
	ringSize := len(queue.ring)
	queue.mu.Unlock()
	if stats.Items < 0 || stats.Bytes < 0 || stats.NormalItems < 0 ||
		stats.NormalBytes < 0 || stats.EmergencyItems < 0 || stats.EmergencyBytes < 0 ||
		limits.NormalItems <= 0 || limits.NormalBytes <= 0 || limits.EmergencyItems <= 0 ||
		limits.EmergencyBytes <= 0 || limits.TotalItems <= 0 || limits.TotalBytes <= 0 ||
		limits.MaxPayloadBytes <= 0 || limits.TotalItems < limits.NormalItems ||
		limits.TotalItems < limits.EmergencyItems || ringSize != limits.TotalItems ||
		count != stats.Items || closed != stats.Closed ||
		stats.Items > limits.TotalItems || stats.Bytes > limits.TotalBytes ||
		stats.NormalItems > limits.NormalItems || stats.NormalBytes > limits.NormalBytes ||
		stats.EmergencyItems > limits.EmergencyItems || stats.EmergencyBytes > limits.EmergencyBytes {
		return QueueStats{}, QueueLimits{}, false
	}
	if stats.Items != stats.NormalItems+stats.EmergencyItems ||
		stats.Bytes != stats.NormalBytes+stats.EmergencyBytes {
		return QueueStats{}, QueueLimits{}, false
	}
	return stats, limits, true
}

func effectiveAggregateQueueCapacity(limits QueueLimits) (int64, int64, bool) {
	normalItems := int64(limits.NormalItems)
	emergencyItems := int64(limits.EmergencyItems)
	if normalItems <= 0 || emergencyItems <= 0 ||
		normalItems > math.MaxInt64-emergencyItems ||
		limits.NormalBytes <= 0 || limits.EmergencyBytes <= 0 ||
		limits.NormalBytes > math.MaxInt64-limits.EmergencyBytes {
		return 0, 0, false
	}
	itemCapacity := normalItems + emergencyItems
	if total := int64(limits.TotalItems); total < itemCapacity {
		itemCapacity = total
	}
	byteCapacity := limits.NormalBytes + limits.EmergencyBytes
	if limits.TotalBytes < byteCapacity {
		byteCapacity = limits.TotalBytes
	}
	return itemCapacity, byteCapacity, itemCapacity > 0 && byteCapacity > 0
}

func addAggregateQueueValue(target *int64, value int64) bool {
	if target == nil || value < 0 || *target < 0 || value > math.MaxInt64-*target {
		return false
	}
	*target += value
	return true
}
