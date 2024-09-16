package step

import (
	"strings"
	"testing"

	mockenv "github.com/bitrise-steplib/bitrise-step-pull-intermediate-files/mocks"

	"github.com/bitrise-io/go-steputils/v2/stepconf"
	"github.com/bitrise-io/go-utils/v2/command"
	"github.com/bitrise-io/go-utils/v2/log"
	"github.com/stretchr/testify/assert"
)

func Test_GivenInputs_WhenCreatingConfig_ThenMappingIsCorrect(t *testing.T) {
	testCases := []struct {
		name              string
		artifactSources   []string
		finishedStages    string
		finishedWorkflows string
		expectError       bool
	}{
		{
			name: "only staged pipeline",
			artifactSources: []string{
				"stage1.workflow1",
				"stage2.*",
			},
			finishedStages:    "[]",
			finishedWorkflows: "",
		},
		{
			name: "only graph pipeline",
			artifactSources: []string{
				"workflow1",
				"workflows1_.*",
			},
			finishedStages:    "",
			finishedWorkflows: "[]",
		},
		{
			name:              "both pipeline types set",
			finishedStages:    "[]",
			finishedWorkflows: "[]",
			expectError:       true,
		},
		{
			name:              "none of the pipeline types set",
			finishedStages:    "[]",
			finishedWorkflows: "[]",
			expectError:       true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			// Given
			envRepository := new(mockenv.Repository)
			envRepository.On("Get", "artifact_sources").Return(strings.Join(testCase.artifactSources, ","))
			envRepository.On("Get", "verbose").Return("true")
			envRepository.On("Get", "app_slug").Return("app-slug")
			envRepository.On("Get", "finished_stage").Return(testCase.finishedStages)
			envRepository.On("Get", "finished_workflows").Return(testCase.finishedWorkflows)
			envRepository.On("Get", "bitrise_api_base_url").Return("url")
			envRepository.On("Get", "bitrise_api_access_token").Return("token")

			inputParser := stepconf.NewInputParser(envRepository)
			cmdFactory := command.NewFactory(envRepository)
			step := IntermediateFileDownloader{
				inputParser:   inputParser,
				envRepository: envRepository,
				cmdFactory:    cmdFactory,
				logger:        log.NewLogger(),
			}

			// When
			config, err := step.ProcessConfig()

			// Then
			if testCase.expectError {
				assert.True(t, err != nil)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, testCase.artifactSources, config.ArtifactSources)
			}
		})
	}
}

func Test_GivenNoToken_WhenCreatingConfig_ThenErrorIsCorrect(t *testing.T) {
	// Given
	envRepository := new(mockenv.Repository)
	envRepository.On("Get", "artifact_sources").Return("stage1.workflow1,stage2.*")
	envRepository.On("Get", "verbose").Return("true")
	envRepository.On("Get", "app_slug").Return("app-slug")
	envRepository.On("Get", "finished_stage").Return("[]")
	envRepository.On("Get", "finished_workflows").Return("[]")
	envRepository.On("Get", "bitrise_api_base_url").Return("url")
	envRepository.On("Get", "bitrise_api_access_token").Return("")

	inputParser := stepconf.NewInputParser(envRepository)
	cmdFactory := command.NewFactory(envRepository)
	step := IntermediateFileDownloader{
		inputParser:   inputParser,
		envRepository: envRepository,
		cmdFactory:    cmdFactory,
		logger:        log.NewLogger(),
	}

	// When
	_, err := step.ProcessConfig()

	// Then
	assert.ErrorIs(t, err, ErrMissingAccessToken)
}

func Test_Export(t *testing.T) {
	envRepository := new(mockenv.Repository)

	testCases := []struct {
		desc        string
		inputResult Result
	}{
		{
			desc: "when there are more than one result, it exports a coma separated list",
			inputResult: Result{
				IntermediateFiles: map[string]string{"ENV_KEY_A": "aa.txt", "ENV_KEY_B": "bb.txt"},
			},
		},
		{
			desc: "when there is a result element",
			inputResult: Result{
				IntermediateFiles: map[string]string{"ENV_KEY_A": "aa.txt"},
			},
		},
		{
			desc: "when there is no result element",
			inputResult: Result{
				IntermediateFiles: map[string]string{},
			},
		},
	}
	for _, tC := range testCases {
		t.Run(tC.desc, func(t *testing.T) {
			step := IntermediateFileDownloader{
				envRepository: envRepository,
				logger:        log.NewLogger(),
			}

			for envKey, path := range tC.inputResult.IntermediateFiles {
				envRepository.On("Set", envKey, path).Return(nil)
			}

			err := step.Export(tC.inputResult)

			envRepository.AssertExpectations(t)
			assert.NoError(t, err)
		})
	}
}
