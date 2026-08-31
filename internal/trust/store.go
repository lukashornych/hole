package trust

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// fileName is the trust record inside ~/.hole.
const fileName = "trust.json"

// record is one project's accepted grant set.
type record struct {
	// Digest is the grant set the user accepted; a wider one prompts again.
	Digest string `json:"digest"`
	// Keys are the accepted settings keys, so the file is readable without recomputing them.
	Keys      []string  `json:"keys,omitempty"`
	GrantedAt time.Time `json:"grantedAt"`
}

// trustFile is the on-disk shape, keyed by resolved project path.
type trustFile struct {
	Projects map[string]record `json:"projects"`
}

// Store records which projects' settings the user has accepted, in ~/.hole/trust.json.
//
// Its own directory is not part of any sandbox mount, so a running agent cannot grant itself
// trust for the next start.
type Store struct {
	path string
}

// NewStore returns the store backed by holeDir. The file is created on the first decision;
// a missing file simply means nothing has been trusted yet.
func NewStore(holeDir string) *Store {
	return &Store{path: filepath.Join(holeDir, fileName)}
}

// Path is the trust file path, named in messages so the user can inspect or delete it.
func (s *Store) Path() string { return s.path }

// Trusted reports whether this project has been accepted with exactly this grant set.
func (s *Store) Trusted(projectDir, digest string) bool {
	doc, err := s.read()
	if err != nil {
		// An unreadable record is treated as "nothing trusted": the failure direction has to
		// be a prompt, never a silent grant.
		return false
	}
	entry, ok := doc.Projects[projectDir]
	return ok && entry.Digest == digest
}

// Trust records a decision, replacing any earlier one for the same project.
func (s *Store) Trust(projectDir, digest string, keys []string) error {
	doc, err := s.read()
	if err != nil {
		return err
	}
	if doc.Projects == nil {
		doc.Projects = map[string]record{}
	}
	doc.Projects[projectDir] = record{Digest: digest, Keys: keys, GrantedAt: time.Now()}
	return s.write(doc)
}

func (s *Store) read() (trustFile, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return trustFile{}, nil
		}
		return trustFile{}, fmt.Errorf("read %s: %w", s.path, err)
	}
	var doc trustFile
	if err := json.Unmarshal(data, &doc); err != nil {
		return trustFile{}, fmt.Errorf("%s is not valid JSON: %w", s.path, err)
	}
	return doc, nil
}

// write replaces the file atomically, so a concurrent reader never sees a half-written record.
func (s *Store) write(doc trustFile) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(s.path), err)
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode trust record: %w", err)
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(filepath.Dir(s.path), fileName+".*")
	if err != nil {
		return fmt.Errorf("create temporary trust record: %w", err)
	}
	tempPath := temp.Name()
	defer func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}()
	if err := temp.Chmod(0o600); err != nil {
		return fmt.Errorf("set permissions on %s: %w", tempPath, err)
	}
	if _, err := temp.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", tempPath, err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", tempPath, err)
	}
	if err := os.Rename(tempPath, s.path); err != nil {
		return fmt.Errorf("replace %s: %w", s.path, err)
	}
	return nil
}
