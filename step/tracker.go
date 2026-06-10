package step

import (
	"github.com/bitrise-io/go-utils/v2/analytics"
	"github.com/bitrise-io/go-utils/v2/env"
	"github.com/bitrise-io/go-utils/v2/log"

	"github.com/bitrise-steplib/bitrise-step-pull-intermediate-files/downloader"
)

type tracker struct {
	tracker analytics.Tracker
}

func newTracker(envRepo env.Repository, logger log.Logger) tracker {
	p := analytics.Properties{
		"step_id":    "pull-intermediate-files",
		"build_slug": envRepo.Get("BITRISE_BUILD_SLUG"),
		"app_slug":   envRepo.Get("BITRISE_APP_SLUG"),
	}
	return tracker{
		tracker: analytics.NewDefaultTracker(logger, p),
	}
}

func (t *tracker) logFileTransfer(details downloader.TransferDetails, err error) {
	properties := analytics.Properties{
		"storage_host": details.Hostname,
		"duration_ms":  details.Duration.Milliseconds(),
		"size_bytes":   details.Size,
	}
	if details.MD5 != "" {
		properties["md5"] = details.MD5
	}
	if details.ETag != "" {
		properties["etag"] = details.ETag
	}
	if details.ChecksumStatus != "" {
		properties["checksum_status"] = details.ChecksumStatus
	}
	if err != nil {
		properties["error"] = err.Error()
	}

	t.tracker.Enqueue("intermediate_file_downloaded", properties)
}

func (t *tracker) wait() {
	t.tracker.Wait()
}
