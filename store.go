package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Store 以单个 JSON 文件持久化状态；写入走临时文件 + rename，保证崩溃/重启后状态完整。
type Store struct {
	path string
}

func NewStore(dataDir string) *Store {
	return &Store{path: filepath.Join(dataDir, "state.json")}
}

func (st *Store) Load() (*State, error) {
	b, err := os.ReadFile(st.path)
	if err != nil {
		if os.IsNotExist(err) {
			return newState(), nil
		}
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	if s.Accounts == nil {
		return newState(), nil
	}
	return &s, nil
}

func (st *Store) Save(s *State) error {
	if err := os.MkdirAll(filepath.Dir(st.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, st.path)
}

func newState() *State {
	return &State{
		Accounts:      map[string]*Account{},
		Events:        map[string]*ProcessedEvent{},
		RefIndex:      map[string]string{},
		Disputes:      map[string]*Dispute{},
		Notifications: map[string][]*Notification{},
		FX:            map[string]*FXPair{},
	}
}
