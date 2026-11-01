package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/Higangssh/hyperbutler/internal/config"
	"github.com/Higangssh/hyperbutler/internal/diag"
	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/transport"
)

// sizeFlag is a byte-count flag such as --memory 4GiB or --disk-size 64GiB.
type sizeFlag struct {
	n   uint64
	set bool
}

func (s *sizeFlag) String() string {
	if !s.set {
		return ""
	}
	return engine.FormatSize(s.n)
}

func (s *sizeFlag) Set(v string) error {
	n, err := engine.ParseSize(v)
	if err != nil {
		return err
	}
	s.n, s.set = n, true
	return nil
}

// parseMemoryMB turns a --memory value into whole MiB (config.ParseMemoryMB).
func parseMemoryMB(v string) (int, error) { return config.ParseMemoryMB(v) }

// newFlagSet is a flag set that reports errors instead of exiting, writing usage to w.
func newFlagSet(name string, w io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("hyperbutler "+name, flag.ContinueOnError)
	fs.SetOutput(w)
	return fs
}

// parseInterspersed parses args into fs, allowing flags before, between and after
// positional arguments, which it returns. "--" ends the flags.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, &flagError{err}
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		// flag stops at the first positional argument, or after "--".
		if len(args) > len(rest) && args[len(args)-len(rest)-1] == "--" {
			return append(pos, rest...), nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// flagError is a flag the flag set refused (or -h), which it already printed with the
// command's flags. run exits 2 for it (0 for -h).
type flagError struct{ err error }

func (e *flagError) Error() string { return e.err.Error() }
func (e *flagError) Unwrap() error { return e.err }

// parseArgs parses args into fs and rejects positional arguments, which no hyperbutler
// command takes.
func parseArgs(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return &flagError{err}
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("%s: unexpected argument %q", fs.Name(), fs.Arg(0))
	}
	return nil
}

// debugPipeEnv names the variable that turns the engine's debug endpoint on when
// --debug-pipe is not given (ADR 0003, guardrail 4). It is read by `hyperbutler start`.
const debugPipeEnv = "HYPERBUTLER_DEBUG_PIPE"

// checkDiagFlags validates the engine's diag settings before anything is started.
func checkDiagFlags(c diag.Config) error {
	if c.MemoryLimit != "" {
		if err := engine.ValidateMemoryLimit(c.MemoryLimit); err != nil {
			return fmt.Errorf("--memlimit: %w", err)
		}
	}
	if c.DebugPipe != "" {
		if err := transport.ValidatePipeName(c.DebugPipe); err != nil {
			return fmt.Errorf("--debug-pipe (or %s): %w", debugPipeEnv, err)
		}
	}
	return nil
}

// engineArgs is the command line of the background `hyperbutler engine` process.
func engineArgs(home, kernel, initrd, manifest string, dcfg diag.Config) []string {
	args := []string{"engine", "--home", home}
	if manifest != "" {
		args = append(args, "--image", manifest)
	} else {
		args = append(args, "--kernel", kernel, "--initrd", initrd)
	}
	if dcfg.MemoryLimit != "" {
		args = append(args, "--memlimit", dcfg.MemoryLimit)
	}
	if dcfg.DebugPipe != "" {
		args = append(args, "--debug-pipe", dcfg.DebugPipe)
	}
	return args
}

// errUsage asks main to print the command list.
var errUsage = errors.New("usage")

// printable escapes control characters (other than tab and newline) and invalid UTF-8
// in text that came from the guest, so a console log cannot drive the terminal.
func printable(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		switch {
		case r == utf8.RuneError && size <= 1:
			out = fmt.Appendf(out, `\x%02x`, b[0])
		case r == '\n' || r == '\t':
			out = append(out, byte(r))
		case r == '\r':
			// Windows line ends; a lone CR could overwrite the line on screen.
			if len(b) > 1 && b[1] == '\n' {
				out = append(out, '\r')
			} else {
				out = append(out, `\r`...)
			}
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == 0x2028 || r == 0x2029 ||
			r >= 0x200b && r <= 0x200f || r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069 || r == 0xfeff:
			out = fmt.Appendf(out, `\u%04x`, r)
		default:
			out = append(out, b[:size]...)
		}
		b = b[size:]
	}
	return out
}

// lastLines returns the last n lines of b (all of b if n <= 0).
func lastLines(b []byte, n int) []byte {
	if n <= 0 {
		return b
	}
	end := len(b)
	if end > 0 && b[end-1] == '\n' {
		end--
	}
	for i := end - 1; i >= 0; i-- {
		if b[i] == '\n' {
			n--
			if n == 0 {
				return b[i+1:]
			}
		}
	}
	return b
}

// npipeHost is the docker endpoint URL of a local pipe name:
// \\.\pipe\docker_engine_hyperbutler -> npipe:////./pipe/docker_engine_hyperbutler.
func npipeHost(pipe string) string { return "npipe://" + strings.ReplaceAll(pipe, `\`, "/") }
