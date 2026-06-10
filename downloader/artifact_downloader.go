package downloader

import (
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bitrise-steplib/bitrise-step-pull-intermediate-files/api"

	"github.com/bitrise-io/go-utils/filedownloader"
	"github.com/bitrise-io/go-utils/pathutil"
	"github.com/bitrise-io/go-utils/retry"
	"github.com/bitrise-io/go-utils/v2/command"
	"github.com/bitrise-io/go-utils/v2/log"
	"github.com/bitrise-io/go-utils/v2/retryhttp"
	"github.com/bitrise-io/got"
	"github.com/hashicorp/go-retryablehttp"
)

const (
	filePermission               = 0o655
	maxConcurrentDownloadThreads = 10
	etagFetchTimeout             = 30 * time.Second
)

// checksumStatus describes the outcome of validating a downloaded file against its remote ETag.
type checksumStatus string

const (
	checksumSingleOK         checksumStatus = "single:ok"
	checksumSingleMismatch   checksumStatus = "single:mismatch"
	checksumMultipartOK      checksumStatus = "multipart:ok"
	checksumMultipartUnknown checksumStatus = "multipart:unverified"
	checksumETagUnavailable  checksumStatus = "etag:unavailable"
)

type ArtifactDownloadResult struct {
	DownloadError   error
	DownloadPath    string
	DownloadURL     string
	DownloadDetails TransferDetails
	EnvKey          string
}

type TransferDetails struct {
	Size     int64
	Duration time.Duration
	Hostname string

	// MD5 is the hex-encoded MD5 of the full downloaded file.
	MD5 string
	// ETag is the remote ETag of the fetched object (quotes stripped).
	ETag string
	// ChecksumStatus is the outcome of validating MD5/ETag.
	ChecksumStatus string
}

type downloadJob struct {
	ResponseModel api.ArtifactResponseItemModel
	TargetDir     string
}

type ConcurrentArtifactDownloader struct {
	Timeout        time.Duration
	Logger         log.Logger
	CommandFactory command.Factory
}

func NewConcurrentArtifactDownloader(timeout time.Duration, logger log.Logger, commandFactory command.Factory) *ConcurrentArtifactDownloader {
	return &ConcurrentArtifactDownloader{
		Timeout:        timeout,
		Logger:         logger,
		CommandFactory: commandFactory,
	}
}

func (ad *ConcurrentArtifactDownloader) DownloadAndSaveArtifacts(artifacts []api.ArtifactResponseItemModel, targetDir string) ([]ArtifactDownloadResult, error) {
	if _, err := os.Stat(targetDir); os.IsNotExist(err) {
		if err := os.Mkdir(targetDir, filePermission); err != nil {
			return nil, err
		}
	}

	return ad.downloadParallel(artifacts, targetDir)
}

func (ad *ConcurrentArtifactDownloader) downloadParallel(artifacts []api.ArtifactResponseItemModel, targetDir string) ([]ArtifactDownloadResult, error) {
	var downloadResults []ArtifactDownloadResult

	jobs := make(chan downloadJob, len(artifacts))
	results := make(chan ArtifactDownloadResult, len(artifacts))

	for i := 0; i < maxConcurrentDownloadThreads; i++ {
		go ad.download(jobs, results)
	}

	for _, artifact := range artifacts {
		jobs <- downloadJob{
			ResponseModel: artifact,
			TargetDir:     targetDir,
		}
	}
	close(jobs)

	for i := 0; i < len(artifacts); i++ {
		res := <-results
		downloadResults = append(downloadResults, res)
	}

	return downloadResults, nil
}

func (ad *ConcurrentArtifactDownloader) download(jobs <-chan downloadJob, results chan<- ArtifactDownloadResult) {
	for j := range jobs {
		var fileFullPath string
		var details TransferDetails
		var err error

		switch {
		case j.ResponseModel.IntermediateFileInfo.IsDir && filepath.Ext(j.ResponseModel.Title) == ".tar":
			// Support deploy-to-bitrise-io version 2.1.2 and 2.1.3, which creates tar archives.
			fileFullPath, details, err = ad.downloadAndExtractTarArchive(j.TargetDir, j.ResponseModel.Title, j.ResponseModel.DownloadURL)
		case j.ResponseModel.IntermediateFileInfo.IsDir:
			fileFullPath, details, err = ad.downloadAndExtractZipArchive(j.TargetDir, j.ResponseModel.Title, j.ResponseModel.DownloadURL)
		default:
			fileFullPath, details, err = ad.downloadFile(j.TargetDir, j.ResponseModel.Title, j.ResponseModel.DownloadURL)
		}

		if err != nil {
			results <- ArtifactDownloadResult{DownloadError: err, DownloadURL: j.ResponseModel.DownloadURL}
			continue
		}

		results <- ArtifactDownloadResult{
			DownloadPath:    fileFullPath,
			DownloadURL:     j.ResponseModel.DownloadURL,
			DownloadDetails: details,
			EnvKey:          j.ResponseModel.IntermediateFileInfo.EnvKey,
		}
	}
}

