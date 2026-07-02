// genman compiles the CLI help text (internal/help) into gzipped man pages.
// Run from the repo root:
//
//	go run ./tools/genman            # writes man/docker-pin.1.gz + man/docker-unpin.1.gz
//
// The release workflow runs this before goreleaser so the deb and brew
// archive can ship the pages. Single source of truth: the man pages can
// never drift from --help because both render the same strings.
package main

import (
	"bytes"
	"compress/gzip"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Miista/homebrew-docker-pin/internal/help"
)

func main() {
	dir := flag.String("dir", "man", "output directory")
	flag.Parse()

	if err := os.MkdirAll(*dir, 0o755); err != nil {
		fatal(err)
	}
	write(*dir, "docker-pin", "pin compose service images to tag + SHA digest",
		"docker pin", help.PinUsage, help.PinTopics)
	write(*dir, "docker-unpin", "remove the SHA digest pin from service images",
		"docker unpin", help.UnpinUsage, help.UnpinTopics)
}

func write(dir, name, oneline, synopsis, usage string, topics []help.Topic) {
	var b bytes.Buffer
	fmt.Fprintf(&b, ".TH %s 1 \"\" \"%s\" \"User Commands\"\n", strings.ToUpper(name), name)
	fmt.Fprintf(&b, ".SH NAME\n%s \\- %s\n", name, oneline)
	fmt.Fprintf(&b, ".SH SYNOPSIS\n.B %s\n\\fIcommand\\fR [\\fIargs\\fR]\n", synopsis)
	b.WriteString(".SH DESCRIPTION\n")
	verbatim(&b, usage)
	b.WriteString(".SH COMMANDS\n")
	for _, t := range topics {
		fmt.Fprintf(&b, ".SS %s\n", t.Cmd)
		verbatim(&b, t.Text)
	}

	out := filepath.Join(dir, name+".1.gz")
	f, err := os.Create(out)
	if err != nil {
		fatal(err)
	}
	zw := gzip.NewWriter(f)
	if _, err := zw.Write(b.Bytes()); err != nil {
		fatal(err)
	}
	if err := zw.Close(); err != nil {
		fatal(err)
	}
	if err := f.Close(); err != nil {
		fatal(err)
	}
	fmt.Println("wrote", out)
}

// verbatim emits text as a no-fill block, escaping troff control characters.
func verbatim(b *bytes.Buffer, text string) {
	b.WriteString(".nf\n")
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		line = strings.ReplaceAll(line, "\\", "\\\\")
		if strings.HasPrefix(line, ".") || strings.HasPrefix(line, "'") {
			line = "\\&" + line
		}
		b.WriteString(line + "\n")
	}
	b.WriteString(".fi\n")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "genman:", err)
	os.Exit(1)
}
