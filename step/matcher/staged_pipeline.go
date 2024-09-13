package matcher

import (
	"regexp"

	"github.com/bitrise-io/go-utils/v2/log"
	"github.com/bitrise-steplib/bitrise-step-pull-intermediate-files/model"
)

const DELIMITER = "."

type stagedPipelineMatcher struct {
	finishedStages model.FinishedStages
	targetNames    []string
	logger         log.Logger
}

type keyValuePair struct {
	key   string
	value string
}

func newStagedPipelineMatcher(finishedStages model.FinishedStages, targetNames []string, logger log.Logger) stagedPipelineMatcher {
	return stagedPipelineMatcher{
		finishedStages: finishedStages,
		targetNames:    targetNames,
		logger:         logger,
	}
}

func (spm stagedPipelineMatcher) Matches() ([]string, error) {
	buildIDsSet := make(map[string]bool)

	kvpSlice := spm.createKeyValuePairSlice()

	if len(spm.targetNames) == 0 {
		for _, kvPair := range kvpSlice {
			buildIDsSet[kvPair.value] = true
		}

		return convertKeySetToSlice(buildIDsSet), nil
	}

	for _, target := range spm.targetNames {
		for _, kvPair := range kvpSlice {
			matched, err := regexp.MatchString(target, kvPair.key)
			if err != nil {
				return nil, err
			}

			if matched {
				buildIDsSet[kvPair.value] = true
			}
		}
	}

	return convertKeySetToSlice(buildIDsSet), nil
}

func (spm stagedPipelineMatcher) createKeyValuePairSlice() []keyValuePair {
	var stageWorkflowMap []keyValuePair
	for _, stage := range spm.finishedStages {
		for _, wf := range stage.Workflows {
			if wf.ExternalId == "" {
				spm.logger.Printf("Skipping workflow %s in stage %s. Workflow was not executed.", wf.Name, stage.Name)
				continue
			}
			stageWorkflowMap = append(stageWorkflowMap, keyValuePair{
				key:   stage.Name + DELIMITER + wf.Name,
				value: wf.ExternalId,
			})
		}
	}

	return stageWorkflowMap
}