func (ad *ConcurrentArtifactDownloader) downloadFile(targetDir, fileName, downloadURL string) (string, TransferDetails, error) {
	fileFullPath := filepath.Join(targetDir, fileName)

	ctx, cancel := context.WithTimeout(context.Background(), ad.Timeout)

	start := time.Now()

	err := downloadWithRetry(ctx, ad.createClient(), downloadURL, fileFullPath, ad.Logger)
	if err != nil {
		// fallback to single threaded download - the error with the 416 status code seems to happen for 0 size files with got
		errorMessage := err.Error()
		if strings.Contains(errorMessage, "Response status code is not ok: 416") || strings.Contains(errorMessage, "unexpected EOF") {
			ad.Logger.Warnf("Multi threaded download failed, switching to single threaded download")

			cancel()

			ctx, cancel = context.WithTimeout(context.Background(), ad.Timeout)

			start = time.Now()

			downloader := filedownloader.NewWithContext(ctx, retryhttp.NewClient(ad.Logger).StandardClient())
			err = downloader.Get(fileFullPath, downloadURL)
		}

		if err != nil {
			cancel()

			details := TransferDetails{
				Size:     fileSize(fileFullPath),
				Duration: time.Since(start),
				Hostname: extractHost(downloadURL),
			}

			return "", details, fmt.Errorf("unable to download file from %s: %w", downloadURL, err)
		}
	}

	cancel()

	details := TransferDetails{
		Size:     fileSize(fileFullPath),
		Duration: time.Since(start),
		Hostname: extractHost(downloadURL),
	}

	ad.verifyAndLogChecksum(fileFullPath, downloadURL, &details)

	return fileFullPath, details, nil
}

func (ad *ConcurrentArtifactDownloader) downloadAndExtractZipArchive(targetDir, fileName, downloadURL string) (string, TransferDetails, error) {
	tmpDir, err := pathutil.NormalizedOSTempDirPath("pull-intermediate-files")
	if err != nil {
		return "", TransferDetails{}, err
	}

	fileFullPath, details, err := ad.downloadFile(tmpDir, fileName, downloadURL)
	if err != nil {
		return "", details, err
	}

	dirName := strings.TrimSuffix(fileName, filepath.Ext(fileName))
	dirPath := filepath.Join(targetDir, dirName)

	if err := os.MkdirAll(dirPath, os.ModePerm); err != nil {
		return "", details, err
	}

	if err := ad.extractZipArchive(fileFullPath, dirPath); err != nil {
		return "", details, err
	}

	return dirPath, details, nil
}

func (ad *ConcurrentArtifactDownloader) downloadAndExtractTarArchive(targetDir, fileName, downloadURL string) (string, TransferDetails, error) {
	client := ad.createClient()

	start := time.Now()

	resp, err := client.Get(downloadURL)
	if err != nil {
		return "", TransferDetails{}, err
	}

	details := TransferDetails{
		// Tar archives are not created by the deploy step anymore, so we should not run into this case.
		// The tar command streams the data from the standard input, so we cannot get the size of the file easily.
		Size:     -2,
		Duration: time.Since(start),
		Hostname: extractHost(downloadURL),
	}

	dirName := strings.TrimSuffix(fileName, filepath.Ext(fileName))
	dirPath := filepath.Join(targetDir, dirName)

	if err := os.MkdirAll(dirPath, os.ModePerm); err != nil {
		return "", details, err
	}

	if err := ad.extractTarArchive(resp.Body, dirPath); err != nil {
		return "", details, err
	}

	if err := resp.Body.Close(); err != nil {
		ad.Logger.Warnf("Failed to close response body: %s", err)
	}

	return dirPath, details, nil
}

func (ad *ConcurrentArtifactDownloader) extractZipArchive(archivePath string, targetDir string) error {
	cmd := ad.CommandFactory.Create("unzip", []string{"-o", archivePath}, &command.Opts{Dir: targetDir})
	return ad.runExtractionCommand(cmd)
}

