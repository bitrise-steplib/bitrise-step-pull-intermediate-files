package downloader

import (
	"archive/zip"
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/bitrise-io/go-utils/pathutil"
	"github.com/bitrise-io/go-utils/v2/log"
	"github.com/bitrise-steplib/bitrise-step-pull-intermediate-files/api"
	"github.com/bitrise-steplib/bitrise-step-pull-intermediate-files/mocks"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

func getDownloadDir(t *testing.T) string {
	t.Helper()
	tempPath, err := pathutil.NormalizedOSTempDirPath("_tmp")
	assert.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tempPath) })
	return tempPath
}

func Test_DownloadAndSaveArtifacts(t *testing.T) {
	svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "dummy data")
	}))
	defer svr.Close()

	targetDir := getDownloadDir(t)

	var artifacts []api.ArtifactResponseItemModel
	var expectedDownloadResults []ArtifactDownloadResult
	for i := 1; i <= 11; i++ {
		downloadURL := fmt.Sprintf(svr.URL+"/%d.txt", i)
		artifacts = append(artifacts, api.ArtifactResponseItemModel{DownloadURL: downloadURL, Title: fmt.Sprintf("%d.txt", i)})
		expectedDownloadResults = append(expectedDownloadResults, ArtifactDownloadResult{
			DownloadPath: targetDir + fmt.Sprintf("/%d.txt", i),
			DownloadURL:  downloadURL,
			DownloadDetails: TransferDetails{
				Size:     10,
				Hostname: "127.0.0.1",
			},
		})
	}

	artifactDownloader := NewConcurrentArtifactDownloader(5*time.Minute, log.NewLogger(), nil, false)

	downloadResults, err := artifactDownloader.DownloadAndSaveArtifacts(artifacts, targetDir)
	assert.NoError(t, err)
	assert.Equal(t, len(expectedDownloadResults), len(downloadResults))

	getResult := func(downloadURL string) *ArtifactDownloadResult {
		for _, result := range downloadResults {
			if result.DownloadURL == downloadURL {
				return &result
			}
		}
		return nil
	}

	// We need to ignore the Duration field because it is not deterministic
	ignoreFields := cmpopts.IgnoreFields(TransferDetails{}, "Duration")

	for _, exp := range expectedDownloadResults {
		result := getResult(exp.DownloadURL)
		assert.NotNil(t, result)

		if !cmp.Equal(exp, *result, ignoreFields) {
			t.Errorf("Download results mismatch (-want +got):\n%s", cmp.Diff(exp, result, ignoreFields))
		}
	}
}

func Test_fileCRC32C(t *testing.T) {
	fsys := fstest.MapFS{"artifact": {Data: []byte("hello")}}

	got, err := fileCRC32C(fsys, "artifact")

	assert.NoError(t, err)
	assert.Equal(t, "mnG7TA==", got) // base64 big-endian CRC32C (Castagnoli) of "hello"
}

func Test_DownloadAndSaveArtifacts_DownloadFails(t *testing.T) {
	svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer svr.Close()

	targetDir := getDownloadDir(t)

	downloadURL := svr.URL + "/1.txt"

	var artifacts []api.ArtifactResponseItemModel
	artifacts = append(artifacts,
		api.ArtifactResponseItemModel{DownloadURL: downloadURL, Title: "1.txt"})

	// TODO: mock command factory
	artifactDownloader := NewConcurrentArtifactDownloader(5*time.Minute, log.NewLogger(), nil, false)

	result, err := artifactDownloader.DownloadAndSaveArtifacts(artifacts, targetDir)

	assert.EqualError(t, result[0].DownloadError, fmt.Sprintf("unable to download file from %s: failed to download intermediate file: Response status code is not ok: 401", downloadURL))
	assert.NoError(t, err)
}

