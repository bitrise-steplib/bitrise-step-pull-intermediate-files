package downloader

import (
	"crypto/md5"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// independentMultipartETag recomputes the S3/R2 multipart ETag in a straightforward in-memory way,
// as an independent cross-check of the streaming implementation.
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

func Test_validateChecksum(t *testing.T) {
	cases := map[string]struct {
		md5sum        string
		multipartETag string
		etag          string
		want          checksumStatus
	}{
		"single match": {
			md5sum: "5d41402abc4b2a76b9719d911017c592",
			etag:   "5d41402abc4b2a76b9719d911017c592",
			want:   checksumSingleOK,
		},
		"single match is case-insensitive": {
			md5sum: "5D41402ABC4B2A76B9719D911017C592",
			etag:   "5d41402abc4b2a76b9719d911017c592",
			want:   checksumSingleOK,
		},
		"single mismatch": {
			md5sum: "5d41402abc4b2a76b9719d911017c592",
			etag:   "00000000000000000000000000000000",
			want:   checksumSingleMismatch,
		},
		"multipart match": {
			multipartETag: "554a2f6105cc700b8cc987b5ddfb8102-2",
			etag:          "554a2f6105cc700b8cc987b5ddfb8102-2",
			want:          checksumMultipartOK,
		},
		"multipart match is case-insensitive": {
			multipartETag: "554a2f6105cc700b8cc987b5ddfb8102-2",
			etag:          "554A2F6105CC700B8CC987B5DDFB8102-2",
			want:          checksumMultipartOK,
		},
		"multipart mismatch": {
			multipartETag: "554a2f6105cc700b8cc987b5ddfb8102-2",
			etag:          "deadbeefdeadbeefdeadbeefdeadbeef-2",
			want:          checksumMultipartMismatch,
		},
		"any dash-suffixed etag is treated as multipart and can mismatch": {
			multipartETag: "554a2f6105cc700b8cc987b5ddfb8102-2",
			etag:          "abc-notanumber",
			want:          checksumMultipartMismatch,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, validateChecksum(tc.md5sum, tc.multipartETag, tc.etag))
		})
	}
}

func Test_fileChecksums(t *testing.T) {
	cases := map[string]struct {
		given         string
		partSize      int64
		wantMD5       string
		wantMultipart string
	}{
		"single-shot returns the whole-file MD5": {
			given:    "hello",
			partSize: 0,
			wantMD5:  "5d41402abc4b2a76b9719d911017c592",
		},
		"empty file": {
			given:    "",
			partSize: 0,
			wantMD5:  "d41d8cd98f00b204e9800998ecf8427e",
		},
		"multipart fitting one part": {
			given:         "hello",
			partSize:      10,
			wantMD5:       "5d41402abc4b2a76b9719d911017c592",
			wantMultipart: "62109206880d38a4010a98e11243924a-1",
		},
		"multipart spanning two parts with a trailing partial part": {
			given:         "hello",
			partSize:      3,
			wantMD5:       "5d41402abc4b2a76b9719d911017c592",
			wantMultipart: "554a2f6105cc700b8cc987b5ddfb8102-2",
		},
		"multipart on an exact-multiple boundary has no trailing part": {
			given:         "abcdef",
			partSize:      3,
			wantMD5:       "e80b5017098950fc58aad83c8c14978e",
			wantMultipart: "4c8e93283780e078db9e0c6b9b3f8043-2",
		},
		"multipart spanning three parts": {
			given:         "abcdefg",
			partSize:      3,
			wantMD5:       "7ac66c0f148de9519b8bd264312c4d64",
			wantMultipart: "d322b115ece92a45e0909788b142235c-3",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "artifact")
			require.NoError(t, os.WriteFile(path, []byte(tc.given), 0o600))

			gotMD5, gotMultipart, err := fileChecksums(path, tc.partSize)

			require.NoError(t, err)
			require.Equal(t, tc.wantMD5, gotMD5)
			require.Equal(t, tc.wantMultipart, gotMultipart)
		})
	}
}

func Test_multipartPartSize(t *testing.T) {
	const target = int64(100 * (1 << 20)) // 100 MiB

	cases := map[string]struct {
		fileSize int64
		want     int64
	}{
		"tiny file uses the target part size":            {fileSize: 1, want: target},
		"a few MiB use the target part size":             {fileSize: 6 * 1024 * 1024, want: target},
		"dozens of parts still use the target part size": {fileSize: 50 * target, want: target},
		"beyond the 10,000-part cap grows the part size": {fileSize: target*10_000 + 1, want: target + 1},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, multipartPartSize(tc.fileSize))
		})
	}
}
