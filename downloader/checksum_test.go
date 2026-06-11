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

func Test_validateChecksum_multipartDeterministic(t *testing.T) {
	ad := &ConcurrentArtifactDownloader{}
	size := int64(6 * 1024 * 1024) // 6 MiB -> 1 part at the 100 MiB deterministic part size
	path := writeTempFile(t, bytes.Repeat([]byte("a"), int(size)))
	md5sum, err := fileMD5(path)
	assert.NoError(t, err)

	// The expected ETag is recomputed with the same deterministic part size validateChecksum uses.
	expected, err := multipartETag(path, multipartPartSize(size))
	assert.NoError(t, err)

	assert.Equal(t, checksumMultipartOK, ad.validateChecksum(path, md5sum, expected))
	assert.Equal(t, checksumMultipartUnknown, ad.validateChecksum(path, md5sum, "0123456789abcdef0123456789abcdef-2"))
}

func Test_validateChecksum_multipartMismatch(t *testing.T) {
	ad := &ConcurrentArtifactDownloader{}
	path := writeTempFile(t, []byte("small file"))
	md5sum, err := fileMD5(path)
	assert.NoError(t, err)

	// An ETag the deterministic recomputation can't reproduce is reported as unverified — it may be
	// a non-MD5 ETag (SSE-KMS/SSE-C), not necessarily corruption.
	assert.Equal(t, checksumMultipartUnknown, ad.validateChecksum(path, md5sum, "abc-3"))
	// Any "-"-suffixed value is treated as multipart; a malformed one simply won't match.
	assert.Equal(t, checksumMultipartUnknown, ad.validateChecksum(path, md5sum, "abc-notanumber"))
}

func Test_multipartPartSize(t *testing.T) {
	const target = int64(100 * (1 << 20)) // 100 MiB

	// Below ~976 GiB the 100 MiB target always wins.
	assert.Equal(t, target, multipartPartSize(1))
	assert.Equal(t, target, multipartPartSize(6*1024*1024))
	assert.Equal(t, target, multipartPartSize(50*target)) // 50 parts, still 100 MiB each

	// Past the 10,000-part cap the part size grows to ceil(size / 10_000).
	huge := target*10_000 + 1
	assert.Equal(t, (huge+9_999)/10_000, multipartPartSize(huge))
}
