package tunnel

import (
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

const (
	backoffBase = 250 * time.Millisecond
	backoffMax  = 30 * time.Second
	// After close 1012 (edge deploy) the first retry is uniform in [0, restartJitter) to spread
	// the reconnect storm of every agent at once.
	restartJitter = 3 * time.Second
)

// backoff computes full-jitter exponential delays: U(0, min(max, base·2^attempt)).
type backoff struct {
	rand func() float64
}

func newBackoff() *backoff { return &backoff{rand: rand.Float64} }

func (b *backoff) delay(attempt int) time.Duration {
	ceiling := backoffMax
	if attempt < 30 {
		if d := backoffBase << attempt; d < ceiling {
			ceiling = d
		}
	}
	return time.Duration(b.rand() * float64(ceiling))
}

func (b *backoff) restart() time.Duration {
	return time.Duration(b.rand() * float64(restartJitter))
}

// retryAfter parses a Retry-After header (seconds or HTTP date); 0 when absent/invalid.
func retryAfter(h http.Header, now time.Time) time.Duration {
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return min(time.Duration(secs)*time.Second, 10*time.Minute)
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return min(t.Sub(now), 10*time.Minute)
	}
	return 0
}
