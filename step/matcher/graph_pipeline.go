package matcher

import (
	"regexp"

	"github.com/bitrise-io/go-utils/v2/log"
	"github.com/bitrise-steplib/bitrise-step-pull-intermediate-files/model"
)

type graphPipelineMatcher struct {
	finishedWorkflows model.FinishedWorkflows
	targetNames       []string
	logger            log.Logger
}

func newGraphPipelineMatcher(finishedWorkflows model.FinishedWorkflows, targetNames []string, logger log.Logger) graphPipelineMatcher {
	return graphPipelineMatcher{
		finishedWorkflows: finishedWorkflows,
		targetNames:       targetNames,
		logger:            logger,
	}
}

func (g graphPipelineMatcher) Matches() ([]string, error) {
	var executedWorkflows []model.Workflow
	for _, workflow := range g.finishedWorkflows {
		if workflow.ExternalId == "" {
			g.logger.Printf("Skipping workflow %s because it was not executed.", workflow.Name)
			continue
		}
		executedWorkflows = append(executedWorkflows, workflow)
	}

	identifiers := make(map[string]bool)

	if len(g.targetNames) == 0 {
		for _, workflow := range executedWorkflows {
			identifiers[workflow.ExternalId] = true
		}

		return convertKeySetToSlice(identifiers), nil
	}

	for _, workflow := range executedWorkflows {
		for _, target := range g.targetNames {
			matched, err := regexp.MatchString(target, workflow.Name)
			if err != nil {
				return nil, err
			}

			if matched {
				identifiers[workflow.ExternalId] = true
				break
			}
		}
	}

	return convertKeySetToSlice(identifiers), nil
}
