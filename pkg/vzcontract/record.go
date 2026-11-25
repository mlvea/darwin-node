package vzcontract

import (
	"encoding/json"
	"io"
	"path/filepath"
	"runtime"
)

// TestdataDir is the seed trace directory next to this source file.
func TestdataDir() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return filepath.Join("testdata", "vz_traces")
	}
	return filepath.Join(filepath.Dir(file), "testdata", "vz_traces")
}

// Append writes one JSON object and a newline.
func Append(w io.Writer, line Line) error {
	if w == nil {
		return io.ErrClosedPipe
	}
	buf, err := json.Marshal(line)
	if err != nil {
		return err
	}
	buf = append(buf, '\n')
	_, err = w.Write(buf)
	return err
}
