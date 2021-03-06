// Package backoff provides a tool for repeatedly calling a function until it
// returns a nil error.  It implements exponential backoff with a defined
// maximum value, along with some time randomization.
package backoff

import (
	"log"
	"math/rand"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	pusherRetries = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pusher_retries_total",
			Help: "The number of times we have retried the function",
		},
		[]string{"function"},
	)
	pusherMaxRetries = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pusher_max_retries_total",
			Help: "The number of times we have hit the max backoff time when retrying the function",
		},
		[]string{"function"},
	)
)

func init() {
	prometheus.MustRegister(pusherRetries)
	prometheus.MustRegister(pusherMaxRetries)
}

// Retry retries calling a function until the function returns a non-nil error.
// It increments two prometheus counters to keep track of how many errors it has
// seen: one for all errors, and just when the max error count has been reached.
// The counters are indexed by the passed-in label. For best results, make sure
// that maxBackoff > 2*initialBackoff.
func Retry(f func() error, initialBackoff, maxBackoff time.Duration, label string) {
	waitTime := initialBackoff
	for err := f(); err != nil; err = f() {
		if waitTime > maxBackoff {
			pusherMaxRetries.WithLabelValues(label).Inc()
			ns := maxBackoff.Nanoseconds()
			waitTime = time.Duration((ns/2)+rand.Int63n(ns/2)) * time.Nanosecond
		}
		log.Printf("Call to %s failed (error: %q), will retry after %s", label, err, waitTime.String())
		pusherRetries.WithLabelValues(label).Inc()
		time.Sleep(waitTime)
		waitTime *= 2
	}
}
