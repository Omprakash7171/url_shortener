// Package analytics records redirect events onto a Redis stream. Enqueueing is
// non-blocking: a slow or dead Redis must never stall a redirect response.
package analytics

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"

	"urlshortener/internal/metrics"
)

const (
	// bufferSize bounds in-flight events per process; beyond this the recorder
	// drops events (counted) instead of blocking the request path.
	bufferSize = 4096
	// pushTimeout caps the XADD attempt per event batch item.
	pushTimeout = 200 * time.Millisecond
)

type Event struct {
	Code     string
	Referrer string
	TS       time.Time
}

type Recorder struct {
	stream string
	rdb    redis.UniversalClient
	ch     chan Event
}

func NewRecorder(rdb redis.UniversalClient, stream string) *Recorder {
	r := &Recorder{rdb: rdb, stream: stream, ch: make(chan Event, bufferSize)}
	go r.run()
	return r
}

// Push enqueues an event without blocking. It returns false when the buffer is
// full and the event is dropped.
func (r *Recorder) Push(e Event) bool {
	select {
	case r.ch <- e:
		return true
	default:
		metrics.EventsDropped.Inc()
		return false
	}
}

func (r *Recorder) run() {
	for e := range r.ch {
		ctx, cancel := context.WithTimeout(context.Background(), pushTimeout)
		_, err := r.rdb.XAdd(ctx, &redis.XAddArgs{
			Stream: r.stream,
			Values: map[string]any{
				"code":     e.Code,
				"referrer": e.Referrer,
				"ts":       e.TS.Unix(),
			},
		}).Result()
		cancel()
		if err != nil {
			metrics.EventsFailed.Inc()
			continue
		}
		metrics.EventsPublished.Inc()
	}
}