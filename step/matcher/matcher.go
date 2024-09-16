package matcher

import (
	"github.com/bitrise-io/go-utils/v2/log"
	"github.com/bitrise-steplib/bitrise-step-pull-intermediate-files/model"
)

type BuildIDMatcher interface {
	Matches() ([]string, error)
}

func NewBuildIDMatcher(finishedStages model.FinishedStages, finishedWorkflows model.FinishedWorkflows, targetNames []string, logger log.Logger) BuildIDMatcher {
	if len(finishedStages) > 0 {
		return newStagedPipelineMatcher(finishedStages, targetNames, logger)
	}

	return newGraphPipelineMatcher(finishedWorkflows, targetNames, logger)
}

func convertKeySetToSlice(set map[string]bool) []string {
	var ids []string

	for k := range set {
		ids = append(ids, k)
	}

	return ids
}
