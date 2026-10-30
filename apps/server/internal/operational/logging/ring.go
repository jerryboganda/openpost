package logging

import (
	"strings"
	"sync"
)

const maxRecentLineBytes = 4 << 10

// RecentLines keeps the newest log lines in memory so an administrator tool
// can inspect production behavior without shell access to the host. Lines come
// from the redacting JSON logger, so secrets never enter the buffer.
type RecentLines struct {
	mu    sync.Mutex
	lines []string
	next  int
	count int
}

// NewRecentLines returns a buffer that retains the newest capacity lines.
func NewRecentLines(capacity int) *RecentLines {
	if capacity < 1 {
		capacity = 1
	}
	return &RecentLines{lines: make([]string, capacity)}
}

// Write implements io.Writer for the JSON log stream, one record per line.
func (r *RecentLines) Write(contents []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, line := range strings.Split(string(contents), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > maxRecentLineBytes {
			line = line[:maxRecentLineBytes]
		}
		r.lines[r.next] = line
		r.next = (r.next + 1) % len(r.lines)
		if r.count < len(r.lines) {
			r.count++
		}
	}
	return len(contents), nil
}

// Snapshot returns up to limit of the newest lines that satisfy keep, oldest
// first. A nil keep accepts every line.
func (r *RecentLines) Snapshot(limit int, keep func(string) bool) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if limit < 1 {
		return nil
	}
	selected := make([]string, 0, min(limit, r.count))
	for age := 1; age <= r.count && len(selected) < limit; age++ {
		line := r.lines[(r.next-age+len(r.lines))%len(r.lines)]
		if keep == nil || keep(line) {
			selected = append(selected, line)
		}
	}
	for left, right := 0, len(selected)-1; left < right; left, right = left+1, right-1 {
		selected[left], selected[right] = selected[right], selected[left]
	}
	return selected
}
