package matcher

import (
	"regexp"

	"github.com/bitrise-io/go-utils/log"
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

func (s stagedPipelineMatcher) Matches() ([]string, error) {
	buildIDsSet := make(map[string]bool)

	kvpSlice := s.createKeyValuePairSlice()

	if len(s.targetNames) == 0 {
		for _, kvPair := range kvpSlice {
			buildIDsSet[kvPair.value] = true
		}

		return convertKeySetToArray(buildIDsSet), nil
	}

	for _, target := range s.targetNames {
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

	return convertKeySetToArray(buildIDsSet), nil
}

func (s stagedPipelineMatcher) createKeyValuePairSlice() []keyValuePair {
	var stageWorkflowMap []keyValuePair
	for _, stage := range s.finishedStages {
		for _, wf := range stage.Workflows {
			if wf.ExternalId == "" {
				s.logger.Printf("Skipping workflow %s in stage %s. Workflow was not executed.", wf.Name, stage.Name)
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
