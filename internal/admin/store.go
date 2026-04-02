package admin

import (
	"sort"
	"sync"
	"time"
)

// ─── Log entry ────────────────────────────────────────────────────────────────

type LogEntry struct {
	ID         string    `json:"id"`
	Timestamp  time.Time `json:"timestamp"`
	IP         string    `json:"ip"`
	Method     string    `json:"method"`
	Path       string    `json:"path"`
	Decision   string    `json:"decision"` // "allow" | "block"
	Reason     string    `json:"reason"`
	StatusCode int       `json:"status_code"`
	LatencyMs  int64     `json:"latency_ms"`
}

// ─── Metric buckets (one per minute) ─────────────────────────────────────────

type Bucket struct {
	Timestamp    time.Time `json:"timestamp"`
	RequestCount int64
	BlockCount   int64
	LatencySum   int64
	LatencyCount int64
	Latencies    []int64 // kept to compute percentiles, capped at 200 per bucket
	Status2xx    int64
	Status3xx    int64
	Status4xx    int64
	Status5xx    int64
}

func (b *Bucket) addLatency(ms int64) {
	b.LatencySum += ms
	b.LatencyCount++
	if len(b.Latencies) < 200 {
		b.Latencies = append(b.Latencies, ms)
	}
}

func (b *Bucket) percentile(p float64) float64 {
	if len(b.Latencies) == 0 {
		return 0
	}
	sorted := make([]int64, len(b.Latencies))
	copy(sorted, b.Latencies)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(float64(len(sorted)-1) * p)
	return float64(sorted[idx])
}

func (b *Bucket) avgLatency() float64 {
	if b.LatencyCount == 0 {
		return 0
	}
	return float64(b.LatencySum) / float64(b.LatencyCount)
}

// ─── Route stats ──────────────────────────────────────────────────────────────

type RouteStat struct {
	Route        string
	Method       string
	RequestCount int64
	TotalLatency int64
	ErrorCount   int64
	MaxLatency   int64
	Latencies    []int64
}

// ─── Store ────────────────────────────────────────────────────────────────────

type Store struct {
	mu        sync.RWMutex
	logs      []LogEntry
	maxLogs   int
	buckets   []Bucket
	maxBuckets int
	routes    map[string]*RouteStat // key: "METHOD /path"
	startTime time.Time
	logSeq    int64
}

func NewStore() *Store {
	return &Store{
		logs:       make([]LogEntry, 0, 1000),
		maxLogs:    1000,
		buckets:    make([]Bucket, 0, 1440),
		maxBuckets: 1440,
		routes:     make(map[string]*RouteStat),
		startTime:  time.Now(),
	}
}

// AddLog records an incoming request. Thread-safe.
func (s *Store) AddLog(e LogEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.logSeq++

	if len(s.logs) >= s.maxLogs {
		s.logs = s.logs[1:]
	}
	s.logs = append(s.logs, e)

	s.updateBucket(e)
	s.updateRoute(e)
}

func (s *Store) updateBucket(e LogEntry) {
	ts := e.Timestamp.Truncate(time.Minute)

	if len(s.buckets) == 0 || s.buckets[len(s.buckets)-1].Timestamp.Before(ts) {
		if len(s.buckets) >= s.maxBuckets {
			s.buckets = s.buckets[1:]
		}
		s.buckets = append(s.buckets, Bucket{Timestamp: ts})
	}

	b := &s.buckets[len(s.buckets)-1]
	b.RequestCount++
	b.addLatency(e.LatencyMs)

	if e.Decision == "block" {
		b.BlockCount++
	}

	switch {
	case e.StatusCode >= 500:
		b.Status5xx++
	case e.StatusCode >= 400:
		b.Status4xx++
	case e.StatusCode >= 300:
		b.Status3xx++
	default:
		b.Status2xx++
	}
}

func (s *Store) updateRoute(e LogEntry) {
	key := e.Method + " " + e.Path
	rs, ok := s.routes[key]
	if !ok {
		rs = &RouteStat{Route: e.Path, Method: e.Method}
		s.routes[key] = rs
	}
	rs.RequestCount++
	rs.TotalLatency += e.LatencyMs
	if e.LatencyMs > rs.MaxLatency {
		rs.MaxLatency = e.LatencyMs
	}
	if e.StatusCode >= 400 {
		rs.ErrorCount++
	}
	if len(rs.Latencies) < 500 {
		rs.Latencies = append(rs.Latencies, e.LatencyMs)
	} else {
		// circular replacement
		rs.Latencies[rs.RequestCount%500] = e.LatencyMs
	}
}

// ─── Query methods ────────────────────────────────────────────────────────────

func (s *Store) GetLogs(filter string, limit int) []LogEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]LogEntry, 0, limit)
	for i := len(s.logs) - 1; i >= 0 && len(result) < limit; i-- {
		l := s.logs[i]
		if filter == "all" || l.Decision == filter {
			result = append(result, l)
		}
	}
	return result
}

func (s *Store) GetBucketsSince(since time.Time) []Bucket {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]Bucket, 0)
	for _, b := range s.buckets {
		if !b.Timestamp.Before(since) {
			result = append(result, b)
		}
	}
	return result
}

func (s *Store) GetTotals() (total, blocked int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, b := range s.buckets {
		total += b.RequestCount
		blocked += b.BlockCount
	}
	return
}

func (s *Store) GetTopRoutes(limit int) []RouteStat {
	s.mu.RLock()
	defer s.mu.RUnlock()

	all := make([]RouteStat, 0, len(s.routes))
	for _, rs := range s.routes {
		all = append(all, *rs)
	}
	sort.Slice(all, func(i, j int) bool {
		return all[i].RequestCount > all[j].RequestCount
	})
	if len(all) > limit {
		all = all[:limit]
	}
	return all
}

func (s *Store) GetStatusTotals() (s2xx, s3xx, s4xx, s5xx int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, b := range s.buckets {
		s2xx += b.Status2xx
		s3xx += b.Status3xx
		s4xx += b.Status4xx
		s5xx += b.Status5xx
	}
	return
}

func (s *Store) StartTime() time.Time { return s.startTime }