func (ad *ConcurrentArtifactDownloader) extractTarArchive(r io.Reader, targetDir string) error {
	tarArgs := []string{
		"-x",      // -x: extract files from an archive: https://www.gnu.org/software/tar/manual/html_node/extract.html#SEC25
		"-f", "-", // -f "-": reads the archive from standard input: https://www.gnu.org/software/tar/manual/html_node/Device.html#SEC155
	}
	cmd := ad.CommandFactory.Create("tar", tarArgs, &command.Opts{
		Stdin: r,
		Dir:   targetDir,
	})
	return ad.runExtractionCommand(cmd)
}

func (ad *ConcurrentArtifactDownloader) runExtractionCommand(cmd command.Command) error {
	if out, err := cmd.RunAndReturnTrimmedCombinedOutput(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return fmt.Errorf("command failed with exit status %d (%s): %w", exitErr.ExitCode(), cmd.PrintableCommandArgs(), errors.New(out))
		}
		return fmt.Errorf("%s failed: %w", cmd.PrintableCommandArgs(), err)
	}

	return nil
}

func (ad *ConcurrentArtifactDownloader) createClient() *retryablehttp.Client {
	client := retryhttp.NewClient(ad.Logger)
	client.CheckRetry = func(ctx context.Context, resp *http.Response, err error) (bool, error) {
		// We are using this default retry policy as part of the default client settings
		shouldRetry, err := retryablehttp.DefaultRetryPolicy(ctx, resp, err)

		if shouldRetry {
			statusCode := -1
			if resp != nil {
				statusCode = resp.StatusCode
			}
			ad.Logger.Warnf("Retrying download, http status: %d, error: %s", statusCode, err)
		}

		return shouldRetry, err
	}
	client.ErrorHandler = func(resp *http.Response, err error, numTries int) (*http.Response, error) {
		statusCode := -1
		if resp != nil {
			statusCode = resp.StatusCode
		}
		ad.Logger.Warnf("After %d retries http status: %d, error: %s", numTries, statusCode, err)

		return resp, err
	}
	return client
}

// verifyAndLogChecksum computes the MD5 of the downloaded file, fetches the remote ETag and
// validates one against the other. It is purely diagnostic: failures are logged, never returned.
func (ad *ConcurrentArtifactDownloader) verifyAndLogChecksum(path, downloadURL string, details *TransferDetails) {
	md5sum, err := fileMD5(path)
	if err != nil {
		ad.Logger.Warnf("Failed to compute MD5 of %s: %s", filepath.Base(path), err)
		return
	}
	details.MD5 = md5sum

	etag, err := ad.fetchETag(downloadURL)
	if err != nil {
		details.ChecksumStatus = string(checksumETagUnavailable)
		ad.Logger.Warnf("Could not fetch ETag for %s (md5=%s): %s", filepath.Base(path), md5sum, err)
		return
	}
	details.ETag = etag

	status := ad.validateChecksum(path, md5sum, etag)
	details.ChecksumStatus = string(status)

	switch status {
	case checksumSingleMismatch:
		ad.Logger.Warnf("Checksum mismatch for %s: md5=%s does not match single-part ETag=%s", filepath.Base(path), md5sum, etag)
	case checksumMultipartUnknown:
		ad.Logger.Warnf("Could not verify multipart ETag for %s: md5=%s, etag=%s (upload part size unknown)", filepath.Base(path), md5sum, etag)
	default:
		ad.Logger.Printf("Checksum for %s: md5=%s, etag=%s, validation=%s", filepath.Base(path), md5sum, etag, status)
	}
}

// validateChecksum compares the file's MD5 against the remote ETag. An ETag of the form
// "<hash>-<N>" denotes a multipart upload, whose ETag is the MD5 of the concatenated binary
// MD5 digests of each part, suffixed with the part count; otherwise it is a plain MD5.
func (ad *ConcurrentArtifactDownloader) validateChecksum(path, md5sum, etag string) checksumStatus {
	if dash := strings.LastIndex(etag, "-"); dash != -1 {
		parts, err := strconv.Atoi(etag[dash+1:])
		if err != nil || parts < 1 {
			return checksumMultipartUnknown
		}
		if matchMultipartETag(path, etag, parts) {
			return checksumMultipartOK
		}
		return checksumMultipartUnknown
	}

	if strings.EqualFold(md5sum, etag) {
		return checksumSingleOK
	}
	return checksumSingleMismatch
}

