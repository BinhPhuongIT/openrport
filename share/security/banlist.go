package security

import (
	"sync"
	"time"

	chshare "github.com/cloudradar-monitoring/rport/share"
)

type BanList struct {
	banDuration time.Duration
	mu          sync.RWMutex
	visitors    map[string]time.Time
}

func NewBanList(banDuration time.Duration) *BanList {
	return &BanList{
		banDuration: banDuration,
		visitors:    make(map[string]time.Time),
	}
}

func (l *BanList) Add(visitorKey string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.visitors[visitorKey] = time.Now().Add(l.banDuration)
}

func (l *BanList) IsBanned(visitorKey string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	banExpiry, found := l.visitors[visitorKey]
	return found && banExpiry.After(time.Now())
}

// MaxBadAttemptsBanList bans visitors by their keys after N failed consecutive attempts for Z period.
type MaxBadAttemptsBanList struct {
	banDuration    time.Duration
	maxBadAttempts int
	mu             sync.RWMutex
	visitors       map[string]*visitor
	logger         *chshare.Logger
}

type visitor struct {
	badAttempts int
	banTime     *time.Time
}

func NewMaxBadAttemptsBanList(maxBadAttempts int, banDuration time.Duration, logger *chshare.Logger) *MaxBadAttemptsBanList {
	return &MaxBadAttemptsBanList{
		banDuration:    banDuration,
		maxBadAttempts: maxBadAttempts,
		visitors:       make(map[string]*visitor),
		logger:         logger,
	}
}

// AddBadAttempt registers a bad attempt of a visitor.
func (l *MaxBadAttemptsBanList) AddBadAttempt(visitorKey string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	v, found := l.visitors[visitorKey]
	if !found {
		v = &visitor{}
		l.visitors[visitorKey] = v
	}

	v.badAttempts++

	if v.badAttempts == l.maxBadAttempts {
		t := time.Now().Add(l.banDuration)
		if l.logger != nil {
			l.logger.Infof("Too many bad attempts. Client is banned, ip: %s, ban expiry: %s", visitorKey, t)
		}
		v.banTime = &t
		v.badAttempts = 0
	}
}

// AddBadAttempt registers a successful attempt of a visitor.
func (l *MaxBadAttemptsBanList) AddSuccessAttempt(visitorKey string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	v, found := l.visitors[visitorKey]
	if found {
		v.badAttempts = 0
		v.banTime = nil
	}
}

// IsBanned checks whether a given visitor is banned or not.
func (l *MaxBadAttemptsBanList) IsBanned(visitorKey string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	v, found := l.visitors[visitorKey]
	return found && v.banTime != nil && v.banTime.After(time.Now())
}
