//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// timestamped matches a line that already starts with a time, like progress lines.
var timestamped = regexp.MustCompile(`^\d\d:\d\d:\d\d `)

// logFilePath is E2E_LOG_FILE, or e2e.log next to the tests.
func logFilePath() string {
	if path := os.Getenv("E2E_LOG_FILE"); path != "" {
		return path
	}

	return filepath.Join(repoRoot, "test", "e2e", "e2e.log")
}

// teeOutput sends everything the process writes to stdout and stderr on to
// where it went before, and also into the file at path with the time in front
// of every line, so a run can be followed with tail -f however it was started.
// That includes the testing package's own lines: it picks up os.Stdout when
// m.Run starts. A file already at path is kept as <name>.prev.log. The
// returned function restores stdout and stderr and closes the file.
func teeOutput(path string) func() {
	if _, err := os.Stat(path); err == nil {
		ext := filepath.Ext(path)
		_ = os.Rename(path, path[:len(path)-len(ext)]+".prev"+ext)
	}

	file, err := os.Create(path)
	if err != nil {
		progress("not writing a log file: %v", err)
		return func() {}
	}

	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)

	// tee replaces orig with a pipe whose lines go to orig and to the file.
	tee := func(orig *os.File) *os.File {
		reader, writer, err := os.Pipe()
		if err != nil {
			return orig
		}

		wg.Add(1)

		go func() {
			defer wg.Done()

			lines := bufio.NewReader(reader)

			for {
				line, err := lines.ReadBytes('\n')
				if len(line) > 0 {
					_, _ = orig.Write(line)

					// test2json framing bytes (0x16) mean nothing in a plain file.
					text := bytes.TrimRight(bytes.ReplaceAll(line, []byte{0x16}, nil), "\n")

					mu.Lock()
					if timestamped.Match(text) {
						fmt.Fprintf(file, "%s\n", text)
					} else {
						fmt.Fprintf(file, "%s %s\n", time.Now().Format("15:04:05"), text)
					}
					mu.Unlock()
				}

				if err != nil {
					return
				}
			}
		}()

		return writer
	}

	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout = tee(origOut)
	os.Stderr = tee(origErr)

	progress("writing a copy of all output to %s", path)

	return func() {
		pipedOut, pipedErr := os.Stdout, os.Stderr
		os.Stdout, os.Stderr = origOut, origErr

		for _, piped := range []*os.File{pipedOut, pipedErr} {
			if piped != origOut && piped != origErr {
				_ = piped.Close()
			}
		}

		wg.Wait()
		_ = file.Close()
	}
}
