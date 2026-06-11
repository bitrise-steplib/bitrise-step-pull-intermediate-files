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

func Test_fileChecksums_multipartMatchesIndependentComputation(t *testing.T) {
	content := bytes.Repeat([]byte("x"), 12) // 5 + 5 + 2 -> 3 parts at partSize 5
	path := writeTempFile(t, content)

	md5hex, multipart, err := fileChecksums(path, 5)

	assert.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("%x", md5.Sum(content)), md5hex) // whole-file MD5 from the same pass
	assert.Equal(t, independentMultipartETag(content, 5), multipart)
	assert.True(t, len(multipart) > 2 && multipart[len(multipart)-2:] == "-3")
}

func Test_fileChecksums_exactMultipleBoundary(t *testing.T) {
	content := bytes.Repeat([]byte("y"), 10) // 5 + 5 -> exactly 2 parts, no trailing part
	path := writeTempFile(t, content)

	_, multipart, err := fileChecksums(path, 5)

	assert.NoError(t, err)
	assert.Equal(t, independentMultipartETag(content, 5), multipart)
	assert.Equal(t, "-2", multipart[len(multipart)-2:])
}

func Test_fileChecksums_md5OnlyWhenNoPartSize(t *testing.T) {
	content := bytes.Repeat([]byte("z"), 3000) // larger than the read buffer step is unnecessary here
	path := writeTempFile(t, content)

	md5hex, multipart, err := fileChecksums(path, 0)

	assert.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("%x", md5.Sum(content)), md5hex)
	assert.Empty(t, multipart) // no multipart ETag is computed when partSize == 0
}

func Test_validateChecksum_singlePart(t *testing.T) {
	ad := &ConcurrentArtifactDownloader{}
	md5sum := fmt.Sprintf("%x", md5.Sum([]byte("hello world")))

	assert.Equal(t, checksumSingleOK, ad.validateChecksum(md5sum, "", md5sum))
	assert.Equal(t, checksumSingleOK, ad.validateChecksum(md5sum, "", "5EB63BBBE01EEED093CB22BB8F5ACDC3")) // case-insensitive
	assert.Equal(t, checksumSingleMismatch, ad.validateChecksum(md5sum, "", "deadbeefdeadbeefdeadbeefdeadbeef"))
}

func Test_validateChecksum_multipartDeterministic(t *testing.T) {
	ad := &ConcurrentArtifactDownloader{}
	size := int64(6 * 1024 * 1024) // 6 MiB -> 1 part at the 100 MiB deterministic part size
	path := writeTempFile(t, bytes.Repeat([]byte("a"), int(size)))

	// Compute both checksums in one pass, exactly as verifyAndLogChecksum does.
	md5sum, expected, err := fileChecksums(path, multipartPartSize(size))
	assert.NoError(t, err)

	assert.Equal(t, checksumMultipartOK, ad.validateChecksum(md5sum, expected, expected))
	assert.Equal(t, checksumMultipartMismatch, ad.validateChecksum(md5sum, expected, "0123456789abcdef0123456789abcdef-2"))
}

func Test_validateChecksum_multipartMismatch(t *testing.T) {
	ad := &ConcurrentArtifactDownloader{}
	md5sum := fmt.Sprintf("%x", md5.Sum([]byte("small file")))
	recomputed := "0123456789abcdef0123456789abcdef-1" // what the local file produced

	// A remote ETag the recomputation can't reproduce is reported as a mismatch — it may be a
	// non-MD5 ETag (SSE-KMS/SSE-C), not necessarily corruption.
	assert.Equal(t, checksumMultipartMismatch, ad.validateChecksum(md5sum, recomputed, "abc-3"))
	// Any "-"-suffixed value is treated as multipart; a malformed one simply won't match.
	assert.Equal(t, checksumMultipartMismatch, ad.validateChecksum(md5sum, recomputed, "abc-notanumber"))
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
