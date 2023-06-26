package step

import (
	"regexp"

	"github.com/bitrise-io/go-utils/log"
	"github.com/bitrise-steplib/bitrise-step-pull-intermediate-files/model"
)

const DELIMITER = "."

type BuildIDGetter struct {
	FinishedStages model.FinishedStages
	TargetNames    []string
	logger        log.Logger
}

type keyValuePair struct {
	key   string
	value string
}

func NewBuildIDGetter(finishedStages model.FinishedStages, targetNames []string, logger log.Logger) BuildIDGetter {
	return BuildIDGetter{
		FinishedStages: finishedStages,
		TargetNames:    targetNames,
		logger: logger,
	}
}

func (bg BuildIDGetter) GetBuildIDs() ([]string, error) {
	buildIDsSet := make(map[string]bool)

	kvpSlice := bg.createKeyValuePairSlice()

	if len(bg.TargetNames) == 0 {
		for _, kvPair := range kvpSlice {
			buildIDsSet[kvPair.value] = true
		}

		return convertKeySetToArray(buildIDsSet), nil
	}

	for _, target := range bg.TargetNames {
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

func convertKeySetToArray(set map[string]bool) []string {
	var ids []string

	for k := range set {
		ids = append(ids, k)
	}

	return ids
}

func (bg BuildIDGetter) createKeyValuePairSlice() []keyValuePair {
	var stageWorkflowMap []keyValuePair
	for _, stage := range bg.FinishedStages {
		for _, wf := range stage.Workflows {
			if wf.ExternalId == "" {
				bg.logger.Printf("Skipping workflow %s in stage %s. Workflow was not executed.", wf.Name, stage.Name)
				continue;
			}
			stageWorkflowMap = append(stageWorkflowMap, keyValuePair{
				key:   stage.Name + DELIMITER + wf.Name,
				value: wf.ExternalId,
			})
		}
	}

	return stageWorkflowMap
}
