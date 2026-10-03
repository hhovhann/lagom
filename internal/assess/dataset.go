package assess

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"lagom/internal/providers"
)

// maxExamples bounds a dataset so one file cannot trigger an unbounded number of paid calls.
const maxExamples = 5000

const (
	maxFileBytes  = 20 << 20  // one PDF or image
	maxTotalBytes = 512 << 20 // all files of a dataset, held in memory
)

var fileTypes = map[string]string{
	".pdf": "application/pdf", ".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".webp": "image/webp",
}

// LoadJSONL reads labelled examples, one JSON object per line:
//
//	{"task":"classify","prompt":"Label this ticket ...","verify":"equals:billing"}
//
// A line may add "files": ["docs/t12-001.pdf"] to send PDFs or images with the prompt.
// Paths are relative to the current directory; use LoadJSONLDir to anchor them elsewhere.
func LoadJSONL(r io.Reader) ([]Example, error) { return LoadJSONLDir(r, ".") }

// LoadJSONLDir is LoadJSONL with file paths resolved against dir (normally the dataset's
// own folder). A path may not be absolute or climb out of dir.
func LoadJSONLDir(r io.Reader, dir string) ([]Example, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	var out []Example
	total := 0
	for line := 1; sc.Scan(); line++ {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var ex Example
		if err := json.Unmarshal(sc.Bytes(), &ex); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if ex.Task == "" || ex.Prompt == "" || ex.Verify == "" {
			return nil, fmt.Errorf("line %d: task, prompt and verify are all required", line)
		}
		if len(out) >= maxExamples {
			return nil, fmt.Errorf("more than %d examples", maxExamples)
		}
		for _, f := range ex.Files {
			a, n, err := loadAttachment(dir, f)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", line, err)
			}
			if total += n; total > maxTotalBytes {
				return nil, fmt.Errorf("line %d: the dataset's files exceed %d MB in total", line, maxTotalBytes>>20)
			}
			ex.Attachments = append(ex.Attachments, a)
		}
		out = append(out, ex)
	}
	return out, sc.Err()
}

func loadAttachment(dir, name string) (providers.Attachment, int, error) {
	mime, ok := fileTypes[strings.ToLower(filepath.Ext(name))]
	if !ok {
		return providers.Attachment{}, 0, fmt.Errorf("file %q: use .pdf, .png, .jpg, .gif or .webp", name)
	}
	if !filepath.IsLocal(name) {
		return providers.Attachment{}, 0, fmt.Errorf("file %q must be a relative path inside the dataset folder", name)
	}
	path := filepath.Join(dir, name)
	st, err := os.Stat(path)
	if err != nil {
		return providers.Attachment{}, 0, fmt.Errorf("file %q: %w", name, err)
	}
	if st.Size() > maxFileBytes {
		return providers.Attachment{}, 0, fmt.Errorf("file %q is %d MB; the limit is %d MB", name, st.Size()>>20, maxFileBytes>>20)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return providers.Attachment{}, 0, fmt.Errorf("file %q: %w", name, err)
	}
	return providers.Attachment{MediaType: mime, Data: base64.StdEncoding.EncodeToString(b), Name: filepath.Base(name)}, len(b), nil
}
