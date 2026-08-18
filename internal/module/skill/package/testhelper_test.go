package skillpkg

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

// archiveBuilder builds an in-memory tar.gz archive for tests. Each file is a
// regular entry; the builder never sets an executable bit unless asked (to
// exercise the exec-bit detection path).
type archiveBuilder struct {
	files []archiveFile
}

type archiveFile struct {
	path    string
	content []byte
	mode    int64
	symlink bool
}

func (b *archiveBuilder) File(path string, content []byte) *archiveBuilder {
	b.files = append(b.files, archiveFile{path: path, content: content, mode: 0o644})
	return b
}

func (b *archiveBuilder) ExecFile(path string, content []byte) *archiveBuilder {
	b.files = append(b.files, archiveFile{path: path, content: content, mode: 0o755})
	return b
}

// Symlink adds a symlink entry whose target is NOT followed on parse.
func (b *archiveBuilder) Symlink(path, target string) *archiveBuilder {
	b.files = append(b.files, archiveFile{path: path, content: []byte(target), mode: 0o777, symlink: true})
	return b
}

// Bytes renders the archive to gzip+tar bytes.
func (b *archiveBuilder) Bytes() []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range b.files {
		if f.symlink {
			_ = tw.WriteHeader(&tar.Header{
				Name:     f.path,
				Mode:     f.mode,
				Size:     0,
				Typeflag: tar.TypeSymlink,
				Linkname: string(f.content),
			})
			continue
		}
		_ = tw.WriteHeader(&tar.Header{
			Name: f.path, Mode: f.mode, Size: int64(len(f.content)),
		})
		_, _ = tw.Write(f.content)
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// memReader is an ArchiveReader over an in-memory byte slice.
type memReader struct{ data []byte }

func (m memReader) Open(_ string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(m.data)), nil
}

// bombArchive builds a gzip+tar archive whose second entry's header lies
// about a huge size (size), so the size-cap check fires without allocating
// that many bytes (§4.4 anti-compression-bomb).
func bombArchive(t *testing.T, size int64) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "SKILL.md", Mode: 0o644, Size: int64(len("---\nname: x\n---\nbody"))}))
	_, _ = tw.Write([]byte("---\nname: x\n---\nbody"))
	// A header that declares a size over the cap but is followed by no bytes.
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "assets/huge.bin", Mode: 0o644, Size: size}))
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}
