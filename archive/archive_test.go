package archive_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/archive"
)

func build(t *testing.T, codec archive.Codec) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	w, err := archive.NewWriter(&buf, codec)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Dir("src"); err != nil {
		t.Fatal(err)
	}
	if err := w.File("src/main.py", false, 5, strings.NewReader("print")); err != nil {
		t.Fatal(err)
	}
	if err := w.File("run.sh", true, 2, strings.NewReader("go")); err != nil {
		t.Fatal(err)
	}
	digest, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), digest
}

func TestEveryCodecRoundTripsToTheSameDigest(t *testing.T) {
	var digests []string
	for _, codec := range []archive.Codec{archive.CodecNone, archive.CodecGzip, archive.CodecZstd} {
		raw, digest := build(t, codec)
		var seen []string
		read, err := archive.Read(bytes.NewReader(raw), func(e archive.Entry, body io.Reader) error {
			seen = append(seen, e.Path)
			if e.Path == "run.sh" && !e.Executable {
				t.Error("executable bit lost")
			}
			return nil
		})
		if err != nil || read != digest {
			t.Fatalf("%s: Read = %s %v, want %s", codec, read, err, digest)
		}
		if strings.Join(seen, ",") != "src,src/main.py,run.sh" {
			t.Fatalf("%s: entries = %v", codec, seen)
		}
		digests = append(digests, digest)
	}
	if digests[0] != digests[1] || digests[1] != digests[2] {
		t.Fatalf("codecs disagree on the digest: %v", digests)
	}
}

func TestReadRefusesLinksTraversalAndOrphans(t *testing.T) {
	for name, header := range map[string]*tar.Header{
		"symlink":   {Typeflag: tar.TypeSymlink, Name: "link", Linkname: "/etc/passwd"},
		"hardlink":  {Typeflag: tar.TypeLink, Name: "link", Linkname: "x"},
		"traversal": {Typeflag: tar.TypeReg, Name: "../escape", Size: 0},
		"absolute":  {Typeflag: tar.TypeReg, Name: "/etc/passwd", Size: 0},
		"orphan":    {Typeflag: tar.TypeReg, Name: "missing/file", Size: 0},
		"device":    {Typeflag: tar.TypeChar, Name: "dev"},
	} {
		var buf bytes.Buffer
		buf.WriteString("execenv-archive-1 none\n")
		tw := tar.NewWriter(&buf)
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		_ = tw.Close()
		_, err := archive.Read(&buf, func(archive.Entry, io.Reader) error { return nil })
		if !errors.Is(err, execenv.ErrInvalid) {
			t.Errorf("%s: Read = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := archive.Read(strings.NewReader("not an archive"), func(archive.Entry, io.Reader) error { return nil }); !errors.Is(err, execenv.ErrInvalid) {
		t.Errorf("headerless Read = %v", err)
	}
}

func TestDigestIgnoresOrder(t *testing.T) {
	a := []archive.Entry{{Path: "a", Dir: true}, {Path: "a/b", Size: 1, SHA256: "00"}}
	b := []archive.Entry{a[1], a[0]}
	if archive.Digest(a) != archive.Digest(b) {
		t.Fatal("digest depends on order")
	}
}

func TestArchiveRejectsAFileAsParent(t *testing.T) {
	var buf bytes.Buffer
	w, err := archive.NewWriter(&buf, archive.CodecNone)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.File("leaf", false, 1, strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if err := w.Dir("leaf/child"); !errors.Is(err, execenv.ErrInvalid) {
		t.Fatalf("writer admitted a directory below a file: %v", err)
	}

	buf.Reset()
	buf.WriteString("execenv-archive-1 none\n")
	tw := tar.NewWriter(&buf)
	for _, h := range []*tar.Header{
		{Name: "leaf", Typeflag: tar.TypeReg},
		{Name: "leaf/child/", Typeflag: tar.TypeDir},
	} {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Read(&buf, func(archive.Entry, io.Reader) error { return nil }); !errors.Is(err, execenv.ErrInvalid) {
		t.Fatalf("reader admitted a directory below a file: %v", err)
	}
}

func TestArchiveChecksCompressedFooter(t *testing.T) {
	raw, _ := build(t, archive.CodecGzip)
	raw = raw[:len(raw)-1]
	if _, err := archive.Read(bytes.NewReader(raw), func(archive.Entry, io.Reader) error { return nil }); !errors.Is(err, execenv.ErrInvalid) {
		t.Fatalf("incomplete gzip footer: %v", err)
	}
}

func TestArchiveRequiresBothEndBlocks(t *testing.T) {
	raw, _ := build(t, archive.CodecNone)
	raw = raw[:len(raw)-512]
	if _, err := archive.Read(bytes.NewReader(raw), func(archive.Entry, io.Reader) error { return nil }); !errors.Is(err, execenv.ErrInvalid) {
		t.Fatalf("archive missing its final end block: %v", err)
	}
}
