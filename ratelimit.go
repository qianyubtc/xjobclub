package main

import (
	"sync"
	"time"
)

// limiter 简单的内存滑动窗口限流（单进程够用）。
type limiter struct {
	mu   sync.Mutex
	m    map[string][]int64
	last time.Time
}

func newLimiter() *limiter { return &limiter{m: map[string][]int64{}, last: time.Now()} }

// count 窗口内已记录的次数（不计入）。
func (l *limiter) count(key string, window time.Duration) int {
	cut := time.Now().Add(-window).UnixNano()
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, t := range l.m[key] {
		if t >= cut {
			n++
		}
	}
	return n
}

func (l *limiter) allow(key string, max int, window time.Duration) bool {
	now := time.Now()
	cut := now.Add(-window).UnixNano()
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.last) > 10*time.Minute || len(l.m) > 50000 {
		for k, ts := range l.m {
			if len(ts) == 0 || ts[len(ts)-1] < now.Add(-time.Hour).UnixNano() {
				delete(l.m, k)
			}
		}
		l.last = now
	}
	ts := l.m[key]
	i := 0
	for i < len(ts) && ts[i] < cut {
		i++
	}
	ts = ts[i:]
	if len(ts) >= max {
		l.m[key] = ts
		return false
	}
	l.m[key] = append(ts, now.UnixNano())
	return true
}
