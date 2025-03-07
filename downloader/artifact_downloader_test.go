package downloader

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bitrise-steplib/bitrise-step-pull-intermediate-files/api"
	"github.com/bitrise-steplib/bitrise-step-pull-intermediate-files/mocks"

	"github.com/bitrise-io/go-utils/pathutil"
	"github.com/bitrise-io/go-utils/v2/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

const relativeDownloadPath = "_tmp"

func getDownloadDir(dirName string) (string, error) {
	tempPath, err := pathutil.NormalizedOSTempDirPath(dirName)
	if err != nil {
		return "", err
	}

	return tempPath, nil
}

func Test_DownloadAndSaveArtifacts(t *testing.T) {
	svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "dummy data")
	}))
	defer svr.Close()

	targetDir, err := getDownloadDir(relativeDownloadPath)
	assert.NoError(t, err)

	var artifacts []api.ArtifactResponseItemModel
	var expectedDownloadResults []ArtifactDownloadResult
	for i := 1; i <= 11; i++ {
		downloadURL := fmt.Sprintf(svr.URL+"/%d.txt", i)
		artifacts = append(artifacts, api.ArtifactResponseItemModel{DownloadURL: downloadURL, Title: fmt.Sprintf("%d.txt", i)})
		expectedDownloadResults = append(expectedDownloadResults, ArtifactDownloadResult{
			DownloadPath: targetDir + fmt.Sprintf("/%d.txt", i),
			DownloadURL:  downloadURL,
		})
	}

	artifactDownloader := NewConcurrentArtifactDownloader(5*time.Minute, log.NewLogger(), nil)

	downloadResults, err := artifactDownloader.DownloadAndSaveArtifacts(artifacts, targetDir)

	assert.NoError(t, err)
	assert.ElementsMatch(t, expectedDownloadResults, downloadResults)

	_ = os.RemoveAll(targetDir)
}

func Test_DownloadAndSaveArtifacts_DownloadFails(t *testing.T) {
	svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer svr.Close()

	targetDir, err := getDownloadDir(relativeDownloadPath)
	assert.NoError(t, err)

	downloadURL := svr.URL + "/1.txt"

	var artifacts []api.ArtifactResponseItemModel
	artifacts = append(artifacts,
		api.ArtifactResponseItemModel{DownloadURL: downloadURL, Title: "1.txt"})

	// TODO: mock command factory
	artifactDownloader := NewConcurrentArtifactDownloader(5*time.Minute, log.NewLogger(), nil)

	result, err := artifactDownloader.DownloadAndSaveArtifacts(artifacts, targetDir)

	assert.EqualError(t, result[0].DownloadError, fmt.Sprintf("unable to download file from %s: Response status code is not ok: 401", downloadURL))
	assert.NoError(t, err)

	_ = os.RemoveAll(targetDir)
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

	targetDir, err := getDownloadDir(relativeDownloadPath)
	assert.NoError(t, err)

	artifacts := []api.ArtifactResponseItemModel{
		{DownloadURL: svr.URL + "/1.txt", Title: "1.txt"},
	}

	artifactDownloader := NewConcurrentArtifactDownloader(5*time.Minute, log.NewLogger(), nil)
	_, err = artifactDownloader.DownloadAndSaveArtifacts(artifacts, targetDir)

	assert.NoError(t, err)
	// Total request count is:
	// 1 + 2 * 5 = 11
	// -----
	// 1 -> initial request to get the content range
	// 2 * -> the library uses a min of 2 concurrent downloads
	// 5 -> first request + 4 retries
	assert.Equal(t, uint64(11), receivedRequestCount.Load())

	_ = os.RemoveAll(targetDir)
}

func Test_DownloadAndSaveZipDirectoryArtifacts(t *testing.T) {
	svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "dummy data")
	}))
	defer svr.Close()

	targetDir, err := getDownloadDir(relativeDownloadPath)
	assert.NoError(t, err)

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
	expectedDownloadResults := []ArtifactDownloadResult{
		{
			DownloadPath: targetDir + "/1",
			DownloadURL:  downloadURL,
		},
	}

	cmd := new(mocks.Command)
	cmd.On("RunAndReturnTrimmedCombinedOutput").Return("", nil).Once()

	cmdFactory := new(mocks.Factory)
	cmdFactory.On("Create", "unzip", mock.Anything, mock.Anything).Return(cmd).Once()

	artifactDownloader := NewConcurrentArtifactDownloader(5*time.Minute, log.NewLogger(), cmdFactory)

	downloadResults, err := artifactDownloader.DownloadAndSaveArtifacts(artifacts, targetDir)

	assert.NoError(t, err)
	assert.ElementsMatch(t, expectedDownloadResults, downloadResults)
	cmd.AssertExpectations(t)
	cmdFactory.AssertExpectations(t)
	assert.Len(t, cmdFactory.Calls, 1)
	assert.Len(t, cmdFactory.Calls[0].Arguments, 3)
	assert.IsType(t, []string{}, cmdFactory.Calls[0].Arguments[1])
	unzipCmdArguments := cmdFactory.Calls[0].Arguments[1].([]string)
	assert.Len(t, unzipCmdArguments, 2)
	assert.Equal(t, "-o", unzipCmdArguments[0])

	_ = os.RemoveAll(targetDir)
}

func Test_DownloadAndSaveTarDirectoryArtifacts(t *testing.T) {
	svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "dummy data")
	}))
	defer svr.Close()

	targetDir, err := getDownloadDir(relativeDownloadPath)
	assert.NoError(t, err)

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
	expectedDownloadResults := []ArtifactDownloadResult{
		{
			DownloadPath: targetDir + "/1",
			DownloadURL:  downloadURL,
		},
	}

	cmd := new(mocks.Command)
	cmd.On("RunAndReturnTrimmedCombinedOutput").Return("", nil).Once()

	cmdFactory := new(mocks.Factory)
	cmdFactory.On("Create", "tar", mock.Anything, mock.Anything).Return(cmd).Once()

	artifactDownloader := NewConcurrentArtifactDownloader(5*time.Minute, log.NewLogger(), cmdFactory)

	downloadResults, err := artifactDownloader.DownloadAndSaveArtifacts(artifacts, targetDir)

	assert.NoError(t, err)
	assert.ElementsMatch(t, expectedDownloadResults, downloadResults)
	cmd.AssertExpectations(t)
	cmdFactory.AssertExpectations(t)

	_ = os.RemoveAll(targetDir)
}
