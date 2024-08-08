package step

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bitrise-io/go-steputils/stepconf"
	"github.com/bitrise-io/go-utils/command"
	"github.com/bitrise-io/go-utils/env"
	"github.com/bitrise-io/go-utils/log"
	"github.com/bitrise-io/go-utils/pathutil"
	"github.com/bitrise-steplib/bitrise-step-pull-intermediate-files/api"
	"github.com/bitrise-steplib/bitrise-step-pull-intermediate-files/downloader"
	"github.com/bitrise-steplib/bitrise-step-pull-intermediate-files/export"
	"github.com/bitrise-steplib/bitrise-step-pull-intermediate-files/model"
	"github.com/bitrise-steplib/bitrise-step-pull-intermediate-files/step/matcher"
)

const downloadDirPrefix = "_artifact_pull"

var ErrMissingAccessToken = errors.New("missing Bitrise API access token")

type Input struct {
	ArtifactSources       string          `env:"artifact_sources,required"`
	Verbose               bool            `env:"verbose,opt[true,false]"`
	AppSlug               string          `env:"app_slug,required"`
	FinishedStages        string          `env:"finished_stage"`
	FinishedWorkflows     string          `env:"finished_workflows"`
	BitriseAPIBaseURL     string          `env:"bitrise_api_base_url,required"`
	BitriseAPIAccessToken stepconf.Secret `env:"bitrise_api_access_token"`
}

type Config struct {
	ArtifactSources       []string
	AppSlug               string
	FinishedStages        model.FinishedStages
	FinishedWorkflows     model.FinishedWorkflows
	BitriseAPIBaseURL     string
	BitriseAPIAccessToken string
}

type Result struct {
	IntermediateFiles map[string]string
}

type IntermediateFileDownloader struct {
	inputParser   stepconf.InputParser
	envRepository env.Repository
	cmdFactory    command.Factory
	logger        log.Logger
}

func NewIntermediateFileDownloader(inputParser stepconf.InputParser, envRepository env.Repository, cmdFactory command.Factory, logger log.Logger) IntermediateFileDownloader {
	return IntermediateFileDownloader{inputParser: inputParser, envRepository: envRepository, cmdFactory: cmdFactory, logger: logger}
}

func (d IntermediateFileDownloader) ProcessConfig() (Config, error) {
	var input Input
	err := d.inputParser.Parse(&input)
	if err != nil {
		return Config{}, err
	}

	stepconf.Print(input)
	d.logger.EnableDebugLog(input.Verbose)

	if strings.TrimSpace(string(input.BitriseAPIAccessToken)) == "" {
		return Config{}, ErrMissingAccessToken
	}

	if input.FinishedStages == "" && input.FinishedWorkflows == "" {
		return Config{}, fmt.Errorf("both finished stages and workflows inputs are missing")
	} else if input.FinishedStages != "" && input.FinishedWorkflows != "" {
		return Config{}, fmt.Errorf("both finished stages and workflows inputs are set")
	}

	var finishedStagesModel model.FinishedStages
	if input.FinishedStages != "" {
		if err := json.Unmarshal([]byte(input.FinishedStages), &finishedStagesModel); err != nil {
			return Config{}, fmt.Errorf("invalid finished stages: %w", err)
		}
	}

	var finishedWorkflows model.FinishedWorkflows
	if input.FinishedWorkflows != "" {
		if err := json.Unmarshal([]byte(input.FinishedWorkflows), &finishedWorkflows); err != nil {
			return Config{}, fmt.Errorf("invalid finished workflows: %w", err)
		}
	}

	return Config{
		ArtifactSources:       strings.Split(input.ArtifactSources, ","),
		AppSlug:               input.AppSlug,
		FinishedStages:        finishedStagesModel,
		FinishedWorkflows:     finishedWorkflows,
		BitriseAPIBaseURL:     input.BitriseAPIBaseURL,
		BitriseAPIAccessToken: string(input.BitriseAPIAccessToken),
	}, nil
}

func (d IntermediateFileDownloader) Run(cfg Config) (Result, error) {
	buildIdMatcher := matcher.NewBuildIDMatcher(cfg.FinishedStages, cfg.FinishedWorkflows, cfg.ArtifactSources, d.logger)
	buildIDs, err := buildIdMatcher.Matches()
	if err != nil {
		return Result{}, fmt.Errorf("failed to get build IDs: %w", err)
	}

	d.logger.Println()
	d.logger.Debugf("Downloading artifacts for builds %+v", buildIDs)
	d.logger.Printf("Getting the list of artifacts of %d builds", len(buildIDs))

	artifactLister, err := api.NewArtifactLister(cfg.BitriseAPIBaseURL, cfg.BitriseAPIAccessToken, d.logger)
	if err != nil {
		return Result{}, fmt.Errorf("failed to create artifact lister: %w", err)
	}
	artifacts, err := artifactLister.ListIntermediateFileDetails(cfg.AppSlug, buildIDs)
	if err != nil {
		return Result{}, fmt.Errorf("failed to list artifacts: %w", err)
	}

	d.logger.Printf("Downloading %d artifacts", len(artifacts))

	targetDir, err := pathutil.NormalizedOSTempDirPath(downloadDirPrefix)
	if err != nil {
		return Result{}, fmt.Errorf("failed to create artifact download directory: %w", err)
	}

	artifactDownloader := downloader.NewConcurrentArtifactDownloader(5*time.Minute, d.logger, d.cmdFactory)
	downloadResults, err := artifactDownloader.DownloadAndSaveArtifacts(artifacts, targetDir)
	if err != nil {
		return Result{}, fmt.Errorf("failed to download artifacts: %w", err)
	}

	intermediateFiles := map[string]string{}
	for _, downloadResult := range downloadResults {
		if downloadResult.DownloadError != nil {
			return Result{}, fmt.Errorf("failed to download artifact from %s, error: %s", downloadResult.DownloadURL, downloadResult.DownloadError.Error())
		}

		d.logger.Debugf("Artifact downloaded: %s", downloadResult.DownloadPath)

		intermediateFiles[downloadResult.EnvKey] = downloadResult.DownloadPath
	}

	return Result{IntermediateFiles: intermediateFiles}, nil
}

func (d IntermediateFileDownloader) Export(result Result) error {
	exporter := export.NewOutputExporter(d.logger, d.envRepository)
	return exporter.Export(result.IntermediateFiles)
}
