package runner

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sbraznikov/istari/internal/harness"
)

// A transcript is a run's events, one JSON object per line, in numbered
// parts: <run>.jsonl, then <run>.1.jsonl, <run>.2.jsonl… A part that grows
// past the size limit is closed and the next one begins; parts are never
// rewritten, so a reader's position stays valid (spec section 10: run output
// in rotated files, the log refers to them).
type transcript struct {
	path string // part 0
	max  int64
	part int
	f    *os.File
	size int64
}

// partPath returns the path of part n of the transcript at path.
func partPath(path string, n int) string {
	if n == 0 {
		return path
	}
	return strings.TrimSuffix(path, ".jsonl") + "." + strconv.Itoa(n) + ".jsonl"
}

func (t *transcript) write(e harness.Event) error {
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if t.f != nil && t.size > 0 && t.size+int64(len(line)) > t.max {
		t.f.Close()
		t.f, t.size = nil, 0
		t.part++
	}
	if t.f == nil {
		if err := os.MkdirAll(filepath.Dir(t.path), 0o700); err != nil {
			return err
		}
		f, err := os.OpenFile(partPath(t.path, t.part), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		t.f = f
	}
	n, err := t.f.Write(line) // one write per line: a reader never sees half of one
	t.size += int64(n)
	return err
}

func (t *transcript) close() {
	if t.f != nil {
		t.f.Close()
	}
}

// TranscriptPos is a reader's position in a transcript.
type TranscriptPos struct {
	Part   int   `json:"part"`
	Offset int64 `json:"offset"`
}

// ReadTranscript returns the complete lines after pos, across parts, and the
// position after them.
func ReadTranscript(path string, pos TranscriptPos) ([]json.RawMessage, TranscriptPos, error) {
	lines := []json.RawMessage{}
	for {
		f, err := os.Open(partPath(path, pos.Part))
		if errors.Is(err, os.ErrNotExist) {
			return lines, pos, nil
		}
		if err != nil {
			return nil, pos, err
		}
		if _, err := f.Seek(pos.Offset, io.SeekStart); err != nil {
			f.Close()
			return nil, pos, err
		}
		r := bufio.NewReader(f)
		for {
			line, err := r.ReadBytes('\n')
			if err != nil { // not a complete line yet
				break
			}
			pos.Offset += int64(len(line))
			lines = append(lines, json.RawMessage(line[:len(line)-1]))
		}
		f.Close()
		if _, err := os.Stat(partPath(path, pos.Part+1)); err != nil {
			return lines, pos, nil
		}
		pos = TranscriptPos{Part: pos.Part + 1} // this part is closed: go on with the next
	}
}
