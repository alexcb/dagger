package core

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

type acbDump struct {
	w       *os.File
	doClose bool
}

func (a acbDump) Write(p []byte) (int, error) {
	return a.w.Write(p)
}

func (a acbDump) Close() error {
	if a.doClose {
		return a.w.Close()
	}
	return nil
}

func newACBDumpStdout() io.WriteCloser {
	return &acbDump{
		w: os.Stdout,
	}
}
func newACBDumpStderr() io.WriteCloser {
	return &acbDump{
		w: os.Stderr,
	}
}

func newACBDumpFile(p string) io.WriteCloser {
	w, err := os.Create(p)
	if err != nil {
		panic(err)
	}
	return &acbDump{
		w:       w,
		doClose: true,
	}
}

func sanitizeFilename(input string) string {
	illegalChars := `<>:"/\|?*`

	// not actually illegal, but i dont want spaces
	illegalChars += ` `

	// Map replaces any illegal character with a hyphen '-'
	escapeFn := func(r rune) rune {
		if strings.ContainsRune(illegalChars, r) || r < 32 {
			return '-'
		}
		return r
	}

	return strings.Map(escapeFn, input)
}

func newDumpFilePair(name string) (io.WriteCloser, io.WriteCloser) {
	if name != "" {
		name += "."
		name = sanitizeFilename(name)
	}
	now := time.Now().UnixNano()
	stdoutPath := fmt.Sprintf("/tmp/%s%v.stdout", name, now)
	stderrPath := fmt.Sprintf("/tmp/%s%v.stderr", name, now)
	fmt.Printf("ACB Dumping to %s and %s\n", stdoutPath, stderrPath)
	return newACBDumpFile(stdoutPath), newACBDumpFile(stderrPath)
}
