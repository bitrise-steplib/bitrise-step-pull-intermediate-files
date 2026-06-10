package downloader

import (
	"bytes"
	"crypto/md5"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func writeTempFile(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "artifact")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("failed to write temp file: %s", err)
	}
	return path
}

// independentMultipartETag recomputes the S3/R2 multipart ETag in a straightforward in-memory
// way, to cross-check the streaming implementation.
func independentMultipartETag(data []byte, partSize int) string {
	var digests []byte
	parts := 0
	for off := 0; off < len(data); off += partSize {
		end := min(off+partSize, len(data))
		sum := md5.Sum(data[off:end])
		digests = append(digests, sum[:]...)
		parts++
	}
	final := md5.Sum(digests)
	return fmt.Sprintf("%x-%d", final, parts)
}

func Test_fileMD5(t *testing.T) {
	path := writeTempFile(t, []byte("hello"))

	got, err := fileMD5(path)

	assert.NoError(t, err)
	assert.Equal(t, "5d41402abc4b2a76b9719d911017c592", got)
}

func Test_multipartETag_matchesIndependentComputation(t *testing.T) {
	content := bytes.Repeat([]byte("x"), 12) // 5 + 5 + 2 -> 3 parts at partSize 5
	path := writeTempFile(t, content)

	got, err := multipartETag(path, 5)

	assert.NoError(t, err)
	assert.Equal(t, independentMultipartETag(content, 5), got)
	assert.True(t, len(got) > 2 && got[len(got)-2:] == "-3")
}

func Test_multipartETag_exactMultipleBoundary(t *testing.T) {
	content := bytes.Repeat([]byte("y"), 10) // 5 + 5 -> exactly 2 parts, no trailing part
	path := writeTempFile(t, content)

	got, err := multipartETag(path, 5)

	assert.NoError(t, err)
	assert.Equal(t, independentMultipartETag(content, 5), got)
	assert.Equal(t, "-2", got[len(got)-2:])
}

func Test_validateChecksum_singlePart(t *testing.T) {
	ad := &ConcurrentArtifactDownloader{}
	path := writeTempFile(t, []byte("hello world"))
	md5sum, err := fileMD5(path)
	assert.NoError(t, err)

	assert.Equal(t, checksumSingleOK, ad.validateChecksum(path, md5sum, md5sum))
	assert.Equal(t, checksumSingleOK, ad.validateChecksum(path, md5sum, "5EB63BBBE01EEED093CB22BB8F5ACDC3")) // case-insensitive
	assert.Equal(t, checksumSingleMismatch, ad.validateChecksum(path, md5sum, "deadbeefdeadbeefdeadbeefdeadbeef"))
}

func Test_validateChecksum_multipartInferred(t *testing.T) {
	ad := &ConcurrentArtifactDownloader{}
	size := 6 * 1024 * 1024 // 6 MiB -> 2 parts at a 5 MiB part size
	path := writeTempFile(t, bytes.Repeat([]byte("a"), size))
	md5sum, err := fileMD5(path)
	assert.NoError(t, err)

	expected, err := multipartETag(path, 5*1024*1024)
	assert.NoError(t, err)

	assert.Equal(t, checksumMultipartOK, ad.validateChecksum(path, md5sum, expected))
	assert.Equal(t, checksumMultipartUnknown, ad.validateChecksum(path, md5sum, "0123456789abcdef0123456789abcdef-2"))
}

func Test_validateChecksum_multipartUnverifiableForUnusualPartSize(t *testing.T) {
	ad := &ConcurrentArtifactDownloader{}
	path := writeTempFile(t, []byte("small file"))
	md5sum, err := fileMD5(path)
	assert.NoError(t, err)

	// No plausible common part size splits a 10-byte file into 3 parts.
	assert.Equal(t, checksumMultipartUnknown, ad.validateChecksum(path, md5sum, "abc-3"))
	// Malformed multipart suffix.
	assert.Equal(t, checksumMultipartUnknown, ad.validateChecksum(path, md5sum, "abc-notanumber"))
}

func Test_candidatePartSizes(t *testing.T) {
	// Single-part multipart upload covers the whole file in one part.
	assert.Equal(t, []int64{1234}, candidatePartSizes(1234, 1))

	// 6 MiB into 2 parts: both 5 MiB and 5 MB qualify.
	got := candidatePartSizes(6*1024*1024, 2)
	assert.Contains(t, got, int64(5*1024*1024))
	assert.Contains(t, got, int64(5_000_000))

	// Degenerate inputs.
	assert.Nil(t, candidatePartSizes(0, 2))
	assert.Nil(t, candidatePartSizes(100, 0))
}
