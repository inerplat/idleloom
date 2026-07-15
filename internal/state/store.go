package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

var ErrDeviceBusy = errors.New("device is prepared for another ResourceClaim")

type PreparedClaim struct {
	UID       string `json:"uid"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Pool      string `json:"pool"`
	Device    string `json:"device"`
}

type diskState struct {
	Prepared map[string]PreparedClaim `json:"prepared"`
}

type Store struct {
	mu    sync.Mutex
	path  string
	state diskState
}

func Open(path string) (*Store, error) {
	s := &Store{
		path:  path,
		state: diskState{Prepared: map[string]PreparedClaim{}},
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("read state: %w", err)
	}
	if err := json.Unmarshal(data, &s.state); err != nil {
		return nil, fmt.Errorf("decode state: %w", err)
	}
	if s.state.Prepared == nil {
		s.state.Prepared = map[string]PreparedClaim{}
	}
	return s, nil
}

func (s *Store) Reserve(claim PreparedClaim) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.state.Prepared[claim.UID]; ok {
		if existing.Pool == claim.Pool && existing.Device == claim.Device {
			return nil
		}
		return fmt.Errorf("claim %s is already prepared for %s/%s", claim.UID, existing.Pool, existing.Device)
	}
	for _, existing := range s.state.Prepared {
		if existing.Pool == claim.Pool && existing.Device == claim.Device {
			return fmt.Errorf("%w: %s/%s is owned by claim %s", ErrDeviceBusy, claim.Pool, claim.Device, existing.UID)
		}
	}

	s.state.Prepared[claim.UID] = claim
	if err := s.persistLocked(); err != nil {
		delete(s.state.Prepared, claim.UID)
		return err
	}
	return nil
}

func (s *Store) Release(uid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state.Prepared[uid]; !ok {
		return nil
	}
	claim := s.state.Prepared[uid]
	delete(s.state.Prepared, uid)
	if err := s.persistLocked(); err != nil {
		s.state.Prepared[uid] = claim
		return err
	}
	return nil
}

func (s *Store) persistLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".prepared-claims-*")
	if err != nil {
		return fmt.Errorf("create temporary state: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		return errors.Join(fmt.Errorf("chmod temporary state: %w", err), tmp.Close())
	}
	if _, err := tmp.Write(data); err != nil {
		return errors.Join(fmt.Errorf("write temporary state: %w", err), tmp.Close())
	}
	if err := tmp.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync temporary state: %w", err), tmp.Close())
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary state: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	dir, err := os.Open(filepath.Dir(s.path))
	if err != nil {
		return fmt.Errorf("open state directory: %w", err)
	}
	if err := dir.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync state directory: %w", err), dir.Close())
	}
	return dir.Close()
}
