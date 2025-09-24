package acbutil

import "sync"

var m map[string]struct{}
var mu sync.Mutex

func init() {
	m = map[string]struct{}{}
}

func Mark(s string) {
	mu.Lock()
	defer mu.Unlock()
	m[s] = struct{}{}
}

func IsInteresting(s string) bool {
	mu.Lock()
	defer mu.Unlock()

	_, found := m[s]
	return found
}
