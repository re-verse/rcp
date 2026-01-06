// rcp.go
package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
)

const defaultMaxBytes = 100000

func usage() {
	fmt.Fprintln(os.Stderr, `rcp - copy text to clipboard via OSC52 (works over SSH/tmux when supported)

Usage:
  rcp <file>         Copy a file's contents
  rcp                Copy stdin if piped (e.g., command | rcp)
  rcp -              Copy stdin explicitly

Extras:
  rcp -c <file>      Copy: "cat <file>" + newline + file contents
  rcp -e "command"   Copy: "<command>" + newline + command output

Notes:
  - If you run rcp with no args on a normal terminal (no pipe), it shows this help.
  - -c only makes sense with a filename (stdin has no name).
  - -e runs the command using: bash -c "<command>"

Env:
  RCOPY_MAX_BYTES=100000
`)
	os.Exit(2)
}

func getenvInt(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func isStdinPiped() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) == 0
}

type tooLargeError struct {
	got int
	max int
}

func (e tooLargeError) Error() string { return "too large" }
func errTooLarge(got, max int) error  { return tooLargeError{got: got, max: max} }

func asTooLarge(err error) (tooLargeError, bool) {
	var e tooLargeError
	if errors.As(err, &e) {
		return e, true
	}
	return tooLargeError{}, false
}

type limitedBuffer struct {
	buf bytes.Buffer
	n   int
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if l.n+len(p) > l.max {
		return 0, errTooLarge(l.n+len(p), l.max)
	}
	n, err := l.buf.Write(p)
	l.n += n
	return n, err
}

func copyLimited(dst *limitedBuffer, r io.Reader) error {
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
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

func printTooLargeOrDie(err error, maxBytes int, hint string) {
	if e, ok := asTooLarge(err); ok {
		got := e.got
		if hint == "" {
			hint = "<input>"
		}
		fmt.Fprintf(os.Stderr, "rcp: %d bytes exceeds limit %d. Refusing.\n\n", got, maxBytes)
		fmt.Fprintf(os.Stderr, "Tip:\n  RCOPY_MAX_BYTES=%d rcp %s\n\n(Or export RCOPY_MAX_BYTES for this shell.)\n",
			got+1024, hint)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func main() {
	withCmd := flag.Bool("c", false, "prepend `cat <file>` before file contents")
	execCmd := flag.String("e", "", "run command via bash -c and prepend the command")
	help := flag.Bool("h", false, "help")
	flag.Usage = usage
	flag.Parse()

	// support "/?" and "-?" like the bash version
	for _, a := range os.Args[1:] {
		if a == "/?" || a == "-?" || a == "--help" {
			usage()
		}
	}
	if *help {
		usage()
	}

	maxBytes := getenvInt("RCOPY_MAX_BYTES", defaultMaxBytes)

	// Validate combos
	if *execCmd != "" && *withCmd {
		fmt.Fprintln(os.Stderr, "rcp: -c can't be used with -e")
		os.Exit(2)
	}

	args := flag.Args()

	mode := ""
	src := ""

	if *execCmd != "" {
		mode = "exec"
	} else if len(args) >= 1 {
		if args[0] == "-" {
			mode = "stdin"
		} else {
			mode = "file"
			src = args[0]
		}
	} else {
		if isStdinPiped() {
			mode = "stdin"
		} else {
			usage()
		}
	}

	var out limitedBuffer
	out.max = maxBytes

	switch mode {
	case "exec":
		if _, err := out.Write([]byte(*execCmd + "\n")); err != nil {
			printTooLargeOrDie(err, maxBytes, "")
		}

		cmd := exec.Command("bash", "-c", *execCmd)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			printTooLargeOrDie(err, maxBytes, "")
		}
		cmd.Stderr = os.Stderr

		if err := cmd.Start(); err != nil {
			printTooLargeOrDie(err, maxBytes, "")
		}

		if err := copyLimited(&out, stdout); err != nil {
			printTooLargeOrDie(err, maxBytes, "<input>")
		}

		if err := cmd.Wait(); err != nil {
			// Command failed; still exit non-zero
			printTooLargeOrDie(err, maxBytes, "")
		}

	case "stdin":
		if *withCmd {
			fmt.Fprintln(os.Stderr, "rcp: -c only works with a filename (rcp -c <file>)")
			os.Exit(2)
		}
		if err := copyLimited(&out, os.Stdin); err != nil {
			printTooLargeOrDie(err, maxBytes, "<input>")
		}

	case "file":
		f, err := os.Open(src)
		if err != nil {
			fmt.Fprintf(os.Stderr, "rcp: not a file: %s\n", src)
			os.Exit(1)
		}
		defer f.Close()

		if *withCmd {
			if _, err := out.Write([]byte("cat " + src + "\n")); err != nil {
				printTooLargeOrDie(err, maxBytes, src)
			}
		}

		if err := copyLimited(&out, f); err != nil {
			printTooLargeOrDie(err, maxBytes, src)
		}

	default:
		usage()
	}

	// Emit OSC52 (stdout ONLY)
	b64 := base64.StdEncoding.EncodeToString(out.buf.Bytes())
	fmt.Printf("\033]52;c;%s\033\\", b64)

	// Status to stderr
	fmt.Fprintf(os.Stderr, "Sent %d bytes via OSC52\n", out.n)
}

