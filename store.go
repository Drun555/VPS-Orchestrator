package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type Store struct {
	mu    sync.RWMutex
	path  string
	state State
}

func NewStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	store := &Store{path: path, state: State{Version: stateVersion, Servers: []Server{}, Users: []User{}}}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read state: %w", err)
		}
		if err := store.saveLocked(store.state); err != nil {
			return nil, err
		}
		return store, nil
	}
	if err := json.Unmarshal(data, &store.state); err != nil {
		return nil, fmt.Errorf("parse state: %w", err)
	}
	if store.state.Version == 1 {
		backupPath := path + ".v1.bak"
		backup, err := os.OpenFile(backupPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(err, os.ErrExist) {
			previous, readErr := os.ReadFile(backupPath)
			if readErr != nil || !bytes.Equal(previous, data) {
				return nil, fmt.Errorf("migration backup exists with different contents; inspect %s", backupPath)
			}
		} else if err != nil {
			return nil, fmt.Errorf("create migration backup: %w", err)
		} else {
			_, writeErr := backup.Write(data)
			syncErr := backup.Sync()
			closeErr := backup.Close()
			if writeErr != nil {
				return nil, writeErr
			}
			if syncErr != nil {
				return nil, syncErr
			}
			if closeErr != nil {
				return nil, closeErr
			}
		}
		for i := range store.state.Users {
			store.state.Users[i].ServerMode = "all"
			store.state.Users[i].ServerIDs = []string{}
		}
		store.state.Version = stateVersion
		if err := store.saveLocked(store.state); err != nil {
			return nil, err
		}
	}
	if store.state.Version != stateVersion {
		return nil, fmt.Errorf("unsupported state version %d", store.state.Version)
	}
	if store.state.Servers == nil {
		store.state.Servers = []Server{}
	}
	if store.state.Users == nil {
		store.state.Users = []User{}
	}
	for index := range store.state.Users {
		if store.state.Users[index].Links == nil {
			store.state.Users[index].Links = make(map[string]UserLink)
		}
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("protect state: %w", err)
	}
	return store, nil
}

func (s *Store) Snapshot() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneState(s.state)
}

func (s *Store) Update(update func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := cloneState(s.state)
	if err := update(&next); err != nil {
		return err
	}
	if err := s.saveLocked(next); err != nil {
		return err
	}
	s.state = next
	return nil
}

func (s *Store) saveLocked(state State) error {
	directory := filepath.Dir(s.path)
	temporary, err := os.CreateTemp(directory, ".master.json.*")
	if err != nil {
		return fmt.Errorf("create temporary state: %w", err)
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()

	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("protect temporary state: %w", err)
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(state); err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close state: %w", err)
	}
	if err := os.Rename(temporaryPath, s.path); err != nil {
		return fmt.Errorf("commit state: %w", err)
	}
	committed = true
	return os.Chmod(s.path, 0o600)
}

func cloneState(state State) State {
	data, err := json.Marshal(state)
	if err != nil {
		panic(err)
	}
	var cloned State
	if err := json.Unmarshal(data, &cloned); err != nil {
		panic(err)
	}
	return cloned
}