func Test_DownloadAndSaveArtifacts_RetriesFailingDownload(t *testing.T) {
	var receivedRequestCount atomic.Uint64
	svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedRequestCount.Add(1)

		if receivedRequestCount.Load() == 1 {
			// The first request needs to be valid because this is for getting the downloadable content range.
			// The library will not attempt to perform any retries if this fails.
			w.Header().Set("content-length", "1")
			w.Header().Set("content-range", "0/2")
			_, _ = w.Write([]byte("a"))
		} else {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer svr.Close()

	targetDir := getDownloadDir(t)

	artifacts := []api.ArtifactResponseItemModel{
		{DownloadURL: svr.URL + "/1.txt", Title: "1.txt"},
	}

	artifactDownloader := NewConcurrentArtifactDownloader(5*time.Second, log.NewLogger(), nil, false)
	_, err := artifactDownloader.DownloadAndSaveArtifacts(artifacts, targetDir)

	assert.NoError(t, err)
	assert.Greater(t, receivedRequestCount.Load(), uint64(1))
}

func Test_DownloadAndSaveZipDirectoryArtifacts(t *testing.T) {
	svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "dummy data")
	}))
	defer svr.Close()

	targetDir := getDownloadDir(t)

	downloadURL := fmt.Sprintf("%s/1.zip", svr.URL)
	artifacts := []api.ArtifactResponseItemModel{
		{
			DownloadURL: downloadURL,
			Title:       "1.zip",
			IntermediateFileInfo: api.IntermediateFileInfo{
				IsDir: true,
			},
		},
	}

	cmd := new(mocks.Command)
	cmd.On("RunAndReturnTrimmedCombinedOutput").Return("", nil).Once()

	cmdFactory := new(mocks.Factory)
	cmdFactory.On("Create", "unzip", mock.Anything, mock.Anything).Return(cmd).Once()

	artifactDownloader := NewConcurrentArtifactDownloader(5*time.Minute, log.NewLogger(), cmdFactory, false)

	downloadResults, err := artifactDownloader.DownloadAndSaveArtifacts(artifacts, targetDir)
	assert.NoError(t, err)

	assert.Equal(t, 1, len(downloadResults))
	assert.Equal(t, targetDir+"/1", downloadResults[0].DownloadPath)
	assert.Equal(t, downloadURL, downloadResults[0].DownloadURL)
	assert.Equal(t, "127.0.0.1", downloadResults[0].DownloadDetails.Hostname)
	assert.Greater(t, downloadResults[0].DownloadDetails.Duration, time.Duration(0))
	assert.Equal(t, int64(10), downloadResults[0].DownloadDetails.Size)

	cmd.AssertExpectations(t)
	cmdFactory.AssertExpectations(t)
	assert.Len(t, cmdFactory.Calls, 1)
	assert.Len(t, cmdFactory.Calls[0].Arguments, 3)
	assert.IsType(t, []string{}, cmdFactory.Calls[0].Arguments[1])
	unzipCmdArguments := cmdFactory.Calls[0].Arguments[1].([]string)
	assert.Len(t, unzipCmdArguments, 2)
	assert.Equal(t, "-o", unzipCmdArguments[0])
}

func Test_DownloadAndSaveZipDirectoryArtifacts_ZipV2(t *testing.T) {
	// Build a real zip archive so the pure-Go ziputil.UnZip has valid input to extract.
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	w, err := zw.Create("hello.txt")
	assert.NoError(t, err)
	_, err = w.Write([]byte("hello world"))
	assert.NoError(t, err)
	assert.NoError(t, zw.Close())
	zipBytes := archive.Bytes()

	svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(zipBytes)
	}))
	defer svr.Close()

	targetDir := getDownloadDir(t)

	downloadURL := fmt.Sprintf("%s/1.zip", svr.URL)
	artifacts := []api.ArtifactResponseItemModel{
		{
			DownloadURL: downloadURL,
			Title:       "1.zip",
			IntermediateFileInfo: api.IntermediateFileInfo{
				IsDir: true,
			},
		},
	}

	// A command factory with no expectations: invoking the `unzip` CLI here would fail the test,
	// proving the v2 path bypasses it entirely.
	cmdFactory := new(mocks.Factory)

	artifactDownloader := NewConcurrentArtifactDownloader(5*time.Minute, log.NewLogger(), cmdFactory, true)

	downloadResults, err := artifactDownloader.DownloadAndSaveArtifacts(artifacts, targetDir)
	assert.NoError(t, err)

	assert.Equal(t, 1, len(downloadResults))
	assert.NoError(t, downloadResults[0].DownloadError)
	assert.Equal(t, filepath.Join(targetDir, "1"), downloadResults[0].DownloadPath)

	extracted, err := os.ReadFile(filepath.Join(targetDir, "1", "hello.txt"))
	assert.NoError(t, err)
	assert.Equal(t, "hello world", string(extracted))

	// The pure-Go extractor must not have shelled out to the `unzip` CLI.
	assert.Len(t, cmdFactory.Calls, 0)
}

func Test_DownloadAndSaveTarDirectoryArtifacts(t *testing.T) {
	svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "dummy data")
	}))
	defer svr.Close()

	targetDir := getDownloadDir(t)

	downloadURL := fmt.Sprintf("%s/1.tar", svr.URL)
	artifacts := []api.ArtifactResponseItemModel{
		{
			DownloadURL: downloadURL,
			Title:       "1.tar",
			IntermediateFileInfo: api.IntermediateFileInfo{
				IsDir: true,
			},
		},
	}

	cmd := new(mocks.Command)
	cmd.On("RunAndReturnTrimmedCombinedOutput").Return("", nil).Once()

	cmdFactory := new(mocks.Factory)
	cmdFactory.On("Create", "tar", mock.Anything, mock.Anything).Return(cmd).Once()

	artifactDownloader := NewConcurrentArtifactDownloader(5*time.Minute, log.NewLogger(), cmdFactory, false)

	downloadResults, err := artifactDownloader.DownloadAndSaveArtifacts(artifacts, targetDir)
	assert.NoError(t, err)

	assert.Equal(t, 1, len(downloadResults))
	assert.Equal(t, targetDir+"/1", downloadResults[0].DownloadPath)
	assert.Equal(t, downloadURL, downloadResults[0].DownloadURL)
	assert.Equal(t, "127.0.0.1", downloadResults[0].DownloadDetails.Hostname)
	assert.Greater(t, downloadResults[0].DownloadDetails.Duration, time.Duration(0))
	assert.Equal(t, int64(-2), downloadResults[0].DownloadDetails.Size)

	cmd.AssertExpectations(t)
	cmdFactory.AssertExpectations(t)
}
