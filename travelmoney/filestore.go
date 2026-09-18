package travelmoney

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// FileStore 把事件以 JSON Lines 追加到单个文件；重启后 Replay 即可恢复。
// 每行一个 Envelope（含类型与载荷），文件只追加、不修改、不删除。
type FileStore struct {
	mu sync.Mutex
	f  *os.File
	p  string
}

// OpenFileStore 打开（必要时创建）事件日志文件。
func OpenFileStore(path string) (*FileStore, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &FileStore{f: f, p: path}, nil
}

// Path 返回日志文件路径。
func (s *FileStore) Path() string { return s.p }

func (s *FileStore) Append(env Envelope, _ any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := json.Marshal(env)
	if err != nil {
		return err
	}
	if _, err := s.f.Write(append(raw, '\n')); err != nil {
		return err
	}
	return s.f.Sync()
}

func (s *FileStore) Replay(handle func(Envelope) error) error {
	s.mu.Lock()
	f, err := os.Open(s.p)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var env Envelope
			if jerr := json.Unmarshal(line, &env); jerr != nil {
				return jerr
			}
			if herr := handle(env); herr != nil {
				return herr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// Close 关闭日志文件。
func (s *FileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Close()
}