// fetchETag reads the remote object's ETag via a single-byte ranged GET. S3/R2 return the full
// object's ETag on a 206 response, and Range is not a signed header so it works with the
// presigned download URL.
func (ad *ConcurrentArtifactDownloader) fetchETag(downloadURL string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), etagFetchTimeout)
	defer cancel()

	req, err := retryablehttp.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Range", "bytes=0-0")

	resp, err := ad.createClient().Do(req)
	if err != nil {
		return "", err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	etag := strings.Trim(resp.Header.Get("ETag"), `"`)
	if etag == "" {
		return "", fmt.Errorf("response has no ETag header")
	}

	return etag, nil
}

// fileMD5 returns the hex-encoded MD5 of the whole file.
func fileMD5(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}

	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// matchMultipartETag reports whether the file reproduces the given multipart ETag for any
// plausible upload part size. The part size is unknown at download time, so candidates are
// derived from common values that yield exactly the expected part count.
func matchMultipartETag(path, expected string, parts int) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}

	for _, partSize := range candidatePartSizes(info.Size(), parts) {
		etag, err := multipartETag(path, partSize)
		if err != nil {
			continue
		}
		if etag == expected {
			return true
		}
	}

	return false
}

// candidatePartSizes returns plausible upload part sizes that would split a file of fileSize
// bytes into exactly the given number of parts. The cheap ceil(fileSize/partSize)==parts filter
// prunes the common-value list (in both MiB and MB units) down to the few that could match,
// before any file is read.
func candidatePartSizes(fileSize int64, parts int) []int64 {
	if parts < 1 || fileSize < 1 {
		return nil
	}
	// A single-part multipart upload covers the whole file in one part.
	if parts == 1 {
		return []int64{fileSize}
	}

	baseMB := []int64{5, 8, 10, 15, 16, 25, 32, 50, 64, 100, 128, 256, 512, 1024}
	units := []int64{1 << 20, 1_000_000} // MiB and MB.

	seen := map[int64]bool{}
	var candidates []int64
	for _, base := range baseMB {
		for _, unit := range units {
			partSize := base * unit
			if partSize < 1 || seen[partSize] {
				continue
			}
			seen[partSize] = true
			if (fileSize+partSize-1)/partSize == int64(parts) {
				candidates = append(candidates, partSize)
			}
		}
	}

	return candidates
}

// multipartETag computes the S3/R2 multipart ETag of a file for a given part size: the MD5 of
// the concatenated binary MD5 digests of each part, hex-encoded and suffixed with the part count.
func multipartETag(path string, partSize int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var digests []byte
	parts := 0
	for {
		partHash := md5.New()
		n, err := io.CopyN(partHash, f, partSize)
		if n > 0 {
			digests = append(digests, partHash.Sum(nil)...)
			parts++
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}

	sum := md5.Sum(digests)
	return fmt.Sprintf("%x-%d", sum, parts), nil
}

func extractHost(downloadURL string) string {
	u, err := url.Parse(downloadURL)
	if err != nil {
		return "unknown"
	}

	return strings.TrimPrefix(u.Hostname(), "www.")
}

func fileSize(path string) int64 {
	f, err := os.Stat(path)
	if err != nil {
		return -1
	}

	return f.Size()
}

func downloadWithRetry(ctx context.Context, httpClient *retryablehttp.Client, url, dest string, logger log.Logger) error {
	return retry.Times(5).Wait(5 * time.Second).TryWithAbort(func(attempt uint) (error, bool) {
		if attempt != 0 {
			logger.Debugf("Retrying intermediate file download... (attempt %d)", attempt+1)
		}

		logger.Debugf("Downloading intermediate file...")
		downloadErr := download(ctx, httpClient, url, dest, logger)
		if downloadErr != nil {
			logger.Debugf("Failed to download intermediate file: %s", downloadErr)
			return fmt.Errorf("failed to download intermediate file: %w", downloadErr), false
		}

		return nil, false
	})
}

func download(ctx context.Context, httpClient *retryablehttp.Client, url string, dest string, logger log.Logger) error {
	if t, ok := httpClient.HTTPClient.Transport.(*http.Transport); ok {
		t.ForceAttemptHTTP2 = false
		t.DialContext = (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
			DualStack: false,
		}).DialContext
		t.ResponseHeaderTimeout = 30 * time.Second
	}

	downloader := got.New()
	downloader.Client = httpClient.StandardClient()

	gDownload := got.NewDownload(ctx, url, dest)
	// Client has to be set on "Download" as well,
	// as depending on how downloader is called
	// either the Client from the downloader or from the Download will be used.
	gDownload.Client = httpClient.StandardClient()
	gDownload.Concurrency = 0
	gDownload.Logger = logger
	gDownload.MaxRetryPerChunk = 5
	gDownload.ChunkRetryThreshold = 10 * time.Second

	return downloader.Do(gDownload)
}
