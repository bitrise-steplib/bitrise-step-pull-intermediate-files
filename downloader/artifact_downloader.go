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
	"strings"
	"sync"
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
	multipartMaxParts            = 10_000 // provider hard cap on parts per upload
)

// multipartTargetPartSize mirrors the storage backend's target part size, so the upload part size
// can be recomputed deterministically. It is a var only so tests can lower it to exercise
// multi-part validation without a multi-hundred-MiB file.
var multipartTargetPartSize int64 = 100 * (1 << 20) // 100 MiB

// checksumStatus describes the outcome of validating a downloaded file against its remote ETag.
type checksumStatus string

const (
	checksumSingleOK          checksumStatus = "single:ok"
	checksumSingleMismatch    checksumStatus = "single:mismatch"
	checksumMultipartOK       checksumStatus = "multipart:ok"
	checksumMultipartMismatch checksumStatus = "multipart:mismatch"
	checksumETagUnavailable   checksumStatus = "etag:unavailable"
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

	// Reuse the object's ETag from the download responses for checksum validation, instead of
	// issuing a separate request. This relies on a response carrying the ETag; if none does, the
	// ETag stays empty and checksum validation is skipped.
	recorder := &etagRecorder{}

	ctx, cancel := context.WithTimeout(context.Background(), ad.Timeout)

	start := time.Now()

	client := ad.createClient()
	recorder.attachTo(client)

	err := downloadWithRetry(ctx, client, downloadURL, fileFullPath, ad.Logger)
	if err != nil {
		// fallback to single threaded download - the error with the 416 status code seems to happen for 0 size files with got
		errorMessage := err.Error()
		if strings.Contains(errorMessage, "Response status code is not ok: 416") || strings.Contains(errorMessage, "unexpected EOF") {
			ad.Logger.Warnf("Multi threaded download failed, switching to single threaded download")

			cancel()

			ctx, cancel = context.WithTimeout(context.Background(), ad.Timeout)

			start = time.Now()

			fallbackClient := retryhttp.NewClient(ad.Logger)
			recorder.attachTo(fallbackClient)
			downloader := filedownloader.NewWithContext(ctx, fallbackClient.StandardClient())
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
		ETag:     recorder.value(),
	}

	ad.verifyAndLogChecksum(fileFullPath, &details)

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

// verifyAndLogChecksum computes the MD5 of the downloaded file and validates it against the ETag
// captured from the download response. It is purely diagnostic: failures are logged, never returned.
func (ad *ConcurrentArtifactDownloader) verifyAndLogChecksum(path string, details *TransferDetails) {
	// Compute the whole-file MD5 and (only for a multipart ETag) the multipart ETag in a single
	// pass over the file, so a large download is not re-read for validation.
	var partSize int64
	if strings.Contains(details.ETag, "-") {
		partSize = multipartPartSize(details.Size)
	}

	md5sum, multipartETag, err := fileChecksums(path, partSize)
	if err != nil {
		ad.Logger.Warnf("Failed to compute checksum of %s: %s", filepath.Base(path), err)
		return
	}
	details.MD5 = md5sum

	if details.ETag == "" {
		details.ChecksumStatus = string(checksumETagUnavailable)
		// Some responses (e.g. empty objects) carry no ETag; only flag it for non-empty files.
		if details.Size > 0 {
			ad.Logger.Warnf("No ETag in download response for %s (md5=%s)", filepath.Base(path), md5sum)
		}
		return
	}

	status := validateChecksum(md5sum, multipartETag, details.ETag)
	details.ChecksumStatus = string(status)

	switch status {
	case checksumSingleMismatch:
		ad.Logger.Warnf("Checksum mismatch for %s: md5=%s, etag=%s", filepath.Base(path), md5sum, details.ETag)
	case checksumMultipartMismatch:
		ad.Logger.Warnf("Multipart ETag mismatch for %s: md5=%s, recomputed=%s, etag=%s (object may use a non-MD5 ETag or a different upload part size)", filepath.Base(path), md5sum, multipartETag, details.ETag)
	default:
		ad.Logger.Printf("Checksum for %s: md5=%s, etag=%s, validation=%s", filepath.Base(path), md5sum, details.ETag, status)
	}
}

// validateChecksum compares the precomputed local checksums against the remote ETag: an ETag of the
// form "<hash>-<N>" is multipart and compared against multipartETag, otherwise it is a plain MD5
// compared against md5sum.
func validateChecksum(md5sum, multipartETag, etag string) checksumStatus {
	if strings.Contains(etag, "-") {
		if strings.EqualFold(multipartETag, etag) {
			return checksumMultipartOK
		}
		return checksumMultipartMismatch
	}

	if strings.EqualFold(md5sum, etag) {
		return checksumSingleOK
	}
	return checksumSingleMismatch
}

// etagRecorder captures the object's ETag from download responses, so checksum validation can
// reuse it instead of issuing a separate request. The mutex guards against concurrent responses.
type etagRecorder struct {
	mu   sync.Mutex
	etag string
}

// attachTo installs the recorder as the client's response hook.
func (r *etagRecorder) attachTo(client *retryablehttp.Client) {
	client.ResponseLogHook = func(_ retryablehttp.Logger, resp *http.Response) {
		if resp == nil {
			return
		}
		etag := strings.Trim(resp.Header.Get("ETag"), `"`)
		if etag == "" {
			return
		}
		r.mu.Lock()
		r.etag = etag
		r.mu.Unlock()
	}
}

func (r *etagRecorder) value() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.etag
}

// multipartPartSize recomputes the upload part size deterministically, mirroring the storage
// backend: a 100 MiB target, raised only enough to keep the part count within the 10,000-part cap.
func multipartPartSize(fileSize int64) int64 {
	partSize := multipartTargetPartSize
	if capped := (fileSize + multipartMaxParts - 1) / multipartMaxParts; capped > partSize {
		partSize = capped
	}
	return partSize
}

// fileChecksums reads the file once and returns the hex-encoded MD5 of the whole file and, when
// partSize > 0, the S3/R2 multipart ETag for that part size: the MD5 of the concatenated binary MD5
// digests of each part, hex-encoded and suffixed with the part count. Computing both in a single
// pass avoids re-reading large files during checksum validation.
func fileChecksums(path string, partSize int64) (md5hex, multipartETag string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer func() { err = errors.Join(err, f.Close()) }()

	fullHash := md5.New()

	if partSize <= 0 {
		if _, err := io.Copy(fullHash, f); err != nil {
			return "", "", err
		}
		return fmt.Sprintf("%x", fullHash.Sum(nil)), "", nil
	}

	// Hash each part separately while feeding every byte to the whole-file hash. io.CopyN reads one
	// part per iteration; a short read means EOF. Parts are counted from the bytes actually read.
	partHash := md5.New()
	combined := io.MultiWriter(fullHash, partHash)
	var partDigests []byte
	partCount := 0
	for {
		partHash.Reset()
		n, err := io.CopyN(combined, f, partSize)
		if n > 0 {
			partDigests = append(partDigests, partHash.Sum(nil)...)
			partCount++
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", "", err
		}
	}

	sum := md5.Sum(partDigests)
	return fmt.Sprintf("%x", fullHash.Sum(nil)), fmt.Sprintf("%x-%d", sum, partCount), nil
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
