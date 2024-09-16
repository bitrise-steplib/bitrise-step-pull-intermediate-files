package main

import (
	"errors"
	"os"

	"github.com/bitrise-io/go-steputils/v2/stepconf"
	"github.com/bitrise-io/go-steputils/v2/stepenv"
	"github.com/bitrise-io/go-utils/v2/command"
	"github.com/bitrise-io/go-utils/v2/env"
	"github.com/bitrise-io/go-utils/v2/log"
	"github.com/bitrise-steplib/bitrise-step-pull-intermediate-files/step"
)

func main() {
	os.Exit(run())
}

func run() int {
	logger := log.NewLogger()

	downloader := createIntermediateFileDownloader(logger)
	config, err := downloader.ProcessConfig()
	if err != nil {
		if errors.Is(err, step.ErrMissingAccessToken) {
			logger.Warnf("Bitrise API token is unavailable, skipping step.")
			logger.Printf("Hint: maybe this build is running in the first stage of a pipeline? If so, pulling intermediate files is not possible.")
			return 0
		} else {
			logger.Errorf("Process config: %s", err)
			return 1
		}
	}

	result, err := downloader.Run(config)
	if err != nil {
		logger.Errorf("Run: %s", err)
		return 1
	}

	if err := downloader.Export(result); err != nil {
		logger.Errorf("Export outputs: %s", err)
		return 1
	}

	return 0
}

func createIntermediateFileDownloader(logger log.Logger) step.IntermediateFileDownloader {
	envRepository := stepenv.NewRepository(env.NewRepository())
	cmdFactory := command.NewFactory(envRepository)
	inputParser := stepconf.NewInputParser(envRepository)

	return step.NewIntermediateFileDownloader(inputParser, envRepository, cmdFactory, logger)
}
