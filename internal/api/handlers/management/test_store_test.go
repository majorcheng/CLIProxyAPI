package management

import (
	"context"
	"sync"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type memoryAuthStore struct {
	mu             sync.Mutex
	items          map[string]*coreauth.Auth
	deletedIDs     []string
	persistCalls   []memoryPersistCall
	baseDir        string
	globalProxyURL string
}

type memoryPersistCall struct {
	Message string
	Paths   []string
}

func (s *memoryAuthStore) List(_ context.Context) ([]*coreauth.Auth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]*coreauth.Auth, 0, len(s.items))
	for _, item := range s.items {
		out = append(out, item)
	}
	return out, nil
}

func (s *memoryAuthStore) Save(_ context.Context, auth *coreauth.Auth) (string, error) {
	if auth == nil {
		return "", nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.items == nil {
		s.items = make(map[string]*coreauth.Auth)
	}
	s.items[auth.ID] = auth
	return auth.ID, nil
}

func (s *memoryAuthStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.deletedIDs = append(s.deletedIDs, id)
	delete(s.items, id)
	return nil
}

func (s *memoryAuthStore) PersistAuthFiles(_ context.Context, message string, paths ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	clonedPaths := append([]string(nil), paths...)
	s.persistCalls = append(s.persistCalls, memoryPersistCall{
		Message: message,
		Paths:   clonedPaths,
	})
	for _, id := range clonedPaths {
		delete(s.items, id)
	}
	return nil
}

func (s *memoryAuthStore) SetBaseDir(dir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.baseDir = dir
}

func (s *memoryAuthStore) SetGlobalProxyURL(proxyURL string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.globalProxyURL = proxyURL
}
