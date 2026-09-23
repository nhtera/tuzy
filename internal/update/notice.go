package update

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// NoticeInterval is how often the notice checks GitHub and mentions an update, at most.
const NoticeInterval = 24 * time.Hour

type noticeCache struct {
	CheckedAt  time.Time `json:"checked_at"`
	Latest     string    `json:"latest"`
	NotifiedAt time.Time `json:"notified_at"`
}

// NoticePath is $UserCacheDir/tuzy/update.json.
func NoticePath() string {
	d, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "tuzy", "update.json")
}

func readCache(p string) noticeCache {
	var c noticeCache
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	return c
}

func writeCache(p string, c noticeCache) {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	b, _ := json.Marshal(c)
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	tmp := p + "." + hex.EncodeToString(rnd[:]) + ".tmp" // unique: refresh and notice may race
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, p)
	}
}

// RefreshNotice looks up the latest release when the cached answer is older than a day. It never
// blocks for more than 2 s; failures are silent.
func RefreshNotice(ctx context.Context, u *Updater, p string, now time.Time) {
	if p == "" {
		return
	}
	c := readCache(p)
	if now.Sub(c.CheckedAt) < NoticeInterval {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	v, err := u.Latest(ctx)
	c.CheckedAt = now
	if err == nil {
		c.Latest = v
	}
	writeCache(p, c)
}

// PendingNotice returns "vX.Y.Z" when a newer release is known and the user wasn't told today.
func PendingNotice(u *Updater, p string, now time.Time) string {
	if p == "" {
		return ""
	}
	c := readCache(p)
	if c.Latest == "" || !u.Newer(c.Latest) || now.Sub(c.NotifiedAt) < NoticeInterval {
		return ""
	}
	c.NotifiedAt = now
	writeCache(p, c)
	return c.Latest
}
