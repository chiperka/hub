package server

import (
	"strings"
	"sync"

	"chiperka-hub/internal/compile"
)

// registryAuths keeps the registry credentials a CLI submitted with a run.
// They live only in memory: they are usually short-lived CI job tokens, so they
// are never persisted, never returned by the API and dropped when the run ends.
type registryAuths struct {
	mu    sync.RWMutex
	byRun map[string][]compile.RegistryAuth
}

func newRegistryAuths() *registryAuths {
	return &registryAuths{byRun: map[string][]compile.RegistryAuth{}}
}

func (a *registryAuths) Set(runID string, auths []compile.RegistryAuth) {
	valid := make([]compile.RegistryAuth, 0, len(auths))
	for _, ra := range auths {
		ra.Host = strings.TrimSpace(ra.Host)
		if ra.Host != "" && ra.Username != "" && ra.Password != "" {
			valid = append(valid, ra)
		}
	}
	if len(valid) == 0 {
		return
	}
	a.mu.Lock()
	a.byRun[runID] = valid
	a.mu.Unlock()
}

func (a *registryAuths) Get(runID string) []compile.RegistryAuth {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.byRun[runID]
}

func (a *registryAuths) Delete(runID string) {
	a.mu.Lock()
	delete(a.byRun, runID)
	a.mu.Unlock()
}
